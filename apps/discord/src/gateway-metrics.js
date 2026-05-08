/**
 * Periodic gateway-side metric emitters for Phase 1 monitoring.
 *
 * Two timers, both run on the gateway-only ECS task:
 *   1. Positive-signal heartbeat (30 s) — emits gateway_heartbeat_healthy
 *      when discord.js reports a connected, ack'd, recently-active
 *      WebSocket. Missing emissions are the alarm condition (paired
 *      with treat_missing_data = breaching at the alarm side), which
 *      catches the wedge classes client.isReady() alone misses:
 *      heartbeat-zombie, dispatch-deadlock, event-loop saturation.
 *   2. Active-guild gauge (60 s) — emits active_guild_count carrying
 *      client.guilds.cache.size. Used for install/uninstall trend
 *      detection and as a sanity check on guild-scoped features.
 *
 * Composite readiness threshold (60 s ack age) lines up with the alarm
 * window: alarm fires after 60 s of missing heartbeats. Client-side
 * threshold one heartbeat-interval (~41 s) plus one buffer cycle.
 */
const { WebSocketShardDestroyRecovery } = require('discord.js');
const logger = require('./logger');
const { AUDIT_EVENTS } = require('./constants');

const HEARTBEAT_INTERVAL_MS = 30_000;
const ACTIVE_GUILD_INTERVAL_MS = 60_000;
const HEARTBEAT_ACK_AGE_THRESHOLD_MS = 60_000;

// Auto-recovery thresholds for the zombie-WS pattern observed
// 2026-05-08 (sandbox + prod): bot's `activity_age_ms` climbed past
// 60s while heartbeat ACKs kept landing — Discord stopped pushing
// events but discord.js didn't detect the dead session. Both bots
// hit it simultaneously when their gateway pod blipped. Fix: when
// activity is stale past `RECOVERY_ACTIVITY_THRESHOLD_MS` AND the
// client thinks it's ready (so this isn't a normal boot/disconnect
// flap), terminate every shard's WebSocket so the manager
// reconnects fresh. `RECOVERY_COOLDOWN_MS` prevents a reconnect
// storm if termination doesn't immediately restore activity.
const RECOVERY_ACTIVITY_THRESHOLD_MS = 120_000;
const RECOVERY_COOLDOWN_MS = 10 * 60_000;
let lastRecoveryAttemptAt = 0;

// Composite-readiness gateway-activity signal. client.isReady() alone
// misses event-loop wedges (DB pool exhaustion, qurl-service stalls,
// Auth0 refresh hangs) — the WebSocket stays connected but the raw
// handler stops firing.
//
// Threshold = 60 s matches the heartbeat-ack threshold. discord.js
// emits a heartbeat every ~41 s and the bot's `client.on('raw', ...)`
// handler updates this timestamp on every WebSocket frame including
// heartbeats — so an idle gateway still ticks. A wedge at the event-
// loop level prevents the raw handler from running, the timestamp
// goes stale, and the composite goes unhealthy.
const GATEWAY_ACTIVITY_THRESHOLD_MS = 60_000;
let lastGatewayActivityAt = 0;

/**
 * Note that the gateway just received a frame. Called from the
 * `client.on('raw', ...)` hook in index.js (gateway-only path —
 * never invoked from the HTTP role; if `readGatewayHealth` is ever
 * called from HTTP, it'll always report unhealthy because this
 * timestamp stays at 0). Cheap — just a timestamp update — so it's
 * safe to fire on every frame including heartbeats.
 *
 * Defensive arg shape: discord.js's `raw` event hands the listener
 * a packet object as the first argument, NOT a clock function. The
 * `typeof maybeNow === 'function'` check lets the same function
 * be passed directly to `client.on('raw', noteGatewayActivity)`
 * (saves a closure allocation per frame) AND still accept a custom
 * clock from tests via `noteGatewayActivity(() => fakeNow)`.
 *
 * @param {(() => number) | unknown} [maybeNow] — optional clock fn
 *   for tests; any non-function argument falls back to Date.now.
 */
function noteGatewayActivity(maybeNow) {
  const now = typeof maybeNow === 'function' ? maybeNow : Date.now;
  lastGatewayActivityAt = now();
}

/**
 * Test-only reset for the module-level activity timestamp. NOT
 * exported in the public API — accessed via `_test` for jest.
 */
function _resetGatewayActivity() {
  lastGatewayActivityAt = 0;
}

/**
 * Test-only reset for the recovery cooldown clock.
 */
function _resetRecoveryClock() {
  lastRecoveryAttemptAt = 0;
}

/**
 * Detect the zombie-WS pattern (heartbeats land but Discord events
 * don't) and force a fresh session. Returns the action taken so
 * callers can log + test the decision.
 *
 * Decision tree:
 *  - is_ready=false → boot/reconnect already in flight, skip
 *  - activity_age_ms < RECOVERY_ACTIVITY_THRESHOLD_MS → not zombie
 *  - within RECOVERY_COOLDOWN_MS of last attempt → debounce
 *  - else → terminate every shard via shard.destroy() so the manager
 *    re-IDENTIFYs cleanly. Per-shard so a future multi-shard config
 *    only restarts the wedged shard, not the whole client.
 *
 * @returns {{ triggered: boolean, reason: string }}
 */
function maybeAutoRecoverZombieWS(client, snapshot, now = Date.now) {
  if (!snapshot.is_ready) return { triggered: false, reason: 'not_ready' };
  if (snapshot.activity_age_ms === null) return { triggered: false, reason: 'no_activity_baseline' };
  if (snapshot.activity_age_ms < RECOVERY_ACTIVITY_THRESHOLD_MS) {
    return { triggered: false, reason: 'within_threshold' };
  }
  const t = now();
  if (t - lastRecoveryAttemptAt < RECOVERY_COOLDOWN_MS) {
    return { triggered: false, reason: 'cooldown' };
  }

  // Per-shard destroy so a future multi-shard config (see the
  // multi-shard TODO in readGatewayHealth above) only resets the
  // wedged shard. discord.js shard.destroy is async and only
  // re-IDENTIFYs when `recover` is set — without it, the shard
  // transitions to Idle and stays there. `code` (NOT closeCode —
  // different field name) 4000 = re-establishable. Fire-and-catch:
  // tick() is sync but destroy awaits internals (updateSessionInfo,
  // ws onclose race, internalConnect). Letting that promise reject
  // would surface as an unhandledRejection that crashes the process
  // on Node 16+.
  //
  // Cooldown choice: stamped optimistically as soon as destroy() is
  // dispatched (line below), NOT on resolved-success. Rationale: if
  // every shard's destroy rejects asynchronously, we'd be locked out
  // for 10 min — but the bot is still emitting unhealthy heartbeats,
  // and the activity_age_ms alarm (qurl-integrations-infra #446)
  // will page on-call within ~2 min. Async rejections are rare and
  // operator-handled; the optimistic stamp prevents a reconnect
  // storm in the common case where destroy() succeeds.
  let shardsTerminated = 0;
  if (client.ws?.shards && typeof client.ws.shards.values === 'function') {
    for (const shard of client.ws.shards.values()) {
      if (typeof shard?.destroy !== 'function') continue;
      try {
        const result = shard.destroy({
          code: 4000,
          reason: 'auto-recovery: zombie ws (activity stale)',
          recover: WebSocketShardDestroyRecovery.Reconnect,
        });
        if (result && typeof result.catch === 'function') {
          result.catch((err) => {
            logger.warn('Shard destroy promise rejected during auto-recovery', { error: err?.message });
          });
        }
        shardsTerminated++;
      } catch (err) {
        // Sync throw = bad-args / TypeError before the async body
        // runs. Already-Idle shards return synchronously without
        // throwing, so this branch is for genuinely unexpected
        // shapes; log and keep going so a single broken shard
        // doesn't block the others.
        logger.warn('Shard destroy threw synchronously during auto-recovery', { error: err?.message });
      }
    }
  }

  // Only stamp the cooldown when something actually got terminated.
  // If client.ws.shards is missing/empty (rare — would mean is_ready
  // is true while no shards exist, contradiction in steady state),
  // retry on the next tick rather than locking out for 10 min.
  if (shardsTerminated === 0) {
    return { triggered: false, reason: 'no_shards' };
  }
  // TODO(multi-shard): pairs with the multi-shard TODO in
  // readGatewayHealth above. When sharding flips on, the actually-
  // wedged shard could be the one that sync-throws while a healthy
  // shard's destroy succeeds — locking recovery out for 10 min while
  // the stuck shard stays stuck. Move to a per-shard cooldown map
  // (lastRecoveryAttemptAt[shardId]) at that point.
  lastRecoveryAttemptAt = t;
  return { triggered: true, reason: 'zombie_ws', shardsTerminated };
}

/**
 * Inspect the discord.js client's WebSocketManager and return a
 * health snapshot. Pure function — callers decide whether to emit.
 *
 * @param {import('discord.js').Client} client
 * @param {() => number} now - Injected for testability.
 * @returns {{ healthy: boolean, ping_ms: number, ack_age_ms: number|null, is_ready: boolean }}
 */
function readGatewayHealth(client, now = Date.now) {
  // Single point-in-time read so ack_age_ms and activity_age_ms are
  // computed against the same `t`. Practically irrelevant (microseconds
  // of drift between two now() calls), but useful for tests injecting
  // a non-monotonic now and for keeping the snapshot semantically
  // atomic.
  const t = now();
  const isReady = typeof client.isReady === 'function' ? client.isReady() : false;
  const ping = client.ws?.ping;
  const ping_ms = typeof ping === 'number' ? ping : -1;

  // discord.js v14 exposes the most recent heartbeat-round-trip
  // timestamp on each WebSocketShard as `lastPingTimestamp` (set in
  // WebSocketManager.js on every HEARTBEAT_ACK; initialized to -1
  // pre-first-ack per WebSocketShard.js:54). NOT `lastHeartbeatAcked`
  // — that field doesn't exist in v14. Iterate shards.values() so a
  // future sharding flip is automatic; the oldest timestamp across
  // shards is the worst-case heartbeat age and the right number to
  // alarm on.
  let oldestAck = null;
  if (client.ws?.shards && typeof client.ws.shards.values === 'function') {
    for (const shard of client.ws.shards.values()) {
      const acked = shard?.lastPingTimestamp;
      // > 0 rejects both the -1 sentinel (pre-first-ack) and any
      // future change to 0 / null without falsely emitting "healthy"
      // before the first heartbeat round-trip completes.
      // TODO(multi-shard): if shard 0 ack'd recently and shard 1 has
      // never ack'd (-1), this loop reports healthy from shard 0
      // alone — a stuck-since-boot shard wouldn't move the needle.
      // Single-shard today so unobservable, but when sharding flips
      // on, treat a -1 sentinel as itself unhealthy after a settle
      // window (e.g. >2× heartbeat interval since process start).
      if (typeof acked === 'number' && acked > 0) {
        if (oldestAck === null || acked < oldestAck) oldestAck = acked;
      }
    }
  }
  // Math.max(0, ...) guards against an NTP step backward producing a
  // negative age that would otherwise satisfy `< 60_000` and falsely
  // mark the gateway healthy. Date.now() is wall-clock, not monotonic;
  // Fargate hosts get periodic NTP corrections.
  const ack_age_ms = oldestAck === null ? null : Math.max(0, t - oldestAck);

  // Activity check (Justin #193 §2 — dispatch-wedge / event-loop
  // saturation). lastGatewayActivityAt = 0 means we've never seen a
  // frame, so the bot can't be healthy yet (boot window). Same NTP
  // clamp logic as ack_age — a backward step shouldn't paper over a
  // stale timestamp.
  const activity_age_ms = lastGatewayActivityAt === 0
    ? null
    : Math.max(0, t - lastGatewayActivityAt);

  const healthy =
    isReady &&
    ping_ms > 0 &&
    ack_age_ms !== null &&
    ack_age_ms < HEARTBEAT_ACK_AGE_THRESHOLD_MS &&
    activity_age_ms !== null &&
    activity_age_ms < GATEWAY_ACTIVITY_THRESHOLD_MS;

  return {
    healthy,
    ping_ms,
    ack_age_ms,
    activity_age_ms,
    is_ready: isReady,
  };
}

/**
 * Start the positive-signal heartbeat timer. Returns the timer handle
 * so callers (gracefulShutdown) can clear it.
 *
 * Only emits on the healthy path — silence is the alarm condition.
 * Emitting an "unhealthy" event would create a second metric the
 * operator would have to remember to watch; the missing-data trick
 * collapses that into one alarm.
 *
 * @param {import('discord.js').Client} client
 * @param {{ intervalMs?: number, now?: () => number }} [opts]
 */
function startGatewayHeartbeat(client, opts = {}) {
  const intervalMs = opts.intervalMs ?? HEARTBEAT_INTERVAL_MS;
  const now = opts.now ?? Date.now;

  // Per-timer transition memory so we log a single warn at the
  // healthy → unhealthy edge and a single info at the recovery edge,
  // not every 30 s of cadence. Initial null = first observation is
  // silent (boot window can flap freely without log spam).
  let prevHealthy = null;

  function tick() {
    try {
      const snapshot = readGatewayHealth(client, now);
      if (snapshot.healthy) {
        logger.audit(AUDIT_EVENTS.GATEWAY_HEARTBEAT, {
          ping_ms: snapshot.ping_ms,
          ack_age_ms: snapshot.ack_age_ms,
          activity_age_ms: snapshot.activity_age_ms,
        });
      } else {
        // Pair-event for the healthy heartbeat so activity_age_ms is
        // observable as a metric on EVERY tick. Without this, the
        // healthy emission caps activity_age_ms at <60s by definition
        // and a Max(activity_age_ms) > 60s alarm would never fire — yet
        // 60s+ is exactly the zombie-WS signal we need to alarm on
        // (5/8 incident). null-safe payload: ack_age_ms is null pre-
        // first-ack, ping_ms is -1 pre-first-ws.
        logger.audit(AUDIT_EVENTS.GATEWAY_HEARTBEAT_UNHEALTHY, {
          ping_ms: snapshot.ping_ms,
          ack_age_ms: snapshot.ack_age_ms,
          activity_age_ms: snapshot.activity_age_ms,
          is_ready: snapshot.is_ready,
        });
      }
      // Edge-triggered logs so on-call can distinguish "wedged for
      // 5 min" (one warn at t=0, then silence on the metric side) from
      // "metric pipeline broken" (zero log lines + zero metric).
      if (prevHealthy === true && !snapshot.healthy) {
        logger.warn('Gateway heartbeat: healthy → unhealthy', {
          is_ready: snapshot.is_ready,
          ping_ms: snapshot.ping_ms,
          ack_age_ms: snapshot.ack_age_ms,
          activity_age_ms: snapshot.activity_age_ms,
        });
      }
      // Zombie-WS recovery runs on every unhealthy tick (not just
      // the edge) — debounced by RECOVERY_COOLDOWN_MS internally.
      // Edge-only would miss the case where the bot was already
      // unhealthy at boot and stayed there.
      if (!snapshot.healthy) {
        const recovery = maybeAutoRecoverZombieWS(client, snapshot, now);
        if (recovery.triggered) {
          logger.warn('Gateway auto-recovery: forcing reconnect (zombie WS)', {
            activity_age_ms: snapshot.activity_age_ms,
            ack_age_ms: snapshot.ack_age_ms,
            shards_terminated: recovery.shardsTerminated,
          });
        }
      }
      if (prevHealthy === false && snapshot.healthy) {
        logger.info('Gateway heartbeat: unhealthy → healthy', {
          ping_ms: snapshot.ping_ms,
          ack_age_ms: snapshot.ack_age_ms,
          activity_age_ms: snapshot.activity_age_ms,
        });
      }
      prevHealthy = snapshot.healthy;
    } catch (err) {
      // Heartbeat must never break the bot. Swallow + log so a future
      // discord.js API change doesn't take down the gateway process.
      logger.warn('Gateway heartbeat sampler threw', { error: err?.message });
    }
  }

  // Run once immediately so the first metric datapoint lands inside
  // the alarm's 60s evaluation window. Without this, setInterval
  // doesn't fire until t+30s and the alarm transitions
  // INSUFFICIENT_DATA → ALARM during steady-state boot. Note: the
  // 30s interval × 2 fits inside the 60s alarm window so a single
  // missed sample doesn't trip the alarm — only sustained absence
  // does. If you ever drop the alarm threshold to 30s, also drop the
  // sampling interval to avoid flap.
  tick();

  const timer = setInterval(tick, intervalMs);

  // .unref() so the timer doesn't keep the event loop alive at shutdown.
  if (typeof timer.unref === 'function') timer.unref();

  return timer;
}

/**
 * Start the active-guild-count gauge. Same shape as heartbeat, lower
 * cadence — guild membership doesn't change often.
 *
 * @param {import('discord.js').Client} client
 * @param {{ intervalMs?: number }} [opts]
 */
function startActiveGuildCount(client, opts = {}) {
  const intervalMs = opts.intervalMs ?? ACTIVE_GUILD_INTERVAL_MS;

  function tick() {
    try {
      const count = client.guilds?.cache?.size;
      if (typeof count === 'number') {
        logger.audit(AUDIT_EVENTS.ACTIVE_GUILD_COUNT, { count });
      }
    } catch (err) {
      logger.warn('Active-guild-count sampler threw', { error: err?.message });
    }
  }

  // Symmetric with startGatewayHeartbeat — runOnce so the first
  // datapoint lands inside any future alarm window without waiting
  // for the first interval. Today this metric is gauge-only on the
  // dashboard, so the immediate emit is just consistency, but it
  // keeps both timers behaving the same way.
  //
  // Caveat (#196): index.js calls startActiveGuildCount() right
  // after client.login() resolves, which is BEFORE the gateway READY
  // event populates client.guilds.cache. The first datapoint here
  // can be 0 while the bot is actually in N guilds — an artifact of
  // the cache hydrating asynchronously. The 60s interval makes this
  // self-correcting within one window. Phase 2 alarm-wiring on this
  // metric must reference #196 and decide on a fix (defer first
  // emit until ready, OR alarm-side ignore-the-boot-window).
  tick();

  const timer = setInterval(tick, intervalMs);

  if (typeof timer.unref === 'function') timer.unref();

  return timer;
}

module.exports = {
  startGatewayHeartbeat,
  startActiveGuildCount,
  readGatewayHealth,
  noteGatewayActivity,
  maybeAutoRecoverZombieWS,
  HEARTBEAT_INTERVAL_MS,
  ACTIVE_GUILD_INTERVAL_MS,
  HEARTBEAT_ACK_AGE_THRESHOLD_MS,
  GATEWAY_ACTIVITY_THRESHOLD_MS,
  RECOVERY_ACTIVITY_THRESHOLD_MS,
  RECOVERY_COOLDOWN_MS,
  // Test-only exports (NODE_ENV-gated to keep live state out of prod
  // consumers, mirroring the commands.js _test pattern).
  // _test gated on NODE_ENV === 'test' (NOT !== 'production') so
  // a misconfigured prod task with NODE_ENV unset doesn't ship the
  // internal handles. jest sets NODE_ENV='test' automatically; if
  // local-dev workflows need access, set NODE_ENV=test for that
  // shell.
  ...(process.env.NODE_ENV === 'test' && {
    _test: { _resetGatewayActivity, _resetRecoveryClock },
  }),
};
