// discord-rest — Discord REST client wrappers (no Gateway / WebSocket).
//
// discord.js's Client object connects to the Discord Gateway via
// WebSocket and maintains a long-lived guild/user cache. That's the
// right shape for the bot's event-driven interactions work, but it's
// the wrong shape for HTTP-process tasks:
//   - Only one Gateway connection may be active per bot token at a
//     time. An HTTP-process replica that called `client.login()`
//     would conflict with the gateway process, causing a session-
//     identity flap every few seconds.
//   - HTTP traffic (OAuth callbacks, GitHub webhooks, /health probes)
//     doesn't need the cache at all — every interaction is one-shot
//     and the REST API gives us fresh data.
//
// This module wraps `@discordjs/rest` with the small subset of
// operations HTTP-side code needs: DM delivery, role assignment, DM
// channel existence checks. Callable from the gateway process too
// (it just makes REST calls alongside its Gateway connection), so
// the same helpers work in either role — no "which process am I"
// logic inside the helpers.
//
// Auth: the bot token is read once at module-load via
// `config.DISCORD_TOKEN`. Same token as the gateway — Discord scopes
// tokens to applications, not connection types.

const { REST } = require('@discordjs/rest');
const { Routes } = require('discord-api-types/v10');
const config = require('./config');
const logger = require('./logger');

// One REST client per process — discord.js's REST client is stateful
// only in that it holds the token + rate-limit buckets. Rate-limit
// buckets are per-process by default, which is fine for a two-process
// split (gateway + HTTP rarely hit the same endpoint concurrently at
// a volume that matters).
const rest = new REST({ version: '10' });
if (config.DISCORD_TOKEN) {
  rest.setToken(config.DISCORD_TOKEN);
}

/**
 * Send a direct message to a Discord user via REST (no Gateway).
 *
 * Two-call flow:
 *   1. POST /users/@me/channels → creates (or returns existing) DM
 *      channel, returns { id }.
 *   2. POST /channels/:id/messages → posts the message body.
 *
 * Returns `{ ok: true, channelId }` on success, `{ ok: false, error }`
 * on failure. Callers log/branch on `.ok` rather than letting
 * exceptions propagate — matches the ergonomics of the legacy
 * gateway-based `sendDM` in `discord.js`.
 *
 * @param {string} userId — Discord snowflake.
 * @param {object} message — discord.js-compatible message payload
 *   (content, embeds, components, etc.). Passed through verbatim to
 *   the REST endpoint.
 */
async function sendDM(userId, message) {
  if (!config.DISCORD_TOKEN) {
    return { ok: false, error: 'DISCORD_TOKEN not configured' };
  }
  try {
    const channel = await rest.post(Routes.userChannels(), {
      body: { recipient_id: userId },
    });
    await rest.post(Routes.channelMessages(channel.id), {
      body: message,
    });
    return { ok: true, channelId: channel.id };
  } catch (err) {
    // Discord returns HTTP 403 "Cannot send messages to this user"
    // when the recipient has DMs disabled or has blocked the bot —
    // expected operational error, not a bug. Logged at info level
    // to avoid oncall noise; caller decides whether to fall back
    // to a channel mention.
    const level = err.status === 403 ? 'info' : 'error';
    logger[level]('sendDM via REST failed', {
      userId, status: err.status, message: err.message,
    });
    return { ok: false, error: err.message, status: err.status };
  }
}

/**
 * Add a role to a guild member via REST (no Gateway).
 * Idempotent on the Discord side — re-adding an existing role is
 * a no-op.
 */
async function addRoleToMember(guildId, userId, roleId) {
  if (!config.DISCORD_TOKEN) {
    return { ok: false, error: 'DISCORD_TOKEN not configured' };
  }
  try {
    await rest.put(Routes.guildMemberRole(guildId, userId, roleId));
    return { ok: true };
  } catch (err) {
    logger.error('addRoleToMember via REST failed', {
      guildId, userId, roleId, status: err.status, message: err.message,
    });
    return { ok: false, error: err.message, status: err.status };
  }
}

/**
 * Remove a role from a guild member via REST (no Gateway).
 */
async function removeRoleFromMember(guildId, userId, roleId) {
  if (!config.DISCORD_TOKEN) {
    return { ok: false, error: 'DISCORD_TOKEN not configured' };
  }
  try {
    await rest.delete(Routes.guildMemberRole(guildId, userId, roleId));
    return { ok: true };
  } catch (err) {
    logger.error('removeRoleFromMember via REST failed', {
      guildId, userId, roleId, status: err.status, message: err.message,
    });
    return { ok: false, error: err.message, status: err.status };
  }
}

module.exports = {
  rest,
  sendDM,
  addRoleToMember,
  removeRoleFromMember,
};
