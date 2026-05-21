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
    // ensureWebhookSubscription dedupes on (owner_id, url); two
    // guilds sharing an owner get the SAME webhookId + secret. If a
    // future qurl-service change generates a new secret on re-link
    // for the same owner, take the newer value — entries are
    // last-write-wins by design.
    entry.webhookSecret = webhookSecret;
    entry.webhookId = webhookId;
  }
  entry.guildIds.add(guildId);
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

// One refresh pass. Rebuilds the per-guild map from `guild_configs`
// AND rediscovers the default-key owner_id (idempotent). Throws on
// any sub-step failure so the caller increments the failure counter
// — partial-pass state would mask drift.
async function scanOnce() {
  const rows = await db.scanGuildSubscriptions();

  // Rebuild rather than diff-apply: tiny table (low hundreds at
  // scale), simpler than reconciling adds/drops, and any synchronous
  // upsertGuild calls between scans are preserved because we rebuild
  // the map before swapping.
  const next = new Map();
  for (const r of rows) {
    let entry = next.get(r.webhookOwnerId);
    if (!entry) {
      entry = { guildIds: new Set(), webhookSecret: r.webhookSecret, webhookId: r.webhookId };
      next.set(r.webhookOwnerId, entry);
    }
    entry.guildIds.add(r.guildId);
  }

  const discoveredOwner = await discoverDefaultOwnerId();
  if (discoveredOwner) {
    defaultOwnerId = discoveredOwner;
    if (config.QURL_WEBHOOK_SECRET) {
      // We don't know the webhookId for the default-key sub from
      // GET /v1/webhooks?limit=1 in a stable way — but the receiver
      // only needs webhookSecret. Synthetic webhookId acts as a
      // marker for the default entry; never sent over the wire.
      next.set(discoveredOwner, {
        guildIds: new Set(['__default__']),
        webhookSecret: config.QURL_WEBHOOK_SECRET,
        webhookId: '__default__',
      });
    }
  }

  // Atomic-ish swap. Clear-and-repopulate so any caller holding a
  // reference to `subscriptions` sees a consistent post-swap state
  // (Map.set is synchronous; no caller can observe a half-built map
  // between iterations in single-threaded Node).
  subscriptions.clear();
  for (const [k, v] of next.entries()) subscriptions.set(k, v);
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
  // Exposed for the splice points in qurl-oauth.js + commands.js so
  // they can do a synchronous local update after writing to DDB. NOT
  // exposed as part of the receiver's hot path.
  scanOnce,
  _resetForTesting,
};
