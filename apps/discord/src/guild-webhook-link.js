// Per-guild qurl-service webhook subscription provisioning.
//
// Called from the two `setGuildApiKey` call sites (qurl-oauth callback +
// /qurl setup admin form) and from the one-time backfill script. The
// caller has already persisted the API key to DDB; this module's job
// is to:
//   1. ensure a qurl.accessed subscription exists under that API key
//   2. discover the qurl-service owner_id the key resolves to
//   3. persist {webhook_id, encrypted secret, owner_id} on the
//      guild_configs row
//   4. update the in-process subscription registry synchronously so
//      the registering replica is immediately correct
//
// Partial-failure: if step 3 throws AFTER step 1 succeeded, the
// just-created subscription is best-effort DELETE'd so the admin's
// account isn't littered with orphan webhooks.
//
// All paths are non-blocking on caller error: the OAuth callback +
// /qurl setup should report success for KEY LINKING even when the
// view-counter wiring fails — the key still works, view counter just
// degrades to the polling fallback until backfill catches up. This
// module logs + audits but never re-throws to the caller.

const config = require('./config');
const db = require('./store');
const logger = require('./logger');
const { AUDIT_EVENTS } = require('./constants');
const { ensureWebhookSubscription, deleteSubscription } = require('./qurl-webhook-registrar');
const subs = require('./webhook-subscriptions');

// 10s matches qurl-oauth.js's QURL_SERVICE_TIMEOUT_MS budget; keep
// it tight so a stuck qurl-service GET doesn't block /qurl setup.
const QURL_SERVICE_TIMEOUT_MS = 10_000;

function bridgeUrl() {
  // BASE_URL is validated upstream (variables.tf regex pins
  // https://host[:port][/segment] with no trailing slash), but we
  // still strip a trailing slash defensively so a future relaxation
  // of that validation can't render `https://bot//webhooks/qurl`.
  return `${config.BASE_URL.replace(/\/$/, '')}/webhooks/qurl`;
}

// Discover the qurl-service owner_id by listing the first webhook
// under this key. Any of the caller's subs work since they all share
// the key's owner — including the one we just created via
// ensureWebhookSubscription. Returns null if the list is empty
// (defensive — shouldn't happen since we just registered one) and
// throws on transport / non-2xx errors so the partial-failure path
// can roll back.
async function discoverOwnerId(apiKey) {
  const resp = await fetch(`${config.QURL_ENDPOINT}/v1/webhooks?limit=1`, {
    method: 'GET',
    headers: { Authorization: `Bearer ${apiKey}` },
    signal: AbortSignal.timeout(QURL_SERVICE_TIMEOUT_MS),
  });
  if (!resp.ok) {
    throw new Error(`GET /v1/webhooks returned ${resp.status} on owner_id discovery`);
  }
  const body = await resp.json();
  const first = Array.isArray(body?.data) ? body.data[0] : null;
  return typeof first?.owner_id === 'string' && first.owner_id.length > 0
    ? first.owner_id
    : null;
}

// Provision (idempotent) a per-guild webhook subscription.
//
// Returns { ok: true, action } on success or { ok: false, reason } on
// any failure. Never throws; the caller's user-facing flow continues
// regardless of outcome.
//
// `action` mirrors ensureWebhookSubscription's: 'created' | 'rotated'
// | 'reused'. Useful for distinguishing "fresh guild" from "admin
// shared their key across N guilds" in audit logs.
async function linkGuildWebhookSubscription({ guildId, apiKey, descriptionContext }) {
  if (!guildId || !apiKey) {
    return { ok: false, reason: 'missing-args' };
  }
  if (!config.BASE_URL || !config.QURL_ENDPOINT) {
    logger.warn('Per-guild webhook link skipped: BASE_URL or QURL_ENDPOINT unset', { guildId });
    return { ok: false, reason: 'config-missing' };
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
    return { ok: false, reason: 'register-failed' };
  }

  const { webhookId, secret, action } = registered;

  let webhookOwnerId;
  try {
    webhookOwnerId = await discoverOwnerId(apiKey);
  } catch (err) {
    // Owner_id discovery failed AFTER we successfully registered.
    // Without owner_id the receiver can't route inbound events to
    // this subscription's secret — best-effort DELETE to avoid an
    // orphan billing-active sub on the admin's account.
    deleteSubscription({ apiEndpoint: config.QURL_ENDPOINT, apiKey, webhookId })
      .catch((dErr) => logger.warn('Best-effort orphan-subscription delete threw', {
        error: dErr?.message, webhookId, guildId,
      }));
    logger.warn('Per-guild webhook owner_id discovery failed; rolled back', {
      error: err?.message, guildId, webhookId,
    });
    return { ok: false, reason: 'owner-discovery-failed' };
  }
  if (!webhookOwnerId) {
    deleteSubscription({ apiEndpoint: config.QURL_ENDPOINT, apiKey, webhookId })
      .catch((dErr) => logger.warn('Best-effort orphan-subscription delete threw', {
        error: dErr?.message, webhookId, guildId,
      }));
    logger.warn('Per-guild webhook owner_id missing on discovery response; rolled back', {
      guildId, webhookId,
    });
    return { ok: false, reason: 'owner-missing' };
  }

  try {
    await db.setGuildWebhookSubscription(guildId, {
      webhookId,
      webhookSecret: secret,
      webhookOwnerId,
    });
  } catch (err) {
    // DDB write failed AFTER subscription was successfully created.
    // Same orphan-cleanup rationale as qurl-oauth.js:413's orphan-
    // mint cleanup. ensureWebhookSubscription's own dedupe heals on
    // the next /qurl setup attempt.
    deleteSubscription({ apiEndpoint: config.QURL_ENDPOINT, apiKey, webhookId })
      .catch((dErr) => logger.warn('Best-effort orphan-subscription delete threw', {
        error: dErr?.message, webhookId, guildId,
      }));
    logger.warn('Per-guild webhook subscription persisted-create rollback', {
      error: err?.message, guildId, webhookId,
    });
    return { ok: false, reason: 'persist-failed' };
  }

  subs.upsertGuild({ guildId, ownerId: webhookOwnerId, webhookId, webhookSecret: secret });
  logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTERED, {
    guild_id: guildId, action,
  });
  return { ok: true, action };
}

// Best-effort tear-down of a guild's webhook subscription, used by
// the unlink path. Reference-counted via listGuildSubscriptionsByOwner
// so multi-guild admins (one auth0 owner across N guilds) don't kill
// sibling delivery when one guild unlinks.
//
// DELETE 401 = the API key the helper would use has been revoked
// already (typical re-key flow); DELETE 404 = subscription already
// gone. Both swallowed; the QURL_WEBHOOK_SUBSCRIPTION_DELETE_FAILED
// audit captures them for forensic counting.
async function unlinkGuildWebhookSubscription({ guildId, apiKey, webhookId, webhookOwnerId }) {
  if (!guildId || !webhookOwnerId || !webhookId) {
    return { ok: false, reason: 'missing-args' };
  }

  let siblings = [];
  try {
    siblings = await db.listGuildSubscriptionsByOwner(webhookOwnerId);
  } catch (err) {
    logger.warn('listGuildSubscriptionsByOwner threw — skipping DELETE to avoid killing siblings', {
      error: err?.message, guildId, webhookOwnerId,
    });
    return { ok: false, reason: 'list-failed' };
  }
  const otherGuilds = siblings.filter(s => s.guildId !== guildId);
  if (otherGuilds.length > 0) {
    // Other guilds share this subscription; leave it alone. Local
    // registry update only — let the next tick observe the row drop.
    subs.removeGuild({ guildId, ownerId: webhookOwnerId });
    return { ok: true, reason: 'kept-for-siblings', siblingCount: otherGuilds.length };
  }

  // Last guild for this subscription. Best-effort DELETE.
  if (!apiKey || !config.QURL_ENDPOINT) {
    logger.warn('Skipping DELETE: missing apiKey or QURL_ENDPOINT', { guildId });
    subs.removeGuild({ guildId, ownerId: webhookOwnerId });
    return { ok: false, reason: 'cannot-delete' };
  }
  try {
    await deleteSubscription({
      apiEndpoint: config.QURL_ENDPOINT, apiKey, webhookId,
    });
    logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_DELETED, {
      guild_id: guildId,
    });
  } catch (err) {
    // 401 = key revoked (typical re-key flow); 404 should already be
    // swallowed inside deleteSubscription itself (see qurl-webhook-
    // registrar.js:192). Anything else is an unexpected qurl-service
    // failure mode; log + audit but DON'T re-throw, since the caller
    // (removeGuildApiKey) is in a critical unlink path.
    const status = err?.status;
    logger.warn('Per-guild webhook subscription DELETE failed (swallowed)', {
      error: err?.message, status, guildId, webhookId,
    });
    logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_DELETE_FAILED, {
      guild_id: guildId, status: status || null,
    });
  }
  subs.removeGuild({ guildId, ownerId: webhookOwnerId });
  return { ok: true, reason: 'deleted' };
}

// One-shot orchestrator for the unlink path: load the guild's current
// state, tear down the qurl-service subscription (ref-counted across
// siblings), then delete the DDB row. Future callers that today don't
// exist (an `/qurl unlink` admin command, an OAuth revoke handler)
// MUST use this entry point rather than calling db.removeGuildApiKey
// directly — otherwise the webhook subscription orphans on
// qurl-service and the bot leaves an unprotected secret-less row
// behind. See removeGuildApiKey in ddb-store.js for the comment that
// points future authors here.
async function unlinkGuildAndWebhook(guildId) {
  if (!guildId) return { ok: false, reason: 'missing-args' };
  let row;
  try {
    row = await db.getGuildConfigWithApiKey(guildId);
  } catch (err) {
    logger.warn('unlinkGuildAndWebhook: failed to load guild row', {
      error: err?.message, guildId,
    });
    return { ok: false, reason: 'read-failed' };
  }
  if (!row) return { ok: true, reason: 'not-found' };

  // Tear down the subscription FIRST so a partial-failure leaves
  // observable orphan state (a DDB row with stale webhook_id) rather
  // than an unobservable orphan (a row deleted but the webhook still
  // billing-active on qurl-service). Best-effort throughout.
  if (row.webhook_id && row.webhook_owner_id) {
    await unlinkGuildWebhookSubscription({
      guildId,
      apiKey: row.qurl_api_key,
      webhookId: row.webhook_id,
      webhookOwnerId: row.webhook_owner_id,
    });
  }

  try {
    await db.removeGuildApiKey(guildId);
  } catch (err) {
    logger.warn('unlinkGuildAndWebhook: removeGuildApiKey failed', {
      error: err?.message, guildId,
    });
    return { ok: false, reason: 'delete-failed' };
  }
  return { ok: true, reason: 'unlinked' };
}

module.exports = {
  linkGuildWebhookSubscription,
  unlinkGuildWebhookSubscription,
  unlinkGuildAndWebhook,
};
