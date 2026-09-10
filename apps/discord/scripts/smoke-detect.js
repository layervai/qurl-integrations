// Live check: use the deployed bot's QURL_ENDPOINT, QURL_API_KEY,
// DETECT_TUNNEL_SLUG, QURL_DEPLOYMENT, and non-prod host overrides.
// An optional image path plus DETECT_SMOKE_QURL_ID checks known attribution.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { detectWatermark } = require('../src/connector');

async function main() {
  const bindingId = process.env.DETECT_SMOKE_BINDING_ID;
  assert.match(bindingId || '', /^eib_[A-Za-z0-9]{11}$/, 'Set DETECT_SMOKE_BINDING_ID to the test server binding');
  const imagePath = process.argv[2];
  const expectedId = process.env.DETECT_SMOKE_QURL_ID;
  assert.equal(Boolean(imagePath), Boolean(expectedId), 'Supply both an image path and DETECT_SMOKE_QURL_ID');
  const bytes = imagePath ? fs.readFileSync(imagePath) : Buffer.from(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=',
    'base64',
  );
  const result = await detectWatermark(bytes, { bindingId, contentType: 'image/png' });
  assert.equal(result.detected, Boolean(expectedId));
  assert.equal(result.qurl_id, expectedId || null);
  console.log('Discord detect live smoke passed');
}

main().catch(error => {
  // Error messages from dependencies can contain credentials; print only type/status.
  console.error('Discord detect live smoke failed', { name: error.name, status: error.status });
  process.exitCode = 1;
});
