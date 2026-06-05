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
const aliasUsage = "Usage:\n• `/qurl-admin set-alias $<alias> $<id>`\n• `/qurl-admin unset-alias $<alias>`\n\nAliases are lowercase alphanumeric + dashes, up to 64 chars."

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

	msgAliasTargetInvalid = "Target must be a qURL Connector ID (`$prod-dashboard`). qURL Connector IDs are 3-64 chars, start with a lowercase letter, contain lowercase letters/numbers/hyphens, and end with a letter or number.\n\n" + aliasUsage
	msgAliasMissing       = reasonAliasMissing + "\n\n" + aliasUsage
	msgAliasNoSigil       = reasonAliasNoSigil + "\n\n" + aliasUsage
	msgAliasEmptyName     = reasonAliasEmptyName + "\n\n" + aliasUsage
)

// msgAliasTargetNotTunnel is the rejection for a set-alias target that
// isn't a `$<slug>` at all — a raw URL, an `r_<id>` resource id, or a
// bare token missing the `$` sigil. set-alias only points an alias at a
// tunnel `$slug` now (the slug→resource_id resolution is the admin act
// that authorizes the resource for use in the channel). The copy leads
// with the tunnel-slug form so a sigil-less typo stays actionable, and
// mentions URLs/resource-ids only parenthetically (the forms migrating
// admins are most likely to try) rather than asserting the admin typed
// one. Distinct from [msgAliasTargetInvalid], which fires once a
// `$`-prefixed target fails the tunnel-slug grammar.
const msgAliasTargetNotTunnel = "`/qurl-admin set-alias` points an alias at a qURL Connector ID — `/qurl-admin set-alias $<alias> $<id>`. (Raw URLs and resource IDs aren't supported targets.)"

// aliasArgs is the parsed shape of a `/qurl-admin set-alias $a <target>` or
// `/qurl-admin unset-alias $a` text body. Kept as a separate value type so
// the parser is unit-testable without spinning a full handler.
type aliasArgs struct {
	Alias  string // sigil stripped (no leading `$`)
	Target string // tunnel `$<slug>` (sigil kept); empty for unsetalias
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
// Tunnels-only target: the only accepted setalias target is a tunnel
// `$<slug>`. Any target that doesn't start with `$` — a raw URL, an
// `r_<id>`, or a sigil-less typo — is rejected with the uniform
// [msgAliasTargetNotTunnel] copy (so a valid `r_<id>` and a `r_<typo>`
// read the same). A `$`-prefixed token that then fails the tunnel-slug
// grammar gets [msgAliasTargetInvalid] + the usage dump. The deeper "is
// this an active tunnel?" check is the persistence/qurl-service layer's
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
	// parsing. The handler echoes the slug into a Slack inline-code
	// fence (\`$<slug>\`) on the success-copy path, and the audit log
	// emits it on the happy path — backticks break the fence, control
	// bytes garble the log line, and both are footguns we close at
	// the parser rather than at the response. The admin-gate trust
	// model makes these rendering/logging hygiene, not security.
	for _, r := range tgt {
		if r == '`' || !unicode.IsPrint(r) {
			return nil, msgAliasTargetInvalid
		}
	}
	// Tunnels-only: a tunnel `$slug` is the only accepted target. Reject
	// ALL non-`$` targets — URLs, `r_<id>`s, and sigil-less typos alike —
	// with the one not-a-tunnel message so the copy is uniform (a
	// `r_<typo>` and a valid `r_<id>` should read the same). `$<slug>`
	// then validates against the tunnel-slug grammar (which diverges from
	// the alias grammar: slugs must start with a letter and be at least
	// three characters).
	if !strings.HasPrefix(tgt, "$") {
		return nil, msgAliasTargetNotTunnel
	}
	slug, msg := requireAlias(tgt)
	if msg != "" || !tunnelSlugPattern.MatchString(slug) {
		return nil, msgAliasTargetInvalid
	}
	out.Target = "$" + slug
	return out, ""
}

func validateChannelShortcutToken(tok string) (alias, reason string) {
	return validateAliasTokenForNoun(tok, "Channel alias", "channel alias")
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
// created here — the async set-alias path runs on runAsync's own ctx
// (bounded by [asyncWorkTimeout], 25s, so a wedged upstream can't hang
// the slug-resolve + bind indefinitely), so only the synchronous unset
// path needs the shorter sync timeout ([aliasPreamble]).
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
//
// set-alias does NOT use this: a slug target resolves through
// qurl-service before the DDB write, so it runs on [runAsync]'s own ctx
// (~25s) and calls [aliasValidate] directly — it never needs the sync
// timer this helper layers on.
func (h *Handler) aliasPreamble(w http.ResponseWriter, values url.Values, verb string) (ctx context.Context, cancel context.CancelFunc, teamID, channelID string, ok bool) {
	cancel = func() {}
	teamID, channelID, ok = h.aliasValidate(w, values, verb)
	if !ok {
		return
	}
	ctx, cancel = context.WithTimeout(h.baseCtx, aliasSyncTimeout)
	return
}

// requireAliasAdminGate is the in-code admin gate shared by set-alias and
// unset-alias: Slack does not restrict a slash command to workspace admins,
// so the `/qurl-admin` registration can't be the gate. It checks AdminStore
// is wired (requireAdminStoreSync — guarantees AdminStore, == aliasStore in
// prod per cmd/main.go SetAliasStore, is non-nil for CheckAdmin) and that
// the caller is a qURL admin (requireAdminSync). Returns false (and has
// written the reply) when either fails; callers `if !h.requireAliasAdminGate(...) { return }`.
func (h *Handler) requireAliasAdminGate(w http.ResponseWriter, teamID string, values url.Values, action AdminAction) bool {
	if !h.requireAdminStoreSync(w) {
		return false
	}
	return h.requireAdminSync(w, teamID, strings.TrimSpace(values.Get(fieldUserID)), action)
}

// handleSetAlias routes `/qurl-admin set-alias $<alias> <target>`.
//
// **Admin restriction:** Enforced in code via requireAdminSync (a
// CheckAdmin lookup against AdminStore), the same gate handleExposeConnector
// and the admin membership verbs use. Slack does NOT restrict a slash command
// to workspace admins — the "admins only" label on the `/qurl-admin`
// registration is display text, not enforcement — so this code gate is the
// only real boundary. It runs before alias resolution, so the CR feedback
// on the old #230 (claude-bot review 2026-05-10) that flagged "admin gate
// before alias resolution" stays addressed: a non-admin is denied before
// any tunnel/resource lookup. (setup is different — it lives on the open
// `/qurl` command and is guarded at the OAuth-callback bind layer instead;
// see handleSetup.)
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

	// parseAliasArgs enforces the tunnels-only target gate: it accepts
	// only a tunnel `$slug` and rejects URL / `r_<id>` targets with
	// msgAliasTargetNotTunnel (and malformed targets with the usage
	// dump). So args.Target here is always `$<slug>`, and the only act
	// left is to resolve the slug and bind.
	args, userMsg := parseAliasArgs(rest, true)
	if userMsg != "" {
		respondSlack(w, userMsg)
		return
	}

	// Slug target resolves through qurl-service before the DDB write, so
	// it runs async on runAsync's own ctx — aliasValidate (no timer)
	// rather than aliasPreamble.
	teamID, channelID, ok := h.aliasValidate(w, values, "setalias")
	if !ok {
		return
	}

	// Admin gate, in code (see requireAliasAdminGate). Runs after
	// aliasValidate (team/channel IDs + store-wired check) but before the
	// slug resolve + DDB bind, so a non-admin is denied before any resource
	// interaction. The parse/validate usage hints (parseAliasArgs above) are
	// deliberately surfaced before the gate: the grammar is public — it's in
	// the ungated `/qurl-admin help` — so a non-admin's malformed attempt gets
	// the usage reply rather than a bare denial. The gate's guarantee is "no
	// slug resolve or store mutation before the admin check," not "no parser
	// feedback to non-admins."
	if !h.requireAliasAdminGate(w, teamID, values, AdminActionSetAlias) {
		return
	}
	slug := strings.TrimPrefix(args.Target, "$")
	h.runAsync(w, "setalias_slug", values, func(ctx context.Context, log *slog.Logger) {
		msg := h.resolveAndBindTunnelSlugAlias(ctx, log, teamID, channelID, args.Alias, slug)
		_ = h.postResponse(log, values.Get(fieldResponseURL), msg)
	})
}

// resolveAndBindTunnelSlugAlias resolves a tunnel `$slug` to its
// resource_id, binds `alias`→resource_id on (teamID, channelID), and
// renders the admin-facing result. set-alias is the only caller and the
// only target form is a slug, so the bind always carries an opaque
// `r_<id>` — the success copy deliberately echoes the `$slug` the admin
// typed (the noun `/qurl list` shows) rather than the internal
// resource_id.
//
// TOCTOU note: the slug→resource_id resolve and the DDB bind are two
// steps; if the tunnel is deleted upstream between them, the binding
// lands pointing at a dead resource. That's acceptable — a stale
// binding is caught at mint time: the bound resource_id flows to the
// qurl-service mint call, which rejects a deleted resource, surfaced to
// the user via [mapMintError]. So the worst case is a deferred,
// well-handled error rather than a silent success.
func (h *Handler) resolveAndBindTunnelSlugAlias(ctx context.Context, log *slog.Logger, teamID, channelID, alias, slug string) string {
	resourceID, err := h.resolveTunnelSlugAliasTarget(ctx, teamID, slug)
	if err != nil {
		log.Error("setalias tunnel slug target resolution failed", "error", err, "team_id", teamID, "channel_id", channelID, "alias", alias, "slug", slug)
		if errors.Is(err, errTunnelSlugNotFound) {
			return fmt.Sprintf("qURL Connector `$%s` was not found. Run `/qurl-admin expose-connector %s` first, then retry this alias.", slug, slug)
		}
		return sanitizeAPIError(err, "Failed to resolve qURL Connector ID")
	}

	// Multi-alias write: BindChannelAlias issues an atomic UpdateItem
	// on alias_bindings.#a with attribute_not_exists. A second alias
	// name on the same channel succeeds (different map key); a
	// duplicate alias name surfaces as slackdata.ErrAliasAlreadyBound and is
	// rendered as a refusal. The refusal copy names only the alias
	// (not its bound target) to keep the info-disclosure surface
	// narrow — claude-bot review #5 on the prior single-alias version.
	err = h.aliasStore.BindChannelAlias(ctx, teamID, channelID, alias, resourceID)
	if errors.Is(err, slackdata.ErrAliasAlreadyBound) {
		return fmt.Sprintf("Alias `$%s` is already bound in this channel. Run `/qurl-admin unset-alias $%s` first, or pick a different alias.", alias, alias)
	}
	if err != nil {
		log.Error("setalias write failed", "error", err, "team_id", teamID, "channel_id", channelID, "alias", alias)
		return "Failed to update alias. Please try again."
	}
	// Admin-verb audit trail: log the bound (alias, slug, resource_id)
	// triple on success so post-incident reconstruction doesn't depend
	// on re-querying the DDB table. Every logged field is opaque or
	// validated (team/channel/alias/slug upstream; resource_id is a
	// server-minted `r_<id>` with no embeddable credentials), so no
	// redaction is needed.
	logAliasBound(teamID, channelID, alias, slug, resourceID)
	return fmt.Sprintf("Alias `$%s` now points to qURL Connector `$%s` in this channel.", alias, slug)
}

func logAliasBound(teamID, channelID, alias, slug, resourceID string) {
	slog.LogAttrs(context.Background(), slog.LevelInfo, "alias bound",
		slog.String("team_id", teamID),
		slog.String("channel_id", channelID),
		slog.String("alias", alias),
		slog.String("slug", slug),
		slog.String("resource_id", resourceID),
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

// handleUnsetAlias routes `/qurl-admin unset-alias $<alias>`.
//
// **Admin restriction:** Same in-code requireAdminSync gate as
// handleSetAlias — see that comment. The CR "info-disclosure" concern (a
// non-admin probing alias existence via the response delta) is closed by
// the gate running before any store read.
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

	// Admin gate, in code (see requireAliasAdminGate). Runs after
	// aliasPreamble (IDs + store-wired check) but before UnbindChannelAlias,
	// so a non-admin is denied before any store write.
	if !h.requireAliasAdminGate(w, teamID, values, AdminActionUnsetAlias) {
		return
	}

	err := h.aliasStore.UnbindChannelAlias(ctx, teamID, channelID, args.Alias)
	if errors.Is(err, slackdata.ErrAliasNotFound) {
		respondSlack(w, fmt.Sprintf("Alias `$%s` is not bound in this channel. Nothing to clear.", args.Alias))
		return
	}
	if err != nil {
		slog.Error("unsetalias write failed", "error", err, "team_id", teamID, "channel_id", channelID, "alias", args.Alias)
		respondSlack(w, "Failed to clear alias. Please try again.")
		return
	}
	// Admin-verb audit trail: counterpart to the setalias "alias bound"
	// audit line. team/channel/alias are validated upstream.
	slog.Info("alias cleared", "team_id", teamID, "channel_id", channelID, "alias", args.Alias)
	respondSlack(w, fmt.Sprintf("Alias `$%s` is no longer bound to this channel.", args.Alias))
}
