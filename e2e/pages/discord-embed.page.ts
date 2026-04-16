/**
 * Page Object Model for parsing Discord embed messages.
 */

import { Locator } from '@playwright/test';
import { DiscordSelectors } from '../helpers/discord-selectors';

export interface EmbedData {
  title?: string;
  description?: string;
  fields: Array<{ name: string; value: string }>;
  footer?: string;
  thumbnailSrc?: string;
  imageSrc?: string;
  color?: string;
}

export class DiscordEmbedPage {
  constructor(private readonly embedLocator: Locator) {}

  /**
   * Parse all data from the embed.
   */
  async parse(): Promise<EmbedData> {
    const title = await this.embedLocator
      .locator(DiscordSelectors.embed.title)
      .textContent()
      .catch(() => undefined);

    const description = await this.embedLocator
      .locator(DiscordSelectors.embed.description)
      .textContent()
      .catch(() => undefined);

    const footer = await this.embedLocator
      .locator(DiscordSelectors.embed.footer)
      .textContent()
      .catch(() => undefined);

    const thumbnailSrc = await this.embedLocator
      .locator(DiscordSelectors.embed.thumbnail + ' img')
      .getAttribute('src')
      .catch(() => undefined);

    const imageSrc = await this.embedLocator
      .locator(DiscordSelectors.embed.image + ' img')
      .getAttribute('src')
      .catch(() => undefined);

    // Parse fields
    const fieldEls = this.embedLocator.locator(DiscordSelectors.embed.field);
    const fieldCount = await fieldEls.count();
    const fields: Array<{ name: string; value: string }> = [];

    for (let i = 0; i < fieldCount; i++) {
      const name = (await fieldEls.nth(i).locator(DiscordSelectors.embed.fieldName).textContent()) ?? '';
      const value = (await fieldEls.nth(i).locator(DiscordSelectors.embed.fieldValue).textContent()) ?? '';
      fields.push({ name: name.trim(), value: value.trim() });
    }

    return {
      title: title?.trim(),
      description: description?.trim(),
      fields,
      footer: footer?.trim(),
      thumbnailSrc: thumbnailSrc ?? undefined,
      imageSrc: imageSrc ?? undefined,
    };
  }

  /**
   * Get a specific field value by name.
   */
  async getFieldValue(fieldName: string): Promise<string | undefined> {
    const data = await this.parse();
    const field = data.fields.find(
      (f) => f.name.toLowerCase() === fieldName.toLowerCase(),
    );
    return field?.value;
  }

  /**
   * Check if the embed contains a QURL link.
   */
  async getQurlLink(): Promise<string | undefined> {
    const data = await this.parse();
    const allText = [
      data.title,
      data.description,
      ...data.fields.map((f) => f.value),
      data.footer,
    ]
      .filter(Boolean)
      .join(' ');

    const match = allText.match(/https?:\/\/(?:qurl\.io|qurl\.dev)\/[a-zA-Z0-9_-]+/);
    return match?.[0];
  }

  /**
   * Check if this is an error embed.
   */
  async isError(): Promise<boolean> {
    const data = await this.parse();
    return (
      data.title?.toLowerCase().includes('error') === true ||
      data.description?.toLowerCase().includes('error') === true
    );
  }
}
