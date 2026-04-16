/**
 * Page Object Model for Discord channel interactions.
 */

import { Page, Locator, expect } from '@playwright/test';
import { DiscordSelectors } from '../helpers/discord-selectors';

export class DiscordChannelPage {
  private readonly messageInput: Locator;
  private readonly messageList: Locator;

  constructor(private readonly page: Page) {
    this.messageInput = page.locator(DiscordSelectors.channel.messageInput);
    this.messageList = page.locator(DiscordSelectors.channel.messageList);
  }

  /**
   * Navigate to a specific channel by guild and channel ID.
   */
  async goto(guildId: string, channelId: string): Promise<void> {
    await this.page.goto(`https://discord.com/channels/${guildId}/${channelId}`);
    await expect(this.messageInput).toBeVisible({ timeout: 20_000 });
  }

  /**
   * Send a plain text message in the current channel.
   */
  async sendMessage(text: string): Promise<void> {
    await this.messageInput.click();
    await this.messageInput.fill(text);
    await this.page.keyboard.press('Enter');
    await this.page.waitForTimeout(1_000);
  }

  /**
   * Type a slash command and wait for the autocomplete dropdown.
   */
  async typeSlashCommand(command: string): Promise<void> {
    await this.messageInput.click();
    await this.messageInput.fill(`/${command}`);
    await this.page.waitForTimeout(1_500);
  }

  /**
   * Select a slash command from the autocomplete dropdown.
   */
  async selectSlashCommand(commandName: string): Promise<void> {
    const option = this.page
      .locator(DiscordSelectors.slashCommand.commandOption)
      .filter({ hasText: commandName });
    await expect(option.first()).toBeVisible({ timeout: 10_000 });
    await option.first().click();
    await this.page.waitForTimeout(500);
  }

  /**
   * Get the last N messages in the channel.
   */
  async getMessages(count: number = 5): Promise<string[]> {
    const items = this.page.locator(DiscordSelectors.channel.messageItem);
    const total = await items.count();
    const start = Math.max(0, total - count);

    const messages: string[] = [];
    for (let i = start; i < total; i++) {
      const text = await items.nth(i)
        .locator(DiscordSelectors.channel.messageContent)
        .textContent()
        .catch(() => '');
      messages.push(text ?? '');
    }
    return messages;
  }

  /**
   * Get the last message element.
   */
  getLastMessage(): Locator {
    return this.page.locator(DiscordSelectors.channel.messageItem).last();
  }

  /**
   * Get the last embed in the channel.
   */
  getLastEmbed(): Locator {
    return this.getLastMessage().locator(DiscordSelectors.embed.container).first();
  }

  /**
   * Wait for a new message to appear in the channel.
   */
  async waitForNewMessage(timeoutMs: number = 30_000): Promise<Locator> {
    const currentCount = await this.page.locator(DiscordSelectors.channel.messageItem).count();

    await expect(async () => {
      const newCount = await this.page.locator(DiscordSelectors.channel.messageItem).count();
      expect(newCount).toBeGreaterThan(currentCount);
    }).toPass({ timeout: timeoutMs });

    return this.getLastMessage();
  }

  /**
   * Click a button in the last message by label.
   */
  async clickButtonInLastMessage(label: string): Promise<void> {
    const button = this.getLastMessage().locator(
      DiscordSelectors.interaction.buttonByLabel(label),
    );
    await expect(button).toBeVisible({ timeout: 10_000 });
    await button.click();
  }

  /**
   * Upload a file to the current channel.
   */
  async uploadFile(filePath: string): Promise<void> {
    const fileInput = this.page.locator('input[type="file"]');
    await fileInput.setInputFiles(filePath);
    await this.page.waitForTimeout(2_000);
  }

  /**
   * Check if the channel header contains the expected text.
   */
  async getChannelName(): Promise<string> {
    const header = this.page.locator(DiscordSelectors.channel.channelHeader);
    return (await header.textContent()) ?? '';
  }
}
