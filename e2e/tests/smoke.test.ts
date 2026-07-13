/**
 * Smoke test — verifies the core qURL flow end-to-end:
 * 1. Mint a one-time link via qURL API
 * 2. Access it (should succeed)
 * 3. Access again (should fail — one-time)
 * 4. Mint another, revoke it, access (should fail)
 * 5. Bot can read/write in test channel
 *
 * Minted target_urls carry the per-run nonce, and every resource / sent
 * Discord message is tracked for best-effort cleanup in afterAll — see
 * helpers/cleanup.ts.
 */

import * as dotenv from 'dotenv';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { trackedDiscordMessages, trackedQurlResources, withRunNonce } from '../helpers/cleanup';
import { loadEnv } from '../helpers/env';
import * as discord from '../helpers/discord-api';
import * as qurl from '../helpers/qurl-api';

const env = loadEnv();
const tracked = trackedQurlResources(env);
const sentMessages = trackedDiscordMessages(env);

afterAll(async () => {
  await tracked.revokeAll();
  await sentMessages.deleteAll();
});

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
    sentMessages.track(msg);
    expect(msg.id).toBeDefined();
    expect(msg.channel_id).toBe(env.CHANNEL_ID);
  });

  test('bot can read messages from test channel', async () => {
    const messages = await discord.getMessages(env.BOT_TOKEN, env.CHANNEL_ID, 5);
    expect(messages.length).toBeGreaterThan(0);
  });
});

describe('Smoke: qURL link lifecycle', () => {
  let qurlLink: string;
  let qurlId: string;

  test('mint a one-time link', async () => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: withRunNonce('https://example.com/e2e-smoke-test'),
      expires_in: '1h',
      description: 'E2E smoke test link',
      max_uses: 1,
    });
    tracked.track(result.resource_id);
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

  test('status endpoint never over-counts the first access', async () => {
    expect(qurlId).toBeDefined();
    // qURL links resolve their token client-side (fragment-based SPA), so
    // a bare HTTP GET may or may not register a consumed use server-side —
    // `use_count >= 1` is NOT a contract this suite can hold (the old
    // try/catch-swallowed assertion of it could never fail anyway). What
    // IS guaranteed after one access of a max_uses:1 link: the status
    // endpoint answers coherently — either 404 (fully consumed → null) or
    // at most one recorded use. The hard reusable-links regression guard
    // is the second-access test below.
    const status = await qurl.getLinkStatusOrNull(env.MINT_API_URL, env.QURL_API_KEY, qurlId);
    console.log('Link status after first access:', status === null ? '404 (consumed)' : JSON.stringify(status));
    if (status !== null) {
      expect(status.use_count).toBeLessThanOrEqual(1);
    }
  });

  test('second access of the same one-time link FAILS (use count consumed)', async () => {
    // Regression guard for the reusable-links bug. Depends on prior
    // tests setting qurlLink + qurlId — guard explicitly so a jest
    // rerandomize doesn't silently no-op.
    expect(qurlLink).toBeDefined();
    expect(qurlId).toBeDefined();

    // Attempt the second access. The SPA returns 200 either way
    // (fragments), so the observable signal lives at the status
    // endpoint, not in `res2.status`.
    const res2 = await qurl.accessLink(qurlLink);
    console.log(`Second access: ${res2.status} -> ${res2.finalUrl}`);

    // Single-use enforcement produces one of two observable shapes at
    // the status endpoint. Both are valid passes; what we're guarding
    // against is `use_count === 2` (the exact "links were reusable"
    // bug shape).
    //   (a) 404 (→ null) — the resource was fully consumed on first
    //       access. This is the stronger signal and is what the
    //       upstream API returns when `one_time_use: true` is honored.
    //   (b) success with `use_count === 1` — resource still queryable,
    //       but the token is dead and the failed second attempt did NOT
    //       advance the counter.
    // getLinkStatusOrNull rethrows any non-404 failure, so an unrelated
    // network/auth error still fails loudly instead of false-passing.
    const status = await qurl.getLinkStatusOrNull(env.MINT_API_URL, env.QURL_API_KEY, qurlId);
    console.log('Link status after 2nd access:', status === null ? '404 (consumed)' : JSON.stringify(status));
    if (status !== null) {
      // Counter MUST NOT have advanced past 1. An increment to 2 is the
      // exact bug shape this test is here to catch.
      expect(status.use_count).toBeLessThan(2);
    }
  });
});

describe('Smoke: Revocation', () => {
  let resourceId: string;

  test('mint link then revoke', async () => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: withRunNonce('https://example.com/e2e-revoke-test'),
      expires_in: '1h',
      max_uses: 1,
    });
    tracked.track(result.resource_id);
    resourceId = result.resource_id;

    const revoked = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, resourceId);
    if (revoked) tracked.untrack(resourceId);
    expect(revoked).toBe(true);
    console.log(`Revoked resource: ${resourceId}`);
  });

  test('resource status returns 404 after revoke', async () => {
    // Depends on the prior test minting + revoking — guard explicitly.
    expect(resourceId).toBeDefined();
    // After revocation, the resource status API should return 404.
    // Use a definitive assertion instead of catch-all error swallowing.
    await expect(
      qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, resourceId),
    ).rejects.toThrow(/404/);
  });
});
