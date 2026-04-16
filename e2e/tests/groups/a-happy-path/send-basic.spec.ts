/**
 * Group A — Happy path: Basic /qurl send flow end-to-end.
 * @smoke
 */

import { test, expect } from '../../../fixtures/discord-fixtures';
import { executeSendFlow } from '../../../helpers/send-flow';
import { extractFirstQurlLink, openQurlLink } from '../../../helpers/qurl-link';
import { waitForDm } from '../../../helpers/wait-for-dm';

test.describe('Happy Path — /qurl send', () => {
  test.beforeEach(async ({ channelPage, env }) => {
    await channelPage.goto(env.DISCORD_GUILD_ID, env.DISCORD_CHANNEL_ID);
  });

  test('send a basic HTTPS link to a recipient @smoke', async ({
    senderPage,
    recipientPage,
    env,
    browser,
  }) => {
    // Sender executes /qurl send with a basic URL
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'E2E test — basic happy path',
    });

    expect(result.submitted).toBe(true);
    expect(result.errorMessage).toBeUndefined();
    expect(result.qurlLink).toBeDefined();

    // Verify the QURL link resolves
    const linkResult = await openQurlLink(browser, result.qurlLink!);
    expect(linkResult.ok).toBe(true);
    expect(linkResult.isExpired).toBe(false);

    // Verify recipient received the DM
    const dm = await waitForDm(recipientPage, {
      botName: 'QURL',
      timeoutMs: 60_000,
    });

    expect(dm.content + JSON.stringify(dm.embeds)).toContain('example.com');
    expect(dm.hasButton).toBe(true);
  });

  test('send with expiry and verify bot response embed', async ({
    senderPage,
    env,
  }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/time-limited',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      expiry: '1h',
      message: 'Expires in 1 hour',
    });

    expect(result.submitted).toBe(true);
    expect(result.responseText).toBeDefined();
    expect(result.qurlLink).toBeDefined();
  });

  test('send without a message (optional field)', async ({
    senderPage,
    env,
  }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/no-message',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
    });

    expect(result.submitted).toBe(true);
    expect(result.errorMessage).toBeUndefined();
  });

  test('send and verify QURL link contains expected domain', async ({
    senderPage,
    env,
  }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/verify-domain',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'Domain check',
    });

    expect(result.qurlLink).toBeDefined();
    const links = extractFirstQurlLink(result.responseText ?? '');
    expect(links).toMatch(/qurl\.(io|dev)/);
  });
});
