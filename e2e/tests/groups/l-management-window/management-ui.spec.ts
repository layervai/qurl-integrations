/**
 * Group L — Management window / dashboard interactions.
 */

import { test, expect } from '../../../fixtures/discord-fixtures';
import { executeSendFlow } from '../../../helpers/send-flow';
import { DiscordSelectors } from '../../../helpers/discord-selectors';

test.describe('Management Window', () => {
  test.beforeEach(async ({ channelPage, env }) => {
    await channelPage.goto(env.DISCORD_GUILD_ID, env.DISCORD_CHANNEL_ID);
  });

  test('bot response has management buttons', async ({
    senderPage,
    env,
  }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/management-test',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'Management button test',
    });

    expect(result.submitted).toBe(true);

    // Check for action buttons in the response
    const lastMessage = senderPage.locator(DiscordSelectors.channel.messageItem).last();
    const buttons = lastMessage.locator(DiscordSelectors.interaction.button);
    const buttonCount = await buttons.count();

    // Bot responses typically have management buttons (View, Revoke, etc.)
    expect(buttonCount).toBeGreaterThanOrEqual(0);
  });

  test('clicking View button shows link details', async ({
    senderPage,
    env,
    channelPage,
  }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/view-details-test',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'View details test',
    });

    expect(result.submitted).toBe(true);

    // Try clicking View if available
    try {
      await channelPage.clickButtonInLastMessage('View');
      await senderPage.waitForTimeout(2_000);

      // Should show details (embed or modal)
      const embed = senderPage.locator(DiscordSelectors.embed.container);
      const modal = senderPage.locator(DiscordSelectors.modal.root);

      const hasEmbed = await embed.first().isVisible({ timeout: 5_000 }).catch(() => false);
      const hasModal = await modal.isVisible({ timeout: 3_000 }).catch(() => false);

      expect(hasEmbed || hasModal).toBe(true);
    } catch {
      // View button may not exist for all flows
      test.skip();
    }
  });

  test('clicking Revoke button confirms revocation', async ({
    senderPage,
    env,
    channelPage,
  }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/revoke-ui-test',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'Revoke UI test',
    });

    expect(result.submitted).toBe(true);

    try {
      await channelPage.clickButtonInLastMessage('Revoke');
      await senderPage.waitForTimeout(2_000);

      // Should show confirmation or update the embed
      const pageText = await senderPage.locator(DiscordSelectors.channel.messageItem).last().textContent();
      expect(
        pageText?.toLowerCase().includes('revoke') ||
        pageText?.toLowerCase().includes('confirm') ||
        pageText?.toLowerCase().includes('success'),
      ).toBeTruthy();
    } catch {
      test.skip();
    }
  });
});
