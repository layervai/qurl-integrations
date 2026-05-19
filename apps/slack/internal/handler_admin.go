package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

// handleAdmin parses the `admin <verb> ...` form via the shared parser
// and dispatches to the action-specific handler. Each verb is split
// into one of two shapes:
//
//   - SYNC (allow / disallow / status / revoke): a single DDB or qURL
//     API call. The Fargate path is in-account latency to DDB or
//     ~50ms to qurl-service, comfortably under Slack's 3s ack budget.
//     Reply is written directly to `w`.
//
//   - ASYNC (policies / revoke-all): paginated walks that can exceed
//     the 3s synchronous window. Ack via runAsync (200 + ackWorkingOnIt)
//     and deliver the rendered result through response_url. Mirrors
//     the create/list async pattern in handler.go.
//
// Admin commands are rejected with a graceful message when
// `Config.AdminStore` is unset — production wires one in cmd/main.go;
// sandbox configs without the three QURL_*_TABLE env vars stay
// crash-free on `/qurl admin`.
func (h *Handler) handleAdmin(w http.ResponseWriter, values url.Values) {
	text := strings.TrimSpace(values.Get(fieldText))
	cmd, err := Parse(text)
	if err != nil {
		respondSlack(w, ":warning: "+err.Error())
		return
	}
	// No `cmd.Subcommand != SubcmdAdmin` guard: this entry point is
	// only reached from the `text == "admin"` / `HasPrefix("admin ")`
	// branches in handleSlashCommand, and the parser's parseAdmin
	// dispatch produces SubcmdAdmin for that input class. If the
	// parser ever drifts and emits a different subcommand here,
	// cmd.AdminAction will land empty and the switch's `default:`
	// arm renders the same "unknown" copy a guard would have.
	teamID := values.Get(fieldTeamID)
	userID := values.Get(fieldUserID)
	// `admin claim` is routed to [Handler.handleAdminClaim] directly
	// by handleSlashCommand BEFORE this dispatcher (see handler.go).
	// It never reaches handleAdmin in normal Slack traffic. The
	// short-circuit here catches a defensive misroute (e.g. a
	// synthetic test that posts `text=admin claim` through this entry
	// point) AND bypasses requireAdminStoreSync — the whole point of
	// the claim flow is to create the first admin from a bootstrap
	// code on a workspace where CheckAdmin returns (false, "") and
	// AdminStore presence is irrelevant to the modal-open call.
	// The matching `case AdminClaim` arm inside the switch is dead
	// code that only exists to satisfy the `exhaustive` lint; the
	// short-circuit above is the load-bearing branch.
	if cmd.AdminAction == AdminClaim {
		h.handleAdminClaim(w, values)
		return
	}
	// Every other verb needs AdminStore. Short-circuit once here
	// instead of repeating the guard in each switch arm.
	if !h.requireAdminStoreSync(w) {
		return
	}
	switch cmd.AdminAction {
	case AdminAllow:
		h.handleAdminAllow(w, values, teamID, userID, cmd)
	case AdminDisallow:
		h.handleAdminDisallow(w, values, teamID, userID, cmd)
	case AdminPolicies:
		h.handleAdminPolicies(w, values, teamID, userID)
	case AdminStatus:
		h.handleAdminStatus(w, values, teamID, userID)
	case AdminRevoke:
		h.handleAdminRevoke(w, values, teamID, userID, cmd)
	case AdminRevokeAll:
		h.handleAdminRevokeAll(w, values, teamID, userID, cmd)
	case AdminClaim:
		// Dead code — short-circuited above so the AdminStore guard
		// is skipped. Present only to satisfy the `exhaustive` lint.
		h.handleAdminClaim(w, values)
	default:
		respondSlack(w, fmt.Sprintf("Unknown admin action: `%s`. Try `/qurl help`.", cmd.AdminAction))
	}
}

// requireAdminStoreSync renders the "admin features not configured"
// reply when AdminStore is nil (sandbox / no-DDB deployments).
// Returns true when the caller may proceed; false when a reply has
// already been written.
func (h *Handler) requireAdminStoreSync(w http.ResponseWriter) bool {
	if h.cfg.AdminStore == nil {
		respondSlack(w, "Admin features are not configured for this deployment.")
		return false
	}
	return true
}

// adminGateBudget bounds the sync admin-gate CheckAdmin call so a
// hung DDB can't out-block Slack's 3s slash-command ack window. The
// gate is the FIRST upstream call on every sync admin verb; using
// `context.Background()` here would let a misbehaving upstream
// silently consume the entire user-visible budget. 1s leaves
// adminSyncVerbBudget (1.5s) for the verb body and ~500ms for the
// JSON-encode of the reply.
const adminGateBudget = 1 * time.Second

// adminSyncVerbBudget bounds the verb-body work for sync admin
// verbs (allow / disallow / status / revoke) so the full
// gate + body + encode chain fits inside Slack's 3s slash-command
// ack window. Without this, asyncWorkTimeout (25s) would silently
// let the verb body wedge past 3s and the user would see no reply
// at all (Slack drops slash-command responses that miss the ack).
//
// 1.5s leaves ~1s of the 3s window for the gate (adminGateBudget=1s)
// and ~500ms for response_encode + write. Verbs that genuinely
// can't fit this budget (e.g. handleAdminStatus when the workspace
// has thousands of policies and countPoliciesForTeam paginates)
// should move to async — see issue #358 for the
// countPoliciesForTeam cap that keeps status sync-safe.
const adminSyncVerbBudget = 1500 * time.Millisecond

// requireAdminSync centralizes the admin-only gate for sync handlers.
// Returns true when the caller may proceed; false when the request
// is denied (and a reply has already been written to `w`). The
// asymmetry between slog attrs (team_id + user_id + raw err) and the
// user-visible reply (generic upstream-error) is deliberate: detailed
// causes live in CloudWatch where on-call can read them; the wire
// surface never includes upstream message bodies that could carry
// request IDs or stack-frame fragments.
func (h *Handler) requireAdminSync(w http.ResponseWriter, teamID, userID string) bool {
	if teamID == "" || userID == "" {
		respondSlack(w, ":warning: missing team_id or user_id in slash command payload")
		return false
	}
	ctx, cancel := context.WithTimeout(h.baseCtx, adminGateBudget)
	defer cancel()
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(ctx, teamID, userID)
	if err != nil {
		slog.Error("admin check failed", "error", err, "team_id", teamID, "user_id", userID)
		respondSlack(w, ":warning: failed to verify admin status (upstream error; see logs).")
		return false
	}
	if !isAdmin {
		// Audit non-admin denials so brute-force / curiosity probes
		// are visible to on-call. Distinct slog.Warn level from the
		// success path's slog.Info so dashboards can filter "denied"
		// without scanning every admin command.
		slog.Warn("admin command denied: non-admin", "team_id", teamID, "user_id", userID)
		respondSlack(w, ":warning: this command is admin-only")
		return false
	}
	return true
}

// requireAdminAsync is the async-handler counterpart of
// [requireAdminSync]. Returns true when the caller may proceed;
// false when the worker should bail (a reply has already been
// posted to response_url via `post`). The same audit-vs-wire
// asymmetry applies.
//
// The gate is bounded by adminGateBudget independently of the
// worker's asyncWorkTimeout so a hung CheckAdmin can't pin a worker
// for the full 25s. With defaultMaxConcurrentAsync=50, a sustained
// DDB blip on workspace_mappings would otherwise exhaust the pool.
func (h *Handler) requireAdminAsync(ctx context.Context, log *slog.Logger, post func(string), teamID, userID string) bool {
	if teamID == "" || userID == "" {
		post(":warning: missing team_id or user_id in slash command payload")
		return false
	}
	gateCtx, cancel := context.WithTimeout(ctx, adminGateBudget)
	defer cancel()
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(gateCtx, teamID, userID)
	if err != nil {
		log.Error("admin check failed", "error", err, "team_id", teamID, "user_id", userID)
		post(":warning: failed to verify admin status (upstream error; see logs).")
		return false
	}
	if !isAdmin {
		log.Warn("admin command denied: non-admin", "team_id", teamID, "user_id", userID)
		post(":warning: this command is admin-only")
		return false
	}
	return true
}

// resolveAliasOrReplySync resolves an alias to a Resource for sync
// handlers. Returns (res, true) on success; (nil, false) when an
// error has already been written to `w` and the caller should bail.
// 404 surfaces as a friendly "alias not found" message; other
// errors get a generic upstream-error reply (the raw err.Error()
// could leak service internals).
func (h *Handler) resolveAliasOrReplySync(ctx context.Context, w http.ResponseWriter, c *client.Client, teamID, userID, alias string) (*client.Resource, bool) {
	res, err := c.GetResourceByAlias(ctx, alias)
	if err == nil {
		return res, true
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		slog.Info("admin: alias not found", "team_id", teamID, "user_id", userID, "alias", alias)
		respondSlack(w, fmt.Sprintf(":warning: alias `$%s` not found.", alias))
		return nil, false
	}
	slog.Error("resolve alias failed", "error", err, "team_id", teamID, "user_id", userID, "alias", alias)
	respondSlack(w, fmt.Sprintf(":warning: failed to look up alias `$%s` (upstream error; see logs).", alias))
	return nil, false
}

// adminPolicyMutation describes an allow/disallow operation. Pulled
// out as a struct so handleAdminAllow and handleAdminDisallow can
// share a single dispatch helper without dupl-lint shouting — the
// two flows are structurally identical modulo the API call, the
// idempotent status code, and the user-facing label.
type adminPolicyMutation struct {
	label            string
	successVerb      string
	idempotentStatus int
	idempotentMsgFmt string
	// call is a closure that captures the slackdata.Store from the
	// handler's receiver — runAdminPolicyMutation already holds it on
	// h.cfg.AdminStore, so threading it through as a parameter would
	// be redundant.
	call func(ctx context.Context, teamID, channelID, resourceID string) error
}

// runAdminPolicyMutation factors the gate / auth / resolve / mutate
// pipeline shared by handleAdminAllow and handleAdminDisallow. Every
// slog line on this path carries `user_id` because admin mutations
// are the kind of event where "who did this" is the most load-bearing
// audit field — if `prod-db` is allowed in the wrong channel the
// on-call needs an immediate answer.
func (h *Handler) runAdminPolicyMutation(w http.ResponseWriter, teamID, userID string, cmd *Command, m adminPolicyMutation) {
	if !h.requireAdminSync(w, teamID, userID) {
		return
	}
	// Use a fresh context bounded to the sync-verb budget so the
	// full gate + body chain fits inside Slack's 3s ack window.
	// asyncWorkTimeout (25s) would silently let the body wedge past
	// 3s and the user would see no reply at all.
	ctx, cancel := context.WithTimeout(h.baseCtx, adminSyncVerbBudget)
	defer cancel()
	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		slog.Error("failed to get API key", "error", err, "team_id", teamID, "user_id", userID)
		respondSlack(w, authErrorMessage(err))
		return
	}
	res, ok := h.resolveAliasOrReplySync(ctx, w, c, teamID, userID, cmd.Alias)
	if !ok {
		return
	}
	if err := m.call(ctx, teamID, cmd.ChannelID, res.ResourceID); err != nil {
		var se *slackdata.Error
		if errors.As(err, &se) && se.StatusCode == m.idempotentStatus {
			respondSlack(w, fmt.Sprintf(m.idempotentMsgFmt, cmd.Alias, cmd.ChannelID))
			return
		}
		slog.Error(m.label+" policy failed", "error", err, "team_id", teamID, "user_id", userID, "resource_id", res.ResourceID, "channel_id", cmd.ChannelID)
		respondSlack(w, fmt.Sprintf(":warning: failed to %s alias (upstream error; see logs).", m.label))
		return
	}
	slog.Info("admin "+m.label+" succeeded", "team_id", teamID, "user_id", userID, "resource_id", res.ResourceID, "channel_id", cmd.ChannelID, "alias", cmd.Alias)
	respondSlack(w, fmt.Sprintf("%s `$%s` in <#%s>.", m.successVerb, cmd.Alias, cmd.ChannelID))
}

// handleAdminAllow whitelists `$alias` for use in `#channel`. Resolves
// the alias to a resource_id via the customer-facing
// `/v1/resources/by-alias/:alias` endpoint, then writes the
// resource_id into the channel_policies row's `allowed_resource_ids`
// SS via slackdata.Store.AllowResource. 409 means the policy already
// exists — surfaced as an idempotent "nothing to do" reply.
//
// The alias→resource_id binding lives in the orthogonal
// `alias_bindings` Map attribute on the same row (mutated by a
// separate alias-bind command; not in scope for this verb). A
// channel with `allow` rows but no alias bindings renders aliasless
// entries on `/qurl admin policies` — see renderPolicies.
func (h *Handler) handleAdminAllow(w http.ResponseWriter, _ url.Values, teamID, userID string, cmd *Command) {
	h.runAdminPolicyMutation(w, teamID, userID, cmd, adminPolicyMutation{
		label:            "allow",
		successVerb:      "Allowed",
		idempotentStatus: http.StatusConflict,
		idempotentMsgFmt: "`$%s` is already allowed in <#%s> — nothing to do.",
		call: func(ctx context.Context, teamID, channelID, resourceID string) error {
			return h.cfg.AdminStore.AllowResource(ctx, teamID, channelID, resourceID)
		},
	})
}

// handleAdminDisallow is the symmetric counterpart to handleAdminAllow.
// A 404-on-disallow (no policy row exists for that channel/alias pair)
// is surfaced gracefully instead of the raw error — operators who run
// `disallow` on an alias that was never allowed shouldn't see a scary
// stack trace.
func (h *Handler) handleAdminDisallow(w http.ResponseWriter, _ url.Values, teamID, userID string, cmd *Command) {
	h.runAdminPolicyMutation(w, teamID, userID, cmd, adminPolicyMutation{
		label:            "disallow",
		successVerb:      "Disallowed",
		idempotentStatus: http.StatusNotFound,
		idempotentMsgFmt: "`$%s` was not allowed in <#%s> — nothing to remove.",
		call: func(ctx context.Context, teamID, channelID, resourceID string) error {
			return h.cfg.AdminStore.DisallowResource(ctx, teamID, channelID, resourceID)
		},
	})
}

// handleAdminStatus renders the workspace-config sanity check. The
// fingerprint is sha256(api_key)[:8] hex — NOT last-4 of the API key —
// so an attacker who reads Slack's audit log can't reconstruct any
// portion of the secret. The fingerprint shape is fenced by the
// `TestHandleAdminStatus_FingerprintIsNotLast4` regression test.
func (h *Handler) handleAdminStatus(w http.ResponseWriter, _ url.Values, teamID, userID string) {
	if !h.requireAdminSync(w, teamID, userID) {
		return
	}
	ctx, cancel := context.WithTimeout(h.baseCtx, adminSyncVerbBudget)
	defer cancel()
	cfg, err := h.cfg.AdminStore.GetWorkspaceConfig(ctx, teamID)
	if err != nil {
		slog.Error("get workspace config failed", "error", err, "team_id", teamID, "user_id", userID)
		respondSlack(w, ":warning: failed to load workspace config (upstream error; see logs).")
		return
	}
	configuredAt := "unknown"
	if !cfg.ConfiguredAt.IsZero() {
		configuredAt = cfg.ConfiguredAt.UTC().Format(time.RFC3339)
	}
	// Render fingerprint only when populated. Empty value produces a
	// dangling label that reads as a bug (`*API key fingerprint:* \`\``)
	// — preferable to skip the line until the workspace_mappings row
	// carries the sha256[:8] value.
	fingerprintLine := "• *API key fingerprint:* (not yet plumbed; see follow-up)"
	if cfg.APIKeyFingerprint != "" {
		fingerprintLine = fmt.Sprintf("• *API key fingerprint:* `%s` (sha256 first 8 hex)", cfg.APIKeyFingerprint)
	}
	// Policy count comes from slackdata.WorkspaceConfig — see #358
	// for the upcoming page-cap surface that will let this line
	// render "≥ N" when the underlying count walk truncates. Until
	// that lands the bare number is authoritative because
	// countPoliciesForTeam walks every page.
	policyCountLine := fmt.Sprintf("• *Channel policies:* %d", cfg.PolicyCount)
	body := strings.Join([]string{
		"*Workspace status*",
		fmt.Sprintf("• *Owner ID:* `%s`", cfg.OwnerID),
		fingerprintLine,
		fmt.Sprintf("• *Seed admin:* <@%s>", cfg.SeedAdminUserID),
		"• *Configured at:* " + configuredAt,
		policyCountLine,
	}, "\n")
	respondSlack(w, body)
}

// handleAdminRevoke deletes a single qURL by its `qurl_id`. Reuses the
// customer-facing DELETE so quota/audit logs reflect the action. 404
// surfaces as a friendly "already revoked or typo'd?" message (mirrors
// the policy-not-found graceful path in handleAdminDisallow); other
// failures surface a generic upstream-error.
func (h *Handler) handleAdminRevoke(w http.ResponseWriter, _ url.Values, teamID, userID string, cmd *Command) {
	if !h.requireAdminSync(w, teamID, userID) {
		return
	}
	ctx, cancel := context.WithTimeout(h.baseCtx, adminSyncVerbBudget)
	defer cancel()
	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		slog.Error("failed to get API key", "error", err, "team_id", teamID, "user_id", userID)
		respondSlack(w, authErrorMessage(err))
		return
	}
	if err := c.Delete(ctx, cmd.Target); err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			slog.Info("admin revoke: qURL not found (already revoked or typo'd)", "team_id", teamID, "user_id", userID, "qurl_id", cmd.Target)
			respondSlack(w, fmt.Sprintf("`%s` not found — already revoked, or check the qurl_id.", cmd.Target))
			return
		}
		slog.Error("revoke qURL failed", "error", err, "team_id", teamID, "user_id", userID, "qurl_id", cmd.Target)
		respondSlack(w, fmt.Sprintf(":warning: failed to revoke `%s` (upstream error; see logs).", cmd.Target))
		return
	}
	slog.Info("admin revoke succeeded", "team_id", teamID, "user_id", userID, "qurl_id", cmd.Target)
	respondSlack(w, fmt.Sprintf("Revoked `%s`.", cmd.Target))
}

// --- Async admin handlers (policies + revoke-all) ---

// adminPoliciesPageSize is the per-page row count requested from
// slackdata. Matches the `limit=50` value in the plan's Phase 3c
// command-implementations table.
//
// Note the asymmetry with [adminPoliciesReplyByteCap] below: a
// rendered line is `• <#C…> ← \`$…\` (\`r_…\`)\n` ≈ 50-80 bytes
// for typical alias/resource lengths, so the 3800-byte body cap
// trims at ~50-76 entries. In practice the page size IS the cap
// for short identifiers; the byte cap is the defense-in-depth
// against an outlier workspace whose aliases (or resource_ids)
// run long. Operators reading `Channel policies (50)` against a
// 50-row workspace will see all 50; against a 50-row workspace
// with 50-char aliases they'll see `(28 of 50)` and the trailing
// hint. Worth restating here so a future tweak to either constant
// doesn't silently regress the contract.
const adminPoliciesPageSize = 50

// adminPoliciesReplyByteCap caps the rendered policy listing's rows
// section so an unusually-long alias / resource_id payload can't push
// past Slack's 4000-char `text` ceiling. The cap is on the
// rows-only `strings.Builder` inside renderPolicies; the rendered
// body envelope adds header (~60B) plus trailing hint (≤260B), so
// 3500 leaves ~440 chars of headroom on a 4000-char ceiling. The
// earlier 3800 cap was close enough to the ceiling that a 50-row
// page of 50-char aliases + 34-char resource_ids landed at ~3952
// bytes, which left only 48 bytes of safety margin. See
// [adminPoliciesPageSize] above for the interaction between this
// cap and the page size; TestRenderPolicies_WorstCaseFitsSlack4000
// fences the math.
const adminPoliciesReplyByteCap = 3500

// handleAdminPolicies lists the channel/alias policy rows for the
// caller's workspace, paged 50 per call. Async because a workspace
// with many policies plus the DDB round-trip can exceed the 3s
// synchronous reply window. PR-3c.5 ships the first page only;
// cursor-driven next-page navigation lands in PR-3c.6.
func (h *Handler) handleAdminPolicies(w http.ResponseWriter, values url.Values, teamID, userID string) {
	h.runAsync(w, "admin-policies", values, func(ctx context.Context, log *slog.Logger) {
		responseURL := values.Get(fieldResponseURL)
		post := func(text string) { h.postResponse(log, responseURL, text) }
		if !h.requireAdminAsync(ctx, log, post, teamID, userID) {
			return
		}
		// adminPoliciesPageSize caps DDB ROWS, not flattened entries.
		// A row with N alias_bindings + M aliasless allowed_resource_ids
		// renders N+M PolicyEntries, so a page of 50 rows can produce
		// >50 entries when channels carry multiple bindings.
		// renderPolicies's byte-cap is the second-line defense.
		list, err := h.cfg.AdminStore.ListPolicies(ctx, teamID, "", adminPoliciesPageSize)
		if err != nil {
			log.Error("list policies failed", "error", err, "team_id", teamID, "user_id", userID)
			post(":warning: failed to list policies (upstream error; see logs).")
			return
		}
		post(renderPolicies(list))
	})
}

// renderPolicies builds the Slack-mrkdwn body for an admin-policies
// list. Extracted so the byte-cap + header logic stays unit-testable.
//
// One row per PolicyEntry. ListPolicies flattens `alias_bindings`
// into one entry per binding (alias-name asc) and emits any
// `allowed_resource_ids` SS members without an alias binding as
// aliasless entries (rendered as "(no alias bound)"). A channel
// with N alias bindings + M aliasless resources contributes N+M
// lines to the rendered output, consecutively.
func renderPolicies(list *slackdata.PolicyList) string {
	if list == nil || len(list.Entries) == 0 {
		return "No channel policies configured. Use `/qurl admin allow #channel $alias` to add one."
	}
	var rows strings.Builder
	// 80B/entry × 50 = 4000B, just over adminPoliciesReplyByteCap=3500
	// so a worst-case page fits without a re-grow. Was *64 (3200B),
	// which forced one realloc on a full 50-row page.
	rows.Grow(len(list.Entries) * 80)
	rendered := 0
	for i := range list.Entries {
		e := &list.Entries[i]
		// Aliasless entries describe channels with `/qurl admin allow`
		// rows whose resource_id has no matching alias_binding on the
		// row. That's a legitimate surface (allow + alias-bind are
		// orthogonal commands), not a bug — render an explicit
		// "no alias bound" marker rather than empty backticks.
		aliasFragment := fmt.Sprintf("`$%s`", e.Alias)
		if e.Alias == "" {
			aliasFragment = "_(no alias bound)_"
		}
		line := fmt.Sprintf("• <#%s> ← %s (`%s`)\n", e.ChannelID, aliasFragment, e.ResourceID)
		// Always emit at least one row before honoring the byte cap.
		// A pathologically long single entry (alias or resource id
		// pushing the first line past adminPoliciesReplyByteCap) would
		// otherwise render `*Channel policies (0 of N):*` with an
		// empty body — the operator gets a "more not shown" hint
		// with nothing to see. The first row may push the rendered
		// envelope past the cap, but the 256-byte Grow headroom on
		// the outer builder and Slack's 4000-byte hard ceiling
		// absorb a single long line. The cap then takes over for
		// subsequent rows.
		if rendered > 0 && rows.Len()+len(line) > adminPoliciesReplyByteCap {
			break
		}
		rows.WriteString(line)
		rendered++
	}
	var header string
	if rendered < len(list.Entries) {
		header = fmt.Sprintf("*Channel policies (%d of %d):*\n", rendered, len(list.Entries))
	} else {
		header = fmt.Sprintf("*Channel policies (%d):*\n", rendered)
	}
	var b strings.Builder
	b.Grow(len(header) + rows.Len() + 256)
	b.WriteString(header)
	b.WriteString(rows.String())
	// byte-cap-hit and HasMore can co-occur: a single page truncated
	// by the byte cap AND more pages available upstream. Emit BOTH
	// hints in that case so the operator doesn't think they've seen
	// everything just because the trimmed-page hint fired. Without
	// this, a workspace with >50 long-aliased policies would see
	// "N more not shown" with no nudge that further pages exist
	// upstream of the current page.
	if rendered < len(list.Entries) {
		fmt.Fprintf(&b, "_…%d more not shown (reply size cap); see Block Kit pagination in upcoming release._", len(list.Entries)-rendered)
		if list.HasMore {
			b.WriteString("\n_…and more pages available upstream; cursor navigation lands in the next release._")
		}
	} else if list.HasMore {
		b.WriteString("_…more pages available; cursor navigation lands in the next release._")
	}
	return strings.TrimRight(b.String(), "\n")
}

// adminRevokeAllMaxPages caps the per-call page walk so a runaway
// alias with thousands of qURLs can't pin a single async worker for
// the entire 25s asyncWorkTimeout. With adminRevokeAllPageSize=20
// this caps a single invocation at ~100 qURLs — operators get a
// re-run hint when truncated.
const adminRevokeAllMaxPages = 5

// adminRevokeAllPageSize is the page limit handed to ListByResource.
const adminRevokeAllPageSize = 20

// revokeAllResult is the audit/UI shape returned by the inner walk
// loop. Pulled out so handleAdminRevokeAll stays inside the gocognit
// ceiling and so the loop can be tested independently of the
// slash-command shell.
type revokeAllResult struct {
	revoked     int
	alreadyGone int
	failed      int
	rateLimited bool
	truncated   bool
	// serverPaginationBug is true iff qurl-service shipped a page
	// with `has_more=true` but an empty `next_cursor`. The server
	// is telling us more rows exist but giving us no way to advance —
	// silently treating that as "done" would leave the operator
	// thinking they revoked everything when they didn't.
	serverPaginationBug bool
	// deadlineExceeded is true iff the caller-supplied context's
	// deadline fired (async worker timeout).
	deadlineExceeded bool
	// canceled is true iff the caller-supplied context was canceled
	// (SIGTERM mid-walk). Distinct from deadlineExceeded so dashboards
	// can tell timeouts apart from shutdown cancellations.
	canceled bool
	// fatalErr is set when ListByResource returns an error mid-walk,
	// which is the only case that aborts the whole command (a single
	// failed Delete is best-effort and just bumps `failed`).
	fatalErr error
}

// recordCtxErr classifies a non-nil ctx.Err() into the result's
// deadlineExceeded vs canceled fields. Centralizes the
// errors.Is(..., context.DeadlineExceeded) check so both call sites
// stay consistent.
//
// Callers MUST pass `ctx.Err()`. The two sentinel errors
// (context.DeadlineExceeded and context.Canceled) are the only
// values ctx.Err() ever returns, so the explicit branches are
// exhaustive for that input. An "other error" lands at the
// log.Warn branch instead of silently being classified as a
// cancellation — that surfaces the bug in CloudWatch the moment a
// future patch accidentally passes a non-ctx error. The logger is
// the runAsync-derived `slog.Logger` so the violation-warn line
// inherits the workflow's team_id/user_id/trigger_id attrs (a
// bare slog.Warn would surface the violation without context — the
// case where you most want it).
func recordCtxErr(log *slog.Logger, err error, r *revokeAllResult) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		r.deadlineExceeded = true
	case errors.Is(err, context.Canceled):
		r.canceled = true
	default:
		// Contract violation — caller passed something that wasn't
		// ctx.Err(). Default to canceled so the best-effort
		// revoke-all walk surfaces SOME terminal state, but log
		// the violation loudly so it's audit-greppable.
		log.Warn("recordCtxErr: caller passed non-ctx error; classifying as canceled", "error", err)
		r.canceled = true
	}
}

// deletePage walks one page of qURLs, issuing DELETE for each and
// updating the result counters. Returns false when the caller should
// break out of the outer page loop (deadline / cancel mid-page); true
// when the page completed and the caller should continue.
func (h *Handler) deletePage(ctx context.Context, log *slog.Logger, c *client.Client, teamID, userID, resourceID string, qurls []client.QURL, r *revokeAllResult) bool {
	for i := range qurls {
		if ctxErr := ctx.Err(); ctxErr != nil {
			recordCtxErr(log, ctxErr, r)
			return false
		}
		id := qurls[i].ResourceID
		if err := c.Delete(ctx, id); err != nil {
			// Symmetric with the post-ListByResource reclassify in
			// runRevokeAllWalk: a deadline that fires INSIDE c.Delete
			// gets wrapped as a transport error, and without this
			// check it would land in r.failed++ rather than
			// r.deadlineExceeded. Same load-bearing reason — the
			// reply renderer's "budget elapsed" branch is the right
			// surface for ctx exhaustion, not a fictitious failure.
			if ctxErr := ctx.Err(); ctxErr != nil {
				recordCtxErr(log, ctxErr, r)
				return false
			}
			var apiErr *client.APIError
			if errors.As(err, &apiErr) {
				// 404 mid-walk is the expected outcome when a qURL
				// expired or was revoked by another admin between
				// the ListByResource and this Delete. Don't escalate
				// to `failed` — it would inflate the reply count and
				// pull on-call into a non-issue.
				if apiErr.StatusCode == http.StatusNotFound {
					log.Info("revoke-all: qURL already gone (race)", "team_id", teamID, "user_id", userID, "resource_id", resourceID, "qurl_id", id)
					r.alreadyGone++
					continue
				}
				// 429 — bail out of the entire walk. Continuing would
				// pile on more 429s. The user-facing reply nudges the
				// operator to re-run after the rate-limit window.
				if apiErr.StatusCode == http.StatusTooManyRequests {
					log.Warn("revoke-all: rate-limited mid-walk", "team_id", teamID, "user_id", userID, "resource_id", resourceID, "qurl_id", id, "retry_after_secs", apiErr.RetryAfter)
					r.rateLimited = true
					return false
				}
			}
			log.Warn("revoke-all: delete failed", "team_id", teamID, "user_id", userID, "resource_id", resourceID, "qurl_id", id, "error", err)
			r.failed++
			continue
		}
		r.revoked++
	}
	return true
}

// runRevokeAllWalk performs the alias-bound revoke walk. Centralizes
// the deadline / page-cap exit logic so the handler shell just renders
// the result. The async ctx already carries the asyncWorkTimeout deadline
// from runAsync, so we don't need a separate wall-clock budget — the
// underlying ctx.Err() check covers both cancellation and timeout.
func (h *Handler) runRevokeAllWalk(ctx context.Context, log *slog.Logger, c *client.Client, teamID, userID, resourceID string) revokeAllResult {
	r := revokeAllResult{}
	cursor := ""
	for page := 0; page < adminRevokeAllMaxPages; page++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			recordCtxErr(log, ctxErr, &r)
			break
		}
		out, err := c.ListByResource(ctx, client.ListByResourceInput{
			ResourceID: resourceID,
			Limit:      adminRevokeAllPageSize,
			Cursor:     cursor,
			Status:     client.StatusActive,
		})
		if err != nil {
			// Classify ctx-fired errors as deadline/canceled rather
			// than as a fatal upstream error. Without this branch,
			// a deadline that fires INSIDE ListByResource propagates
			// as `r.fatalErr` and the operator sees the generic
			// "failed to enumerate qURLs" reply instead of the more
			// useful "budget elapsed — re-run" hint. The pre-call
			// ctx.Err() guard catches deadlines that fire BEFORE the
			// call; this catches the ones that fire DURING.
			if ctxErr := ctx.Err(); ctxErr != nil {
				recordCtxErr(log, ctxErr, &r)
				return r
			}
			log.Error("list by resource failed", "error", err, "team_id", teamID, "user_id", userID, "resource_id", resourceID)
			r.fatalErr = err
			return r
		}
		if !h.deletePage(ctx, log, c, teamID, userID, resourceID, out.QURLs, &r) {
			break
		}
		// has_more=true with empty next_cursor is a buggy server
		// response — treat it as end-of-listing (the loop has nothing
		// to advance to). Without setting serverPaginationBug, the
		// operator would see "Revoked N" with no re-run hint and
		// assume the work is done.
		if out.HasMore && out.NextCursor == "" {
			log.Warn("revoke-all: server reported has_more=true with empty cursor; treating as end of listing",
				"team_id", teamID, "user_id", userID, "resource_id", resourceID, "page", page)
			r.serverPaginationBug = true
			break
		}
		if !out.HasMore {
			break
		}
		if page == adminRevokeAllMaxPages-1 {
			r.truncated = true
			break
		}
		cursor = out.NextCursor
	}
	return r
}

// handleAdminRevokeAll resolves the alias to a resource_id, pages the
// active qURLs bound to it, and DELETEs each. Async because the page
// walk + per-row Delete can exceed the 3s synchronous reply window
// even on a fast upstream.
//
// Best-effort delete semantics: errors on individual qURLs are logged
// but don't abort the loop — a 404 on one row (already revoked)
// shouldn't cancel the rest.
func (h *Handler) handleAdminRevokeAll(w http.ResponseWriter, values url.Values, teamID, userID string, cmd *Command) {
	h.runAsync(w, "admin-revoke-all", values, func(ctx context.Context, log *slog.Logger) {
		responseURL := values.Get(fieldResponseURL)
		post := func(text string) { h.postResponse(log, responseURL, text) }
		if !h.requireAdminAsync(ctx, log, post, teamID, userID) {
			return
		}
		c, err := h.authenticatedClient(ctx, teamID)
		if err != nil {
			log.Error("failed to get API key", "error", err, "team_id", teamID, "user_id", userID)
			post(authErrorMessage(err))
			return
		}
		// Resolve alias inside the async worker so a slow lookup doesn't
		// race the 3s ack window — the user already got "Working on it…".
		res, err := c.GetResourceByAlias(ctx, cmd.Alias)
		if err != nil {
			var apiErr *client.APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				log.Info("admin revoke-all: alias not found", "team_id", teamID, "user_id", userID, "alias", cmd.Alias)
				post(fmt.Sprintf(":warning: alias `$%s` not found.", cmd.Alias))
				return
			}
			log.Error("resolve alias failed", "error", err, "team_id", teamID, "user_id", userID, "alias", cmd.Alias)
			post(fmt.Sprintf(":warning: failed to look up alias `$%s` (upstream error; see logs).", cmd.Alias))
			return
		}
		r := h.runRevokeAllWalk(ctx, log, c, teamID, userID, res.ResourceID)
		if r.fatalErr != nil {
			post(":warning: failed to enumerate qURLs (upstream error; see logs).")
			return
		}
		log.Info("admin revoke-all completed", "team_id", teamID, "user_id", userID, "resource_id", res.ResourceID, "alias", cmd.Alias, "revoked", r.revoked, "already_gone", r.alreadyGone, "failed", r.failed, "rate_limited", r.rateLimited, "truncated", r.truncated, "server_pagination_bug", r.serverPaginationBug, "deadline_exceeded", r.deadlineExceeded, "canceled", r.canceled)
		post(renderRevokeAllReply(cmd.Alias, &r))
	})
}

// renderRevokeAllReply builds the user-visible summary for a revoke-all
// walk. Extracted so handleAdminRevokeAll stays inside the gocognit
// ceiling and so the reply text is unit-testable independently of the
// async worker scaffolding.
func renderRevokeAllReply(alias string, r *revokeAllResult) string {
	// Switch the leading verb based on whether anything was actually
	// revoked. With `r.revoked == 0` the original "Revoked 0 qURL(s)"
	// copy + a terminal reason (rate-limited / deadline / etc.) reads
	// as a misleading mock-success — operators want to see "no qURLs
	// revoked" up-front so the reason for the early bail isn't
	// buried in a half-sentence.
	var msg string
	if r.revoked == 0 {
		msg = fmt.Sprintf("No qURLs revoked for `$%s`.", alias)
	} else {
		msg = fmt.Sprintf("Revoked %d qURL(s) bound to `$%s`.", r.revoked, alias)
	}
	if r.alreadyGone > 0 {
		msg += fmt.Sprintf(" %d already gone (race or expired).", r.alreadyGone)
	}
	if r.failed > 0 {
		msg += fmt.Sprintf(" %d failed (see logs).", r.failed)
	}
	var reasons []string
	if r.rateLimited {
		// Rate-limited first because the operator's "wait then re-run"
		// action is qualitatively different from the others
		// ("re-run again now").
		reasons = append(reasons, "rate-limited by upstream (wait then re-run)")
	}
	if r.truncated {
		reasons = append(reasons, fmt.Sprintf("hit page limit (%d)", adminRevokeAllMaxPages))
	}
	if r.serverPaginationBug {
		reasons = append(reasons, "server pagination bug (more rows reported but no cursor)")
	}
	if r.deadlineExceeded {
		reasons = append(reasons, "budget elapsed")
	}
	if r.canceled {
		reasons = append(reasons, "request canceled")
	}
	if len(reasons) > 0 {
		msg += fmt.Sprintf(" %s — re-run `/qurl admin revoke-all $%s` to revoke any remaining qURLs.",
			strings.Join(reasons, "; "), alias)
	}
	return msg
}
