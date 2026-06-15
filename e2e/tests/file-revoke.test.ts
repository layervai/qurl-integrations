/**
 * File upload → revoke → status-404 E2E flow.
 *
 * Covers the revoke path for actual file resources (images/PDFs/etc.) — the
 * other revoke tests (smoke.test.ts, link-lifecycle.test.ts) only exercise
 * URL-based qURLs (`target_url=https://example.com/...`). This file fills
 * the gap by uploading a real file via the connector's `/upload` endpoint,
 * minting a per-recipient render-at-mint view (`POST /api/mint_link`) and
 * confirming it serves through the tunnel, then revoking by resource_id and
 * confirming the qURL resource is marked revoked via getLinkStatus.
 *
 * Revoke semantics: `DELETE /v1/resources/{id}` kills the qURL-layer token
 * chain (the qurl.link → NHP knock → tunnel view). #1111 removed the legacy
 * md5-addressed fileviewer URL, so under the render-at-mint tunnel the only
 * access is the minted qurl.link, which this revoke invalidates. The canonical
 * revoke assertion is `getLinkStatus` returning 404, matching smoke.test.ts.
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
import { viewViaQurlLink } from '../helpers/tunnelView';

const env = loadEnv();

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
    const upload = await qurl.uploadFile(
      env.UPLOAD_API_URL,
      { bytes: ONE_PIXEL_PNG, filename: 'revoke-test.png', mime: 'image/png' },
      env.QURL_API_KEY,
    );
    expect(upload.resource_id).toMatch(/^r_/);

    // Mint the RECIPIENT view, then drive the real knock against THAT link.
    // Why not reuse upload.qurl_link: in render-at-mint mode the /upload qurl_link
    // is the connector's "never-shared per-upload qURL" (its knock 404s).
    // Recipients only ever reach content through a per-recipient view minted via
    // POST /api/mint_link, whose knock resolves to
    // r_<tunnel>.qurl.site/views/<mint-id>. expires_at is required by
    // render-at-mint (1h here; the connector clamps to its own cap).
    const expiresAt = new Date(Date.now() + 60 * 60 * 1000).toISOString();
    const minted = await qurl.mintConnectorView(env.UPLOAD_API_URL, upload.resource_id, env.QURL_API_KEY, {
      expiresAt,
      oneTimeUse: true,
    });

    // View through the REAL recipient path: qurl.link → NHP knock → tunnel view.
    // #1111 decommissioned the legacy fileviewer host, so only a real browser
    // completes the SPA-driven knock (the SPA reads the #at_ fragment in JS). A
    // 200 means the baked image served end-to-end through the tunnel.
    const view = await viewViaQurlLink(minted.qurl_link);
    expect(view.status).toBe(200);

    const revoked = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id);
    expect(revoked).toBe(true);

    // Canonical post-revoke assertion: the resource status API reports 404 once
    // revoked (matches smoke.test.ts). (Synchronous delete-on-revoke of the
    // baked object is tracked separately under the render-at-mint cleanup path.)
    await expect(
      qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id),
    ).rejects.toThrow(/404/);
    // Generous timeout: connector mint + headless-browser knock (cold chromium
    // launch + navigation + the helper's own 30s tunnel-view budget) on CI.
  }, 90_000);

  test('double revoke on file is idempotent', async () => {
    const upload = await qurl.uploadFile(
      env.UPLOAD_API_URL,
      { bytes: ONE_PIXEL_PNG, filename: 'double-revoke.png', mime: 'image/png' },
      env.QURL_API_KEY,
    );

    const first = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id);
    expect(first).toBe(true);

    // Second revoke should not throw. `revokeLink` is typed Promise<boolean>
    // and the smoke/link-lifecycle sibling test uses the same "does not
    // throw" contract. Covered by `resolves.not.toThrow()` for the
    // explicit contract expression.
    await expect(
      qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id),
    ).resolves.not.toThrow();

    // Resource is still revoked after the redundant call.
    await expect(
      qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, upload.resource_id),
    ).rejects.toThrow(/404/);
  });
});
