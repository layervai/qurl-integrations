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
 *   --ledger PATH  Where to record created resources (default: <tmpdir>/loadtest-ledger-<ts>.jsonl)
 *   --reclaim PATH Revoke everything in a previous run's ledger, then exit
 *
 * Every resource this script creates is recorded to the ledger before it is
 * used, and revoked before the script exits — on the normal path, on a thrown
 * error, and on Ctrl-C, which stops the run rather than racing it. Minted
 * links are therefore dead once a run ends; don't expect to open one after
 * the summary prints.
 *
 * If the process is killed hard enough to skip all of that (SIGKILL, a dead
 * laptop), the ledger is the recovery path:
 *
 *   node scripts/loadtest-standalone.js --reclaim <tmpdir>/loadtest-ledger-<ts>.jsonl
 *
 * Reclaim resolves its target host from the ambient QURL_ENDPOINT, so each
 * ledger line records the endpoint it was written against and a sweep refuses
 * to run against a different one. Re-running --reclaim is safe: revoked ids
 * are pruned from the ledger and an already-gone resource counts as reclaimed.
 *
 * One window this cannot close: if a create succeeds server-side but the
 * response is lost, the resource exists and its id never reached the ledger.
 */

const fs = require('fs');
const os = require('os');
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
const { createOneTimeLink, deleteLink } = require('../src/qurl');

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

// Default under os.tmpdir() and created 0600, following the rationale
// gateway-resume-spike.js records in this same directory: a predictable path
// in a world-writable /tmp can be pre-created as a symlink, and appendFileSync
// follows symlinks. The contents are only resource ids, but the write
// primitive is real.
const LEDGER_PATH = getArg('ledger', path.join(os.tmpdir(), `loadtest-ledger-${Date.now()}.jsonl`));

// Prove the ledger is writable BEFORE anything creates a resource. Discovering
// an unwritable ledger after the first mint orphans that resource: its id was
// never recorded and, once the process stops, nothing knows it exists.
function preflightLedger() {
  try {
    fs.closeSync(fs.openSync(LEDGER_PATH, 'a', 0o600));
  } catch (e) {
    console.error(`FATAL: reclaim ledger ${LEDGER_PATH} is not writable — ${e.message}`);
    console.error('Refusing to create resources that could not be recorded. Pass --ledger PATH.');
    process.exit(1);
  }
}

// Set as soon as a sweep begins. The run loop and the per-recipient loops
// check it: a sweep snapshots the ledger and then yields at every await, so
// without this the loop keeps minting ids the snapshot can never see and the
// script exits having reclaimed only part of what it made.
let stopping = false;

// Creates that have been issued but not yet recorded. `stopping` ends the
// loops, but it cannot un-issue a request already awaiting a response: that
// response still resolves and still appends. The drain re-reads the ledger,
// which only catches an append that has already happened — so it also waits
// on this counter, or a create resolving just after the final pass would
// leave an id no pass ever saw.
let inFlightCreates = 0;
async function trackCreate(fn) {
  inFlightCreates++;
  try {
    return await fn();
  } finally {
    inFlightCreates--;
  }
}

// Every resource is appended to the ledger the moment it exists, before any
// further work. Holding the ids only in memory would not survive how these
// runs actually end — a two-hour soak gets Ctrl-C'd. Recovering an unrecorded
// resource means finding it by description or target through the listing's
// free-text search, which is imprecise and would collaterally revoke another
// run's resources; the ledger is exact.
//
// Recipient links from mintLinks are deliberately not recorded individually:
// deleting the parent file resource revokes every qURL minted against it
// (shared/client/client.go documents the cascade).
function recordResource(resourceId, kind) {
  if (!resourceId) {
    // Loud, not silent: a created-but-unrecorded resource makes the closing
    // "N revoked" a lie by omission, and this is the only place that can
    // notice. The upload path hand-rolls its fetch and does not validate the
    // response shape the way connector.js does.
    console.error(`WARNING: ${kind} response carried no resource_id — that resource cannot be reclaimed.`);
    return;
  }
  try {
    fs.appendFileSync(
      LEDGER_PATH,
      // The endpoint travels with the id: --reclaim resolves its target from
      // ambient config, so without provenance a ledger handed to an operator
      // in the wrong shell issues bulk deletes against the wrong tenancy.
      `${JSON.stringify({ resource_id: resourceId, kind, endpoint: config.QURL_ENDPOINT })}\n`,
    );
  } catch (e) {
    // A ledger we cannot write is a leak we cannot reclaim, so stop minting.
    // Deliberately NOT process.exit: everything recorded before this point is
    // still on disk and still reclaimable, and exiting here would skip the
    // sweep that reclaims it — giving up hardest exactly where giving up
    // leaks most.
    console.error(`FATAL: cannot write reclaim ledger ${LEDGER_PATH} — ${e.message}`);
    console.error('Stopping the run; reclaiming what was recorded before the failure.');
    stopping = true;
  }
}

// Returns an array of ids, or null when the ledger file does not exist.
// Null and [] mean opposite things in recovery mode — "your path is wrong,
// nothing was swept" versus "nothing is outstanding" — and collapsing them
// is how an operator walks away believing they are clean.
function readLedger(ledgerPath, quiet = false) {
  // statSync rather than existsSync: existsSync is true for a directory, and
  // readFileSync would then throw EISDIR out of the sweep.
  let stat;
  try {
    stat = fs.statSync(ledgerPath);
  } catch {
    return null;
  }
  if (!stat.isFile()) return null;
  const ids = [];
  fs.readFileSync(ledgerPath, 'utf8').split('\n').forEach((line, index) => {
    const trimmed = line.trim();
    if (!trimmed) return;
    try {
      const { resource_id: id } = JSON.parse(trimmed);
      if (id) ids.push(id);
    } catch {
      // A torn final line is what a hard kill mid-append looks like: skip it
      // and reclaim the rest rather than abandon the whole ledger. Report
      // position and size only, never content — --reclaim takes an arbitrary
      // operator-supplied path, and one aimed at .env.loadtest would echo the
      // API key into the scrollback.
      // Quiet on drain passes: the same torn line would otherwise be reported
      // once per pass and read as several torn lines.
      if (!quiet) console.error(`  Skipping unparseable ledger line ${index + 1} (${Buffer.byteLength(trimmed)} bytes)`);
    }
  });
  return ids;
}

// Stands in for an entry that records no endpoint, so such an entry reads as
// foreign to the tenancy guard rather than as an absence of evidence.
const UNRECORDED_ENDPOINT = '(no endpoint recorded)';

// Endpoints recorded in a ledger, so a sweep can refuse to delete against a
// tenancy other than the one the resources were created on.
function ledgerEndpoints(ledgerPath) {
  const endpoints = new Set();
  // Same stat guard as readLedger rather than existsSync: a directory path
  // would otherwise throw EISDIR here. Today reclaim returns before reaching
  // this, but that ordering should not be what keeps it safe.
  try {
    if (!fs.statSync(ledgerPath).isFile()) return endpoints;
  } catch {
    return endpoints;
  }
  for (const line of fs.readFileSync(ledgerPath, 'utf8').split('\n')) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    try {
      const { resource_id: id, endpoint } = JSON.parse(trimmed);
      if (!id) continue;
      // Fail closed. An entry with no recorded endpoint — an older or
      // hand-edited ledger, or a run with QURL_ENDPOINT unset — would
      // otherwise contribute nothing to the set and let the guard pass
      // trivially, which is the opposite of what a safety rail should do
      // when its input is missing.
      endpoints.add(endpoint || UNRECORDED_ENDPOINT);
    } catch { /* torn line — readLedger already reports it */ }
  }
  return endpoints;
}

// Rewrite the ledger with only what is still outstanding, so a re-run sweeps
// the remainder instead of re-revoking everything. Truncated rather than
// deleted on a clean sweep: an empty ledger reads as "nothing outstanding",
// while a missing one is indistinguishable from a mistyped path.
// Surviving entries are kept as their ORIGINAL lines rather than
// re-serialized, so nothing recorded is lost in a prune. Re-serializing had
// dropped `endpoint`, which silently disarmed the tenancy guard on exactly
// the recovery re-run it exists to protect; keeping the line verbatim makes
// that class of loss impossible rather than merely fixed once.
function pruneLedger(ledgerPath, remainingIds) {
  try {
    const kept = [];
    const seen = new Set();
    for (const line of fs.readFileSync(ledgerPath, 'utf8').split('\n')) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      try {
        const { resource_id: id } = JSON.parse(trimmed);
        if (id && remainingIds.has(id) && !seen.has(id)) {
          seen.add(id);
          kept.push(trimmed);
        }
      } catch { /* torn line: carries no id, so there is nothing to keep */ }
    }
    fs.writeFileSync(ledgerPath, kept.length ? `${kept.join('\n')}\n` : '');
  } catch (e) {
    console.error(`  Could not prune ledger ${ledgerPath} — ${e.message}`);
  }
}

// Revoke everything recorded in the ledger. Returns { missing, revoked, failed }.
// Safe to re-run: successfully revoked ids are pruned, and an already-gone
// resource counts as revoked rather than failed.
async function reclaim(ledgerPath) {
  stopping = true;

  const initial = readLedger(ledgerPath);
  if (initial === null) {
    console.error(`Reclaim: no ledger file at ${ledgerPath} — nothing was reclaimed.`);
    return { missing: true, revoked: 0, failed: 0 };
  }

  // A ledger with content but no readable ids is corruption, not cleanliness.
  // Recovery mode exists because something already went wrong, so this must
  // not read as an all-clear the way a genuinely empty ledger does.
  const populatedLines = fs.readFileSync(ledgerPath, 'utf8').split('\n').filter(l => l.trim()).length;
  if (initial.length === 0 && populatedLines > 0) {
    console.error(`Reclaim: ${ledgerPath} holds ${populatedLines} line(s) but no readable resource ids — treating as corrupt, not clean.`);
    return { missing: false, revoked: 0, failed: 0, unreadable: true };
  }

  // Always say which tenancy is about to be deleted from — this is a bulk
  // delete whose target list comes from a file and whose target host comes
  // from ambient config, and those two can disagree.
  console.log(`Reclaim: target endpoint ${config.QURL_ENDPOINT}`);
  // Every recorded endpoint must be the current one, not merely include it:
  // deletes are issued per id with no per-id host, so a ledger mixing two
  // tenancies would otherwise pass and then delete the other one's resources.
  // Compared with a trailing slash trimmed: QURL_ENDPOINT is not normalized
  // in config.js, so the same tenancy spelled with and without one would
  // otherwise refuse a legitimate recovery sweep mid-incident.
  const normalizeEndpoint = (value) => String(value).replace(/\/+$/, '');
  const current = normalizeEndpoint(config.QURL_ENDPOINT);
  const foreign = [...ledgerEndpoints(ledgerPath)].filter(e => normalizeEndpoint(e) !== current);
  if (foreign.length > 0) {
    console.error(`Reclaim: this ledger records resources on ${foreign.join(', ')}, not ${config.QURL_ENDPOINT}.`);
    console.error('Refusing to delete against a different tenancy. Set QURL_ENDPOINT to match and re-run.');
    return { missing: false, revoked: 0, failed: 0, refused: true };
  }

  const swept = new Set();
  const outstanding = new Set();
  const causes = new Map();
  let revoked = 0;

  // Drain rather than sweep once. An in-flight round can append after the
  // snapshot is taken, so keep passing until a pass finds nothing new.
  for (;;) {
    const pending = [...new Set(readLedger(ledgerPath, true) || [])].filter(id => !swept.has(id));
    if (pending.length === 0) {
      // Nothing new on disk, but a create may still be in flight and about to
      // append. Wait for it rather than finishing and exiting past it.
      if (inFlightCreates === 0) break;
      await new Promise(r => setTimeout(r, 100));
      continue;
    }
    // Floor, not an estimate: the 50ms pacing gap is the only part that is
    // known here, and per-request latency adds to it. Overstating the number
    // the heartbeat exists to contextualise would undercut it.
    const seconds = Math.max(1, Math.round(pending.length * 0.05));
    console.log(`Reclaim: revoking ${pending.length} resource(s) from ${ledgerPath} (>=${seconds}s)...`);
    let done = 0;
    // Serial with a short gap. The tenancy is shared and rate-limited per
    // account, and a burst of hundreds of deletes is what trips it.
    for (const id of pending) {
      swept.add(id);
      try {
        await deleteLink(id);
        revoked++;
        outstanding.delete(id);
      } catch (e) {
        // An already-gone resource is the successful end state for a reclaim.
        // callQurl collapses API errors to a status-only string, so matching
        // the status is all that is available.
        //
        // TODO(upstream-contract): 404 and 410 are the statuses qurl-service
        // uses for a resource that no longer exists. If it adopts another,
        // nothing here fails loudly — a re-run would simply sweep the same
        // ids forever, reporting them as failures.
        if (/\((404|410)\)/.test(e.message)) {
          revoked++;
          outstanding.delete(id);
        } else {
          outstanding.add(id);
          causes.set(e.message, (causes.get(e.message) || 0) + 1);
        }
      }
      done++;
      // Without a heartbeat a 12,000-id sweep looks wedged for ten minutes
      // after the summary has printed, and the operator reaches for kill -9 —
      // the exact hard kill this whole mechanism exists to survive.
      if (done % 250 === 0) console.log(`  ...${done}/${pending.length}`);
      await new Promise(r => setTimeout(r, 50));
    }
  }

  // Load-bearing and invisible: there is no await between the breaking
  // readLedger above and this write, so recordResource cannot interleave and
  // have its append truncated away. Inserting any await (including an
  // awaiting log) between them silently reintroduces the leak.
  pruneLedger(ledgerPath, outstanding);

  const failed = outstanding.size;
  if (swept.size === 0) {
    console.log(`Reclaim: nothing outstanding in ${ledgerPath}.`);
    return { missing: false, revoked: 0, failed: 0 };
  }
  console.log(`Reclaim: ${revoked} revoked, ${failed} failed.`);
  // A tally rather than one sampled message: 401 (key rotated mid-soak), 429
  // (rate limited) and the rest demand different responses, and one sample
  // only identifies the cause if the run failed uniformly.
  for (const [message, n] of [...causes.entries()].sort((a, b) => b[1] - a[1])) {
    console.error(`  ${n}x ${message}`);
  }
  if (failed > 0) {
    console.error(`Reclaim: ${failed} resource(s) still on the tenancy — re-run with --reclaim ${ledgerPath}`);
  }
  return { missing: false, revoked, failed };
}

// Every reclaim path goes through here: the normal end of a run, a thrown
// error, and both signals. The promise is memoized rather than guarded by a
// boolean so a Ctrl-C arriving *during* the end-of-run sweep waits for the
// sweep already in flight instead of either starting a competing one or
// exiting out from under it — both of which strand the resources the sweep
// had not reached yet.
let reclaimInFlight = null;
function reclaimOnce(ledgerPath) {
  if (!reclaimInFlight) reclaimInFlight = reclaim(ledgerPath);
  return reclaimInFlight;
}

// Which ledger a signal should sweep: this run's own, or the one named by
// --reclaim while that recovery mode is running.
let activeLedgerPath = LEDGER_PATH;

// Exit codes follow the convention gateway-resume-spike.js documents, so a
// wrapper can tell user-cancel from external termination: 130 for SIGINT,
// 143 for SIGTERM. An interrupted soak is never a success, even when its
// sweep succeeds.
const SIGNAL_EXIT_CODES = { SIGINT: 130, SIGTERM: 143 };

let signalled = false;
async function reclaimAndExit(signal) {
  if (signalled) {
    // The first signal starts a sweep that can run for minutes. Swallowing
    // every later one leaves no way out but kill -9 — the hard kill this
    // mechanism exists to survive — so the second one is an immediate abort.
    console.error(`\nSecond ${signal} — aborting without finishing the sweep.`);
    console.error(`Resources already created are still recorded; re-run with --reclaim ${activeLedgerPath}`);
    process.exit(SIGNAL_EXIT_CODES[signal] || 1);
  }
  signalled = true;
  console.log(`\nReceived ${signal} — stopping the run and reclaiming what it created...`);
  try {
    await reclaimOnce(activeLedgerPath);
  } catch (e) {
    console.error(`Reclaim failed: ${e.message}`);
  }
  process.exit(SIGNAL_EXIT_CODES[signal] || 1);
}

// Registered before anything can create a resource — including the preflight
// smoke link, which is minted well before the run loop starts.
function installSignalHandlers() {
  process.on('SIGINT', () => { reclaimAndExit('SIGINT'); });
  process.on('SIGTERM', () => { reclaimAndExit('SIGTERM'); });
}

// Extracted from main so the suite can cover it. This guard prevents the
// worst outcome in the file: `--reclaim` with no value would fall through to
// a FULL LOAD TEST, minting thousands of resources when the operator asked to
// delete some, and `--reclaim --ledger x` would take the next flag as the
// path, find no such file, and report a clean exit.
function parseReclaimArg(argv) {
  const index = argv.indexOf('--reclaim');
  if (index === -1) return { requested: false, path: null };
  const value = argv[index + 1];
  if (!value || value.startsWith('--')) return { requested: true, path: null };
  return { requested: true, path: value };
}

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
  if (!stopping && (FILE_PATH || !INCLUDE_LOCATION)) {
    const filePath = FILE_PATH || await generateTestFile();
    const fileBuffer = fs.readFileSync(filePath);
    const blob = new Blob([fileBuffer], { type: 'application/octet-stream' });

    // Upload via fetch to connector (simulating what the bot does)
    const uploadStart = performance.now();
    const form = new FormData();
    form.append('file', blob, `loadtest-round${roundNum}.bin`);

    const headers = {};
    if (config.QURL_API_KEY) headers['Authorization'] = `Bearer ${config.QURL_API_KEY}`;

    const uploadResult = await trackCreate(async () => {
      const uploadResp = await fetch(`${config.CONNECTOR_URL}/api/upload`, {
        method: 'POST', body: form, headers,
      });
      if (!uploadResp.ok) throw new Error(`Upload failed: ${uploadResp.status}`);
      const parsed = await uploadResp.json();
      // Recorded before anything is minted against it. Reclaiming this parent
      // is what reclaims the recipient links, so it has to be on disk first.
      recordResource(parsed.resource_id, 'upload');
      return parsed;
    });
    results.uploadMs = performance.now() - uploadStart;

    // Mint links in batches of 10
    const mintStart = performance.now();
    const expiresAt = expiryToISO('24h');
    for (let i = 0; i < COUNT && !stopping; i += 10) {
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
  if (!stopping && INCLUDE_LOCATION) {
    const locStart = performance.now();
    for (let i = 0; i < COUNT && !stopping; i++) {
      try {
        await trackCreate(async () => {
          const loc = await createOneTimeLink(TEST_LOCATION_URL, '24h', 'Load test location');
          recordResource(loc.resource_id, 'location');
        });
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

  installSignalHandlers();

  // Recovery mode for a previous run that was killed before it could reclaim.
  // Runs before the endpoint guard below: revoking resources that already
  // exist is always the safe direction, wherever they were created.
  const { requested, path: reclaimOnly } = parseReclaimArg(args);
  if (requested) {
    if (!reclaimOnly) {
      console.error('FATAL: --reclaim needs a ledger path, e.g. --reclaim /tmp/loadtest-ledger-<ts>.jsonl');
      process.exit(1);
    }
    activeLedgerPath = reclaimOnly;
    const {
      missing, failed, refused, unreadable,
    } = await reclaimOnce(reclaimOnly);
    // A missing ledger is a failure in recovery mode, not "nothing to do":
    // the operator is here because something already went wrong, and a
    // cheerful exit 0 on a mistyped path is how live resources get abandoned.
    process.exit(missing || refused || unreadable || failed > 0 ? 1 : 0);
  }

  preflightLedger();

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
    const r = await trackCreate(async () => {
      const link = await createOneTimeLink('https://example.com', '24h', 'smoke test');
      recordResource(link.resource_id, 'smoke');
      return link;
    });
    console.log(`Smoke test OK: ${r.resource_id}`);
  } catch (e) {
    console.error(`FATAL: Smoke test failed — ${e.message}`);
    process.exit(1);
  }

  console.log(`Load test: ${COUNT} recipients/round, ${DURATION_S}s duration, ${INTERVAL_S}s interval`);
  console.log(`File: ${FILE_PATH || 'auto-generated 1MB'}, Location: ${INCLUDE_LOCATION}`);
  console.log(`Ledger: ${LEDGER_PATH}`);
  console.log('---');

  const startTime = Date.now();
  const endTime = startTime + DURATION_S * 1000;
  let round = 0;
  const allResults = [];

  // `stopping` ends the loop as well as the clock: a sweep triggered by a
  // signal must not race rounds that keep appending ids behind it.
  while (Date.now() < endTime && !stopping) {
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
    if (remaining > INTERVAL_S * 1000 && !stopping) {
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

  // The minted counts above and the sweep's count below measure different
  // things — recipient links versus the parent resources that own them — so
  // say so, or the two numbers look like a leak of the difference.
  const parents = [...new Set(readLedger(LEDGER_PATH) || [])].length;
  const minted = allResults.reduce((s, r) => s + r.fileLinks + r.locLinks, 0);
  console.log(`Reclaimable parents: ${parents} (covering ${minted} minted qURLs)`);
  console.log(`Ledger: ${LEDGER_PATH}`);

  const { failed } = await reclaimOnce(LEDGER_PATH);
  // exitCode rather than exit: console output to a pipe is async in Node, and
  // process.exit here can truncate the one record of what the sweep did.
  if (failed > 0) process.exitCode = 1;
}

// Guarded so the suite can require this file for its pure helpers without
// launching a load test — the shape gateway-resume-spike.js established.
if (require.main === module) {
  main().catch(async (e) => {
    console.error('Fatal:', e);
    // The run may already have created resources before falling over. Reclaim
    // them rather than leave them on a shared tenancy.
    try {
      await reclaimOnce(activeLedgerPath);
    } catch (reclaimError) {
      console.error('Reclaim failed:', reclaimError.message);
    }
    process.exit(1);
  });
}

// Exported so the suite covers them without live API traffic.
module.exports = {
  readLedger, pruneLedger, ledgerEndpoints, reclaim, parseReclaimArg,
};
