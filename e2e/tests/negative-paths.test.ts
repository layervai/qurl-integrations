/**
 * Negative path tests — error handling and edge cases.
 */

// TODO: Add afterAll cleanup to revoke/delete test resources

import * as dotenv from 'dotenv';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { loadEnv } from '../helpers/env';
import * as qurl from '../helpers/qurl-api';

const env = loadEnv();

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
  test('access non-existent qurl path returns SPA (fragment-based)', async () => {
    const res = await qurl.accessLink('https://qurl.link.layerv.xyz/#at_nonexistent_token_xxx');
    // SPA always loads; client-side handles invalid tokens
    expect(res.status).toBe(200);
  });

  test('access completely invalid URL gracefully', async () => {
    try {
      await qurl.accessLink('not-a-url');
    } catch (e) {
      expect((e as Error).message).toMatch(/URL|fetch/i);
    }
  });
});

describe('Negative: Invalid Revocation', () => {
  test('revoke non-existent resource returns false', async () => {
    const result = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, 'r_nonexistent_xxx');
    expect(result).toBe(false);
  });

  test('revoke with invalid API key fails', async () => {
    const minted = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: 'https://example.com/revoke-bad-key',
    });
    const result = await qurl.revokeLink(env.MINT_API_URL, 'invalid-key', minted.resource_id);
    expect(result).toBe(false);
  });
});

describe('Negative: Status Checks', () => {
  test('status of non-existent resource throws 404', async () => {
    await expect(
      qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, 'r_nonexistent'),
    ).rejects.toThrow(/404/);
  });
});
