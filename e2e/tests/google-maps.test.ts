/**
 * Google Maps location sharing E2E tests.
 *
 * Tests the full flow: upload google-map JSON to connector → mint link →
 * access fileviewer → verify the fallback "Open in Google Maps" card
 * renders with the correctly-constructed mapsURL href.
 *
 * Prior shape (pre-qurl-integrations-infra#726): the fileviewer carried
 * a GOOGLE_MAPS_API_KEY in its .env and rendered an inline Maps Embed
 * API iframe. #726 removed the API key entirely — the iframe path is
 * dormant code and the fallback card is the steady-state render.
 * mapsURL construction is identical on both paths, so all URL-shape
 * regressions (e.g. the "Erbil restaurant" /maps/place vs /maps/search
 * bug fixed in infra#155) remain testable via the fallback link's href.
 *
 * Also tests edge cases: short URL resolution, coordinates, plain text
 * queries, and non-Maps JSON that should NOT trigger the Maps path.
 *
 * TODO(upstream-line-refs): the comments below cite line numbers in
 * `qurl-s3-connector/internal/handler/handler.go` (a different repo).
 * The line refs will rot as that file churns — when a test fails in
 * a way that no longer matches the cited line, search for the symbol
 * (`mapFallbackTmpl`, `mapEmbedTmpl`, `QueryEscape`, `isGoogleMapsURL`)
 * rather than trusting the line number. Same convention as the
 * `TODO(upstream-rebrand)` marker on the qURL error-string match.
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
 * Retries transient 429 rate-limits from the upstream qURL API — the connector's
 * upload returns `{success:true, resource_url:..., error:"QURL creation failed:
 * ... 429 ..."}` without a `resource_id` when the internal mint is throttled. A
 * silent undefined resource_id breaks downstream revoke tests; surface it loudly
 * and retry with backoff instead.
 *
 * TODO(upstream-rebrand): the literal "QURL creation failed" mirrors upstream's
 * current error text. Update this doc comment when upstream qurl-service
 * rebrands its error strings.
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
      // Linear 1.5s / 3s / 6s backoff — qURL API's rate window is short enough
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

describe('Google Maps: Fallback card render', () => {
  // GOOGLE_MAPS_API_KEY was removed from the fileviewer deploy in
  // qurl-integrations-infra#726 — Maps share renders go through
  // mapFallbackTmpl (handler.go:1513) which shows an "Open in
  // Google Maps" card with a hyperlink to the resolved Google
  // Maps URL. The previous "iframe embed" assertions were only
  // valid when an API key was wired; the URL-construction logic
  // (mapsURL in handler.go:2451-2509) still runs regardless of
  // the key, so the underlying URL shape continues to be testable.
  test('google-map with short URL renders fallback card with original URL', async () => {
    const originalUrl = 'https://maps.app.goo.gl/DvXv2GW9xc5ZGq3r8';
    const upload = await uploadMapLocation({
      type: 'google-map',
      url: originalUrl,
    });
    expect(upload.viewerUrl).toContain('/view/');

    const html = await fetchViewerPage(upload.viewerUrl);
    // Fallback card: no iframe, no embed; "Open in Google Maps"
    // link points at the original URL.
    expect(html).not.toContain('google.com/maps/embed');
    expect(html).not.toContain('<iframe');
    expect(html).toContain('Open in Google Maps');
    expect(html).toContain(`href="${originalUrl}"`);
    // Should NOT contain the canvas/watermark template (the page
    // is still routed as a Maps share, not as a generic file).
    expect(html).not.toContain('drawWatermark');
  });

  test('google-map with query renders fallback card with /maps/search URL', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      query: 'Eiffel Tower, Paris',
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    expect(html).not.toContain('google.com/maps/embed');
    expect(html).not.toContain('<iframe');
    expect(html).toContain('Open in Google Maps');
    // /maps/search/<query> with space as %20 (per handler.go's
    // QueryEscape→%20 normalization at line 2492).
    expect(html).toContain('https://www.google.com/maps/search/Eiffel%20Tower');
  });

  test('google-map with coordinates renders fallback card with /maps/@lat,lng URL', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      lat: 48.8584,
      lng: 2.2945,
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    expect(html).not.toContain('google.com/maps/embed');
    expect(html).not.toContain('<iframe');
    expect(html).toContain('Open in Google Maps');
    // /maps/@<lat>,<lng>,17z with %.7f formatting on coords (per
    // handler.go:2507).
    expect(html).toContain('https://www.google.com/maps/@48.8584000,2.2945000,17z');
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

  test('"Open in Google Maps" URL is anchored to coords when query + lat/lng both supplied', async () => {
    // Regression guard for the "Erbil restaurant" bug: when the bot
    // uploads a named place that also carries lat/lng from Places
    // autocomplete, the mapsURL (shown under "Open in Google Maps")
    // must be a /maps/place/<name>/@<lat>,<lng>,17z form — NOT
    // /maps/search/<name> — otherwise Google resolves the name on
    // the recipient's side and opens a namesake near THEIR
    // geolocation instead of the place the sender picked. Fixed in
    // qurl-integrations-infra#155.
    //
    // This regression class survives the iframe→fallback-card
    // switch: the mapsURL string is the SAME on both paths (only
    // the embed iframe goes away). So this test continues to
    // catch the original bug shape via the fallback link's href.
    const upload = await uploadMapLocation({
      type: 'google-map',
      query: 'Erbil restaurant',
      lat: 36.1911,
      lng: 44.0094,
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    // Anchored URL form: the NAME segment must pin the original query
    // ("Erbil restaurant", with space as %20) followed by the
    // @lat,lng,17z anchor. Pinning the name — not just `[^/]*` — is
    // what catches a regression that returns the right coords but the
    // wrong place identity. `%.7f` formatting on the server side
    // produces the trailing zeros.
    //
    // Strict %20 (not tolerant `(?:%20|\+)`): handler.go normalizes
    // url.QueryEscape's default '+' to '%20' via
    // `strings.ReplaceAll(url.QueryEscape(s), "+", "%20")` — so the
    // contract is firm and matches the strict-`%20` Eiffel + Revoke
    // assertions below. cr cycle 1 on infra#726 follow-up.
    expect(html).toMatch(
      /https:\/\/www\.google\.com\/maps\/place\/Erbil%20restaurant\/@36\.1911000,44\.0094000,17z/,
    );
    // Negative pin: /maps/search/Erbil must appear NOWHERE in the
    // rendered HTML. It's only emitted by the pre-fix fallback path,
    // so its presence is a direct regression signal. Looser than the
    // prior href-structure-matching regex (which could be defeated by
    // template reflows) but sharper at detecting the actual bug shape.
    expect(html).not.toMatch(/\/maps\/search\/Erbil/);
  });

  test('fallback card has expected layout markers', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      query: 'Central Park',
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    // mapFallbackTmpl markers:
    // - Centered card layout via `.card` class
    // - "Shared Location" heading
    // - "Open in Google Maps to view this location." body copy
    //
    // Deliberately NOT asserting CSS details (e.g. `100vh`) — viewport
    // unit refactors (e.g. `100vh` → `100dvh`) would fail the test for
    // no functional reason. The three markers above pin the template
    // firmly enough.
    expect(html).toContain('class="card"');
    expect(html).toContain('Shared Location');
    expect(html).toContain('Open in Google Maps to view this location.');
    // Negative: the iframe-embed template's `map-container` class
    // and `Shared securely by` footer were specific to mapEmbedTmpl
    // (which doesn't fire without an API key). Assert they're absent
    // so a future re-enable of the iframe path that forgets to
    // update this test fails loud.
    expect(html).not.toContain('map-container');
    expect(html).not.toContain('Shared securely by');
  });
});

describe('Google Maps: Non-Maps JSON', () => {
  test('regular JSON does NOT trigger Maps render path', async () => {
    const upload = await uploadMapLocation(
      { name: 'config.json', version: 1 },
      'config.json',
    );

    const html = await fetchViewerPage(upload.viewerUrl);
    // Should use the canvas/template path, not the Maps render (neither
    // iframe nor fallback card).
    expect(html).not.toContain('google.com/maps/embed');
    expect(html).not.toContain('Open in Google Maps to view this location.');
    expect(html).toContain('canvas');
  });

  test('google-map with no query/coords/url falls through to template', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      // No url, query, lat, or lng
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    // No valid location data → falls through to template renderer.
    // Neither Maps render path (iframe nor fallback card) fires.
    expect(html).not.toContain('google.com/maps/embed');
    expect(html).not.toContain('Open in Google Maps to view this location.');
  });
});

describe('Google Maps: Edge Cases', () => {
  // mapsURL construction happens regardless of API key (handler.go:2451-2509);
  // the API-key check at 2516 only gates whether the iframe or the
  // fallback card renders. So all edge-case assertions are on the
  // fallback card's "Open in Google Maps" href.
  test('google-map with very long query renders fallback card', async () => {
    const longQuery = 'A'.repeat(900); // under 1000 char limit
    const upload = await uploadMapLocation({
      type: 'google-map',
      query: longQuery,
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    expect(html).not.toContain('google.com/maps/embed');
    expect(html).toContain('Open in Google Maps');
    // The 900-char A string is QueryEscape-clean, so it appears
    // verbatim in /maps/search/.
    expect(html).toContain(`https://www.google.com/maps/search/${longQuery}`);
  });

  test('google-map with query over 1000 chars falls through (no Maps URL)', async () => {
    const tooLong = 'B'.repeat(1001);
    const upload = await uploadMapLocation({
      type: 'google-map',
      query: tooLong,
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    // Over the 1000-char cap, handler.go skips the maps render path
    // entirely — mapsURL stays empty, falls through to the generic
    // template renderer. Neither embed nor fallback fires.
    expect(html).not.toContain('google.com/maps/embed');
    expect(html).not.toContain('Open in Google Maps to view this location.');
  });

  test('google-map with null island (0,0) renders fallback card', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      lat: 0,
      lng: 0,
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    expect(html).not.toContain('google.com/maps/embed');
    expect(html).toContain('Open in Google Maps');
    expect(html).toContain('https://www.google.com/maps/@0.0000000,0.0000000,17z');
  });

  test('google-map with negative coordinates (Sydney) renders fallback card', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      lat: -33.8688,
      lng: 151.2093,
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    expect(html).not.toContain('google.com/maps/embed');
    expect(html).toContain('Open in Google Maps');
    expect(html).toContain('https://www.google.com/maps/@-33.8688000,151.2093000,17z');
  });

  test('google-map with invalid coordinates falls through (no Maps URL)', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      lat: 999,
      lng: 999,
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    // Coords outside [-90,90]/[-180,180] are rejected at handler.go:2504-2506,
    // mapsURL stays empty, falls through to the generic template.
    expect(html).not.toContain('google.com/maps/embed');
    expect(html).not.toContain('Open in Google Maps to view this location.');
  });

  test('google-map with URL over 2000 chars falls through (no Maps URL)', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      url: 'https://maps.app.goo.gl/' + 'x'.repeat(2000),
    });

    const html = await fetchViewerPage(upload.viewerUrl);
    expect(html).not.toContain('google.com/maps/embed');
    expect(html).not.toContain('Open in Google Maps to view this location.');
  });
});

describe('Google Maps: Revoke', () => {
  // NOTE on revoke semantics (today vs. after infra#93/#139 ship):
  //
  // Today: `DELETE /v1/resources/{id}` kills the qURL-layer token chain
  // (qurl.link / qurl.site / resource status endpoint) but the
  // md5-addressed fileviewer URL still serves the content.
  //
  // Product decision (infra#139, accepted): revoke SHOULD also hard-kill
  // the fileviewer URL. Implementation is folded into infra#93 — on revoke,
  // the connector's new `DELETE /api/resources/:md5` fires synchronously
  // alongside the qURL-layer delete, removing the S3 object, which makes
  // subsequent fileviewer /view/:md5 requests 404 naturally.
  //
  // These tests verify today's revoke contract via `getLinkStatus()` → 404
  // (the canonical post-revoke signal; matches smoke.test.ts:103). Once
  // infra#93 ships, add `(await fetch(upload.viewerUrl)).status === 404`
  // alongside — marked with a TODO below.
  //
  // Pre-revoke read also asserts the fallback-card render shape so a
  // future regression that breaks the map render path (e.g. mapsURL
  // construction broken) fails these tests loudly regardless of the
  // revoke pathway.

  test('revoke location qURL → getLinkStatus returns 404', async () => {
    const upload = await uploadMapLocation({
      type: 'google-map',
      query: 'Revoke Test, Boston',
    });

    // Pre-revoke read: fallback-card shape (no API key in deploy).
    // Status-only check on the viewer would also pass on a generic-
    // template fallthrough, so assert the fallback card's specific
    // markers — both that the maps-render path fired AND that the
    // resolved mapsURL is in the href.
    const before = await fetchViewerPage(upload.viewerUrl);
    expect(before).toContain('Open in Google Maps');
    expect(before).toContain('Open in Google Maps to view this location.');
    expect(before).toContain('https://www.google.com/maps/search/Revoke%20Test');

    const revoked = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resourceId);
    expect(revoked).toBe(true);

    // Matches smoke.test.ts:103 — the one canonical revoke-effect assertion.
    // TODO(infra#93/#139): once revoke-triggered S3 delete ships, also
    // `expect((await fetch(upload.viewerUrl)).status).toBe(404)` here.
    await expect(
      qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, upload.resourceId),
    ).rejects.toThrow(/404/);
  });

  test('revoke coordinate-based location also kills the resource', async () => {
    // Explicitly targets the lat/lng → /maps/@<lat>,<lng>,17z path.
    // Previously this asserted the iframe-embed shape (handler's
    // mapEmbedTmpl); after #726 stripped GOOGLE_MAPS_API_KEY the
    // assertion is the fallback card's href instead.
    const upload = await uploadMapLocation({
      type: 'google-map',
      lat: 40.7128,
      lng: -74.0060,
    });

    const beforeHtml = await fetchViewerPage(upload.viewerUrl);
    expect(beforeHtml).toContain('Open in Google Maps');
    expect(beforeHtml).toContain('https://www.google.com/maps/@40.7128000,-74.0060000,17z');

    const revoked = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, upload.resourceId);
    expect(revoked).toBe(true);

    await expect(
      qurl.getLinkStatus(env.MINT_API_URL, env.QURL_API_KEY, upload.resourceId),
    ).rejects.toThrow(/404/);
  });
});
