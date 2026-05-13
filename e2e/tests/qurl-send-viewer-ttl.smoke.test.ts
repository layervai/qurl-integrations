/**
 * Smoke for the Snapchat-style auto-destruct path (qurl-integrations#283).
 *
 * 2026-05-13 incident: PR qurl-integrations-infra#540 added connector-side
 * `session_duration` forwarding. The deployed prod qurl-service still
 * had a 5-minute floor. Every /qurl send with a small viewer_ttl_seconds
 * returned "Failed to create links" — for hours, without the prod e2e
 * smoke catching it. The smoke only exercised the no-TTL path, which
 * takes the connector's "use server default" branch and bypasses the
 * session_duration → qurl-service contract entirely.
 *
 * This test pins the contract: an upload with viewer_ttl_seconds=30
 * must succeed (HTTP 2xx) and return hash + resource_id. Closes that
 * coverage gap so the next regression of this shape doesn't ship green.
 * (The /upload endpoint does NOT return qurl_link — minting is a
 * separate /mint-link call; the smoke pins the upload contract only.)
 *
 * What this catches:
 *   - qurl-service tightens its session_duration floor again
 *   - connector stops forwarding session_duration but starts setting
 *     a different field that downstream rejects
 *   - any change to the connector→qurl-service contract that breaks
 *     the small-TTL path while leaving the no-TTL path working
 *
 * What this does NOT catch:
 *   - per-recipient view-window enforcement (separate Traefik layer)
 *   - the file actually rendering with a working countdown (UI layer)
 *
 * Both of those have separate dedicated tests in this suite.
 */

import * as dotenv from 'dotenv';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { loadEnv } from '../helpers/env';
import * as qurl from '../helpers/qurl-api';

const env = loadEnv();

describe('Smoke: /qurl send with viewer_ttl_seconds (Snapchat path)', () => {
  // Per-test unique fixture content to avoid the connector's md5
  // content-addressed dedup shortcut. If both tests uploaded the
  // same bytes, the second call would short-circuit to the cached
  // resource from the first call (minted WITH viewer_ttl_seconds=30)
  // and the "control" no-TTL case would silently fail to exercise
  // the "use server default" branch it's supposed to pin.
  const tempFiles: string[] = [];
  function freshFixture(label: string): string {
    // os.tmpdir() rather than __dirname so a killed-mid-test run doesn't
    // leak fixture files into the e2e source tree (and from there into
    // `git status`). afterAll cleanup is still best-effort.
    const f = path.join(
      os.tmpdir(),
      `qurl-send-viewer-ttl.smoke.fixture.${label}.${Date.now()}.${Math.random().toString(36).slice(2)}.txt`,
    );
    fs.writeFileSync(f, `e2e smoke fixture ${label} ${new Date().toISOString()} ${Math.random()}\n`);
    tempFiles.push(f);
    return f;
  }

  afterAll(() => {
    for (const f of tempFiles) {
      try { fs.unlinkSync(f); } catch { /* best-effort */ }
    }
  });

  test('connector accepts viewer_ttl_seconds=30 and qurl-service mints a link', async () => {
    const tempFile = freshFixture('ttl30');
    // 30s = below the historical 5-minute floor. If qurl-service ever
    // re-tightens the floor without coordinating with the connector,
    // this assertion catches it before users do.
    const result = await qurl.uploadFile(
      env.UPLOAD_API_URL,
      tempFile,
      env.QURL_API_KEY,
      { viewerTtlSeconds: 30 },
    );

    // Connector /upload contract: hash + resource_id (NOT qurl_link —
    // minting is a separate /mint-link call; the bot's connector.js
    // confirms /upload never returns qurl_link).
    //
    // The 2026-05-13 incident shape (4xx from qurl-service) is caught
    // *implicitly* via uploadFile's `!res.ok → throw` at qurl-api.ts:51:
    // Jest fails the test on the thrown "Upload failed: 400 ..." and
    // the connector regression is surfaced. We do NOT assert on
    // result.error here because the helper never returns with that
    // field set; if qurl-service ever changes the regression shape to
    // 200-with-error-body, uploadFile would need to be widened too.
    expect(result.hash).toMatch(/^[0-9a-f]{32}$/);
    expect(result.resource_id).toBeDefined();
  });

  test('connector accepts no viewer_ttl_seconds (control case)', async () => {
    // Without viewer_ttl_seconds the connector sends empty
    // session_duration → qurl-service applies its server default. This
    // path was the only one exercised by smoke before #283. Kept
    // alongside the TTL'd case so a regression that breaks the
    // no-TTL path is also caught here, not silently in another file.
    //
    // Fresh fixture (different bytes from the TTL'd test) so the
    // connector's content-addressed dedup doesn't return the cached
    // TTL'd resource.
    const tempFile = freshFixture('no-ttl');
    const result = await qurl.uploadFile(
      env.UPLOAD_API_URL,
      tempFile,
      env.QURL_API_KEY,
    );
    expect(result.hash).toMatch(/^[0-9a-f]{32}$/);
    expect(result.resource_id).toBeDefined();
  });
});
