/**
 * Group G — Negative / error cases: invalid inputs, missing fields, bad URLs.
 */

import { test, expect } from '../../../fixtures/discord-fixtures';
import { executeSendFlow } from '../../../helpers/send-flow';

test.describe('Negative Cases', () => {
  test.beforeEach(async ({ channelPage, env }) => {
    await channelPage.goto(env.DISCORD_GUILD_ID, env.DISCORD_CHANNEL_ID);
  });

  test('send without location shows error', async ({ senderPage, env }) => {
    const result = await executeSendFlow(senderPage, {
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'No location provided',
    });

    // Should fail or show an error — location is required
    // The command might not even submit without a location
    expect(result.errorMessage || !result.submitted).toBeTruthy();
  });

  test('send with invalid URL shows error', async ({ senderPage, env }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'not-a-valid-url',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'Invalid URL test',
    });

    expect(result.errorMessage).toBeDefined();
  });

  test('send with non-existent recipient shows error', async ({ senderPage }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/bad-recipient',
      recipient: 'nonexistent_user_12345_xyz',
      message: 'Bad recipient test',
    });

    // Autocomplete won't find the user, so flow may fail
    expect(result.errorMessage || !result.qurlLink).toBeTruthy();
  });

  test('send with invalid expiry format', async ({ senderPage, env }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/bad-expiry',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      expiry: 'invalid-duration',
      message: 'Bad expiry test',
    });

    expect(result.errorMessage).toBeDefined();
  });

  test('send with extremely long URL', async ({ senderPage, env }) => {
    const longUrl = 'https://example.com/' + 'a'.repeat(2000);
    const result = await executeSendFlow(senderPage, {
      location: longUrl,
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'Extremely long URL',
    });

    // Should either succeed or show a meaningful error
    expect(result.submitted || result.errorMessage).toBeTruthy();
  });

  test('send with javascript: URL is rejected', async ({ senderPage, env }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'javascript:alert(1)',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'XSS attempt',
    });

    expect(result.errorMessage).toBeDefined();
  });
});
