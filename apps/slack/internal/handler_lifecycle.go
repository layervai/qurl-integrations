package internal

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/shared/auth"
)

// Slack Events API lifecycle event types. Both arrive as an `event_callback`
// envelope whose inner `event.type` is one of these.
//
//   - app_uninstalled: the workspace removed the app. The bot token is dead.
//   - tokens_revoked: one or more of the app's tokens were revoked. Purge only
//     when Slack lists a bot token; a lone user-token revoke must not forget a
//     still-installed workspace. Today the app stores only the workspace bot
//     token (see shared/auth.SlackBotTokenInstall), but this explicit guard keeps
//     future user-token scopes from widening the teardown path by accident. If
//     Slack bot-token rotation is enabled later, revisit this path before
//     treating rotated bot tokens as teardown signals.
const (
	slackEnvelopeTypeEventCallback = "event_callback"
	slackEventTypeAppUninstalled   = "app_uninstalled"
	slackEventTypeTokensRevoked    = "tokens_revoked"
	lifecycleEventTypeUnknown      = "unknown"
)

// lifecyclePurgeTimeout bounds each asynchronous workspace-id purge kicked off
// by a lifecycle event. It is generous relative to the few DeleteItem/Query
// calls a purge makes (workspace_state row + agent_state partition +
// workspace_mappings row + the team's channel_policies rows) but still bounded
// so a DDB brownout can't pin an async worker slot indefinitely. The purge runs
// off h.baseCtx (NOT the request ctx), because the 200 OK is returned to Slack
// before the purge starts.
const lifecyclePurgeTimeout = 20 * time.Second

// lifecyclePurgeRetryAttempts gives an ack-first purge a bounded transient-DDB
// recovery path after Slack has already received 200 OK and will not redeliver
// the event. Each workspace id's retry loop still lives inside
// lifecyclePurgeTimeout. A retry restarts purgeWorkspace from its first query;
// deletes are idempotent, so partial progress persists, but very large
// partitions that cannot drain inside this fixed budget need the batched drain
// optimization tracked in #928.
const (
	lifecyclePurgeRetryAttempts  = 3
	lifecyclePurgeRetryBaseDelay = 500 * time.Millisecond
)

// isLifecycleEvent reports whether an inner event is an install-teardown signal
// that should trigger a full workspace purge.
func isLifecycleEvent(event *slackInnerEvent, botTokenRotationEnabled bool) bool {
	if event == nil {
		return false
	}
	switch event.Type {
	case slackEventTypeAppUninstalled:
		return true
	case slackEventTypeTokensRevoked:
		return !botTokenRotationEnabled && isBotTokensRevokedEvent(event)
	default:
		return false
	}
}

func isBotTokensRevokedEvent(event *slackInnerEvent) bool {
	return event != nil && event.Type == slackEventTypeTokensRevoked && event.Tokens != nil && len(event.Tokens.Bot) > 0
}

// orderedIDSet accumulates non-empty, whitespace-trimmed workspace ids in
// insertion order while skipping duplicates. The lifecycle and slash-uninstall
// resolvers both add candidates incrementally with fallbacks between adds, so a
// tiny accumulator keeps those call sites explicit without duplicating the
// trim/dedupe closure.
type orderedIDSet struct {
	seen map[string]struct{}
	ids  []string
}

func (s *orderedIDSet) add(raw string) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return
	}
	if s.seen == nil {
		s.seen = map[string]struct{}{}
	}
	if _, ok := s.seen[id]; ok {
		return
	}
	s.seen[id] = struct{}{}
	s.ids = append(s.ids, id)
}

func (s *orderedIDSet) empty() bool {
	return len(s.ids) == 0
}

type slackEventPartitionResolution struct {
	agentWrite              string
	lifecyclePurge          []string
	lifecycleAgentStateOnly []string
}

// resolveSlackEventPartitions keeps the agent write key and lifecycle purge keys
// in one place. Org-level Grid installs write conversation/dedupe rows under the
// enterprise id, but several agent-state item types are always team-keyed
// (pending actions, audit, pane context, rate counters). For org lifecycle
// callbacks, the full cascade stays on the enterprise partition while delivered
// team ids get an agent-state-only sweep so a mixed workspace-level install's
// bot token, mapping, and policies are not over-deleted.
func resolveSlackEventPartitions(env *slackEventEnvelope) slackEventPartitionResolution {
	if env == nil {
		return slackEventPartitionResolution{}
	}
	var purge orderedIDSet
	if len(env.Authorizations) == 0 {
		purge.add(env.TeamID)
		if purge.empty() {
			purge.add(env.EnterpriseID)
		}
		return slackEventPartitionResolution{agentWrite: firstResolvedID(purge.ids), lifecyclePurge: purge.ids}
	}

	enterpriseInstall := false
	var enterpriseIDs orderedIDSet
	var teamIDs orderedIDSet
	for _, authz := range env.Authorizations {
		if authz.IsEnterpriseInstall {
			enterpriseInstall = true
			enterpriseIDs.add(authz.EnterpriseID)
			teamIDs.add(authz.TeamID)
		}
	}
	if enterpriseInstall {
		agentWrite := firstResolvedID(enterpriseIDs.ids)
		if agentWrite == "" {
			agentWrite = strings.TrimSpace(env.EnterpriseID)
		}
		if agentWrite == "" {
			agentWrite = strings.TrimSpace(env.TeamID)
		}
		if agentWrite == "" {
			agentWrite = firstResolvedID(teamIDs.ids)
		}
		purge.add(agentWrite)
		var agentStateOnly orderedIDSet
		addAgentStateOnly := func(raw string) {
			id := strings.TrimSpace(raw)
			if id == "" || id == agentWrite {
				return
			}
			agentStateOnly.add(id)
		}
		addAgentStateOnly(env.TeamID)
		for _, teamID := range teamIDs.ids {
			addAgentStateOnly(teamID)
		}
		return slackEventPartitionResolution{
			agentWrite:              agentWrite,
			lifecyclePurge:          purge.ids,
			lifecycleAgentStateOnly: agentStateOnly.ids,
		}
	}

	purge.add(env.TeamID)
	for _, authz := range env.Authorizations {
		purge.add(authz.TeamID)
	}
	if purge.empty() {
		purge.add(env.EnterpriseID)
	}
	return slackEventPartitionResolution{agentWrite: firstResolvedID(purge.ids), lifecyclePurge: purge.ids}
}

func firstResolvedID(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func lifecycleEventTypeForLog(eventType string) string {
	switch eventType {
	case slackEventTypeAppUninstalled, slackEventTypeTokensRevoked:
		return eventType
	default:
		return lifecycleEventTypeUnknown
	}
}

// lifecycleWorkspaceIDs resolves the DDB partition key(s) that receive the full
// workspace cascade. Slack's Events API authorization metadata disambiguates
// Enterprise Grid org installs from workspace installs: org installs fully purge
// the enterprise partition, while any delivered team ids are handled by an
// agent-state-only sweep. Older/partial payloads without authorizations cannot
// safely prove org-install vs workspace-install when both IDs are present, so
// they use team_id when present because Slack documents team_id as the workspace
// identifier for token-revocation callbacks; enterprise_id is only a fallback
// when team_id is absent.
func lifecycleWorkspaceIDs(env *slackEventEnvelope) []string {
	return resolveSlackEventPartitions(env).lifecyclePurge
}

func lifecyclePurgeCutoff(env *slackEventEnvelope, observedAt time.Time) time.Time {
	observedAt = observedAt.UTC()
	if env == nil || env.EventTime <= 0 {
		return observedAt
	}
	eventAt := time.Unix(env.EventTime, 0).UTC()
	if eventAt.Before(observedAt) {
		return eventAt
	}
	return observedAt
}

// handleLifecycleEvent runs the Slack app-uninstall / token-revoke cascade. It is
// reached from handleEvent for an event_callback whose inner type is an install
// teardown, AFTER the Slack signature has been verified and AFTER handleEvent has
// already committed to responding 200 OK. It therefore never touches the
// http.ResponseWriter — the ack is the caller's job — and it schedules the purge
// asynchronously so a slow DDB sweep can't delay (or fail) the 200 that stops
// Slack from retrying the delivery forever.
//
// A purge with no resolvable workspace id is dropped with a log line: there are
// no rows to delete and issuing a delete against an empty key would only 400.
func (h *Handler) handleLifecycleEvent(env *slackEventEnvelope) {
	purgeCutoff := lifecyclePurgeCutoff(env, h.now())
	partitionResolution := resolveSlackEventPartitions(env)
	workspaceIDs := partitionResolution.lifecyclePurge
	agentStateOnlyIDs := partitionResolution.lifecycleAgentStateOnly
	log := slog.With(
		"surface", "lifecycle",
		"event_type", lifecycleEventTypeForLog(env.Event.Type),
		"has_team_id", env.TeamID != "",
		"has_enterprise_id", env.EnterpriseID != "",
		"has_event_id", env.EventID != "",
	)
	if len(workspaceIDs) == 0 && len(agentStateOnlyIDs) == 0 {
		log.Warn("lifecycle event with no team_id/enterprise_id — nothing to purge")
		return
	}
	log.Info("lifecycle event received — purging workspace data",
		"workspace_id_count", len(workspaceIDs),
		"agent_state_only_workspace_id_count", len(agentStateOnlyIDs),
		"purge_cutoff", purgeCutoff.UTC().Format(time.RFC3339),
		"upstream_qurl_key_revoke_deferred", true,
		"follow_up_issue", "layervai/qurl-integrations#926",
	)
	// Off the request goroutine: handleEvent has already written 200, and the
	// purge's DeleteItem/Query calls must not block (or fail) that ack. h.Go waits
	// for the purge goroutine to unwind during shutdown, but h.baseCtx cancellation
	// can still abort the sweep before it completes; #927 tracks durable recovery
	// for that ack-after-loss window. This deliberately bypasses the general async
	// semaphore: lifecycle events are rare, and dropping a teardown because the
	// slash/agent pool is full would lose the signal after Slack has already
	// received 200.
	h.Go(func() {
		baseCtx := h.baseCtx
		if baseCtx == nil {
			baseCtx = context.Background()
		}
		for _, workspaceID := range workspaceIDs {
			ctx, cancel := context.WithTimeout(baseCtx, lifecyclePurgeTimeout)
			h.purgeWorkspaceWithRetry(ctx, log.With("workspace_id", workspaceID), workspaceID, purgeCutoff)
			cancel()
		}
		for _, workspaceID := range agentStateOnlyIDs {
			ctx, cancel := context.WithTimeout(baseCtx, lifecyclePurgeTimeout)
			h.purgeAgentStateWithRetry(ctx, log.With("workspace_id", workspaceID, "agent_state_only", true), workspaceID)
			cancel()
		}
	})
}

// workspaceStateDeleter is the optional capability a mutable AuthProvider
// implements to remove an ENTIRE workspace_state row (encrypted Slack bot token,
// encrypted qURL key, install metadata — all of it) for the uninstall/revoke
// cascade. *auth.DDBProvider implements it; providers that can't (e.g.
// auth.EnvProvider) are simply skipped by the purge. It is kept off the base
// auth.Provider interface — and discovered by type assertion — for the same
// reason as workspaceKeyRevoker: the base interface stays minimal and non-Slack
// consumers (cli) need no workspace-row teardown. The narrow per-consumer
// interface is the repo's standard seam for these capability checks.
type workspaceStateDeleter interface {
	DeleteWorkspaceState(ctx context.Context, workspaceID string) error
}

// workspaceStateIdentityDeleter is the production auth-provider extension that
// atomically returns the old qURL key identity while deleting workspace_state.
// Lifecycle cannot revoke upstream yet (#926), but logging this non-secret
// identity before the local row disappears keeps operator/manual revocation
// actionable without delaying Marketplace-required local data removal.
type workspaceStateIdentityDeleter interface {
	DeleteWorkspaceStateWithIdentity(ctx context.Context, workspaceID string) (auth.DeletedWorkspaceStateIdentity, error)
}

type workspaceStateBeforeIdentityDeleter interface {
	DeleteWorkspaceStateBeforeWithIdentity(ctx context.Context, workspaceID string, cutoff time.Time) (auth.DeletedWorkspaceStateIdentity, error)
}

// purgeWorkspace forgets every trace of a Slack workspace's bot data across the
// per-workspace DynamoDB tables. It is the storage cascade behind both a Slack
// lifecycle teardown (app_uninstalled / tokens_revoked) and the `/qurl uninstall`
// command:
//
//   - workspace_state (auth provider): the encrypted Slack bot token + its data
//     key, the encrypted qURL API key + its data key, and all install/setup
//     metadata. Removed via the workspaceStateDeleter capability.
//   - workspace_mappings (AdminStore): owner + admin set + agent toggle.
//   - channel_policies (AdminStore): every channel's alias_bindings and
//     allowed_resource_ids for this team.
//   - qurl_agent_state (AgentStore): conversation transcripts, dedupe markers,
//     pending actions, pane context, rate counters, and recent action audit rows
//     under this workspace/enterprise partition.
//
// Best-effort by design: it ATTEMPTS every delete regardless of whether any
// one fails, so a transient error on one table never strands the others. Each
// delete is independently idempotent (an absent row is a no-op), so a partial
// prior purge converges cleanly when a later retry or fresh teardown signal runs.
// Failures are logged (workspace id only; the deletes carry no token material, so
// nothing secret is logged) and returned as a joined error so async callers can
// retry or emit a final manual-cleanup signal. A lifecycle ack has already been
// sent, and `/qurl uninstall` reports success off its own primary DeleteAPIKey
// result, not this sweep.
//
// Upstream qURL key revocation is NOT done here — it is the caller's concern.
// The `/qurl uninstall` path best-efforts it before calling this. The lifecycle
// path currently prioritizes local data deletion because owner-authorized
// upstream revocation from Slack app-console uninstall is not available yet.
// TODO(#926): once owner-auth revocation exists, revoke-before-purge for
// lifecycle events too so app-console uninstalls do not leave a live upstream
// qURL key that is no longer locally referenceable.
//
// Keeping purgeWorkspace a pure local storage sweep keeps the auth package free
// of the qurl-service client dependency.
func (h *Handler) purgeWorkspace(ctx context.Context, log *slog.Logger, workspaceID string, cutoff time.Time) error {
	if workspaceID == "" {
		log.Warn("purgeWorkspace called with empty workspace id — skipping")
		return nil
	}

	var errs []error

	// workspace_state — the encrypted bot token lives here, so this is the
	// load-bearing delete for the Marketplace "uninstall forgets the token"
	// requirement. Skip silently when the provider can't delete a row (sandbox /
	// EnvProvider); there's no per-workspace state to remove in that mode.
	switch deleter := h.cfg.AuthProvider.(type) {
	case workspaceStateBeforeIdentityDeleter:
		identity, err := deleter.DeleteWorkspaceStateBeforeWithIdentity(ctx, workspaceID, cutoff)
		switch {
		case err == nil:
			logWorkspaceStateDeleted(log, identity)
		case errors.Is(err, auth.ErrWorkspaceStateUpdatedAfterCutoff):
			log.Info("purgeWorkspace: retained workspace_state row updated after purge cutoff")
		default:
			log.Error("purgeWorkspace: failed to delete workspace_state row", "error", err)
			errs = append(errs, err)
		}
	case workspaceStateIdentityDeleter:
		identity, err := deleter.DeleteWorkspaceStateWithIdentity(ctx, workspaceID)
		if err != nil {
			log.Error("purgeWorkspace: failed to delete workspace_state row", "error", err)
			errs = append(errs, err)
		} else {
			logWorkspaceStateDeleted(log, identity)
		}
	case workspaceStateDeleter:
		if err := deleter.DeleteWorkspaceState(ctx, workspaceID); err != nil {
			log.Error("purgeWorkspace: failed to delete workspace_state row", "error", err)
			errs = append(errs, err)
		} else {
			logWorkspaceStateDeleted(log, auth.DeletedWorkspaceStateIdentity{})
		}
	default:
		log.Debug("purgeWorkspace: auth provider does not support workspace_state delete — skipping")
	}

	// qurl_agent_state is optional because conversation mode can be dark or
	// partially configured. When wired, purge the partition explicitly rather than
	// waiting for DynamoDB TTL so Marketplace app deletion forgets user-authored
	// agent data on the same teardown path as the durable workspace tables.
	if h.cfg.AgentStore != nil {
		if err := h.cfg.AgentStore.PurgeWorkspaceAgentState(ctx, workspaceID); err != nil {
			log.Error("purgeWorkspace: failed to purge qurl_agent_state rows", "error", err)
			errs = append(errs, err)
		} else {
			log.Info("purgeWorkspace: purged qurl_agent_state rows")
		}
	} else {
		log.Debug("purgeWorkspace: AgentStore unwired — skipping qurl_agent_state purge")
	}

	// workspace_mappings + channel_policies live behind AdminStore. When it's
	// unwired (sandbox / no-DDB) there's nothing to purge there.
	if h.cfg.AdminStore == nil {
		log.Debug("purgeWorkspace: AdminStore unwired — skipping mappings/policies purge")
		return errors.Join(errs...)
	}
	if err := h.cfg.AdminStore.DeleteWorkspaceMappingBefore(ctx, workspaceID, cutoff); err != nil {
		log.Error("purgeWorkspace: failed to delete workspace_mappings row", "error", err)
		errs = append(errs, err)
	} else {
		log.Info("purgeWorkspace: purged or retained workspace_mappings row")
	}
	if err := h.cfg.AdminStore.PurgeTeamChannelPoliciesBefore(ctx, workspaceID, cutoff); err != nil {
		log.Error("purgeWorkspace: failed to purge channel_policies rows", "error", err)
		errs = append(errs, err)
	} else {
		log.Info("purgeWorkspace: purged or retained channel_policies rows")
	}
	return errors.Join(errs...)
}

func logWorkspaceStateDeleted(log *slog.Logger, identity auth.DeletedWorkspaceStateIdentity) {
	if !identity.Deleted {
		log.Info("purgeWorkspace: workspace_state row absent or retained")
		return
	}
	attrs := []any{}
	if identity.QURLAPIKeyID != "" {
		attrs = append(attrs,
			"deleted_workspace_qurl_key_id_present", true,
			"deleted_workspace_qurl_account_id_present", identity.QURLAccountID != "",
			"upstream_qurl_key_identity_seen", true,
			"upstream_qurl_key_follow_up_issue", "layervai/qurl-integrations#926",
		)
	}
	log.Info("purgeWorkspace: deleted workspace_state row", attrs...)
}

func (h *Handler) purgeWorkspaceWithRetry(ctx context.Context, log *slog.Logger, workspaceID string, cutoff time.Time) {
	retryLifecyclePurge(ctx, log, "purgeWorkspace", func(attemptLog *slog.Logger) error {
		return h.purgeWorkspace(ctx, attemptLog, workspaceID, cutoff)
	})
}

func (h *Handler) purgeAgentStateWithRetry(ctx context.Context, log *slog.Logger, workspaceID string) {
	retryLifecyclePurge(ctx, log, "purgeAgentState", func(attemptLog *slog.Logger) error {
		return h.purgeAgentStatePartition(ctx, attemptLog, workspaceID)
	})
}

func (h *Handler) purgeAgentStatePartition(ctx context.Context, log *slog.Logger, workspaceID string) error {
	if workspaceID == "" {
		log.Warn("purgeAgentState called with empty workspace id — skipping")
		return nil
	}
	if h.cfg.AgentStore == nil {
		log.Debug("purgeAgentState: AgentStore unwired — skipping qurl_agent_state purge")
		return nil
	}
	if err := h.cfg.AgentStore.PurgeWorkspaceAgentState(ctx, workspaceID); err != nil {
		log.Error("purgeAgentState: failed to purge qurl_agent_state rows", "error", err)
		return err
	}
	log.Info("purgeAgentState: purged qurl_agent_state rows")
	return nil
}

func retryLifecyclePurge(ctx context.Context, log *slog.Logger, op string, purge func(*slog.Logger) error) {
	for attempt := 1; attempt <= lifecyclePurgeRetryAttempts; attempt++ {
		attemptLog := log.With("attempt", attempt, "max_attempts", lifecyclePurgeRetryAttempts)
		err := purge(attemptLog)
		if err == nil {
			return
		}
		if attempt == lifecyclePurgeRetryAttempts {
			// Keep cleanup_action_required stable: qurl-integrations-infra#1284
			// tracks the CloudWatch alarm that should page on this field.
			log.Error(op+": exhausted retries; manual cleanup may be required",
				"attempts", attempt,
				"cleanup_action_required", true,
				"error", err,
			)
			return
		}

		delay := time.Duration(attempt) * lifecyclePurgeRetryBaseDelay
		log.Warn(op+": retrying failed purge",
			"attempt", attempt,
			"next_attempt", attempt+1,
			"retry_delay", delay.String(),
			"error", err,
		)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			log.Error(op+": retry canceled before next attempt", "error", ctx.Err())
			return
		case <-timer.C:
		}
	}
}
