/**
 * Global setup — logs in both Discord accounts using persistent browser
 * profiles (user-data-dir). This preserves IndexedDB where Discord stores
 * auth tokens, unlike storageState which only captures cookies + localStorage.
 */

import { chromium, FullConfig } from '@playwright/test';
import { DiscordLoginPage } from './pages/discord-login.page';
import { loadEnv } from './helpers/env';
import { profileDir, isProfileFresh, ensureAuthDir } from './helpers/auth-state';

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
    const userDataDir = profileDir(account.name);

    if (isProfileFresh(account.name)) {
      console.log(`[global-setup] Profile for ${account.name} is fresh, skipping login`);
      continue;
    }

    console.log(`[global-setup] Logging in ${account.name}...`);

    const context = await chromium.launchPersistentContext(userDataDir, {
      headless: !!process.env.CI || process.env.HEADLESS === 'true',
      viewport: { width: 1440, height: 900 },
      locale: 'en-US',
      args: ['--no-sandbox', '--disable-gpu'],
    });

    const page = context.pages()[0] || await context.newPage();
    const loginPage = new DiscordLoginPage(page);

    await loginPage.login(account.email, account.password, account.totp);

    const loggedIn = await loginPage.isLoggedIn();
    if (!loggedIn) {
      await context.close();
      throw new Error(`Failed to log in as ${account.name} (${account.email})`);
    }

    console.log(`[global-setup] Logged in ${account.name}, profile saved to ${userDataDir}`);
    await context.close();
  }
}

export default globalSetup;
