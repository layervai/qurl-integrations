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
 *   --allow-production
 *                  Run anyway when a target is refused by the guard below.
 *
 * Target safety:
 *   QURL_ENDPOINT and CONNECTOR_URL must BOTH resolve to a host this script
 *   positively recognizes as non-production — loopback, or a hostname named in
 *   the comma-separated LOADTEST_TARGET_OK env var. Every other host is
 *   refused. See the "Target safety guard" section below.
 */

const fs = require('fs');
const path = require('path');

// Load env from .env.loadtest (so user doesn't need to pass env vars on CLI).
//
// Gated on direct invocation so that requiring this module from a test doesn't
// splice a developer's local .env.loadtest into process.env. It has to stay
// ABOVE the `require('../src/config')` below — config.js reads process.env at
// ITS module load, so moving this into main() would leave config resolving the
// un-augmented environment.
const envFile = path.join(__dirname, '..', '.env.loadtest');
if (require.main === module && fs.existsSync(envFile)) {
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
// Loopback classification for the target guard below reuses the reviewed range
// primitives rather than re-deriving 127.0.0.0/8 and ::1 by hand.
const { parseIPv4Octets, ipv4LocalScope, ipv6LocalScope } = require('../src/utils/private-host');

// ---------------------------------------------------------------------------
// Target safety guard
//
// A normal run mints COUNT resources per round for the whole DURATION_S window
// — tens of thousands of them. The one thing this script must never do is land
// that volume on an environment nobody meant to load test, so target selection
// is an ALLOWLIST: a target is refused unless positively recognized as
// non-production.
//
// This replaced a two-entry denylist that compared the two production URLs by
// exact string. That failed OPEN — a regional host, a custom domain, a new API
// hostname, a trailing slash or a different port all missed the comparison and
// took the full load. Matching is on the parsed hostname now, so `/`- and
// port-suffixed spellings of a production host no longer slip past, and an
// unrecognized host fails CLOSED.

// TODO(upstream-contract): production hostnames, mirrored from src/config.js's
// NODE_ENV=production fallbacks for QURL_ENDPOINT / CONNECTOR_URL and from
// connector.js's DETECT_TUNNEL_PROD_HOST_SUFFIX. When qurl-service moves or
// adds a production hostname, this list moves with it.
//
// `api.layerv.ai` is pinned as an exact HOST and deliberately not as a parent
// domain: `api.staging.layerv.ai` is a legitimate non-production endpoint (see
// connector.js's DETECT_TUNNEL_NON_PROD_QURL_ENDPOINT_HOSTS), so a
// domain-wide rule would misfile staging as production.
const PROD_HOSTS = new Set(['api.layerv.ai', 'get.qurl.link']);
// Matched as apex-or-subdomain: every resolved qURL site is a `*.qurl.site`
// host. The non-prod tunnel suffixes `.qurl.site.layerv.xyz` /
// `.qurl.site.layerv.ai` end in `.layerv.xyz` / `.layerv.ai` and so do NOT
// match this rule.
const PROD_DOMAINS = ['qurl.site'];

// Verdicts that may proceed without --allow-production.
const SAFE_VERDICTS = new Set(['loopback', 'allowlisted']);
const VERDICT_LABEL = {
  loopback: 'ok — loopback',
  allowlisted: 'ok — named in LOADTEST_TARGET_OK',
  production: 'REFUSED — production',
  unrecognized: 'REFUSED — not a recognized non-production host',
  unparseable: 'REFUSED — not a parseable URL',
};

/**
 * Lowercase, and drop one trailing root dot. `api.layerv.ai.` is the same host
 * as `api.layerv.ai` to DNS but a different string, so without this the
 * fully-qualified spelling misses the production check on both sides of the
 * guard — and on the allowlist side that is a fail-OPEN, since the entry would
 * be stored as an ordinary grant instead of being rejected as production.
 */
function normalizeHost(host) {
  const lowered = String(host).toLowerCase();
  return lowered.endsWith('.') ? lowered.slice(0, -1) : lowered;
}

/** Normalized hostname of a target URL, or null when it won't parse. */
function targetHost(rawUrl) {
  try {
    return normalizeHost(new URL(rawUrl).hostname);
  } catch {
    // Unparseable is not "safe" — the caller files null as its own refused
    // verdict, so a malformed target fails closed like an unknown host does.
    return null;
  }
}

/**
 * Loopback only — deliberately NOT `isPrivateHost` from
 * src/utils/private-host.js. A 10.x / 192.168.x address is "private" but can
 * perfectly well be a production VPC endpoint, so private-ness is no evidence
 * that a target is safe to load test. Loopback is: it cannot leave the host.
 *
 * What matters here is soundness, not completeness — this must never call a
 * non-loopback host loopback, but an exotic spelling of loopback going
 * unrecognized is fine, because unrecognized means "refuse". That is the
 * inverse of private-host.js's SSRF screen, where a missed spelling lets an
 * SSRF through and which therefore has to decode every literal form. Hosts
 * arrive here canonicalized by `new URL()` in any case, so decimal/hex/octal
 * IPv4 literals have already been folded to dotted quads.
 */
function isLoopbackHost(host) {
  if (host === 'localhost') return true;
  if (host.startsWith('[') && host.endsWith(']')) {
    const inner = host.slice(1, -1);
    // ipv6LocalScope's documented contract: the caller gates on the ':'. Its
    // leading parseInt is a lenient prefix parse, so an un-gated DNS name
    // could read as a hextet.
    return inner.includes(':') && ipv6LocalScope(inner) === 'loopback';
  }
  return ipv4LocalScope(parseIPv4Octets(host)) === 'loopback';
}

function isProductionHost(host) {
  if (PROD_HOSTS.has(host)) return true;
  return PROD_DOMAINS.some((domain) => host === domain || host.endsWith(`.${domain}`));
}

/**
 * Parse LOADTEST_TARGET_OK — comma-separated hostnames the operator is
 * explicitly naming as the intended targets. Real sandbox/staging hostnames
 * are infra-owned and must not be committed to this public repo (the same
 * reason config.js takes DETECT_EXTRA_NON_PROD_QURL_ENDPOINT_HOSTS from the
 * environment), so naming them per run is the only shape an allowlist can
 * take here.
 *
 * Same parsing idiom as config.js — split on comma, trim, lowercase, drop
 * empties — and the same fail-fast posture: an entry that could never match a
 * hostname is reported instead of shipping a quietly-inert grant.
 */
function parseTargetAllowlist(raw) {
  const hosts = new Set();
  const errors = [];
  const entries = String(raw || '').split(',').map((s) => normalizeHost(s.trim())).filter(Boolean);
  for (const entry of entries) {
    if (entry.includes('/') || entry.includes(':')) {
      // Covers `https://host`, `host/path` and `host:9808` alike. Bracketed
      // IPv6 is rejected by the same rule and needs no entry — loopback is
      // recognized without one.
      errors.push(`LOADTEST_TARGET_OK entry '${entry}' must be a bare hostname — no scheme, path, or port.`);
    } else if (entry.includes('*')) {
      errors.push(`LOADTEST_TARGET_OK entry '${entry}' must be a bare hostname — wildcards are not matched.`);
    } else if (isProductionHost(entry)) {
      // Production is not grantable through the environment: env vars get
      // copied between environments and pasted into .env.loadtest files, a
      // typed command-line flag does not.
      errors.push(`LOADTEST_TARGET_OK entry '${entry}' is a production host — pass --allow-production on the command line if that is really the intent.`);
    } else {
      hosts.add(entry);
    }
  }
  return { hosts, errors };
}

function classifyTarget(name, rawUrl, allowedHosts) {
  const host = targetHost(rawUrl);
  if (host === null) return { name, rawUrl, host, verdict: 'unparseable' };
  // Production is tested FIRST so an allowlist entry can never shadow it.
  if (isProductionHost(host)) return { name, rawUrl, host, verdict: 'production' };
  if (isLoopbackHost(host)) return { name, rawUrl, host, verdict: 'loopback' };
  if (allowedHosts.has(host)) return { name, rawUrl, host, verdict: 'allowlisted' };
  return { name, rawUrl, host, verdict: 'unrecognized' };
}

/** Classify both targets. The connector is judged independently of the API. */
function classifyTargets({ qurlEndpoint, connectorUrl, allowedHosts }) {
  return [
    classifyTarget('QURL_ENDPOINT', qurlEndpoint, allowedHosts),
    classifyTarget('CONNECTOR_URL', connectorUrl, allowedHosts),
  ];
}

const isRefusedTarget = (target) => !SAFE_VERDICTS.has(target.verdict);

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
  // Refuse any target not positively recognized as non-production. See the
  // "Target safety guard" section above for why this is an allowlist.
  const allowProd = process.argv.includes('--allow-production') || process.env.LOADTEST_ALLOW_PRODUCTION === '1';
  const { hosts: allowedHosts, errors: allowlistErrors } = parseTargetAllowlist(process.env.LOADTEST_TARGET_OK);
  // A malformed entry is reported even under --allow-production: it means the
  // operator believes they granted a host they did not, which is worth knowing
  // before the next run relies on it.
  if (allowlistErrors.length > 0) {
    for (const err of allowlistErrors) console.error(`FATAL: ${err}`);
    process.exit(1);
  }
  const targets = classifyTargets({
    qurlEndpoint: config.QURL_ENDPOINT,
    connectorUrl: config.CONNECTOR_URL,
    allowedHosts,
  });
  const refused = targets.filter(isRefusedTarget);
  if (refused.length > 0 && !allowProd) {
    console.error('FATAL: refusing to load test a target that is not a recognized sandbox.');
    for (const t of targets) {
      console.error(`  ${t.name.padEnd(14)} = ${t.rawUrl}  [${VERDICT_LABEL[t.verdict]}]`);
    }
    console.error('This run mints thousands of resources; a target has to be recognized as');
    console.error('non-production before it gets that volume. Point QURL_ENDPOINT/CONNECTOR_URL');
    console.error('at a sandbox, name the intended host(s) in LOADTEST_TARGET_OK (comma-separated');
    console.error('hostnames), or pass --allow-production.');
    process.exit(1);
  }
  if (refused.length > 0) {
    // Worded for either override — the flag and LOADTEST_ALLOW_PRODUCTION=1
    // both land here, so naming only the flag would misreport the env path.
    const named = refused.map((t) => `${t.name}=${t.host || t.rawUrl}`).join(', ');
    console.warn(`WARNING: target guard overridden — running against ${named}`);
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

// Only run the CLI entry point when invoked directly. Imported-from-test loads
// only the exported helpers — see tests/loadtest-target-guard.test.js.
if (require.main === module) {
  main().catch(e => { console.error('Fatal:', e); process.exit(1); });
}

module.exports = {
  normalizeHost,
  targetHost,
  isLoopbackHost,
  isProductionHost,
  parseTargetAllowlist,
  classifyTarget,
  classifyTargets,
  isRefusedTarget,
};
