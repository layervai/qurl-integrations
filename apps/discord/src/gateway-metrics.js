/**
 * Periodic gateway-side metric emitters for Phase 1 monitoring.
 *
 * Two timers, both run on the gateway-only ECS task:
 *   1. Heartbeat (30 s) — dual-emission. Healthy ticks emit
 *      gateway_heartbeat_healthy (paired with the missing-data
 *      `gateway_heartbeat_silence` alarm); unhealthy ticks emit
 *      gateway_heartbeat_unhealthy carrying activity_age_ms for
 *      observability.
 *   2. Active-guild gauge (60 s) — emits active_guild_count carrying
 *      client.guilds.cache.size.
 *
 * Health is gated on heartbeat ACK age only. activity_age_ms is
 * reported as a metric for observability but does NOT gate health.
 * Why: discord.js's `client.on('raw', ...)` fires on op-0 dispatched
 * events only — HEARTBEAT_ACK and other control packets never trigger
 * it. So an idle bot (no chat traffic) and a wedged bot (no ACKs)
 * look identical at this signal level. Gating health on
 * activity_age_ms therefore false-positives on quiet workspaces;
 * gating on ACK age catches the real WS-wedge case.
 */
const logger = require('./logger');
const { AUDIT_EVENTS } = require('./constants');

const HEARTBEAT_INTERVAL_MS = 30_000;
const ACTIVE_GUILD_INTERVAL_MS = 60_000;
const HEARTBEAT_ACK_AGE_THRESHOLD_MS = 60_000;

// Time-since-last-dispatch gauge. discord.js's `client.on('raw', ...)`
// fires on dispatched gateway events (op 0) only — control packets like
// HEARTBEAT_ACK do NOT trigger it. So this is a "time since last Discord
// event" signal, not a "time since last frame" signal. Useful as an
// observability gauge, NOT as a health predicate (an idle bot with a
// healthy WebSocket has the same shape as a wedged bot).
let lastGatewayActivityAt = 0;

function noteGatewayActivity(maybeNow) {
  const now = typeof maybeNow === 'function' ? maybeNow : Date.now;
  lastGatewayActivityAt = now();
}

function _resetGatewayActivity() {
  lastGatewayActivityAt = 0;
}

/**
 * Inspect the discord.js client's WebSocketManager and return a
 * health snapshot. Pure function — callers decide whether to emit.
 *
 * @param {import('discord.js').Client} client
 * @param {() => number} now - Injected for testability.
 * @returns {{ healthy: boolean, ping_ms: number, ack_age_ms: number|null, activity_age_ms: number|null, is_ready: boolean }}
 */
function readGatewayHealth(client, now = Date.now) {
  const t = now();
  const isReady = typeof client.isReady === 'function' ? client.isReady() : false;
  const ping = client.ws?.ping;
  const ping_ms = typeof ping === 'number' ? ping : -1;

  // discord.js v14 stores the most recent HEARTBEAT_ACK timestamp on
  // each WebSocketShard as `lastPingTimestamp` (-1 pre-first-ack).
  // Iterate so a future sharding flip is automatic; oldest across
  // shards is the worst-case heartbeat age.
  let oldestAck = null;
  if (client.ws?.shards && typeof client.ws.shards.values === 'function') {
    for (const shard of client.ws.shards.values()) {
      const acked = shard?.lastPingTimestamp;
      // > 0 rejects both the -1 sentinel and any future change to
      // 0 / null without falsely emitting "healthy" before the first
      // heartbeat round-trip.
      if (typeof acked === 'number' && acked > 0) {
        if (oldestAck === null || acked < oldestAck) oldestAck = acked;
      }
    }
  }
  // Math.max(0, ...) guards against an NTP step backward producing a
  // negative age that would otherwise satisfy `< 60_000`.
  const ack_age_ms = oldestAck === null ? null : Math.max(0, t - oldestAck);

  const activity_age_ms = lastGatewayActivityAt === 0
    ? null
    : Math.max(0, t - lastGatewayActivityAt);

  const healthy =
    isReady &&
    ping_ms > 0 &&
    ack_age_ms !== null &&
    ack_age_ms < HEARTBEAT_ACK_AGE_THRESHOLD_MS;

  return {
    healthy,
    ping_ms,
    ack_age_ms,
    activity_age_ms,
    is_ready: isReady,
  };
}

/**
 * Start the heartbeat timer. Returns the timer handle so callers
 * (gracefulShutdown) can clear it.
 *
 * @param {import('discord.js').Client} client
 * @param {{ intervalMs?: number, now?: () => number }} [opts]
 */
function startGatewayHeartbeat(client, opts = {}) {
  const intervalMs = opts.intervalMs ?? HEARTBEAT_INTERVAL_MS;
  const now = opts.now ?? Date.now;

  // Per-timer transition memory so we log a single warn at the
  // healthy → unhealthy edge and a single info at recovery, not every
  // 30 s of cadence. Initial null = first observation is silent (boot
  // window can flap freely without log spam).
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
        logger.audit(AUDIT_EVENTS.GATEWAY_HEARTBEAT_UNHEALTHY, {
          ping_ms: snapshot.ping_ms,
          ack_age_ms: snapshot.ack_age_ms,
          activity_age_ms: snapshot.activity_age_ms,
          is_ready: snapshot.is_ready,
        });
      }
      if (prevHealthy === true && !snapshot.healthy) {
        logger.warn('Gateway heartbeat: healthy → unhealthy', {
          is_ready: snapshot.is_ready,
          ping_ms: snapshot.ping_ms,
          ack_age_ms: snapshot.ack_age_ms,
          activity_age_ms: snapshot.activity_age_ms,
        });
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
  // INSUFFICIENT_DATA → ALARM during steady-state boot.
  tick();

  const timer = setInterval(tick, intervalMs);
  if (typeof timer.unref === 'function') timer.unref();
  return timer;
}

/**
 * Start the active-guild-count gauge.
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

  // Caveat (#196): index.js calls startActiveGuildCount() right after
  // client.login() resolves, which is BEFORE the gateway READY event
  // populates client.guilds.cache. The first datapoint here can be 0
  // while the bot is actually in N guilds — an artifact of the cache
  // hydrating asynchronously. The 60s interval makes this self-
  // correcting within one window.
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
  HEARTBEAT_INTERVAL_MS,
  ACTIVE_GUILD_INTERVAL_MS,
  HEARTBEAT_ACK_AGE_THRESHOLD_MS,
  ...(process.env.NODE_ENV === 'test' && {
    _test: { _resetGatewayActivity },
  }),
};
