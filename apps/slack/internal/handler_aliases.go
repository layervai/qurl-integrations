package internal

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

// aliasesResourceFanoutLimit bounds the parallelism of per-row
// alias→target resolution. A channel's alias_bindings map can hold
// dozens of aliases; sequential fetches against a slow customer API
// would chew through asyncWorkTimeout (25s). 8 parallel workers
// leaves comfortable headroom (~200ms nominal × ceil(N/8) batches).
const aliasesResourceFanoutLimit = 8

// handleAliases implements `/qurl aliases`. Lists the aliases bound
// to the current channel via a single GetItem on channel_policies
// (PK=team, SK=channel — point read, no pagination concerns).
//
// Async-defer pattern:
//  1. Ack 200 + spinner.
//  2. Goroutine reads channel_policies via the DDB-direct AdminStore,
//     fans out customer-API resource fetches to render human-readable
//     target/alias fields, and POSTs the result to response_url.
//
// TODO: rate-limit. /qurl aliases is read-amplified — one DDB
// GetItem plus N customer-API reads (N = the channel's
// `alias_bindings` size, capped only by the channel's own size).
// A user spamming the verb amplifies upstream load by ~N. The in-bot
// rate-limit gate (slackdata.CheckRateLimit) is a stub today for
// /qurl get; once that lands, the same gate should cover /qurl
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
	// once followed by every alias that resolves to it here. Per-group
	// resource fetch is best-effort — a failed fetch degrades to an
	// alias-only line (never the opaque resource_id) rather than dropping
	// the group.
	groups := groupAliasEntriesByResource(entries)
	lines := fanoutAliasGroups(ctx, log, c, groups, aliasesResourceFanoutLimit)
	sort.Strings(lines)

	body := "*Aliases configured for this channel:*\n" +
		"_Format: `$<slug>` → the aliases that resolve to it. Run `/qurl get` with the slug or any alias._\n" +
		strings.Join(lines, "\n")
	_ = h.postResponse(log, responseURL, body)
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

// fanoutAliasGroups renders the per-group alias lines using a bounded
// worker pool — one resource fetch per group, NOT per alias (grouping by
// resource_id means two aliases on the same tunnel cost a single fetch).
// The dispatcher honors ctx.Done() while waiting for a semaphore slot —
// without this, a cancellation that fires while the loop is queuing rows
// (more groups than `limit`) would block on `sem <- {}` indefinitely
// (the workers only honor ctx through their downstream HTTP call).
// Groups that don't get dispatched fall back to alias-only lines via
// [formatAliasGroupLine] so the user still sees one line per group.
//
// Output order is non-deterministic — the caller sorts before rendering.
func fanoutAliasGroups(ctx context.Context, log *slog.Logger, c *client.Client, groups []aliasGroup, limit int) []string {
	if limit < 1 {
		limit = 1
	}
	lines := make([]string, len(groups))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
loop:
	for i := range groups {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			// Fill un-dispatched tail with alias-only fallbacks — no fetch
			// happened, so there's no slug and (by design) no resource_id.
			for j := i; j < len(groups); j++ {
				lines[j] = formatAliasGroupLine("", "", groups[j].aliases)
			}
			break loop
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			g := &groups[idx]
			target, slug := "", ""
			if g.resourceID != "" {
				// Resolve the group's canonical resource directly by the
				// resource_id we already hold from channel_policies, via
				// GET /v1/resources/{id}. The response carries the full
				// Resource: a tunnel has a Slug and no TargetURL; a legacy
				// URL binding the reverse. One fetch covers the whole group
				// — every alias in it points at this same resource_id.
				if r, rerr := c.GetResource(ctx, g.resourceID); rerr == nil {
					target = r.TargetURL
					slug = r.Slug
				} else if errors.Is(rerr, context.Canceled) || errors.Is(rerr, context.DeadlineExceeded) {
					// Distinct log from the 404/5xx branch so operators
					// can tell "request was cut short by SIGTERM /
					// asyncWorkTimeout" apart from "upstream rejected
					// this id" when triaging.
					log.Debug("aliases: resource fetch canceled before completion", "error", rerr, "resource_id", g.resourceID)
				} else {
					log.Debug("aliases: resource fetch failed in fanout", "error", rerr, "resource_id", g.resourceID)
				}
			}
			lines[idx] = formatAliasGroupLine(target, slug, g.aliases)
		}(i)
	}
	wg.Wait()
	return lines
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
