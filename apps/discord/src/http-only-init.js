// http-only-init — boot wiring for `PROCESS_ROLE=http` replicas.
//
// In combined / gateway mode, `client.login()` does two things our
// route handlers depend on: (1) it sets the bot token on
// `client.rest`, so REST helpers can authenticate, and (2) the
// resulting `ready` event triggers refreshCache(), populating
// guild/roles/channels for OAuth + webhook handlers.
//
// http-only mode skips login() (Discord bot tokens admit only one
// active Gateway connection per token, so the http-only replica
// must not collide with the gateway singleton). Without the two
// side effects above, every helper that touches REST — sendDM,
// channels.X.send, member.roles.add — fails with a 401 on the
// first request. This module reproduces both side effects via the
// REST path so the http replica can serve traffic immediately.
//
// Failures here are intentionally fatal: an http-only replica
// that can't reach Discord can't service its own routes, so we
// crash-loop and let the orchestrator reschedule rather than
// silently start with a cold cache and 5xx every webhook.
//
// Cache invalidation: in combined / gateway mode, the
// `client.on('roleDelete' / 'channelDelete')` handlers in
// `src/discord.js` invalidate the cache when guild admins delete
// tracked roles or channels. Those events ride the Gateway and
// don't fire in http-only mode. To close the resulting staleness
// window we run a periodic REST refreshCache() (every
// REFRESH_INTERVAL_MS) so deletions propagate within one interval
// rather than waiting for a replica restart.

// 10-minute refresh interval — short enough that a deleted role
// stays cached for at most one window before invalidation, long
// enough that the periodic two-REST-call cost (guild.roles.fetch +
// guild.channels.fetch) is rounding error against the bot's normal
// REST budget. Tunable via env var if a future operator needs more
// or less aggressive invalidation.
const REFRESH_INTERVAL_MS = (() => {
  const raw = process.env.HTTP_ONLY_REFRESH_INTERVAL_MS;
  if (!raw) return 10 * 60 * 1000;
  const parsed = parseInt(raw, 10);
  // Reject non-positive / NaN. Bare-minimum 30s floor so a misconfigured
  // 1ms doesn't hammer the Discord REST API.
  if (!Number.isFinite(parsed) || parsed < 30_000) return 10 * 60 * 1000;
  return parsed;
})();

/**
 * Initialize a process running under `PROCESS_ROLE=http` so its
 * route handlers can hit Discord via REST without a Gateway login.
 *
 * Side effects:
 *   - `client.rest.setToken(config.DISCORD_TOKEN)` so REST calls
 *     authenticate (login() is the normal seeder; we skip it here).
 *   - Initial `await refreshCache()` when GUILD_ID is set, so
 *     single-guild OAuth + webhook handlers find a populated cache
 *     on the first request. Multi-tenant deployments (GUILD_ID
 *     unset) skip the refresh — refreshCache is a no-op there, and
 *     the OpenNHP routes that consume the cache aren't mounted
 *     anyway.
 *   - Periodic `setInterval` calling refreshCache every
 *     REFRESH_INTERVAL_MS, gated on the same GUILD_ID check.
 *     `.unref()` so the timer doesn't block process exit;
 *     `gracefulShutdown` in index.js explicitly clears it for
 *     symmetry with the other intervals.
 *   - Boot-time `logger.warn` naming the cache-invalidation
 *     limitation in http-only mode so future grep-from-logs lands
 *     on the periodic-refresh strategy.
 *
 * Returns the periodic-refresh timer so the caller can clearInterval
 * on shutdown. Returns `null` in multi-tenant mode (no timer set up).
 *
 * Pure dependency injection (client, config, refreshCache, logger
 * passed in) — keeps the helper testable without importing the
 * heavy discord.js Client at test time.
 */
async function initHttpOnly({ client, config, refreshCache, logger }) {
  client.rest.setToken(config.DISCORD_TOKEN);
  if (!config.GUILD_ID) {
    return null;
  }
  await refreshCache();
  logger.warn(
    'http-only mode: Gateway-driven cache invalidation (roleDelete/channelDelete) is unavailable. ' +
    `Periodic REST refreshCache compensates every ${Math.round(REFRESH_INTERVAL_MS / 60_000)} minute(s). ` +
    'See src/http-only-init.js for rationale; tune via HTTP_ONLY_REFRESH_INTERVAL_MS env var.'
  );
  const timer = setInterval(() => {
    refreshCache().catch(err => {
      logger.error('Periodic refreshCache failed in http-only mode (will retry next interval)', {
        errorMessage: err?.message,
      });
    });
  }, REFRESH_INTERVAL_MS);
  timer.unref();
  return timer;
}

module.exports = { initHttpOnly, REFRESH_INTERVAL_MS };
