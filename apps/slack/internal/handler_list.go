package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/layervai/qurl-integrations/shared/client"
)

// listResourcesPageLimit is the default page size for `/qurl list`.
// Pivoted from the legacy 5-qURLs render to 10-resources because:
//
//  1. Resources are coarser than qURLs (one Resource per target URL,
//     many qURLs per Resource), so 10 of them carry more information
//     than 5 qURLs.
//  2. Listing surfaces the `$<alias>` / `$<resource_id>` shape so
//     users can copy-paste straight into `/qurl get $<token>` — more
//     rows in one slash-command response means fewer round-trips for
//     the common "show me what's available" workflow.
//  3. Slack's ephemeral message budget comfortably fits 10 rows (each
//     at ~80-120 chars) without scrolling.
const listResourcesPageLimit = 10

// listResourcesEmptyMessage is the friendly empty-state copy. Points
// the user at `/qurl create <url>` so a brand-new workspace knows
// what to do next.
const listResourcesEmptyMessage = ":mag: No qURL resources found in this workspace. Create one with `/qurl create <url>` first."

// listResourcesNonAdminPaginationGapMessage is the user-facing copy
// for the case where a non-admin's filtered set is empty AND the
// master list reports has_more=true. Without this distinct copy a
// non-admin in a heavy workspace whose allow-listed resources sit
// past the first page would see "Create one with /qurl create"
// (wrong advice — the issue is pagination scope, not absence).
//
// "Allow specific resources" is the right verb for the common
// case: an empty filtered set most often means no allow-list rows
// for this channel yet, not an over-broad one that needs narrowing.
// The hint nudges toward an admin escalation path rather than a
// duplicate-resource workflow.
const listResourcesNonAdminPaginationGapMessage = ":mag: No allowed resources on the first page of this workspace's listing. Resources allowed in this channel may exist past the first page — ask an admin to allow specific resources in this channel, or check `/qurl aliases` to see if a specific alias is set."

// resourceTypeTunnel is the wire-level discriminator for tunnel
// resources. Lifted to a constant so the list renderer keys on the
// upstream type rather than guessing from empty `target_url` — a
// non-tunnel resource with a transient empty target (data glitch,
// partially-populated row) must NOT be silently re-labeled "(tunnel)".
const resourceTypeTunnel = "tunnel"

// handleListResources implements the refactored `/qurl list`. Pivots
// the legacy "5 most recent qURLs" output to "10 most recent
// Resources" so each line is a copy-paste-ready `$<alias>` (when
// bound) or `$<resource_id>` token the user can pipe into
// `/qurl get $<token>` without a manual alias-resolution step.
//
// Channel scoping:
//   - workspace admins see every resource in the master listing.
//   - non-admins see only resources allowed in the current channel
//     via channel_policies → resource_id set membership.
//   - non-admin + empty channel_id is fail-closed (returns empty).
//   - non-admin + policy-fetch failure is fail-closed (returns
//     empty); under-show beats leak.
//
// DM channels: Slack DMs use `D…` channel IDs that almost certainly
// have no policy entries, so a non-admin running `/qurl list` from a
// DM hits the "filtered to empty" branch. Intentional — DMs aren't
// an admin-controlled scope.
func (h *Handler) handleListResources(w http.ResponseWriter, values url.Values) {
	h.runAsync(w, "list", values, func(ctx context.Context, log *slog.Logger) {
		h.processListResources(ctx, log, values)
	})
}

// processListResources is the async-worker body for /qurl list.
func (h *Handler) processListResources(ctx context.Context, log *slog.Logger, values url.Values) {
	responseURL := values.Get(fieldResponseURL)
	teamID := values.Get(fieldTeamID)
	channelID := values.Get(fieldChannelID)
	userID := values.Get(fieldUserID)

	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		log.Error("list: API key lookup failed", "error", err)
		_ = h.postResponse(log, responseURL, ":warning: "+authErrorMessage(err))
		return
	}

	page, err := c.ListResources(ctx, client.ListResourcesInput{Limit: listResourcesPageLimit})
	if err != nil {
		_ = h.postResponse(log, responseURL, ":warning: "+mapListResourcesError(log, teamID, err))
		return
	}

	resources, isAdmin := h.scopeResourcesForUser(ctx, log, teamID, channelID, userID, page.Resources)

	if len(resources) == 0 {
		// Pagination-gap copy only fires when (a) the user is a
		// non-admin, (b) we had channel context to filter against,
		// AND (c) the master list reports has_more. The
		// has-channel guard distinguishes "filtered set is empty
		// because the channel has no allow-list rows on the first
		// page" from "we never had a channel" (fail-closed branch
		// in scopeResourcesForUser). Without it, the empty-channel
		// fail-closed path would surface "ask an admin to allow
		// specific resources in *this channel*" — misleading,
		// because by construction there's no channel.
		if !isAdmin && channelID != "" && page.HasMore {
			log.Warn("list: non-admin filtered set is empty but master list has_more — allow-listed resources may sit past first page",
				"team_id", teamID, "channel_id", channelID, "user_id", userID, "page_limit", listResourcesPageLimit)
			_ = h.postResponse(log, responseURL, listResourcesNonAdminPaginationGapMessage)
			return
		}
		_ = h.postResponse(log, responseURL, listResourcesEmptyMessage)
		return
	}

	// Stable order for two-call idempotency at the Slack ephemeral
	// surface — the server's pagination cursor implies an order but
	// not a stable one across re-queries. Sort by the underlying
	// token (alias if bound, else resource_id) BEFORE formatting.
	sort.SliceStable(resources, func(i, j int) bool {
		return resourceSortKey(&resources[i]) < resourceSortKey(&resources[j])
	})
	lines := make([]string, 0, len(resources))
	for i := range resources {
		lines = append(lines, formatResourceListLine(&resources[i]))
	}

	body := "*qURL Resources:*\n" + strings.Join(lines, "\n") +
		"\n\n_Copy any `$token` and run `/qurl get $token` to mint a qURL link._"
	if page.HasMore {
		// Footer copy depends on whether the user's view is filtered.
		// Admins see the master list directly, so "more past first N"
		// matches what they'd expect. Non-admins see the filtered
		// subset — additional allow-listed resources may sit past the
		// first master-page-N scan, which the admin-copy footer
		// understates ("more past first N" reads as "more of the same
		// kind"). Distinct non-admin copy makes the gap explicit so
		// users don't assume the rendered rows are exhaustive.
		if isAdmin {
			body += fmt.Sprintf("\n_…more results past the first %d-row scan — narrow with `/qurl get $alias` or ask an admin._", listResourcesPageLimit)
		} else {
			body += fmt.Sprintf("\n_Showing allow-listed resources from the first %d-row scan; others may sit past it. Ask an admin to allow more in this channel, or narrow with `/qurl get $alias`._", listResourcesPageLimit)
		}
	}
	_ = h.postResponse(log, responseURL, body)
}

// scopeResourcesForUser narrows the master resource list to what the
// requesting user can see in the current channel:
//
//   - workspace admins see the full list (no filter).
//   - non-admins see only resources allowed in `channelID` —
//     [Store.AllowedResourceIDsForChannel] unions both surfaces on
//     the channel_policies row (`allowed_resource_ids` SS and the
//     `alias_bindings` Map values) so a resource visible via either
//     `/qurl get $r_<id>` or `/qurl get $alias` shows up here.
//   - non-admin + empty channelID is fail-closed (returns empty);
//     a real Slack slash-command payload always carries channel_id
//     (DMs use D… IDs), so a missing value is malformed and must
//     NOT leak the master list.
//   - non-admin + policy-fetch failure is fail-closed (returns
//     empty); under-show beats leak.
func (h *Handler) scopeResourcesForUser(ctx context.Context, log *slog.Logger, teamID, channelID, userID string, resources []client.Resource) ([]client.Resource, bool) {
	if h.cfg.AdminStore == nil {
		// No AdminStore → no admin probe, no policy fetch. Fail-closed
		// for non-admins because we can't determine allow-listed
		// resources. Production wires AdminStore from QURL_*_TABLE
		// env vars; sandbox/no-DDB deployments get the empty list.
		log.Warn("list: AdminStore is nil; treating user as non-admin and returning empty list (fail-closed)",
			"team_id", teamID, "user_id", userID)
		return nil, false
	}
	isAdmin := h.userIsWorkspaceAdmin(ctx, log, teamID, userID)
	if isAdmin {
		return resources, true
	}
	if channelID == "" {
		log.Warn("list: empty channel_id for non-admin — returning empty list (fail-closed)",
			"team_id", teamID, "user_id", userID)
		return nil, false
	}
	allowed, allowErr := h.cfg.AdminStore.AllowedResourceIDsForChannel(ctx, teamID, channelID)
	if allowErr != nil {
		log.Warn("list: allowed-resource fetch failed — falling back to empty allow set (fail-closed)",
			"error", allowErr, "team_id", teamID, "channel_id", channelID)
		return nil, false
	}
	return filterResourcesByAllowedSet(resources, allowed), false
}

// userIsWorkspaceAdmin probes Store.CheckAdmin for the (team, user)
// pair. Returns false on any error — the non-admin branch is the
// safe default ("under-show, don't leak"). Panics or network failures
// are surfaced through the err and folded into the false return;
// callers don't need to distinguish "definitely not admin" from
// "couldn't check".
//
// teamID == "" or userID == "" returns false — synthetic test
// payloads or wire-shape regressions should under-show rather than
// leak the master list.
func (h *Handler) userIsWorkspaceAdmin(ctx context.Context, log *slog.Logger, teamID, userID string) bool {
	if teamID == "" || userID == "" {
		return false
	}
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(ctx, teamID, userID)
	if err != nil {
		log.Debug("list: admin check failed — treating as non-admin",
			"error", err, "team_id", teamID, "user_id", userID)
		return false
	}
	return isAdmin
}

// filterResourcesByAllowedSet returns the subset of resources whose
// ResourceID appears in the allowed set. Empty allow set returns an
// empty slice — non-admin in a channel with zero policies should see
// zero resources, not the full list.
func filterResourcesByAllowedSet(resources []client.Resource, allowed map[string]struct{}) []client.Resource {
	if len(allowed) == 0 {
		return []client.Resource{}
	}
	out := make([]client.Resource, 0, len(resources))
	for i := range resources {
		if _, ok := allowed[resources[i].ResourceID]; ok {
			out = append(out, resources[i])
		}
	}
	return out
}

// resourceSortKey returns the token used to order rows in the
// /qurl list output. Aliased rows sort by alias, un-aliased rows
// sort by resource_id — the same token that ends up in the rendered
// `$<token>` prefix.
func resourceSortKey(r *client.Resource) string {
	if r.Alias != "" {
		return r.Alias
	}
	return r.ResourceID
}

// formatResourceListLine renders one resource as a single text line
// in /qurl list output. Per-line shape:
//
//   - With alias bound:    `• \`$<alias>\` → <target_url>`
//   - Without alias bound: `• \`$<resource_id>\` → <target_url> (no alias set)`
//   - Tunnel (Type=tunnel): `• \`$<token>\` → (tunnel)`
//   - Tunnel with slug:    `• \`$<token>\` → (tunnel) [slug:<slug>]`
//   - Empty target (other): `• \`$<token>\` → <empty>`
//
// When `r.Description` is set, it appends ` — <description>` as a
// trailing annotation (after the alias-set/tunnel/slug hints). Legacy
// /qurl list rendered description on a separate visual axis; the
// trailing-em-dash form keeps the one-line-per-row shape needed for
// the copy-paste-ready `$<token>` workflow while preserving the
// operator-authored context.
//
// The token-in-backticks shape lets Slack render it as inline code
// (easy click-to-copy) while keeping the line plaintext-greppable
// for operator workflows. The "(no alias set)" annotation flags
// non-tunnel rows that would benefit from a future `/qurl setalias`
// call — it's suppressed on the tunnel branch because the "(tunnel)"
// placeholder is already a strong visual signal and the doubled-up
// "(tunnel) (no alias set)" reads as redundant noise.
//
// Tunnel detection keys on r.Type == "tunnel" (the upstream resource
// type), NOT on empty target_url — keying on the empty field would
// silently re-label any non-tunnel row with a missing target as
// "(tunnel)", which would mislead operators triaging a data glitch.
//
// Slug fragment ([slug:<slug>]) renders ONLY on tunnel rows that carry
// a non-empty Slug. URL/transit resources never carry a slug on the
// wire (qurl-service rejects slug on non-tunnel creates) so the
// fragment is structurally tunnel-scoped. A tunnel row WITHOUT a slug
// — legacy / pre-Phase-1A — renders the fragment-free shape so existing
// fixtures (and operator muscle memory) keep working. The customer's
// onboarding flow runs `/qurl list resources` to match what their
// sidecar provisioned (via QURL_TUNNEL_SLUG) against a resource_id
// before pairing it with an alias.
func formatResourceListLine(r *client.Resource) string {
	token := r.Alias
	noAlias := false
	if token == "" {
		token = r.ResourceID
		noAlias = true
	}
	descSuffix := ""
	if r.Description != "" {
		descSuffix = " — " + r.Description
	}
	if r.Type == resourceTypeTunnel {
		// Tunnel-type resources have no target_url (the FRP server
		// is the effective destination). Render a placeholder so the
		// row still reads cleanly. The "(no alias set)" annotation
		// is suppressed here — see godoc.
		slugFrag := ""
		if r.Slug != "" {
			slugFrag = " [slug:" + r.Slug + "]"
		}
		return fmt.Sprintf("• `$%s` → (tunnel)%s%s", token, slugFrag, descSuffix)
	}
	target := r.TargetURL
	if target == "" {
		// Non-tunnel row with an empty target_url — render "<empty>"
		// rather than silently labeling as a tunnel. Surfaces the
		// data anomaly to user + ops without misleading either.
		target = "<empty>"
	}
	suffix := ""
	if noAlias {
		suffix = " (no alias set)"
	}
	return fmt.Sprintf("• `$%s` → %s%s%s", token, target, suffix, descSuffix)
}

// mapListResourcesError surfaces a friendly user-facing error for
// /qurl list failures. Auth-class (401/403) maps to authFailureMessage;
// everything else falls back to serviceUnreachableMessage. Raw
// APIError text MUST NOT reach Slack — it carries internal codes
// that are operator-grade, not user-grade.
func mapListResourcesError(log *slog.Logger, teamID string, err error) string {
	log.Warn("list: list resources failed", "error", err, "team_id", teamID)
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
		return authFailureMessage
	}
	return serviceUnreachableMessage
}
