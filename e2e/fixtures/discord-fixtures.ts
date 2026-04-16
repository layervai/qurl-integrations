/**
 * Custom Playwright fixtures for QURL Discord bot E2E tests.
 *
 * Discord stores auth tokens in IndexedDB, which storageState doesn't capture.
 * So instead of reusing cached state, we log in fresh per-context and keep
 * the context alive for the duration of the test.
 */

import { test as base, Page, BrowserContext } from '@playwright/test';
import { DiscordChannelPage } from '../pages/discord-channel.page';
import { DiscordDmPage } from '../pages/discord-dm.page';
import { DiscordEmbedPage } from '../pages/discord-embed.page';
import { DiscordModalPage } from '../pages/discord-modal.page';
import { DiscordVoicePage } from '../pages/discord-voice.page';
import { DiscordUserPickerPage } from '../pages/discord-user-picker.page';
import { DiscordLoginPage } from '../pages/discord-login.page';
import { loadEnv, E2EEnv } from '../helpers/env';

export interface DiscordFixtures {
  env: E2EEnv;
  senderPage: Page;
  recipientPage: Page;
  senderContext: BrowserContext;
  recipientContext: BrowserContext;
  channelPage: DiscordChannelPage;
  dmPage: DiscordDmPage;
  modalPage: DiscordModalPage;
  embedPage: DiscordEmbedPage;
  voicePage: DiscordVoicePage;
  userPickerPage: DiscordUserPickerPage;
}

export const test = base.extend<DiscordFixtures>({
  env: async ({}, use) => {
    use(loadEnv());
  },

  senderContext: async ({ browser, env }, use) => {
    const context = await browser.newContext({
      viewport: { width: 1440, height: 900 },
      locale: 'en-US',
    });
    // Log in within this context
    const page = await context.newPage();
    const login = new DiscordLoginPage(page);
    await login.login(env.DISCORD_SENDER_EMAIL, env.DISCORD_SENDER_PASSWORD, env.DISCORD_SENDER_TOTP_SECRET);
    await page.close();
    await use(context);
    await context.close();
  },

  recipientContext: async ({ browser, env }, use) => {
    const context = await browser.newContext({
      viewport: { width: 1440, height: 900 },
      locale: 'en-US',
    });
    const page = await context.newPage();
    const login = new DiscordLoginPage(page);
    await login.login(env.DISCORD_RECIPIENT_EMAIL, env.DISCORD_RECIPIENT_PASSWORD, env.DISCORD_RECIPIENT_TOTP_SECRET);
    await page.close();
    await use(context);
    await context.close();
  },

  senderPage: async ({ senderContext }, use) => {
    const page = await senderContext.newPage();
    await use(page);
  },

  recipientPage: async ({ recipientContext }, use) => {
    const page = await recipientContext.newPage();
    await use(page);
  },

  channelPage: async ({ senderPage }, use) => {
    await use(new DiscordChannelPage(senderPage));
  },

  dmPage: async ({ recipientPage }, use) => {
    await use(new DiscordDmPage(recipientPage));
  },

  modalPage: async ({ senderPage }, use) => {
    await use(new DiscordModalPage(senderPage));
  },

  embedPage: async ({ senderPage }, use) => {
    const channelPage = new DiscordChannelPage(senderPage);
    const embedLocator = channelPage.getLastEmbed();
    await use(new DiscordEmbedPage(embedLocator));
  },

  voicePage: async ({ senderPage }, use) => {
    await use(new DiscordVoicePage(senderPage));
  },

  userPickerPage: async ({ senderPage }, use) => {
    await use(new DiscordUserPickerPage(senderPage));
  },
});

export { expect } from '@playwright/test';
