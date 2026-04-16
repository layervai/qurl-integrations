/**
 * Custom Playwright fixtures for QURL Discord bot E2E tests.
 *
 * Discord auth tokens exist only in-memory during a browser session — they
 * don't persist to disk (not in cookies, localStorage, or IndexedDB in a
 * way that survives context restart). So we must login and run tests in
 * the SAME browser context without closing it.
 *
 * Each test gets its own sender + recipient page from long-lived contexts
 * that were authenticated at the start of the test run.
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

// Shared state across all tests in the worker (workers=1, so these are singletons)
let sharedBrowser: Browser | null = null;
let senderCtx: BrowserContext | null = null;
let recipientCtx: BrowserContext | null = null;
let senderLoggedIn = false;
let recipientLoggedIn = false;

async function getOrCreateBrowser(): Promise<Browser> {
  if (!sharedBrowser) {
    const headless = !!process.env.CI || process.env.HEADLESS === 'true';
    sharedBrowser = await chromium.launch({
      headless,
      args: ['--no-sandbox', '--disable-gpu'],
    });
  }
  return sharedBrowser;
}

async function getAuthenticatedContext(
  browser: Browser,
  role: 'sender' | 'recipient',
  email: string,
  password: string,
  totp?: string,
): Promise<BrowserContext> {
  const isLoggedIn = role === 'sender' ? senderLoggedIn : recipientLoggedIn;
  const existingCtx = role === 'sender' ? senderCtx : recipientCtx;

  if (existingCtx && isLoggedIn) {
    return existingCtx;
  }

  const context = existingCtx || await browser.newContext({
    viewport: { width: 1440, height: 900 },
    locale: 'en-US',
  });

  // Login
  const page = await context.newPage();
  const loginPage = new DiscordLoginPage(page);

  console.log(`[fixture] Logging in ${role}...`);
  await loginPage.login(email, password, totp);

  const loggedIn = await loginPage.isLoggedIn();
  if (!loggedIn) {
    throw new Error(`Failed to log in as ${role} (${email})`);
  }
  console.log(`[fixture] ${role} logged in successfully`);

  // Store for reuse
  if (role === 'sender') {
    senderCtx = context;
    senderLoggedIn = true;
  } else {
    recipientCtx = context;
    recipientLoggedIn = true;
  }

  // Close the login page (tests will create their own pages)
  await page.close();

  return context;
}

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

  senderContext: async ({ env }, use) => {
    const browser = await getOrCreateBrowser();
    const context = await getAuthenticatedContext(
      browser, 'sender',
      env.DISCORD_SENDER_EMAIL,
      env.DISCORD_SENDER_PASSWORD,
      env.DISCORD_SENDER_TOTP_SECRET,
    );
    await use(context);
    // Don't close — reused across tests
  },

  recipientContext: async ({ env }, use) => {
    const browser = await getOrCreateBrowser();
    const context = await getAuthenticatedContext(
      browser, 'recipient',
      env.DISCORD_RECIPIENT_EMAIL,
      env.DISCORD_RECIPIENT_PASSWORD,
      env.DISCORD_RECIPIENT_TOTP_SECRET,
    );
    await use(context);
    // Don't close — reused across tests
  },

  senderPage: async ({ senderContext }, use) => {
    const page = await senderContext.newPage();
    await use(page);
    await page.close();
  },

  recipientPage: async ({ recipientContext }, use) => {
    const page = await recipientContext.newPage();
    await use(page);
    await page.close();
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
