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
const { callQurlService } = require('./qurl-webhook-registrar');

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
// Discovery-only failure counter. Tracked separately from
// consecutiveFailures so a transient qurl-service 5xx during
// discoverDefaultOwnerId doesn't take down BYOK delivery (the DDB
// rows are independent). Receiver still 401s any default-key
// webhooks until discovery succeeds, but BYOK guilds keep flowing.
let discoveryConsecutiveFailures = 0;
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

// Re-entrancy guard. setInterval doesn't await the previous tick, so
// if scanGuildSubscriptions takes longer than REFRESH_INTERVAL_MS,
// the next tick would fire while the current one is awaiting — both
// would .clear() upsertsDuringScan and the second would wipe the
// first's in-flight tracking, defeating the concurrent-upsert
// preservation. Skipping the overlapping tick is safe: the receiver's
// 503-unprimed path covers any extra delay.
let scanInFlight = false;

// Timestamp (ms) of the most recent successful scan. The receiver
// reads this to decide whether an OWNER_UNKNOWN looks like "sibling
// replica caught a fresh-link gap" (recent enough to be a still-
// converging eventual-consistency state — return 503 so qurl-service
// retries) or "owner is genuinely absent" (we've had time to refresh
// at least twice — return 401).
let lastScanCompletedAt = 0;
// Tolerance past one full refresh interval. Accounts for clock drift,
// DDB cross-AZ replication delay, and scan execution time. After
// REFRESH_INTERVAL_MS + grace, an OWNER_UNKNOWN is treated as a real
// 401 — we've had two scan chances to see the owner.
const SIBLING_LAG_GRACE_MS = 5_000;

// Sentinel for the default-key entry's webhookId and guildIds.
// Double-underscore-bracketed string can't collide with a real
// qurl-service webhook_id (those are `wh_...`-prefixed opaque base64-
// ish strings, never bracketed underscores) or a Discord guild_id
// (those are decimal-only snowflakes). JSON-serializable so a future
// debug-dump endpoint or telemetry that walks the cache won't surface
// `undefined` for the default entry — the choice cost ~zero vs Symbol
// here since the receiver never compares webhookId for equality.
const DEFAULT_KEY_SENTINEL = '__default-key__';

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
  // Type-strict: a caller-bug passing `ownerId: 0` or `ownerId: {}`
  // would pass a truthy-only check for some shapes and then break
  // downstream Map.get(ownerId) lookups via strict equality.
  if (typeof guildId !== 'string' || !guildId.length
      || typeof ownerId !== 'string' || !ownerId.length
      || (typeof webhookId !== 'string' || !webhookId.length)
      || (typeof webhookSecret !== 'string' || !webhookSecret.length)) {
    throw new Error('upsertGuild: guildId, ownerId, webhookId, webhookSecret must all be non-empty strings');
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
//
// No production caller exists today; the future /qurl unlink admin
// command MUST call this synchronously after writing to DDB so the
// registering replica's cache is immediately correct (mirror of
// upsertGuild's sync-update pattern).
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
  // limit=100 (was limit=10): owner_id is identical across all subs
  // this key owns, so we only need one valid row. Bumped to 100 so
  // a contract drift that drops owner_id from rows 1..N silently
  // doesn't fail discovery — a bot key with 50+ subs is plausible
  // at scale; 100 stays under the typical page-size cap with no
  // pagination needed for the read-side.
  // Go through callQurlService for consistency with the rest of the
  // registrar surface — same QurlServiceError shape, same op-tagged
  // network-error handling, same 10s timeout default. The receiver
  // shouldn't grow a second bespoke fetch path.
  const body = await callQurlService({
    method: 'GET',
    path: '/v1/webhooks?limit=100',
    apiEndpoint: config.QURL_ENDPOINT,
    apiKey: config.QURL_API_KEY,
  });
  // Local name `webhooks` (not `subs`) so it doesn't shadow the
  // module convention everyone else uses for the registry import.
  const webhooks = Array.isArray(body?.data) ? body.data : [];
  for (const w of webhooks) {
    if (typeof w?.owner_id === 'string' && w.owner_id.length > 0) return w.owner_id;
  }
  return null;
}

// One refresh pass. Rebuilds the per-owner map from `guild_configs`
// and rediscovers the default-key owner_id. Throws on any sub-step
// failure; the caller (refreshTick) increments consecutiveFailures.
async function scanOnce() {
  if (scanInFlight) {
    // Drop the overlap rather than corrupt upsertsDuringScan tracking
    // (see comment on the guard above). Return a sentinel so the
    // caller can distinguish "completed a real refresh" from "no-op'd
    // because another was in flight" — refreshTick uses this to
    // avoid resetting consecutiveFailures during a long-running
    // outage (a slow scan that overlaps the next tick).
    return 'skipped';
  }
  scanInFlight = true;
  try {
    // INVARIANT: only upserts that land between this .clear() and the
    // snapshot read below count as "concurrent with this scan." The
    // clear MUST happen before any await — JS's run-to-completion
    // semantics mean every upsertGuild call up to here landed in this
    // synchronous block (and is now zeroed); calls after the next
    // await yield are what we need to track. A future async-ifying
    // refactor that introduced an await between .clear() and the
    // first network read would defeat this — the gap could swallow
    // an upsertGuild that ran during the await, leaving its row in
    // the cleared set but not re-applied via the liveSnapshot path.
    // Between scans the set accumulates harmless noise (all entries
    // are post-prior-scan synchronous upserts); the .clear is what
    // re-zeros the in-scan tracker.
    upsertsDuringScan.clear();

    // TODO(#486): GSI on webhook_owner_id when guild_configs row
    // count exceeds ~10k — scanAll is bounded by table size, not
    // result size. When the GSI lands, the priming path can also drop
    // to eventually-consistent reads (SIBLING_LAG_GRACE_MS already
    // absorbs replication lag); strong consistency pays for itself in
    // propagateGuildWebhookSubscription, not here.
    //
    // discoverDefaultOwnerId only fires while there's something to
    // discover: the bot's own owner_id never changes in-process (env
    // is read at boot), so once we have it AND a secret to pair with,
    // the GET is wasted load on qurl-service.
    //
    // Discovery is fired in parallel with the DDB scan for latency,
    // BUT its failure is caught locally instead of via Promise.all so
    // a transient qurl-service 5xx during boot doesn't 503 every BYOK
    // guild for 30s. The default-key entry stays absent (next tick
    // retries); BYOK guilds resolve from the DDB rows normally.
    const needsDefaultDiscovery = !!config.QURL_WEBHOOK_SECRET && !defaultOwnerId;
    const discoveryPromise = needsDefaultDiscovery
      ? discoverDefaultOwnerId().then(
        (owner) => ({ ok: true, owner }),
        (err) => ({ ok: false, error: err }),
      )
      : Promise.resolve({ ok: true, owner: defaultOwnerId });
    const [rows, discoveryResult] = await Promise.all([
      db.scanGuildSubscriptions(),
      discoveryPromise,
    ]);
    let discoveredOwner;
    if (discoveryResult.ok) {
      discoveredOwner = discoveryResult.owner;
      // Reset on success — independent of consecutiveFailures so a
      // recovered discovery doesn't reset the DDB-scan counter.
      discoveryConsecutiveFailures = 0;
    } else {
      discoveredOwner = defaultOwnerId; // keep prior value (likely null pre-first-success)
      discoveryConsecutiveFailures += 1;
      const level = discoveryConsecutiveFailures >= REFRESH_FAIL_ESCALATE_AT ? 'error' : 'warn';
      logger[level]('default-key discovery failed (BYOK path unaffected)', {
        error: discoveryResult.error.message,
        consecutiveFailures: discoveryConsecutiveFailures,
      });
      if (discoveryConsecutiveFailures === REFRESH_FAIL_ESCALATE_AT) {
        // Alarm-once at threshold so CloudWatch metric filters fire
        // a single page per outage burst, not on every subsequent
        // failed tick. Reset on success above.
        logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_DEFAULT_DISCOVERY_FAIL, {
          consecutive_failures: discoveryConsecutiveFailures,
        });
      }
    }

    // Tiebreaker: when sibling rows for one owner disagree on
    // (secret, webhookId) — the propagateGuildWebhookSubscription
    // window — the row with the newest updatedAt wins. winningUpdatedAt
    // is a scan-local Map so the visible cache entry shape stays
    // { guildIds, webhookSecret, webhookId }.
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
      // Collision case: a BYOK guild that linked using the bot's OWN
      // default API key has webhook_owner_id === discoveredOwner. The
      // BYOK row already populated `next`; don't clobber its guildIds
      // / webhookId with the synthetic default-key shape. The
      // secret is identical in this case (both came from the same
      // qurl-service subscription), so leaving the BYOK entry in
      // place is observationally correct.
      if (config.QURL_WEBHOOK_SECRET && !next.has(discoveredOwner)) {
        // String sentinel — receiver only reads webhookSecret, never
        // compares webhookId. Bracketed underscores can't collide
        // with `wh_...` qurl-service IDs or decimal Discord
        // snowflakes (the only legal guildIds values).
        next.set(discoveredOwner, {
          guildIds: new Set([DEFAULT_KEY_SENTINEL]),
          webhookSecret: config.QURL_WEBHOOK_SECRET,
          webhookId: DEFAULT_KEY_SENTINEL,
        });
      }
    } else if (config.QURL_WEBHOOK_SECRET) {
      // Only warn when we WANTED a default entry (secret is set) but
      // discovery failed — that's the alarm-worthy case (fresh-deploy
      // gap that outlasts the Lambda, qurl-service drift, etc.). For
      // pure-BYOK setups (no default secret), discovery is
      // intentionally skipped earlier and a null result here would
      // just be the no-op path; warning every 30s in that
      // configuration would dilute the alarm signal-to-noise.
      logger.warn('webhook-subscriptions: default-key owner_id discovery returned null', {
        hasApiKey: Boolean(config.QURL_API_KEY),
        hasEndpoint: Boolean(config.QURL_ENDPOINT),
      });
    }

    // Only snapshot when concurrent upserts could have landed.
    const liveSnapshot = upsertsDuringScan.size > 0 ? new Map(subscriptions) : null;
    subscriptions.clear();
    for (const [k, v] of next.entries()) subscriptions.set(k, v);

    if (liveSnapshot) {
      // For any owner touched by upsertGuild during the scan, the
      // in-memory write IS the truth — even when scan returned a row
      // for the same owner. The scan may have caught a pre-rotate
      // sibling row before propagateGuildWebhookSubscription
      // converged; preferring liveSnapshot closes the up-to-30s
      // window where the cache would otherwise hold a stale secret.
      for (const ownerId of upsertsDuringScan) {
        const liveEntry = liveSnapshot.get(ownerId);
        if (liveEntry) subscriptions.set(ownerId, liveEntry);
      }
    }

    // INVARIANT: primed only ever transitions false → true within a
    // process. It never flips back (transient scan failures leave the
    // old map in place rather than re-flagging unprimed). This is
    // load-bearing for the receiver's two-limiter threat model: the
    // post-primed unknown-owner path runs unbounded HMAC work only when
    // we're certain the cache reflects the world. If you ever need to
    // re-flag unprimed mid-flight (e.g. an explicit cache-rebuild
    // command), add a per-IP ceiling on the unprimed path FIRST.
    // _resetForTesting() is the only allowed back-transition.
    primed = true;
    lastScanCompletedAt = Date.now();
    return 'completed';
  } finally {
    scanInFlight = false;
  }
}

// Receiver helper: tells the receiver whether an OWNER_UNKNOWN is
// likely a sibling-replica eventual-consistency lag (just-linked
// guild not yet visible to this replica's scan) vs. a genuinely
// absent owner. Used to upgrade 401 → 503 inside the lag window so
// qurl-service's 5x retry catches the next-tick refresh.
function isWithinSiblingLagWindow() {
  if (lastScanCompletedAt === 0) return true; // never completed
  const elapsed = Date.now() - lastScanCompletedAt;
  return elapsed < (REFRESH_INTERVAL_MS + SIBLING_LAG_GRACE_MS);
}

async function refreshTick() {
  try {
    const result = await scanOnce();
    // Only reset on a completed scan. If scanOnce was 'skipped' (the
    // previous tick is still in flight), leave the failure counter
    // alone — a slow scan during an outage could otherwise mask the
    // REFRESH_FAIL_ESCALATE_AT alarm.
    if (result === 'completed') consecutiveFailures = 0;
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

// Safe to call without having called start() — on the gateway tier
// where the registry is never started, this is a no-op. If you ever
// extend this beyond clearInterval, keep the unconditional-safe
// contract: server.js calls stop() unconditionally for the gateway
// tier (no start() match) and the HTTP tier alike.
function stop() {
  if (timer) {
    clearInterval(timer);
    timer = null;
  }
}

// Test-only: drive lastScanCompletedAt directly so the receiver's
// isWithinSiblingLagWindow check can be exercised at both ends of
// the window in unit tests without time-mocking Date.now globally.
function _setLastScanCompletedAtForTesting(ts) {
  lastScanCompletedAt = ts;
}

// Test-only: reset module-level state. Production code must never
// reach for this — the leading-underscore signals intent. Mirrors
// the ddb-store.js pattern for _TABLES_FOR_TESTING.
function _resetForTesting() {
  subscriptions.clear();
  primed = false;
  consecutiveFailures = 0;
  discoveryConsecutiveFailures = 0;
  defaultOwnerId = null;
  upsertsDuringScan.clear();
  scanInFlight = false;
  lastScanCompletedAt = 0;
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
  isWithinSiblingLagWindow,
  upsertGuild,
  removeGuild,
  // Test-only: production callers should let the 30s ticker drive
  // refresh. Exposed because the test suite needs to drive scans
  // deterministically without waiting on real intervals.
  scanOnce,
  // Test-only: drives the failure-counter + escalation-audit logic
  // synchronously. Production code uses the 30s setInterval inside
  // start() to call this.
  _refreshTickForTesting: refreshTick,
  _setLastScanCompletedAtForTesting,
  _resetForTesting,
};
