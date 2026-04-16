/**
 * Group B — File upload tests with various MIME types and sizes.
 */

import { test, expect } from '../../../fixtures/discord-fixtures';
import { executeSendFlow } from '../../../helpers/send-flow';
import { testFilePath, getMimeMatrix } from '../../../helpers/test-data-lookup';

test.describe('File Uploads', () => {
  test.beforeEach(async ({ channelPage, env }) => {
    await channelPage.goto(env.DISCORD_GUILD_ID, env.DISCORD_CHANNEL_ID);
  });

  test('upload a small text file', async ({ senderPage, env }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/file-test',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      filePath: testFilePath('test-1kb.txt'),
      message: 'Small text file attached',
    });

    expect(result.submitted).toBe(true);
    expect(result.errorMessage).toBeUndefined();
  });

  test('upload a medium CSV file', async ({ senderPage, env }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/csv-test',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      filePath: testFilePath('test-200kb.csv'),
      message: 'CSV data file',
    });

    expect(result.submitted).toBe(true);
    expect(result.errorMessage).toBeUndefined();
  });

  test('upload a JSON file', async ({ senderPage, env }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/json-test',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      filePath: testFilePath('test-2kb.json'),
      message: 'JSON config file',
    });

    expect(result.submitted).toBe(true);
    expect(result.errorMessage).toBeUndefined();
  });

  test('upload a binary file', async ({ senderPage, env }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/bin-test',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      filePath: testFilePath('test-256kb.bin'),
      message: 'Binary file test',
    });

    expect(result.submitted).toBe(true);
    expect(result.errorMessage).toBeUndefined();
  });

  test('upload an SVG file', async ({ senderPage, env }) => {
    const result = await executeSendFlow(senderPage, {
      location: 'https://example.com/svg-test',
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      filePath: testFilePath('test-3kb.svg'),
      message: 'SVG image file',
    });

    expect(result.submitted).toBe(true);
    expect(result.errorMessage).toBeUndefined();
  });
});
