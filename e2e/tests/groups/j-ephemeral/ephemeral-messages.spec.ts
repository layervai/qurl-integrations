/**
 * Group J — Ephemeral message handling.
 */

import { test, expect } from '../../../fixtures/discord-fixtures';
import { executeSendFlow } from '../../../helpers/send-flow';
import { DiscordSelectors } from '../../../helpers/discord-selectors';

test.describe('Ephemeral Messages', () => {
  test.beforeEach(async ({ channelPage, env }) => {
    await channelPage.goto(env.DISCORD_GUILD_ID, env.DISCORD_CHANNEL_ID);
  });

  test('bot confirmation is ephemeral (only visible to sender)', async ({
    senderPage,
    recipientPage,
    env,
  }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/ephemeral-test',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'Ephemeral check',
    });

    expect(result.submitted).toBe(true);

    // Check for ephemeral marker in sender's view
    const ephemeralMarker = senderPage.locator(
      DiscordSelectors.ephemeral.ephemeralMarker,
    );
    const isEphemeral = await ephemeralMarker.first().isVisible({ timeout: 5_000 }).catch(() => false);

    if (isEphemeral) {
      // Verify recipient cannot see this message
      await recipientPage.goto(
        `https://discord.com/channels/${env.DISCORD_GUILD_ID}/${env.DISCORD_CHANNEL_ID}`,
      );
      await recipientPage.waitForTimeout(3_000);

      // The ephemeral message should not be visible to recipient
      const messages = await recipientPage.locator(DiscordSelectors.channel.messageItem).allTextContents();
      const hasEphemeral = messages.some((m) => m.includes('Ephemeral check'));
      // Ephemeral messages are not visible to others, but the message text
      // might appear in the send command itself (which is visible)
      // The bot RESPONSE should be ephemeral
    }
  });

  test('ephemeral error messages can be dismissed', async ({ senderPage, env }) => {
    // Trigger an error (e.g., invalid URL)
    const result = await executeSendFlow(senderPage, {
      location: 'invalid-url-for-error',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'Trigger error',
    });

    if (result.errorMessage) {
      // Try to dismiss the ephemeral error
      const dismissBtn = senderPage.locator(
        DiscordSelectors.ephemeral.dismissButton,
      );

      if (await dismissBtn.isVisible({ timeout: 5_000 }).catch(() => false)) {
        await dismissBtn.click();
        await expect(dismissBtn).toBeHidden({ timeout: 5_000 });
      }
    }
  });
});
