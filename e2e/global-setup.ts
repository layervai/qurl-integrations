/**
 * Global setup — runs before all tests.
 * Logs in both sender and recipient accounts, saves storageState.
 */

import { chromium, FullConfig } from '@playwright/test';
import { DiscordLoginPage } from './pages/discord-login.page';
import { loadEnv } from './helpers/env';
import {
  authFilePath,
  ensureAuthDir,
  isAuthStateFresh,
} from './helpers/auth-state';

async function globalSetup(_config: FullConfig): Promise<void> {
  const env = loadEnv();
  ensureAuthDir();

  const accounts = [
    {
      name: 'sender' as const,
      email: env.DISCORD_SENDER_EMAIL,
      password: env.DISCORD_SENDER_PASSWORD,
      totp: env.DISCORD_SENDER_TOTP_SECRET,
    },
    {
      name: 'recipient' as const,
      email: env.DISCORD_RECIPIENT_EMAIL,
      password: env.DISCORD_RECIPIENT_PASSWORD,
      totp: env.DISCORD_RECIPIENT_TOTP_SECRET,
    },
  ];

  for (const account of accounts) {
    const stateFile = authFilePath(account.name);

    if (isAuthStateFresh(account.name)) {
      console.log(`[global-setup] Auth state for ${account.name} is fresh, skipping login`);
      continue;
    }

    console.log(`[global-setup] Logging in ${account.name}...`);

    const browser = await chromium.launch({
      headless: !!process.env.CI || !!process.env.HEADLESS,
    });

    const context = await browser.newContext({
      viewport: { width: 1440, height: 900 },
      locale: 'en-US',
    });

    const page = await context.newPage();
    const loginPage = new DiscordLoginPage(page);

    await loginPage.login(account.email, account.password, account.totp);

    // Verify login succeeded
    const loggedIn = await loginPage.isLoggedIn();
    if (!loggedIn) {
      throw new Error(`Failed to log in as ${account.name} (${account.email})`);
    }

    // Save storage state
    await context.storageState({ path: stateFile });
    console.log(`[global-setup] Saved auth state for ${account.name} -> ${stateFile}`);

    await context.close();
    await browser.close();
  }
}

export default globalSetup;
