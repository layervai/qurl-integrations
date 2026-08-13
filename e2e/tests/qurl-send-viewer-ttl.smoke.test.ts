/**
 * Smoke for the auto-destruct UPLOAD path (qurl-integrations#283).
 *
 * Pins the upload-time `session_duration` contract the 2026-05-13 incident
 * broke: the connector forwards an upload's `viewer_ttl_seconds` to
 * qurl-service as `session_duration` (connector.js `appendViewerTtl`), and a
 * sub-floor value was rejected for hours, un-caught, because the only smoke
 * took the no-TTL "use server default" branch. This sends a TTL'd upload
 * (must be accepted) plus a no-TTL control. Full incident write-up + design
 * rationale are in the PR description and #283.
 *
 * Honest boundary: catches a floor regression only IF qurl-service enforces
 * the floor at upload (CreateQURL). The bot also forwards session_duration
 * at MINT; pinning that needs a connector-mint helper + live run — #631.
 * `30` is a future-regression guard kept clear of today's 1s
 * `MinSessionDuration` floor (tighten to 1 once #631's live run confirms 1 is
 * accepted). Does NOT catch: mint-time floor (#631), per-recipient
 * view-window (Traefik), or the UI countdown.
 */

import * as dotenv from 'dotenv';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { trackedQurlResources } from '../helpers/cleanup';
import { loadEnv } from '../helpers/env';
import * as qurl from '../helpers/qurl-api';

const env = loadEnv();
const tracked = trackedQurlResources(env);

describe('Smoke: /qurl send with viewer_ttl_seconds (Snapchat path)', () => {
  // Per-test unique fixture content to avoid the connector's md5
  // content-addressed dedup shortcut. If both tests uploaded the
  // same bytes, the second call would short-circuit to the cached
  // resource from the first call (uploaded WITH viewer_ttl_seconds=30)
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

  // Best-effort cleanup: revoke the uploaded resources so each run doesn't
  // leak two S3 resources (revokeAll warns-but-never-throws — see
  // helpers/cleanup.ts), then remove the local fixtures (swallowed; a
  // stale temp file is not a leaked live resource).
  afterAll(async () => {
    await tracked.revokeAll();
    for (const f of tempFiles) {
      try { fs.unlinkSync(f); } catch { /* best-effort */ }
    }
  });

  // Name says "accepts", not "forwards": the assertion only observes a 2xx
  // upload (resource_id). A connector that silently DROPPED viewer_ttl_seconds
  // would still pass — forwarding is verified only transitively, when
  // qurl-service rejects a sub-floor session_duration (which today's 1s floor
  // doesn't, for 30).
  //
  // Contract verified against the connector source (qurl-s3-connector
  // internal/handler/handler.go): /api/upload parses viewer_ttl_seconds →
  // session_duration → CreateQURL, and a CreateQURL rejection surfaces as
  // HTTP 200 + success:true + an `error` string + NO resource_id (the file
  // uploaded; the mint failed). So the regression is caught by uploadFile's
  // throw-on-missing-resource_id (not a 4xx) — exactly the 2026-05-13 shape.
  test('connector accepts upload with viewer_ttl_seconds=30', async () => {
    const tempFile = freshFixture('ttl30');
    const result = await qurl.uploadFile(
      env.UPLOAD_API_URL,
      tempFile,
      env.QURL_API_KEY,
      { viewerTtlSeconds: 30 },
    );
    tracked.track(result.resource_id); // afterAll revoke
    expect(result.resource_id).toBeTruthy();
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
