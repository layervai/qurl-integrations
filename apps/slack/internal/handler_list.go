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

// resourceTypeTunnel is the wire-level discriminator for tunnel
// resources. Lifted to a constant so the list renderer keys on the
// upstream type rather than guessing from empty `target_url` — a
// non-tunnel resource with a transient empty target (data glitch,
// partially-populated row) must NOT be silently re-labeled "(tunnel)".
const resourceTypeTunnel = "tunnel"

// handleListResources implements `/qurl list`. Returns the 10 most
// recent Resources rendered as copy-paste-ready `$<alias>` (when
// bound) or `$<resource_id>` tokens — each line pipes straight into
// `/qurl get $<token>` without a manual alias-resolution step.
//
// The list is unscoped: every workspace member sees the same master
// listing. Capability gating on individual resources happens at mint
// time — `/qurl get $r_<id>` still enforces the channel allow set
// for non-admins (see handler_get.go), so dropping the list-side
// filter widens disclosure within a workspace but not capability.
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
		h.postResponse(log, responseURL, ":warning: "+authErrorMessage(err))
		return
	}

	page, err := c.ListResources(ctx, client.ListResourcesInput{Limit: listResourcesPageLimit})
	if err != nil {
		h.postResponse(log, responseURL, ":warning: "+mapListResourcesError(log, teamID, err))
		return
	}

	resources := page.Resources
	if len(resources) == 0 {
		h.postResponse(log, responseURL, listResourcesEmptyMessage)
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
		body += fmt.Sprintf("\n_…more results past the first %d-row scan — narrow with `/qurl get $alias` or ask an admin._", listResourcesPageLimit)
	}
	h.postResponse(log, responseURL, body)
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
//   - Empty target (other): `• \`$<token>\` → <empty>`
//
// When `r.Description` is set, it appends ` — <description>` as a
// trailing annotation (after the alias-set/tunnel hints). Legacy
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
		return fmt.Sprintf("• `$%s` → (tunnel)%s", token, descSuffix)
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
