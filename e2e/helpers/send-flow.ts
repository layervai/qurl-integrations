/**
 * High-level orchestrator for the /qurl send slash command flow.
 * Drives the Discord UI through typing the command, filling options,
 * and submitting.
 */

import { Page, expect } from '@playwright/test';
import { DiscordSelectors } from './discord-selectors';
import { retry } from './retry';

export interface SendFlowOptions {
  /** Location URL to share */
  location?: string;
  /** Recipient username */
  recipient?: string;
  /** Expiry duration (e.g., "1h", "30m", "7d") */
  expiry?: string;
  /** Custom message */
  message?: string;
  /** File path to attach */
  filePath?: string;
  /** Whether to wait for the bot response embed */
  waitForResponse?: boolean;
  /** Timeout for bot response (default: 30s) */
  responseTimeoutMs?: number;
}

export interface SendFlowResult {
  /** Whether the command was submitted successfully */
  submitted: boolean;
  /** The bot's response embed text, if waitForResponse was true */
  responseText?: string;
  /** The QURL link from the response, if present */
  qurlLink?: string;
  /** Error message from the bot, if any */
  errorMessage?: string;
}

/**
 * Execute the /qurl send flow in the current channel.
 * Assumes the page is already on the target channel.
 */
export async function executeSendFlow(
  page: Page,
  options: SendFlowOptions,
): Promise<SendFlowResult> {
  const {
    location,
    recipient,
    expiry,
    message,
    filePath,
    waitForResponse = true,
    responseTimeoutMs = 30_000,
  } = options;

  // Focus the message input
  const messageInput = page.locator(DiscordSelectors.channel.messageInput);
  await expect(messageInput).toBeVisible({ timeout: 10_000 });
  await messageInput.click();

  // Type the slash command
  await messageInput.fill('/qurl send');
  await page.waitForTimeout(1_500);

  // Wait for autocomplete to appear and select the command
  const autocomplete = page.locator(DiscordSelectors.slashCommand.commandOption);
  await expect(autocomplete.first()).toBeVisible({ timeout: 10_000 });

  // Click the /qurl send option
  const sendOption = autocomplete.filter({ hasText: 'send' });
  await expect(sendOption.first()).toBeVisible({ timeout: 5_000 });
  await sendOption.first().click();
  await page.waitForTimeout(500);

  // Fill in the location option if provided
  if (location) {
    await fillSlashCommandOption(page, 'location', location);
  }

  // Fill in recipient if provided
  if (recipient) {
    await fillSlashCommandOption(page, 'recipient', recipient);
    // Wait for user autocomplete and select
    await page.waitForTimeout(1_000);
    const userOption = page.locator(DiscordSelectors.slashCommand.autocompleteRow)
      .filter({ hasText: recipient });
    if (await userOption.first().isVisible({ timeout: 5_000 }).catch(() => false)) {
      await userOption.first().click();
    }
  }

  // Fill in expiry if provided
  if (expiry) {
    await fillSlashCommandOption(page, 'expiry', expiry);
  }

  // Fill in message if provided
  if (message) {
    await fillSlashCommandOption(page, 'message', message);
  }

  // Attach file if provided
  if (filePath) {
    const fileInput = page.locator('input[type="file"]');
    await fileInput.setInputFiles(filePath);
    await page.waitForTimeout(1_000);
  }

  // Submit the command
  await page.keyboard.press('Enter');

  const result: SendFlowResult = { submitted: true };

  if (!waitForResponse) {
    return result;
  }

  // Wait for the bot response
  try {
    const responseEmbed = await waitForBotResponse(page, responseTimeoutMs);
    result.responseText = responseEmbed.text;
    result.qurlLink = responseEmbed.qurlLink;
    result.errorMessage = responseEmbed.errorMessage;
  } catch (error) {
    result.errorMessage = `Timed out waiting for bot response: ${error}`;
  }

  return result;
}

async function fillSlashCommandOption(
  page: Page,
  optionName: string,
  value: string,
): Promise<void> {
  // The slash command option inputs appear inline
  // Tab to next option or click on it
  const optionInput = page.locator(DiscordSelectors.slashCommand.optionInput)
    .or(page.locator(`[aria-label*="${optionName}" i]`))
    .or(page.locator(`[placeholder*="${optionName}" i]`));

  if (await optionInput.first().isVisible({ timeout: 3_000 }).catch(() => false)) {
    await optionInput.first().click();
    await optionInput.first().fill(value);
  } else {
    // Try typing with tab navigation
    await page.keyboard.press('Tab');
    await page.waitForTimeout(300);
    await page.keyboard.type(value);
  }

  await page.waitForTimeout(500);
}

interface BotResponse {
  text: string;
  qurlLink?: string;
  errorMessage?: string;
}

async function waitForBotResponse(
  page: Page,
  timeoutMs: number,
): Promise<BotResponse> {
  return retry(
    async () => {
      // Get the latest message in the channel
      const messages = page.locator(DiscordSelectors.channel.messageItem);
      const lastMessage = messages.last();
      await expect(lastMessage).toBeVisible({ timeout: 5_000 });

      // Check for embed
      const embed = lastMessage.locator(DiscordSelectors.embed.container);
      const hasEmbed = await embed.first().isVisible({ timeout: 3_000 }).catch(() => false);

      let text = '';
      let qurlLink: string | undefined;
      let errorMessage: string | undefined;

      if (hasEmbed) {
        text = await embed.first().textContent() ?? '';

        // Look for QURL link in embed
        const linkMatch = text.match(/https?:\/\/(?:qurl\.io|qurl\.dev)\/[a-zA-Z0-9_-]+/);
        if (linkMatch) {
          qurlLink = linkMatch[0];
        }

        // Check if it's an error
        const embedTitle = await embed.first()
          .locator(DiscordSelectors.embed.title)
          .textContent()
          .catch(() => '');
        if (embedTitle?.toLowerCase().includes('error')) {
          errorMessage = text;
        }
      } else {
        text = await lastMessage
          .locator(DiscordSelectors.channel.messageContent)
          .textContent() ?? '';
      }

      if (!text) {
        throw new Error('No response text found yet');
      }

      return { text, qurlLink, errorMessage };
    },
    {
      maxAttempts: Math.ceil(timeoutMs / 3_000),
      initialDelayMs: 3_000,
      backoffMultiplier: 1,
    },
  );
}
