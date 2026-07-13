/**
 * Concurrency tests — parallel operations, race conditions.
 *
 * This suite mints more live resources per run than any other (50 in the
 * stress test alone), so every minted target_url carries the per-run
 * nonce and every resource_id is tracked for best-effort revocation in
 * afterAll — see helpers/cleanup.ts (pattern from
 * location-variants.test.ts / #657).
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

describe('Concurrency: Parallel Minting', () => {
  test('mint 10 links in parallel', async () => {
    const promises = Array.from({ length: 10 }, (_, i) =>
      qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
        target_url: withRunNonce(`https://example.com/parallel-${i}`),
        expires_in: '1h',
      }),
    );
    const results = await Promise.all(promises);
    results.forEach((r) => tracked.track(r.resource_id));
    expect(results).toHaveLength(10);

    // All should have unique resource_ids
    const ids = new Set(results.map((r) => r.resource_id));
    expect(ids.size).toBe(10);

    // All should have valid links
    results.forEach((r) => {
      expect(r.qurl_link).toBeDefined();
      expect(r.resource_id).toMatch(/^r_/);
    });
  });

  test('mint 50 links in parallel (stress)', async () => {
    const promises = Array.from({ length: 50 }, (_, i) =>
      qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
        target_url: withRunNonce(`https://example.com/stress-${i}`),
        expires_in: '30m',
      }),
    );
    const results = await Promise.allSettled(promises);
    const fulfilled = results.filter(
      (r): r is PromiseFulfilledResult<qurl.MintResult> => r.status === 'fulfilled',
    );
    fulfilled.forEach((r) => tracked.track(r.value.resource_id));

    console.log(`50 parallel mints: ${fulfilled.length} succeeded, ${results.length - fulfilled.length} failed`);
    // At least 80% should succeed (allow for rate limiting)
    expect(fulfilled.length).toBeGreaterThanOrEqual(40);
  }, 30_000);
});

describe('Concurrency: Parallel Access', () => {
  test('10 parallel accesses of a max_uses:1 link never over-count server-side', async () => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: withRunNonce('https://example.com/parallel-access'),
      max_uses: 1,
    });
    tracked.track(result.resource_id);

    const results = await Promise.all(
      Array.from({ length: 10 }, () => qurl.accessLink(result.qurl_link)),
    );

    // The SPA itself serves every GET (the token lives in the fragment
    // and resolves client-side), so all ten come back 200 …
    expect(results.filter((r) => r.ok)).toHaveLength(10);

    // … which means the RACE assertion lives at the status endpoint, not
    // in the HTTP statuses: however the ten parallel GETs interleaved, a
    // max_uses:1 link must never record more than one consumed use. Two
    // valid pass shapes, same as smoke.test.ts's second-access guard:
    //   (a) status 404s (→ null) — resource fully consumed;
    //   (b) status resolves with use_count <= 1 — a bare fetch of the SPA
    //       may or may not consume a use (the knock is client-side JS),
    //       but ten racing accesses must never advance the counter past
    //       the cap.
    // getLinkStatusOrNull rethrows any non-404 failure, so an unrelated
    // auth/network error still fails loudly instead of false-passing.
    const status = await qurl.getLinkStatusOrNull(env.MINT_API_URL, env.QURL_API_KEY, result.qurl_id);
    if (status !== null) {
      expect(status.use_count).toBeLessThanOrEqual(1);
    }
  });
});

describe('Concurrency: Mint and Revoke Race', () => {
  test('revoke immediately after mint', async () => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: withRunNonce('https://example.com/mint-then-revoke'),
    });
    tracked.track(result.resource_id);
    // Revoke immediately
    const revoked = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, result.resource_id);
    if (revoked) tracked.untrack(result.resource_id);
    expect(revoked).toBe(true);
  });

  test('parallel mint and revoke of different resources', async () => {
    // Mint 5 resources
    const minted = await Promise.all(
      Array.from({ length: 5 }, (_, i) =>
        qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
          target_url: withRunNonce(`https://example.com/race-${i}`),
        }),
      ),
    );
    minted.forEach((r) => tracked.track(r.resource_id));

    // Revoke all in parallel; untrack only what verifiably revoked so a
    // partial failure leaves the stragglers for afterAll.
    const revokes = await Promise.all(
      minted.map((r) => qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, r.resource_id)),
    );
    minted.forEach((r, i) => {
      if (revokes[i]) tracked.untrack(r.resource_id);
    });
    expect(revokes.every((r) => r === true)).toBe(true);
  });
});
