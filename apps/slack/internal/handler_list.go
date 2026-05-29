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
// deliberately (via `/qurl-admin tunnel install`) so a real workspace has few
// of them, well within Slack's ephemeral-message budget.
//
// A server-side `type=tunnel` filter (tracked in #531) would let this
// drop to a normal page size and make `page.HasMore` mean "more
// tunnels" rather than "more resources of any type" — see the footer
// caveat in [Handler.processListResources].
const listResourcesScanLimit = 100

// listTunnelsEmptyMessage is the friendly empty-state copy for a
// workspace with zero tunnels. `/qurl-admin tunnel install` is admin-only,
// so the copy names the command without imperatively telling every
// member to run it — a non-admin reading this is routed implicitly to
// their Slack admin. Post-revert of #234 (#459) `/qurl list` no longer
// probes admin status, so there is a single empty-state for everyone
// rather than the old admin/non-admin branch.
const listTunnelsEmptyMessage = ":mag: No tunnels found in this workspace. A Slack admin can set one up with `/qurl-admin tunnel install <slug>`."

// handleListResources implements `/qurl list`. It lists the workspace's
// tunnel resources (type=tunnel only — URL/transit resources are
// filtered out) so each line is a copy-paste-ready `$<slug>` token the
// user can pipe into `/qurl get $<slug>` without a manual lookup. The
// slug is the same stable handle `/qurl-admin tunnel install <slug>` binds as
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
	// The tunnel listing is workspace-wide (unscoped post-#459), but the
	// per-row alias annotations are channel-scoped — channelAliasesByResourceID
	// reads THIS channel's alias_bindings so each row can show the shortcuts
	// that resolve here. Empty channelID (synthetic payload / DM) degrades to
	// slug-only rows inside the helper.
	channelID := values.Get(fieldChannelID)

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

	// Map each tunnel's resource_id to its channel-bound `$alias`
	// shortcuts so each row can show them next to the slug. Best-effort:
	// a fetch failure renders slug-only. Built BEFORE the sort because the
	// sort keys on [tunnelDisplayToken], which promotes a slug-less
	// tunnel's first bound alias — the sort key must match what the row
	// renders.
	aliasMap := h.channelAliasesByResourceID(ctx, log, teamID, channelID)

	// Precompute each row's display token once (keyed by resource_id)
	// rather than recomputing it inside the O(n log n) comparator below.
	displayTok := make(map[string]string, len(resources))
	for i := range resources {
		displayTok[resources[i].ResourceID] = tunnelDisplayToken(&resources[i], aliasMap[resources[i].ResourceID])
	}

	// Stable order for two-call idempotency at the Slack ephemeral
	// surface — the server's pagination cursor implies an order but not a
	// stable one across re-queries. Tokenless rows (slug-less, alias-less —
	// the bare-resource_id "(no slug …)" rows) sort to the END so the
	// legible $slug/$alias tunnels lead rather than a wall of opaque
	// r_<id>s. Within each cohort, sort by the displayed token with
	// resource_id as a tiebreaker (so two rows sharing a token — a slug ==
	// another row's alias, or two tokenless rows — order deterministically
	// rather than inheriting the unstable upstream order). BEFORE formatting.
	sort.SliceStable(resources, func(i, j int) bool {
		ti, tj := displayTok[resources[i].ResourceID], displayTok[resources[j].ResourceID]
		if (ti == "") != (tj == "") {
			return ti != "" // non-empty (legible) token sorts first
		}
		if ti != tj {
			return ti < tj
		}
		return resources[i].ResourceID < resources[j].ResourceID
	})

	lines := make([]string, 0, len(resources))
	for i := range resources {
		lines = append(lines, formatTunnelListLine(&resources[i], aliasMap[resources[i].ResourceID]))
	}

	body := "*Protected Tunnel Resources:*\n" + strings.Join(lines, "\n") +
		"\n\n_Copy any `$slug` or `$alias` and run `/qurl get` on it to mint a one-time qURL link. Any `(also …)` aliases shown are specific to this channel._"
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

// tunnelToken returns the resource-intrinsic `$<token>` for a tunnel.
// Precedence:
//
//  1. Slug — the stable, owner-scoped tunnel handle. `/qurl-admin tunnel
//     install <slug>` binds `$<slug>` as a channel alias, so the slug
//     pastes straight into `/qurl get $<slug>`. This is the common case
//     and the identifier we want to surface (never the opaque r_<id>).
//  2. Resource-level alias — fallback when a tunnel somehow carries no
//     slug but does have an alias.
//  3. "" — a slug-less, resource-alias-less tunnel has no intrinsic
//     `$<token>` the user can `get` (get is slug/alias-only now).
//     Vanishingly rare (tunnels are created with a slug). This function
//     stops at ""; the channel-alias promotion and the bare-resource_id
//     fallback both live downstream — in [tunnelDisplayToken] and
//     [formatTunnelListLine] respectively, not here.
func tunnelToken(r *client.Resource) string {
	if r.Slug != "" {
		return r.Slug
	}
	return r.Alias // "" when the tunnel has neither a slug nor an alias
}

// tunnelDisplayToken is the `$<token>` shown for a tunnel in /qurl list AND
// the key rows sort by — both go through this one function so the sort order
// matches what each row actually renders. It extends [tunnelToken] with
// channel-alias promotion: when a tunnel has no intrinsic token (slug-less,
// resource-alias-less) but a channel `$alias` binds to it, the first
// (lexically sorted) bound alias becomes the token. Returns "" only when
// there is no token at all — formatTunnelListLine then renders the bare
// resource_id.
//
// Surprise to note: because promotion picks the lexically-first bound alias,
// a slug-less tunnel's displayed identity can change if an admin later binds
// an alphabetically-earlier alias (e.g. `$zebra` → `$aardvark`). Rare —
// installs come with a slug — but documented so the flip isn't a mystery.
func tunnelDisplayToken(r *client.Resource, boundAliases []string) string {
	if t := tunnelToken(r); t != "" {
		return t
	}
	if len(boundAliases) > 0 {
		return boundAliases[0] // sorted by channelAliasesByResourceID
	}
	return ""
}

// formatTunnelListLine renders one tunnel resource as a single text
// line in /qurl list output:
//
//   - Slug only:           • `$<slug>`
//   - With bound aliases:  • `$<slug>` (also `$<alias>`, `$<alias2>`)
//   - With description:    • `$<slug>` → <description>
//
// The primary token is [tunnelDisplayToken] (slug-first; never the opaque
// r_<id>). `boundAliases` are the channel `$alias` shortcuts that resolve
// to this tunnel in `/qurl get` — a tunnel can have several. They render
// as "(also …)" so the user sees every name that works, EXCLUDING the
// primary token itself (the install flow binds `$<slug>` as a channel
// alias, so the slug would otherwise appear twice). The token-in-backticks
// shape lets Slack render each as inline code. There is no `(tunnel)` label
// or `[slug:...]` fragment — the whole list is tunnels and the token IS the
// slug. The arrow joins to the human-readable description when one is set.
//
// A slug-less, resource-alias-less tunnel with a bound channel `$alias` has
// that alias promoted to the primary token (by [tunnelDisplayToken]) so the
// row shows a name the user can `get` against — rather than a bare
// resource_id labeled "(no slug set)" sitting next to an "(also `$alias`)"
// that advertises a usable token. With no bound alias either, the tunnel has
// no token `/qurl get` can accept at all (the opaque resource_id isn't an
// alias-shaped token), so the row renders the bare resource_id WITHOUT a `$`
// sigil and spells out that it's not usable from Slack until an admin sets a
// slug — keeping the "copy a token and get it" promise honest.
func formatTunnelListLine(r *client.Resource, boundAliases []string) string {
	token := tunnelDisplayToken(r, boundAliases)
	var line string
	if token == "" {
		line = "• `" + r.ResourceID + "` (no slug — ask your Slack admin to set one)"
	} else {
		line = "• `$" + token + "`"
	}
	extras := make([]string, 0, len(boundAliases))
	for _, a := range boundAliases {
		if a != token {
			extras = append(extras, "`$"+a+"`")
		}
	}
	if len(extras) > 0 {
		line += " (also " + strings.Join(extras, ", ") + ")"
	}
	if r.Description != "" {
		line += " → " + r.Description
	}
	return line
}

// channelAliasesByResourceID builds resource_id → sorted channel-bound
// `$alias` shortcuts from the channel's policy entries, so /qurl list
// can show every alias that resolves to each tunnel. Best-effort:
// AdminStore-nil, empty channel, or a fetch failure yields nil (rows
// render slug-only) — the listing must still render.
//
// Cost: one GetChannelPolicy (GetItem) per /qurl list, purely for the
// cosmetic alias display. Post-#459 the list is unscoped — the admin
// gate / channel-policy filter (scopeResourcesForUser) was removed — so
// there's no longer an allow-set read on this path to fold this into;
// it's a standalone read. Acceptable for the discovery surface.
func (h *Handler) channelAliasesByResourceID(ctx context.Context, log *slog.Logger, teamID, channelID string) map[string][]string {
	if h.cfg.AdminStore == nil || channelID == "" {
		return nil
	}
	entries, err := h.cfg.AdminStore.GetChannelPolicy(ctx, teamID, channelID)
	if err != nil {
		log.Debug("list: channel-policy fetch for alias display failed — rendering slug-only",
			"error", err, "team_id", teamID, "channel_id", channelID)
		return nil
	}
	out := make(map[string][]string)
	for i := range entries {
		if entries[i].Alias != "" && entries[i].ResourceID != "" {
			out[entries[i].ResourceID] = append(out[entries[i].ResourceID], entries[i].Alias)
		}
	}
	for id := range out {
		sort.Strings(out[id])
	}
	return out
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
