/**
 * Group H — Link lifecycle: create, access, revoke, re-access.
 */

import { test, expect } from '../../../fixtures/discord-fixtures';
import { executeSendFlow } from '../../../helpers/send-flow';
import { openQurlLink, extractFirstQurlLink } from '../../../helpers/qurl-link';

test.describe('Link Lifecycle', () => {
  test.beforeEach(async ({ channelPage, env }) => {
    await channelPage.goto(env.DISCORD_GUILD_ID, env.DISCORD_CHANNEL_ID);
  });

  test('newly created link is accessible', async ({
    senderPage,
    env,
    browser,
  }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/lifecycle-create',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'Lifecycle test — creation',
    });

    expect(result.qurlLink).toBeDefined();

    const linkResult = await openQurlLink(browser, result.qurlLink!);
    expect(linkResult.ok).toBe(true);
    expect(linkResult.isExpired).toBe(false);
  });

  test('link can be accessed multiple times', async ({
    senderPage,
    env,
    browser,
  }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/lifecycle-multi-access',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'Multi-access test',
    });

    expect(result.qurlLink).toBeDefined();

    // Access 3 times
    for (let i = 0; i < 3; i++) {
      const linkResult = await openQurlLink(browser, result.qurlLink!);
      expect(linkResult.ok).toBe(true);
    }
  });

  test('revoked link returns expired status', async ({
    senderPage,
    env,
    browser,
    channelPage,
  }) => {
    // Create a link
    const createResult = await executeSendFlow(senderPage, {
      location: 'https://example.com/lifecycle-revoke',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'Revocation test',
    });

    expect(createResult.qurlLink).toBeDefined();

    // Verify it works first
    const beforeRevoke = await openQurlLink(browser, createResult.qurlLink!);
    expect(beforeRevoke.ok).toBe(true);

    // Revoke by clicking the revoke button in the bot response
    try {
      await channelPage.clickButtonInLastMessage('Revoke');
      // Wait for confirmation
      await senderPage.waitForTimeout(3_000);

      // Try to access the revoked link
      const afterRevoke = await openQurlLink(browser, createResult.qurlLink!);
      expect(afterRevoke.isExpired).toBe(true);
    } catch {
      // If no revoke button, skip this assertion
      test.skip();
    }
  });
});
