/**
 * Group C — Location URL variants: HTTPS, S3, GCS, encoded, unicode, etc.
 */

import { test, expect } from '../../../fixtures/discord-fixtures';
import { executeSendFlow } from '../../../helpers/send-flow';
import { getLocations } from '../../../helpers/test-data-lookup';

test.describe('Location Variants', () => {
  test.beforeEach(async ({ channelPage, env }) => {
    await channelPage.goto(env.DISCORD_GUILD_ID, env.DISCORD_CHANNEL_ID);
  });

  const locations = getLocations();

  for (const loc of locations.filter((l) => l.type === 'https').slice(0, 5)) {
    test(`send with ${loc.label} (${loc.id})`, async ({ senderPage, env }) => {
      const result = await executeSendFlow(senderPage, {
        location: loc.url,
        recipient: env.DISCORD_RECIPIENT_USERNAME,
        message: `Testing location: ${loc.label}`,
      });

      expect(result.submitted).toBe(true);
      expect(result.errorMessage).toBeUndefined();
    });
  }

  test('send with S3 URI', async ({ senderPage, env }) => {
    const s3Loc = locations.find((l) => l.id === 's3-basic')!;
    const result = await executeSendFlow(senderPage, {
      location: s3Loc.url,
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'S3 location test',
    });

    expect(result.submitted).toBe(true);
  });

  test('send with GCS URI', async ({ senderPage, env }) => {
    const gcsLoc = locations.find((l) => l.id === 'gcs-basic')!;
    const result = await executeSendFlow(senderPage, {
      location: gcsLoc.url,
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'GCS location test',
    });

    expect(result.submitted).toBe(true);
  });

  test('send with URL-encoded characters', async ({ senderPage, env }) => {
    const encodedLoc = locations.find((l) => l.id === 'https-encoded')!;
    const result = await executeSendFlow(senderPage, {
      location: encodedLoc.url,
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'Encoded URL test',
    });

    expect(result.submitted).toBe(true);
    expect(result.errorMessage).toBeUndefined();
  });

  test('send with unicode in URL path', async ({ senderPage, env }) => {
    const unicodeLoc = locations.find((l) => l.id === 'https-unicode')!;
    const result = await executeSendFlow(senderPage, {
      location: unicodeLoc.url,
      recipient: env.DISCORD_RECIPIENT_USERNAME,
      message: 'Unicode URL test',
    });

    expect(result.submitted).toBe(true);
  });
});
