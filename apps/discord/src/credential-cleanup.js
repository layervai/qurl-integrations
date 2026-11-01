const config = require('./config');
const { AUDIT_EVENTS } = require('./constants');
const db = require('./store');
const logger = require('./logger');
const { decrypt } = require('./utils/crypto');
const { getBotGuildInstallation } = require('./discord-rest');
const { getIdentity } = require('./qurl');
const { callQurlService, deleteSubscription } = require('./qurl-webhook-registrar');
const subscriptions = require('./webhook-subscriptions');

let timer;
let running;
let nextBucket = 0;

async function processJob(original) {
  let job = original;
  if (job.attempts > 6) return db.finishCredentialCleanup(job, 'manual', 'retry_limit');
  if (job.reason === 'verify_removal') {
    const row = await db.getGuildCleanupSnapshot(job.guild_id);
    if (!row?.qurl_api_key) return db.finishCredentialCleanup(job, 'complete', 'no_credential');
    if (job.expected_installation_joined_at
        && row.qurl_installation_joined_at !== job.expected_installation_joined_at) {
      return db.finishCredentialCleanup(job, 'complete', 'new_installation');
    }
    const membership = await getBotGuildInstallation(job.guild_id);
    if (membership.ok && membership.joinedAt === row.qurl_installation_joined_at) {
      await db.restoreGuildInstallation(job, row);
      return db.finishCredentialCleanup(job, 'complete', 'still_installed');
    }
    const absent = membership.status === 404 && [10004, 10007].includes(membership.code);
    if (!membership.ok && !absent) return db.finishCredentialCleanup(job, 'retry', 'membership_unknown');
    // Snapshot and retained capability move together; a concurrent setup wins
    // through the generation/ciphertext condition instead of losing its new key.
    const cacheRevision = subscriptions.getMutationRevision();
    job = await db.retireGuildCredential(job, row);
    if (!job) return db.finishCredentialCleanup(original, 'retry', 'credential_changed');
    const current = await db.getGuildCleanupSnapshot(job.guild_id);
    if (!current?.qurl_api_key) subscriptions.removeGuild({ guildId: job.guild_id, ownerId: row.webhook_owner_id, expectedRevision: cacheRevision });
  }
  const apiKey = job.qurl_api_key ? decrypt(job.qurl_api_key) : null;
  if (!apiKey || apiKey === config.QURL_API_KEY) {
    return db.finishCredentialCleanup(job, 'manual', 'protected_or_missing_capability');
  }
  // Never revoke a key that a concurrent setup kept for this guild.
  const current = await db.getGuildCleanupSnapshot(job.guild_id);
  const keyInUse = current?.qurl_api_key && decrypt(current.qurl_api_key) === apiKey;
  if (job.webhook_id) {
    // A retained owner binding was verified when this exact key was saved.
    // Avoid GET /me when that proof exists: frozen accounts can still self-revoke.
    const ownerId = job.qurl_owner_id && job.qurl_owner_id === job.webhook_owner_id
      ? job.qurl_owner_id : (await getIdentity(apiKey, job.guild_id)).owner_id;
    if (job.webhook_owner_id && job.webhook_owner_id !== ownerId) {
      return db.finishCredentialCleanup(job, 'manual', 'owner_mismatch');
    }
    // Older rollback facts can lack an owner. Resolve it through the retained
    // key before addressing a head; never infer it from the guild's new owner.
    job = { ...job, webhook_owner_id: ownerId };
    const claim = await db.claimWebhookCleanup(job, config.CREDENTIAL_CLEANUP_SCOPE);
    if (!claim) {
      return db.finishCredentialCleanup(job, 'retry', 'references_or_migration_pending');
    }
    if (claim === true) {
      await deleteSubscription({ apiEndpoint: config.QURL_ENDPOINT.replace(/\/+$/, ''), apiKey, webhookId: job.webhook_id });
      await db.completeWebhookCleanup(job);
    }
  }
  if (job.revoke_key !== false && !keyInUse
      && (!['shared', 'dedicated'].includes(job.qurl_key_kind)
        || (job.qurl_key_kind === 'dedicated' && !job.qurl_key_id))) {
    return db.finishCredentialCleanup(job, 'manual', 'key_provenance_unknown');
  }
  if (job.revoke_key !== false && job.qurl_key_kind === 'dedicated' && job.qurl_key_id && !keyInUse) {
    // This narrow route accepts the same dedicated key under frozen-account
    // policy. A cold 401 is uncertain and follows the retry/manual path.
    await callQurlService({ method: 'DELETE', path: '/v1/me/api-key',
      apiEndpoint: config.QURL_ENDPOINT.replace(/\/+$/, ''), apiKey, expectedStatus: 204 });
  }
  return db.finishCredentialCleanup(job, 'complete', keyInUse ? 'active_key_preserved' : 'cleanup_confirmed');
}

async function tick() {
  if (running) return running;
  running = (async () => {
    // One bounded page per shard and tick; replicas share atomic claims.
    const buckets = Array.from({ length: 4 }, () => nextBucket++ % 16);
    await Promise.all(buckets.map(async bucket => {
      let job;
      try {
        job = await db.claimCredentialCleanup(bucket);
        if (!job) return;
        const outcome = await processJob(job);
        if (outcome === 'manual') logger.audit(AUDIT_EVENTS.CREDENTIAL_CLEANUP_FAILED, { job_id: job.job_id, reason: 'manual' });
      } catch {
        if (!job) { logger.audit(AUDIT_EVENTS.CREDENTIAL_CLEANUP_FAILED, { reason: 'claim_failed' }); return; }
        try {
          const state = await db.finishCredentialCleanup(job, 'retry', 'cleanup_unconfirmed');
          if (state === 'manual') logger.audit(AUDIT_EVENTS.CREDENTIAL_CLEANUP_FAILED, { job_id: job.job_id, reason: 'cleanup_unconfirmed' });
        } catch { logger.audit(AUDIT_EVENTS.CREDENTIAL_CLEANUP_FAILED, { job_id: job.job_id, reason: 'outcome_not_saved' }); }
      }
    }));
  })().finally(() => { running = null; });
  return running;
}
function start() {
  if (!config.ENABLE_CREDENTIAL_CLEANUP_WORKER || timer) return;
  if (!config.ENABLE_CREDENTIAL_LIFECYCLE || !config.CREDENTIAL_CLEANUP_SCOPE) {
    throw new Error('Credential cleanup requires lifecycle writers and an attested fleet scope');
  }
  timer = setInterval(tick, 15_000);
  timer.unref();
  void tick();
}
async function stop() { clearInterval(timer); timer = null; await running; }
module.exports = { start, stop, tick, processJob };
