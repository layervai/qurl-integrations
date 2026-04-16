/**
 * Group D — Recipient-focused tests: DM delivery, notifications.
 */

import { test, expect } from '../../../fixtures/discord-fixtures';
import { executeSendFlow } from '../../../helpers/send-flow';
import { waitForDm } from '../../../helpers/wait-for-dm';
import { DiscordDmPage } from '../../../pages/discord-dm.page';

test.describe('Recipient Delivery', () => {
  test.beforeEach(async ({ channelPage, env }) => {
    await channelPage.goto(env.DISCORD_GUILD_ID, env.DISCORD_CHANNEL_ID);
  });

  test('recipient receives DM with QURL link', async ({
    senderPage,
    recipientPage,
    env,
  }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/dm-delivery-test',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'Please check this link',
    });

    expect(result.submitted).toBe(true);
    expect(result.qurlLink).toBeDefined();

    // Switch to recipient and check DM
    const dm = await waitForDm(recipientPage, {
      botName: 'QURL',
      timeoutMs: 60_000,
    });

    expect(dm.embeds.length).toBeGreaterThan(0);
  });

  test('recipient DM contains sender information', async ({
    senderPage,
    recipientPage,
    env,
  }) => {
    await executeSendFlow(senderPage, {
      location: 'https://example.com/sender-info-test',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'Sender info check',
    });

    const dm = await waitForDm(recipientPage, { botName: 'QURL' });
    const allText = dm.content + JSON.stringify(dm.embeds);

    // The DM should reference the sender or the message
    expect(allText).toContain('Sender info check');
  });

  test('recipient can click the QURL link button', async ({
    senderPage,
    recipientPage,
    env,
  }) => {
    await executeSendFlow(senderPage, {
      location: 'https://example.com/button-click-test',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'Click the button',
    });

    const dmPage = new DiscordDmPage(recipientPage);
    await dmPage.openDmWith('QURL');

    // Should have a button to open the QURL link
    const lastMsg = dmPage.getLastMessage();
    const buttons = lastMsg.locator('button');
    const buttonCount = await buttons.count();
    expect(buttonCount).toBeGreaterThan(0);
  });
});
