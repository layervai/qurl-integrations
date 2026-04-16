/**
 * Group F — Message content variants: long, markdown, unicode, empty.
 */

import { test, expect } from '../../../fixtures/discord-fixtures';
import { executeSendFlow } from '../../../helpers/send-flow';
import { getNames } from '../../../helpers/test-data-lookup';

test.describe('Message Variants', () => {
  test.beforeEach(async ({ channelPage, env }) => {
    await channelPage.goto(env.DISCORD_GUILD_ID, env.DISCORD_CHANNEL_ID);
  });

  test('send with a long message', async ({ senderPage, env }) => {
    const nameEntry = getNames().find((n) => n.id === 'long-msg')!;

    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/long-message',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: nameEntry.message,
    });

    expect(result.submitted).toBe(true);
    expect(result.errorMessage).toBeUndefined();
  });

  test('send with markdown in message', async ({ senderPage, env }) => {
    const nameEntry = getNames().find((n) => n.id === 'markdown-msg')!;

    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/markdown-msg',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: nameEntry.message,
    });

    expect(result.submitted).toBe(true);
  });

  test('send with URL in message', async ({ senderPage, env }) => {
    const nameEntry = getNames().find((n) => n.id === 'url-in-msg')!;

    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/url-in-message',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: nameEntry.message,
    });

    expect(result.submitted).toBe(true);
  });

  test('send with empty message', async ({ senderPage, env }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/empty-message',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: '',
    });

    expect(result.submitted).toBe(true);
    expect(result.errorMessage).toBeUndefined();
  });

  test('send with minimal single-character message', async ({ senderPage, env }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/minimal-msg',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: '.',
    });

    expect(result.submitted).toBe(true);
  });
});
