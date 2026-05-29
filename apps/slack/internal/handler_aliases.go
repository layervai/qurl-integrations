package internal

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

// handleAliases implements `/qurl aliases`. Lists the aliases bound
// to the current channel via a single GetItem on channel_policies
// (PK=team, SK=channel — point read, no pagination concerns).
//
// Async-defer pattern:
//  1. Ack 200 + spinner.
//  2. Goroutine reads channel_policies via the DDB-direct AdminStore,
//     resolves each bound tunnel's slug from a single ListResources
//     call (the same source `/qurl list` uses — the one the API
//     actually returns slugs on), and POSTs the result to response_url.
//
// TODO: rate-limit. /qurl aliases costs one DDB GetItem plus one
// ListResources page per invocation (constant, no longer amplified by
// the channel's alias count). A user can still spam the verb; the
// in-bot rate-limit gate (slackdata.CheckRateLimit) is a stub today
// for /qurl get, and once it lands the same gate should cover /qurl
// aliases. Tracked alongside the /qurl get rate-limit TODO in
// SLACK_QURL_ROLLOUT.md.
func (h *Handler) handleAliases(w http.ResponseWriter, values url.Values) {
	h.runAsync(w, "aliases", values, func(ctx context.Context, log *slog.Logger) {
		h.processAliases(ctx, log, values)
	})
}

// processAliases is the async-worker body for /qurl aliases.
func (h *Handler) processAliases(ctx context.Context, log *slog.Logger, values url.Values) {
	responseURL := values.Get(fieldResponseURL)
	teamID := values.Get(fieldTeamID)
	channelID := values.Get(fieldChannelID)

	if h.cfg.AdminStore == nil {
		log.Warn("aliases: AdminStore is nil; replying not-configured")
		_ = h.postResponse(log, responseURL, ":warning: Admin features are not configured for this deployment.")
		return
	}
	if channelID == "" {
		// Slack always sends a channel_id on slash commands; an
		// empty value means a synthetic payload (test harness or
		// future channel-less invocation). Fail closed rather than
		// fan out a team-wide list — that's not the v1 surface.
		log.Warn("aliases: empty channel_id; refusing team-wide list")
		_ = h.postResponse(log, responseURL, ":warning: "+channelRequiredMessage)
		return
	}

	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		log.Error("aliases: API key lookup failed", "error", err)
		_ = h.postResponse(log, responseURL, ":warning: "+authErrorMessage(err))
		return
	}

	entries, err := h.cfg.AdminStore.GetChannelPolicy(ctx, teamID, channelID)
	if err != nil {
		// Raw store error text (`AdminError: Unauthorized [bad_token]
		// (401)`) MUST NOT reach the user — strip to the generic
		// serviceUnreachableMessage. Auth-class failures land on the
		// same generic path because the operator-facing detail is in
		// the slog line, not the wire reply.
		log.Warn("aliases: GetChannelPolicy failed", "error", err, "team_id", teamID, "channel_id", channelID)
		_ = h.postResponse(log, responseURL, ":warning: "+serviceUnreachableMessage)
		return
	}
	if len(entries) == 0 {
		_ = h.postResponse(log, responseURL, ":mag: No aliases are configured for this channel yet. Run `/qurl-admin set-alias $<alias> $<slug>` to add one.")
		return
	}

	// Collapse the per-alias bindings into one group per tunnel: several
	// aliases can point to the same slug, so the listing shows the slug
	// once followed by every alias that resolves to it here.
	groups := groupAliasEntriesByResource(entries)

	// Resolve each group's slug from the workspace tunnel list — the SAME
	// source `/qurl list` reads (and the one the API actually populates
	// slugs on; the per-id `GET /v1/resources/{id}` path does not). One
	// fetch covers every group, joined by resource_id. Best-effort: a
	// failed fetch degrades each row to its channel aliases alone (never
	// the opaque resource_id).
	byID := resourcesByResourceID(ctx, log, c)
	lines := make([]string, 0, len(groups))
	for i := range groups {
		target, slug := "", ""
		if r := byID[groups[i].resourceID]; r != nil {
			target, slug = r.TargetURL, r.Slug
		}
		lines = append(lines, formatAliasGroupLine(target, slug, groups[i].aliases))
	}
	sort.Strings(lines)

	body := "*Aliases configured for this channel:*\n" +
		"_Format: `$<slug>` → the aliases that resolve to it. Run `/qurl get` with the slug or any alias._\n" +
		strings.Join(lines, "\n")
	_ = h.postResponse(log, responseURL, body)
}

// resourcesByResourceID fetches the workspace's resources in a single
// page and indexes them by resource_id, so /qurl aliases can resolve
// each bound tunnel's slug from the SAME source /qurl list uses. The
// per-id `GET /v1/resources/{id}` path does NOT return the tunnel slug;
// the list path does, so a workspace-list join is the reliable resolver.
//
// Best-effort: a fetch failure yields nil and the caller degrades every
// row to its channel aliases alone. Bounded to one [listResourcesScanLimit]
// page — a resource past the first page won't resolve (same cap as
// /qurl list; tracked in #531).
func resourcesByResourceID(ctx context.Context, log *slog.Logger, c *client.Client) map[string]*client.Resource {
	page, err := c.ListResources(ctx, client.ListResourcesInput{Limit: listResourcesScanLimit})
	if err != nil {
		log.Warn("aliases: list resources for slug resolution failed — rendering alias-only", "error", err)
		return nil
	}
	out := make(map[string]*client.Resource, len(page.Resources))
	for i := range page.Resources {
		out[page.Resources[i].ResourceID] = &page.Resources[i]
	}
	return out
}

// aliasGroup collects every channel alias bound to one resource, so
// /qurl aliases renders a single line per tunnel (its slug plus all the
// aliases that resolve to it) rather than one line per alias.
type aliasGroup struct {
	resourceID string
	aliases    []string // channel aliases bound to resourceID, sorted
}

// groupAliasEntriesByResource collapses per-alias PolicyEntry rows into
// one aliasGroup per resource_id — several aliases can point to the same
// tunnel. Resource-less rows (legacy/synthetic, resource_id == "") each
// become their own group keyed by alias so they aren't merged together
// or dropped. Insertion order is preserved; the caller sorts the
// rendered lines.
func groupAliasEntriesByResource(entries []slackdata.PolicyEntry) []aliasGroup {
	idx := make(map[string]int, len(entries))
	groups := make([]aliasGroup, 0, len(entries))
	for i := range entries {
		rid := entries[i].ResourceID
		key := rid
		if key == "" {
			// No shared resource — key each resource-less row uniquely
			// so two of them don't collapse into one line. The "\x00"
			// sentinel can't appear in a real resource_id (DDB string
			// attrs written by this code are alias/slug/r_-id shaped), so
			// it can't collide with a populated rid.
			key = "\x00" + entries[i].Alias
		}
		gi, ok := idx[key]
		if !ok {
			gi = len(groups)
			groups = append(groups, aliasGroup{resourceID: rid})
			idx[key] = gi
		}
		if entries[i].Alias != "" {
			groups[gi].aliases = append(groups[gi].aliases, entries[i].Alias)
		}
	}
	for i := range groups {
		sort.Strings(groups[i].aliases)
	}
	return groups
}

// formatAliasGroupLine renders one /qurl aliases line as
// `$<slug> → <the aliases>`, so the immutable tunnel slug reads as the
// canonical name and the `$alias` tokens as its channel-scoped alternate
// names. The left side, most to least specific:
//
//   - tunnel slug:        • `$<slug>` → `$<a1>`, `$<a2>`
//   - legacy URL target:  • <url> (legacy URL) → `$<a1>`, `$<a2>`
//   - slug unresolved:    • `$<a1>`, `$<a2>`
//
// The opaque resource_id is never surfaced to users: a group whose slug
// can't be resolved (upstream fetch failed, or a legacy resource that
// predates the slug requirement) degrades to listing its channel aliases
// alone — the user can still `/qurl get $alias`.
//
// An alias equal to the slug is dropped from the right side — the install
// flow binds `$<slug>` as a channel alias, so it would otherwise list
// itself. A group whose only alias IS the slug renders just the slug.
func formatAliasGroupLine(target, slug string, aliases []string) string {
	rhs := make([]string, 0, len(aliases))
	for _, a := range aliases {
		if a != slug {
			rhs = append(rhs, "`$"+a+"`")
		}
	}
	var left string
	switch {
	case slug != "":
		left = "`$" + slug + "`"
	case target != "":
		left = target + " (legacy URL)"
	default:
		// No slug resolved — never fall back to the opaque resource_id.
		// Show the channel aliases alone so the row still renders and
		// `/qurl get $alias` still works.
		if len(rhs) == 0 {
			return "• (no alias)"
		}
		return "• " + strings.Join(rhs, ", ")
	}
	if len(rhs) == 0 {
		return "• " + left
	}
	return "• " + left + " → " + strings.Join(rhs, ", ")
}
