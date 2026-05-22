// Per-guild qurl-service webhook subscription provisioning.
// Called from the two setGuildApiKey call sites + the backfill script.
// Never re-throws to the caller: the OAuth callback / /qurl setup
// should succeed for key linking even when view-counter wiring fails
// (the polling fallback covers it until backfill catches up).

const config = require('./config');
const db = require('./store');
const logger = require('./logger');
const { AUDIT_EVENTS } = require('./constants');
const { ensureWebhookSubscription, deleteSubscription } = require('./qurl-webhook-registrar');
const subs = require('./webhook-subscriptions');

// Frozen discriminator for the `reason` field on link results.
// Audit-log parsers / dashboards key on this; keeping it as an enum
// catches typos that would silently break a metric filter.
const LINK_RESULTS = Object.freeze({
  MISSING_ARGS: 'missing-args',
  CONFIG_MISSING: 'config-missing',
  REGISTER_FAILED: 'register-failed',
  OWNER_MISSING: 'owner-missing',
  PERSIST_FAILED: 'persist-failed',
});

function bridgeUrl() {
  // BASE_URL is validated upstream; multi-slash strip is defense
  // against future config-drift.
  return `${config.BASE_URL.replace(/\/+$/, '')}/webhooks/qurl`;
}

// Fire-and-forget DELETE used by linkGuildWebhookSubscription's
// rollback paths. Intentionally not awaited: the orphan-on-
// qurl-service is itself ephemeral (it 401s every delivery on a
// stale key) and the audit event is the operator-visible signal.
//
// Audit-noise discrimination: 404 is swallowed inside the registrar
// (concurrent-delete race; the goal state is already met). 401 is
// the routine re-key signal — the admin revoked the key on
// layerv.ai before our DELETE landed — so we log but DON'T audit
// (would flood the alarm channel on every key rotation). 5xx and
// network errors DO audit so an orphan-subscription leak is visible.
function bestEffortDeleteSubscription({ apiKey, webhookId, guildId }) {
  deleteSubscription({ apiEndpoint: config.QURL_ENDPOINT, apiKey, webhookId })
    .catch((dErr) => {
      const status = dErr?.status;
      logger.warn('Best-effort orphan-subscription delete threw', {
        error: dErr?.message, status, webhookId, guildId,
      });
      if (status === 401) {
        // Routine re-key — admin revoked the key on layerv.ai before
        // our DELETE landed. Not alarm-worthy, but an orphan webhook
        // is now stranded on qurl-service's account. Log at info so a
        // per-guild history grep can still surface the orphan (without
        // page-loading the audit channel on every key rotation).
        logger.info('Orphan webhook subscription left on qurl-service (401 = key revoked)', {
          webhook_id: webhookId, guild_id: guildId,
        });
        return;
      }
      // Audit any non-401 failure path INCLUDING network errors that
      // surface as `status === undefined` (DNS, TLS, AbortError on
      // timeout). Those previously fell through without an audit,
      // hiding stranded orphans behind a single warn log.
      logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_DELETE_FAILED, {
        guild_id: guildId,
        status: status || null,
        error_type: dErr?.name || 'unknown',
        path: 'rollback',
      });
    });
}

// Audit-emit helper for linkGuildWebhookSubscription's failure
// branches. The MISSING_ARGS branch passes guild_id='<missing>' as a
// sentinel; metric-filter dashboards that group by guild_id will
// surface this as a synthetic group — acceptable since it should
// only fire on caller-bug paths.
function auditLinkFailure(guildId, reason, extra) {
  logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTER_FAILED, {
    guild_id: guildId, reason, ...(extra || {}),
  });
}

// Provision (idempotent) a per-guild webhook subscription.
// `action` mirrors ensureWebhookSubscription: 'created' | 'rotated' | 'reused'.
async function linkGuildWebhookSubscription({ guildId, apiKey, descriptionContext }) {
  if (!guildId || !apiKey) {
    auditLinkFailure(guildId || '<missing>', LINK_RESULTS.MISSING_ARGS);
    return { ok: false, reason: LINK_RESULTS.MISSING_ARGS };
  }
  if (!config.BASE_URL || !config.QURL_ENDPOINT) {
    logger.warn('Per-guild webhook link skipped: BASE_URL or QURL_ENDPOINT unset', { guildId });
    auditLinkFailure(guildId, LINK_RESULTS.CONFIG_MISSING);
    return { ok: false, reason: LINK_RESULTS.CONFIG_MISSING };
  }

  let registered = null;
  try {
    registered = await ensureWebhookSubscription({
      apiEndpoint: config.QURL_ENDPOINT,
      apiKey,
      bridgeUrl: bridgeUrl(),
      description: `Discord bot view counter (guild=${guildId}${descriptionContext ? `, ${descriptionContext}` : ''})`,
    });
  } catch (err) {
    logger.warn('Per-guild webhook subscription registration failed', {
      error: err?.message, guildId,
    });
    auditLinkFailure(guildId, LINK_RESULTS.REGISTER_FAILED);
    return { ok: false, reason: LINK_RESULTS.REGISTER_FAILED };
  }

  const { webhookId, secret, action, ownerId: webhookOwnerId } = registered;
  if (typeof webhookOwnerId !== 'string' || !webhookOwnerId) {
    bestEffortDeleteSubscription({ apiKey, webhookId, guildId });
    logger.warn('Per-guild webhook ownerId missing from registrar response; rolled back', {
      guildId, webhookId, action,
    });
    auditLinkFailure(guildId, LINK_RESULTS.OWNER_MISSING);
    return { ok: false, reason: LINK_RESULTS.OWNER_MISSING };
  }

  try {
    await db.setGuildWebhookSubscription(guildId, {
      webhookId,
      webhookSecret: secret,
      webhookOwnerId,
    });
  } catch (err) {
    bestEffortDeleteSubscription({ apiKey, webhookId, guildId });
    // ConditionalCheckFailedException from
    // setGuildWebhookSubscription's attribute_exists(qurl_api_key)
    // guard means setGuildApiKey wasn't called first (or its row
    // was wiped between the two writes). Treat as hard caller-bug:
    // bail out without touching the cache. The wider rollback (orphan
    // DELETE above) still runs.
    const isOrphanGuard = err?.name === 'ConditionalCheckFailedException';
    logger.warn('Per-guild webhook subscription persisted-create rollback', {
      error: err?.message, guildId, webhookId,
      orphan_guard_tripped: isOrphanGuard,
    });
    auditLinkFailure(guildId, LINK_RESULTS.PERSIST_FAILED);
    return { ok: false, reason: LINK_RESULTS.PERSIST_FAILED };
  }

  // Seed the in-memory cache BEFORE propagation. Propagation runs a
  // full table scan + N parallel sibling updates; a webhook that lands
  // on this replica during that window would otherwise 503/401 until
  // the next 30s tick. Propagation is best-effort by design — the
  // registering replica's correctness shouldn't be gated on it.
  // upsertGuild validates input types; if a future caller-bug feeds
  // in an unexpected shape that the upstream guards missed, we still
  // want the success audit to fire — DDB is authoritative, so a
  // failed in-memory upsert is correctable on the next 30s tick.
  try {
    subs.upsertGuild({ guildId, ownerId: webhookOwnerId, webhookId, webhookSecret: secret });
  } catch (err) {
    logger.warn('subs.upsertGuild rejected (cache will reconcile on next scan)', {
      error: err?.message, guildId,
    });
  }

  // Sibling rows under the same owner may hold a pre-rotate secret.
  // Best-effort propagation; scanOnce's updatedAt tiebreaker still
  // picks the freshly-written primary if this fails. Pass
  // excludeGuildId so the primary row (just written above) isn't
  // updated a second time.
  try {
    const propagateResult = await db.propagateGuildWebhookSubscription(webhookOwnerId, {
      webhookId, webhookSecret: secret, excludeGuildId: guildId,
    });
    if (propagateResult.failed > 0) {
      // Partial success: some sibling rows were updated, others
      // threw. Their cache entries will pick up the stale secret on
      // the next 30s tick and 401 every inbound webhook until
      // another propagate run converges them. Audit so the
      // CloudWatch alarm fires; the registration itself is still OK.
      logger.warn('Per-guild webhook secret propagation partially failed', {
        guildId, webhookOwnerId, webhookId, ...propagateResult,
      });
      logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTER_FAILED, {
        guild_id: guildId,
        reason: 'propagate-partial',
        updated: propagateResult.updated,
        failed: propagateResult.failed,
      });
    }
  } catch (err) {
    logger.warn('Per-guild webhook secret propagation to siblings failed (non-blocking)', {
      error: err?.message, guildId, webhookOwnerId, webhookId,
    });
  }

  logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTERED, {
    guild_id: guildId, action,
  });
  return { ok: true, action };
}

module.exports = {
  linkGuildWebhookSubscription,
  LINK_RESULTS,
};
