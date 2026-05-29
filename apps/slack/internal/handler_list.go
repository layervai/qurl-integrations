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

// listTunnelsEmptyMessage is the friendly empty-state copy for a
// workspace with zero tunnels. `/qurl tunnel install` is admin-only,
// so the copy names the command without imperatively telling every
// member to run it — a non-admin reading this is routed implicitly to
// their Slack admin. Post-revert of #234 (#459) `/qurl list` no longer
// probes admin status, so there is a single empty-state for everyone
// rather than the old admin/non-admin branch.
const listTunnelsEmptyMessage = ":mag: No tunnels found in this workspace. A Slack admin can set one up with `/qurl tunnel install <slug>`."

// handleListResources implements `/qurl list`. It lists the workspace's
// tunnel resources (type=tunnel only — URL/transit resources are
// filtered out) so each line is a copy-paste-ready `$<slug>` token the
// user can pipe into `/qurl get $<slug>` without a manual lookup. The
// slug is the same stable handle `/qurl tunnel install <slug>` binds as
// a channel alias, so the listed token resolves directly in `/qurl get`.
//
// The listing is unscoped: every workspace member sees every tunnel,
// in every channel including DMs. #234 had added a non-admin
// channel-policy filter here; #459 reverted it because the gate
// dead-ended workspace owners who weren't Slack-admins — their only
// `/qurl list` output was a fail-closed empty state with no recoverable
// path — while adding no real capability boundary. Capability gating
// still happens at mint time: `/qurl get $r_<id>` enforces the channel
// allow-set for non-admins via [Handler.resourceAllowedForUser], and
// `/qurl get $<slug>` / `$<alias>` resolve through the per-channel
// binding. So dropping the list-side filter widens disclosure within a
// workspace (every member sees every tunnel's slug) but not capability.
func (h *Handler) handleListResources(w http.ResponseWriter, values url.Values) {
	h.runAsync(w, "list", values, func(ctx context.Context, log *slog.Logger) {
		h.processListResources(ctx, log, values)
	})
}

// processListResources is the async-worker body for /qurl list.
func (h *Handler) processListResources(ctx context.Context, log *slog.Logger, values url.Values) {
	responseURL := values.Get(fieldResponseURL)
	teamID := values.Get(fieldTeamID)

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
	// filter, so drop URL/transit resources here. The result is the
	// full workspace tunnel set — unscoped post-revert of #234 (#459),
	// so every member sees the same listing regardless of channel.
	resources := filterTunnelResources(page.Resources)

	if len(resources) == 0 {
		_ = h.postResponse(log, responseURL, listTunnelsEmptyMessage)
		return
	}

	// Stable order for two-call idempotency at the Slack ephemeral
	// surface — the server's pagination cursor implies an order but
	// not a stable one across re-queries. Sort by the displayed token
	// (the same slug→alias→resource_id precedence the rows render),
	// with resource_id as a tiebreaker so two rows sharing a token
	// (e.g. a slug == another row's alias) order deterministically
	// rather than inheriting the unstable upstream order. BEFORE
	// formatting.
	sort.SliceStable(resources, func(i, j int) bool {
		ti, tj := tunnelToken(&resources[i]), tunnelToken(&resources[j])
		if ti != tj {
			return ti < tj
		}
		return resources[i].ResourceID < resources[j].ResourceID
	})
	lines := make([]string, 0, len(resources))
	for i := range resources {
		lines = append(lines, formatTunnelListLine(&resources[i]))
	}

	body := "*qURL Tunnels:*\n" + strings.Join(lines, "\n") +
		"\n\n_Copy any `$slug` and run `/qurl get $slug` to mint a one-time qURL link._"
	if page.HasMore {
		// page.HasMore is a master-list signal — more resources of ANY
		// type, not necessarily more tunnels — so this footer can fire
		// even when every tunnel is already shown. See the #531 caveat
		// on listResourcesScanLimit; until then we warn rather than
		// risk implying the listing is exhaustive.
		body += fmt.Sprintf("\n_…more resources past the first %d-row scan — some tunnels may not be shown._", listResourcesScanLimit)
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
