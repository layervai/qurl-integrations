/**
 * Page Object Model for Discord Direct Messages.
 */

import { Page, Locator, expect } from '@playwright/test';
import { DiscordSelectors } from '../helpers/discord-selectors';

export class DiscordDmPage {
  constructor(private readonly page: Page) {}

  /**
   * Navigate to the DM list.
   */
  async gotoDmList(): Promise<void> {
    await this.page.goto('https://discord.com/channels/@me');
    await this.page.waitForSelector(DiscordSelectors.dm.container, { timeout: 15_000 });
  }

  /**
   * Open a DM conversation with a specific user.
   */
  async openDmWith(username: string): Promise<void> {
    await this.gotoDmList();

    const dmLink = this.page.locator(DiscordSelectors.dm.container + ' a')
      .filter({ hasText: username });
    await expect(dmLink.first()).toBeVisible({ timeout: 10_000 });
    await dmLink.first().click();
    await this.page.waitForTimeout(1_000);
  }

  /**
   * Get the last message in the current DM conversation.
   */
  getLastMessage(): Locator {
    return this.page.locator(DiscordSelectors.channel.messageItem).last();
  }

  /**
   * Get text content of the last message.
   */
  async getLastMessageText(): Promise<string> {
    const msg = this.getLastMessage();
    return (await msg.locator(DiscordSelectors.channel.messageContent).textContent()) ?? '';
  }

  /**
   * Get all embed data from the last message.
   */
  async getLastMessageEmbeds(): Promise<Array<{ title: string; description: string }>> {
    const msg = this.getLastMessage();
    const embeds = msg.locator(DiscordSelectors.embed.container);
    const count = await embeds.count();

    const result: Array<{ title: string; description: string }> = [];
    for (let i = 0; i < count; i++) {
      const embed = embeds.nth(i);
      const title = (await embed.locator(DiscordSelectors.embed.title).textContent().catch(() => '')) ?? '';
      const description = (await embed.locator(DiscordSelectors.embed.description).textContent().catch(() => '')) ?? '';
      result.push({ title, description });
    }
    return result;
  }

  /**
   * Click a button in the last DM message.
   */
  async clickButton(label: string): Promise<void> {
    const button = this.getLastMessage().locator(
      DiscordSelectors.interaction.buttonByLabel(label),
    );
    await expect(button).toBeVisible({ timeout: 10_000 });
    await button.click();
  }

  /**
   * Wait for a new message in the DM.
   */
  async waitForNewMessage(timeoutMs: number = 30_000): Promise<Locator> {
    const currentCount = await this.page.locator(DiscordSelectors.channel.messageItem).count();

    await expect(async () => {
      const newCount = await this.page.locator(DiscordSelectors.channel.messageItem).count();
      expect(newCount).toBeGreaterThan(currentCount);
    }).toPass({ timeout: timeoutMs });

    return this.getLastMessage();
  }
}
