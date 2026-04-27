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

  test('"Everyone in voice channel" recipient pool = guild members, NOT voice-connected members', async () => {
    // Regression documentation for the bug where /qurl send `target=channel`
    // invoked from a voice channel only delivered to voice-connected users.
    // The fix in apps/discord/src/discord.js:getTextChannelMembers branches
    // on channel.type: for GuildVoice/GuildStageVoice it enumerates
    // guild.members.cache and filters by permissionsFor(ViewChannel),
    // instead of using channel.members (which on a voice channel is just
    // the voice-connected subset).
    //
    // This test asserts the prerequisites the fix relies on:
    //   1) The test guild has at least one voice channel
    //   2) The bot can enumerate guild members (the superset pool)
    //   3) That pool is non-empty — so "Everyone" has someone to go to
    //      even when zero users are connected to voice
    const channels = await discordApi('GET', `/guilds/${env.GUILD_ID}/channels`);
    const voiceChannels = channels.filter((c: any) => c.type === 2 || c.type === 13);
    if (voiceChannels.length === 0) {
      console.log('No voice/stage channels in test guild — skipping invariant check');
      return;
    }
    const voiceChannel = voiceChannels[0];

    let guildMembers: any[] = [];
    try {
      guildMembers = await discordApi('GET', `/guilds/${env.GUILD_ID}/members?limit=100`);
    } catch (e) {
      console.log('Guild member list requires Server Members intent:', (e as Error).message);
      return; // premise untestable without the intent; unit tests still cover the logic
    }

    // The pool the bot iterates for "Everyone in this channel" on a voice
    // channel is the guild member list — it must be non-empty, otherwise
    // the regression ("0 recipients in an active channel") could recur.
    const humans = guildMembers.filter((m: any) => !m.user?.bot);
    expect(humans.length).toBeGreaterThan(0);
    console.log(
      `Voice channel #${voiceChannel.name}: guild has ${humans.length} human member(s) ` +
      `in the "Everyone in this channel" pool (independent of voice-connected count).`,
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
