package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

// Compile-time check that the adapter satisfies the agent port.
var _ agent.Backend = (*agentBackend)(nil)

// agentBackend adapts the qURL client and channel_policies to [agent.Backend].
// Every read is scoped to what the calling user can see in their channel: the
// qURL API key is workspace-wide, so the Slack layer — not the LLM — enforces
// channel visibility via channel_policies. This is the boundary that keeps the
// agent from leaking resource existence across channels.
//
// The scope is deliberately CHANNEL-level, not per-individual-user: every member
// of a channel shares its channel_policies view, so "what the calling user can
// access" is approximated by channel membership — matching Slack's own channel
// visibility model (the agent never reveals more than a channel member could
// already reach with the slash commands). A tighter per-user resource check would
// need user-level identity beyond the workspace key; that's a deliberate v1 choice,
// not an oversight. See Slack's agent-design data-boundary guidance.
type agentBackend struct {
	authClient func(ctx context.Context, teamID string) (*client.Client, error)
	store      *slackdata.Store
	log        *slog.Logger

	// Per-turn memo of the channel's reachable resource set. A backend is built
	// once per turn (newAgentBackend in processAgentEvent) and reused across the
	// model's tool calls; the channel scope is invariant within a turn, so
	// list_resources + every resolve_token share one GetItem instead of
	// re-reading the same channel_policies row each call. sync.Once keeps the
	// memo safe even if parallel tool use is ever enabled (today it's disabled,
	// so calls are sequential).
	allowedOnce sync.Once
	allowed     map[string]struct{}
	allowedErr  error

	// Per-turn memo of the channel's reachable resource scan. list_resources can
	// be called several times in a turn; collectChannelResources pages the
	// workspace-wide list (up to channelResourcesMaxPages reads — and a stale
	// channel_policies id defeats its early-stop, forcing the full scan), so
	// without this memo every call re-pages. The scan is invariant within a
	// read-only turn (the agent loop only reads; mutations are separate click
	// interactions), so one scan is shared across the turn. Like allowedOnce, a
	// failed scan is cached too: a transient error on the first call fails
	// list_resources for the rest of the turn rather than re-hammering a failing
	// API mid-scan — intentional, fail-fast.
	resourcesOnce sync.Once
	resources     []client.Resource
	resourcesErr  error

	// Per-turn memo of the channel's alias bindings (GetChannelPolicy), shared by
	// list_aliases and list_resources. list_resources joins it (rid -> $channelAlias) to
	// name resources that carry no intrinsic alias/slug — e.g. an agent-protected URL,
	// whose handle lives here, not on the qURL resource. Mirrors allowedOnce: same
	// channel_policies row, a different projection (one extra GetItem/turn, negligible
	// against the workspace resource scan).
	policyOnce    sync.Once
	policyEntries []slackdata.PolicyEntry
	policyErr     error
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
	b.allowedOnce.Do(func() {
		b.allowed, b.allowedErr = b.store.AllowedResourceIDsForChannel(ctx, tc.TeamID, tc.ChannelID)
	})
	return b.allowed, b.allowedErr
}

// channelResources returns the channel's reachable resource set, scanned once and
// memoized for the turn (mirrors channelAllowed). ctx, c, and allowed are used
// only on the first call; subsequent calls return the cached scan, so
// list_resources costs one workspace scan per turn no matter how many times the
// model calls it.
func (b *agentBackend) channelResources(ctx context.Context, c *client.Client, allowed map[string]struct{}) ([]client.Resource, error) {
	b.resourcesOnce.Do(func() {
		b.resources, b.resourcesErr = collectChannelResources(ctx, c, allowed)
	})
	return b.resources, b.resourcesErr
}

// channelPolicy returns the channel's alias bindings, fetched once and memoized for the
// turn (mirrors channelAllowed). Shared by list_aliases (the binding list) and
// list_resources (the rid -> $channelAlias label join).
func (b *agentBackend) channelPolicy(ctx context.Context, tc *agent.TurnContext) ([]slackdata.PolicyEntry, error) {
	b.policyOnce.Do(func() {
		b.policyEntries, b.policyErr = b.store.GetChannelPolicy(ctx, tc.TeamID, tc.ChannelID)
	})
	return b.policyEntries, b.policyErr
}

// fail logs a backend read error for operators and returns it wrapped with op
// context. The agent loop turns the error into a model-safe generic string, so
// this log is the only operator-visible record of why a read failed.
func (b *agentBackend) fail(op string, err error) (string, error) {
	b.log.Error("agent backend read failed", "op", op, "error", err)
	return "", fmt.Errorf("%s: %w", op, err)
}

// authClientForTurn resolves the workspace's qURL client for a tool call,
// translating the "workspace not connected" case into the same actionable nudge
// the slash path uses (workspaceNotSetupMessage), returned as CONTENT so the
// model relays "run /qurl setup <email>" instead of a generic model-safe error.
// Any other client error stays a fail() (logged, collapsed to the generic
// string). nudged is true on the unbound case so callers return the nudge
// without treating it as a hard error. The nudge is workspace state, not an
// LLM-distilled value, so it is safe to surface verbatim.
func (b *agentBackend) authClientForTurn(ctx context.Context, op string, tc *agent.TurnContext) (c *client.Client, nudge string, nudged bool, errOut error) {
	c, err := b.authClient(ctx, tc.TeamID)
	if err == nil {
		return c, "", false, nil
	}
	if errors.Is(err, auth.ErrWorkspaceNotConfigured) {
		return nil, workspaceNotSetupMessage, true, nil
	}
	_, errOut = b.fail(op, err)
	return nil, "", false, errOut
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
	c, nudge, nudged, err := b.authClientForTurn(ctx, "list resources: client", tc)
	if nudged {
		return nudge, nil
	}
	if err != nil {
		return "", err
	}
	resources, err := b.channelResources(ctx, c, allowed)
	if err != nil {
		return b.fail("list resources", err)
	}
	if len(resources) == 0 {
		return "No resources are protected in this channel yet.", nil
	}
	// Channel alias bindings name resources that carry no intrinsic alias/slug (e.g.
	// agent-protected URLs). Build rid -> channel alias. alias_bindings is map[alias]rid,
	// so a resource can have several; GetChannelPolicy ranges that Go map (randomized
	// order), so pick the lexicographically smallest deterministically — otherwise the
	// label for a multi-bound resource would flip turn-to-turn.
	entries, err := b.channelPolicy(ctx, tc)
	if err != nil {
		return b.fail("list resources: aliases", err)
	}
	channelAlias := make(map[string]string, len(entries))
	for i := range entries {
		rid, alias := entries[i].ResourceID, entries[i].Alias
		if cur, ok := channelAlias[rid]; !ok || alias < cur {
			channelAlias[rid] = alias
		}
	}
	lines := make([]string, 0, len(resources))
	for i := range resources {
		lines = append(lines, formatResourceLine(&resources[i], channelAlias[resources[i].ResourceID]))
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
//
// The len(found) >= len(allowed) early-stop can't fire if a channel_policies row
// references a resource id that no longer exists workspace-side (a stale policy):
// found never reaches len(allowed), so the loop runs the full
// channelResourcesMaxPages. Correct, just worst-case more reads until the stale
// row is cleaned up. channelResources memoizes this per turn, so that worst-case
// scan is paid at most once per turn however many times list_resources is called
// (it bounds the repeat, not the single-scan cost).
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
	entries, err := b.channelPolicy(ctx, tc)
	if err != nil {
		return b.fail("list aliases", err)
	}
	if len(entries) == 0 {
		return "No aliases are bound in this channel.", nil
	}
	lines := make([]string, 0, len(entries))
	for i := range entries {
		// Just the alias — its bound resource id is internal plumbing the model would
		// otherwise echo to the user. The alias IS the user-facing handle.
		lines = append(lines, "- $"+entries[i].Alias)
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
	// Channel alias first. No `allowed`-set gate here (unlike the slug branch
	// below): LookupChannelAlias reads only THIS channel's alias_bindings, so it is
	// inherently same-channel — no cross-channel leak — and
	// AllowedResourceIDsForChannel unions alias_bindings into the allowed set, so a
	// bound rid is in `allowed` by construction; a gate would be a no-op. (A binding
	// to a since-deleted workspace resource would resolve here while list_resources
	// omits it, but gating on `allowed` wouldn't change that — the rid is still in
	// the union.)
	if _, found, err := b.store.LookupChannelAlias(ctx, tc.TeamID, tc.ChannelID, token); err != nil {
		return b.fail("resolve token: alias lookup", err)
	} else if found {
		// Confirm it resolves, but don't surface the opaque resource id it binds to.
		return fmt.Sprintf("`$%s` is an alias bound in this channel.", token), nil
	}
	// Otherwise a tunnel slug, but only reveal it if it's reachable here.
	allowed, err := b.channelAllowed(ctx, tc)
	if err != nil {
		return b.fail("resolve token: channel scope", err)
	}
	c, nudge, nudged, err := b.authClientForTurn(ctx, "resolve token: client", tc)
	if nudged {
		return nudge, nil
	}
	if err != nil {
		return "", err
	}
	out, err := c.ListResources(ctx, client.ListResourcesInput{Slug: token})
	if err != nil {
		return b.fail("resolve token", err)
	}
	for i := range out.Resources {
		r := &out.Resources[i]
		if _, ok := allowed[r.ResourceID]; ok {
			// Symmetric with the alias branch above: confirm it resolves without echoing
			// the token twice (embedding formatResourceLine would repeat `$token`) or
			// coupling to that helper's bullet format. "connector" already conveys the type.
			return fmt.Sprintf("`$%s` is a connector in this channel.", token), nil
		}
	}
	return fmt.Sprintf("`$%s` doesn't resolve to anything reachable in this channel.", token), nil
}

// Quota reports the workspace plan and usage.
func (b *agentBackend) Quota(ctx context.Context, tc *agent.TurnContext) (string, error) {
	c, nudge, nudged, err := b.authClientForTurn(ctx, "quota: client", tc)
	if nudged {
		return nudge, nil
	}
	if err != nil {
		return "", err
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

// formatResourceLine renders one resource for the model: a $handle (channel alias,
// else the resource's intrinsic alias, else its slug), display name, and type. The
// internal resource id is deliberately OMITTED — it's opaque plumbing customers don't
// care about, and the model echoes tool output verbatim, so the only reliable way to
// keep `r_…` out of a user-facing reply is to keep it out of the model's context.
// channelAlias is the in-channel binding and wins: it's the handle members type after
// /qurl get, and the ONLY handle for an agent-protected URL (no intrinsic alias, no
// slug). Pass "" when the channel doesn't bind this resource. Kept compact so several
// fit the context cheaply.
func formatResourceLine(r *client.Resource, channelAlias string) string {
	label := channelAlias
	if label == "" {
		label = r.Alias
	}
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
	if r.Type != "" {
		b.WriteString("(")
		b.WriteString(r.Type)
		b.WriteString(")")
	}
	return strings.TrimRight(b.String(), " ")
}
