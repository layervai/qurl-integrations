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

// listResourcesScanLimit is the page size for the single
// `/v1/resources` fetch backing `/qurl list`. `/qurl list` shows only
// tunnel resources, but the API has no server-side type filter, so the
// tunnel filter runs client-side on the fetched page. We over-fetch
// (the server's max) rather than page to a small display target: a
// 10-row page could surface zero tunnels in a URL-heavy workspace even
// when tunnels exist, hiding the very thing the command is for. One
// generous page keeps the command single-request while making the
// client-side tunnel filter reliable; tunnel resources are created
// deliberately (via `/qurl tunnel install`) so a real workspace has few
// of them, well within Slack's ephemeral-message budget.
//
// A server-side `type=tunnel` filter (tracked in #531) would let this
// drop to a normal page size and make `page.HasMore` mean "more
// tunnels" rather than "more resources of any type" — see the footer
// caveat in [Handler.processListResources].
const listResourcesScanLimit = 100

// listTunnelsEmptyMessage is the friendly empty-state copy. Points the
// user at `/qurl tunnel install <slug>` — the way tunnels are created —
// so a workspace with no tunnels yet knows what to do next.
const listTunnelsEmptyMessage = ":mag: No tunnels found in this workspace. Set one up with `/qurl tunnel install <slug>` first."

// listTunnelsNonAdminPaginationGapMessage is the user-facing copy
// for the case where a non-admin's filtered set is empty AND the
// master list reports has_more=true. Without this distinct copy a
// non-admin in a heavy workspace whose allow-listed tunnels sit
// past the first page would see the plain empty-state (wrong advice —
// the issue is pagination scope, not absence).
//
// "Allow specific tunnels" is the right verb for the common case: an
// empty filtered set most often means no allow-list rows for this
// channel yet, not an over-broad one that needs narrowing. The hint
// nudges toward an admin escalation path.
const listTunnelsNonAdminPaginationGapMessage = ":mag: No allowed tunnels on the first page of this workspace's listing. Tunnels allowed in this channel may exist past the first page — ask an admin to allow specific tunnels in this channel, or check `/qurl aliases` to see if a specific alias is set."

// handleListResources implements `/qurl list`. It lists the workspace's
// tunnel resources (type=tunnel only — URL/transit resources are
// filtered out) so each line is a copy-paste-ready `$<slug>` token the
// user can pipe into `/qurl get $<slug>` without a manual lookup. The
// slug is the same stable handle `/qurl tunnel install <slug>` binds as
// a channel alias, so the listed token resolves directly in `/qurl get`.
//
// Channel scoping:
//   - workspace admins see every tunnel in the master listing.
//   - non-admins see only tunnels allowed in the current channel
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

	page, err := c.ListResources(ctx, client.ListResourcesInput{Limit: listResourcesScanLimit})
	if err != nil {
		_ = h.postResponse(log, responseURL, ":warning: "+mapListResourcesError(log, teamID, err))
		return
	}

	// `/qurl list` shows tunnels only. The API has no server-side type
	// filter, so drop URL/transit resources here — BEFORE channel
	// scoping — so every downstream branch (empty-state, scoping,
	// footer) operates on the tunnel set.
	tunnels := filterTunnelResources(page.Resources)

	resources, isAdmin := h.scopeResourcesForUser(ctx, log, teamID, channelID, userID, tunnels)

	if len(resources) == 0 {
		// Pagination-gap copy only fires when (a) the user is a
		// non-admin, (b) we had channel context to filter against,
		// AND (c) the master list reports has_more. The
		// has-channel guard distinguishes "filtered set is empty
		// because the channel has no allow-list rows on the first
		// page" from "we never had a channel" (fail-closed branch
		// in scopeResourcesForUser). Without it, the empty-channel
		// fail-closed path would surface "ask an admin to allow
		// specific tunnels in *this channel*" — misleading,
		// because by construction there's no channel.
		//
		// TODO(#531): page.HasMore is a master-list signal ("more
		// resources of any type"), not "more tunnels". In a URL-heavy
		// workspace with zero tunnels but >scan-limit resources, this
		// branch fires and over-claims that allowed tunnels may sit
		// past the page. The server-side type=tunnel filter in #531
		// makes has_more tunnel-specific and removes the over-claim.
		if !isAdmin && channelID != "" && page.HasMore {
			log.Warn("list: non-admin filtered set is empty but master list has_more — allow-listed tunnels may sit past first page",
				"team_id", teamID, "channel_id", channelID, "user_id", userID, "scan_limit", listResourcesScanLimit)
			_ = h.postResponse(log, responseURL, listTunnelsNonAdminPaginationGapMessage)
			return
		}
		_ = h.postResponse(log, responseURL, listTunnelsEmptyMessage)
		return
	}

	// Stable order for two-call idempotency at the Slack ephemeral
	// surface — the server's pagination cursor implies an order but
	// not a stable one across re-queries. Sort by the displayed token
	// (the same slug→alias→resource_id precedence the rows render)
	// BEFORE formatting.
	sort.SliceStable(resources, func(i, j int) bool {
		return tunnelToken(&resources[i]) < tunnelToken(&resources[j])
	})
	lines := make([]string, 0, len(resources))
	for i := range resources {
		lines = append(lines, formatTunnelListLine(&resources[i]))
	}

	body := "*qURL Tunnels:*\n" + strings.Join(lines, "\n") +
		"\n\n_Copy any `$slug` and run `/qurl get $slug` to mint a one-time qURL link._"
	if page.HasMore {
		// has_more here means the workspace has more resources than the
		// single scan page — there may be additional tunnels we didn't
		// see. Footer copy depends on whether the user's view is
		// filtered. Admins see the master list directly; non-admins see
		// the channel-allowed subset, where allow-listed tunnels may sit
		// past the first scan invisibly — distinct copy makes that gap
		// explicit so users don't assume the rendered rows are exhaustive.
		if isAdmin {
			body += fmt.Sprintf("\n_…more resources past the first %d-row scan — some tunnels may not be shown._", listResourcesScanLimit)
		} else {
			body += fmt.Sprintf("\n_Showing allow-listed tunnels from the first %d-row scan; additional allow-listed tunnels may sit past it. Ask an admin to allow more in this channel._", listResourcesScanLimit)
		}
	}
	_ = h.postResponse(log, responseURL, body)
}

// filterTunnelResources returns only the tunnel-type resources from the
// fetched page. `/qurl list` is tunnel-scoped; URL/transit resources are
// dropped. Keys on r.Type == [client.ResourceTypeTunnel] (the upstream
// discriminator), NOT on an empty target_url, so a non-tunnel row with a
// transient empty target isn't mis-included.
func filterTunnelResources(resources []client.Resource) []client.Resource {
	out := make([]client.Resource, 0, len(resources))
	for i := range resources {
		if resources[i].Type == client.ResourceTypeTunnel {
			out = append(out, resources[i])
		}
	}
	return out
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

// tunnelToken returns the `$<token>` identifier shown for a tunnel in
// /qurl list — and the key rows sort by. Precedence:
//
//  1. Slug — the stable, owner-scoped tunnel handle. `/qurl tunnel
//     install <slug>` binds `$<slug>` as a channel alias, so the slug
//     pastes straight into `/qurl get $<slug>`. This is the common case
//     and the identifier we want to surface (never the opaque r_<id>).
//  2. Resource-level alias — fallback when a tunnel somehow carries no
//     slug but does have an alias.
//  3. resource_id — last resort for a legacy slug-less, alias-less
//     tunnel. Vanishingly rare (tunnels are created with a slug), but
//     better than an empty `$` token, and `/qurl get $r_<id>` still
//     resolves it via the channel allow-set.
func tunnelToken(r *client.Resource) string {
	if r.Slug != "" {
		return r.Slug
	}
	if r.Alias != "" {
		return r.Alias
	}
	return r.ResourceID
}

// formatTunnelListLine renders one tunnel resource as a single text
// line in /qurl list output:
//
//   - No description:   • `$<slug>`
//   - With description: • `$<slug>` → <description>
//
// The token is [tunnelToken] (slug-first; never the opaque r_<id> in
// the common case). The token-in-backticks shape lets Slack render it
// as inline code (easy click-to-copy) while keeping the line
// plaintext-greppable. There is no `(tunnel)` label or `[slug:...]`
// fragment — the whole list is tunnels and the token IS the slug, so
// both would be redundant noise. The arrow joins the slug to its
// human-readable description when one is set; an undescribed tunnel
// renders just the token.
func formatTunnelListLine(r *client.Resource) string {
	line := "• `$" + tunnelToken(r) + "`"
	if r.Description != "" {
		line += " → " + r.Description
	}
	return line
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
