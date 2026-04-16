/**
 * Poll recipient DM inbox for a new message from the QURL bot.
 */

import { Page, expect } from '@playwright/test';
import { DiscordSelectors } from './discord-selectors';
import { retry } from './retry';

export interface DmMessage {
  content: string;
  embeds: Array<{
    title?: string;
    description?: string;
    fields: Array<{ name: string; value: string }>;
    footer?: string;
  }>;
  hasButton: boolean;
  buttonLabels: string[];
  timestamp: string;
}

export interface WaitForDmOptions {
  /** How long to wait total (default: 60s) */
  timeoutMs?: number;
  /** Poll interval (default: 3s) */
  pollIntervalMs?: number;
  /** Only match messages newer than this ISO timestamp */
  afterTimestamp?: string;
  /** Expected bot name (default: "QURL") */
  botName?: string;
}

/**
 * Navigate to DMs with the bot and wait for a new message.
 * Returns parsed message data.
 */
export async function waitForDm(
  page: Page,
  options: WaitForDmOptions = {},
): Promise<DmMessage> {
  const {
    timeoutMs = 60_000,
    pollIntervalMs = 3_000,
    botName = 'QURL',
  } = options;

  const maxAttempts = Math.ceil(timeoutMs / pollIntervalMs);

  return retry(
    async () => {
      // Navigate to DMs
      await page.goto('https://discord.com/channels/@me');
      await page.waitForSelector(DiscordSelectors.dm.container, { timeout: 10_000 });

      // Look for the bot DM channel
      const botDm = page.locator(`${DiscordSelectors.dm.container} a`).filter({ hasText: botName });
      await expect(botDm.first()).toBeVisible({ timeout: 10_000 });
      await botDm.first().click();

      // Wait for messages to load
      await page.waitForSelector(DiscordSelectors.channel.messageList, { timeout: 10_000 });

      // Get the last message
      const messages = page.locator(DiscordSelectors.channel.messageItem);
      const lastMessage = messages.last();
      await expect(lastMessage).toBeVisible({ timeout: 5_000 });

      // Parse content
      const content = await lastMessage
        .locator(DiscordSelectors.channel.messageContent)
        .textContent() ?? '';

      // Parse embeds
      const embedEls = lastMessage.locator(DiscordSelectors.embed.container);
      const embedCount = await embedEls.count();
      const embeds: DmMessage['embeds'] = [];

      for (let i = 0; i < embedCount; i++) {
        const embed = embedEls.nth(i);
        const title = await embed.locator(DiscordSelectors.embed.title).textContent().catch(() => undefined);
        const description = await embed.locator(DiscordSelectors.embed.description).textContent().catch(() => undefined);
        const footer = await embed.locator(DiscordSelectors.embed.footer).textContent().catch(() => undefined);

        const fieldEls = embed.locator(DiscordSelectors.embed.field);
        const fieldCount = await fieldEls.count();
        const fields: Array<{ name: string; value: string }> = [];

        for (let j = 0; j < fieldCount; j++) {
          const name = await fieldEls.nth(j).locator(DiscordSelectors.embed.fieldName).textContent() ?? '';
          const value = await fieldEls.nth(j).locator(DiscordSelectors.embed.fieldValue).textContent() ?? '';
          fields.push({ name, value });
        }

        embeds.push({
          title: title ?? undefined,
          description: description ?? undefined,
          fields,
          footer: footer ?? undefined,
        });
      }

      // Parse buttons
      const buttons = lastMessage.locator(DiscordSelectors.interaction.button);
      const buttonCount = await buttons.count();
      const buttonLabels: string[] = [];
      for (let i = 0; i < buttonCount; i++) {
        const label = await buttons.nth(i).textContent() ?? '';
        buttonLabels.push(label.trim());
      }

      // Get timestamp
      const timeEl = lastMessage.locator('time').first();
      const timestamp = await timeEl.getAttribute('datetime') ?? new Date().toISOString();

      return {
        content,
        embeds,
        hasButton: buttonCount > 0,
        buttonLabels,
        timestamp,
      };
    },
    {
      maxAttempts,
      initialDelayMs: pollIntervalMs,
      backoffMultiplier: 1,
      onRetry: (attempt) => {
        console.log(`[waitForDm] Attempt ${attempt}/${maxAttempts} — no new DM yet...`);
      },
    },
  );
}
