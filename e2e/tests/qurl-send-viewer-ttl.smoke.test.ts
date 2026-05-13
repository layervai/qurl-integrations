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
 * must succeed and return a qurl_link. Closes that coverage gap so the
 * next regression of this shape doesn't ship green.
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
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { loadEnv } from '../helpers/env';
import * as qurl from '../helpers/qurl-api';

const env = loadEnv();

describe('Smoke: /qurl send with viewer_ttl_seconds (Snapchat path)', () => {
  let tempFile: string;

  beforeAll(() => {
    // Tiny fixture — the file content doesn't matter; we're pinning
    // the connector → qurl-service contract, not the upload pipeline.
    tempFile = path.join(__dirname, 'qurl-send-viewer-ttl.smoke.fixture.txt');
    fs.writeFileSync(tempFile, `e2e smoke fixture ${new Date().toISOString()}\n`);
  });

  afterAll(() => {
    try { fs.unlinkSync(tempFile); } catch { /* best-effort */ }
  });

  test('connector accepts viewer_ttl_seconds=30 and qurl-service mints a link', async () => {
    // 30s = below the historical 5-minute floor. If qurl-service ever
    // re-tightens the floor without coordinating with the connector,
    // this assertion catches it before users do.
    const result = await qurl.uploadFile(
      env.UPLOAD_API_URL,
      tempFile,
      env.QURL_API_KEY,
      { viewerTtlSeconds: 30 },
    );

    // Connector contract: success path returns hash + resource_id +
    // qurl_link. Failure path returns hash + a non-null `error` (the
    // 2026-05-13 incident shape). Pin both — if `error` is set we
    // know the contract regression is back.
    expect(result.hash).toMatch(/^[0-9a-f]{32}$/);
    expect(result.error).toBeUndefined();
    expect(result.qurl_link).toBeDefined();
    expect(result.qurl_link).toMatch(/^https?:\/\//);
  });

  test('connector accepts no viewer_ttl_seconds (control case)', async () => {
    // Without viewer_ttl_seconds the connector sends empty
    // session_duration → qurl-service applies its server default. This
    // path was the only one exercised by smoke before #283. Kept
    // alongside the TTL'd case so a regression that breaks the
    // no-TTL path is also caught here, not silently in another file.
    const result = await qurl.uploadFile(
      env.UPLOAD_API_URL,
      tempFile,
      env.QURL_API_KEY,
    );
    expect(result.hash).toMatch(/^[0-9a-f]{32}$/);
    expect(result.error).toBeUndefined();
    expect(result.qurl_link).toBeDefined();
  });
});
