/**
 * Concurrency tests — parallel operations, race conditions.
 */

// TODO: Add afterAll cleanup to revoke/delete test resources

import * as dotenv from 'dotenv';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { loadEnv } from '../helpers/env';
import * as qurl from '../helpers/qurl-api';

const env = loadEnv();

describe('Concurrency: Parallel Minting', () => {
  test('mint 10 links in parallel', async () => {
    const promises = Array.from({ length: 10 }, (_, i) =>
      qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
        target_url: `https://example.com/parallel-${i}`,
        expires_in: '1h',
      }),
    );
    const results = await Promise.all(promises);
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
        target_url: `https://example.com/stress-${i}`,
        expires_in: '30m',
      }),
    );
    const results = await Promise.allSettled(promises);
    const fulfilled = results.filter((r) => r.status === 'fulfilled');
    const rejected = results.filter((r) => r.status === 'rejected');

    console.log(`50 parallel mints: ${fulfilled.length} succeeded, ${rejected.length} failed`);
    // At least 80% should succeed (allow for rate limiting)
    expect(fulfilled.length).toBeGreaterThanOrEqual(40);
  }, 30_000);
});

describe('Concurrency: Parallel Access', () => {
  test('access same link 10 times in parallel', async () => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: 'https://example.com/parallel-access',
      max_uses: 1,
    });

    const promises = Array.from({ length: 10 }, () =>
      qurl.accessLink(result.qurl_link),
    );
    const results = await Promise.all(promises);

    // SPA always returns 200 (fragment-based), but server-side
    // should only count 1 real access
    const ok = results.filter((r) => r.ok);
    expect(ok.length).toBe(10); // SPA always 200
  });
});

describe('Concurrency: Mint and Revoke Race', () => {
  test('revoke immediately after mint', async () => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: 'https://example.com/mint-then-revoke',
    });
    // Revoke immediately
    const revoked = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, result.resource_id);
    expect(revoked).toBe(true);
  });

  test('parallel mint and revoke of different resources', async () => {
    // Mint 5 resources
    const minted = await Promise.all(
      Array.from({ length: 5 }, (_, i) =>
        qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
          target_url: `https://example.com/race-${i}`,
        }),
      ),
    );

    // Revoke all in parallel
    const revokes = await Promise.all(
      minted.map((r) => qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, r.resource_id)),
    );
    expect(revokes.every((r) => r === true)).toBe(true);
  });
});
