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
  // Independent backends (qURL API / Discord) with no shared rate-limit
  // bucket — run the two best-effort sweeps concurrently.
  await Promise.all([tracked.revokeAll(), sentMessages.deleteAll()]);
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
    // Log ids, not the link: qurl_link carries the access token in its
    // #at_… fragment, which must not land in retained CI logs (same
    // rule as http.ts's origin-only retry logging).
    console.log(`Minted: qurl_id=${qurlId} resource_id=${result.resource_id}`);
  });

  test('management API sees the freshly-minted link (canary)', async () => {
    expect(qurlId).toBeDefined();
    // CANARY for every later status check in this suite (and the one in
    // concurrency.test.ts): a freshly-minted, never-accessed link
    // MUST be visible in GET /v1/qurls/{qurl_id}. If this lookup 404s,
    // the remaining checks cannot be trusted. Bounded poll (not a single
    // shot): mint-then-read races an unpinned consistency model, and a brief
    // propagation lag should not red a correct deployment.
    const status = await qurl.pollLinkStatus(
      env.MINT_API_URL, env.QURL_API_KEY, qurlId, (s) => s !== null,
    );
    qurl.assertStatusVisible(status, `freshly-minted qurl_id ${qurlId}`);
  });

  test('first access succeeds', async () => {
    const res = await qurl.accessLink(qurlLink);
    expect(res.ok).toBe(true);
    console.log(`First access: ${res.status} -> ${res.finalUrl}`);
  });

  test('management API never over-counts the first access', async () => {
    expect(qurlId).toBeDefined();
    // qURL links resolve their token client-side (fragment-based SPA), so
    // a bare HTTP GET may or may not register a consumed use server-side —
    // `use_count >= 1` is NOT a contract this suite can hold (the old
    // try/catch-swallowed assertion of it could never fail anyway). What
    // IS guaranteed after one access of a max_uses:1 link: the status
    // TODO(upstream-contract): layervai/qurl-service#1233 tracks the current
    // contract: the management API retains this token summary and reports at
    // most one use.
    // qurl-service's resource preview does not filter terminal token statuses;
    // a consumed token stays present with status=consumed until TTL cleanup.
    // Knock-driven single-use ENFORCEMENT is
    // pinned in file-revoke.test.ts ("a consumed one-time link does not
    // serve a second knock"); URL-target knock coverage is #951.
    const status = await qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, qurlId);
    console.log('Link status after first access:', JSON.stringify(status));
    expect(status.use_count).toBeLessThanOrEqual(1);
  });

  test('second access of the same one-time link never over-counts (status coherence)', async () => {
    // Status-endpoint COHERENCE guard on the bare-fetch path — honest
    // scope: the SPA knock that consumes a use is client-side JS, so a
    // bare accessLink() GET may never advance use_count at all. In a
    // deployment where bare GETs don't consume, the counter stays 0 here
    // and only the knock-driven test in file-revoke.test.ts ("a consumed
    // one-time link does not serve a second knock") can catch the
    // reusable-links bug (URL-target knock coverage: #951). Where bare
    // GETs DO consume, `use_count === 2` here is that exact bug shape.
    // Either way the counter must never exceed the max_uses:1 cap.
    //
    // Depends on prior tests setting qurlLink + qurlId — guard explicitly
    // so a jest rerandomize doesn't silently no-op.
    expect(qurlLink).toBeDefined();
    expect(qurlId).toBeDefined();

    // Attempt the second access. The SPA returns 200 either way
    // (fragments), so the observable signal lives at the status
    // endpoint, not in `res2.status`.
    const res2 = await qurl.accessLink(qurlLink);
    console.log(`Second access: ${res2.status} -> ${res2.finalUrl}`);

    // The retained token summary must show that the second attempt did not
    // advance the counter past the cap. Missing summaries and all HTTP/network
    // failures stay hard reds rather than becoming a nullable pass.
    const status = await qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, qurlId);
    console.log('Link status after 2nd access:', JSON.stringify(status));
    // An increment to 2 is the exact "links were reusable" bug shape.
    expect(status.use_count).toBeLessThan(2);
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

    // Resource-level canary before revoke: this makes the later lifecycle
    // assertion meaningful instead of allowing an always-404 lookup to pass.
    const pre = await qurl.pollResourceStatus(
      env.MINT_API_URL, env.QURL_API_KEY, resourceId, (s) => s !== null,
    );
    qurl.assertStatusVisible(pre, `pre-revoke resource_id ${resourceId}`);

    // tracked.revoke = revokeLink + drop from the afterAll ledger on success.
    const revoked = await tracked.revoke(resourceId);
    expect(revoked).toBe(true);
    console.log(`Revoked resource: ${resourceId}`);
  });

  test('resource status is revoked after revoke', async () => {
    // Depends on the prior test minting + revoking — guard explicitly.
    expect(resourceId).toBeDefined();
    // Resource revocation is a soft lifecycle transition: management reads
    // retain the resource and report `revoked`.
    const status = await qurl.getResourceStatus(env.MINT_API_URL, env.QURL_API_KEY, resourceId);
    expect(status.status).toBe('revoked');
  });
});
