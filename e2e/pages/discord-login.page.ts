/**
 * Page Object Model for Discord login flow.
 */

import { Page, expect } from '@playwright/test';
import { DiscordSelectors } from '../helpers/discord-selectors';
import { generateFreshTotp } from '../helpers/totp';
import { retry } from '../helpers/retry';

export class DiscordLoginPage {
  constructor(private readonly page: Page) {}

  async goto(): Promise<void> {
    await this.page.goto('https://discord.com/login', {
      waitUntil: 'networkidle',
    });
  }

  async login(email: string, password: string, totpSecret?: string): Promise<void> {
    await this.goto();

    // Fill email
    const emailInput = this.page.locator(DiscordSelectors.login.emailInput);
    await expect(emailInput).toBeVisible({ timeout: 15_000 });
    await emailInput.fill(email);

    // Fill password
    const passwordInput = this.page.locator(DiscordSelectors.login.passwordInput);
    await passwordInput.fill(password);

    // Click login
    const loginButton = this.page.locator(DiscordSelectors.login.loginButton);
    await loginButton.click();

    // Handle 2FA if needed
    if (totpSecret) {
      await this.handleTotp(totpSecret);
    }

    // Wait for app to load
    await this.waitForAppLoad();
  }

  private async handleTotp(secret: string): Promise<void> {
    await retry(
      async () => {
        const totpInput = this.page.locator(DiscordSelectors.login.totpInput);
        await expect(totpInput).toBeVisible({ timeout: 10_000 });

        const code = await generateFreshTotp(secret);
        await totpInput.fill(code);

        const submitButton = this.page.locator(DiscordSelectors.login.totpSubmit);
        await submitButton.click();
      },
      {
        maxAttempts: 3,
        initialDelayMs: 2_000,
        onRetry: (attempt) => {
          console.log(`[TOTP] Retry attempt ${attempt} — code may have expired`);
        },
      },
    );
  }

  private async waitForAppLoad(): Promise<void> {
    // Wait for Discord app shell to appear
    await expect(this.page.locator(DiscordSelectors.login.appLoaded)).toBeVisible({
      timeout: 30_000,
    });

    // Wait for network to settle
    await this.page.waitForLoadState('networkidle');
  }

  async isLoggedIn(): Promise<boolean> {
    try {
      await this.page.locator(DiscordSelectors.login.appLoaded).waitFor({
        state: 'visible',
        timeout: 5_000,
      });
      return true;
    } catch {
      return false;
    }
  }
}
