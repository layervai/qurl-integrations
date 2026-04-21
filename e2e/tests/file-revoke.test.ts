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

interface UploadResponse {
  resource_url: string;
  resource_id: string;
}

/** Upload an in-memory image buffer and return the viewer URL + resource_id. */
async function uploadImage(
  buf: Uint8Array,
  filename: string,
  mime: string,
): Promise<{ viewerUrl: string; resourceId: string }> {
  const form = new FormData();
  // `@types/node` types Uint8Array with `ArrayBufferLike` which narrower
   // than `BlobPart`'s `ArrayBufferView<ArrayBuffer>` expects. Runtime is
   // fine; cast to satisfy the compiler.
  form.append('file', new Blob([buf as BlobPart], { type: mime }), filename);
  const res = await fetch(`${env.UPLOAD_API_URL}/upload`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${env.QURL_API_KEY}` },
    body: form,
  });
  if (!res.ok) throw new Error(`Upload failed: ${res.status} ${await res.text()}`);
  const data = (await res.json()) as Partial<UploadResponse>;
  if (!data.resource_url || !data.resource_id) {
    throw new Error(`Unexpected upload response: ${JSON.stringify(data)}`);
  }
  return { viewerUrl: data.resource_url, resourceId: data.resource_id };
}

// Minimal valid 1x1 PNG (pixel=white). Avoids on-disk fixtures so this test
// runs wherever the e2e package is checked out without extra setup.
const ONE_PIXEL_PNG = new Uint8Array([
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

    // The contract for a second revoke is "does not throw, viewer stays
    // 404". `revokeLink` is typed Promise<boolean>, so asserting `typeof`
    // would be tautological (what link-lifecycle.test.ts does). The
    // meaningful assertion is the one below: viewer URL returns 404
    // whether the API returned true (200/204) or false (404 from the
    // already-revoked resource).
    await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resourceId);

    const after = await fetch(upload.viewerUrl);
    expect(after.status).toBe(404);
  });
});
