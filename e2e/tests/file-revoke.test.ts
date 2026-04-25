/**
 * File upload → revoke → status-404 E2E flow.
 *
 * Covers the revoke path for actual file resources (images/PDFs/etc.) — the
 * other revoke tests (smoke.test.ts, link-lifecycle.test.ts) only exercise
 * URL-based qURLs (`target_url=https://example.com/...`). This file fills
 * the gap by uploading a real file via the connector's `/upload` endpoint,
 * verifying the viewer URL responds, revoking by resource_id, and confirming
 * the qURL resource is marked revoked via getLinkStatus.
 *
 * Revoke semantics: `DELETE /v1/resources/{id}` kills the qURL-layer token
 * chain (qurl.link / qurl.site). The underlying md5-addressed fileviewer
 * URL is NOT gated by the qURL API — it remains reachable to anyone who
 * knows the md5 until the S3 lifecycle (~8 days) expires it. The canonical
 * revoke assertion is `getLinkStatus` returning 404, matching the pattern
 * at smoke.test.ts:103.
 *
 * Without this coverage, a regression in the revoke API for
 * connector-uploaded resources would ship silently (URL-mint revoke
 * already covered by smoke.test.ts).
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

/** Upload an in-memory image buffer and return the viewer URL + resource_id.
 *
 * Retries on 429 rate-limit bursts from the upstream qURL API — the connector
 * responds with `{success:true, resource_url:..., error:"QURL creation failed:
 * ... 429 ..."}` and no `resource_id`. Treat that as transient and back off.
 */
async function uploadImage(
  buf: Uint8Array,
  filename: string,
  mime: string,
): Promise<{ viewerUrl: string; resourceId: string }> {
  const maxAttempts = 4;
  const baseDelayMs = 1500;
  for (let i = 0; i < maxAttempts; i++) {
    const form = new FormData();
    // `@types/node` types Uint8Array as `ArrayBufferLike`, which is narrower
    // than `BlobPart`'s `ArrayBufferView<ArrayBuffer>` expects. Runtime is
    // fine; cast to satisfy the compiler.
    form.append('file', new Blob([buf as BlobPart], { type: mime }), filename);
    const res = await fetch(`${env.UPLOAD_API_URL}/upload`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${env.QURL_API_KEY}` },
      body: form,
    });
    if (!res.ok) throw new Error(`Upload failed: ${res.status} ${await res.text()}`);
    const data = (await res.json()) as Partial<UploadResponse> & { error?: string };
    if (data.resource_url && data.resource_id) {
      return { viewerUrl: data.resource_url, resourceId: data.resource_id };
    }
    const errStr = typeof data.error === 'string' ? data.error : '';
    if (/429|rate.limit|too many requests/i.test(errStr)) {
      await new Promise((r) => setTimeout(r, baseDelayMs * (i + 1)));
      continue;
    }
    throw new Error(`Unexpected upload response: ${JSON.stringify(data)}`);
  }
  throw new Error(`uploadImage: still rate-limited after ${maxAttempts} attempts`);
}

// Valid 1x1 transparent PNG (standard test fixture — widely used, CRC/zlib
// checks pass). Exercises the image-upload path including the fileviewer's
// watermark-stamping flow end-to-end, so a regression in image handling
// fails this test loudly.
//
// Source: the canonical "smallest valid PNG" (67 bytes) — base64 form is
// `iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=`.
// Hex-literal spelling below keeps the test self-contained and easy to
// diff if bytes need swapping for a different fixture.
const ONE_PIXEL_PNG = Uint8Array.from(
  Buffer.from(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=',
    'base64',
  ),
);

describe('File Revoke', () => {
  test('upload file → view 200 → revoke → getLinkStatus 404', async () => {
    const upload = await uploadImage(ONE_PIXEL_PNG, 'revoke-test.png', 'image/png');
    expect(upload.viewerUrl).toContain('/view/');
    expect(upload.resourceId).toMatch(/^r_/);

    const before = await fetch(upload.viewerUrl);
    expect(before.status).toBe(200);

    const revoked = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resourceId);
    expect(revoked).toBe(true);

    // Canonical post-revoke assertion today — matches smoke.test.ts:103.
    // The md5-addressed fileviewer URL is NOT yet gated by the qURL-layer
    // revoke (product decision to FIX that is in infra#139, implementation
    // folded into infra#93's synchronous-delete-on-revoke scope).
    // TODO(infra#93/#139): add `expect((await fetch(upload.viewerUrl)).status).toBe(404)`
    // after the revoke-triggered S3 delete wiring ships.
    await expect(
      qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, upload.resourceId),
    ).rejects.toThrow(/404/);
  });

  test('double revoke on file is idempotent', async () => {
    const upload = await uploadImage(ONE_PIXEL_PNG, 'double-revoke.png', 'image/png');

    const first = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resourceId);
    expect(first).toBe(true);

    // Second revoke should not throw. `revokeLink` is typed Promise<boolean>
    // and the smoke/link-lifecycle sibling test uses the same "does not
    // throw" contract. Covered by `resolves.not.toThrow()` for the
    // explicit contract expression.
    await expect(
      qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resourceId),
    ).resolves.not.toThrow();

    // Resource is still revoked after the redundant call.
    await expect(
      qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, upload.resourceId),
    ).rejects.toThrow(/404/);
  });
});
