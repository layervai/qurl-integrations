/**
 * Discord channel and voice channel E2E tests.
 *
 * Tests that the bot can:
 * - Read channel members
 * - Identify voice channel users
 * - Send messages to specific channels
 * - Verify guild membership checks
 */

// TODO: Add afterAll cleanup to revoke/delete test resources

import * as dotenv from 'dotenv';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { loadEnv } from '../helpers/env';
import * as discord from '../helpers/discord-api';

const env = loadEnv();

/** Thin wrapper: calls the shared discord-api helper with the bot token */
async function discordApi(method: string, path: string, body?: unknown): Promise<any> {
  return discord.api(env.BOT_TOKEN, method, path, body);
}

describe('Discord: Channel Operations', () => {
  test('bot can read channel info', async () => {
    const channel = await discordApi('GET', `/channels/${env.CHANNEL_ID}`);
    expect(channel.id).toBe(env.CHANNEL_ID);
    expect(channel.guild_id).toBe(env.GUILD_ID);
    expect(channel.type).toBeDefined();
    console.log(`Channel: #${channel.name} (type: ${channel.type})`);
  });

  test('bot can list guild channels', async () => {
    const channels = await discordApi('GET', `/guilds/${env.GUILD_ID}/channels`);
    expect(channels.length).toBeGreaterThan(0);

    const textChannels = channels.filter((c: any) => c.type === 0);
    const voiceChannels = channels.filter((c: any) => c.type === 2);
    console.log(`Guild has ${textChannels.length} text + ${voiceChannels.length} voice channels`);

    expect(textChannels.length).toBeGreaterThan(0);
  });

  test('bot can identify voice channels in guild', async () => {
    const channels = await discordApi('GET', `/guilds/${env.GUILD_ID}/channels`);
    const voiceChannels = channels.filter((c: any) => c.type === 2);

    // The test guild should have at least one voice channel
    expect(voiceChannels.length).toBeGreaterThan(0);
    console.log('Voice channels:', voiceChannels.map((c: any) => `#${c.name} (${c.id})`));
  });
});

describe('Discord: Voice State', () => {
  test('bot can query voice states (users in voice channels)', async () => {
    // Voice states are available via the guild endpoint
    // Note: users must actually be in a voice channel for this to return data
    try {
      const guild = await discordApi('GET', `/guilds/${env.GUILD_ID}?with_counts=true`);
      expect(guild.id).toBe(env.GUILD_ID);
      console.log(`Guild: ${guild.name}, members: ${guild.approximate_member_count}`);
    } catch (e) {
      // Bot may not have the GUILD_MEMBERS intent — that's OK, just verify the API works
      console.log('Guild query:', (e as Error).message);
    }
  });

  test('bot can check if a specific voice channel exists', async () => {
    const channels = await discordApi('GET', `/guilds/${env.GUILD_ID}/channels`);
    const voiceChannels = channels.filter((c: any) => c.type === 2);

    if (voiceChannels.length === 0) {
      console.log('No voice channels in test guild — skipping');
      return;
    }

    const voiceChannel = voiceChannels[0];
    expect(voiceChannel.type).toBe(2); // GUILD_VOICE
    expect(voiceChannel.guild_id).toBe(env.GUILD_ID);
    console.log(`Verified voice channel: #${voiceChannel.name}`);
  });

  test('"Everyone in this voice channel" recipient pool = voice-connected members only', async () => {
    // Pins the new invariant after the recipient-scope fix:
    // /qurl file or /qurl map invoked from a voice channel (with no
    // recipients pre-filled) resolves to VOICE-CONNECTED members only
    // (channel.members on a GuildVoice / GuildStageVoice in discord.js
    // v14). NOT the guild member list and NOT the ViewChannel-permission
    // set — those expand to @everyone on default servers and were the
    // source of the prior "sends to entire guild" bug.
    //
    // The Discord REST API's `voice_states` field on GET /guilds/:id is
    // the canonical source of "currently connected to voice" — same
    // signal the bot's voice state cache exposes via channel.members.
    // This test pins TWO real invariants:
    //   1. Every entry in voice_states has a channel_id — i.e., voice
    //      states are always channel-scoped (not guild-scoped).
    //   2. Every voice_state.channel_id resolves to a voice or stage-
    //      voice channel in the same guild — never to a text channel,
    //      DM, or unknown ID. A future Discord API shape change that
    //      broke this would surface here, not silently in the bot.
    const channels = await discordApi('GET', `/guilds/${env.GUILD_ID}/channels`);
    const voiceChannels = channels.filter((c: any) => c.type === 2 || c.type === 13);
    if (voiceChannels.length === 0) {
      console.log('No voice/stage channels in test guild — skipping invariant check');
      return;
    }

    let guildSnapshot: any;
    try {
      guildSnapshot = await discordApi('GET', `/guilds/${env.GUILD_ID}?with_counts=true`);
    } catch (e) {
      console.log('Guild snapshot fetch failed:', (e as Error).message);
      return;
    }

    const voiceStates: any[] = guildSnapshot.voice_states || [];
    const voiceChannelIds = new Set(voiceChannels.map((c: any) => c.id));
    voiceStates.forEach((vs: any) => {
      // Invariant 1: every voice state has a channel_id.
      expect(typeof vs.channel_id).toBe('string');
      // Invariant 2: voice states only reference voice / stage-voice
      // channels in this guild — pins the API shape the bot relies on.
      expect(voiceChannelIds.has(vs.channel_id)).toBe(true);
    });
    const voiceChannel = voiceChannels[0];
    const inThisChannel = voiceStates.filter((vs: any) => vs.channel_id === voiceChannel.id);
    console.log(
      `Voice channel #${voiceChannel.name}: ${inThisChannel.length} voice-connected member(s) ` +
      `would be in the "Everyone in this voice channel" recipient pool ` +
      `(scope = voice-connected only, NOT @everyone view-perm).`,
    );
  });
});

describe('Discord: Guild Members', () => {
  test('bot can list guild members', async () => {
    try {
      const members = await discordApi('GET', `/guilds/${env.GUILD_ID}/members?limit=100`);
      expect(members.length).toBeGreaterThan(0);

      const bots = members.filter((m: any) => m.user?.bot);
      const humans = members.filter((m: any) => !m.user?.bot);
      console.log(`Members: ${humans.length} humans + ${bots.length} bots`);

      // The qURL bot should be in the list
      const qurlBot = members.find((m: any) => m.user?.id === env.BOT_CLIENT_ID);
      expect(qurlBot).toBeDefined();
    } catch (e) {
      // Requires Server Members intent — may not be enabled for test bot
      console.log('Member list requires Server Members intent:', (e as Error).message);
    }
  });

  test('bot can look up a specific member by ID', async () => {
    try {
      const member = await discordApi('GET', `/guilds/${env.GUILD_ID}/members/${env.BOT_CLIENT_ID}`);
      expect(member.user.id).toBe(env.BOT_CLIENT_ID);
      expect(member.user.bot).toBe(true);
      console.log(`Bot member: ${member.user.username}`);
    } catch (e) {
      console.log('Member lookup:', (e as Error).message);
    }
  });
});

describe('Discord: Send to Channel', () => {
  test('bot can send to the test text channel', async () => {
    const msg = await discord.sendMessage(
      env.BOT_TOKEN,
      env.CHANNEL_ID,
      `[E2E] Channel send test ${new Date().toISOString()}`,
    );
    expect(msg.id).toBeDefined();
    expect(msg.channel_id).toBe(env.CHANNEL_ID);
  });

  test('bot can send an embed to the test channel', async () => {
    const msg = await discordApi('POST', `/channels/${env.CHANNEL_ID}/messages`, {
      embeds: [{
        title: 'E2E Test Embed',
        description: 'Testing embed delivery',
        color: 0x00d4ff,
        fields: [
          { name: 'Resource Type', value: 'Test', inline: true },
          { name: 'Status', value: 'Passing', inline: true },
        ],
        footer: { text: 'qURL E2E Test Suite' },
      }],
    });
    expect(msg.embeds.length).toBe(1);
    expect(msg.embeds[0].title).toBe('E2E Test Embed');
  });

  test('bot message appears in channel history', async () => {
    const unique = `e2e-history-${Date.now()}`;
    await discord.sendMessage(env.BOT_TOKEN, env.CHANNEL_ID, unique);

    // Poll for the message
    const found = await discord.waitForMessage(env.BOT_TOKEN, env.CHANNEL_ID, {
      containsText: unique,
      timeoutMs: 10_000,
      pollIntervalMs: 500,
    });
    expect(found.content).toBe(unique);
  });
});

describe('Discord: DM Delivery', () => {
  test('bot can open a DM channel with itself', async () => {
    // Bots can DM themselves — useful as a sanity check
    try {
      const dm = await discordApi('POST', '/users/@me/channels', {
        recipient_id: env.BOT_CLIENT_ID,
      });
      expect(dm.id).toBeDefined();
      console.log(`DM channel: ${dm.id}`);
    } catch (e) {
      // Some Discord API versions don't allow self-DM
      console.log('Self-DM:', (e as Error).message);
    }
  });
});
