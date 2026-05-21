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
// A leaked orphan converges via the future orphan-sweeper noted
// in PR #485's Out-of-scope section.
function bestEffortDeleteSubscription({ apiKey, webhookId, guildId }) {
  deleteSubscription({ apiEndpoint: config.QURL_ENDPOINT, apiKey, webhookId })
    .catch((dErr) => {
      logger.warn('Best-effort orphan-subscription delete threw', {
        error: dErr?.message, webhookId, guildId,
      });
      logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_DELETE_FAILED, {
        guild_id: guildId, status: dErr?.status || null, path: 'rollback',
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
    logger.warn('Per-guild webhook subscription persisted-create rollback', {
      error: err?.message, guildId, webhookId,
    });
    auditLinkFailure(guildId, LINK_RESULTS.PERSIST_FAILED);
    return { ok: false, reason: LINK_RESULTS.PERSIST_FAILED };
  }

  // Sibling rows under the same owner may hold a pre-rotate secret.
  // Best-effort propagation; scanOnce's updatedAt tiebreaker still
  // picks the freshly-written primary if this fails. Pass
  // excludeGuildId so the primary row (just written above) isn't
  // updated a second time.
  try {
    await db.propagateGuildWebhookSubscription(webhookOwnerId, {
      webhookId, webhookSecret: secret, excludeGuildId: guildId,
    });
  } catch (err) {
    logger.warn('Per-guild webhook secret propagation to siblings failed (non-blocking)', {
      error: err?.message, guildId, webhookOwnerId, webhookId,
    });
  }

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
  logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTERED, {
    guild_id: guildId, action,
  });
  return { ok: true, action };
}

module.exports = {
  linkGuildWebhookSubscription,
  LINK_RESULTS,
};
