package internal

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// Slack Events API lifecycle event types. Both arrive as an `event_callback`
// envelope whose inner `event.type` is one of these.
//
//   - app_uninstalled: the workspace removed the app. The bot token is dead.
//   - tokens_revoked: one or more of the app's tokens were revoked. Purge only
//     when Slack lists a bot token; a lone user-token revoke must not forget a
//     still-installed workspace. Today the app stores only the workspace bot
//     token (see shared/auth.SlackBotTokenInstall), but this explicit guard keeps
//     future user-token scopes from widening the teardown path by accident.
const (
	slackEventTypeAppUninstalled = "app_uninstalled"
	slackEventTypeTokensRevoked  = "tokens_revoked"
)

// lifecyclePurgeTimeout bounds the asynchronous workspace purge kicked off by a
// lifecycle event. It is generous relative to the few DeleteItem/Query calls a
// purge makes (workspace_state row + workspace_mappings row + the team's
// channel_policies rows) but still bounded so a DDB brownout can't pin an async
// worker slot indefinitely. The purge runs off h.baseCtx (NOT the request ctx),
// because the 200 OK is returned to Slack before the purge starts.
const lifecyclePurgeTimeout = 20 * time.Second

// isLifecycleEvent reports whether an inner event is an install-teardown signal
// that should trigger a full workspace purge.
func isLifecycleEvent(event *slackInnerEvent) bool {
	if event == nil {
		return false
	}
	switch event.Type {
	case slackEventTypeAppUninstalled:
		return true
	case slackEventTypeTokensRevoked:
		return event.Tokens != nil && len(event.Tokens.Bot) > 0
	default:
		return false
	}
}

// lifecycleWorkspaceIDs resolves every workspace identity a lifecycle event may
// refer to, matching the per-workspace DDB partition key: team_id for a normal
// install, and enterprise_id for an org-level Grid install. Slack Grid lifecycle
// payloads can carry both IDs, while the stored bot token may live under the
// enterprise key, so callers purge each unique candidate idempotently.
func lifecycleWorkspaceIDs(env *slackEventEnvelope) []string {
	var ids []string
	seen := map[string]struct{}{}
	for _, raw := range []string{env.TeamID, env.EnterpriseID} {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
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
	workspaceIDs := lifecycleWorkspaceIDs(env)
	log := slog.With(
		"surface", "lifecycle",
		"event_type", env.Event.Type,
		"team_id", env.TeamID,
		"enterprise_id", env.EnterpriseID,
		"event_id", env.EventID,
	)
	if len(workspaceIDs) == 0 {
		log.Warn("lifecycle event with no team_id/enterprise_id — nothing to purge")
		return
	}
	log.Info("lifecycle event received — purging workspace data", "workspace_ids", workspaceIDs)
	// Off the request goroutine: handleEvent has already written 200, and the
	// purge's DeleteItem/Query calls must not block (or fail) that ack. h.Go is
	// wg-tracked so a graceful shutdown drains an in-flight purge.
	h.Go(func() {
		ctx, cancel := context.WithTimeout(h.baseCtx, lifecyclePurgeTimeout)
		defer cancel()
		for _, workspaceID := range workspaceIDs {
			h.purgeWorkspace(ctx, log.With("workspace_id", workspaceID), workspaceID)
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
//
// Best-effort by design: it ATTEMPTS all three deletes regardless of whether any
// one fails, so a transient error on one table never strands the others. Each
// delete is independently idempotent (an absent row is a no-op), so a partial
// prior purge — or a re-delivered Slack event — converges cleanly on a retry.
// Failures are logged (workspace id only; the deletes carry no token material, so
// nothing secret is logged) and otherwise swallowed: a lifecycle ack has already
// been sent, and `/qurl uninstall` reports success off its own primary
// DeleteAPIKey result, not this sweep.
//
// Upstream qURL key revocation is NOT done here — it is the caller's concern (the
// `/qurl uninstall` path best-efforts it before calling this; the lifecycle path
// has no live credential to revoke with). This keeps purgeWorkspace a pure local
// storage sweep.
func (h *Handler) purgeWorkspace(ctx context.Context, log *slog.Logger, workspaceID string) {
	if workspaceID == "" {
		log.Warn("purgeWorkspace called with empty workspace id — skipping")
		return
	}

	// workspace_state — the encrypted bot token lives here, so this is the
	// load-bearing delete for the Marketplace "uninstall forgets the token"
	// requirement. Skip silently when the provider can't delete a row (sandbox /
	// EnvProvider); there's no per-workspace state to remove in that mode.
	if deleter, ok := h.cfg.AuthProvider.(workspaceStateDeleter); ok {
		if err := deleter.DeleteWorkspaceState(ctx, workspaceID); err != nil {
			log.Error("purgeWorkspace: failed to delete workspace_state row", "error", err)
		} else {
			log.Info("purgeWorkspace: deleted workspace_state row")
		}
	} else {
		log.Debug("purgeWorkspace: auth provider does not support workspace_state delete — skipping")
	}

	// workspace_mappings + channel_policies live behind AdminStore. When it's
	// unwired (sandbox / no-DDB) there's nothing to purge there.
	if h.cfg.AdminStore == nil {
		log.Debug("purgeWorkspace: AdminStore unwired — skipping mappings/policies purge")
		return
	}
	if err := h.cfg.AdminStore.DeleteWorkspaceMapping(ctx, workspaceID); err != nil {
		log.Error("purgeWorkspace: failed to delete workspace_mappings row", "error", err)
	} else {
		log.Info("purgeWorkspace: deleted workspace_mappings row")
	}
	if err := h.cfg.AdminStore.PurgeTeamChannelPolicies(ctx, workspaceID); err != nil {
		log.Error("purgeWorkspace: failed to purge channel_policies rows", "error", err)
	} else {
		log.Info("purgeWorkspace: purged channel_policies rows")
	}
}
