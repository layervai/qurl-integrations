/**
 * Discord bot interaction tests:
 * - Bot can send messages with embeds
 * - Bot can read channel history
 * - Message format verification
 * - Channel permissions
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

describe('Discord Bot: Channel Operations', () => {
  test('bot user info is correct', async () => {
    const me = await discord.getMe(env.BOT_TOKEN);
    expect(me.id).toBe(env.BOT_CLIENT_ID);
    expect(me.username).toBeDefined();
  });

  test('send and immediately read back a message', async () => {
    const unique = `e2e-${Date.now()}`;
    const sent = await discord.sendMessage(env.BOT_TOKEN, env.CHANNEL_ID, unique);
    sentMessages.track(sent);
    expect(sent.id).toBeDefined();

    const messages = await discord.getMessages(env.BOT_TOKEN, env.CHANNEL_ID, 5);
    const found = messages.find((m) => m.content === unique);
    expect(found).toBeDefined();
    expect(found!.author.id).toBe(env.BOT_CLIENT_ID);
  });

  test('send message with content', async () => {
    const msg = await discord.sendMessage(env.BOT_TOKEN, env.CHANNEL_ID, '[E2E] embed placeholder test');
    sentMessages.track(msg);
    expect(msg.id).toBeDefined();
    expect(msg.content).toContain('embed placeholder');
  });

  test('read messages after a specific message ID', async () => {
    const m1 = await discord.sendMessage(env.BOT_TOKEN, env.CHANNEL_ID, `before-${Date.now()}`);
    sentMessages.track(m1);
    const m2Content = `after-${Date.now()}`;
    const m2 = await discord.sendMessage(env.BOT_TOKEN, env.CHANNEL_ID, m2Content);
    sentMessages.track(m2);

    // waitForMessage with afterMessageId drives getMessagesAfter under
    // the hood (helpers/discord-api.ts), polling until m2 is visible in
    // channel history — replacing the previous undocumented bare 1s
    // sleep between the sends with an explicit, bounded wait that
    // tolerates history-read lag.
    const found = await discord.waitForMessage(env.BOT_TOKEN, env.CHANNEL_ID, {
      afterMessageId: m1.id,
      containsText: m2Content,
      timeoutMs: 10_000,
      pollIntervalMs: 500,
    });
    expect(found.id).toBe(m2.id);
  });

  test('waitForMessage finds a message by content', async () => {
    const unique = `wait-test-${Date.now()}`;
    // Race the delayed send against the poll so the message lands while
    // waitForMessage is already polling — exercising the poll loop, not
    // just its first read. allSettled (instead of the previous floating
    // `setTimeout(() => discord.sendMessage(...))` with no .catch) so
    // BOTH arms always run to completion: a send failure surfaces as the
    // root-cause error (not an unhandled rejection plus a baffling 10s
    // poll timeout), no poll keeps running past the test ("Cannot log
    // after tests are done"), and the send arm's track() always executes
    // so a sent message is cleaned up even when the poll arm fails.
    const [pollRes, sendRes] = await Promise.allSettled([
      discord.waitForMessage(env.BOT_TOKEN, env.CHANNEL_ID, {
        // Author-scoped: the unique text already makes cross-author
        // collisions implausible, but the poll only ever wants the
        // bot's own send.
        fromAuthorId: env.BOT_CLIENT_ID,
        containsText: unique,
        timeoutMs: 10_000,
        pollIntervalMs: 500,
      }),
      (async () => {
        await new Promise((r) => setTimeout(r, 500));
        const msg = await discord.sendMessage(env.BOT_TOKEN, env.CHANNEL_ID, unique);
        sentMessages.track(msg);
        return msg;
      })(),
    ]);
    // Send failure first: if the send never landed, the poll timeout is
    // just its symptom.
    if (sendRes.status === 'rejected') throw sendRes.reason;
    if (pollRes.status === 'rejected') throw pollRes.reason;
    expect(pollRes.value.content).toBe(unique);
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
