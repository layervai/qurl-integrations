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

// noChannelAliasesMessage is the /qurl aliases empty state. It fires both
// when the channel has no policy entries at all and when every entry is only
// a tunnel's auto-bound `$<slug>` (no user-defined alias) — in both cases the
// channel has no real alias worth listing, so saying "no aliases" is clearer
// than printing bare `$<slug>` rows that read as if the slug were an alias.
const noChannelAliasesMessage = ":mag: No aliases are configured for this channel yet. Run `/qurl-admin set-alias $<alias> $<id>` to add one."

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
// the channel's alias count). A user can still spam the verb; if that
// becomes a real cost problem, add an aliases-specific gate rather than
// reusing the mint quota enforced by slackdata.CheckRateLimit.
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
		_ = h.postResponse(log, responseURL, noChannelAliasesMessage)
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
	// the opaque resource_id). Skipped entirely when every group is
	// resource-less (all-legacy channel) — there's nothing to join, so we
	// don't spend an upstream call.
	var byID map[string]client.Resource
	var hasMore bool
	if groupsNeedResolution(groups) {
		byID, hasMore = resourcesByResourceID(ctx, log, c)
	}
	lines := make([]string, 0, len(groups))
	unresolved := 0
	for i := range groups {
		// Resource-less groups (the "\x00"-keyed legacy/synthetic rows)
		// never participate in the join — skip the lookup. For a real
		// resource_id, a miss (nil map from a failed fetch, or absent from
		// the scanned page) leaves r as the zero-value Resource, which
		// formatAliasGroupLine degrades to the alias-only row, and counts
		// toward the pagination-gap signal below.
		var r client.Resource
		if rid := groups[i].resourceID; rid != "" {
			var ok bool
			if r, ok = byID[rid]; !ok {
				unresolved++
			}
		}
		// Skip a tunnel whose only channel alias is its own slug. The install
		// flow auto-binds `$<slug>` as a channel alias, so every installed
		// tunnel carries one entry equal to its slug — but the slug is the
		// tunnel's ID, not a user-defined alias, and listing it here made it
		// ambiguous whether the token was an ID or an alias. A resource-less
		// or slug-unresolved group (slug == "") has no slug to exclude, so any
		// alias it carries still counts. When this filters every group, the
		// len(lines)==0 guard below renders the no-aliases empty state.
		if !hasNonSlugAlias(r.Slug, groups[i].aliases) {
			continue
		}
		lines = append(lines, formatAliasGroupLine(r.TargetURL, r.Slug, r.Description, groups[i].aliases))
	}
	sort.Strings(lines)

	if len(lines) == 0 {
		_ = h.postResponse(log, responseURL, noChannelAliasesMessage)
		return
	}

	if hasMore && unresolved > 0 {
		// A bound tunnel resolved to alias-only AND the page reports more
		// resources past it. Two causes share this signal because
		// page.HasMore is a master-list flag ("more resources of any
		// type", not "more tunnels" — until the #531 server-side filter):
		// the bound resource may be paginated out, OR the binding may be
		// stale (its resource was deleted but the DDB row remains). The
		// message names both so operators don't only chase pagination.
		// One triage line for "why doesn't `$foo` show its slug?"; arms
		// the #555 follow-up. Additive only — the rendered rows are
		// unchanged.
		log.Warn("aliases: listing may be incomplete — a bound resource was not on the scanned page (paginated out, or the binding is stale)",
			"unresolved_groups", unresolved, "scan_limit", listResourcesScanLimit, "team_id", teamID, "channel_id", channelID)
	}

	body := "*Aliases configured for this channel:*\n" +
		"_Format: `$<id>` → the aliases that resolve to it. Run `/qurl get` with the ID or any alias._\n" +
		strings.Join(lines, "\n")
	_ = h.postResponse(log, responseURL, body)
}

// groupsNeedResolution reports whether any group carries a resource_id —
// i.e. whether the ListResources slug join is worth an upstream call. A
// channel whose bindings are all resource-less (legacy/synthetic) renders
// alias-only with no fetch.
func groupsNeedResolution(groups []aliasGroup) bool {
	for i := range groups {
		if groups[i].resourceID != "" {
			return true
		}
	}
	return false
}

// resourcesByResourceID fetches one page of the workspace's resources
// and indexes them by resource_id for the slug join in processAliases
// (see the call site for why the list path, not the per-id path, is the
// reliable slug source). Every resource on the page is indexed —
// intentionally NOT filtered to type=tunnel like /qurl list — so a
// legacy URL binding still resolves its target_url and renders the escaped
// "<url> (legacy URL) → $alias" line.
//
// Best-effort: a fetch failure yields (nil, false) — a nil-map lookup
// returns the zero-value Resource, so the caller degrades each row to
// alias-only. Bounded to one [listResourcesScanLimit] page; hasMore
// reports page.HasMore so the caller can flag a binding that may simply
// be paginated out. (Same cap as /qurl list; the incomplete-listing
// follow-up is tracked in #555, which depends on the #531 server-side
// type=tunnel filter.)
func resourcesByResourceID(ctx context.Context, log *slog.Logger, c *client.Client) (map[string]client.Resource, bool) {
	page, err := c.ListResources(ctx, client.ListResourcesInput{Limit: listResourcesScanLimit})
	if err != nil {
		log.Warn("aliases: list resources for slug resolution failed — rendering alias-only", "error", err)
		return nil, false
	}
	out := make(map[string]client.Resource, len(page.Resources))
	for i := range page.Resources {
		out[page.Resources[i].ResourceID] = page.Resources[i]
	}
	return out, page.HasMore
}

// hasNonSlugAlias reports whether a group carries a channel alias other than
// the tunnel's own slug. The install flow auto-binds `$<slug>` as a channel
// alias, so a tunnel with no user-defined alias still has one entry equal to
// its slug; that isn't a real alias and shouldn't list under /qurl aliases.
// A resource-less or slug-unresolved group (slug == "") has no slug to
// exclude, so any alias it carries counts.
func hasNonSlugAlias(slug string, aliases []string) bool {
	for _, a := range aliases {
		if a != slug {
			return true
		}
	}
	return false
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
//
// An em-dash joins the id to the tunnel's Display Name when present:
// • `$<slug>` — <Display Name> → `$<a1>`. The Display Name reuses the
// resource description field (see handleSetDisplayName) and is normally set;
// the empty guard handles the alias-only fallback rows (no resource fetch,
// so no description) and is defensive otherwise.
func formatAliasGroupLine(target, slug, description string, aliases []string) string {
	rhs := make([]string, 0, len(aliases))
	for _, a := range aliases {
		if a != slug {
			rhs = append(rhs, mrkdwnTokenSpan(a))
		}
	}
	var left string
	switch {
	case slug != "":
		left = mrkdwnTokenSpan(slug)
		// Append the tunnel's Display Name to the id when present. The
		// description field doubles as the Display Name (see
		// handleSetDisplayName); it's normally set, but the alias-only
		// fallback rows pass "" (no resource fetch happened), so guard it.
		if description != "" {
			left += " — " + escapeMrkdwnText(description)
		}
	case target != "":
		left = escapeMrkdwnText(target) + " (legacy URL)"
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
