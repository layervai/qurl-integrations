#!/usr/bin/env node
/**
 * Standalone load test script — runs outside Discord, hits APIs directly.
 * Usage: node scripts/loadtest-standalone.js --count 200 --duration 7200 --interval 30
 *
 * Options:
 *   --count N      Recipients per round (default: 100)
 *   --duration S   Total duration in seconds (default: 7200 = 2 hours)
 *   --interval S   Seconds between rounds (default: 60)
 *   --file PATH    Local file to upload (default: generates a 1MB test file)
 *   --location     Include a location link in each round
 */

const fs = require('fs');
const path = require('path');

// Load env from .env.loadtest (so user doesn't need to pass env vars on CLI)
const envFile = path.join(__dirname, '..', '.env.loadtest');
if (fs.existsSync(envFile)) {
  for (const line of fs.readFileSync(envFile, 'utf8').split('\n')) {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith('#')) continue;
    const [key, ...rest] = trimmed.split('=');
    if (key && rest.length) process.env[key.trim()] = rest.join('=').trim();
  }
}

const config = require('../src/config');
const { mintLinks } = require('../src/connector');
const { createOneTimeLink } = require('../src/qurl');

const args = process.argv.slice(2);
function getArg(name, defaultVal) {
  const idx = args.indexOf(`--${name}`);
  if (idx === -1) return defaultVal;
  return args[idx + 1] || defaultVal;
}
const hasFlag = (name) => args.includes(`--${name}`);

const COUNT = parseInt(getArg('count', '100'));
const DURATION_S = parseInt(getArg('duration', '7200'));
const INTERVAL_S = parseInt(getArg('interval', '60'));
const FILE_PATH = getArg('file', null);
const INCLUDE_LOCATION = hasFlag('location');
const TEST_LOCATION_URL = 'https://www.google.com/maps/place/?q=place_id:ChIJLU7jZClu5kcRbUm7GCkGkNQ'; // Eiffel Tower

async function generateTestFile() {
  const tmpPath = path.join('/tmp', `loadtest-${Date.now()}.bin`);
  const buf = Buffer.alloc(1024 * 1024, 'A'); // 1MB
  fs.writeFileSync(tmpPath, buf);
  return tmpPath;
}

// Reuse the shared parser — it has the overflow protection that this
// ad-hoc copy used to lack.
const { expiryToISO } = require('../src/utils/time');

async function runRound(roundNum) {
  const roundStart = performance.now();
  const results = { fileLinks: 0, fileFail: 0, locLinks: 0, locFail: 0, uploadMs: 0, mintMs: 0, locMs: 0 };

  // File pipeline
  if (FILE_PATH || !INCLUDE_LOCATION) {
    const filePath = FILE_PATH || await generateTestFile();
    const fileBuffer = fs.readFileSync(filePath);
    const blob = new Blob([fileBuffer], { type: 'application/octet-stream' });

    // Upload via fetch to connector (simulating what the bot does)
    const uploadStart = performance.now();
    const form = new FormData();
    form.append('file', blob, `loadtest-round${roundNum}.bin`);

    const headers = {};
    if (config.QURL_API_KEY) headers['Authorization'] = `Bearer ${config.QURL_API_KEY}`;

    const uploadResp = await fetch(`${config.CONNECTOR_URL}/api/upload`, {
      method: 'POST', body: form, headers,
    });
    if (!uploadResp.ok) throw new Error(`Upload failed: ${uploadResp.status}`);
    const uploadResult = await uploadResp.json();
    results.uploadMs = performance.now() - uploadStart;

    // Mint links in batches of 10
    const mintStart = performance.now();
    const expiresAt = expiryToISO('24h');
    for (let i = 0; i < COUNT; i += 10) {
      const batchSize = Math.min(10, COUNT - i);
      try {
        await mintLinks(uploadResult.resource_id, expiresAt, batchSize);
        results.fileLinks += batchSize;
      } catch (e) {
        if (results.fileFail === 0) console.error(`  File mint error: ${e.message}`);
        results.fileFail += batchSize;
      }
    }
    results.mintMs = performance.now() - mintStart;
  }

  // Location pipeline
  if (INCLUDE_LOCATION) {
    const locStart = performance.now();
    for (let i = 0; i < COUNT; i++) {
      try {
        await createOneTimeLink(TEST_LOCATION_URL, '24h', 'Load test location');
        results.locLinks++;
      } catch (e) {
        if (results.locFail === 0) console.error(`  Location mint error: ${e.message}`);
        results.locFail++;
      }
    }
    results.locMs = performance.now() - locStart;
  }

  const totalMs = performance.now() - roundStart;
  return { ...results, totalMs };
}

async function main() {
  // Preflight checks
  if (!config.QURL_API_KEY) { console.error('FATAL: QURL_API_KEY not set'); process.exit(1); }
  // Hard-block loadtest runs against production URLs unless the caller
  // explicitly opts in. Accidentally firing 12,000 mint operations at prod
  // from a dev laptop is not a great outcome.
  const allowProd = process.argv.includes('--allow-production') || process.env.LOADTEST_ALLOW_PRODUCTION === '1';
  const hittingProdQurl = config.QURL_ENDPOINT === 'https://api.layerv.ai';
  const hittingProdConnector = config.CONNECTOR_URL === 'https://get.qurl.link:9808';
  if ((hittingProdQurl || hittingProdConnector) && !allowProd) {
    console.error('FATAL: loadtest is pointed at production endpoints.');
    console.error('  QURL_ENDPOINT  =', config.QURL_ENDPOINT);
    console.error('  CONNECTOR_URL  =', config.CONNECTOR_URL);
    console.error('Set QURL_ENDPOINT/CONNECTOR_URL to a sandbox, or pass --allow-production.');
    process.exit(1);
  }

  // Quick smoke test
  console.log('Running smoke test...');
  try {
    const r = await createOneTimeLink('https://example.com', '24h', 'smoke test');
    console.log(`Smoke test OK: ${r.resource_id}`);
  } catch (e) {
    console.error(`FATAL: Smoke test failed — ${e.message}`);
    process.exit(1);
  }

  console.log(`Load test: ${COUNT} recipients/round, ${DURATION_S}s duration, ${INTERVAL_S}s interval`);
  console.log(`File: ${FILE_PATH || 'auto-generated 1MB'}, Location: ${INCLUDE_LOCATION}`);
  console.log('---');

  const startTime = Date.now();
  const endTime = startTime + DURATION_S * 1000;
  let round = 0;
  const allResults = [];

  while (Date.now() < endTime) {
    round++;
    const elapsed = ((Date.now() - startTime) / 1000).toFixed(0);
    console.log(`[${elapsed}s] Round ${round} starting...`);

    try {
      const results = await runRound(round);
      allResults.push(results);

      let line = `[${elapsed}s] Round ${round}: `;
      if (results.fileLinks > 0) line += `file(upload=${results.uploadMs.toFixed(0)}ms mint=${results.mintMs.toFixed(0)}ms ok=${results.fileLinks} fail=${results.fileFail}) `;
      if (results.locLinks > 0) line += `location(${results.locMs.toFixed(0)}ms ok=${results.locLinks} fail=${results.locFail}) `;
      line += `total=${(results.totalMs / 1000).toFixed(1)}s`;
      console.log(line);
    } catch (error) {
      console.error(`[${elapsed}s] Round ${round} FAILED: ${error.message}`);
    }

    // Wait for next round
    const remaining = endTime - Date.now();
    if (remaining > INTERVAL_S * 1000) {
      await new Promise(r => setTimeout(r, INTERVAL_S * 1000));
    } else {
      break;
    }
  }

  // Summary
  console.log('\n=== SUMMARY ===');
  console.log(`Rounds: ${allResults.length}`);
  console.log(`Total links minted: ${allResults.reduce((s, r) => s + r.fileLinks + r.locLinks, 0)}`);
  console.log(`Total failures: ${allResults.reduce((s, r) => s + r.fileFail + r.locFail, 0)}`);
  if (allResults.length > 0) {
    const avgTotal = allResults.reduce((s, r) => s + r.totalMs, 0) / allResults.length;
    console.log(`Avg round time: ${(avgTotal / 1000).toFixed(1)}s`);
    if (allResults[0].uploadMs > 0) {
      const avgUpload = allResults.reduce((s, r) => s + r.uploadMs, 0) / allResults.length;
      const avgMint = allResults.reduce((s, r) => s + r.mintMs, 0) / allResults.length;
      console.log(`Avg upload: ${avgUpload.toFixed(0)}ms, avg mint: ${avgMint.toFixed(0)}ms`);
    }
  }
}

main().catch(e => { console.error('Fatal:', e); process.exit(1); });
