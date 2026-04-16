/**
 * Discord auth lives in the page's JS runtime (not in cookies/storage that
 * transfers across pages). We must log in on the SAME page that runs the tests.
 *
 * Approach: senderPage logs in, then the test uses that exact page for everything.
 * The page stays open for the test's lifetime.
 */

import { test as base, Page, BrowserContext, chromium, Browser } from '@playwright/test';
import { DiscordChannelPage } from '../pages/discord-channel.page';
import { DiscordDmPage } from '../pages/discord-dm.page';
import { DiscordEmbedPage } from '../pages/discord-embed.page';
import { DiscordModalPage } from '../pages/discord-modal.page';
import { DiscordVoicePage } from '../pages/discord-voice.page';
import { DiscordUserPickerPage } from '../pages/discord-user-picker.page';
import { DiscordLoginPage } from '../pages/discord-login.page';
import { loadEnv, E2EEnv } from '../helpers/env';

// Shared browser instance (workers=1)
let sharedBrowser: Browser | null = null;
// Keep pages alive across tests so we don't re-login
let senderPage_: Page | null = null;
let recipientPage_: Page | null = null;

async function getBrowser(): Promise<Browser> {
  if (!sharedBrowser || !sharedBrowser.isConnected()) {
    const headless = !!process.env.CI || process.env.HEADLESS === 'true';
    sharedBrowser = await chromium.launch({
      headless: false, // Discord blocks headless browsers; use headful on Xvfb
      args: ['--no-sandbox', '--disable-gpu', '--disable-blink-features=AutomationControlled'],
    });
  }
  return sharedBrowser;
}

async function getLoggedInPage(
  browser: Browser,
  role: 'sender' | 'recipient',
  email: string,
  password: string,
  totp?: string,
): Promise<Page> {
  const existing = role === 'sender' ? senderPage_ : recipientPage_;

  // Reuse if the page is still open and not crashed
  if (existing && !existing.isClosed()) {
    return existing;
  }

  // Create a new page and log in
  const context = await browser.newContext({
    viewport: { width: 1440, height: 900 },
    locale: 'en-US',
  });
  const page = await context.newPage();

  console.log(`[fixture] Logging in ${role} on a fresh page...`);
  const loginPage = new DiscordLoginPage(page);
  await loginPage.login(email, password, totp);

  const loggedIn = await loginPage.isLoggedIn();
  if (!loggedIn) {
    throw new Error(`Failed to log in as ${role} (${email})`);
  }
  console.log(`[fixture] ${role} logged in successfully`);

  // Cache for reuse
  if (role === 'sender') senderPage_ = page;
  else recipientPage_ = page;

  return page;
}

export interface DiscordFixtures {
  env: E2EEnv;
  senderPage: Page;
  recipientPage: Page;
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

  senderPage: async ({ env }, use) => {
    const browser = await getBrowser();
    const page = await getLoggedInPage(
      browser, 'sender',
      env.DISCORD_SENDER_EMAIL,
      env.DISCORD_SENDER_PASSWORD,
      env.DISCORD_SENDER_TOTP_SECRET,
    );
    await use(page);
    // Don't close — reused across tests
  },

  recipientPage: async ({ env }, use) => {
    const browser = await getBrowser();
    const page = await getLoggedInPage(
      browser, 'recipient',
      env.DISCORD_RECIPIENT_EMAIL,
      env.DISCORD_RECIPIENT_PASSWORD,
      env.DISCORD_RECIPIENT_TOTP_SECRET,
    );
    await use(page);
    // Don't close — reused across tests
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
