/**
 * Negative path tests — error handling and edge cases.
 *
 * The two tests that mint REAL resources (to derive a live origin / to
 * exercise a bad-key revoke) nonce-tag their target_urls and track the
 * resource_ids for best-effort revocation in afterAll — see
 * helpers/cleanup.ts.
 */

import { generateKeyPairSync } from 'crypto';
import * as dotenv from 'dotenv';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { trackedQurlResources, withRunNonce } from '../helpers/cleanup';
import { loadEnv } from '../helpers/env';
import * as qurl from '../helpers/qurl-api';

const env = loadEnv();
const tracked = trackedQurlResources(env);
let nonExistentPublicResourceId: string;

beforeAll(() => {
  // Generate a fresh, structurally valid P-256 SPKI public key. It is
  // astronomically unlikely to identify a provisioned resource, so this
  // exercises "well-formed but absent" rather than malformed-ID rejection.
  const { publicKey } = generateKeyPairSync('ec', { namedCurve: 'prime256v1' });
  nonExistentPublicResourceId = Buffer.from(
    publicKey.export({ type: 'spki', format: 'der' }),
  ).toString('base64url');
});

afterAll(() => tracked.revokeAll());

describe('Negative: Invalid Mint Requests', () => {
  test('mint without target_url fails', async () => {
    await expect(
      qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {}),
    ).rejects.toThrow(/400|422|target/i);
  });

  test('mint with invalid API key fails', async () => {
    await expect(
      qurl.mintLink(env.MINT_API_URL, 'invalid-key-xxx', {
        target_url: 'https://example.com/bad-key',
      }),
    ).rejects.toThrow(/401|403|unauthorized/i);
  });

  test('mint with empty target_url fails', async () => {
    await expect(
      qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
        target_url: '',
      }),
    ).rejects.toThrow(/400|422|invalid|required/i);
  });
});

describe('Negative: Invalid Access', () => {
  test('access non-existent qURL path returns SPA (fragment-based)', async () => {
    // Derive the resolution origin from a real minted link so this test
    // carries no hardcoded environment host.
    const minted = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: withRunNonce('https://example.com/negative-access'),
    });
    tracked.track(minted.resource_id);
    const origin = new URL(minted.qurl_link).origin;
    const res = await qurl.accessLink(`${origin}/#at_nonexistent_token_xxx`);
    // SPA always loads; client-side handles invalid tokens
    expect(res.status).toBe(200);
  });

  test('access of an unparseable URL rejects', async () => {
    // fetch() can't parse this at all, so accessLink MUST reject. Assert
    // the rejection — the previous try/catch-around-expect shape ran zero
    // assertions (and passed) whenever nothing threw.
    await expect(qurl.accessLink('not-a-url')).rejects.toThrow(/URL|fetch|invalid/i);
  });
});

describe('Negative: Invalid Revocation', () => {
  test('revoke non-existent resource returns false', async () => {
    const result = await qurl.revokeLink(
      env.MINT_API_URL, env.QURL_API_KEY, nonExistentPublicResourceId,
    );
    expect(result).toBe(false);
  });

  test('revoke with invalid API key fails', async () => {
    const minted = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: withRunNonce('https://example.com/revoke-bad-key'),
    });
    // Track before the bad-key attempt: that revoke MUST NOT delete the
    // resource, so afterAll (with the valid key) owns the real cleanup.
    tracked.track(minted.resource_id);
    const result = await qurl.revokeLink(env.MINT_API_URL, 'invalid-key', minted.resource_id);
    expect(result).toBe(false);
  });
});

describe('Negative: Status Checks', () => {
  test('status of non-existent resource throws 404', async () => {
    await expect(
      qurl.getResourceStatus(env.MINT_API_URL, env.QURL_API_KEY, nonExistentPublicResourceId),
    ).rejects.toThrow(/404/);
  });
});
