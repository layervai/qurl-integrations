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
const logger = require('./logger');

/**
 * Send a direct message to a Discord user via REST (no Gateway).
 *
 * Two-call flow:
 *   1. POST /users/@me/channels → creates (or returns existing) DM
 *      channel, returns { id }.
 *   2. POST /channels/:id/messages → posts the message body.
 *
 * Returns `{ ok: true, channelId, messageId }` on success,
 * `{ ok: false, error }` on failure. Callers log/branch on `.ok` rather
 * than letting exceptions propagate — matches the ergonomics of the
 * legacy gateway-based `sendDM` in `discord.js`. `messageId` is captured
 * so the /qURL revoke path can edit the recipient's DM in place after a
 * successful revoke.
 *
 * @param {string} userId — Discord snowflake.
 * @param {object} message — discord.js-compatible message payload
 *   (content, embeds, components, etc.). Passed through verbatim to
 *   the REST endpoint.
 */
// Note: DISCORD_TOKEN presence is enforced at boot by
// boot-requirements.js (it's in the bootRequired list for every
// mode). The helpers below trust that guarantee — no defensive
// `if (!config.DISCORD_TOKEN)` short-circuit here. Matches the
// pattern in src/discord.js and http-only-init.js.

async function sendDM(userId, message) {
  try {
    const channel = await client.rest.post(Routes.userChannels(), {
      body: { recipient_id: userId },
    });
    const sent = await client.rest.post(Routes.channelMessages(channel.id), {
      body: message,
    });
    return { ok: true, channelId: channel.id, messageId: sent.id };
  } catch (err) {
    // Discord returns HTTP 403 for several recipient-side reasons:
    // DMs disabled, the bot was blocked, server-level DM restrictions,
    // or "Missing Access" when there's no shared guild. All are
    // expected operational outcomes (not bugs), so log at info level
    // to keep oncall signal-to-noise high. Caller decides whether to
    // fall back to a channel mention.
    const level = err.status === 403 ? 'info' : 'error';
    // `errorMessage` (not `message`) so the log key doesn't shadow the
    // function parameter `message` (the DM body) — readers searching
    // for "message that failed to send" should land on the body, not
    // the error string.
    logger[level]('sendDM via REST failed', {
      userId, status: err.status, errorMessage: err.message,
    });
    return { ok: false, error: err.message, status: err.status };
  }
}

// Discord API codes that are operational outcomes for a DM edit —
// recipient deleted the message, blocked the bot, closed the channel,
// the bot was kicked from a shared guild. Surface as
// `{ ok: false, expected: true }` at info level so they don't pollute
// oncall signal.
//
// Gated on the API code (not bare HTTP status) on purpose: a 403 or
// 404 WITHOUT one of these codes is suspicious — examples include a
// revoked bot token mid-flight (403 with no JSON body), a routing
// proxy returning a synthetic 404, or a Discord-side bug. Logging
// those at warn keeps the unknowns visible. New expected codes belong
// here, not in the predicate.
// Codes plus human-readable descriptions. The description lands in the
// info-level log when it matches, so a future log search for an
// unexpected spike (e.g., 50007 on PATCH — not observed today) reads
// the cause directly rather than requiring an external code lookup.
const DM_EDIT_EXPECTED_API_CODE_DESCRIPTIONS = new Map([
  [10003, 'Unknown Channel — DM channel deleted'],
  [10008, 'Unknown Message — recipient deleted the DM'],
  [50001, 'Missing Access — bot kicked / lost shared guild'],
  // 50007 is observed at POST /messages time (new DM rejected), not
  // typically at PATCH on an existing message in an already-open DM
  // channel. Kept defensively — if it ever fires on PATCH it's still
  // a recipient-side state change, not an oncall surprise. The
  // description tag in the log makes the spike greppable for #369.
  [50007, 'Cannot send messages to this user — DMs disabled / blocked'],
]);

/**
 * Edit a previously-sent DM via REST (no Gateway). Single PATCH —
 * no `channels.fetch` + `messages.fetch` preamble that would be 2
 * extra round-trips on a cold cache.
 *
 * CONTRACT: Discord's PATCH /channels/:cid/messages/:mid does NOT
 * clear unset fields. Callers MUST pass `components: []` explicitly
 * to strip a previous Link button. See buildRevokedDMPayload in
 * commands.js for the contract-bearing payload.
 *
 * No retry on transient 5xx / 429. `client.rest` handles 429 backoff
 * automatically, but a 502/503 from Discord during a revoke fan-out
 * would leave the original Step Through button live in that
 * recipient's DM after the underlying qURL is already DELETEd. The
 * link button now 404s — i.e. the same UX that existed before this
 * feature, just on whatever subset of recipients hit the transient.
 * Accept as a known limitation; the revoke success/total counts are
 * not affected (those track the DELETE, not the edit).
 */
async function editDM(channelId, messageId, message) {
  try {
    await client.rest.patch(Routes.channelMessage(channelId, messageId), { body: message });
    return { ok: true };
  } catch (error) {
    // Map.get(undefined) returns undefined, which falls through to the
    // unexpected (warn-level) branch. Intentional: an error with no
    // API code (e.g., revoked-token 403 with no JSON body, synthetic
    // proxy 404) is exactly the kind of surprise oncall should see.
    const expectedDescription = DM_EDIT_EXPECTED_API_CODE_DESCRIPTIONS.get(error.code);
    const expected = expectedDescription !== undefined;
    const level = expected ? 'info' : 'warn';
    logger[level]('Failed to edit DM', {
      channelId, messageId, status: error.status, code: error.code,
      errorMessage: error.message,
      // Present only on the expected branch — names which entry in
      // DM_EDIT_EXPECTED_API_CODE_DESCRIPTIONS matched. Keeps a future
      // spike in any single expected code (e.g., 50007 on PATCH)
      // greppable from CloudWatch without an external lookup.
      ...(expected && { expectedReason: expectedDescription }),
    });
    // `code` and `reason` exposed on the return so callers' rolled-up
    // logs can carry the same diagnostic without re-grepping the per-
    // edit log line. `code` is undefined for non-Discord-shape errors;
    // `reason` is undefined unless the code matched the expected set.
    // Currently unread by the revoke-loop caller (it consumes only
    // `expected` via the re-thrown error) — landing pad for the
    // dashboard work tracked in #368 / #370 / #372.
    return { ok: false, expected, code: error.code, reason: expectedDescription };
  }
}

/**
 * Post a message to a guild channel via REST (no Gateway).
 *
 * Why REST and not `interaction.channel.send()`: http-only worker
 * mode skips `client.login()`, so no GUILD_CREATE events fire and
 * `client.channels.cache` stays empty. The discord.js
 * `interaction.channel` getter resolves to null on the cache miss and
 * `.send()` throws synchronously. `interaction.channelId` is set
 * straight from the payload, so REST works in both modes.
 *
 * Returns `{ ok: true, messageId }` on success, `{ ok: false, error,
 * status }` on failure. Caller decides whether to surface or swallow.
 */
async function sendChannelMessage(channelId, message) {
  try {
    const sent = await client.rest.post(Routes.channelMessages(channelId), {
      body: message,
    });
    return { ok: true, messageId: sent.id };
  } catch (err) {
    logger.warn('sendChannelMessage via REST failed', {
      channelId, status: err.status, errorMessage: err.message,
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
  try {
    await client.rest.put(Routes.guildMemberRole(guildId, userId, roleId));
    return { ok: true };
  } catch (err) {
    logger.error('addRoleToMember via REST failed', {
      guildId, userId, roleId, status: err.status, errorMessage: err.message,
    });
    return { ok: false, error: err.message, status: err.status };
  }
}

/**
 * Remove a role from a guild member via REST (no Gateway).
 */
async function removeRoleFromMember(guildId, userId, roleId) {
  try {
    await client.rest.delete(Routes.guildMemberRole(guildId, userId, roleId));
    return { ok: true };
  } catch (err) {
    logger.error('removeRoleFromMember via REST failed', {
      guildId, userId, roleId, status: err.status, errorMessage: err.message,
    });
    return { ok: false, error: err.message, status: err.status };
  }
}

/**
 * Edit an interaction's original reply via the interaction WEBHOOK token
 * (no Gateway, no bot-token auth). This is the cross-replica primitive
 * for the sub-second view counter: the sender's `/qurl send`
 * confirmation is an ephemeral interaction reply, editable ONLY via its
 * interaction token (a portable string), so ANY replica that holds the
 * persisted token can PATCH it — the editing replica need NOT be the one
 * that created the reply or the one running the in-memory monitor.
 *
 * Endpoint: PATCH /webhooks/{application_id}/{token}/messages/@original.
 * The token in the URL path IS the auth for webhook routes (the bot
 * token header @discordjs/rest also attaches is ignored here), which is
 * what makes this work from a replica with no relationship to the
 * original interaction.
 *
 * CAVEAT — token TTL: the interaction token expires ~15 min after the
 * interaction was created. Past that, Discord returns 401/404
 * (10015 Unknown Webhook / 50027 Invalid Webhook Token). That's the same
 * ceiling the in-memory monitor already lives under (it caps at 14 min);
 * this helper inherits it, it is not a new limitation.
 *
 * Returns `{ ok: true }` / `{ ok: false, status, code }` — never throws,
 * matching editDM so the webhook fast-path can branch on `.ok` and let
 * the polling backstop cover a transient miss.
 *
 * SECURITY: `token` is a live bearer cred — callers MUST NOT log it.
 */
async function editInteractionReply(applicationId, token, payload) {
  try {
    await client.rest.patch(Routes.webhookMessage(applicationId, token, '@original'), { body: payload });
    return { ok: true };
  } catch (err) {
    // info (not warn) on the expired-token codes — past the ~15-min TTL
    // this is the EXPECTED terminal state for a long-lived qURL, not an
    // anomaly; the counter just freezes, which the monitor cap already
    // accepts. `errorMessage`/`status`/`code` are logged WITHOUT the
    // token (the URL carrying it is never logged).
    const expired = err.code === 10015 || err.code === 50027 || err.status === 401 || err.status === 404;
    logger[expired ? 'info' : 'warn']('editInteractionReply via webhook token failed', {
      applicationId, status: err.status, code: err.code, expired, errorMessage: err.message,
    });
    return { ok: false, status: err.status, code: err.code };
  }
}

// TODO(pr-4d): migrate routes/oauth.js + routes/webhooks.js to call
// these helpers instead of the gateway-cache helpers in src/discord.js.
// `editDM` is consumed by commands.js's revoke path; `sendDM` /
// `addRoleToMember` / `removeRoleFromMember` are landing pads for the
// route migration.
// Helpers read `client.rest.X` directly (rather than capturing
// `client.rest` at module load) so partial test mocks of
// `../src/discord` don't crash require-time. No production consumer
// needs a direct `rest` reference today.
module.exports = {
  sendDM,
  editDM,
  editInteractionReply,
  sendChannelMessage,
  addRoleToMember,
  removeRoleFromMember,
};
