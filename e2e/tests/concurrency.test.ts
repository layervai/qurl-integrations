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
    // At least 80% should succeed. Env assumption: the target's mint
    // rate limit tolerates a 50-burst with <=20% shed — true of the
    // test/staging envs this suite targets. If a stricter limiter makes
    // this flake, tune the threshold alongside that env change rather
    // than loosening it blind (a big drop in fulfilled mints is signal).
    expect(fulfilled.length).toBeGreaterThanOrEqual(40);
  }, 30_000);
});

describe('Concurrency: Parallel Access', () => {
  test('10 parallel accesses of a max_uses:1 link: SPA serves all, counter stays coherent', async () => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: withRunNonce('https://example.com/parallel-access'),
      max_uses: 1,
    });
    tracked.track(result.resource_id);

    // Canary before the race: the never-accessed link must be visible at
    // the status endpoint, otherwise the post-race dual-shape check below
    // would pass vacuously through its 404 arm (same guard as
    // smoke.test.ts's canary test — see the rationale there; bounded
    // poll for mint-read lag).
    const pre = await qurl.pollLinkStatus(
      env.MINT_API_URL, env.QURL_API_KEY, result.qurl_id, (s) => s !== null,
    );
    qurl.assertStatusVisible(pre, `pre-race qurl_id ${result.qurl_id}`);

    const results = await Promise.all(
      Array.from({ length: 10 }, () => qurl.accessLink(result.qurl_link)),
    );

    // The SPA itself serves every GET (the token lives in the fragment
    // and resolves client-side), so all ten come back 200 …
    expect(results.filter((r) => r.ok)).toHaveLength(10);

    // … and the counter check lives at the status endpoint. Honest
    // scope: the knock that consumes a use is client-side JS, so ten
    // bare GETs may consume nothing at all — this pins parallel SPA
    // serving plus counter COHERENCE (never past the max_uses:1 cap),
    // not consumption-race enforcement. Racing real knocks would need
    // ten parallel browsers; the single-consumption guarantee itself is
    // pinned knock-driven in file-revoke.test.ts ("a consumed one-time
    // link does not serve a second knock"), and URL-target knock
    // coverage is #951. Two valid coherence shapes, same as
    // smoke.test.ts's second-access guard:
    //   (a) status 404s (→ null) — resource fully consumed;
    //   (b) status resolves with use_count <= 1 — the racing accesses
    //       never advanced the counter past the cap.
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
    // Revoke immediately (tracked.revoke also drops it from the afterAll
    // ledger on success)
    const revoked = await tracked.revoke(result.resource_id);
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

    // Revoke all in parallel; tracked.revoke drops each verifiably-revoked
    // id from the afterAll ledger, so a partial failure leaves the
    // stragglers for cleanup.
    const revokes = await Promise.all(
      minted.map((r) => tracked.revoke(r.resource_id)),
    );
    expect(revokes.every((r) => r === true)).toBe(true);
  });
});
