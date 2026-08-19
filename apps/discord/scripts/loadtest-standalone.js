#!/usr/bin/env node
/**
 * Standalone load test script — runs outside Discord, hits APIs directly.
 * Usage: node scripts/loadtest-standalone.js --count 200 --duration 7200 --interval 30
 *
 * Options:
 *   --count N      Recipients per round (default: 100)
 *   --duration S   Total duration in seconds (default: 7200 = 2 hours)
 *   --interval S   Seconds between rounds (default: 60)
 *   --file PATH    Local file to upload (default: generates a 1MB test payload
 *                  in memory — nothing is written to disk)
 *   --location     Include a location link in each round
 *   --allow-production
 *                  Run anyway when a target is refused by the guard below.
 *
 * Flag syntax:
 *   A flag that takes a value accepts either `--file PATH` or `--file=PATH`,
 *   and a repeated flag takes its LAST value. A value that is missing, empty,
 *   or itself a flag is refused before the run starts rather than falling
 *   back to the default — see readFlag below for why that fallback was the
 *   more dangerous answer.
 *
 *   Consequences worth knowing before you type them:
 *     - a path that genuinely begins with `--` needs the inline form,
 *       `--file=--weird.bin`; the separated form reads it as a flag.
 *     - --file must name a REGULAR file. A pipe, process substitution or
 *       /dev/stdin is refused: the file is re-read once per round, so a pipe
 *       would upload real bytes on round one and nothing afterwards.
 *     - a MISSPELLED flag is still ignored silently, and so is `=` on a
 *       boolean flag (`--location=true` leaves the location leg off). Both
 *       are recorded as known gaps at readFlag below.
 *
 *   Hand-rolled rather than node:util parseArgs, which covers these shapes
 *   but throws on the first bad flag — forfeiting "name every bad flag in one
 *   pass" — and rejects `--count -5` as an ambiguous option rather than as
 *   the bad count it is.
 *
 * Load shape:
 *   The file leg mirrors a real send's mintLinksInBatches: a resource's token
 *   pool is TOKENS_PER_RESOURCE deep, so it re-uploads each time the pool
 *   drains. --count N therefore issues ceil(N / TOKENS_PER_RESOURCE) uploads
 *   and the same number of mint calls per round — 10 of each at the default
 *   --count 100. See planMintBatches below.
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
const { mintLinks, reUploadBuffer } = require('../src/connector');
const { createOneTimeLink } = require('../src/qurl');

// The same pool depth the send pipeline batches against — imported, not
// copied, so a change to the cap reaches this script instead of silently
// leaving it issuing a different number of uploads than a real send.
const { TOKENS_PER_RESOURCE } = require('../src/constants');

const args = process.argv.slice(2);

// ---------------------------------------------------------------------------
// CLI flag reading
//
// One reader for every flag that takes a value, so --file and the three
// numeric flags cannot disagree about what a malformed command line means.
// It replaces a `getArg` whose `args[idx + 1] || defaultVal` collapsed three
// different operator mistakes onto the silent default:
//
//   - the flag absent — the only one of the three that should default;
//   - the flag as the final token, with nothing after it;
//   - the flag given an explicitly empty value, `--file ""`.
//
// Defaulting is the worst available outcome for all but the first, because
// the run then does something real that nobody asked for: `--file` as a
// trailing token uploaded a generated 1MB file for the whole window while the
// operator believed they were testing their own payload. Same shape as the
// numeric flags below, where the default ran 100 recipients per round instead
// of the count that was typed.
//
// Three further shapes the old reader got wrong, all of them silent:
//
//   - a value that looks like a flag. `--file --location` consumed
//     `--location` as the path AND left the location leg off, so the command
//     line differed from the run in two ways at once. Recognized on the `--`
//     prefix, which deliberately leaves a lone `-` to the value validators:
//     `--count -5` still reports a bad count rather than a missing value.
//   - `--file=/path`. An `indexOf('--file')` never matches it, so the flag
//     read as absent and the run generated its own payload — the same
//     unasked-for run, from a spelling most CLIs accept. This is also the
//     form that reaches a path which genuinely starts with `--`.
//   - a repeated flag resolved to its FIRST occurrence, so appending
//     `--count 5` to a recalled command line left the earlier value in force.
//     Last wins here, which is what re-typing a flag is meant to do.
//
// Pure and argv-taking, following resolveGuardInputs below, so the suite can
// cover it: the constants it feeds are read by loops inside runRound, which
// no test can reach.
//
// `defaultLabel` is what the missing-value messages call the default — the
// value itself for the numeric flags, prose for --file, whose default is not
// a path at all. A flag whose defaultValue is not worth printing must pass
// one: without it the fallback is String(defaultValue), which renders null as
// the literal "null". Both call sites do; a fourth flag has to as well.
//
// argv is assumed to hold strings, which is what process.argv gives. The
// function is exported for its own tests, not as a general-purpose parser.
function readFlag(argv, flag, defaultValue, defaultLabel = String(defaultValue)) {
  const token = `--${flag}`;
  const inlinePrefix = `${token}=`;
  let index = -1;
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === token || argv[i].startsWith(inlinePrefix)) index = i;
  }
  if (index === -1) return { value: defaultValue };
  const raw = argv[index];
  // An empty inline value (`--file=`) is handed on rather than rejected here.
  // What counts as an empty value is the caller's call — each validator
  // already has to reject `--file ""` arriving by the separated form, and
  // routing both spellings to the same check keeps them from diverging.
  if (raw.startsWith(inlinePrefix)) return { value: raw.slice(inlinePrefix.length) };
  const next = argv[index + 1];
  if (next === undefined) {
    return { error: `${token} was given no value (omit it to use the default of ${defaultLabel})` };
  }
  if (next.startsWith('--')) {
    // Carries the same recovery hint as the branch above: from the operator's
    // seat these are one mistake — they forgot the value — so telling them
    // how to get the default in one case and not the other is arbitrary.
    return {
      error: `${token} was given no value — the next argument is the flag ${next} `
        + `(omit it to use the default of ${defaultLabel})`,
    };
  }
  return { value: next };
}

// Two gaps in the same fault class are deliberately left open here, recorded
// so they are deferrals rather than oversights.
//
// UNKNOWN FLAGS. readFlag is pull-based — it scans argv for one named flag —
// so nothing can see a token that matched nothing. `--fil /tmp/payload.bin`
// and `-file /tmp/payload.bin` both read as "no --file given" and run the
// full window uploading the generated 1MB payload, from a one-character typo:
// the same outcome the header above calls the worst available one. Closing it
// needs a push-based pass that tokenizes argv once against a flag spec and
// reports what went unconsumed, which also subsumes the boolean case below.
// That is a larger change than routing the value-taking flags through one
// reader, and it is not what this one does.
//
// BOOLEAN FLAGS are not routed through readFlag: they take no value, so it
// has nothing to read for them. That leaves `--location=true` and
// `--allow-production=true` unrecognized and therefore silently off — the
// boolean leg of the same hole. Refusing them means first deciding what
// `--location=false` ought to mean, so it stays a separate change rather than
// a quiet side effect of this one.
// Takes argv like every other reader here, so the suite can reach it. It was
// the one flag reader closing over the module's `args`, which is why
// --location had no coverage at all.
const hasFlag = (argv, name) => argv.includes(`--${name}`);

// Numeric flags are validated, not merely parsed. `parseInt` fails silently
// in three directions here: a non-numeric value gives NaN, a negative one is
// returned intact, and with no radix '0x64' reads as 100 while '1e9'
// truncates to 1. NaN and negatives converge on the worst outcome — every
// loop a round runs is bounded by COUNT, the file leg's batch plan and
// the location leg's `i++` alike, so none of them enter. The run then holds
// the target for its whole DURATION_S window issuing zero requests and prints
// "Total links minted: 0" as though that were a measurement.
//
// Matches the whole string against digits rather than parsing its leading
// characters, so a flag-shaped value like '--location' is refused here too.
// readFlag now rejects that shape before this is reached, which makes the
// check redundant on the resolver's path and deliberately kept: this is
// exported and unit-tested in its own right, and a validator that accepts
// '--location' as a count would be wrong whatever called it.
//
// Kept pure so the suite can cover it: the loops it protects live in
// runRound, which no test can reach — it is not exported and its only caller
// is behind `require.main === module`.
function parsePositiveInt(flag, raw) {
  const text = String(raw);
  const trimmed = text.trim();
  if (!/^\d+$/.test(trimmed)) {
    // Quotes the value as given, not the trimmed one, so '' and '  ' are
    // visible as themselves rather than as a message that looks truncated.
    return { error: `--${flag} must be a positive whole number, got ${JSON.stringify(text)}` };
  }
  const value = Number(trimmed);
  if (value === 0) return { error: `--${flag} must be greater than zero` };
  // Past 2^53 the parse stops being exact, so the number the loops would run
  // on is not the number echoed back to the operator. This bounds the parse,
  // not the workload — a count that is exactly representable can still be far
  // more than DURATION_S has room for.
  if (!Number.isSafeInteger(value)) return { error: `--${flag} is too large to be exact: ${trimmed}` };
  return { value };
}

// Resolve all three numeric flags from an argv array. Pure and taking argv as
// a parameter, following resolveGuardInputs below, so the suite covers the
// wiring and not merely the parser: the constants below are the actual
// regression surface, and a call site quietly reverted to `parseInt` would
// leave a green parsePositiveInt behind it.
//
// Argv shape comes from readFlag above, so these three answer a malformed
// command line the same way --file does; what stays here is the part that is
// specific to a number. That includes both spellings: `--count=200` is not
// `--count` to an indexOf, so it read as the flag being ABSENT and ran the
// default of 100 with nothing said. `--duration=60` is the one that hurts —
// it held the target for the default 7200 seconds, so an operator who asked
// for a minute got two hours.
//
// The defaults are ROUTED THROUGH parsePositiveInt rather than returned
// beside it, so a default that the validator would reject cannot ship, and
// there is no "was this token typed or defaulted?" branch between readFlag
// and the parse. They are spelled as strings only to look like the argv they
// stand in for — parsePositiveInt opens with String(raw), so a numeric
// default would behave identically. The routing is what matters here, not
// the quoting.
function resolveNumericArgs(argv) {
  const errors = [];
  const read = (flag, defaultValue) => {
    const { value: raw, error: shapeError } = readFlag(argv, flag, defaultValue);
    if (shapeError) {
      errors.push(shapeError);
      return NaN;
    }
    const { value, error } = parsePositiveInt(flag, raw);
    if (error) {
      errors.push(error);
      return NaN;
    }
    return value;
  };
  // Collected rather than thrown: this runs at module load, which the suite
  // reaches through require(), so the exit belongs in main() alongside every
  // other fatal. Collecting also names every bad flag in one pass instead of
  // one per re-run.
  return { count: read('count', '100'), durationS: read('duration', '7200'), intervalS: read('interval', '60'), errors };
}

// Resolve --file from argv. Null means "none given, generate one", which is
// the ONLY reading of this flag that may fall back to the generated payload —
// every other reading is an operator who named something and must be told
// their command line was not understood.
//
// Split from the readability check below so this half stays pure: argv shapes
// are covered without a filesystem, and the filesystem half is covered
// against real files.
function resolveFileArg(argv) {
  const errors = [];
  const { value, error } = readFlag(argv, 'file', null, 'an auto-generated 1MB test file');
  if (error) {
    errors.push(error);
    return { filePath: null, errors };
  }
  if (value === null) return { filePath: null, errors };
  // Whitespace is checked but NOT stripped: a filename may legitimately carry
  // a leading or trailing space, so trimming would resolve a real path to a
  // different one. A value that is ONLY whitespace is a mistyped flag rather
  // than a path, and the two spellings of it fail differently if left alone —
  // `--file ""` is falsy, so runRound's `FILE_PATH || !INCLUDE_LOCATION`
  // silently generates a payload, while `--file "  "` is truthy and reaches
  // fs.readFileSync to throw mid-round. One message covers both.
  if (value.trim() === '') {
    errors.push(`--file must name a file to upload, got ${JSON.stringify(value)}`);
    return { filePath: null, errors };
  }
  return { filePath: value, errors };
}

// Prove the upload file is readable BEFORE the run starts.
//
// Left to fs.readFileSync, this lands inside the first runRound — which is
// after the preflight smoke test has already minted a real resource. So a
// mistyped path costs a live resource and exits on an unhandled ENOENT stack
// trace, in a script whose whole point is to be started and walked away from.
//
// Returns a message rather than throwing or exiting, so it joins the argument
// errors main() already prints in one pass. Kept out of resolveFileArg so
// that stays pure, and out of module load so `require()`ing this file from
// the suite does not stat the operator's disk.
function checkUploadFile(filePath) {
  // Quoted, like resolveFileArg quotes its own rejection. That function goes
  // out of its way to PRESERVE a leading or trailing space in a real
  // filename, so this is exactly where such a path lands — and rendered raw,
  // `--file /tmp/ spaced  is not a regular file` reads as `/tmp/spaced` and
  // sends the operator looking for the wrong file. A path containing a
  // newline is worse: it splits the line, and only the first half carries the
  // FATAL prefix, so the second reads as a separate fabricated error.
  const shown = JSON.stringify(filePath);
  let stats;
  try {
    // statSync rather than existsSync: existsSync is also true for a
    // directory, and readFileSync would then throw EISDIR out of the round
    // this check exists to protect. It also FOLLOWS symlinks, matching what
    // readFileSync will do — lstatSync here would reject a symlink pointing
    // at a perfectly good payload.
    stats = fs.statSync(filePath);
  } catch (e) {
    return `--file ${shown} cannot be read — ${e.message}`;
  }
  if (!stats.isFile()) {
    // Not just directories. A FIFO, socket, or character device reaches
    // readFileSync too, and runRound re-reads the file once PER ROUND: a pipe
    // would upload real bytes on round one and nothing on every round after,
    // which is a silently wrong measurement rather than a failure. A FIFO
    // with no writer blocks readFileSync forever.
    return `--file ${shown} is not a regular file`;
  }
  try {
    // Existence is not readability: a file owned by another user is the
    // realistic way this bites, and statSync succeeds on it.
    fs.accessSync(filePath, fs.constants.R_OK);
  } catch (e) {
    return `--file ${shown} is not readable — ${e.message}`;
  }
  return null;
}

// Every argument fault in one list, as data — main() only prints and exits.
// Follows targetGuardReport below, for the reason spelled out there: main() is
// unreachable from the suite, so a decision left inline in it is a decision
// nothing can test.
//
// That is not hypothetical here. With this composition inline, three separate
// regressions passed the entire suite green: dropping the --file errors from
// the concatenation (which silently reinstates the exact default-payload bug
// this change exists to remove), discarding the readability result, and moving
// the whole check after the smoke test that mints the first real resource.
//
// `check` is injected so the suite can drive the composition without a disk.
// Called from main() rather than at module load, so `require()`ing this file
// from a test does not stat the operator's disk.
function resolveArgErrors(argv, check = checkUploadFile) {
  const { errors: numericErrors } = resolveNumericArgs(argv);
  const { filePath, errors: fileErrors } = resolveFileArg(argv);
  const errors = [...numericErrors, ...fileErrors];
  // Guarded on filePath rather than run unconditionally: null means either no
  // --file was given or its shape already failed above, and statting that
  // would add a second message naming a path the operator never typed.
  if (filePath) {
    const fileError = check(filePath);
    if (fileError) errors.push(fileError);
  }
  return errors;
}

// The values only. Both resolvers run again inside resolveArgErrors, which is
// where the errors are read — they are discarded here on purpose, because
// nothing may act on them until main() has printed them and exited. A bad
// flag therefore cannot reach the loops through a NaN left in one of these
// constants: main() re-resolves and exits before runRound is ever called.
// Both resolvers are pure, so resolving twice costs an argv scan.
const {
  count: COUNT, durationS: DURATION_S, intervalS: INTERVAL_S,
} = resolveNumericArgs(args);
const { filePath: FILE_PATH } = resolveFileArg(args);
const INCLUDE_LOCATION = hasFlag(args, 'location');
const TEST_LOCATION_URL = 'https://www.google.com/maps/place/?q=place_id:ChIJLU7jZClu5kcRbUm7GCkGkNQ'; // Eiffel Tower

// The auto-generated payload is byte-identical on every round — `Buffer.alloc`
// fills a fixed length with a fixed byte — and the upload filename comes from
// `loadtest-round<n>.bin` at the call site, never from the path. So the
// round-trip through the filesystem this replaced bought nothing: it wrote a
// fresh 1MB file into /tmp every round, read it straight back into a buffer,
// and never removed it. Over a default 2h run at a 60s interval that is ~120
// files and ~120MB left behind, on the one code path here designed to be
// started and walked away from — a plausible way to fill the filesystem
// during exactly the long soak this script exists to run.
//
// Allocated once and reused, so the footprint is 1MB per process rather than
// 1MB per round, and there is no cleanup to get wrong: no unlink to skip on a
// throw, nothing left behind on SIGINT, and no /tmp write at all. A
// caller-supplied --file is still read from disk each round in runRound —
// that is the only case with a path to read.
//
// Every round gets the same Buffer *reference*, which is safe only because
// rounds are strictly sequential (main awaits each runRound before the next)
// and the one consumer, reUploadBuffer, reads the buffer without mutating it.
// Both hold today; a future change that overlaps rounds or hands this to a
// mutating consumer needs a per-round copy instead.
//
// Allocated lazily rather than at module load so that neither a --file run
// nor the test suite's require() of this module pays for 1MB it never uses.
let generatedPayload = null;
function generateTestPayload() {
  if (generatedPayload === null) {
    generatedPayload = Buffer.alloc(1024 * 1024, 'A'); // 1MB
  }
  return generatedPayload;
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
    // Through hasFlag like --location, rather than an inline includes: the
    // two boolean flags reading argv two different ways is the residual of
    // the split this change removes, and this is the one that gates a
    // production-target override.
    allowProdFlag: hasFlag(argv, 'allow-production'),
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

/**
 * Batch plan for minting `count` links against a token pool `tokensPerResource`
 * deep — the load-test mirror of mintLinksInBatches in ../src/commands.js.
 *
 * Extracted as a pure function because scripts/ sits outside this app's jest
 * `collectCoverageFrom`, so loop logic left inline here is unenforced. The plan
 * is data, so tests/loadtest-mint-batches.test.js can pin it without a
 * connector; runRound below only executes it.
 *
 * `reupload: true` marks every batch after the first, because the initial
 * resource arrives with a full pool and each batch drains it. That reproduces
 * mintLinksInBatches' `tokensUsed >= TOKENS_PER_RESOURCE && i > 0` guard: only
 * the final batch can be short, so at every i > 0 the previous batch was full
 * and tokensUsed has already reached the cap — the two conditions collapse into
 * `i > 0`. Kept as an explicit per-batch flag so the mirror stays legible if
 * the batcher's guard ever stops being equivalent.
 *
 * @param {number} count — links to mint across the whole plan.
 * @param {number} [tokensPerResource] — pool depth; defaults to TOKENS_PER_RESOURCE.
 * @returns {Array<{size: number, reupload: boolean}>} — empty when count <= 0.
 *   Fractional counts are not gated here (0.5 plans one batch of 0.5); the
 *   CLI can't produce one, since parsePositiveInt admits only whole numbers.
 */
function planMintBatches(count, tokensPerResource = TOKENS_PER_RESOURCE) {
  const batches = [];
  for (let i = 0; i < count; i += tokensPerResource) {
    batches.push({ size: Math.min(tokensPerResource, count - i), reupload: i > 0 });
  }
  return batches;
}

async function runRound(roundNum) {
  const roundStart = performance.now();
  const results = {
    fileLinks: 0, fileFail: 0, locLinks: 0, locFail: 0,
    uploadMs: 0, mintMs: 0, locMs: 0,
    reuploads: 0, reuploadFail: 0, reuploadMs: 0,
  };

  // File pipeline
  if (FILE_PATH || !INCLUDE_LOCATION) {
    const fileBuffer = FILE_PATH ? fs.readFileSync(FILE_PATH) : generateTestPayload();

    // Upload through the bot's own connector client rather than a hand-rolled
    // fetch. The copy this replaced put the same multipart body on the wire
    // but kept none of the guards around it, and each absence bites hardest
    // in exactly this script:
    //
    //   - no `signal`, so a connector that accepts the connection and then
    //     stalls hangs the round forever, with nothing in a run designed to
    //     be left alone for two hours to notice.
    //   - no `success` check.
    //   - no `resource_id` check, so a 200 carrying no id fed `undefined`
    //     into mintLinks and every batch threw `Invalid resource ID format:
    //     undefined` — reporting 100% MINT failure for what was an upload
    //     fault. That is the same misdirected blame #1168 removed from the
    //     mint call just below.
    //
    // reUploadBuffer despite the name: the "re-" only distinguishes it from
    // the sibling that first downloads from Discord CDN, and registering an
    // in-memory buffer as a fresh resource is precisely what this needs. Its
    // two trailing parameters are deliberately omitted — apiKey falls back to
    // config.QURL_API_KEY, which is what the hand-rolled header read, and
    // appendViewerTtl drops viewerTtlSeconds unless it is positive-finite, so
    // leaving it off sends the same single `file` part, under the same
    // filename and content type, that the hand-rolled form did. What is new
    // is the timeout and the two response checks.
    //
    // Deliberately NOT wrapped, unlike the re-upload leg below: a failure here
    // leaves nothing to mint against at all, so there is no partial round to
    // salvage — it throws out of runRound, main reports the round FAILED and
    // it never reaches allResults. The leg below states its own opposite
    // policy and why. Don't "harmonize" the two.
    //
    // Hoisted: the re-upload leg below registers each fresh resource under
    // the same filename, so a round's resources are one named series.
    const uploadName = `loadtest-round${roundNum}.bin`;
    const uploadStart = performance.now();
    const uploadResult = await reUploadBuffer(
      fileBuffer,
      uploadName,
      'application/octet-stream',
    );
    results.uploadMs = performance.now() - uploadStart;

    // Mint a pool at a time, re-uploading once each pool drains — the shape a
    // real send takes through mintLinksInBatches (../src/commands.js).
    //
    // The re-upload leg is what makes this leg generate real load: reusing one
    // resource_id for every batch spends the initial pool on batch 1 and takes
    // `quota_exceeded` for the rest. tests/loadtest-mint-batches.test.js has
    // the full regression narrative and the numbers.
    const expiresAt = expiryToISO('24h');
    let currentResourceId = uploadResult.resource_id;
    // Own flag rather than reusing fileFail as the "have we logged yet?"
    // signal: a failed re-upload charges fileFail too, so keying the mint log
    // off it would swallow the first mint error on any round where a
    // re-upload failed first — losing exactly the diagnostic that explains
    // what went wrong on the round most in need of one.
    let mintErrorLogged = false;
    for (const batch of planMintBatches(COUNT)) {
      if (batch.reupload) {
        const reStart = performance.now();
        let re = null;
        try {
          re = await reUploadBuffer(fileBuffer, uploadName, 'application/octet-stream');
        } catch (e) {
          if (results.reuploadFail === 0) console.error(`  File re-upload error: ${e.message}`);
          results.reuploadFail++;
        }
        results.reuploadMs += performance.now() - reStart;
        // The previous resource's pool is spent, so there is nothing left to
        // mint against — charge this batch as failed and keep going. That
        // costs one batch per failed upload instead of abandoning the round,
        // so a transient connector blip doesn't truncate the run.
        if (!re) {
          results.fileFail += batch.size;
          continue;
        }
        currentResourceId = re.resource_id;
        results.reuploads++;
      }

      const mintStart = performance.now();
      try {
        await mintLinks(currentResourceId, { expiresAt, n: batch.size });
        results.fileLinks += batch.size;
      } catch (e) {
        if (!mintErrorLogged) {
          console.error(`  File mint error: ${e.message}`);
          mintErrorLogged = true;
        }
        results.fileFail += batch.size;
      }
      // Accumulated per batch rather than wrapped around the loop, so
      // re-upload time stays out of the mint figure — otherwise the new leg
      // would silently inflate reported mint latency.
      results.mintMs += performance.now() - mintStart;
    }
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
  //
  // Arguments first: these need no config, no network and no target guard,
  // and a bad flag is the only fault here that otherwise survives to the end
  // of a full run — exiting 0 having done nothing at all for a numeric flag,
  // or spending the whole window uploading a payload nobody chose for --file.
  const argErrors = resolveArgErrors(args);
  if (argErrors.length > 0) {
    for (const message of argErrors) console.error(`FATAL: ${message}`);
    process.exit(1);
  }
  if (!config.QURL_API_KEY) { console.error('FATAL: QURL_API_KEY not set'); process.exit(1); }
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

      let line = `[${elapsed}s] Round ${round}: `;
      // Reported whenever the file leg ran at all, not just on success: a
      // round whose re-uploads all failed has fileLinks === 0, and the old
      // `fileLinks > 0` gate would have printed nothing for it.
      if (results.fileLinks > 0 || results.fileFail > 0) {
        // reup= counts attempts over time-spent-on-attempts: reuploadMs
        // accumulates outside the try/catch, so counting only successes would
        // put a numerator and denominator from different populations on one
        // field. reupFail= names the failed subset. Both segments drop out
        // when there is nothing to say — at --count <= TOKENS_PER_RESOURCE the
        // plan is a single batch, so a bare `reup=0/0ms` would be pure noise.
        const reupAttempts = results.reuploads + results.reuploadFail;
        line += `file(upload=${results.uploadMs.toFixed(0)}ms `
          + (reupAttempts > 0 ? `reup=${reupAttempts}/${results.reuploadMs.toFixed(0)}ms ` : '')
          + (results.reuploadFail > 0 ? `reupFail=${results.reuploadFail} ` : '')
          + `mint=${results.mintMs.toFixed(0)}ms ok=${results.fileLinks} fail=${results.fileFail}) `;
      }
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
      // Uploads per round is the headline number for whether this test
      // reproduces a real send's load: one initial upload plus one re-upload
      // per drained pool, i.e. ceil(COUNT / TOKENS_PER_RESOURCE) in total.
      //
      // Counts only rounds that got that far. A failed INITIAL upload throws
      // the round out before it reaches allResults, so unlike reupFail= those
      // failures are invisible here — they are already reported as `Round N
      // FAILED` above, and a round with no resource has nothing to tally.
      const reuploads = allResults.reduce((s, r) => s + r.reuploads, 0);
      const reuploadFail = allResults.reduce((s, r) => s + r.reuploadFail, 0);
      const reuploadMs = allResults.reduce((s, r) => s + r.reuploadMs, 0);
      console.log(`Uploads: ${allResults.length + reuploads} ok (${allResults.length} initial + ${reuploads} re-upload)`
        + (reuploadFail > 0 ? `, ${reuploadFail} re-upload failed` : ''));
      // Gated on attempts, not successes: a run whose every re-upload failed
      // still spent time on them, and a failure that sat on the connector's
      // timeout is exactly the latency worth seeing. Averaged over attempts
      // for the same reason the round line counts them.
      if (reuploads + reuploadFail > 0) {
        console.log(`Avg re-upload: ${(reuploadMs / (reuploads + reuploadFail)).toFixed(0)}ms`);
      }
    }
  }
}

// Only run the CLI entry point when invoked directly. Imported-from-test loads
// only the exported helpers — see tests/loadtest-target-guard.test.js.
if (require.main === module) {
  main().catch(e => { console.error('Fatal:', e); process.exit(1); });
}

module.exports = {
  // CLI argument validation
  readFlag,
  hasFlag,
  parsePositiveInt,
  resolveNumericArgs,
  resolveFileArg,
  checkUploadFile,
  resolveArgErrors,
  // Mint batching / token pool
  planMintBatches,
  TOKENS_PER_RESOURCE,
  // The round itself. Exported for tests/loadtest-round-accounting.test.js:
  // planMintBatches covers the batch *plan*, but the per-round accounting
  // wrapped around it — which counter a failed re-upload charges, and which
  // latency lands in which figure — is stateful, lives here, and is reachable
  // no other way. main() is behind `require.main === module` and scripts/ is
  // outside jest's collectCoverageFrom, so without this line nothing enforces
  // any of it. The test stubs ../src/connector; the call sites below stay
  // exactly as written, which is what keeps the AST assertions in
  // tests/loadtest-silent-failure.test.js meaningful.
  runRound,
  // Target safety guard
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
