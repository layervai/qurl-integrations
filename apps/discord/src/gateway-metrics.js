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
const logger = require('./logger');
const { AUDIT_EVENTS } = require('./constants');

const HEARTBEAT_INTERVAL_MS = 30_000;
const ACTIVE_GUILD_INTERVAL_MS = 60_000;
const HEARTBEAT_ACK_AGE_THRESHOLD_MS = 60_000;

/**
 * Inspect the discord.js client's WebSocketManager and return a
 * health snapshot. Pure function — callers decide whether to emit.
 *
 * @param {import('discord.js').Client} client
 * @param {() => number} now - Injected for testability.
 * @returns {{ healthy: boolean, ping_ms: number, ack_age_ms: number|null, is_ready: boolean }}
 */
function readGatewayHealth(client, now = Date.now) {
  const isReady = typeof client.isReady === 'function' ? client.isReady() : false;
  const ping = client.ws?.ping;
  const ping_ms = typeof ping === 'number' ? ping : -1;

  // discord.js v14 exposes per-shard heartbeat-ack timestamps on the
  // WebSocketShard. shards is a Collection; for an unsharded bot there
  // is exactly one shard at id 0. We iterate so a future shard count
  // change is automatic — the oldest ack across shards is the worst
  // case and the right number to alarm on.
  let oldest_ack = null;
  if (client.ws?.shards && typeof client.ws.shards.values === 'function') {
    for (const shard of client.ws.shards.values()) {
      const acked = shard?.lastHeartbeatAcked;
      if (typeof acked === 'number' && acked > 0) {
        if (oldest_ack === null || acked < oldest_ack) oldest_ack = acked;
      }
    }
  }
  const ack_age_ms = oldest_ack === null ? null : now() - oldest_ack;

  const healthy =
    isReady &&
    ping_ms > 0 &&
    ack_age_ms !== null &&
    ack_age_ms < HEARTBEAT_ACK_AGE_THRESHOLD_MS;

  return { healthy, ping_ms, ack_age_ms, is_ready: isReady };
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

  const timer = setInterval(() => {
    try {
      const snapshot = readGatewayHealth(client, now);
      if (snapshot.healthy) {
        logger.audit(AUDIT_EVENTS.GATEWAY_HEARTBEAT, {
          ping_ms: snapshot.ping_ms,
          ack_age_ms: snapshot.ack_age_ms,
        });
      }
    } catch (err) {
      // Heartbeat must never break the bot. Swallow + log so a future
      // discord.js API change doesn't take down the gateway process.
      logger.warn('Gateway heartbeat sampler threw', { error: err?.message });
    }
  }, intervalMs);

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

  const timer = setInterval(() => {
    try {
      const count = client.guilds?.cache?.size;
      if (typeof count === 'number') {
        logger.audit(AUDIT_EVENTS.ACTIVE_GUILD_COUNT, { count });
      }
    } catch (err) {
      logger.warn('Active-guild-count sampler threw', { error: err?.message });
    }
  }, intervalMs);

  if (typeof timer.unref === 'function') timer.unref();

  return timer;
}

module.exports = {
  startGatewayHeartbeat,
  startActiveGuildCount,
  readGatewayHealth,
  HEARTBEAT_INTERVAL_MS,
  ACTIVE_GUILD_INTERVAL_MS,
  HEARTBEAT_ACK_AGE_THRESHOLD_MS,
};
