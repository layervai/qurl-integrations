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
)

// AliasStore is the persistence surface the alias verbs depend on.
//
// One alias is bound per (slack_team_id, slack_channel_id) row by
// design — this matches the post-pivot channel_policies table shape
// owned by qurl-bot-slack-side terraform (modules/qurl-slack-ddb,
// SLACK_QURL_ROLLOUT.md 2026-05-12 architectural update). A small
// interface here rather than a direct dep on the slackdata package
// keeps this PR shippable against main even while #231/#233's
// slackdata pivot rework is in flight; the eventual store satisfies
// the same shape.
//
// **Schema gap (out-of-scope here):** the pre-pivot UX promised
// multiple aliases per channel, but the post-pivot row carries a
// single alias attribute per (team, channel). The right fix is a
// schema reshape (per-alias SK, or an `aliases` SS attribute) tracked
// separately — see qurl-integrations #233's comment thread. The
// verbs here intentionally enforce one-alias-per-channel and surface
// the existing-different-alias case with a refusal message asking
// the operator to `unsetalias` first, so the remediation lands at
// the schema layer rather than papered over in the handler.
type AliasStore interface {
	// LookupChannelAlias returns the (alias, resourceID) pair bound to
	// (teamID, channelID), or ErrAliasNotFound if no alias is set.
	LookupChannelAlias(ctx context.Context, teamID, channelID string) (alias, resourceID string, err error)
	// SetChannelAlias upserts the alias→resourceID binding on
	// (teamID, channelID). Replaces any prior alias on the row.
	SetChannelAlias(ctx context.Context, teamID, channelID, alias, resourceID string) error
	// ClearChannelAlias removes the alias binding on (teamID, channelID).
	// Returns ErrAliasNotFound if there was no alias to clear.
	ClearChannelAlias(ctx context.Context, teamID, channelID string) error
}

// ErrAliasNotFound is returned by AliasStore implementations when the
// requested (team, channel) row has no alias bound. Handlers use
// errors.Is to map to the "no alias set" friendly copy.
var ErrAliasNotFound = errors.New("no alias bound to this channel")

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
// authoritative validator accepts them; the parser only enforces the
// leading/trailing rule, which is what surfaces with a friendlier
// error than punting downstream.
var aliasCharsetPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

// aliasUsage is the help-text body returned when setalias/unsetalias
// is invoked with an obvious typo. Centralized so the parser-rejection
// path and the missing-arg path share the same copy.
const aliasUsage = "Usage:\n• `/qurl setalias $<alias> <url-or-resource-id>`\n• `/qurl unsetalias $<alias>`\n\nAliases are lowercase alphanumeric + dashes, up to 64 chars."

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
	msgAliasTargetInvalid = "Target must be a URL (http/https) or a resource id (`r_…`).\n\n" + aliasUsage
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
	if strings.HasPrefix(tgt, resourceIDPrefix) {
		// `r_…` short-circuit. We don't enforce a deeper character set
		// here — qurl-service's resource-id validator is the
		// authoritative gate. Empty body (`r_`) falls through as a
		// recognizable error at the persistence layer.
		out.Target = tgt
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
		return "", fmt.Sprintf("Alias `$%s` is longer than %d characters.", alias, aliasMaxLen)
	}
	if !aliasCharsetPattern.MatchString(alias) {
		return "", fmt.Sprintf("Alias `$%s` must be lowercase alphanumeric + dashes (no leading/trailing dash).", alias)
	}
	return alias, ""
}

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
	ctx, cancel = context.WithTimeout(h.baseCtx, asyncWorkTimeout)
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

	existingAlias, existingRID, err := h.aliasStore.LookupChannelAlias(ctx, teamID, channelID)
	if err != nil && !errors.Is(err, ErrAliasNotFound) {
		slog.Error("setalias lookup failed", "error", err, "team_id", teamID, "channel_id", channelID)
		respondSlack(w, "Failed to look up the current alias for this channel. Please try again.")
		return
	}

	// Same-target no-op: alias is already bound to this exact target
	// in this exact channel. Surface the no-op explicitly rather than
	// re-writing the row.
	if existingAlias == args.Alias && existingRID == args.Target {
		respondSlack(w, fmt.Sprintf("Alias `$%s` already points to `%s` in this channel. No change.", args.Alias, args.Target))
		return
	}

	// Different-existing-alias path: per the schema gap, we can't
	// hold multiple aliases per channel. Surface this to the admin
	// rather than silently overwriting — overwriting a teammate's
	// alias is a footgun.
	if existingAlias != "" && existingAlias != args.Alias {
		respondSlack(w, fmt.Sprintf("This channel already has alias `$%s` bound to `%s`. Run `/qurl unsetalias $%s` first, or pick a different channel.", existingAlias, existingRID, existingAlias))
		return
	}

	if err := h.aliasStore.SetChannelAlias(ctx, teamID, channelID, args.Alias, args.Target); err != nil {
		slog.Error("setalias write failed", "error", err, "team_id", teamID, "channel_id", channelID, "alias", args.Alias)
		respondSlack(w, "Failed to update alias. Please try again.")
		return
	}
	respondSlack(w, fmt.Sprintf("Alias `$%s` now points to `%s` in this channel.", args.Alias, args.Target))
}

// handleUnsetAlias routes `/qurl unsetalias $<alias>`.
//
// **Admin restriction:** Same Slack-manifest-level gate as
// handleSetAlias — see that comment. The CR feedback's
// "info-disclosure" concern (a non-admin probing alias existence via
// the response delta) is closed structurally by the manifest gate.
//
// **Alias-mismatch posture:** the user names the alias they want to
// clear. If the channel actually has a different alias bound, the
// handler surfaces the mismatch rather than silently clearing the
// wrong binding. This is the principle-of-least-surprise path — an
// admin running `/qurl unsetalias $foo` while the channel actually
// has `$bar` bound should learn about the mismatch, not have `$bar`
// silently disappear.
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

	existingAlias, _, err := h.aliasStore.LookupChannelAlias(ctx, teamID, channelID)
	switch {
	case errors.Is(err, ErrAliasNotFound):
		respondSlack(w, "No alias is set on this channel. Nothing to clear.")
		return
	case err != nil:
		slog.Error("unsetalias lookup failed", "error", err, "team_id", teamID, "channel_id", channelID)
		respondSlack(w, "Failed to look up the current alias for this channel. Please try again.")
		return
	case existingAlias != args.Alias:
		respondSlack(w, fmt.Sprintf("This channel has alias `$%s` bound, not `$%s`. Refusing to clear a different alias.", existingAlias, args.Alias))
		return
	}

	if err := h.aliasStore.ClearChannelAlias(ctx, teamID, channelID); err != nil {
		if errors.Is(err, ErrAliasNotFound) {
			// TOCTOU window: another admin cleared the alias between
			// the lookup and the clear. The user's intent is
			// satisfied either way; render the success copy so they
			// don't retry on a transient race.
			respondSlack(w, fmt.Sprintf("Alias `$%s` is no longer bound to this channel.", args.Alias))
			return
		}
		slog.Error("unsetalias write failed", "error", err, "team_id", teamID, "channel_id", channelID, "alias", args.Alias)
		respondSlack(w, "Failed to clear alias. Please try again.")
		return
	}
	respondSlack(w, fmt.Sprintf("Alias `$%s` is no longer bound to this channel.", args.Alias))
}
