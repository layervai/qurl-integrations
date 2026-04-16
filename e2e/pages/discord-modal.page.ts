/**
 * Page Object Model for Discord modal interactions.
 */

import { Page, Locator, expect } from '@playwright/test';
import { DiscordSelectors } from '../helpers/discord-selectors';

export class DiscordModalPage {
  private readonly modal: Locator;

  constructor(private readonly page: Page) {
    this.modal = page.locator(DiscordSelectors.modal.root);
  }

  /**
   * Wait for a modal to appear.
   */
  async waitForModal(timeoutMs: number = 15_000): Promise<void> {
    await expect(this.modal).toBeVisible({ timeout: timeoutMs });
  }

  /**
   * Check if a modal is currently visible.
   */
  async isVisible(): Promise<boolean> {
    try {
      await this.modal.waitFor({ state: 'visible', timeout: 3_000 });
      return true;
    } catch {
      return false;
    }
  }

  /**
   * Get the modal title text.
   */
  async getTitle(): Promise<string> {
    const header = this.modal.locator(DiscordSelectors.modal.header);
    return (await header.textContent()) ?? '';
  }

  /**
   * Fill a text input in the modal by index.
   */
  async fillInput(index: number, value: string): Promise<void> {
    const inputs = this.modal.locator(DiscordSelectors.modal.textInput);
    const input = inputs.nth(index);
    await expect(input).toBeVisible({ timeout: 5_000 });
    await input.fill(value);
  }

  /**
   * Fill a text input by its label / placeholder.
   */
  async fillInputByLabel(label: string, value: string): Promise<void> {
    const input = this.modal.locator(
      `input[placeholder*="${label}" i], textarea[placeholder*="${label}" i], ` +
      `input[aria-label*="${label}" i], textarea[aria-label*="${label}" i]`,
    );
    await expect(input.first()).toBeVisible({ timeout: 5_000 });
    await input.first().fill(value);
  }

  /**
   * Submit the modal.
   */
  async submit(): Promise<void> {
    const submitBtn = this.modal.locator(DiscordSelectors.modal.submitButton);
    await expect(submitBtn).toBeVisible({ timeout: 5_000 });
    await submitBtn.click();

    // Wait for modal to close
    await expect(this.modal).toBeHidden({ timeout: 10_000 });
  }

  /**
   * Cancel / close the modal.
   */
  async cancel(): Promise<void> {
    const cancelBtn = this.modal.locator(DiscordSelectors.modal.cancelButton);
    if (await cancelBtn.isVisible({ timeout: 2_000 }).catch(() => false)) {
      await cancelBtn.click();
    } else {
      const closeBtn = this.modal.locator(DiscordSelectors.modal.closeButton);
      await closeBtn.click();
    }
    await expect(this.modal).toBeHidden({ timeout: 10_000 });
  }

  /**
   * Get all text input values.
   */
  async getInputValues(): Promise<string[]> {
    const inputs = this.modal.locator(DiscordSelectors.modal.textInput);
    const count = await inputs.count();
    const values: string[] = [];
    for (let i = 0; i < count; i++) {
      values.push(await inputs.nth(i).inputValue());
    }
    return values;
  }
}
