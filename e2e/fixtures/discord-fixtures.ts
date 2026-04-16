/**
 * Custom Playwright fixtures for QURL Discord bot E2E tests.
 */

import { test as base, Page, BrowserContext } from '@playwright/test';
import { DiscordChannelPage } from '../pages/discord-channel.page';
import { DiscordDmPage } from '../pages/discord-dm.page';
import { DiscordEmbedPage } from '../pages/discord-embed.page';
import { DiscordModalPage } from '../pages/discord-modal.page';
import { DiscordVoicePage } from '../pages/discord-voice.page';
import { DiscordUserPickerPage } from '../pages/discord-user-picker.page';
import { loadEnv, E2EEnv } from '../helpers/env';
import { authFilePath } from '../helpers/auth-state';

export interface DiscordFixtures {
  /** Environment config */
  env: E2EEnv;
  /** Sender's page (logged in) */
  senderPage: Page;
  /** Recipient's page (logged in) */
  recipientPage: Page;
  /** Sender's browser context */
  senderContext: BrowserContext;
  /** Recipient's browser context */
  recipientContext: BrowserContext;
  /** Channel POM for sender */
  channelPage: DiscordChannelPage;
  /** DM POM for recipient */
  dmPage: DiscordDmPage;
  /** Modal POM */
  modalPage: DiscordModalPage;
  /** Embed POM factory */
  embedPage: DiscordEmbedPage;
  /** Voice POM */
  voicePage: DiscordVoicePage;
  /** User picker POM */
  userPickerPage: DiscordUserPickerPage;
}

export const test = base.extend<DiscordFixtures>({
  env: async ({}, use) => {
    use(loadEnv());
  },

  senderContext: async ({ browser }, use) => {
    const context = await browser.newContext({
      storageState: authFilePath('sender'),
      viewport: { width: 1440, height: 900 },
      locale: 'en-US',
    });
    await use(context);
    await context.close();
  },

  recipientContext: async ({ browser }, use) => {
    const context = await browser.newContext({
      storageState: authFilePath('recipient'),
      viewport: { width: 1440, height: 900 },
      locale: 'en-US',
    });
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
    // Factory-like: creates embed page from the last embed in the channel
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
