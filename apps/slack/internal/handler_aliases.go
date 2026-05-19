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
	if channelID == "" {
		// Slack always sends a channel_id on slash commands; an
		// empty value means a synthetic payload (test harness or
		// future channel-less invocation). Fail closed rather than
		// fan out a team-wide list — that's not the v1 surface.
		log.Warn("aliases: empty channel_id; refusing team-wide list")
		h.postResponse(log, responseURL, ":warning: This command must be invoked from a channel.")
		return
	}

	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		log.Error("aliases: API key lookup failed", "error", err)
		h.postResponse(log, responseURL, ":warning: "+authErrorMessage(err))
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
		h.postResponse(log, responseURL, ":warning: "+serviceUnreachableMessage)
		return
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
				} else if errors.Is(rerr, context.Canceled) || errors.Is(rerr, context.DeadlineExceeded) {
					// Distinct log from the 404/5xx branch so operators
					// can tell "request was cut short by SIGTERM /
					// asyncWorkTimeout" apart from "upstream rejected
					// this alias" when triaging.
					log.Debug("aliases: resource fetch canceled before completion", "error", rerr, "resource_id", e.ResourceID)
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
