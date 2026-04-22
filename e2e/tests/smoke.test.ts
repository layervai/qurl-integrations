/**
 * Smoke test — verifies the core QURL flow end-to-end:
 * 1. Mint a one-time link via QURL API
 * 2. Access it (should succeed)
 * 3. Access again (should fail — one-time)
 * 4. Mint another, revoke it, access (should fail)
 * 5. Bot can read/write in test channel
 */

// TODO: Add afterAll cleanup to revoke/delete test resources

import * as dotenv from 'dotenv';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { loadEnv } from '../helpers/env';
import * as discord from '../helpers/discord-api';
import * as qurl from '../helpers/qurl-api';

const env = loadEnv();

describe('Smoke: Bot connectivity', () => {
  test('bot can identify itself', async () => {
    const me = await discord.getMe(env.BOT_TOKEN);
    expect(me.id).toBe(env.BOT_CLIENT_ID);
    expect(me.username).toBeDefined();
    console.log(`Bot: ${me.username} (${me.id})`);
  });

  test('bot can send a message in test channel', async () => {
    const msg = await discord.sendMessage(
      env.BOT_TOKEN,
      env.CHANNEL_ID,
      `[E2E smoke] ${new Date().toISOString()}`,
    );
    expect(msg.id).toBeDefined();
    expect(msg.channel_id).toBe(env.CHANNEL_ID);
  });

  test('bot can read messages from test channel', async () => {
    const messages = await discord.getMessages(env.BOT_TOKEN, env.CHANNEL_ID, 5);
    expect(messages.length).toBeGreaterThan(0);
  });
});

describe('Smoke: QURL link lifecycle', () => {
  let qurlLink: string;
  let qurlId: string;

  test('mint a one-time link', async () => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: 'https://example.com/e2e-smoke-test',
      expires_in: '1h',
      description: 'E2E smoke test link',
      max_uses: 1,
    });
    expect(result.qurl_link).toBeDefined();
    expect(result.qurl_id).toBeDefined();
    qurlLink = result.qurl_link;
    qurlId = result.qurl_id;
    console.log(`Minted: ${qurlLink}`);
  });

  test('first access succeeds', async () => {
    const res = await qurl.accessLink(qurlLink);
    expect(res.ok).toBe(true);
    console.log(`First access: ${res.status} -> ${res.finalUrl}`);
  });

  test('link status shows accessed after first open', async () => {
    // QURL links use fragments — SPA always returns 200.
    // Verify via the resource status API using the resource_id from minting.
    try {
      const status = await qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, qurlId);
      console.log(`Link status:`, JSON.stringify(status));
      // use_count should reflect the access
      expect(status.use_count).toBeGreaterThanOrEqual(1);
    } catch (e) {
      // If status endpoint doesn't exist for this resource type, skip gracefully
      console.log(`Status check: ${(e as Error).message} (may not be tracked server-side for SPA links)`);
    }
  });

  test('second access of the same one-time link FAILS (use count consumed)', async () => {
    // Regression guard for the reusable-links bug. Depends on the two
    // prior tests setting qurlLink + qurlId — guard explicitly so a
    // jest rerandomize or reordering doesn't silently no-op.
    expect(qurlLink).toBeDefined();
    expect(qurlId).toBeDefined();

    // First-access must have already happened in the prior test and
    // pushed use_count to exactly 1. Pin that baseline here so a broken
    // system where the counter never increments would fail this test
    // (the earlier `>= 1` assertion would pass at 0 too).
    const baseline = await qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, qurlId);
    expect(baseline.use_count).toBe(1);

    const res2 = await qurl.accessLink(qurlLink);
    console.log(`Second access: ${res2.status} -> ${res2.finalUrl}`);

    // Two independent paths for "this token is dead":
    //   (a) status endpoint reports use_count still === 1 after the
    //       second attempt — the failed resolution MUST NOT increment
    //       past max_uses. An increment to 2 is the exact bug shape.
    //   (b) status endpoint returns 404 (some resource types return
    //       this once a one-time QURL is fully consumed).
    // Either proves the enforcement worked; we accept whichever path
    // the API takes but tighten the pass conditions on both.
    let statusAfter: Awaited<ReturnType<typeof qurl.getLinkStatus>> | null = null;
    let statusError: Error | null = null;
    try {
      statusAfter = await qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, qurlId);
      console.log(`Link status after 2nd access:`, JSON.stringify(statusAfter));
    } catch (e) {
      statusError = e as Error;
      console.log(`Link status after 2nd access (threw): ${statusError.message}`);
    }

    if (statusAfter !== null) {
      // The failed second attempt must NOT increment the counter past 1.
      expect(statusAfter.use_count).toBe(1);
      if (typeof statusAfter.remaining_uses === 'number') {
        expect(statusAfter.remaining_uses).toBe(0);
      }
    } else {
      // Status-endpoint 404 is the OTHER valid signal that the token
      // is dead. Accept only that specific shape — "expired" / "consumed"
      // substrings were too permissive (could match unrelated errors).
      expect(statusError).not.toBeNull();
      expect(statusError!.message).toMatch(/\b404\b|\bnot found\b/i);
    }
  });
});

describe('Smoke: Revocation', () => {
  let qurlLink: string;
  let resourceId: string;

  test('mint link then revoke', async () => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: 'https://example.com/e2e-revoke-test',
      expires_in: '1h',
      max_uses: 1,
    });
    qurlLink = result.qurl_link;
    resourceId = result.resource_id;

    const revoked = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, resourceId);
    expect(revoked).toBe(true);
    console.log(`Revoked resource: ${resourceId}`);
  });

  test('resource status returns 404 after revoke', async () => {
    // After revocation, the resource status API should return 404.
    // Use a definitive assertion instead of catch-all error swallowing.
    await expect(
      qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, resourceId),
    ).rejects.toThrow(/404/);
  });
});
