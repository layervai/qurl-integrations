/**
 * Location variant tests — mint QURL links targeting various URL formats.
 */

import * as dotenv from 'dotenv';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { loadEnv } from '../helpers/env';
import * as qurl from '../helpers/qurl-api';

const env = loadEnv();

const LOCATION_VARIANTS = [
  { id: 'https-basic', url: 'https://example.com' },
  { id: 'https-path', url: 'https://example.com/path/to/resource' },
  { id: 'https-query', url: 'https://example.com/search?q=hello&lang=en' },
  { id: 'https-fragment', url: 'https://example.com/page#section-2' },
  { id: 'https-port', url: 'https://example.com:8443/api' },
  // http-plain and localhost are tested separately as expected rejections
  { id: 'google-maps-full', url: 'https://www.google.com/maps/place/Eiffel+Tower/@48.8584,2.2945,17z/' },
  { id: 'google-maps-short', url: 'https://maps.app.goo.gl/abc123' },
  { id: 'url-encoded', url: 'https://example.com/path%20with%20spaces?q=%E4%B8%AD%E6%96%87' },
  { id: 'unicode-path', url: 'https://example.com/日本語/パス' },
  { id: 'long-url', url: 'https://example.com/' + 'a'.repeat(2000) },
  { id: 'special-chars', url: 'https://example.com/path?a=1&b=<>&c="quotes"' },
  { id: 'ipv4', url: 'https://93.184.216.34/test' },
  // localhost tested separately as expected rejection
  { id: 'deep-path', url: 'https://example.com/a/b/c/d/e/f/g/h/i/j/k/l/m/n' },
];

describe('Location Variants', () => {
  test.each(LOCATION_VARIANTS)('mint link for $id', async ({ id, url }) => {
    const result = await qurl.mintLink(env.MINT_API_URL, env.QURL_API_KEY, {
      target_url: url,
      expires_in: '1h',
      description: `E2E location variant: ${id}`,
    });
    expect(result.qurl_link).toBeDefined();
    expect(result.resource_id).toMatch(/^r_/);
    console.log(`${id}: ${result.qurl_link}`);
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
      target_url: 'https://example.com/access-location-test',
      expires_in: '1h',
    });
    const res = await qurl.accessLink(result.qurl_link);
    expect(res.status).toBe(200);
  });
});
