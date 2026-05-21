// Per-guild qurl-service webhook subscription registry.
//
// Process-local Map<owner_id, { guildIds: Set<string>, webhookSecret, webhookId }>
// keyed by the qurl-service auth0 owner_id that authorizes the
// subscription. Populated from `guild_configs` at boot and refreshed
// every 30s; the registering replica updates its own map synchronously
// inside the link/unlink flow so the user who just ran /qurl setup
// doesn't race their own first view.
//
// Sibling replicas catch up on the next tick. qurl-service's retry
// policy is 1+2+4+8+16=31s exponential backoff with a 30s worker tick
// (≈60s total retry window per `webhook_service.go::scheduleRetryIfNeeded`),
// so a 30s refresh stays inside it — a first-view event that lands on
// a stale replica is retried after this replica has refreshed.
//
// Receiver semantics (in routes/qurl-webhook.js):
//   - Before isPrimed(): unknown owner → 503 (retriable). Cold-start
//     and any-DDB-flake case.
//   - After isPrimed():  unknown owner → 401 (truthful).
// qurl-service does NOT retry 401, so getting this split right is
// load-bearing for don't-drop-views-during-deploy.

const db = require('./store');
const config = require('./config');
const logger = require('./logger');
const { AUDIT_EVENTS } = require('./constants');

const REFRESH_INTERVAL_MS = 30_000;
// After this many consecutive refresh failures we escalate via audit
// so CloudWatch metric-filter alarms can page. We don't crash —
// receiver responds 503 until refresh succeeds, qurl-service retries.
const REFRESH_FAIL_ESCALATE_AT = 3;

// Single-process state. Module-level (not a class) for parity with
// orphan-token-sweeper.js's pattern and so tests can import a fresh
// copy via jest.isolateModules().
const subscriptions = new Map();
let primed = false;
let consecutiveFailures = 0;
let timer = null;

// The default-key entry lives in the same map under whatever owner_id
// the bot's QURL_API_KEY resolves to. We don't know it at boot — we
// discover it by listing the bot's own webhooks (the Lambda registered
// at least one) on every refresh tick. Storing the discovered owner_id
// here keeps the discover/refresh fold idempotent.
let defaultOwnerId = null;

// Owners whose entries must survive scanOnce's clear-and-repopulate
// swap. .clear()'d at the top of each scan; re-applied after the
// swap so a concurrent /qurl setup whose DDB write landed AFTER the
// scan crossed its partition isn't silently dropped.
const upsertsDuringScan = new Set();

// Sentinel for the default-key entry's webhookId field. A unique
// Symbol can never collide with a real qurl-service webhook_id
// (those are opaque strings). The receiver only reads webhookSecret
// from the entry; the field is for debugging.
const DEFAULT_KEY_SENTINEL = Symbol('default-key-subscription');

function getSecretForOwner(ownerId) {
  if (!ownerId) return null;
  const entry = subscriptions.get(ownerId);
  return entry ? entry.webhookSecret : null;
}

function isPrimed() {
  return primed;
}

// Synchronous local-map mutation called by setGuildApiKey-adjacent
// flows AFTER they've successfully written to DDB. The registering
// replica gets immediate consistency; sibling replicas converge on
// next tick. Idempotent: same (guildId, ownerId) re-call just re-adds.
function upsertGuild({ guildId, ownerId, webhookId, webhookSecret }) {
  if (!guildId || !ownerId || !webhookId || !webhookSecret) {
    throw new Error('upsertGuild: guildId, ownerId, webhookId, webhookSecret all required');
  }
  let entry = subscriptions.get(ownerId);
  if (!entry) {
    entry = { guildIds: new Set(), webhookSecret, webhookId };
    subscriptions.set(ownerId, entry);
  } else {
    // Last-write-wins. A second guild's link triggers qurl-service to
    // rotate the shared secret; the newer value must replace the
    // older one for the receiver to match qurl-service's signatures.
    entry.webhookSecret = webhookSecret;
    entry.webhookId = webhookId;
  }
  entry.guildIds.add(guildId);
  upsertsDuringScan.add(ownerId);
}

// Removes guildId from its owner's entry; if the entry is now empty
// (no sibling guilds + not the default-key owner), drops it. Default-
// key entry is never dropped from removeGuild — it's owned by the
// Lambda lifecycle and rediscovered by the tick.
function removeGuild({ guildId, ownerId }) {
  if (!ownerId) return;
  const entry = subscriptions.get(ownerId);
  if (!entry) return;
  entry.guildIds.delete(guildId);
  if (entry.guildIds.size === 0 && ownerId !== defaultOwnerId) {
    subscriptions.delete(ownerId);
  }
}

// Fetches the default key's owner_id by listing its own webhooks.
// Returns null if the list is empty (Lambda hasn't run yet on a
// fresh deploy) — the tick will retry next cycle, and inbound events
// in the gap are 503'd via the unprimed-cache code path. Network /
// HTTP errors are surfaced to the caller (scanOnce) so the tick's
// failure counter increments correctly.
async function discoverDefaultOwnerId() {
  if (!config.QURL_API_KEY || !config.QURL_ENDPOINT) return null;
  const resp = await fetch(`${config.QURL_ENDPOINT}/v1/webhooks?limit=1`, {
    method: 'GET',
    headers: { Authorization: `Bearer ${config.QURL_API_KEY}` },
    signal: AbortSignal.timeout(10_000),
  });
  if (!resp.ok) {
    throw new Error(`discoverDefaultOwnerId: GET /v1/webhooks returned ${resp.status}`);
  }
  const body = await resp.json();
  const subs = Array.isArray(body?.data) ? body.data : [];
  // owner_id is the same across every subscription this key owns;
  // any row works. Subs without owner_id (defensive against contract
  // drift) are skipped.
  for (const s of subs) {
    if (typeof s?.owner_id === 'string' && s.owner_id.length > 0) return s.owner_id;
  }
  return null;
}

// One refresh pass. Rebuilds the per-owner map from `guild_configs`
// and rediscovers the default-key owner_id. Throws on any sub-step
// failure; the caller (refreshTick) increments consecutiveFailures.
async function scanOnce() {
  upsertsDuringScan.clear();

  // Parallelize the two independent network reads: the DDB scan and
  // the qurl-service GET /v1/webhooks. Sequential, they added 50-300ms
  // per tick for no benefit.
  // TODO: GSI on webhook_owner_id when guild_configs row count
  // exceeds ~10k — scanAll inside scanGuildSubscriptions and
  // listGuildSubscriptionsByOwner is bounded by table size, not
  // result size.
  //
  // discoverDefaultOwnerId fires only until success — the bot's own
  // owner_id never changes without a redeploy, so re-fetching every
  // 30s after success is wasted load on qurl-service.
  const needsDefaultDiscovery = !defaultOwnerId || !config.QURL_WEBHOOK_SECRET;
  const [rows, discoveredOwner] = await Promise.all([
    db.scanGuildSubscriptions(),
    needsDefaultDiscovery ? discoverDefaultOwnerId() : Promise.resolve(defaultOwnerId),
  ]);

  // Tiebreaker: when sibling rows for one owner disagree on (secret,
  // webhookId) — the propagateGuildWebhookSubscription window — the
  // row with the newest `updatedAt` wins. The chosen-updatedAt is a
  // scan-local concern; we keep it in a parallel Map instead of
  // attaching to the cache entry shape that the receiver consults.
  const next = new Map();
  const winningUpdatedAt = new Map();
  for (const r of rows) {
    let entry = next.get(r.webhookOwnerId);
    if (!entry) {
      entry = { guildIds: new Set(), webhookSecret: r.webhookSecret, webhookId: r.webhookId };
      next.set(r.webhookOwnerId, entry);
      winningUpdatedAt.set(r.webhookOwnerId, r.updatedAt || '');
    } else if ((r.updatedAt || '') > winningUpdatedAt.get(r.webhookOwnerId)) {
      entry.webhookSecret = r.webhookSecret;
      entry.webhookId = r.webhookId;
      winningUpdatedAt.set(r.webhookOwnerId, r.updatedAt || '');
    }
    entry.guildIds.add(r.guildId);
  }

  if (discoveredOwner) {
    defaultOwnerId = discoveredOwner;
    if (config.QURL_WEBHOOK_SECRET) {
      // Receiver only reads webhookSecret; the Symbol sentinel is
      // for debugging and can never collide with a real opaque
      // webhook_id string from qurl-service.
      next.set(discoveredOwner, {
        guildIds: new Set([DEFAULT_KEY_SENTINEL]),
        webhookSecret: config.QURL_WEBHOOK_SECRET,
        webhookId: DEFAULT_KEY_SENTINEL,
      });
    }
  } else {
    // Default-key owner_id couldn't be discovered (QURL_API_KEY or
    // QURL_ENDPOINT unset, or the Lambda hasn't run yet on a fresh
    // deploy). Inbound webhooks for non-BYOK guilds will 401 until
    // discovery succeeds. Warn so a CloudWatch metric filter on this
    // line can alert on a fresh-deploy gap that outlasts the Lambda.
    logger.warn('webhook-subscriptions: default-key owner_id discovery returned null', {
      hasApiKey: Boolean(config.QURL_API_KEY),
      hasEndpoint: Boolean(config.QURL_ENDPOINT),
      hasWebhookSecret: Boolean(config.QURL_WEBHOOK_SECRET),
    });
  }

  // Only snapshot when concurrent upserts could have landed. The
  // common case (no concurrent /qurl setup) skips the allocation.
  const liveSnapshot = upsertsDuringScan.size > 0 ? new Map(subscriptions) : null;
  subscriptions.clear();
  for (const [k, v] of next.entries()) subscriptions.set(k, v);

  if (liveSnapshot) {
    for (const ownerId of upsertsDuringScan) {
      if (next.has(ownerId)) continue;
      const liveEntry = liveSnapshot.get(ownerId);
      if (liveEntry) subscriptions.set(ownerId, liveEntry);
    }
  }

  primed = true;
}

async function refreshTick() {
  try {
    await scanOnce();
    consecutiveFailures = 0;
  } catch (err) {
    consecutiveFailures += 1;
    const level = consecutiveFailures >= REFRESH_FAIL_ESCALATE_AT ? 'error' : 'warn';
    logger[level]('webhook subscription registry refresh failed', {
      error: err.message,
      consecutiveFailures,
    });
    if (consecutiveFailures === REFRESH_FAIL_ESCALATE_AT) {
      // Audit emitted exactly once at the escalation threshold so a
      // CloudWatch metric-filter alarm fires once per outage burst,
      // not on every subsequent failed tick. Reset on success above.
      logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_CACHE_REFRESH_FAIL, {
        consecutive_failures: consecutiveFailures,
      });
    }
  }
}

// Start the registry: an immediate scan plus a 30s ticker. Returns a
// Promise that resolves after the initial scan attempt — caller can
// `await start()` to block boot on cache-primed if it wants, or
// fire-and-forget for non-blocking startup. We do not throw on first-
// scan failure; the receiver's 503 path handles the unprimed gap and
// the ticker retries.
async function start() {
  if (timer) return; // idempotent
  await refreshTick();
  timer = setInterval(() => {
    refreshTick().catch(err => logger.error('webhook subscription registry refresh crash', { error: err.message }));
  }, REFRESH_INTERVAL_MS);
  timer.unref();
}

function stop() {
  if (timer) {
    clearInterval(timer);
    timer = null;
  }
}

// Test-only: reset module-level state. Production code must never
// reach for this — the leading-underscore signals intent. Mirrors
// the ddb-store.js pattern for _TABLES_FOR_TESTING.
function _resetForTesting() {
  subscriptions.clear();
  primed = false;
  consecutiveFailures = 0;
  defaultOwnerId = null;
  upsertsDuringScan.clear();
  if (timer) {
    clearInterval(timer);
    timer = null;
  }
}

module.exports = {
  start,
  stop,
  getSecretForOwner,
  isPrimed,
  upsertGuild,
  removeGuild,
  // Test-only: production callers should let the 30s ticker drive
  // refresh. Exposed because the test suite needs to drive scans
  // deterministically without waiting on real intervals.
  scanOnce,
  _resetForTesting,
};
