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

/**
 * Initialize a process running under `PROCESS_ROLE=http` so its
 * route handlers can hit Discord via REST without a Gateway login.
 *
 * Two side effects:
 *   - `client.rest.setToken(config.DISCORD_TOKEN)` so REST calls
 *     authenticate (login() is the normal seeder; we skip it here).
 *   - `await refreshCache()` when GUILD_ID is set, so single-guild
 *     OAuth + webhook handlers find a populated cache on the first
 *     request. Multi-tenant deployments (GUILD_ID unset) skip the
 *     refresh — refreshCache is a no-op there, and the OpenNHP
 *     routes that consume the cache aren't mounted anyway.
 *
 * Pure dependency injection (client, config, refreshCache passed
 * in) — keeps the helper testable without importing the heavy
 * discord.js Client at test time.
 */
async function initHttpOnly({ client, config, refreshCache }) {
  client.rest.setToken(config.DISCORD_TOKEN);
  if (config.GUILD_ID) {
    await refreshCache();
  }
}

module.exports = { initHttpOnly };
