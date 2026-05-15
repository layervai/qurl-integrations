package internal

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

// aliasesPageLimit caps how many policy entries we read in a single
// /qurl aliases call. The plan documents 50/page; pagination inside
// Slack is awkward (no prev/next on ephemeral messages) and the v1
// UX assumes a single workspace's alias set fits on one screen — a
// "more truncated" footer surfaces when the page is full.
const aliasesPageLimit = 50

// aliasesResourceFanoutLimit bounds the parallelism of per-row
// alias→target resolution under [aliasesWork]. With 50 entries
// worst-case, sequential fetches against a slow customer API would
// chew through asyncWorkTimeout (25s) on the page. 8 parallel
// workers leaves comfortable headroom: 50/8 = ~7 batches × ~200ms
// nominal = ~1.4s.
const aliasesResourceFanoutLimit = 8

// handleAliases implements `/qurl aliases`. Lists owner-scoped
// aliases filtered to the current channel's allowed set. Same
// async-defer pattern as /qurl get:
//  1. Ack 200 + spinner.
//  2. Goroutine reads channel_policies via the DDB-direct AdminStore,
//     fans out customer-API resource fetches to render human-readable
//     target/alias fields, and POSTs the result to response_url.
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
		h.postResponse(log, responseURL, ":warning: Admin features are not configured for this deployment.")
		return
	}

	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		log.Error("aliases: API key lookup failed", "error", err)
		h.postResponse(log, responseURL, ":warning: "+authErrorMessage(err))
		return
	}

	policies, err := h.cfg.AdminStore.ListPolicies(ctx, teamID, "", aliasesPageLimit)
	if err != nil {
		// Raw store error text (`AdminError: Unauthorized [bad_token]
		// (401)`) MUST NOT reach the user — strip to the generic
		// serviceUnreachableMessage. Auth-class failures land on the
		// same generic path because the operator-facing detail is in
		// the slog line, not the wire reply.
		log.Warn("aliases: ListPolicies failed", "error", err, "team_id", teamID)
		h.postResponse(log, responseURL, ":warning: "+serviceUnreachableMessage)
		return
	}

	// Filter to the current channel's allowed set. An empty channel
	// (synthetic test payload) renders all team policies so callers
	// can introspect.
	entries := policies.Entries
	if channelID != "" {
		entries = filterEntriesByChannel(entries, channelID)
	}
	if len(entries) == 0 {
		h.postResponse(log, responseURL, ":mag: No aliases are allowed in this channel. Ask a workspace admin to run `/qurl admin allow #channel $alias`.")
		return
	}

	// Render each entry as a line. Per-row resource fetch is
	// best-effort — a failed fetch degrades to id-only rather than
	// dropping the entry.
	lines := fanoutAliasRows(ctx, log, c, entries, aliasesResourceFanoutLimit)
	sort.Strings(lines)

	body := "*Aliases allowed in this channel:*\n" + strings.Join(lines, "\n")
	if policies.HasMore {
		body += fmt.Sprintf("\n_…more results truncated (showing first %d)._", aliasesPageLimit)
	}
	h.postResponse(log, responseURL, body)
}

// fanoutAliasRows renders the per-entry alias lines using a bounded
// worker pool. The dispatcher honors ctx.Done() while waiting for a
// semaphore slot — without this, a cancellation that fires while the
// loop is queuing rows (more entries than `limit`) would block on
// `sem <- {}` indefinitely (the workers only honor ctx through their
// downstream HTTP call). Rows that don't get dispatched fall back to
// id-only lines via [formatAliasLine] so the user still sees one
// line per entry.
//
// Output order is non-deterministic — the caller sorts before
// rendering.
func fanoutAliasRows(ctx context.Context, log *slog.Logger, c *client.Client, entries []slackdata.PolicyEntry, limit int) []string {
	if limit < 1 {
		limit = 1
	}
	lines := make([]string, len(entries))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
loop:
	for i := range entries {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			// Fill un-dispatched tail with id-only fallbacks.
			for j := i; j < len(entries); j++ {
				e := &entries[j]
				lines[j] = formatAliasLine(e.Alias, "", e.ResourceID)
			}
			break loop
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			e := &entries[idx]
			alias := e.Alias
			target := ""
			if e.ResourceID != "" && e.Alias != "" {
				// GetResourceByAlias is the customer-facing path that
				// returns Alias + TargetURL. The Store row may carry
				// alias=="" (legacy shape) — fall back to id-only in
				// that case rather than swallowing a 404.
				if r, rerr := c.GetResourceByAlias(ctx, e.Alias); rerr == nil {
					if r.Alias != "" {
						alias = r.Alias
					}
					target = r.TargetURL
				} else {
					log.Debug("aliases: resource fetch failed in fanout", "error", rerr, "resource_id", e.ResourceID)
				}
			}
			lines[idx] = formatAliasLine(alias, target, e.ResourceID)
		}(i)
	}
	wg.Wait()
	return lines
}

// filterEntriesByChannel returns the subset of entries scoped to
// `channelID`. Lifted to a helper so the test can exercise it in
// isolation from the goroutine plumbing.
func filterEntriesByChannel(entries []slackdata.PolicyEntry, channelID string) []slackdata.PolicyEntry {
	out := make([]slackdata.PolicyEntry, 0, len(entries))
	for i := range entries {
		if entries[i].ChannelID == channelID {
			out = append(out, entries[i])
		}
	}
	return out
}

// formatAliasLine renders one row of the /qurl aliases listing. The
// target is optional — when the per-row resource fetch fails we fall
// back to "id only" rather than dropping the entry.
func formatAliasLine(alias, target, resourceID string) string {
	if alias == "" {
		alias = "(no alias)"
	}
	if target != "" {
		return fmt.Sprintf("• `$%s` → %s", alias, target)
	}
	if resourceID != "" {
		return fmt.Sprintf("• `$%s` → `%s`", alias, resourceID)
	}
	return fmt.Sprintf("• `$%s`", alias)
}
