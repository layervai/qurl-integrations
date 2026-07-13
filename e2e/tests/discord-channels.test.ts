/**
 * Discord channel and voice channel E2E tests.
 *
 * Tests that the bot can:
 * - Read channel members
 * - Identify voice channel users
 * - Send messages to specific channels
 * - Verify guild membership checks
 *
 * Every message this suite posts is tracked and best-effort deleted in
 * afterAll (helpers/cleanup.ts) so repeated runs don't pile noise into
 * the shared test channel.
 */

import * as dotenv from 'dotenv';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { trackedDiscordMessages } from '../helpers/cleanup';
import { loadEnv } from '../helpers/env';
import * as discord from '../helpers/discord-api';

const env = loadEnv();
const sentMessages = trackedDiscordMessages(env);

afterAll(() => sentMessages.deleteAll());

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
  test('bot can query guild metadata with member counts', async () => {
    // GET /guilds/{id} needs no privileged intent — only guild
    // membership, which every other test here already depends on, so
    // this asserts hard (the old try/catch "may not have the intent"
    // guard swallowed every failure, including its own expects). Real
    // voice-STATE assertions would need a user actually connected to a
    // voice channel, which an unattended run can't stage; the
    // voice-channel structure checks below cover the REST surface the
    // bot's voice-everyone resolution reads.
    const guild = await discordApi('GET', `/guilds/${env.GUILD_ID}?with_counts=true`);
    expect(guild.id).toBe(env.GUILD_ID);
    expect(guild.approximate_member_count).toBeGreaterThanOrEqual(1);
    console.log(`Guild: ${guild.name}, members: ${guild.approximate_member_count}`);
  });

  test('bot can check if a specific voice channel exists', async () => {
    const channels = await discordApi('GET', `/guilds/${env.GUILD_ID}/channels`);
    const voiceChannels = channels.filter((c: any) => c.type === 2);

    // Hard requirement, not a soft-skip: the sibling test above already
    // pins that the test guild has at least one voice channel, so
    // silently returning here could only hide that same failure.
    expect(voiceChannels.length).toBeGreaterThan(0);

    const voiceChannel = voiceChannels[0];
    expect(voiceChannel.type).toBe(2); // GUILD_VOICE
    expect(voiceChannel.guild_id).toBe(env.GUILD_ID);
    console.log(`Verified voice channel: #${voiceChannel.name}`);
  });
});

describe('Discord: Guild Members', () => {
  // Both tests assert HARD. The production bot requires the
  // GUILD_MEMBERS privileged intent — apps/discord/src/discord.js
  // declares it with a load-time canary because /qurl send recipient
  // resolution reads members.cache — and `GET /guilds/{id}/members` is
  // REST-gated on that same developer-portal toggle. So if the list
  // call 403s here, the portal toggle is off and the bot itself is
  // broken in this environment; that must fail the suite loudly, not
  // get logged away (the old try/catch swallowed its own expects too).
  test('bot can list guild members', async () => {
    const members = await discordApi('GET', `/guilds/${env.GUILD_ID}/members?limit=100`);
    expect(members.length).toBeGreaterThan(0);

    const bots = members.filter((m: any) => m.user?.bot);
    const humans = members.filter((m: any) => !m.user?.bot);
    console.log(`Members: ${humans.length} humans + ${bots.length} bots`);

    // The qURL bot should be in the list
    const qurlBot = members.find((m: any) => m.user?.id === env.BOT_CLIENT_ID);
    expect(qurlBot).toBeDefined();
  });

  test('bot can look up a specific member by ID', async () => {
    // Get Guild Member (single) is not intent-gated, and the bot looks
    // up ITSELF — a member by definition — so this can never
    // legitimately fail on a healthy deployment.
    const member = await discordApi('GET', `/guilds/${env.GUILD_ID}/members/${env.BOT_CLIENT_ID}`);
    expect(member.user.id).toBe(env.BOT_CLIENT_ID);
    expect(member.user.bot).toBe(true);
    console.log(`Bot member: ${member.user.username}`);
  });
});

describe('Discord: Send to Channel', () => {
  test('bot can send to the test text channel', async () => {
    const msg = await discord.sendMessage(
      env.BOT_TOKEN,
      env.CHANNEL_ID,
      `[E2E] Channel send test ${new Date().toISOString()}`,
    );
    sentMessages.track(msg);
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
    sentMessages.track(msg);
    expect(msg.embeds.length).toBe(1);
    expect(msg.embeds[0].title).toBe('E2E Test Embed');
  });

  test('bot message appears in channel history', async () => {
    const unique = `e2e-history-${Date.now()}`;
    const msg = await discord.sendMessage(env.BOT_TOKEN, env.CHANNEL_ID, unique);
    sentMessages.track(msg);

    // Poll for the message (author-scoped to the bot's own send)
    const found = await discord.waitForMessage(env.BOT_TOKEN, env.CHANNEL_ID, {
      fromAuthorId: env.BOT_CLIENT_ID,
      containsText: unique,
      timeoutMs: 10_000,
      pollIntervalMs: 500,
    });
    expect(found.content).toBe(unique);
  });
});

describe('Discord: DM Delivery', () => {
  test('bot can open a DM channel with a guild member', async () => {
    // The bot's real DM surface (recipient notifications) starts with
    // Create DM, so exercise that against the guild OWNER — a user the
    // bot provably shares a guild with. Create DM only opens the channel
    // object; it does NOT send a message or notify anyone. (The previous
    // self-DM version was undefined behavior in the Discord API and
    // swallowed its failures, so it could never fail — or prove
    // anything.)
    const guild = await discordApi('GET', `/guilds/${env.GUILD_ID}`);
    expect(guild.owner_id).toBeDefined();

    const dm = await discord.getDMChannel(env.BOT_TOKEN, guild.owner_id);
    expect(dm.id).toMatch(/^\d+$/); // a real channel snowflake
    console.log(`DM channel with guild owner: ${dm.id}`);
  });
});
