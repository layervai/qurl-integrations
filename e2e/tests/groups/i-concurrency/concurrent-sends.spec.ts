/**
 * Group I — Concurrency: multiple sends in rapid succession.
 */

import { test, expect } from '../../../fixtures/discord-fixtures';
import { executeSendFlow } from '../../../helpers/send-flow';

test.describe('Concurrent Sends', () => {
  test.beforeEach(async ({ channelPage, env }) => {
    await channelPage.goto(env.DISCORD_GUILD_ID, env.DISCORD_CHANNEL_ID);
  });

  test('send multiple links sequentially without errors', async ({
    senderPage,
    env,
  }) => {
    const results = [];

    for (let i = 0; i < 3; i++) {
      const result = await executeSendFlow(senderPage, {
        location: `https://example.com/concurrent-${i}`,
        recipient: env.DISCORD_RECIPIENT_USERNAME,
        message: `Concurrent test #${i}`,
      });
      results.push(result);

      // Small pause between sends to let Discord process
      await senderPage.waitForTimeout(2_000);
    }

    // All should succeed
    for (const result of results) {
      expect(result.submitted).toBe(true);
      expect(result.errorMessage).toBeUndefined();
    }
  });

  test('rapid sends do not cause command collision', async ({
    senderPage,
    env,
  }) => {
    // Send two commands with minimal delay
    const result1 = await executeSendFlow(senderPage, {
      location: 'https://example.com/rapid-1',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'Rapid send 1',
      waitForResponse: true,
    });

    await senderPage.waitForTimeout(1_000);

    const result2 = await executeSendFlow(senderPage, {
      location: 'https://example.com/rapid-2',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'Rapid send 2',
      waitForResponse: true,
    });

    expect(result1.submitted).toBe(true);
    expect(result2.submitted).toBe(true);

    // Each should have its own QURL link
    if (result1.qurlLink && result2.qurlLink) {
      expect(result1.qurlLink).not.toBe(result2.qurlLink);
    }
  });
});
