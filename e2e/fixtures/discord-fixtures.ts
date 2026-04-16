/**
 * Custom Playwright fixtures for QURL Discord bot E2E tests.
 *
 * Uses launchPersistentContext to preserve Discord's IndexedDB auth tokens.
 * Each fixture launches its own persistent context, checks if already logged
 * in, and logs in if needed.
 */

import { test as base, Page, BrowserContext, chromium } from '@playwright/test';
import { DiscordChannelPage } from '../pages/discord-channel.page';
import { DiscordDmPage } from '../pages/discord-dm.page';
import { DiscordEmbedPage } from '../pages/discord-embed.page';
import { DiscordModalPage } from '../pages/discord-modal.page';
import { DiscordVoicePage } from '../pages/discord-voice.page';
import { DiscordUserPickerPage } from '../pages/discord-user-picker.page';
import { DiscordLoginPage } from '../pages/discord-login.page';
import { loadEnv, E2EEnv } from '../helpers/env';
import { profileDir, ensureAuthDir } from '../helpers/auth-state';

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

async function launchDiscordContext(
  account: 'sender' | 'recipient',
  email: string,
  password: string,
  totp?: string,
): Promise<BrowserContext> {
  ensureAuthDir();
  const headless = !!process.env.CI || process.env.HEADLESS === 'true';
  const context = await chromium.launchPersistentContext(profileDir(account), {
    headless,
    viewport: { width: 1440, height: 900 },
    locale: 'en-US',
    args: ['--no-sandbox', '--disable-gpu'],
  });

  // Check if already logged in by navigating to Discord
  const page = context.pages()[0] || await context.newPage();
  await page.goto('https://discord.com/channels/@me', { waitUntil: 'domcontentloaded', timeout: 30_000 });

  // If we land on login page, we need to authenticate
  const isLoginPage = await page.locator('input[name="email"]').isVisible({ timeout: 5_000 }).catch(() => false);
  if (isLoginPage) {
    console.log(`[fixture] ${account} not logged in, authenticating...`);
    const loginPage = new DiscordLoginPage(page);
    await loginPage.login(email, password, totp);
    const loggedIn = await loginPage.isLoggedIn();
    if (!loggedIn) {
      throw new Error(`Failed to log in as ${account} (${email})`);
    }
    console.log(`[fixture] ${account} logged in successfully`);
  } else {
    console.log(`[fixture] ${account} already logged in`);
  }

  return context;
}

export const test = base.extend<DiscordFixtures>({
  env: async ({}, use) => {
    use(loadEnv());
  },

  senderContext: async ({ env }, use) => {
    const context = await launchDiscordContext(
      'sender',
      env.DISCORD_SENDER_EMAIL,
      env.DISCORD_SENDER_PASSWORD,
      env.DISCORD_SENDER_TOTP_SECRET,
    );
    await use(context);
    await context.close();
  },

  recipientContext: async ({ env }, use) => {
    const context = await launchDiscordContext(
      'recipient',
      env.DISCORD_RECIPIENT_EMAIL,
      env.DISCORD_RECIPIENT_PASSWORD,
      env.DISCORD_RECIPIENT_TOTP_SECRET,
    );
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
