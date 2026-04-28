// discord-rest — Discord REST helpers, sharing client.rest with discord.js.
//
// Background: HTTP-process tasks (OAuth callbacks, GitHub webhooks,
// /health probes) don't need the Gateway WebSocket — every interaction
// is one-shot and the REST API gives fresh data. Only one Gateway
// connection may be active per bot token, so an http-only replica
// behind an ALB must NOT call client.login() (it would flap session
// identity against the gateway singleton).
//
// This module exposes the small subset of REST operations http-side
// code needs (DM delivery, role assignment) with an `{ ok, status,
// error }`-shaped return value rather than thrown exceptions —
// callers branch on `.ok` instead of try/catching.
//
// Why share `client.rest`: `@discordjs/rest`'s rate-limit buckets are
// per-instance, not per-process. A second `new REST(...)` would
// negotiate `POST /channels/:id/messages` independently from the
// helpers in `src/discord.js`, so a 429 hit by one wouldn't back off
// the other. Reusing `client.rest` keeps the bucket state shared.
// The Client object is already required at module load by `index.js`
// regardless of role; importing it here adds no extra weight.
//
// Auth: the bot token lands on `client.rest` from one of two paths —
//   - combined / gateway modes: `client.login()` calls `setToken()`
//     internally as part of opening the WebSocket.
//   - http-only mode: `src/http-only-init.js` calls `setToken()`
//     explicitly because login() is gated off in that role.
// Either way, the helpers below see a token-bearing rest by the time
// they're invoked from a route handler.

const { Routes } = require('discord-api-types/v10');
const { client } = require('./discord');
const config = require('./config');
const logger = require('./logger');

// Shared instance — same object as `require('./discord').client.rest`.
// Re-exported below for tests + potential direct use.
const rest = client.rest;

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
    // Discord returns HTTP 403 for several recipient-side reasons:
    // DMs disabled, the bot was blocked, server-level DM restrictions,
    // or "Missing Access" when there's no shared guild. All are
    // expected operational outcomes (not bugs), so log at info level
    // to keep oncall signal-to-noise high. Caller decides whether to
    // fall back to a channel mention.
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
