// http-only-init — boot wiring for `PROCESS_ROLE=http` replicas.
//
// In combined / gateway mode, `client.login()` does two things our
// route handlers depend on: (1) it sets the bot token on
// `client.rest`, so REST helpers can authenticate, and (2) the
// resulting `ready` event triggers refreshCache(), populating the
// cached guild handle.
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
// No periodic refresh, deliberately. Until #1051 this module ran a
// 10-minute REST refreshCache() to compensate for the Gateway
// `roleDelete` / `channelDelete` events that http-only replicas never
// receive. #1051 removed the OpenNHP mode concept along with those
// handlers and the roles/channels cache they invalidated, leaving
// refreshCache() to populate one thing: the module-level `guild`
// handle in src/discord.js. Nothing reads that handle on a schedule —
// `verifyBotPermissions()` and the `Watching guild:` log both hang off
// `client.once('ready')`, which never fires here because login() is
// skipped, and the `getGuild()` export has no production consumers. A
// timer would spend a REST call per interval refreshing a value no
// request path reads.
//
// What still proves this replica can reach Discord before it serves
// traffic is the `client.user` seed below: `GET /users/@me` runs in
// both modes and is fatal on failure, so it covers the token and
// Discord reachability. The single boot-time refreshCache() adds the
// single-guild half — that GUILD_ID actually resolves to a guild this
// bot is in — which is why it, and not the reachability check, is the
// part multi-tenant replicas skip.

/**
 * Initialize a process running under `PROCESS_ROLE=http` so its
 * route handlers can hit Discord via REST without a Gateway login.
 *
 * Side effects:
 *   - `client.rest.setToken(config.DISCORD_TOKEN)` so REST calls
 *     authenticate (login() is the normal seeder; we skip it here).
 *   - `client.user` seeded from REST `GET /users/@me` (see the
 *     inline comment below for why the worker tier needs it).
 *   - Initial `await refreshCache()` when GUILD_ID is set, both to
 *     warm the cached guild handle and as a fatal boot-time
 *     reachability check. Multi-tenant deployments (GUILD_ID unset)
 *     skip it — refreshCache is a no-op there.
 *
 * Returns nothing. This helper used to return a periodic-refresh
 * timer for the caller to clearInterval on shutdown; see the
 * no-periodic-refresh note in the module header for why that timer
 * is gone.
 *
 * Pure dependency injection (client, config, refreshCache, logger
 * passed in) — keeps the helper testable without importing the
 * heavy discord.js Client at test time.
 */
async function initHttpOnly({ client, config, refreshCache, logger }) {
  client.rest.setToken(config.DISCORD_TOKEN);
  // Populate `client.user` from REST GET /users/@me. The Pillar 1
  // worker tier reconstructs INTERACTION_CREATE dispatches via
  // `client.actions.InteractionCreate.handle(data)` — the same code
  // path the legacy Client's WebSocketShard runs on a real gateway
  // dispatch. discord.js's Action.getChannel reads
  // `this.client.user.id` to filter the bot itself out of an
  // interaction's recipient list (discord.js src/client/actions/
  // Action.js: `if (recipient.id !== this.client.user.id)`). In
  // gateway mode the READY handler populates `client.user` (READY.js:
  // `client.user = new ClientUser(client, data.user)`); in http-only
  // mode `client.login()` is intentionally skipped (Discord admits
  // exactly one active Gateway connection per bot token, so http-only
  // replicas must NOT collide with the gateway singleton), so READY
  // never fires here. Without this REST seed, every interaction
  // replayed through the worker tier throws "Cannot read properties
  // of null (reading 'id')" on `client.user.id`.
  //
  // Crash-loop on failure (matches the module's fatal-on-REST-error
  // posture): a worker that can't identify itself can't safely
  // service interactions. Better to surface the misconfig at boot
  // than to fail every interaction silently.
  const ClientUser = require('discord.js').ClientUser;
  const { Routes } = require('discord-api-types/v10');
  const me = await client.rest.get(Routes.user('@me'));
  client.user = new ClientUser(client, me);
  logger.info('http-only mode: client.user seeded from REST GET /users/@me', {
    userId: client.user.id,
    username: client.user.username,
  });
  // Falsy check is intentional and aligned with config.js's GUILD_ID
  // normalization: anything that isn't a 17-20 digit Discord snowflake
  // (unset env, "PLACEHOLDER", whitespace-only, etc.) lands as `null`
  // there, so we treat all falsy values uniformly as multi-tenant mode.
  // '0' is technically truthy in JS but isn't a valid snowflake anyway —
  // config.js's regex check would have rejected it upstream.
  if (!config.GUILD_ID) {
    return;
  }
  await refreshCache();
}

module.exports = { initHttpOnly };
