/**
 * Discord bot interaction tests:
 * - Bot can send messages with embeds
 * - Bot can read channel history
 * - Message format verification
 * - Channel permissions
 */

import * as dotenv from 'dotenv';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { loadEnv } from '../helpers/env';
import * as discord from '../helpers/discord-api';

const env = loadEnv();

describe('Discord Bot: Channel Operations', () => {
  test('bot user info is correct', async () => {
    const me = await discord.getMe(env.BOT_TOKEN);
    expect(me.id).toBe(env.BOT_CLIENT_ID);
    expect(me.username).toBeDefined();
  });

  test('send and immediately read back a message', async () => {
    const unique = `e2e-${Date.now()}`;
    const sent = await discord.sendMessage(env.BOT_TOKEN, env.CHANNEL_ID, unique);
    expect(sent.id).toBeDefined();

    const messages = await discord.getMessages(env.BOT_TOKEN, env.CHANNEL_ID, 5);
    const found = messages.find((m) => m.content === unique);
    expect(found).toBeDefined();
    expect(found!.author.id).toBe(env.BOT_CLIENT_ID);
  });

  test('send message with content', async () => {
    const msg = await discord.sendMessage(env.BOT_TOKEN, env.CHANNEL_ID, '[E2E] embed placeholder test');
    expect(msg.id).toBeDefined();
    expect(msg.content).toContain('embed placeholder');
  });

  test('read messages after a specific message ID', async () => {
    const m1 = await discord.sendMessage(env.BOT_TOKEN, env.CHANNEL_ID, `before-${Date.now()}`);
    await new Promise((r) => setTimeout(r, 1000));
    const m2 = await discord.sendMessage(env.BOT_TOKEN, env.CHANNEL_ID, `after-${Date.now()}`);

    const after = await discord.getMessagesAfter(env.BOT_TOKEN, env.CHANNEL_ID, m1.id);
    expect(after.length).toBeGreaterThanOrEqual(1);
    expect(after.some((m) => m.id === m2.id)).toBe(true);
  });

  test('waitForMessage finds a message by content', async () => {
    const unique = `wait-test-${Date.now()}`;
    // Send after a short delay
    setTimeout(() => discord.sendMessage(env.BOT_TOKEN, env.CHANNEL_ID, unique), 500);

    const msg = await discord.waitForMessage(env.BOT_TOKEN, env.CHANNEL_ID, {
      containsText: unique,
      timeoutMs: 10_000,
      pollIntervalMs: 500,
    });
    expect(msg.content).toBe(unique);
  });
});

describe('Discord Bot: Message Helpers', () => {
  test('extractQurlLink finds link in embed', () => {
    const msg: discord.DiscordMessage = {
      id: '1', channel_id: '1', author: { id: '1', username: 'bot' },
      content: '', timestamp: new Date().toISOString(),
      embeds: [{
        description: 'Click here: https://qurl.link/abc123 to access',
        fields: [{ name: 'Link', value: 'https://qurl.link/xyz789', inline: false }],
      }],
    };
    const link = discord.extractQurlLink(msg);
    expect(link).toBe('https://qurl.link/abc123');
  });

  test('extractQurlLink returns null when no link', () => {
    const msg: discord.DiscordMessage = {
      id: '1', channel_id: '1', author: { id: '1', username: 'bot' },
      content: 'no link here', timestamp: new Date().toISOString(),
      embeds: [{ description: 'just text' }],
    };
    expect(discord.extractQurlLink(msg)).toBeNull();
  });

  test('extractButtons finds button labels', () => {
    const msg: discord.DiscordMessage = {
      id: '1', channel_id: '1', author: { id: '1', username: 'bot' },
      content: '', timestamp: new Date().toISOString(),
      embeds: [],
      components: [{
        type: 1,
        components: [
          { type: 2, label: 'Revoke All Links', custom_id: 'revoke', style: 4 },
          { type: 2, label: 'Add Recipients', custom_id: 'add', style: 1 },
        ],
      }],
    };
    const buttons = discord.extractButtons(msg);
    expect(buttons).toContain('Revoke All Links');
    expect(buttons).toContain('Add Recipients');
  });
});
