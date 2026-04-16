/**
 * Page Object Model for Discord voice channel interactions.
 */

import { Page, expect } from '@playwright/test';
import { DiscordSelectors } from '../helpers/discord-selectors';

export class DiscordVoicePage {
  constructor(private readonly page: Page) {}

  /**
   * Join a voice channel by clicking its name in the sidebar.
   */
  async joinVoiceChannel(channelName: string): Promise<void> {
    const voiceChannel = this.page.locator(
      DiscordSelectors.channel.channelLink(channelName),
    );
    await expect(voiceChannel).toBeVisible({ timeout: 10_000 });
    await voiceChannel.click();

    // Wait for voice connection
    await expect(
      this.page.locator(DiscordSelectors.voice.voiceConnected),
    ).toBeVisible({ timeout: 15_000 });
  }

  /**
   * Disconnect from voice.
   */
  async disconnect(): Promise<void> {
    const disconnectBtn = this.page.locator(DiscordSelectors.voice.disconnectButton);
    if (await disconnectBtn.isVisible({ timeout: 3_000 }).catch(() => false)) {
      await disconnectBtn.click();
    }
  }

  /**
   * Check if currently in a voice channel.
   */
  async isConnected(): Promise<boolean> {
    try {
      await this.page.locator(DiscordSelectors.voice.voiceConnected).waitFor({
        state: 'visible',
        timeout: 3_000,
      });
      return true;
    } catch {
      return false;
    }
  }

  /**
   * Toggle mute.
   */
  async toggleMute(): Promise<void> {
    const muteBtn = this.page.locator(DiscordSelectors.voice.muteButton);
    await expect(muteBtn).toBeVisible({ timeout: 5_000 });
    await muteBtn.click();
  }

  /**
   * Toggle camera.
   */
  async toggleCamera(): Promise<void> {
    const cameraBtn = this.page.locator(DiscordSelectors.voice.videoButton);
    await expect(cameraBtn).toBeVisible({ timeout: 5_000 });
    await cameraBtn.click();
  }
}
