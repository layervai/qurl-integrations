/**
 * Google Maps location sharing E2E tests.
 *
 * Tests the full flow: upload google-map JSON to connector → mint link →
 * access fileviewer → verify Maps iframe embed renders correctly.
 *
 * Also tests edge cases: short URL resolution, coordinates, plain text queries,
 * and non-Maps JSON that should NOT trigger the Maps path.
 */

import * as dotenv from 'dotenv';
import * as path from 'path';
dotenv.config({ path: path.resolve(__dirname, '..', '.env') });

import { loadEnv } from '../helpers/env';
import * as qurl from '../helpers/qurl-api';

const env = loadEnv();

/** Upload a google-map JSON payload to the connector and get back the resource URL */
async function uploadMapLocation(
  payload: Record<string, unknown>,
  filename = 'location.json',
): Promise<{ viewerUrl: string; qurlLink: string; resourceId: string }> {
  const jsonStr = JSON.stringify(payload);
  const formData = new FormData();
  formData.append('file', new Blob([jsonStr], { type: 'application/json' }), filename);

  const res = await fetch(`${env.UPLOAD_API_URL}/upload`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${env.QURL_API_KEY}` },
    body: formData,
  });

  if (!res.ok) throw new Error(`Upload failed: ${res.status} ${await res.text()}`);
  const data = await res.json() as any;

  // The resource_url points to the fileviewer
  return {
    viewerUrl: data.resource_url,
    qurlLink: data.qurl_link,
    resourceId: data.resource_id,
  };
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
    expect(html).toContain('Shared securely via QURL');
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
