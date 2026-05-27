package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

// AliasStore is the persistence surface the alias verbs depend on.
//
// Many aliases may be bound to a single (slack_team_id,
// slack_channel_id) row concurrently — the channel_policies table
// carries an app-managed `alias_bindings: Map<alias_name,
// resource_id>` attribute so a channel can host `$grafana`,
// `$staging-db`, `$logs` etc. simultaneously, each pointing at a
// distinct resource. A small interface here rather than a direct dep
// on the slackdata package keeps this PR shippable against main even
// while #231/#233's slackdata pivot rework is in flight; the eventual
// store satisfies the same shape.
//
// Schema decision locked 2026-05-17: app-managed Map attribute on the
// existing PK/SK; no GSI, no SK reshape, no data migration (the table
// is empty pending Slack bot sandbox deploy).
type AliasStore interface {
	// BindChannelAlias atomically binds aliasName→resourceID on
	// (teamID, channelID). Implementations issue a DynamoDB UpdateItem
	// `SET alias_bindings.#a = :rid` with
	// `ConditionExpression: attribute_not_exists(alias_bindings.#a)` so
	// a duplicate alias name in the same channel returns
	// ErrAliasAlreadyBound rather than overwriting a teammate's
	// binding. Other aliases on the same channel are untouched.
	BindChannelAlias(ctx context.Context, teamID, channelID, aliasName, resourceID string) error
	// UnbindChannelAlias removes aliasName from (teamID, channelID).
	// Implementations issue `REMOVE alias_bindings.#a` with
	// `ConditionExpression: attribute_exists(alias_bindings.#a)`.
	// Returns ErrAliasNotFound when aliasName is not bound in the
	// channel. Other aliases on the same channel are untouched.
	UnbindChannelAlias(ctx context.Context, teamID, channelID, aliasName string) error
}

// ErrAliasAlreadyBound is returned by AliasStore implementations when
// BindChannelAlias is called for an aliasName that already has a
// binding in (teamID, channelID). Handlers use errors.Is to map this
// to the 409 / "alias already bound" friendly copy.
var ErrAliasAlreadyBound = slackdata.ErrAliasAlreadyBound

// ErrAliasNotFound is returned by AliasStore implementations when
// UnbindChannelAlias is called for an aliasName that has no binding
// in (teamID, channelID). Handlers use errors.Is to map this to the
// 404 / "alias not bound" friendly copy.
var ErrAliasNotFound = slackdata.ErrAliasNotFound

// resourceIDPrefix is the wire prefix on every qurl-service resource
// id. setalias discriminates URL targets from resource-id targets on
// this prefix (raw URLs vs `r_…`).
const resourceIDPrefix = "r_"

// aliasMaxLen caps alias length at the qurl-service schema's bound
// (mirrors qurl-service `qurl_resources.alias` GSI key length, nhp
// #1825). Parsing rejects above-cap aliases so the user sees a
// friendlier error than the eventual upstream rejection.
const aliasMaxLen = 64

// aliasCharsetPattern matches the recognized alias charset: lowercase
// alnum + dash, with the leading and trailing char alnum. Intentionally
// permissive on internal `--` runs because qurl-service's own
// authoritative validator (the sparse GSI key handler from nhp #1825)
// accepts them; the parser only enforces the leading/trailing rule,
// which is what surfaces with a friendlier error than punting
// downstream.
var aliasCharsetPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

// aliasUsage is the help-text body returned when setalias/unsetalias
// is invoked with an obvious typo. Centralized so the parser-rejection
// path and the missing-arg path share the same copy.
const aliasUsage = "Usage:\n• `/qurl setalias $<alias> <url-or-resource-id-or-$slug>`\n• `/qurl unsetalias $<alias>`\n\nAliases are lowercase alphanumeric + dashes, up to 64 chars."

// URL scheme constants. Lifted so the parser, validator, and any
// future caller can't drift on the literal string match — and so
// goconst's "3+ occurrences" rule stays clean as the parser grows.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// Parser-rejection user copy. These strings are returned via
// [parseAliasArgs] as the second return value (a user-facing message)
// when the grammar doesn't match — handlers route them straight into
// the ephemeral response, so the wire copy is reviewable in one
// place. Tests pin substrings, not whole-message identity, so
// expanding usage text won't churn the test surface.
//
// Plain strings instead of `error` because (a) these are surface
// copy not propagated errors, (b) sentence punctuation is right for
// the user but trips ST1005 if we wrap in `error`, and (c) the
// dispatcher needs the literal string anyway.
const (
	msgAliasTargetInvalid = "Target must be a URL (http/https), a resource id (`r_...`), or a tunnel slug (`$prod-dashboard`).\n\n" + aliasUsage
	msgAliasMissing       = "Missing alias.\n\n" + aliasUsage
	msgAliasNoSigil       = "Alias must start with `$` (e.g. `$staging`).\n\n" + aliasUsage
	msgAliasEmptyName     = "Missing alias name after `$`.\n\n" + aliasUsage
)

// aliasArgs is the parsed shape of a `/qurl setalias $a <target>` or
// `/qurl unsetalias $a` text body. Kept as a separate value type so
// the parser is unit-testable without spinning a full handler.
type aliasArgs struct {
	Alias  string // sigil stripped (no leading `$`)
	Target string // URL or `r_…` resource id; empty for unsetalias
}

// parseAliasArgs validates the trailing-arg shape of setalias /
// unsetalias.
//
// For setalias, `rest` must be `$<alias> <target>` (exactly 2 tokens).
// For unsetalias, `rest` must be `$<alias>` (exactly 1 token). When
// the grammar doesn't match, returns (nil, userCopy) — the second
// return is the ephemeral text to surface to the user, NOT a Go error
// to propagate. (User-facing copy carries sentence punctuation that
// ST1005 rejects on `error`-typed values.)
//
// Target validation is intentionally light: a `r_…` prefix routes
// through the resource-id branch, otherwise the token must parse as a
// URL with an http/https scheme. The deeper "is this an active
// resource?" check is the persistence layer's job.
func parseAliasArgs(text string, wantTarget bool) (parsed *aliasArgs, userMsg string) {
	tokens := strings.Fields(text)
	if wantTarget {
		if len(tokens) != 2 {
			return nil, aliasUsage
		}
	} else {
		if len(tokens) != 1 {
			return nil, aliasUsage
		}
	}

	alias, msg := requireAlias(tokens[0])
	if msg != "" {
		return nil, msg
	}
	out := &aliasArgs{Alias: alias}
	if !wantTarget {
		return out, ""
	}

	tgt := tokens[1]
	// Reject backticks and any non-printable rune before any further
	// parsing. The handler echoes the target into a Slack inline-code
	// fence (\`<tgt>\`) on the success-copy path, and the audit log
	// emits it on the happy path — backticks break the fence, control
	// bytes garble the log line, and both are footguns we close at
	// the parser rather than at the response. The admin-gate trust
	// model makes these rendering/logging hygiene, not security.
	for _, r := range tgt {
		if r == '`' || !unicode.IsPrint(r) {
			return nil, msgAliasTargetInvalid
		}
	}
	if strings.HasPrefix(tgt, resourceIDPrefix) {
		// `r_…` short-circuit. We don't enforce a deeper character set
		// here — qurl-service's resource-id validator is the
		// authoritative gate — but we DO reject the bare `r_` sigil
		// before writing it, so a junk DDB row never lands.
		if len(tgt) == len(resourceIDPrefix) {
			return nil, msgAliasTargetInvalid
		}
		out.Target = tgt
		return out, ""
	}
	if strings.HasPrefix(tgt, "$") {
		slug, msg := requireAlias(tgt)
		if msg != "" || !tunnelSlugPattern.MatchString(slug) {
			return nil, msgAliasTargetInvalid
		}
		out.Target = "$" + slug
		return out, ""
	}
	u, err := url.Parse(tgt)
	if err != nil || (u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS) || u.Host == "" {
		return nil, msgAliasTargetInvalid
	}
	out.Target = tgt
	return out, ""
}

// requireAlias checks that `tok` is `$<alias>` and returns the alias
// without the sigil. Mirrors the grammar in the broader parser
// (apps/slack/internal/parser.go on #228) so a future consolidation
// is a textual no-op.
//
// Returns (alias, "") on success and ("", userCopy) on rejection.
// See [parseAliasArgs] for the rationale on plain strings vs error.
func requireAlias(tok string) (alias, userMsg string) {
	if tok == "" {
		return "", msgAliasMissing
	}
	if !strings.HasPrefix(tok, "$") {
		return "", msgAliasNoSigil
	}
	alias = strings.TrimPrefix(tok, "$")
	if alias == "" {
		return "", msgAliasEmptyName
	}
	if len(alias) > aliasMaxLen {
		return "", fmt.Sprintf("Alias `$%s` is longer than %d characters.\n\n%s", alias, aliasMaxLen, aliasUsage)
	}
	if !aliasCharsetPattern.MatchString(alias) {
		return "", fmt.Sprintf("Alias `$%s` must be lowercase alphanumeric + dashes (no leading/trailing dash).\n\n%s", alias, aliasUsage)
	}
	return alias, ""
}

// aliasSyncTimeout caps the sync alias-verb deadline tight enough to
// fail inside Slack's 3-second slash-command ack window. Two DDB
// calls (lookup + write) typically resolve in <100ms, so 2.5s leaves
// headroom while keeping the bot's failure mode "we surfaced an
// error" rather than "Slack reported timeout while we kept working
// and the user retried." Re-evaluate (move to runAsync + response_url)
// only if DDB tail latency starts exceeding this budget in practice.
//
// `var` (not `const`) so tests can swap in a short budget without
// dropping a 2.5-second real-time wait into the suite. Production
// code never mutates this — the test path is the only writer.
var aliasSyncTimeout = 2500 * time.Millisecond

// aliasPreamble is the shared prelude for both alias verbs after
// argument parsing: pull team_id + channel_id off the form, verify
// the AliasStore is wired, and set up the worker context. Returns
// (ctx, cancel, teamID, channelID, ok); on !ok the helper has
// already written the user-facing response and the caller must
// return without dialing the store. cancel is always callable
// (no-op when !ok) so caller-side `defer cancel()` is unconditional.
func (h *Handler) aliasPreamble(w http.ResponseWriter, values url.Values, verb string) (ctx context.Context, cancel context.CancelFunc, teamID, channelID string, ok bool) {
	cancel = func() {}
	teamID = strings.TrimSpace(values.Get(fieldTeamID))
	channelID = strings.TrimSpace(values.Get(fieldChannelID))
	if teamID == "" || channelID == "" {
		// Slack always sends these on a slash-command payload; the
		// guard catches a malformed test fixture or a future
		// regression in form parsing rather than silently writing a
		// row with empty keys.
		slog.Warn("alias verb missing team_id or channel_id", "verb", verb, "team_id_present", teamID != "", "channel_id_present", channelID != "")
		respondSlack(w, "Could not read your Slack workspace or channel ID from the command payload.")
		return
	}
	if h.aliasStore == nil {
		// Soft-fail when AliasStore is not configured (sandbox /
		// pre-#231/#233 deploys). Surfacing a configuration error
		// rather than silently dropping makes the bot's state
		// debuggable from the operator side.
		slog.Warn("alias verb invoked with no AliasStore wired — refusing", "verb", verb)
		respondSlack(w, "Alias storage is not configured on this Slack bot deployment. Contact the operator.")
		return
	}
	ctx, cancel = context.WithTimeout(h.baseCtx, aliasSyncTimeout)
	ok = true
	return
}

// handleSetAlias routes `/qurl setalias $<alias> <target>`.
//
// **Admin restriction:** This handler is admin-gated at the Slack app
// manifest level — the `/qurl setalias` command must be declared
// admin-only in the install config. The same posture is used for
// `/qurl setup` (see handleSetup) — gating in the manifest avoids an
// extra Slack API round-trip per invocation. The CR feedback on the
// old #230 (claude-bot review id 2026-05-10) flagged "admin gate
// before alias resolution" as an info-disclosure surface. Moving the
// gate to the manifest closes that gap structurally: a non-admin's
// command never reaches this handler.
//
// **Synchronous reply contract:** sets reply ephemerally per the
// admin-verb posture in the rollout doc — user feedback is
// admin-only, so default-public would be wrong. The qurl-bot-slack
// stack today returns the user copy directly on the slash-command
// HTTP body; the async response_url path (postResponse) is used only
// when the work cannot complete inside Slack's 3-second ack window.
// Alias upsert is a single DDB UpdateItem — synchronous is fine.
func (h *Handler) handleSetAlias(w http.ResponseWriter, values url.Values) {
	text := strings.TrimSpace(values.Get(fieldText))
	rest := strings.TrimSpace(strings.TrimPrefix(text, "setalias"))
	if rest == text {
		rest = strings.TrimSpace(strings.TrimPrefix(text, "set-alias"))
	}

	args, userMsg := parseAliasArgs(rest, true)
	if userMsg != "" {
		respondSlack(w, userMsg)
		return
	}

	ctx, cancel, teamID, channelID, ok := h.aliasPreamble(w, values, "setalias")
	if !ok {
		return
	}
	defer cancel()

	target := args.Target
	if strings.HasPrefix(target, "$") {
		resourceID, err := h.resolveTunnelSlugAliasTarget(ctx, teamID, strings.TrimPrefix(target, "$"))
		if err != nil {
			slog.Error("setalias tunnel slug target resolution failed", "error", err, "team_id", teamID, "channel_id", channelID, "alias", args.Alias) //nolint:gosec // G706: slog escapes control bytes in attribute values; team/channel/alias are Slack IDs or validated slug-like input.
			respondSlack(w, sanitizeAPIError(err, "Failed to resolve tunnel slug"))
			return
		}
		target = resourceID
	}

	// Multi-alias write: BindChannelAlias issues an atomic UpdateItem
	// on alias_bindings.#a with attribute_not_exists. A second alias
	// name on the same channel succeeds (different map key); a
	// duplicate alias name surfaces as ErrAliasAlreadyBound and is
	// rendered as a refusal. The refusal copy names only the alias
	// (not its bound target) to keep the info-disclosure surface
	// narrow — claude-bot review #5 on the prior single-alias version.
	err := h.aliasStore.BindChannelAlias(ctx, teamID, channelID, args.Alias, target)
	if errors.Is(err, ErrAliasAlreadyBound) {
		respondSlack(w, fmt.Sprintf("Alias `$%s` is already bound in this channel. Run `/qurl unsetalias $%s` first, or pick a different alias.", args.Alias, args.Alias))
		return
	}
	if err != nil {
		slog.Error("setalias write failed", "error", err, "team_id", teamID, "channel_id", channelID, "alias", args.Alias) //nolint:gosec // G706: slog escapes control bytes in attribute values; team/channel/alias are validated upstream.
		respondSlack(w, "Failed to update alias. Please try again.")
		return
	}
	// Admin-verb audit trail: log the bound (alias, target) pair on
	// success so post-incident reconstruction doesn't depend on
	// re-querying the DDB table at the time of the question. team/channel/alias
	// are validated upstream; target is redacted (userinfo + raw query
	// stripped) so credentials embedded by a setting admin don't
	// land in operator-visible logs where the readership is wider
	// than the writer's admin scope.
	slog.Info("alias bound", "team_id", teamID, "channel_id", channelID, "alias", args.Alias, "target", redactURLForLog(target)) //nolint:gosec // G706: slog escapes control bytes in attribute values; target is redacted before logging.
	respondSlack(w, fmt.Sprintf("Alias `$%s` now points to `%s` in this channel.", args.Alias, target))
}

func (h *Handler) resolveTunnelSlugAliasTarget(ctx context.Context, teamID, slug string) (string, error) {
	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		return "", err
	}
	resource, err := c.CreateResource(ctx, &client.CreateResourceInput{
		Type:         client.ResourceTypeTunnel,
		Slug:         slug,
		FindOrCreate: true,
		Description:  "Slack alias target for " + slug,
	})
	if err != nil {
		return "", err
	}
	return resource.ResourceID, nil
}

// redactURLForLog strips userinfo (e.g. `user:token@`) and any raw
// query string from a setalias target before it lands in operator
// logs. The success-copy path still shows the verbatim target to the
// admin who set it, but the audit-log readership is typically wider
// than the manifest-gated admin pool and shouldn't see embedded
// credentials. Non-URL targets (`r_…` resource ids, or anything that
// fails to re-parse) are returned unchanged — the parser has already
// fenced backticks and non-printable runes upstream.
func redactURLForLog(target string) string {
	if strings.HasPrefix(target, resourceIDPrefix) {
		return target
	}
	u, err := url.Parse(target)
	if err != nil {
		return target
	}
	redacted := *u
	redacted.User = nil
	redacted.RawQuery = ""
	redacted.Fragment = ""
	return redacted.String()
}

// handleUnsetAlias routes `/qurl unsetalias $<alias>`.
//
// **Admin restriction:** Same Slack-manifest-level gate as
// handleSetAlias — see that comment. The CR feedback's
// "info-disclosure" concern (a non-admin probing alias existence via
// the response delta) is closed structurally by the manifest gate.
//
// **Not-bound posture:** UnbindChannelAlias is conditional on
// attribute_exists(alias_bindings.#a) — clearing an alias that
// isn't bound surfaces as ErrAliasNotFound and is rendered as
// "no such alias on this channel." Other aliases on the same channel
// are untouched.
func (h *Handler) handleUnsetAlias(w http.ResponseWriter, values url.Values) {
	text := strings.TrimSpace(values.Get(fieldText))
	rest := strings.TrimSpace(strings.TrimPrefix(text, "unsetalias"))

	args, userMsg := parseAliasArgs(rest, false)
	if userMsg != "" {
		respondSlack(w, userMsg)
		return
	}

	ctx, cancel, teamID, channelID, ok := h.aliasPreamble(w, values, "unsetalias")
	if !ok {
		return
	}
	defer cancel()

	err := h.aliasStore.UnbindChannelAlias(ctx, teamID, channelID, args.Alias)
	if errors.Is(err, ErrAliasNotFound) {
		respondSlack(w, fmt.Sprintf("Alias `$%s` is not bound in this channel. Nothing to clear.", args.Alias))
		return
	}
	if err != nil {
		slog.Error("unsetalias write failed", "error", err, "team_id", teamID, "channel_id", channelID, "alias", args.Alias) //nolint:gosec // G706: slog escapes control bytes in attribute values; team/channel/alias are validated upstream.
		respondSlack(w, "Failed to clear alias. Please try again.")
		return
	}
	// Admin-verb audit trail: counterpart to the setalias "alias bound"
	// audit line. team/channel/alias are validated upstream.
	slog.Info("alias cleared", "team_id", teamID, "channel_id", channelID, "alias", args.Alias) //nolint:gosec // G706: slog escapes control bytes in attribute values; values are validated upstream.
	respondSlack(w, fmt.Sprintf("Alias `$%s` is no longer bound to this channel.", args.Alias))
}
