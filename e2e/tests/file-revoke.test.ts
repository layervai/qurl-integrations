/**
 * File upload → view → revoke → 404 E2E flow.
 *
 * Covers the revoke path for actual file resources (images/PDFs/etc.) — the
 * other revoke tests (smoke.test.ts, link-lifecycle.test.ts) only exercise
 * URL-based qurls (`target_url=https://example.com/...`). This file fills the
 * gap by uploading a real file via the connector's `/upload` endpoint, minting
 * a QURL for it, accessing the fileviewer `/view/:md5` page (expects 200),
 * revoking by resource_id, and re-accessing (expects 404).
 *
 * Without this coverage, a regression that leaves files accessible after
 * revoke would ship silently — the revoke API would still return true, but
 * the viewer URL would keep serving the content.
 */

import * as dotenv from 'dotenv';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { loadEnv } from '../helpers/env';
import * as qurl from '../helpers/qurl-api';

const env = loadEnv();

/** Upload an in-memory image buffer and return the viewer URL + resource_id. */
async function uploadImage(
  buf: Buffer,
  filename: string,
  mime: string,
): Promise<{ viewerUrl: string; resourceId: string }> {
  const form = new FormData();
  // Buffer.from(array) returns `Buffer<ArrayBufferLike>` which TS won't accept
  // as a BlobPart (it narrows to `ArrayBufferView<ArrayBuffer>`). Cast through
  // unknown — the runtime value is a valid ArrayBufferView and the lib.dom
  // type is overly strict here.
  form.append('file', new Blob([buf as unknown as BlobPart], { type: mime }), filename);
  const res = await fetch(`${env.UPLOAD_API_URL}/upload`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${env.QURL_API_KEY}` },
    body: form,
  });
  if (!res.ok) throw new Error(`Upload failed: ${res.status} ${await res.text()}`);
  const data = await res.json() as any;
  return { viewerUrl: data.resource_url, resourceId: data.resource_id };
}

// Minimal valid 1x1 PNG (pixel=white). Avoids on-disk fixtures so this test
// runs wherever the e2e package is checked out without extra setup.
const ONE_PIXEL_PNG = Buffer.from([
  0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
  0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
  0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
  0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
  0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41,
  0x54, 0x78, 0x9c, 0x63, 0xfa, 0xff, 0xff, 0x3f,
  0x00, 0x05, 0xfe, 0x02, 0xfe, 0xdc, 0xcc, 0x59,
  0xe7, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e,
  0x44, 0xae, 0x42, 0x60, 0x82,
]);

describe('File Revoke', () => {
  test('upload file → view 200 → revoke → view 404', async () => {
    const upload = await uploadImage(ONE_PIXEL_PNG, 'revoke-test.png', 'image/png');
    expect(upload.viewerUrl).toContain('/view/');
    expect(upload.resourceId).toMatch(/^r_/);

    const before = await fetch(upload.viewerUrl);
    expect(before.status).toBe(200);

    const revoked = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resourceId);
    expect(revoked).toBe(true);

    const after = await fetch(upload.viewerUrl);
    expect(after.status).toBe(404);
  });

  test('double revoke on file is idempotent and viewer stays 404', async () => {
    const upload = await uploadImage(ONE_PIXEL_PNG, 'double-revoke.png', 'image/png');

    const first = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resourceId);
    expect(first).toBe(true);

    // Second revoke: API accepts 404 or 200 as non-failure — mirrors the
    // existing idempotency test in link-lifecycle.test.ts
    const second = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resourceId);
    expect(typeof second).toBe('boolean');

    const after = await fetch(upload.viewerUrl);
    expect(after.status).toBe(404);
  });
});
