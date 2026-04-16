/**
 * Group E — Expiry duration tests.
 */

import { test, expect } from '../../../fixtures/discord-fixtures';
import { executeSendFlow } from '../../../helpers/send-flow';
import { openQurlLink } from '../../../helpers/qurl-link';

test.describe('Expiry Variants', () => {
  test.beforeEach(async ({ channelPage, env }) => {
    await channelPage.goto(env.DISCORD_GUILD_ID, env.DISCORD_CHANNEL_ID);
  });

  const expiryDurations = ['5m', '15m', '1h', '6h', '24h', '7d', '30d'];

  for (const expiry of expiryDurations) {
    test(`send with expiry ${expiry}`, async ({ senderPage, env }) => {
      const result = await executeSendFlow(senderPage, {
        location: `https://example.com/expiry-${expiry}`,
        recipient: env.DISCORD_RECIPIENT_USERNAME,
        expiry,
        message: `Expiry test: ${expiry}`,
      });

      expect(result.submitted).toBe(true);
      expect(result.errorMessage).toBeUndefined();
      expect(result.qurlLink).toBeDefined();
    });
  }

  test('link with short expiry is accessible immediately', async ({
    senderPage,
    env,
    browser,
  }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/short-expiry-access',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      expiry: '5m',
      message: 'Short-lived link',
    });

    expect(result.qurlLink).toBeDefined();

    // Verify the link works right after creation
    const linkResult = await openQurlLink(browser, result.qurlLink!);
    expect(linkResult.ok).toBe(true);
    expect(linkResult.isExpired).toBe(false);
  });

  test('send without expiry uses default', async ({ senderPage, env }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/default-expiry',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'No explicit expiry',
    });

    expect(result.submitted).toBe(true);
    expect(result.errorMessage).toBeUndefined();
  });
});
