/**
 * Link lifecycle tests:
 * - Mint with various expiry values
 * - One-time access enforcement (via resource status API)
 * - Revocation
 * - Expiry enforcement (using short TTLs)
 * - Multiple links to same target
 *
 * Every minted target_url carries the per-run nonce and every
 * resource_id is tracked for best-effort revocation in afterAll — see
 * helpers/cleanup.ts (pattern from location-variants.test.ts / #657).
 */

import * as dotenv from 'dotenv';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { trackedQurlResources, withRunNonce } from '../helpers/cleanup';
import { loadEnv } from '../helpers/env';
import * as qurl from '../helpers/qurl-api';

const env = loadEnv();
const tracked = trackedQurlResources(env);

afterAll(() => tracked.revokeAll());

describe('Link Lifecycle: Minting', () => {
  test('mint link with default expiry', async () => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: withRunNonce('https://example.com/default-expiry'),
    });
    tracked.track(result.resource_id);
    expect(result.qurl_link).toContain('qurl');
    expect(result.resource_id).toMatch(/^r_/);
    expect(result.qurl_id).toMatch(/^q_/);
  });

  test.each(['30m', '1h', '6h', '24h', '7d'])('mint with expiry=%s', async (expiry) => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: withRunNonce(`https://example.com/expiry-${expiry}`),
      expires_in: expiry,
    });
    tracked.track(result.resource_id);
    expect(result.qurl_link).toBeDefined();
    expect(result.resource_id).toBeDefined();
  });

  test('mint with custom description', async () => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: withRunNonce('https://example.com/described'),
      description: 'Test link with special chars: <>&"\'日本語',
    });
    tracked.track(result.resource_id);
    expect(result.qurl_link).toBeDefined();
  });

  test('mint two links to same target get distinct qurl_ids', async () => {
    // Same nonced URL for both mints (the run nonce is a per-run
    // constant) — the dedup-by-target scenario this test exists to pin.
    const target = withRunNonce('https://example.com/same-target');
    const r1 = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: target,
    });
    tracked.track(r1.resource_id);
    const r2 = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: target,
    });
    tracked.track(r2.resource_id);
    // API may deduplicate resource_id for same target (the tracker
    // dedupes too), but qurl_ids should differ
    expect(r1.qurl_id).not.toBe(r2.qurl_id);
  });
});

describe('Link Lifecycle: Access', () => {
  test('first access returns 200 (SPA loads)', async () => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: withRunNonce('https://example.com/access-test'),
      max_uses: 1,
    });
    tracked.track(result.resource_id);
    const res = await qurl.accessLink(result.qurl_link);
    expect(res.status).toBe(200);
  });

  test('access without redirect returns SPA HTML', async () => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: withRunNonce('https://example.com/no-redirect'),
    });
    tracked.track(result.resource_id);
    const res = await qurl.accessLinkNoRedirect(result.qurl_link);
    // Fragment-based links: SPA page returns 200 directly
    expect([200, 301, 302, 303]).toContain(res.status);
  });

  test('tampered link token still loads SPA (validation is client-side)', async () => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: withRunNonce('https://example.com/tampered'),
    });
    tracked.track(result.resource_id);
    // Modify one char in the fragment
    const tampered = result.qurl_link.slice(0, -1) + 'X';
    const res = await qurl.accessLink(tampered);
    // SPA loads (200) but client-side JS will show error
    expect(res.status).toBe(200);
  });
});

describe('Link Lifecycle: Revocation', () => {
  test('revoke by resource_id succeeds', async () => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: withRunNonce('https://example.com/revoke-test'),
    });
    tracked.track(result.resource_id);
    // tracked.revoke = revokeLink + drop from the afterAll ledger on success.
    const revoked = await tracked.revoke(result.resource_id);
    expect(revoked).toBe(true);
  });

  test('resource returns 404 after revocation', async () => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: withRunNonce('https://example.com/revoke-then-status'),
    });
    tracked.track(result.resource_id);

    // Pre-revoke canary (mirror of smoke's qurl_id canary, here for the
    // resource_id key — #950): the live resource must be VISIBLE at the
    // status endpoint before revocation. Without this, the post-revoke
    // 404 assertions in this suite would pass vacuously if the endpoint
    // keyed on qurl_id and 404'd every resource_id lookup unconditionally.
    const pre = await qurl.getLinkStatusOrNull(env.MINT_API_URL, env.QURL_API_KEY, result.resource_id);
    expect(pre).not.toBeNull();

    const revoked = await tracked.revoke(result.resource_id);
    expect(revoked).toBe(true);

    await expect(
      qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, result.resource_id),
    ).rejects.toThrow(/404/);
  });

  test('double revoke is idempotent (no error)', async () => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: withRunNonce('https://example.com/double-revoke'),
    });
    tracked.track(result.resource_id);
    const first = await tracked.revoke(result.resource_id);
    expect(first).toBe(true);

    // Second revoke must not throw (404 or 200 both acceptable) — the
    // same contract file-revoke.test.ts pins for file resources; raw
    // revokeLink here since the resource already left the ledger. (The
    // previous `expect(typeof second).toBe('boolean')` could never fail:
    // revokeLink is typed Promise<boolean>.)
    await expect(
      qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, result.resource_id),
    ).resolves.not.toThrow();

    // And the resource is still revoked after the redundant call.
    await expect(
      qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, result.resource_id),
    ).rejects.toThrow(/404/);
  });
});

describe('Link Lifecycle: Expiry', () => {
  // TODO: This test waits only 3s for a 1m TTL link and catches all errors,
  // making it a false-green no-op. To properly test expiry, either use a
  // sub-second TTL (if the API supports it) or poll until the status shows expired.
  test.todo('link with short TTL expires after timeout');
});
