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

// Frozen discriminator for the `reason` field on link/unlink results.
// Keeping it as an enum lets downstream audit-log parsers / dashboards
// avoid string-typos and lets the receiver / store tests assert
// against the canonical values.
const LINK_RESULTS = Object.freeze({
  MISSING_ARGS: 'missing-args',
  CONFIG_MISSING: 'config-missing',
  REGISTER_FAILED: 'register-failed',
  OWNER_MISSING: 'owner-missing',
  PERSIST_FAILED: 'persist-failed',
  LIST_FAILED: 'list-failed',
  CANNOT_DELETE: 'cannot-delete',
  KEPT_FOR_SIBLINGS: 'kept-for-siblings',
  DELETED: 'deleted',
  NOT_FOUND: 'not-found',
  READ_FAILED: 'read-failed',
  DELETE_FAILED: 'delete-failed',
  UNLINKED: 'unlinked',
});

function bridgeUrl() {
  // BASE_URL is validated upstream; multi-slash strip is defense
  // against future config-drift.
  return `${config.BASE_URL.replace(/\/+$/, '')}/webhooks/qurl`;
}

// Fire-and-forget DELETE used by the orphan-cleanup paths in
// linkGuildWebhookSubscription. Emits the same DELETE_FAILED audit
// event the non-rollback unlink path uses so a leaked-orphan
// incident shows up on one CloudWatch metric regardless of which
// path produced it.
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

// Centralizes the "warn + audit failure" pattern for the five
// failure branches of linkGuildWebhookSubscription. Without this,
// the branches only logger.warn — CloudWatch metric filters watching
// for register-failure spikes would miss them.
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
  // Best-effort propagation; the scanOnce tiebreaker still picks the
  // freshly-written primary if this fails.
  try {
    await db.propagateGuildWebhookSubscription(webhookOwnerId, {
      webhookId, webhookSecret: secret,
    });
  } catch (err) {
    logger.warn('Per-guild webhook secret propagation to siblings failed (non-blocking)', {
      error: err?.message, guildId, webhookOwnerId, webhookId,
    });
  }

  subs.upsertGuild({ guildId, ownerId: webhookOwnerId, webhookId, webhookSecret: secret });
  logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTERED, {
    guild_id: guildId, action,
  });
  return { ok: true, action };
}

// Reference-counted tear-down for unlink: only DELETE the qurl-service
// subscription if this was the last guild referencing it. 401 / 404 on
// DELETE are swallowed (typical re-key flow has the old key already
// revoked).
async function unlinkGuildWebhookSubscription({ guildId, apiKey, webhookId, webhookOwnerId }) {
  if (!guildId || !webhookOwnerId || !webhookId) {
    return { ok: false, reason: LINK_RESULTS.MISSING_ARGS };
  }

  let siblings = [];
  try {
    siblings = await db.listGuildSubscriptionsByOwner(webhookOwnerId);
  } catch (err) {
    logger.warn('listGuildSubscriptionsByOwner threw — skipping DELETE to avoid killing siblings', {
      error: err?.message, guildId, webhookOwnerId,
    });
    return { ok: false, reason: LINK_RESULTS.LIST_FAILED };
  }
  const otherGuilds = siblings.filter(s => s.guildId !== guildId);
  if (otherGuilds.length > 0) {
    subs.removeGuild({ guildId, ownerId: webhookOwnerId });
    return { ok: true, reason: LINK_RESULTS.KEPT_FOR_SIBLINGS, siblingCount: otherGuilds.length };
  }

  if (!apiKey || !config.QURL_ENDPOINT) {
    logger.warn('Skipping DELETE: missing apiKey or QURL_ENDPOINT', { guildId });
    subs.removeGuild({ guildId, ownerId: webhookOwnerId });
    return { ok: false, reason: LINK_RESULTS.CANNOT_DELETE };
  }
  try {
    await deleteSubscription({
      apiEndpoint: config.QURL_ENDPOINT, apiKey, webhookId,
    });
    logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_DELETED, {
      guild_id: guildId,
    });
  } catch (err) {
    const status = err?.status;
    logger.warn('Per-guild webhook subscription DELETE failed (swallowed)', {
      error: err?.message, status, guildId, webhookId,
    });
    logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_DELETE_FAILED, {
      guild_id: guildId, status: status || null,
    });
  }
  subs.removeGuild({ guildId, ownerId: webhookOwnerId });
  return { ok: true, reason: LINK_RESULTS.DELETED };
}

// One-shot orchestrator for the unlink path. Tears down the
// subscription FIRST (so partial failure leaves observable DDB state
// rather than an unobservable qurl-service orphan), then drops the
// DDB row via the raw delete.
async function unlinkGuildAndWebhook(guildId) {
  if (!guildId) return { ok: false, reason: LINK_RESULTS.MISSING_ARGS };
  let row;
  try {
    row = await db.getGuildConfigWithApiKey(guildId);
  } catch (err) {
    logger.warn('unlinkGuildAndWebhook: failed to load guild row', {
      error: err?.message, guildId,
    });
    return { ok: false, reason: LINK_RESULTS.READ_FAILED };
  }
  if (!row) return { ok: true, reason: LINK_RESULTS.NOT_FOUND };

  if (row.webhook_id && row.webhook_owner_id) {
    await unlinkGuildWebhookSubscription({
      guildId,
      apiKey: row.qurl_api_key,
      webhookId: row.webhook_id,
      webhookOwnerId: row.webhook_owner_id,
    });
  }

  try {
    await db._removeGuildApiKeyRaw(guildId);
  } catch (err) {
    logger.warn('unlinkGuildAndWebhook: _removeGuildApiKeyRaw failed', {
      error: err?.message, guildId,
    });
    return { ok: false, reason: LINK_RESULTS.DELETE_FAILED };
  }
  return { ok: true, reason: LINK_RESULTS.UNLINKED };
}

module.exports = {
  linkGuildWebhookSubscription,
  unlinkGuildWebhookSubscription,
  unlinkGuildAndWebhook,
  LINK_RESULTS,
};
