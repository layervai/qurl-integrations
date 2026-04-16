/**
 * Page Object Model for Discord user select / picker component.
 */

import { Page, Locator, expect } from '@playwright/test';
import { DiscordSelectors } from '../helpers/discord-selectors';

export class DiscordUserPickerPage {
  constructor(private readonly page: Page) {}

  /**
   * Search for a user in the user picker.
   */
  async searchUser(query: string): Promise<void> {
    const searchInput = this.page.locator(DiscordSelectors.userPicker.searchInput);
    await expect(searchInput.first()).toBeVisible({ timeout: 10_000 });
    await searchInput.first().fill(query);
    await this.page.waitForTimeout(1_500);
  }

  /**
   * Select a user from the search results.
   */
  async selectUser(username: string): Promise<void> {
    const option = this.page
      .locator(DiscordSelectors.userPicker.userOption)
      .filter({ hasText: username });
    await expect(option.first()).toBeVisible({ timeout: 10_000 });
    await option.first().click();
  }

  /**
   * Search and select a user in one step.
   */
  async searchAndSelect(username: string): Promise<void> {
    await this.searchUser(username);
    await this.selectUser(username);
  }

  /**
   * Get all currently visible user options.
   */
  async getVisibleUsers(): Promise<string[]> {
    const options = this.page.locator(DiscordSelectors.userPicker.userOption);
    const count = await options.count();
    const users: string[] = [];
    for (let i = 0; i < count; i++) {
      const text = (await options.nth(i).textContent()) ?? '';
      users.push(text.trim());
    }
    return users;
  }

  /**
   * Check if a user appears in the picker results.
   */
  async isUserVisible(username: string): Promise<boolean> {
    const option = this.page
      .locator(DiscordSelectors.userPicker.userOption)
      .filter({ hasText: username });
    try {
      await option.first().waitFor({ state: 'visible', timeout: 5_000 });
      return true;
    } catch {
      return false;
    }
  }

  /**
   * Get selected users (if multi-select).
   */
  async getSelectedUsers(): Promise<string[]> {
    const selected = this.page.locator(DiscordSelectors.userPicker.selectedUser);
    const count = await selected.count();
    const users: string[] = [];
    for (let i = 0; i < count; i++) {
      const text = (await selected.nth(i).textContent()) ?? '';
      users.push(text.trim());
    }
    return users;
  }
}
