/**
 * Google Maps location sharing E2E tests.
 *
 * Tests the full flow: upload google-map JSON to connector → mint link →
 * access fileviewer → verify Maps iframe embed renders correctly.
 *
 * Also tests edge cases: short URL resolution, coordinates, plain text queries,
 * and non-Maps JSON that should NOT trigger the Maps path.
 */

// TODO: Add afterAll cleanup to revoke/delete test resources

import * as dotenv from 'dotenv';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { loadEnv } from '../helpers/env';
import * as qurl from '../helpers/qurl-api';

const env = loadEnv();

/** Connector /upload response shape. Matches the interface in
 * file-revoke.test.ts; in the rate-limited case the fields are
 * present-but-missing (hence `Partial<>`) with the reason in `error`.
 * Claude review on the branch-update commit 57141aa3 flagged that the
 * prior `as any` cast here was asymmetric with the sibling test file
 * — a field rename upstream would surface cleanly on one path and
 * silently on the other. Shared shape makes both paths fail-same.
 */
interface UploadResponse {
  resource_url: string;
  resource_id: string;
  qurl_link: string;
}

/** Upload a google-map JSON payload to the connector and get back the resource URL.
 *
 * Retries transient 429 rate-limits from the upstream QURL API — the connector's
 * upload returns `{success:true, resource_url:..., error:"QURL creation failed:
 * ... 429 ..."}` without a `resource_id` when the internal mint is throttled. A
 * silent undefined resource_id breaks downstream revoke tests; surface it loudly
 * and retry with backoff instead.
 */
async function uploadMapLocation(
  payload: Record<string, unknown>,
  filename = 'location.json',
): Promise<{ viewerUrl: string; qurlLink: string; resourceId: string }> {
  const jsonStr = JSON.stringify(payload);

  const attempt = async (): Promise<{ viewerUrl: string; qurlLink: string; resourceId: string } | { retry: true }> => {
    const formData = new FormData();
    formData.append('file', new Blob([jsonStr], { type: 'application/json' }), filename);

    const res = await fetch(`${env.UPLOAD_API_URL}/upload`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${env.QURL_API_KEY}` },
      body: formData,
    });

    if (!res.ok) throw new Error(`Upload failed: ${res.status} ${await res.text()}`);
    const data = (await res.json()) as Partial<UploadResponse> & { error?: string };
    // Connector returns {success:true, error:"...QURL API returned status 429..."}
    // on a rate-limited internal mint — resource_id will be missing in that case.
    if (!data.resource_id || !data.resource_url || !data.qurl_link) {
      const errStr = typeof data.error === 'string' ? data.error : '';
      if (/429|rate.limit|too many requests/i.test(errStr)) {
        return { retry: true };
      }
      throw new Error(`Upload response missing required fields: ${JSON.stringify(data)}`);
    }
    return {
      viewerUrl: data.resource_url,
      qurlLink: data.qurl_link,
      resourceId: data.resource_id,
    };
  };

  const maxAttempts = 4;
  const baseDelayMs = 1500;
  for (let i = 0; i < maxAttempts; i++) {
    const out = await attempt();
    if ('retry' in out) {
      // Linear 1.5s / 3s / 6s backoff — QURL API's rate window is short enough
      // that exponential doesn't buy much on test bursts.
      await new Promise((r) => setTimeout(r, baseDelayMs * (i + 1)));
      continue;
    }
    return out;
  }
  throw new Error(`uploadMapLocation: still rate-limited after ${maxAttempts} attempts`);
}

/** Fetch the fileviewer page directly (bypass NHP) and return HTML */
async function fetchViewerPage(viewerUrl: string): Promise<string> {
  const res = await fetch(viewerUrl, { redirect: 'follow' });
  if (!res.ok) throw new Error(`Viewer returned ${res.status}`);
  return res.text();
}

describe('Google Maps: Iframe Embed', () => {
  test('google-map with short URL renders Maps iframe', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      url: 'https://maps.app.goo.gl/DvXv2GW9xc5ZGq3r8',
    });
    expect(upload.viewerUrl).toContain('/view/');

    const html = await fetchViewerPage(upload.viewerUrl);
    expect(html).toContain('iframe');
    expect(html).toContain('google.com/maps/embed');
    expect(html).toContain('Open in Google Maps');
    // Should NOT contain the canvas/watermark template
    expect(html).not.toContain('drawWatermark');
  });

  test('google-map with query renders Maps iframe with query', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      query: 'Eiffel Tower, Paris',
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    expect(html).toContain('iframe');
    expect(html).toContain('google.com/maps/embed');
    expect(html).toContain('Eiffel');
  });

  test('google-map with coordinates renders Maps iframe', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      lat: 48.8584,
      lng: 2.2945,
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    expect(html).toContain('iframe');
    expect(html).toContain('google.com/maps/embed');
    expect(html).toContain('48.858');
  });

  test('google-map page has "Open in Google Maps" link', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      query: 'Times Square, NYC',
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    expect(html).toContain('Open in Google Maps');
    expect(html).toContain('target="_blank"');
    expect(html).toContain('https://www.google.com/maps/search/');
  });

  test('google-map page has correct styling (map-container, bar)', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      query: 'Central Park',
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    expect(html).toContain('map-container');
    // Footer text matches the template in qurl-s3-connector handler.go's
    // mapEmbedTmpl: `📍 <span>Shared securely by <a href="https://layerv.ai/qurl" ...>QURL</a></span>`.
    expect(html).toContain('Shared securely by');
    expect(html).toContain('https://layerv.ai/qurl');
    expect(html).toContain('100vh');
  });
});

describe('Google Maps: Non-Maps JSON', () => {
  test('regular JSON does NOT render Maps iframe', async () => {
    const upload = await uploadMapLocation(
      { name: 'config.json', version: 1 },
      'config.json',
    );

    const html = await fetchViewerPage(upload.viewerUrl);
    // Should use the canvas/template path, not the Maps iframe
    expect(html).not.toContain('google.com/maps/embed');
    expect(html).toContain('canvas');
  });

  test('google-map with no query/coords/url falls through to template', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      // No url, query, lat, or lng
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    // No valid location data → falls through to template renderer
    expect(html).not.toContain('google.com/maps/embed');
  });
});

describe('Google Maps: Edge Cases', () => {
  test('google-map with very long query still works', async () => {
    const longQuery = 'A'.repeat(900); // under 1000 char limit
    const upload = await uploadMapLocation({
      type: 'google-map',
      query: longQuery,
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    expect(html).toContain('iframe');
    expect(html).toContain('google.com/maps/embed');
  });

  test('google-map with query over 1000 chars falls through', async () => {
    const tooLong = 'B'.repeat(1001);
    const upload = await uploadMapLocation({
      type: 'google-map',
      query: tooLong,
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    expect(html).not.toContain('google.com/maps/embed');
  });

  test('google-map with null island (0,0) renders correctly', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      lat: 0,
      lng: 0,
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    expect(html).toContain('iframe');
    expect(html).toContain('google.com/maps/embed');
  });

  test('google-map with negative coordinates (Sydney) works', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      lat: -33.8688,
      lng: 151.2093,
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    expect(html).toContain('iframe');
    expect(html).toContain('-33.868');
  });

  test('google-map with invalid coordinates falls through', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      lat: 999,
      lng: 999,
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    expect(html).not.toContain('google.com/maps/embed');
  });

  test('google-map with URL over 2000 chars falls through', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      url: 'https://maps.app.goo.gl/' + 'x'.repeat(2000),
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    expect(html).not.toContain('google.com/maps/embed');
  });
});

describe('Google Maps: Revoke', () => {
  // NOTE on revoke semantics (today vs. after infra#93/#139 ship):
  //
  // Today: `DELETE /v1/resources/{id}` kills the QURL-layer token chain
  // (qurl.link / qurl.site / resource status endpoint) but the
  // md5-addressed fileviewer URL still serves the content.
  //
  // Product decision (infra#139, accepted): revoke SHOULD also hard-kill
  // the fileviewer URL. Implementation is folded into infra#93 — on revoke,
  // the connector's new `DELETE /api/resources/:md5` fires synchronously
  // alongside the QURL-layer delete, removing the S3 object, which makes
  // subsequent fileviewer /view/:md5 requests 404 naturally.
  //
  // These tests verify today's revoke contract via `getLinkStatus()` → 404
  // (the canonical post-revoke signal; matches smoke.test.ts:103). Once
  // infra#93 ships, add `(await fetch(upload.viewerUrl)).status === 404`
  // alongside — marked with a TODO below. The iframe-regression assertion
  // is preserved in the pre-revoke read so a no-API-key deploy still fails
  // these tests loudly regardless.

  test('revoke location qurl → getLinkStatus returns 404', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      query: 'Revoke Test, Boston',
    });

    // Status-only check on the viewer would also pass on the no-API-key
    // fallback page. Assert both status-code AND iframe content so this
    // test catches the same class of regression as the test below.
    const before = await fetchViewerPage(upload.viewerUrl);
    expect(before).toContain('iframe');
    expect(before).toContain('google.com/maps/embed');

    const revoked = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resourceId);
    expect(revoked).toBe(true);

    // Matches smoke.test.ts:103 — the one canonical revoke-effect assertion.
    // TODO(infra#93/#139): once revoke-triggered S3 delete ships, also
    // `expect((await fetch(upload.viewerUrl)).status).toBe(404)` here.
    await expect(
      qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, upload.resourceId),
    ).rejects.toThrow(/404/);
  });

  test('revoke iframe-rendering location also kills the resource', async () => {
    // Explicitly targets the mapEmbedTmpl path (coordinates → iframe),
    // which is the UX regression that motivated PR #86 + today's key wiring.
    const upload = await uploadMapLocation({
      type: 'google-map',
      lat: 40.7128,
      lng: -74.0060,
    });

    const beforeHtml = await fetchViewerPage(upload.viewerUrl);
    // Guard: if `GOOGLE_MAPS_API_KEY` is missing in the deploy, this fails
    // and flags the regression — which is the whole point of this test.
    expect(beforeHtml).toContain('iframe');
    expect(beforeHtml).toContain('google.com/maps/embed');

    const revoked = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resourceId);
    expect(revoked).toBe(true);

    await expect(
      qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, upload.resourceId),
    ).rejects.toThrow(/404/);
  });
});
