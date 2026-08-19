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
 *   --max-fail-rate PCT
 *                  Exit non-zero when the failure rate exceeds this percentage
 *                  (default: 10). Pass 100 to never fail on rate alone.
 *   --allow-production
 *                  Run anyway when a target is refused by the guard below.
 *
 * Exit code:
 *   0 only for a run that measured something and stayed under the threshold.
 *   Non-zero when no round completed, or when either the qURL failure rate or
 *   the round failure rate exceeds --max-fail-rate. Load tests get wrapped in
 *   shell loops and runbook steps, and before this the script exited 0 whether
 *   it minted everything or nothing — see runReport.
 *
 * Target safety:
 *   QURL_ENDPOINT and CONNECTOR_URL must BOTH resolve to a host this script
 *   positively recognizes as non-production — loopback, or a hostname named in
 *   the comma-separated LOADTEST_TARGET_HOSTS env var. Every other host is
 *   refused. See the "Target safety guard" section below.
 *
 *   Two override strengths: LOADTEST_ALLOW_PRODUCTION=1 clears a target the
 *   guard merely failed to RECOGNIZE, but only the typed --allow-production
 *   flag clears a host known to be production. See isTargetAuthorized.
 *
 *   Only loopback is recognized without being named, so a run against a real
 *   sandbox needs LOADTEST_TARGET_HOSTS set. Note that 0.0.0.0 is NOT loopback
 *   (it is the unspecified address); name it explicitly if you bind there.
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
  // Say so: this splice OVERWRITES the caller's environment, so an operator
  // who just exported QURL_ENDPOINT would otherwise see a different value in
  // the guard's verdict table with no explanation for where it came from.
  console.log(`Loaded env overrides from ${envFile}`);
}

const config = require('../src/config');
const { mintLinks } = require('../src/connector');
const { createOneTimeLink } = require('../src/qurl');

const args = process.argv.slice(2);

/**
 * Read `--name value` or `--name=value` from an argv array.
 *
 * Both spellings, because only the space form used to be matched and the
 * equals form fell through to the default SILENTLY. That is tolerable for
 * --count, whose effect is visible in the very next line of output, and much
 * less so for --max-fail-rate, which decides the exit code: an operator who
 * typed `--max-fail-rate=100` to waive the check would have run at the strict
 * default and been failed by it two hours later, with nothing in the log
 * saying which threshold was actually applied. The summary now echoes the
 * threshold for the same reason.
 *
 * Takes argv as an argument rather than closing over the module's, so the flag
 * SPELLINGS are pinnable by a test — the same seam resolveGuardInputs exists
 * for, and for the same reason: nothing else here can catch a renamed flag.
 */
function readArg(argv, name, defaultVal) {
  const flag = `--${name}`;
  const idx = argv.indexOf(flag);
  if (idx !== -1) return argv[idx + 1] || defaultVal;
  const inline = argv.find((a) => a.startsWith(`${flag}=`));
  return inline === undefined ? defaultVal : inline.slice(flag.length + 1) || defaultVal;
}

const getArg = (name, defaultVal) => readArg(args, name, defaultVal);

/**
 * True when `--name` is present but carries no value — as the final token, or
 * as `--name=`. readArg hands back the default for both, which is the same
 * silent fallback the equals form used to have, and it is only tolerable for a
 * flag whose value shows up in the next line of output. A flag that decides
 * the exit code has to reject it: `--max-fail-rate` with the value fat-fingered
 * off would otherwise run at the strict default while the operator believed
 * they had set something else.
 */
function argValueMissing(argv, name) {
  const flag = `--${name}`;
  const idx = argv.indexOf(flag);
  if (idx !== -1) return !argv[idx + 1];
  const inline = argv.find((a) => a.startsWith(`${flag}=`));
  return inline !== undefined && inline.slice(flag.length + 1) === '';
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
//
// Boundary worth stating: production is recognized by NAME. A public IP that
// fronts production reads as `unrecognized`, not `production`, so the weaker
// env override would clear it. That is deliberate — the accident this guard
// exists to stop is NODE_ENV=production falling back to the hostnames below on
// a dev laptop, whereas pointing the load test at a bare production IP is not
// something anyone does by slipping.

// TODO(upstream-contract): production hostnames, mirrored from src/config.js's
// NODE_ENV=production fallbacks for QURL_ENDPOINT / CONNECTOR_URL and from
// connector.js's DETECT_TUNNEL_PROD_HOST_SUFFIX. When qurl-service moves or
// adds a production hostname, this list moves with it.
//
// `api.layerv.ai` is pinned as an exact HOST and deliberately not as a parent
// domain: `api.staging.layerv.ai` is a legitimate non-production endpoint (see
// connector.js's DETECT_TUNNEL_NON_PROD_QURL_ENDPOINT_HOSTS), so a
// domain-wide rule would misfile staging as production.
// `get.qurl.link` is also matched by the `qurl.link` domain rule below; it is
// pinned here as a literal because this set mirrors config.js's production
// fallbacks verbatim, and the drift test asserts against exactly those.
const PROD_HOSTS = new Set(['api.layerv.ai', 'get.qurl.link']);
// Matched as apex-or-subdomain. Every resolved qURL site is a `*.qurl.site`
// host, and minted links render as `https://qurl.link/#at_...`, so both
// domains are production wholesale — including a regional or replacement
// connector like `get-eu.qurl.link`, which is exactly the case an exact-host
// pin would miss. Unlike `layerv.ai`, neither domain has a non-production
// tenant: the non-prod tunnel spellings are `.qurl.site.layerv.xyz` /
// `.qurl.site.layerv.ai` and `qurl.link.layerv.xyz`, all of which end in
// `.layerv.xyz` / `.layerv.ai` and so do NOT match this rule.
const PROD_DOMAINS = ['qurl.site', 'qurl.link'];

// Verdicts that may proceed without --allow-production.
const SAFE_VERDICTS = new Set(['loopback', 'allowlisted']);
const VERDICT_LABEL = {
  loopback: 'ok — loopback',
  allowlisted: 'ok — named in LOADTEST_TARGET_HOSTS',
  production: 'REFUSED — production',
  unrecognized: 'REFUSED — not a recognized non-production host',
  unparseable: 'REFUSED — not a parseable URL',
};

/**
 * Lowercase, and drop trailing root dots. `api.layerv.ai.` is the same host
 * as `api.layerv.ai` to DNS but a different string, so without this the
 * fully-qualified spelling misses the production check on both sides of the
 * guard — and on the allowlist side that is a fail-OPEN, since the entry would
 * be stored as an ordinary grant instead of being rejected as production.
 */
function normalizeHost(host) {
  return String(host).toLowerCase().replace(/\.+$/, '');
}

/** Normalized hostname of a target URL, or null when it won't parse. */
function targetHost(rawUrl) {
  try {
    const host = normalizeHost(new URL(rawUrl).hostname);
    // `localhost:8080` — the realistic scheme-less typo — parses with
    // `localhost:` AS the scheme and an EMPTY host. Reporting that as a host
    // would label it 'unrecognized' and then offer the operator an empty
    // `LOADTEST_TARGET_HOSTS=` to set, which refuses identically: a closed
    // loop. Call it unparseable, which is both true and actionable.
    return host === '' ? null : host;
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
    // Honors ipv6LocalScope's documented contract, which requires the caller
    // to gate on the ':' (its leading parseInt is a lenient prefix parse, so
    // an un-gated DNS name can read as a hextet). This particular call cannot
    // change result either way — it compares against 'loopback', which that
    // function returns only for the exact string '::1' — so the gate is
    // conformance to a documented contract, not a live guard.
    return inner.includes(':') && ipv6LocalScope(inner) === 'loopback';
  }
  return ipv4LocalScope(parseIPv4Octets(host)) === 'loopback';
}

function isProductionHost(host) {
  if (PROD_HOSTS.has(host)) return true;
  return PROD_DOMAINS.some((domain) => host === domain || host.endsWith(`.${domain}`));
}

/**
 * Parse LOADTEST_TARGET_HOSTS — comma-separated hostnames the operator is
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
      // IPv6 is rejected by the same rule. Accepted limitation: loopback needs
      // no entry, but a sandbox reached by IPv6 LITERAL rather than by name
      // cannot be allowlisted at all — it would need --allow-production.
      // Sandboxes here have DNS names; revisit if one ever does not.
      errors.push(`LOADTEST_TARGET_HOSTS entry '${entry}' must be a bare hostname — no scheme, path, or port.`);
    } else if (/^\d+$/.test(entry)) {
      // `LOADTEST_TARGET_HOSTS=1` reads like a boolean and would otherwise be
      // stored as the entry '1', which can never match: `new URL('http://1')`
      // canonicalizes the host to 0.0.0.1, an address, never a name. Sound to
      // reject — an all-digit label never survives canonicalization as a name.
      errors.push(`LOADTEST_TARGET_HOSTS entry '${entry}' must be a bare hostname — this variable names the hosts to allow, it is not an on/off switch.`);
    } else if (entry.includes('*')) {
      errors.push(`LOADTEST_TARGET_HOSTS entry '${entry}' must be a bare hostname — wildcards are not matched.`);
    } else {
      // Normalize through the SAME parser a target goes through, so an entry
      // and a target spell the same host identically. Without this the two
      // sides can disagree — `api.layerv.ai..` normalizes to something
      // isProductionHost does not recognize, and a fullwidth or otherwise
      // IDNA-mapped entry is stored in a form no target can ever match, which
      // is the quietly-inert grant this function exists to prevent.
      const host = targetHost(`https://${entry}`);
      if (host === null || host === '' || host.split('.').includes('')) {
        errors.push(`LOADTEST_TARGET_HOSTS entry '${entry}' is not a parseable hostname.`);
      } else if (isProductionHost(host)) {
        // Production is not grantable through the environment: env vars get
        // copied between environments and pasted into .env.loadtest files, a
        // typed command-line flag does not.
        errors.push(`LOADTEST_TARGET_HOSTS entry '${entry}' is a production host — pass --allow-production on the command line if that is really the intent.`);
      } else {
        hosts.add(host);
      }
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

/**
 * Which override authorizes a refused target. The two strengths are
 * deliberately asymmetric.
 *
 * The typed flag clears anything. The environment clears exactly one case: a
 * real sandbox the operator did not enumerate ('unrecognized').
 *
 * It deliberately does not clear PRODUCTION, because `.env.loadtest` is
 * spliced into process.env at the top of this file — an env-only override
 * would let a gitignored file sitting in a working copy disable this guard
 * permanently and silently, the same vector parseTargetAllowlist refuses for
 * LOADTEST_TARGET_HOSTS. Refusing it there and allowing it here would leave
 * the small hole shut and the large one open.
 *
 * It does not clear 'unparseable' either: a URL that will not parse is never
 * a legitimate sandbox, so there is nothing for the weaker override to mean.
 */
function isTargetAuthorized(target, { allowProdFlag, allowProdEnv }) {
  if (!isRefusedTarget(target)) return true;
  if (allowProdFlag) return true;
  return target.verdict === 'unrecognized' && allowProdEnv;
}

/**
 * Resolve every input the guard reads from the outside world.
 *
 * Takes env and argv as arguments rather than reaching for the globals so the
 * VARIABLE NAMES themselves are pinnable by a test. That is not hypothetical:
 * this allowlist variable has already been renamed once, and renaming it here
 * while updating only the error strings would ship an allowlist that is always
 * empty, with every test still green.
 */
function resolveGuardInputs(env, argv) {
  return {
    allowProdFlag: argv.includes('--allow-production'),
    allowProdEnv: env.LOADTEST_ALLOW_PRODUCTION === '1',
    ...parseTargetAllowlist(env.LOADTEST_TARGET_HOSTS),
  };
}

/**
 * The entire enforcement decision, as data — main() only prints and exits.
 *
 * Split out because main() is not reachable from the test suite (it runs the
 * load test), and scripts/ is outside jest's collectCoverageFrom, so nothing
 * flags the gap. That combination once hid a live hazard: with the decision
 * inline, renaming the allowlist env var and updating only the error strings
 * would ship an allowlist that is always empty, with a green suite.
 */
function targetGuardReport({ targets, allowlistErrors = [], allowProdFlag, allowProdEnv }) {
  const refused = targets.filter(isRefusedTarget);
  const blocked = refused.filter((t) => !isTargetAuthorized(t, { allowProdFlag, allowProdEnv }));
  const lines = [];
  for (const err of allowlistErrors) lines.push(`FATAL: ${err}`);
  if (blocked.length > 0) {
    lines.push('FATAL: refusing to load test a target that is not a recognized sandbox.');
    for (const t of targets) {
      lines.push(`  ${t.name.padEnd(14)} = ${t.rawUrl}  [${VERDICT_LABEL[t.verdict]}]`);
    }
    lines.push('This run mints thousands of resources; a target has to be recognized as');
    lines.push('non-production before it gets that volume.');
    // The verdict table prints whole URLs, but the variable takes hostnames —
    // hand back the parsed host so the operator does not have to extract it.
    const nameable = [...new Set(blocked.filter((t) => t.verdict === 'unrecognized').map((t) => t.host))];
    if (nameable.length > 0) {
      lines.push(`If that is the intended sandbox: LOADTEST_TARGET_HOSTS=${nameable.join(',')}`);
    }
    if (blocked.some((t) => t.verdict === 'unparseable')) {
      lines.push('A target that will not parse needs a scheme, e.g. https://host:port.');
    }
    if (blocked.some((t) => t.verdict === 'production')) {
      // Named separately because LOADTEST_ALLOW_PRODUCTION will NOT clear a
      // production verdict — see isTargetAuthorized.
      lines.push('To load test production anyway, pass --allow-production on the command line.');
    }
  }
  const warnings = [];
  if (blocked.length === 0 && refused.length > 0) {
    // Name the override that fired: only the env leg can come from a
    // gitignored .env.loadtest the operator has forgotten about. Prints
    // rawUrl, not host, so a non-default port stays visible.
    const via = allowProdFlag ? '--allow-production' : 'LOADTEST_ALLOW_PRODUCTION=1';
    const named = refused.map((t) => `${t.name}=${t.rawUrl} (${t.verdict})`).join(', ');
    warnings.push(`WARNING: target guard overridden via ${via} — running against ${named}`);
  }
  return { refused, blocked, lines, warnings, fatal: lines.length > 0 };
}

// ---------------------------------------------------------------------------
// Run reporting
//
// Pure functions, for the same reason targetGuardReport is one: main() and
// runRound() are not reachable from the suite — they run the load test — and
// scripts/ is outside jest's collectCoverageFrom, so nothing flags an untested
// gap here. That combination is not hypothetical for this section. Every
// defect fixed below shipped with the file and survived its whole lifetime,
// and together they let a file leg issuing zero successful mint requests
// (#1168) read as a clean run: the failure counts were suppressed, a latency
// block was printed anyway, and the process exited 0.
//
// The stdout/stderr split is deliberate and load-bearing. Errors belong on
// stderr, as everything else in this file already has them, so stdout has to
// carry enough on its own to tell a good run from a bad one — `node
// scripts/loadtest-standalone.js > run.log` is the ordinary invocation and it
// keeps only that stream. The `fail=` counts, the summary's verdict line and
// the exit code are all on the stdout side for that reason; stderr adds only
// the messages behind them.

/**
 * The per-round line, as data.
 *
 * Each leg is gated on whether it ATTEMPTED anything, never on whether it
 * succeeded. Gating on successes inverts the report precisely: a partial
 * failure (ok=90 fail=10) prints in full, while total failure — the one case
 * worth shouting about — prints nothing but the round's elapsed time. That is
 * what rendered #1168's 100%-failing file leg as `[30s] Round 1: total=0.3s`.
 */
function roundReportLine({ elapsed, round, results }) {
  let line = `[${elapsed}s] Round ${round}: `;
  if (results.fileLinks > 0 || results.fileFail > 0) {
    line += `file(upload=${results.uploadMs.toFixed(0)}ms mint=${results.mintMs.toFixed(0)}ms ok=${results.fileLinks} fail=${results.fileFail}) `;
  }
  if (results.locLinks > 0 || results.locFail > 0) {
    line += `location(${results.locMs.toFixed(0)}ms ok=${results.locLinks} fail=${results.locFail}) `;
  }
  line += `total=${(results.totalMs / 1000).toFixed(1)}s`;
  return line;
}

/**
 * Record one failure against the message that caused it, weighted by the
 * number of attempts it took down — a file batch fails `batchSize` mints at
 * once, a location attempt exactly one. Weighting is what lets the tally be
 * read against the round line: its counts sum to that line's `fail=`.
 */
function tallyFailure(tally, message, weight) {
  tally.set(message, (tally.get(message) || 0) + weight);
}

// Distinct messages reported per leg per round. An error message can carry a
// unique id — a request id, a resource id — in which case every failure is its
// own key and an uncapped flush prints a line per failed attempt. The cap
// bounds that; the omitted keys are the rarest ones, and their volume is still
// named on the trailing line rather than silently dropped.
const ERROR_TALLY_LIMIT = 5;

/**
 * Flush a round's failures: each distinct message with how many attempts it
 * took down, most damaging first.
 *
 * This replaces `if (results.fileFail === 0) console.error(...)`, which deduped
 * on the failure COUNT rather than on the message — so a round printed its
 * first message and dropped every later one no matter what it said. A round
 * mixing a systemic call-shape bug with transient 429s reported whichever
 * landed first and hid the other, which is exactly the pair an operator is
 * trying to tell apart.
 */
function errorTallyLines(tally, label, limit = ERROR_TALLY_LIMIT) {
  // Insertion order is the tiebreak: Array#sort is stable, and a Map iterates
  // in insertion order, so equal-weight messages stay in first-seen order and
  // the output is reproducible for a given round.
  const ranked = [...tally.entries()].sort((a, b) => b[1] - a[1]);
  const lines = ranked.slice(0, limit).map(([message, n]) => `  ${label} error x${n}: ${message}`);
  const hidden = ranked.slice(limit);
  if (hidden.length > 0) {
    const attempts = hidden.reduce((sum, [, n]) => sum + n, 0);
    lines.push(`  ${label} error: ${hidden.length} further distinct message(s) covering ${attempts} attempt(s)`);
  }
  return lines;
}

// A load test run against a healthy sandbox fails a fraction of a percent of
// attempts, so 10% is far above transient 429/5xx noise while still well below
// the systemic breakage this threshold exists to catch — which shows up at or
// near 100%, not at 15%. Tunable per run with --max-fail-rate; 100 disables
// the check, since the comparison is strict.
const DEFAULT_MAX_FAIL_RATE_PCT = 10;

/**
 * Parse --max-fail-rate, as a percentage.
 *
 * Validated rather than left to `Number`'s NaN, which would compare false
 * against every rate and so disable the threshold silently — a fail-OPEN, and
 * this file refuses those (see parseTargetAllowlist). A typo here would
 * otherwise be discovered as a green exit code after a two-hour run.
 */
function parseMaxFailRate(raw) {
  // Checked before Number, which reads whitespace as 0 — a blank value would
  // otherwise become the strictest possible threshold rather than an error.
  if (String(raw).trim() === '') {
    return { error: `--max-fail-rate must be a percentage between 0 and 100, got '${raw}'.` };
  }
  const pct = Number(raw);
  if (!Number.isFinite(pct) || pct < 0 || pct > 100) {
    return { error: `--max-fail-rate must be a percentage between 0 and 100, got '${raw}'.` };
  }
  return { rate: pct / 100 };
}

const formatPct = (rate, digits = 1) => `${(rate * 100).toFixed(digits)}%`;

/**
 * Render a rate and the threshold it exceeded so the two never print the same
 * string. A rate that only just crosses the line — 1001/10000 against 10% —
 * rounds to the threshold's own spelling, and `10.0% exceeds 10.0%` reads as a
 * bug in the tool rather than a finding about the run. Widen both together
 * until they differ, bounded at four decimals — a gap smaller than that is
 * reachable only through float error, never through a ratio of two counts.
 */
function formatRatePair(rate, threshold) {
  for (const digits of [1, 2, 3]) {
    if (formatPct(rate, digits) !== formatPct(threshold, digits)) {
      return [formatPct(rate, digits), formatPct(threshold, digits)];
    }
  }
  return [formatPct(rate, 4), formatPct(threshold, 4)];
}

/**
 * The end-of-run report, as data: the measurement block plus the pass/fail
 * verdict main() turns into an exit code.
 *
 * `roundsAttempted` is passed separately from `allResults` because a round
 * that throws never lands in the results at all. Without it a run whose every
 * round died printed `Rounds: 0` and exited 0 — the same invisibility as the
 * per-round line's, one level up.
 *
 * Two rates are judged against the threshold, not one. They fail
 * independently: a run can mint every qURL it attempts across the rounds that
 * survive while most rounds die in the upload before reaching a mint, and a
 * single blended rate would dilute either failure with the other's successes.
 */
function runReport({ allResults, roundsAttempted, maxFailRate }) {
  const sum = (pick) => allResults.reduce((total, r) => total + pick(r), 0);
  const fileLinks = sum((r) => r.fileLinks);
  const fileFail = sum((r) => r.fileFail);
  const minted = fileLinks + sum((r) => r.locLinks);
  const failedLinks = fileFail + sum((r) => r.locFail);
  const attemptedLinks = minted + failedLinks;
  const roundsCompleted = allResults.length;
  const roundsFailed = Math.max(0, roundsAttempted - roundsCompleted);

  const lines = [];
  lines.push(roundsFailed > 0
    ? `Rounds: ${roundsCompleted} completed, ${roundsFailed} failed`
    : `Rounds: ${roundsCompleted}`);
  lines.push(`Total links minted: ${minted}`);
  lines.push(`Total failures: ${failedLinks}`);
  // Echoed on every run, passing or failing. This is the value that decided
  // the exit code, and printing it only on the FAILED lines meant a run that
  // silently took the default had nothing in its log to say so.
  lines.push(`Failure threshold: ${formatPct(maxFailRate)} (--max-fail-rate)`);

  if (roundsCompleted > 0) {
    lines.push(`Avg round time: ${(sum((r) => r.totalMs) / roundsCompleted / 1000).toFixed(1)}s`);
    // Rounds that ran the file leg — every entry has completed it, since a
    // round throwing inside the leg never reaches allResults.
    const fileRounds = allResults.filter((r) => r.uploadMs > 0);
    if (fileRounds.length > 0) {
      const mean = (rounds, pick) => rounds.reduce((total, r) => total + pick(r), 0) / rounds.length;
      // Every one of these rounds uploaded successfully, so the upload average
      // is over all of them however their mints then went.
      const avgUpload = mean(fileRounds, (r) => r.uploadMs).toFixed(0);
      // Mint latency is averaged over the rounds that actually minted, and
      // reported only when there are some. The old gate was
      // `allResults[0].uploadMs > 0`, which is set before the first mint is
      // even attempted and so held while every mint failed: the summary
      // reported how long the failures took as though it were how long the
      // successes took, and a fast failure reads as a fast success.
      //
      // Excluding rounds that minted nothing matters for the same reason one
      // granularity down. Their mintMs measures only failures, which are
      // typically much faster than a real mint, so folding them in drags the
      // average toward the failures — a run of healthy rounds plus dead ones
      // would report a mint latency neither kind ever saw.
      //
      // Residual limit, inherent to timing the batch loop as a whole rather
      // than each attempt: this is time per ROUND, not per qURL, and a
      // partially-failing round still blends its own failures into its mintMs.
      const mintedRounds = fileRounds.filter((r) => r.fileLinks > 0);
      // Nothing minted has two causes and they are not the same news: every
      // attempt failed, or nothing was ever attempted (--count 0). Reporting
      // the second as "all 0 mint attempt(s) failed" states a failure that
      // did not happen.
      const noMintNote = fileFail > 0
        ? `n/a — all ${fileFail} mint attempt(s) failed`
        : 'n/a — no mint was attempted';
      lines.push(mintedRounds.length > 0
        ? `Avg upload: ${avgUpload}ms, avg mint/round: ${mean(mintedRounds, (r) => r.mintMs).toFixed(0)}ms`
        : `Avg upload: ${avgUpload}ms, avg mint/round: ${noMintNote}`);
    }
  }

  const linkFailRate = attemptedLinks > 0 ? failedLinks / attemptedLinks : 0;
  const roundFailRate = roundsAttempted > 0 ? roundsFailed / roundsAttempted : 0;
  const reasons = [];
  // A run that never completed a round measured nothing; reporting that as a
  // pass is the failure this exit code exists to prevent, and no rate above
  // catches it when roundsAttempted is itself 0.
  if (roundsCompleted === 0) reasons.push('no round completed');
  // Rounds ran but no qURL was ever attempted — `--count 0`, or legs that all
  // did nothing. Both rates are 0/0 and read as a clean run, so this is the
  // same unmeasured-run pass one level down from 'no round completed'.
  if (roundsCompleted > 0 && attemptedLinks === 0) reasons.push('no qURL was attempted');
  if (linkFailRate > maxFailRate) {
    const [rate, limit] = formatRatePair(linkFailRate, maxFailRate);
    reasons.push(`link failure rate ${rate} (${failedLinks}/${attemptedLinks}) exceeds --max-fail-rate ${limit}`);
  }
  if (roundFailRate > maxFailRate) {
    const [rate, limit] = formatRatePair(roundFailRate, maxFailRate);
    reasons.push(`round failure rate ${rate} (${roundsFailed}/${roundsAttempted}) exceeds --max-fail-rate ${limit}`);
  }
  for (const reason of reasons) lines.push(`FAILED: ${reason}`);
  return { lines, failed: reasons.length > 0, linkFailRate, roundFailRate };
}

async function runRound(roundNum) {
  const roundStart = performance.now();
  const results = { fileLinks: 0, fileFail: 0, locLinks: 0, locFail: 0, uploadMs: 0, mintMs: 0, locMs: 0 };

  // Keyed by message and flushed once per leg below, so a round that mixes a
  // systemic failure with transient ones reports both. Scoped per round: the
  // tally answers "what went wrong in THIS round", and a run-long map would
  // reprint every earlier round's messages at every flush.
  const fileErrors = new Map();
  const locErrors = new Map();

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
        await mintLinks(uploadResult.resource_id, { expiresAt, n: batchSize });
        results.fileLinks += batchSize;
      } catch (e) {
        tallyFailure(fileErrors, e.message, batchSize);
        results.fileFail += batchSize;
      }
    }
    results.mintMs = performance.now() - mintStart;
    for (const line of errorTallyLines(fileErrors, 'File mint')) console.error(line);
  }

  // Location pipeline
  if (INCLUDE_LOCATION) {
    const locStart = performance.now();
    for (let i = 0; i < COUNT; i++) {
      try {
        await createOneTimeLink(TEST_LOCATION_URL, '24h', 'Load test location');
        results.locLinks++;
      } catch (e) {
        tallyFailure(locErrors, e.message, 1);
        results.locFail++;
      }
    }
    results.locMs = performance.now() - locStart;
    for (const line of errorTallyLines(locErrors, 'Location mint')) console.error(line);
  }

  const totalMs = performance.now() - roundStart;
  return { ...results, totalMs };
}

async function main() {
  // Preflight checks
  if (!config.QURL_API_KEY) { console.error('FATAL: QURL_API_KEY not set'); process.exit(1); }
  // Parsed up here with the other preflight checks: a mistyped threshold that
  // was only read at the summary would surface as a green exit code two hours
  // after the run it was meant to judge.
  if (argValueMissing(args, 'max-fail-rate')) {
    console.error('FATAL: --max-fail-rate needs a percentage, e.g. --max-fail-rate 10.');
    process.exit(1);
  }
  const { rate: maxFailRate, error: maxFailRateError } =
    parseMaxFailRate(getArg('max-fail-rate', String(DEFAULT_MAX_FAIL_RATE_PCT)));
  if (maxFailRateError) { console.error(`FATAL: ${maxFailRateError}`); process.exit(1); }
  // Refuse any target not positively recognized as non-production. See the
  // "Target safety guard" section above for why this is an allowlist.
  const { allowProdFlag, allowProdEnv, hosts: allowedHosts, errors: allowlistErrors } =
    resolveGuardInputs(process.env, process.argv);
  const report = targetGuardReport({
    targets: classifyTargets({
      qurlEndpoint: config.QURL_ENDPOINT,
      connectorUrl: config.CONNECTOR_URL,
      allowedHosts,
    }),
    // Reported even under --allow-production: a malformed entry means the
    // operator believes they granted a host they did not, which is worth
    // knowing before the next run relies on it.
    allowlistErrors,
    allowProdFlag,
    allowProdEnv,
  });
  for (const line of report.lines) console.error(line);
  if (report.fatal) process.exit(1);
  for (const line of report.warnings) console.warn(line);

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

      console.log(roundReportLine({ elapsed, round, results }));
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
  const summary = runReport({ allResults, roundsAttempted: round, maxFailRate });
  for (const line of summary.lines) console.log(line);
  // exitCode rather than exit: writes to a pipe are asynchronous in Node, and
  // exiting here can truncate the summary that explains the code being set.
  if (summary.failed) process.exitCode = 1;
}

// Only run the CLI entry point when invoked directly. Imported-from-test loads
// only the exported helpers — see tests/loadtest-target-guard.test.js.
if (require.main === module) {
  main().catch(e => { console.error('Fatal:', e); process.exit(1); });
}

module.exports = {
  // Reporting decisions, exported for tests/loadtest-reporting.test.js — see
  // the "Run reporting" section for why they are pure rather than inline.
  readArg,
  argValueMissing,
  roundReportLine,
  tallyFailure,
  errorTallyLines,
  parseMaxFailRate,
  formatRatePair,
  runReport,
  ERROR_TALLY_LIMIT,
  DEFAULT_MAX_FAIL_RATE_PCT,
  resolveGuardInputs,
  targetGuardReport,
  VERDICT_LABEL,
  isTargetAuthorized,
  normalizeHost,
  targetHost,
  isLoopbackHost,
  isProductionHost,
  parseTargetAllowlist,
  classifyTarget,
  classifyTargets,
  isRefusedTarget,
};
