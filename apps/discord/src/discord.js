const { Client, GatewayIntentBits } = require('discord.js');
const config = require('./config');
const logger = require('./logger');
const { AUDIT_EVENTS } = require('./constants');

// Required intents — single source of truth. Each feature that depends
// on a specific intent has its own assertion below; if a future refactor
// trims an entry from this array, the corresponding feature-specific
// assertion fails at module load with a clear error pointing at the
// broken feature.
const intents = [
  GatewayIntentBits.Guilds,
  GatewayIntentBits.GuildMembers,
  GatewayIntentBits.GuildVoiceStates,
];

// Per-feature intent canaries. Each line pins one intent to one feature
// and fails loud if the intent has been removed from `intents` above —
// converting silent-feature-break (e.g., role-mention expansion in
// recipient-parser.js silently resolving to an empty member set when
// `GuildMembers` is dropped) into a startup error with a clear cause.
//
// `assertIntent` takes the intents list as its first argument (rather
// than closing over the module-level `intents`) so it's directly
// testable: a unit test can pass an intents-with-one-stripped array and
// verify the throw, locking in the assertion's purpose against a future
// "let's downgrade to a warn" refactor.
function assertIntent(intentsList, bit, requiredFor) {
  // Belt-and-suspenders: in test environments where GatewayIntentBits
  // is partially mocked, `bit` may be undefined; treat undefined as
  // "intent not declared" rather than "intent silently missing."
  if (bit === undefined || !intentsList.includes(bit)) {
    throw new Error(`Missing required Discord intent for ${requiredFor}. Add the intent back to the \`intents\` array in apps/discord/src/discord.js.`);
  }
}
assertIntent(intents, GatewayIntentBits.Guilds, 'guild bootstrap (caches guilds the bot is in)');
assertIntent(intents, GatewayIntentBits.GuildMembers, '/qurl send + /qurl map recipient resolution (members.cache for role-mention expansion + members.fetch for selected-user backfill)');
assertIntent(intents, GatewayIntentBits.GuildVoiceStates, '/qurl send + /qurl map voice-channel-everyone resolution (channel.members for voice-connected snapshot in the confirm card button + <#voice> mention expansion in the recipients string)');

// Negative-intent canaries — pin that the bot has NOT silently
// re-broadened gateway scope. Every intent listed here is either:
//   - privileged (would re-introduce dev-portal toggle dependency):
//     - MessageContent
//     - GuildPresences
//   - non-privileged but only useful for code paths we deleted (the
//     symbol was removed by PR #313 and doesn't exist in the tree —
//     `git log --diff-filter=D` if you need the historical context):
//     - DirectMessages → previously consumed by the deleted handleSend
//       DM file-pivot via `awaitMessages`
//
// `GuildVoiceStates` was previously listed in this negative-canary
// block (dropped by PR #313 when its only consumer —
// `getChannelMembers`'s voice-channel branch — was deleted), and was
// re-added in the voice-everyone restoration: `channel.members` for
// voice/stage channels reads the voice-state cache, which is only
// populated when the intent is declared. The positive assertIntent
// above pins the load-bearing feature.
//
// A future PR that re-adds any of these without a paired assertIntent
// + use-case write-up will fail at boot rather than silently expanding
// what crosses the Discord gateway. See PR #313 / issue #317 for
// context on why this matters operationally (event volume + portal-
// toggle drift).
// `bit === undefined` is INTENTIONALLY permissive here (asymmetric vs
// assertIntent, which is strict on undefined). Rationale: assertIntent
// fails closed because a missing intent silently breaks a known feature;
// assertNoIntent has nothing to defend against when the bit doesn't
// exist (a Discord.js bump that drops an intent name from
// GatewayIntentBits can't have silently re-added it). The test at
// `discord.test.js` ('does not throw when the bit is undefined') pins
// the asymmetry.
function assertNoIntent(intentsList, bit, name) {
  if (bit !== undefined && intentsList.includes(bit)) {
    throw new Error(`Intent \`${name}\` was re-added without justification. If the new use case is legitimate, document it via assertIntent at apps/discord/src/discord.js and remove the assertNoIntent below; otherwise drop the intent from the \`intents\` array.`);
  }
}
assertNoIntent(intents, GatewayIntentBits.MessageContent, 'MessageContent');
assertNoIntent(intents, GatewayIntentBits.GuildPresences, 'GuildPresences');
assertNoIntent(intents, GatewayIntentBits.DirectMessages, 'DirectMessages');

const client = new Client({ intents });

// Bitfield form of the same `intents` array, for the Pillar 2
// @discordjs/ws shim path (which takes a bitfield, not an array).
// Single source of truth: both the legacy Client construction
// above AND the shim subscribe to identical events.
//
// Int32 bitwise OR is safe today — Discord's highest declared
// intent bit (GuildVoiceStates = 128, GuildMessageTyping = 16384,
// AutoModerationConfiguration = 1048576, etc.) is well under
// 2³⁰. If Discord ever adds an intent bit ≥ 2³¹, switch to
// `new IntentsBitField(intents).bitfield` (BigInt-backed) — the
// @discordjs/ws WebSocketManager constructor accepts both number
// and bigint forms.
const GATEWAY_INTENTS_BITFIELD = intents.reduce((acc, bit) => acc | bit, 0);

// Cached handle on the single watched guild (single-guild mode only).
// Populated by refreshCache(); read by verifyBotPermissions() at boot.
// Multi-tenant deployments leave it null — there is no single guild to
// watch — and every reader checks for that.
let guild = null;

// Refresh the cached guild handle. Today's two callers — the `ready`
// handler below and initHttpOnly — are mutually exclusive by process
// role, so nothing actually races; the in-flight coalescing is kept
// because refreshCache is exported, and a future caller that overlaps
// with boot init should get one fetch rather than two.
//
// Return shape: the function resolves to `undefined` regardless of
// mode. In multi-tenant mode it short-circuits immediately (no work,
// no state mutation); in single-guild mode it populates the
// module-level `guild` cache as a side effect, but the resolved value
// is still undefined. All call-sites `await refreshCache()` for
// sequencing and then read the cached state directly — none inspect
// the return value, which is why the side-effect-only contract is safe.
let refreshCacheInFlight = null;
async function refreshCache() {
  // Multi-tenant mode: there is no single watched guild to cache.
  // client.guilds.fetch(null) would return ALL guilds the bot is in as a
  // Collection (not a single Guild), so downstream readers expecting a
  // Guild would crash. Short-circuit to a no-op — all callers already
  // check `if (!guild)` before using cached state, and this function
  // doesn't populate `guild` so those sites skip gracefully.
  if (!config.GUILD_ID) return;

  if (refreshCacheInFlight) return refreshCacheInFlight;
  refreshCacheInFlight = (async () => {
    try {
      guild = await client.guilds.fetch(config.GUILD_ID);
      logger.info('Cache refreshed', { guild: guild?.name });
    } catch (error) {
      logger.error('Failed to refresh cache', { error: error.message });
      // Re-throw so callers that `await refreshCache()` can't then assume
      // `guild` is populated. Previously the error was swallowed here and
      // a downstream `guild.members.fetch()` would crash with an opaque
      // TypeError.
      throw error;
    } finally {
      refreshCacheInFlight = null;
    }
  })();
  return refreshCacheInFlight;
}

// Permissions the bot needs to do its job. A missing permission here means
// a slash command will silently fail at runtime — log loud at boot instead
// so misconfigurations surface immediately. Non-fatal: we still boot in
// case the permission gap is intentional for a staging tenant.
//
// The bot needs exactly 4 perms: ViewChannel so it can see and reply in
// the invoking channel, SendMessages for interaction replies, EmbedLinks
// for qURL link previews, and UseApplicationCommands for slash commands.
// That set matches the invite bitmask, so a guild that accepted the
// invite has already granted everything the bot exercises.
async function verifyBotPermissions() {
  try {
    const { PermissionFlagsBits } = require('discord.js');
    const me = await guild.members.fetchMe();
    const required = {
      ViewChannel: PermissionFlagsBits.ViewChannel,
      SendMessages: PermissionFlagsBits.SendMessages,
      EmbedLinks: PermissionFlagsBits.EmbedLinks,
      UseApplicationCommands: PermissionFlagsBits.UseApplicationCommands,
    };
    const missing = Object.entries(required)
      .filter(([, bit]) => !me.permissions.has(bit))
      .map(([name]) => name);
    if (missing.length > 0) {
      logger.error('Bot is missing required Discord permissions in guild', {
        guild: guild?.name, missing,
      });
    } else {
      logger.info('Bot permissions OK', { guild: guild?.name });
    }
  } catch (err) {
    logger.warn('Could not verify bot permissions at boot', { error: err.message });
  }
}

client.once('ready', async () => {
  logger.info(`Discord bot logged in as ${client.user.tag}`);

  // Multi-tenant mode: GUILD_ID unset means no single "watched" guild.
  // refreshCache() and verifyBotPermissions() are both single-guild
  // operations (the first fetches one guild, the second checks perms in
  // it), so both are dormant here.
  if (!config.GUILD_ID) {
    logger.info('Multi-tenant mode: GUILD_ID unset — skipping single-guild cache init.');
    logger.info('Bot is ready. /qurl commands will appear in any guild the bot joins.');
    return;
  }

  await refreshCache();
  await verifyBotPermissions();
  logger.info(`Watching guild: ${guild?.name}`);
});

// guildCreate also fires on every shard ready burst (Discord re-sends
// for every guild the bot is already in). Tag replays via !isReady() so
// the install dashboard widget can filter to genuine installs.
//
// KNOWN LIMITATION (#195): discord.js v14's `Client.isReady()` does
// NOT flip back to false on a session resume, so a forced re-IDENTIFY
// (CLOSE code 4xxx) replays every cached guild with `replay: false`,
// producing a fake install spike. Phase 2 fix uses a settle window
// driven by shardReady/shardResume; see #195.
client.on('guildCreate', (guild) => {
  try {
    const replay = !client.isReady();
    logger.audit(AUDIT_EVENTS.GUILD_INSTALL, {
      guild_id: guild?.id,
      member_count: guild?.memberCount,
      replay,
    });
  } catch (error) {
    logger.error('Error handling guildCreate event', { error: error?.message });
  }
});

client.on('guildDelete', (guild) => {
  try {
    logger.audit(AUDIT_EVENTS.GUILD_UNINSTALL, {
      guild_id: guild?.id,
    });
  } catch (error) {
    logger.error('Error handling guildDelete event', { error: error?.message });
  }
});

// Returns `{ ok, channelId, messageId }`. The refs feed the /qURL
// revoke path (see buildRevokedDMPayload + editDM in discord-rest.js)
// so it can edit the recipient's DM in place after a successful
// revoke — that is the send path in commands.js, which awaits and
// persists the result. The qURL OAuth callback (routes/qurl-oauth.js)
// is fire-and-forget and can keep discarding the return value.
async function sendDM(discordId, message) {
  try {
    const user = await client.users.fetch(discordId);
    const sent = await user.send(message);
    logger.debug('Sent DM', { discordId });
    return { ok: true, channelId: sent.channelId, messageId: sent.id };
  } catch (error) {
    logger.warn(`Failed to DM user ${discordId}`, { error: error.message });
    return { ok: false };
  }
}

// Graceful shutdown — awaits client.destroy() so the caller knows the
// WebSocket is fully closed and no further events will fire. discord.js
// v14 returns a Promise from destroy(); swallowing it risked dropped
// messages during ECS rolling deploys.
async function shutdown() {
  logger.info('Discord client shutting down');
  try {
    await client.destroy();
  } catch (err) {
    logger.warn('client.destroy() threw during shutdown (continuing)', { error: err?.message });
  }
}

module.exports = {
  client,
  GATEWAY_INTENTS_BITFIELD,
  sendDM,
  refreshCache,
  shutdown,
  // Exported only for unit tests to verify the boot canaries throw
  // on a missing intent (assertIntent) or a re-broadened intents array
  // (assertNoIntent). Production callers don't need them — the module-
  // level invocations above are the load-bearing assertions.
  assertIntent,
  assertNoIntent,
  // Test-only today, and the http tier depends on it staying that way.
  // `guild` is refreshed exactly once, at boot — http-only replicas run
  // no periodic refresh because no request path reads this handle (see
  // the module header in src/http-only-init.js). A request-time consumer
  // added here would silently reintroduce the staleness window that
  // refresh used to cover, so give it its own refresh path rather than
  // reading the boot-time snapshot.
  getGuild: () => guild,
};
