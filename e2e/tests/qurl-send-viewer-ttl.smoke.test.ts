/**
 * Upload- and mint-time session_duration floor probes (#283, #631).
 * The connector must accept 1s at both boundaries. A successful request does
 * not prove that the duration was enforced by the served view. The live run
 * must identify the rejecting step if either boundary regresses.
 */

import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';

import { trackedQurlResources } from '../helpers/cleanup';
import { loadEnv } from '../helpers/env';
import * as qurl from '../helpers/qurl-api';

const env = loadEnv();
const tracked = trackedQurlResources(env);

describe('Smoke: /qurl send with viewer_ttl_seconds (Snapchat path)', () => {
  // Per-test unique fixture content to avoid the connector's md5
  // content-addressed dedup shortcut. If both tests uploaded the
  // same bytes, the second call would short-circuit to the cached
  // resource from the first call (uploaded WITH viewer_ttl_seconds=1)
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

  // Fail the hook on resource cleanup errors, but always remove local fixtures.
  // A stale temp file is not a leaked live resource, so unlink remains best-effort.
  afterAll(async () => {
    try {
      await tracked.revokeAll();
    } finally {
      for (const f of tempFiles) {
        try { fs.unlinkSync(f); } catch { /* best-effort */ }
      }
    }
  });

  // Acceptance pins the floor; enforcement by the viewer is a separate journey.
  test('connector accepts upload with viewer_ttl_seconds=1', async () => {
    const tempFile = freshFixture('ttl1');
    const result = await qurl.uploadFile(
      env.UPLOAD_API_URL,
      tempFile,
      env.QURL_API_KEY,
      { viewerTtlSeconds: 1 },
    );
    tracked.track(result.resource_id); // afterAll revoke
    expect(result.resource_id).toBeTruthy();
  });

  test('connector accepts mint-time session_duration=1s after a no-TTL upload', async () => {
    const upload = await qurl.uploadFile(env.UPLOAD_API_URL, freshFixture('mint-ttl1'), env.QURL_API_KEY);
    tracked.track(upload.resource_id);
    const child = await qurl.mintConnectorView(env.UPLOAD_API_URL, upload.resource_id, env.QURL_API_KEY, {
      expiresAt: new Date(Date.now() + 10 * 60_000).toISOString(),
      sessionDurationSeconds: 1,
    });
    tracked.trackChild(child.qurl_id);
    expect(child.qurl_link).toBeTruthy();
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
    tracked.track(result.resource_id); // afterAll revoke
    expect(result.resource_id).toBeTruthy();
  });
});
