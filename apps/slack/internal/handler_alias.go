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
// distinct resource. The concrete slackdata.Store satisfies this small
// interface, which keeps handler tests focused on Slack behavior instead of
// DynamoDB plumbing.
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
	// slackdata.ErrAliasAlreadyBound rather than overwriting a teammate's
	// binding. Other aliases on the same channel are untouched.
	BindChannelAlias(ctx context.Context, teamID, channelID, aliasName, resourceID string) error
	// UnbindChannelAlias removes aliasName from (teamID, channelID).
	// Implementations issue `REMOVE alias_bindings.#a` with
	// `ConditionExpression: attribute_exists(alias_bindings.#a)`.
	// Returns slackdata.ErrAliasNotFound when aliasName is not bound in the
	// channel. Other aliases on the same channel are untouched.
	UnbindChannelAlias(ctx context.Context, teamID, channelID, aliasName string) error
}

var errTunnelSlugNotFound = errors.New("tunnel slug not found")

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
const aliasUsage = "Usage:\n• `/qurl-admin set-alias $<alias> $<slug>`\n• `/qurl-admin unset-alias $<alias>`\n\nAliases are lowercase alphanumeric + dashes, up to 64 chars."

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
	reasonAliasMissing   = "Missing alias."
	reasonAliasNoSigil   = "Alias must start with `$` (e.g. `$staging`)."
	reasonAliasEmptyName = "Missing alias name after `$`."

	msgAliasTargetInvalid = "Target must be a tunnel slug (`$prod-dashboard`). Tunnel slugs are 3-64 chars, start with a lowercase letter, contain lowercase letters/numbers/hyphens, and end with a letter or number.\n\n" + aliasUsage
	msgAliasMissing       = reasonAliasMissing + "\n\n" + aliasUsage
	msgAliasNoSigil       = reasonAliasNoSigil + "\n\n" + aliasUsage
	msgAliasEmptyName     = reasonAliasEmptyName + "\n\n" + aliasUsage
)

// msgAliasTargetNotTunnel is the rejection surfaced when `/qurl
// set-alias` is handed a well-formed but non-tunnel target — a raw URL
// or an `r_<id>` resource id. set-alias only points an alias at a
// tunnel `$slug` now (the slug→resource_id resolution is the admin act
// that authorizes the resource for use in the channel); URL and
// resource-id targets are no longer accepted. Distinct from
// [msgAliasTargetInvalid] (a malformed/garbage target) so the admin
// who typed a valid-but-unsupported target sees why it was refused
// rather than a generic usage dump.
const msgAliasTargetNotTunnel = "`/qurl-admin set-alias` points an alias at a tunnel: `/qurl-admin set-alias $<alias> $<slug>`. URLs and resource IDs aren't supported targets."

// aliasArgs is the parsed shape of a `/qurl-admin set-alias $a <target>` or
// `/qurl-admin unset-alias $a` text body. Kept as a separate value type so
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
// Target validation here is only well-formedness, not policy: a `r_…`
// prefix routes through the resource-id branch, a `$slug` through the
// tunnel-slug branch, and anything else must parse as an http/https
// URL. The tunnels-only POLICY gate (rejecting URL and `r_…` targets)
// lives in [Handler.handleSetAlias], not here — this parser still
// accepts all three shapes so the handler can reject the unsupported
// two with a specific message rather than a generic usage dump. The
// deeper "is this an active resource?" check is the persistence layer's
// job.
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
		// Alias and tunnel-slug grammars intentionally diverge:
		// aliases may start with a digit, while tunnel slugs must
		// start with a letter and be at least three characters.
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

func validateChannelShortcutToken(tok string) (alias, reason string) {
	return validateAliasTokenForNoun(tok, "Channel shortcut", "channel shortcut")
}

func validateAliasTokenForNoun(tok, noun, nounLower string) (alias, reason string) {
	if tok == "" {
		return "", fmt.Sprintf("Missing %s.", nounLower)
	}
	if !strings.HasPrefix(tok, "$") {
		return "", noun + " must start with `$` (e.g. `$staging`)."
	}
	alias = strings.TrimPrefix(tok, "$")
	if alias == "" {
		return "", fmt.Sprintf("Missing %s name after `$`.", nounLower)
	}
	if len(alias) > aliasMaxLen {
		return "", fmt.Sprintf("%s `$%s` is longer than %d characters.", noun, alias, aliasMaxLen)
	}
	if !aliasCharsetPattern.MatchString(alias) {
		return "", fmt.Sprintf("%s `$%s` must be lowercase alphanumeric + dashes (no leading/trailing dash).", noun, alias)
	}
	return alias, ""
}

// requireAlias checks that `tok` is `$<alias>` and returns copy suitable for
// slash-command responses. See [parseAliasArgs] for the rationale on plain
// strings vs error.
func requireAlias(tok string) (alias, userMsg string) {
	alias, reason := validateAliasTokenForNoun(tok, "Alias", "alias")
	if reason != "" {
		return "", reason + "\n\n" + aliasUsage
	}
	return alias, ""
}

// aliasSyncTimeout caps the sync alias-verb deadline tight enough to
// fail inside Slack's 3-second slash-command ack window. Two DDB
// calls (lookup + write) typically resolve in <100ms, so 2.5s leaves
// headroom while keeping the bot's failure mode "we surfaced an
// error" rather than "Slack reported timeout while we kept working
// and the user retried." Slug-targeted set-alias uses runAsync because
// it must first resolve the slug through qurl-service.
//
// `var` (not `const`) so tests can swap in a short budget without
// dropping a 2.5-second real-time wait into the suite. Production
// code never mutates this — the test path is the only writer.
var aliasSyncTimeout = 2500 * time.Millisecond

// aliasValidate is the shared validation prelude for both alias verbs
// after argument parsing: pull team_id + channel_id off the form and
// verify the AliasStore is wired. Returns (teamID, channelID, ok); on
// !ok the helper has already written the user-facing response and the
// caller must return without dialing the store. No worker context is
// created here — the async set-alias path uses runAsync's own ctx, so
// only the synchronous unset path needs the timeout ([aliasPreamble]).
func (h *Handler) aliasValidate(w http.ResponseWriter, values url.Values, verb string) (teamID, channelID string, ok bool) {
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
	ok = true
	return
}

// aliasPreamble is [aliasValidate] plus a synchronous worker context
// bounded by [aliasSyncTimeout]. Used by the synchronous unset-alias
// verb. Returns (ctx, cancel, teamID, channelID, ok); on !ok the helper
// has already written the response and cancel is a callable no-op so
// caller-side `defer cancel()` is unconditional.
func (h *Handler) aliasPreamble(w http.ResponseWriter, values url.Values, verb string) (ctx context.Context, cancel context.CancelFunc, teamID, channelID string, ok bool) {
	cancel = func() {}
	teamID, channelID, ok = h.aliasValidate(w, values, verb)
	if !ok {
		return
	}
	ctx, cancel = context.WithTimeout(h.baseCtx, aliasSyncTimeout)
	return
}

// handleSetAlias routes `/qurl-admin set-alias $<alias> <target>`.
//
// **Admin restriction:** This handler is admin-gated at the Slack app
// config level — the whole `/qurl-admin` command (which carries every
// admin verb except setup) must be declared admin-only in the install
// config. Gating in the app config avoids an extra Slack API round-trip
// per invocation. The CR feedback on the old #230 (claude-bot review id
// 2026-05-10) flagged "admin gate before alias resolution" as an
// info-disclosure surface. Moving the gate to the app config closes that
// gap structurally: a non-admin's command never reaches this handler.
// (setup is different — it lives on the open `/qurl` command and is
// guarded at the OAuth-callback bind layer instead; see handleSetup.)
//
// **Target contract:** the only accepted target is a tunnel `$slug`.
// A raw URL or an `r_<id>` resource id is rejected synchronously with
// [msgAliasTargetNotTunnel] — Slack mints tunnels, not arbitrary URLs.
// Slug targets do a qurl-service lookup before the DDB write, so they
// ack immediately and post the final result (ephemerally — admin-verb
// feedback is admin-only) via response_url.
func (h *Handler) handleSetAlias(w http.ResponseWriter, values url.Values) {
	text := strings.TrimSpace(values.Get(fieldText))
	rest := stripSetAliasPrefix(text)

	args, userMsg := parseAliasArgs(rest, true)
	if userMsg != "" {
		respondSlack(w, userMsg)
		return
	}

	// Tunnels-only target gate: set-alias points an alias at a tunnel
	// `$slug`. A well-formed URL or `r_<id>` parses cleanly above but is
	// no longer an accepted target — reject it with the specific
	// not-a-tunnel copy (vs the generic usage dump a malformed target
	// gets). The slug→resource_id resolution that follows is the admin
	// act that authorizes the resource in this channel.
	if !strings.HasPrefix(args.Target, "$") {
		respondSlack(w, msgAliasTargetNotTunnel)
		return
	}

	// Slug target resolves through qurl-service before the DDB write, so
	// it runs async on runAsync's own ctx — aliasValidate (no timer)
	// rather than aliasPreamble.
	teamID, channelID, ok := h.aliasValidate(w, values, "setalias")
	if !ok {
		return
	}
	slug := strings.TrimPrefix(args.Target, "$")
	h.runAsync(w, "setalias_slug", values, func(ctx context.Context, log *slog.Logger) {
		msg := h.resolveAndBindTunnelSlugAlias(ctx, log, teamID, channelID, args.Alias, slug)
		_ = h.postResponse(log, values.Get(fieldResponseURL), msg)
	})
}

func (h *Handler) resolveAndBindTunnelSlugAlias(ctx context.Context, log *slog.Logger, teamID, channelID, alias, slug string) string {
	resourceID, err := h.resolveTunnelSlugAliasTarget(ctx, teamID, slug)
	if err != nil {
		log.Error("setalias tunnel slug target resolution failed", "error", err, "alias", alias)
		if errors.Is(err, errTunnelSlugNotFound) {
			return fmt.Sprintf("Tunnel slug `$%s` was not found. Run `/qurl-admin tunnel install %s` first, then retry this alias.", slug, slug)
		}
		return sanitizeAPIError(err, "Failed to resolve tunnel slug")
	}
	msg, err := h.bindAliasTarget(ctx, teamID, channelID, alias, resourceID)
	if err != nil {
		log.Error("setalias write failed", "error", err, "alias", alias)
	}
	return msg
}

func (h *Handler) bindAliasTarget(ctx context.Context, teamID, channelID, alias, target string) (string, error) {
	// Multi-alias write: BindChannelAlias issues an atomic UpdateItem
	// on alias_bindings.#a with attribute_not_exists. A second alias
	// name on the same channel succeeds (different map key); a
	// duplicate alias name surfaces as slackdata.ErrAliasAlreadyBound and is
	// rendered as a refusal. The refusal copy names only the alias
	// (not its bound target) to keep the info-disclosure surface
	// narrow — claude-bot review #5 on the prior single-alias version.
	err := h.aliasStore.BindChannelAlias(ctx, teamID, channelID, alias, target)
	if errors.Is(err, slackdata.ErrAliasAlreadyBound) {
		return fmt.Sprintf("Alias `$%s` is already bound in this channel. Run `/qurl-admin unset-alias $%s` first, or pick a different alias.", alias, alias), nil
	}
	if err != nil {
		return "Failed to update alias. Please try again.", err
	}
	// Admin-verb audit trail: log the bound (alias, target) pair on
	// success so post-incident reconstruction doesn't depend on
	// re-querying the DDB table at the time of the question. team/channel/alias
	// are validated upstream; target is redacted (userinfo + raw query
	// stripped) so credentials embedded by a setting admin don't
	// land in operator-visible logs where the readership is wider
	// than the writer's admin scope.
	logAliasBound(teamID, channelID, alias, target)
	return fmt.Sprintf("Alias `$%s` now points to `%s` in this channel.", alias, target), nil
}

func logAliasBound(teamID, channelID, alias, target string) {
	slog.LogAttrs(context.Background(), slog.LevelInfo, "alias bound",
		slog.String("team_id", teamID),
		slog.String("channel_id", channelID),
		slog.String("alias", alias),
		slog.String("target", redactURLForLog(target)),
	)
}

func (h *Handler) resolveTunnelSlugAliasTarget(ctx context.Context, teamID, slug string) (string, error) {
	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		return "", err
	}
	page, err := c.ListResources(ctx, client.ListResourcesInput{Slug: slug})
	if err != nil {
		return "", err
	}
	for i := range page.Resources {
		resource := &page.Resources[i]
		// Defense-in-depth: the server's `?slug=` filter is single-
		// purpose, but re-assert type/slug/active here so an upstream
		// regression can't leak a non-tunnel, wrong-slug, or revoked
		// resource into mintable state (this resolves the resource_id
		// that both /qurl-admin set-alias and /qurl get then mint against).
		if resource.Type == client.ResourceTypeTunnel && resource.Slug == slug && resource.Status == client.StatusActive {
			return resource.ResourceID, nil
		}
	}
	return "", errTunnelSlugNotFound
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

// handleUnsetAlias routes `/qurl-admin unset-alias $<alias>`.
//
// **Admin restriction:** Same Slack-manifest-level gate as
// handleSetAlias — see that comment. The CR feedback's
// "info-disclosure" concern (a non-admin probing alias existence via
// the response delta) is closed structurally by the manifest gate.
//
// **Not-bound posture:** UnbindChannelAlias is conditional on
// attribute_exists(alias_bindings.#a) — clearing an alias that
// isn't bound surfaces as slackdata.ErrAliasNotFound and is rendered as
// "no such alias on this channel." Other aliases on the same channel
// are untouched.
func (h *Handler) handleUnsetAlias(w http.ResponseWriter, values url.Values) {
	text := strings.TrimSpace(values.Get(fieldText))
	rest := stripUnsetAliasPrefix(text)

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
	if errors.Is(err, slackdata.ErrAliasNotFound) {
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
