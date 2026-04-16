/**
 * Custom Playwright fixtures for QURL Discord bot E2E tests.
 *
 * Uses launchPersistentContext with user-data-dir to preserve Discord's
 * IndexedDB auth tokens across global-setup and test runs.
 */

import { test as base, Page, BrowserContext, chromium } from '@playwright/test';
import { DiscordChannelPage } from '../pages/discord-channel.page';
import { DiscordDmPage } from '../pages/discord-dm.page';
import { DiscordEmbedPage } from '../pages/discord-embed.page';
import { DiscordModalPage } from '../pages/discord-modal.page';
import { DiscordVoicePage } from '../pages/discord-voice.page';
import { DiscordUserPickerPage } from '../pages/discord-user-picker.page';
import { loadEnv, E2EEnv } from '../helpers/env';
import { profileDir } from '../helpers/auth-state';

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

  senderContext: async ({}, use) => {
    const context = await chromium.launchPersistentContext(profileDir('sender'), {
      headless: !!process.env.CI || process.env.HEADLESS === 'true',
      viewport: { width: 1440, height: 900 },
      locale: 'en-US',
      args: ['--no-sandbox', '--disable-gpu'],
    });
    await use(context);
    await context.close();
  },

  recipientContext: async ({}, use) => {
    const context = await chromium.launchPersistentContext(profileDir('recipient'), {
      headless: !!process.env.CI || process.env.HEADLESS === 'true',
      viewport: { width: 1440, height: 900 },
      locale: 'en-US',
      args: ['--no-sandbox', '--disable-gpu'],
    });
    await use(context);
    await context.close();
  },

  senderPage: async ({ senderContext }, use) => {
    const page = senderContext.pages()[0] || await senderContext.newPage();
    await use(page);
  },

  recipientPage: async ({ recipientContext }, use) => {
    const page = recipientContext.pages()[0] || await recipientContext.newPage();
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
