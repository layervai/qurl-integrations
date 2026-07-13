/**
 * Location variant tests — mint qURL links targeting various URL formats.
 *
 * Every minted resource is tracked and revoked in afterAll. To make that
 * cleanup unambiguously safe — these fixtures use GENERIC target_urls that a
 * parallel suite or real usage could also mint, and qurl-service dedups by
 * (owner_id, target_url, type) — each target_url carries a per-run nonce so the
 * resources this run revokes are only ever its own. Same uncleaned-resource
 * hygiene fix as the google-maps 409 (qurl-integrations#657).
 *
 * The nonce + tracked-revocation mechanics originated here and now live in
 * helpers/cleanup.ts (withRunNonce / trackedQurlResources), shared with the
 * sibling suites. Two properties this file's fixtures depend on are pinned
 * by the helper's contract: the nonce is URL-safe (alphanumeric + hyphen)
 * and is inserted WITHOUT a `new URL()` round-trip, so the raw unicode /
 * special chars the `unicode-path` and `special-chars` fixtures exercise
 * are never re-escaped. Unlike fixed-fixture file tests (e.g. file-revoke's
 * one-pixel PNG) — whose byte-identical uploads could share an md5 — every
 * variant URL here is already mutually distinct, so the run nonce alone
 * guarantees uniqueness.
 */

import * as dotenv from 'dotenv';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { trackedQurlResources, withRunNonce } from '../helpers/cleanup';
import { loadEnv } from '../helpers/env';
import * as qurl from '../helpers/qurl-api';

const env = loadEnv();

// Every resource minted this run, revoked in afterAll so they don't persist
// and strand at a shared target_url on a future connector type change (the
// #657 disease). DELETE /v1/resources/{id} needs only `qurl:write`.
const tracked = trackedQurlResources(env);

afterAll(() => tracked.revokeAll());

const LOCATION_VARIANTS = [
  { id: 'https-basic', url: 'https://example.com' },
  { id: 'https-path', url: 'https://example.com/path/to/resource' },
  { id: 'https-query', url: 'https://example.com/search?q=hello&lang=en' },
  { id: 'https-fragment', url: 'https://example.com/page#section-2' },
  { id: 'https-port', url: 'https://example.com:8443/api' },
  // http-plain and localhost are tested separately as expected rejections
  // NOTE: maps RENDER support was dropped (product decision); these two now
  // exercise only URL-shape escaping (a long https URL with @coords/path, and a
  // short redirect URL), NOT maps. Kept as generic URL fixtures — the `maps`
  // label is historical and rides the connector maps dead-code cleanup.
  { id: 'google-maps-full', url: 'https://www.google.com/maps/place/Eiffel+Tower/@48.8584,2.2945,17z/' },
  { id: 'google-maps-short', url: 'https://maps.app.goo.gl/abc123' },
  { id: 'url-encoded', url: 'https://example.com/path%20with%20spaces?q=%E4%B8%AD%E6%96%87' },
  { id: 'unicode-path', url: 'https://example.com/日本語/パス' },
  // Kept well under qurl-service's 2048-char MaxTargetURLLength so the appended
  // run nonce (~38 chars: `?_e2e_nonce=` + the per-run nonce) still fits — this
  // exercises the long-path mint, not the length rejection boundary.
  { id: 'long-url', url: 'https://example.com/' + 'a'.repeat(1900) },
  { id: 'special-chars', url: 'https://example.com/path?a=1&b=<>&c="quotes"' },
  { id: 'ipv4', url: 'https://93.184.216.34/test' },
  // localhost tested separately as expected rejection
  { id: 'deep-path', url: 'https://example.com/a/b/c/d/e/f/g/h/i/j/k/l/m/n' },
];

describe('Location Variants', () => {
  test.each(LOCATION_VARIANTS)('mint link for $id', async ({ id, url }) => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: withRunNonce(url),
      expires_in: '1h',
      description: `E2E location variant: ${id}`,
    });
    // Track before the assertions so a successfully-minted resource is always
    // revoked in afterAll even if an expect() below throws — otherwise it leaks
    // past cleanup, reintroducing the exact uncleaned-prod-state class this fix
    // closes. (Matches the track-before-validate pattern in file-revoke.test.ts;
    // track() itself guards undefined, so a malformed mint fails only its
    // assertion below, not also a spurious revoke warning in afterAll.)
    tracked.track(result.resource_id);
    expect(result.qurl_link).toBeDefined();
    // Public resource IDs are opaque qurl-service-owned values.
    expect(result.resource_id).toBeTruthy();
    // resource_id only — qurl_link carries the access token in its
    // #at_… fragment and must not land in retained CI logs.
    console.log(`${id}: resource_id=${result.resource_id}`);
  });

  test('HTTP (non-HTTPS) URL is rejected', async () => {
    await expect(
      qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
        target_url: 'http://example.com/insecure',
      }),
    ).rejects.toThrow(/400|HTTPS/i);
  });

  test('localhost URL is rejected', async () => {
    await expect(
      qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
        target_url: 'http://localhost:3000/dev',
      }),
    ).rejects.toThrow(/400|HTTPS/i);
  });

  test('access a minted location link returns 200', async () => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: withRunNonce('https://example.com/access-location-test'),
      expires_in: '1h',
    });
    tracked.track(result.resource_id);
    const res = await qurl.accessLink(result.qurl_link);
    expect(res.status).toBe(200);
  });
});
