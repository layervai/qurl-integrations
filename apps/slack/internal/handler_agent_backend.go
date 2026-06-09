package internal

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

// Compile-time check that the adapter satisfies the agent port.
var _ agent.Backend = (*agentBackend)(nil)

// agentBackend adapts the qURL client and channel_policies to [agent.Backend].
// Every read is scoped to what the calling user can see in their channel: the
// qURL API key is workspace-wide, so the Slack layer — not the LLM — enforces
// channel visibility via channel_policies. This is the boundary that keeps the
// agent from leaking resource existence across channels.
type agentBackend struct {
	authClient func(ctx context.Context, teamID string) (*client.Client, error)
	store      *slackdata.Store
	log        *slog.Logger

	// Per-turn memo of the channel's reachable resource set. A backend is built
	// once per turn (newAgentBackend in processAgentEvent) and reused across the
	// model's tool calls, which are sequential (parallel tool use is disabled),
	// so a plain field is safe — no mutex. The channel scope is invariant within
	// a turn, so list_resources + every resolve_token share one GetItem instead
	// of re-reading the same channel_policies row each call.
	allowed     map[string]struct{}
	allowedErr  error
	allowedDone bool
}

// newAgentBackend builds the backend from the handler's authenticated-client
// factory and admin store. log is used to surface backend read failures to
// operators — the agent loop collapses them to a generic string for the model,
// so without this the real error would be invisible.
func (h *Handler) newAgentBackend(log *slog.Logger) *agentBackend {
	if log == nil {
		log = slog.Default()
	}
	return &agentBackend{authClient: h.authenticatedClient, store: h.cfg.AdminStore, log: log}
}

// channelAllowed returns the channel's reachable resource-id set, fetched once
// and memoized for the turn.
func (b *agentBackend) channelAllowed(ctx context.Context, tc *agent.TurnContext) (map[string]struct{}, error) {
	if !b.allowedDone {
		b.allowed, b.allowedErr = b.store.AllowedResourceIDsForChannel(ctx, tc.TeamID, tc.ChannelID)
		b.allowedDone = true
	}
	return b.allowed, b.allowedErr
}

// fail logs a backend read error for operators and returns it wrapped with op
// context. The agent loop turns the error into a model-safe generic string, so
// this log is the only operator-visible record of why a read failed.
func (b *agentBackend) fail(op string, err error) (string, error) {
	b.log.Error("agent backend read failed", "op", op, "error", err)
	return "", fmt.Errorf("%s: %w", op, err)
}

// agentBackendUnconfigured is returned (as content, not an error) when the admin
// store isn't wired — a no-DDB deploy answers gracefully instead of erroring.
const agentBackendUnconfigured = "Resource information isn't available in this workspace yet."

// ListResources lists the resources reachable from the caller's channel, filtered
// through channel_policies.
func (b *agentBackend) ListResources(ctx context.Context, tc *agent.TurnContext) (string, error) {
	if b.store == nil {
		return agentBackendUnconfigured, nil
	}
	allowed, err := b.channelAllowed(ctx, tc)
	if err != nil {
		return b.fail("list resources: channel scope", err)
	}
	if len(allowed) == 0 {
		return "No resources are protected in this channel yet.", nil
	}
	c, err := b.authClient(ctx, tc.TeamID)
	if err != nil {
		return b.fail("list resources: client", err)
	}
	resources, err := collectChannelResources(ctx, c, allowed)
	if err != nil {
		return b.fail("list resources", err)
	}
	if len(resources) == 0 {
		return "No resources are protected in this channel yet.", nil
	}
	lines := make([]string, 0, len(resources))
	for i := range resources {
		lines = append(lines, formatResourceLine(&resources[i]))
	}
	sort.Strings(lines)
	return "Resources reachable in this channel:\n" + strings.Join(lines, "\n"), nil
}

// channelResourcesMaxPages bounds the pagination loop. At listResourcesPageLimit
// per page this scans up to 2,000 workspace resources for the channel's
// reachable set — far above any real workspace, while keeping the loop bounded.
const channelResourcesMaxPages = 20

// collectChannelResources pages through the workspace resource list and keeps
// only those in the channel's allowed set, stopping early once every allowed
// resource has been found. Listing is workspace-wide and paginated, so filtering
// only the first page would silently drop channel-reachable resources that sort
// past it in a workspace with more than one page of resources.
func collectChannelResources(ctx context.Context, c *client.Client, allowed map[string]struct{}) ([]client.Resource, error) {
	found := make([]client.Resource, 0, len(allowed))
	cursor := ""
	for page := 0; page < channelResourcesMaxPages; page++ {
		out, err := c.ListResources(ctx, client.ListResourcesInput{Limit: listResourcesPageLimit, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for i := range out.Resources {
			if _, ok := allowed[out.Resources[i].ResourceID]; ok {
				found = append(found, out.Resources[i])
			}
		}
		if len(found) >= len(allowed) || !out.HasMore || out.NextCursor == "" {
			break
		}
		cursor = out.NextCursor
	}
	return found, nil
}

// ListAliases lists the channel-scoped aliases visible to the caller.
func (b *agentBackend) ListAliases(ctx context.Context, tc *agent.TurnContext) (string, error) {
	if b.store == nil {
		return agentBackendUnconfigured, nil
	}
	entries, err := b.store.GetChannelPolicy(ctx, tc.TeamID, tc.ChannelID)
	if err != nil {
		return b.fail("list aliases", err)
	}
	if len(entries) == 0 {
		return "No aliases are bound in this channel.", nil
	}
	lines := make([]string, 0, len(entries))
	for i := range entries {
		lines = append(lines, fmt.Sprintf("- $%s → %s", entries[i].Alias, entries[i].ResourceID))
	}
	sort.Strings(lines)
	return "Aliases in this channel:\n" + strings.Join(lines, "\n"), nil
}

// ResolveToken resolves a $slug/$alias to its resource identity, channel-scoped
// and read-only. It never mints a link or grants access.
func (b *agentBackend) ResolveToken(ctx context.Context, tc *agent.TurnContext, token string) (string, error) {
	if b.store == nil {
		return agentBackendUnconfigured, nil
	}
	token = strings.TrimPrefix(strings.TrimSpace(token), "$")
	if token == "" {
		return "Provide a $alias or $slug to resolve.", nil
	}
	// Channel alias first.
	if rid, found, err := b.store.LookupChannelAlias(ctx, tc.TeamID, tc.ChannelID, token); err != nil {
		return b.fail("resolve token: alias lookup", err)
	} else if found {
		return fmt.Sprintf("`$%s` is an alias in this channel for resource `%s`.", token, rid), nil
	}
	// Otherwise a tunnel slug, but only reveal it if it's reachable here.
	allowed, err := b.channelAllowed(ctx, tc)
	if err != nil {
		return b.fail("resolve token: channel scope", err)
	}
	c, err := b.authClient(ctx, tc.TeamID)
	if err != nil {
		return b.fail("resolve token: client", err)
	}
	out, err := c.ListResources(ctx, client.ListResourcesInput{Slug: token})
	if err != nil {
		return b.fail("resolve token", err)
	}
	for i := range out.Resources {
		r := &out.Resources[i]
		if _, ok := allowed[r.ResourceID]; ok {
			return fmt.Sprintf("`$%s` is a connector in this channel: %s.", token, formatResourceLine(r)), nil
		}
	}
	return fmt.Sprintf("`$%s` doesn't resolve to anything reachable in this channel.", token), nil
}

// Quota reports the workspace plan and usage.
func (b *agentBackend) Quota(ctx context.Context, tc *agent.TurnContext) (string, error) {
	c, err := b.authClient(ctx, tc.TeamID)
	if err != nil {
		return b.fail("quota: client", err)
	}
	q, err := c.GetQuota(ctx)
	if err != nil {
		return b.fail("quota", err)
	}
	plan := q.Plan
	if plan == "" {
		plan = "unknown"
	}
	if q.Usage == nil {
		return fmt.Sprintf("Plan: %s.", plan), nil
	}
	return fmt.Sprintf("Plan: %s. Active qURLs: %d. Created this period: %d.", plan, q.Usage.ActiveQURLs, q.Usage.QURLsCreated), nil
}

// listResourcesPageLimit is the per-page size for the workspace resource list
// that collectChannelResources pages through to find the channel's reachable set.
const listResourcesPageLimit = 100

// formatResourceLine renders one resource for the model: alias-or-slug, display
// name, type, and id. Kept compact so several fit the context cheaply.
func formatResourceLine(r *client.Resource) string {
	label := r.Alias
	if label == "" {
		label = r.Slug
	}
	var b strings.Builder
	b.WriteString("- ")
	if label != "" {
		b.WriteString("$")
		b.WriteString(label)
		b.WriteString(" ")
	}
	if r.Description != "" {
		b.WriteString(r.Description)
		b.WriteString(" ")
	}
	b.WriteString("(")
	if r.Type != "" {
		b.WriteString(r.Type)
		b.WriteString(", ")
	}
	b.WriteString(r.ResourceID)
	b.WriteString(")")
	return b.String()
}
