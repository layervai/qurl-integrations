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
 *   --ledger PATH  Where to record created resources (default: <tmpdir>/loadtest-ledger-<ts>.jsonl)
 *   --reclaim PATH Revoke everything in a previous run's ledger, then exit
 *   --max-fail-rate PCT
 *                  Exit non-zero when the failure rate exceeds this percentage
 *                  (default: DEFAULT_MAX_FAIL_RATE_PCT below, currently 10).
 *                  Pass 100 to never fail on rate alone. Named rather than
 *                  only spelled out because a comment 200 lines from the
 *                  constant is a comment that goes stale silently; the run
 *                  also echoes the threshold it actually applied.
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
 *     - a flag that takes NO value (--location, --allow-production) is
 *       written on its own. `--location=true` is refused rather than
 *       interpreted — omitting the flag is already how it is turned off, so
 *       there is no `=false` left for it to mean. See readBooleanFlag below.
 *     - there are NO positional arguments. Every token has to be a flag this
 *       script declares or the value of one, so a misspelling (`--locatoin`),
 *       a single-dash spelling (`-count 5`), a case slip (`--LOCATION=true`)
 *       and a value handed to a boolean flag positionally (`--location false`)
 *       are all refused rather than ignored. See resolveUnknownArgs below.
 *     - `--` is NOT honoured. With no positionals for it to separate it has
 *       nothing to do here, so it is refused like any other stray token.
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
 *
 * Reclaim:
 *   Every resource this script creates is recorded to the ledger before it is
 *   used, and revoked before the script exits — on the normal path, on a
 *   thrown error, and on Ctrl-C, which stops the run rather than racing it.
 *   Minted links are therefore dead once a run ends; don't expect to open one
 *   after the summary prints.
 *
 *   If the process is killed hard enough to skip all of that (SIGKILL, a dead
 *   laptop), the ledger is the recovery path:
 *
 *     node scripts/loadtest-standalone.js --reclaim <tmpdir>/loadtest-ledger-<ts>.jsonl
 *
 *   --reclaim runs BEFORE the target guard: revoking resources that already
 *   exist is always the safe direction, and a refused target must not strand
 *   a ledger the operator is trying to clean up. Reclaim resolves its host
 *   from the ambient QURL_ENDPOINT, so each ledger line records the endpoint
 *   it was written against and a sweep refuses to run against a different one.
 *   Re-running is safe: revoked ids are pruned and an already-gone resource
 *   counts as reclaimed.
 *
 *   Two windows this cannot close, both leaking exactly the resource in hand:
 *   a create that succeeds server-side whose response is then lost, and a
 *   create whose id cannot be appended because the ledger became unwritable.
 *   Everything recorded before either is still swept.
 */

const fs = require('fs');
const os = require('os');
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
const { createOneTimeLink, deleteLink } = require('../src/qurl');

// The same pool depth the send pipeline batches against — imported, not
// copied, so a change to the cap reaches this script instead of silently
// leaving it issuing a different number of uploads than a real send.
const { TOKENS_PER_RESOURCE } = require('../src/constants');

const args = process.argv.slice(2);

// ---------------------------------------------------------------------------
// The flag table
//
// Every flag this script accepts, whether it takes a value, and what it falls
// back to. One table, read by everything below, so there is a single answer to
// "what does a valid command line look like" rather than one answer per reader.
//
// That single answer is what makes an unknown-argument pass possible at all.
// The readers below are PULL-based — each scans argv for one named flag — so
// none of them can see a token that matched nothing, which is why `--locatoin`,
// `-count 5` and `--LOCATION=true` all read as "flag absent" and ran the full
// window on the defaults. resolveUnknownArgs is the push-based counterpart,
// and it can only exist once something knows which tokens are flags and which
// of them consume the token after them.
//
// This is a single source and not a second list, which is the whole risk in
// introducing it. Both directions of drift are closed:
//
//   - a flag read without a row here throws out of flagSpec, at module load,
//     because the resolvers run there. It cannot ship as a flag the readers
//     honour and the unknown pass refuses.
//   - a row here that no reader consults would be accepted and then ignored —
//     the exact defect this table exists to remove, reintroduced by an entry.
//     The suite's 'wires every flag in the table to a reader' case is what
//     covers that direction, since no throw can.
//
// The numeric defaults are spelled as strings to look like the argv they stand
// in for; parsePositiveInt opens with String(raw), so a numeric literal would
// behave identically. `defaultLabel` is what a missing-value message calls the
// default — see readFlag. Only --file needs one, because its default is not a
// path at all and would otherwise print as the literal "null".
//
// Frozen, rows included, because it is exported. "Single source of truth" is
// the claim this whole section rests on, and an export hands every importer a
// live reference to mutate it through — a suite that edited a row would leak
// that edit into every later test in its file, since jest shares one module
// instance across them. Freezing makes the invariant hold at runtime instead
// of by convention.
const FLAGS = [
  { name: 'count', takesValue: true, defaultValue: '100' },
  { name: 'duration', takesValue: true, defaultValue: '7200' },
  { name: 'interval', takesValue: true, defaultValue: '60' },
  { name: 'file', takesValue: true, defaultValue: null, defaultLabel: 'an auto-generated 1MB test file' },
  { name: 'max-fail-rate', takesValue: true, defaultValue: '10' },
  { name: 'ledger', takesValue: true, defaultValue: null, defaultLabel: 'a generated path under the temp directory' },
  // --reclaim has no default: it is a mode switch, and the absence of a value
  // is refused rather than filled in. The label is what the missing-value
  // message says instead of naming one.
  { name: 'reclaim', takesValue: true, defaultValue: null, defaultLabel: 'no default — a ledger path is required' },
  { name: 'location', takesValue: false },
  { name: 'allow-production', takesValue: false },
].map((spec) => Object.freeze(spec));
Object.freeze(FLAGS);

const FLAG_BY_NAME = new Map(FLAGS.map((spec) => [spec.name, spec]));

// Rendered once, for the messages that have no better hint to offer than the
// list of flags that do exist.
const ACCEPTED_FLAGS = FLAGS.map((spec) => `--${spec.name}`).join(', ');

/**
 * The declared spec for a flag, asserting the arity its reader assumes.
 *
 * Throws rather than collecting, unlike everything else in this section: both
 * failures are WIRING bugs and not operator mistakes, so there is no command
 * line to report them against. They fire at module load — the resolvers run
 * there — so neither can reach a run.
 */
function flagSpec(name, takesValue) {
  const spec = FLAG_BY_NAME.get(name);
  if (spec === undefined) throw new Error(`--${name} is read but not declared in FLAGS`);
  if (spec.takesValue !== takesValue) {
    throw new Error(`--${name} is declared ${spec.takesValue ? 'value-taking' : 'valueless'} in FLAGS but read as the opposite`);
  }
  return spec;
}

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

// The UNKNOWN FLAGS gap this note used to record is closed, by
// resolveUnknownArgs below. Nothing changed here to close it: readFlag is
// still pull-based, still scans argv for one named flag, and still cannot see
// a token that matched nothing. A second, push-based pass over the whole
// command line is what can, which is why that is where it lives.

// Boolean flags are not routed through readFlag: they take no value, so it has
// nothing to read for them. They shared the defect anyway. The `hasFlag` this
// replaces tested argv for the bare token with `includes`, so `--location=true`
// was not recognized as the flag at all and read as ABSENT — the location leg
// off while the command line said to turn it on, the same "did something real,
// just not what was asked for" as the value-taking flags above.
// `--allow-production=1` failed identically; that one fails closed, since the
// guard then refuses the target, but it is no less silent about why.
//
// The `=` spelling is REFUSED rather than interpreted. What `--location=false`
// ought to mean was the open question that kept this out of the change that
// introduced readFlag, and refusing is the answer because it is the only one
// with no silent tail:
//
//   - Interpreting a vocabulary — true/false/1/0 — has to stop somewhere, and
//     every spelling past that edge falls back into the hole being closed.
//     `--location=yes`, or the mistyped `--location=flase`, would read as OFF
//     again — the original fault, now with a value sitting in the operator's
//     shell history to suggest it was honoured.
//   - Refusing has no edge: every `=` spelling of THIS flag name is refused
//     identically, and the message names the form that works. It also cannot
//     turn a leg ON that was meant to be off, nor off that was meant to be on,
//     because the run does not start at all.
//   - There is nothing for `--location=false` to express that omitting the
//     flag does not already express. These are leg switches, not settings.
//
// A POSITIONAL value is deliberately still not this reader's to refuse, and
// `--location false` still returns ON from here. That is not the gap it used
// to be: `false` is a token nothing consumed, which resolveUnknownArgs refuses
// under its one rule, so the run stops. Adding a fifth special case here would
// only mean two readers reporting the same argument twice — and this one
// cannot see enough of the command line to be the right place for it anyway.
function readBooleanFlag(argv, flag) {
  const token = `--${flag}`;
  const inlinePrefix = `${token}=`;
  // ANY occurrence refuses, not merely the last one. A value-taking flag can
  // fall back on "last wins" when it is repeated; there is no such rule to
  // reach for here, so `--location --location=false` has no reading that is
  // not a guess. The FIRST such occurrence is the one reported — which of them
  // is echoed does not change the verdict, and picking one keeps the message
  // deterministic.
  const offending = argv.find((arg) => arg.startsWith(inlinePrefix));
  if (offending !== undefined) {
    // Echo the value, as parsePositiveInt and parseTargetAllowlist do. Without
    // it, a wrapper emitting `--location=$WITH_LOCATION` with the variable
    // unset produces `--location=`, whose message would otherwise be
    // byte-identical to `--location=false` — hiding the actual fault, which is
    // the unset variable and not the value the operator chose.
    const value = offending.slice(inlinePrefix.length);
    return { error: `${token} takes no value, got ${JSON.stringify(value)} — pass ${token} on its own to turn it on, or omit it to leave it off` };
  }
  return { value: argv.includes(token) };
}

// Resolve the boolean flags from an argv array. Pure and argv-taking,
// following resolveGuardInputs and the resolvers below, so the suite covers
// the wiring and not merely the reader — the constant it feeds is read by
// runRound, which no test can reach.
//
// Collected rather than thrown for the same reason as the others: this runs at
// module load, which the suite reaches through require(), so the exit belongs
// in main() alongside every other fatal.
function resolveBooleanArgs(argv) {
  const errors = [];
  const read = (flag) => {
    // Consulted for the assertion alone — a boolean flag has no default and
    // nothing else to look up. It is what stops a flag from being honoured
    // here while the table declares it value-taking, which would have
    // resolveUnknownArgs swallow the token after it as a value.
    flagSpec(flag, false);
    const { value, error } = readBooleanFlag(argv, flag);
    if (error) errors.push(error);
    // A refused flag reads as OFF, never on. main() exits on `errors` before
    // any of this is used, so which value it is should not matter — but the
    // one a reader finds here should still be the fail-closed one.
    return value === true;
  };
  const includeLocation = read('location');
  // --allow-production's VALUE belongs to resolveGuardInputs, which owns every
  // input the guard reads. Only its SHAPE is checked here, for two reasons.
  //
  // The weaker one: it puts a malformed boolean in the same single pass as a
  // malformed --file. main() gates on argErrors, THEN on QURL_API_KEY, and
  // only then reaches the guard — so a bad --file plus a bad
  // --allow-production would otherwise cost an operator without an API key
  // exported three runs to see three messages.
  //
  // The decisive one: resolveGuardInputs's `errors` is not a general bucket.
  // It arrives entirely by spread from parseTargetAllowlist, is destructured
  // as `allowlistErrors`, and main() documents it as meaning specifically
  // "the operator believes they granted a host they did not". A flag-shape
  // error riding in that array would make that comment false and give the
  // array two meanings.
  //
  // Called for its error side effect only; the boolean it returns is the
  // guard's to read, not ours.
  read('allow-production');
  return { includeLocation, errors };
}

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
// The defaults come from FLAGS above and are ROUTED THROUGH parsePositiveInt
// rather than returned beside it, so a default that the validator would reject
// cannot ship, and there is no "was this token typed or defaulted?" branch
// between readFlag and the parse. A fourth numeric flag read here without a
// row in that table throws out of flagSpec at module load, rather than being
// honoured here and refused as unknown two functions down.
function resolveNumericArgs(argv) {
  const errors = [];
  const read = (flag) => {
    const { defaultValue } = flagSpec(flag, true);
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
  return { count: read('count'), durationS: read('duration'), intervalS: read('interval'), errors };
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
  const { defaultValue, defaultLabel } = flagSpec('file', true);
  const { value, error } = readFlag(argv, 'file', defaultValue, defaultLabel);
  if (error) {
    errors.push(error);
    return { filePath: null, errors };
  }
  // Compared against the table's default rather than a repeated `null`: that
  // table owns what "no --file given" resolves to, and this is the one reading
  // of this flag that may fall back to the generated payload.
  //
  // Sound only because that default is a SENTINEL no operator can type. It is
  // readFlag's "flag absent" return, not a value, so `null` here cannot have
  // come from argv. A future flag given a real string default must not copy
  // this line — an operator passing that exact string would be misread as
  // having passed nothing, which is the silent-default bug this file exists to
  // remove, arriving from the one direction the table made easier.
  if (value === defaultValue) return { filePath: null, errors };
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

// --ledger, read through the same reader for the same reasons. It was the last
// flag still on the removed indexOf-based getArg, which means `--ledger=/x`
// read as absent and the run recorded to a generated path instead — and the
// ledger is the one file an operator has to be able to find afterwards, so
// silently writing it somewhere else is the worst version of that fault.
//
// `fallback` is passed rather than closed over so the function stays pure and
// resolveArgErrors can call it for its errors alone.
function resolveLedgerArg(argv, fallback = null) {
  const errors = [];
  const { defaultValue, defaultLabel } = flagSpec('ledger', true);
  const { value, error } = readFlag(argv, 'ledger', defaultValue, defaultLabel);
  if (error) {
    errors.push(error);
    return { ledgerPath: fallback, errors };
  }
  if (value === null) return { ledgerPath: fallback, errors };
  // Whitespace-only is a mistyped flag, not a path — same reasoning as --file,
  // and here it would put the reclaim ledger somewhere unfindable.
  if (value.trim() === '') {
    errors.push(`--ledger must name a file to record created resources in, got ${JSON.stringify(value)}`);
    return { ledgerPath: fallback, errors };
  }
  return { ledgerPath: value, errors };
}

/**
 * --reclaim's SHAPE, reported into the single preflight pass.
 *
 * The mode decision itself stays in parseReclaimArg and is read by main(); what
 * belongs here is only the refusal, because a flag declared in FLAGS whose
 * reader reports nothing is accepted by resolveUnknownArgs and then ignored —
 * the exact defect the table exists to remove. For --reclaim that would be the
 * worst version of it: the flag silently doing nothing means a full load test
 * runs where the operator asked to delete.
 */
function resolveReclaimArg(argv) {
  const errors = [];
  const { requested, path: reclaimPath } = parseReclaimArg(argv);
  if (requested && !reclaimPath) {
    // Deliberately not opening with the flag name, unlike the messages nearby:
    // those are template literals, while a plain string starting with `--` is
    // swept up by the boolean-flag literal guard in
    // tests/loadtest-silent-failure.test.js as though it were an ad-hoc flag
    // read. The name still appears, which is what the wiring check needs.
    errors.push('a ledger path is required — pass it as --reclaim /tmp/loadtest-ledger-<ts>.jsonl');
  }
  return { requested, path: reclaimPath, errors };
}

/**
 * The flag a token names, or null for one that names none. Both spellings
 * resolve to the same spec, so `--file` and `--file=/tmp/x` are one flag
 * rather than a flag and an unknown token — matching readFlag, which accepts
 * both. Splitting on the first `=` only, so `--file=/tmp/run=3.bin` still
 * names --file.
 */
function tokenSpec(token) {
  if (!token.startsWith('--')) return null;
  return FLAG_BY_NAME.get(token.slice(2).split('=', 1)[0]) ?? null;
}

/**
 * Whether readFlag would consume this token as the previous flag's value.
 *
 * Deliberately readFlag's rule and not an approximation of it: an undefined
 * token is a flag left as the final one, and a token beginning with `--` is
 * refused there as a flag-shaped value rather than taken as one. If the two
 * ever disagree, a token gets either consumed here AND reported unknown, or
 * claimed by neither.
 */
const isValueToken = (token) => token !== undefined && !token.startsWith('--');

/**
 * The flag an unrecognized token was probably meant to be, or null.
 *
 * One normalization — drop leading dashes, drop any `=value`, lowercase —
 * covers three separate slips: `-count`, the single-dash spelling that Go's
 * `flag` package accepts and that this repo's Go apps (apps/cli, apps/slack)
 * make a live habit; `--LOCATION`, a shift-key slip past a match that is
 * case-sensitive on purpose; and `---count`. A genuine misspelling like
 * `--locatoin` matches nothing and gets the flag list instead, which is what
 * an edit-distance search would be for — more machinery than these three
 * shapes need.
 */
function suggestFlag(token) {
  const bare = token.replace(/^-+/, '').split('=', 1)[0].toLowerCase();
  if (bare === '') return null;
  // Lowercasing the declared name too cannot change the answer today — every
  // flag above is already lowercase — so this is a forward guard rather than a
  // live one, and no test can distinguish it. It is what would keep the case
  // slip working for a `--dryRun`-style name, which is the whole point of the
  // suggestion.
  const spec = FLAGS.find((s) => s.name.toLowerCase() === bare);
  return spec === undefined ? null : `--${spec.name}`;
}

/**
 * Refuse every token the flag table does not account for.
 *
 * The push-based counterpart to the pull-based readers above, and the last
 * member of the fault class they close. Each of those scans argv for the one
 * flag it was asked about, so a token that matched NOTHING was seen by none of
 * them and ignored whatever it said: `--locatoin` and `-count 5` read as
 * "flag absent" and ran the full unattended window on the defaults, and
 * `--location false` turned the leg ON while the operator was asking for it
 * off. One-character slips, all silent, all producing a real run that nobody
 * asked for.
 *
 * One rule closes them: this script accepts NO positional arguments, so every
 * token is either a flag it declares, or the value immediately following a
 * declared value-taking flag. Everything else is an error.
 *
 * `--` is not honoured, decided rather than overlooked. It separates flags
 * from positionals, and there are no positionals here for it to protect — so
 * honouring it would mean either every token after it is refused anyway, or
 * readFlag and readBooleanFlag learning about it too, since both scan the
 * whole argv and would otherwise keep reading flags this pass had written off.
 * That is a change to every reader in aid of a separator with nothing to
 * separate. Refusing it says the one thing an operator who typed it has wrong.
 *
 * Pure and argv-taking like the resolvers above, and collecting rather than
 * throwing, so main() reports these in the same single pass as everything else.
 */
function resolveUnknownArgs(argv) {
  const errors = [];
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    const spec = tokenSpec(arg);
    if (spec !== null) {
      // Skip what this flag consumes, on exactly readFlag's terms: the inline
      // spelling carries its own value, and the separated one reaches past a
      // token only when readFlag would have taken it.
      if (spec.takesValue && !arg.startsWith(`--${spec.name}=`) && isValueToken(argv[i + 1])) i += 1;
      continue;
    }
    errors.push(unknownArgError(arg, argv[i - 1]));
    // An unrecognized FLAG swallows a following value token, so `--cont 5`
    // costs one message and not two. Its arity is unknowable — that is what
    // makes it unrecognized — but reporting `5` as a stray positional would
    // name something the operator never meant as one, and send them looking
    // for a second mistake they did not make. Same reasoning as
    // resolveArgErrors below declining to stat a --file whose shape already
    // failed. Nothing is hidden by it: the run stops on the flag either way.
    //
    // Two tokens are excluded, both because their arity is NOT in fact
    // unknowable, so there is no guess to justify dropping a real stray:
    //
    //   - `--`, which is a separator and never carries a value;
    //   - an inline spelling, `--cont=5`, which has already been given its
    //     value and so cannot take a separated one as well. Same reasoning as
    //     the recognized branch above, and the same rule, so a misspelling
    //     hides no more than the flag it was meant to be would have.
    if (arg.startsWith('-') && arg !== '--' && !arg.includes('=') && isValueToken(argv[i + 1])) i += 1;
  }
  return { errors };
}

/**
 * The message for one token nothing consumed. Split out so the loop above
 * stays a statement of the rule and this stays a statement of the diagnosis.
 */
function unknownArgError(token, previous) {
  if (token === '--') {
    return '-- has nothing to separate here — this script takes no positional arguments, so every argument is a flag';
  }
  if (token.startsWith('-')) {
    const suggestion = suggestFlag(token);
    if (suggestion !== null) {
      return `${token} is not a flag this script accepts — did you mean ${suggestion}? (flag names are case-sensitive, and take two dashes)`;
    }
    return `${token} is not a flag this script accepts — accepted flags are ${ACCEPTED_FLAGS}`;
  }
  // A stray token straight after a valueless flag is almost never a stray
  // token: it is the `--flag value` habit meeting a flag that takes none, and
  // `--location false` is the one reading in this whole class where the
  // operator got the OPPOSITE of what they typed. Naming the flag turns a
  // generic "no positionals here" into the actual correction. Echoes the value
  // and offers the same recovery as readBooleanFlag's `=` refusal, because
  // from the operator's seat the two are one mistake spelled two ways.
  // Matched against the BARE token, not just the flag it names. After
  // `--location=x` the inline value is already refused by its own reader, and
  // saying "after --location" there names a token the operator did not type
  // while repeating a message they have already been given. The habit this
  // clause diagnoses is `--flag value`, which is the bare spelling by
  // definition; anything else falls through to the plain positional message.
  const before = previous === undefined ? null : tokenSpec(previous);
  if (before !== null && !before.takesValue && previous === `--${before.name}`) {
    return `unexpected argument ${JSON.stringify(token)} after --${before.name} — --${before.name} takes no value; pass it on its own to turn it on, or omit it to leave it off`;
  }
  return `unexpected argument ${JSON.stringify(token)} — this script takes no positional arguments; accepted flags are ${ACCEPTED_FLAGS}`;
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
  const { errors: ledgerErrors } = resolveLedgerArg(argv);
  const { errors: booleanErrors } = resolveBooleanArgs(argv);
  const { errors: unknownErrors } = resolveUnknownArgs(argv);
  // Unknown arguments report LAST among the argv faults. The resolvers above
  // each diagnose a flag the operator did get right the name of, which is the
  // more specific answer; this one is the catch-all for what none of them
  // claimed. The readability check stays after all of them, being the only one
  // that looks past argv.
  const { errors: reclaimErrors } = resolveReclaimArg(argv);
  const errors = [
    ...numericErrors, ...fileErrors, ...ledgerErrors, ...reclaimErrors,
    ...booleanErrors, ...unknownErrors,
  ];
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
const { includeLocation: INCLUDE_LOCATION } = resolveBooleanArgs(args);
const TEST_LOCATION_URL = 'https://www.google.com/maps/place/?q=place_id:ChIJLU7jZClu5kcRbUm7GCkGkNQ'; // Eiffel Tower

// Default under os.tmpdir() and created 0600, following the rationale
// gateway-resume-spike.js records in this same directory: a predictable path
// in a world-writable /tmp can be pre-created as a symlink, and appendFileSync
// follows symlinks. The contents are only resource ids, but the write
// primitive is real.
const DEFAULT_LEDGER_PATH = path.join(os.tmpdir(), `loadtest-ledger-${Date.now()}.jsonl`);
const { ledgerPath: LEDGER_PATH } = resolveLedgerArg(args, DEFAULT_LEDGER_PATH);

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
  // The same charset guard deleteLink applies bot-side (validateResourceId,
  // src/qurl.js), not a stricter one. A revision of this check also required
  // qurl-service's `r_` prefix, on the reasoning that an id passing charset
  // but failing the SDK's semantic check would be swept as a failure forever.
  // That reasoning is sound but the check was wrong here: `validateResourceId`
  // is charset-only *by design*, deferring the prefix to the SDK, and `res-1`
  // is this repo's fixture convention for a resource id across eight test
  // files. Being stricter than the codebase's own guard rejected ids the rest
  // of it treats as valid, and warned on ordinary fixtures.
  //
  // The residual risk is accepted: a charset-clean non-`r_` id can only come
  // from the server, which does not produce one, and if it ever did the
  // repeated failure is visible in the sweep's cause tally.
  if (!resourceId || typeof resourceId !== 'string' || !/^[\w-]+$/.test(resourceId)) {
    // Loud, but deliberately NOT fatal to the run.
    //
    // An earlier revision set `stopping` here, on the reasoning that whatever
    // produced one unusable id produces another every round. The reasoning
    // holds; the mechanism does not. It couples a diagnostic to destructive
    // control flow — one malformed id silently truncates the round mid-batch —
    // and `res-1` is this repo's fixture convention for a resource id across
    // eight test files, so any caller or suite that stubs an upload trips it.
    // Merging #1173's re-upload leg surfaced exactly that: nine accounting
    // tests failed because the round stopped after its first batch.
    //
    // In production the id is always `r_`-shaped, so this branch is
    // effectively unreachable and stopping bought nothing real. The warning is
    // what carries the diagnostic.
    console.error(`WARNING: ${kind} response carried no usable resource_id — that resource cannot be reclaimed.`);
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
    // Only a trailing slash is normalized, so scheme and host casing have to
    // match exactly — worth saying, because the alternative to knowing it is
    // an operator guessing mid-incident.
    console.error('The match is exact apart from a trailing slash: scheme and host casing must agree.');
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
      //
      // Liveness rests on creates eventually settling. On the end-of-run path
      // every round has already awaited them, so the counter is zero. On the
      // signal path a genuinely stuck create would spin here, bounded only by
      // the second-SIGINT abort — so whoever adds the timeout the upload fetch
      // currently lacks should keep that connection in mind rather than
      // treating the two as unrelated.
      if (inFlightCreates === 0) break;
      await new Promise(r => setTimeout(r, 100));
      continue;
    }
    // A floor built from the 50ms pacing gap alone; per-request latency adds
    // to it and can dominate on a large sweep. Labelled as a minimum rather
    // than an estimate, because an operator watching a number they read as an
    // ETA sail past it is the "looks wedged, reach for kill -9" scenario this
    // heartbeat exists to prevent.
    const seconds = Math.max(1, Math.round(pending.length * 0.05));
    console.log(`Reclaim: revoking ${pending.length} resource(s) from ${ledgerPath} (at least ${seconds}s, likely longer)...`);
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
          // Keyed on the cause, not the raw message. callQurl embeds the
          // request path — and therefore the resource id — in every message,
          // so keying on it verbatim gives one bucket per failing id: a
          // uniform 401 across 5,000 ids would print 5,000 lines of "1x"
          // instead of "5000x", defeating the tally and flooding the very
          // scrollback the heartbeat is trying to keep readable.
          const cause = e.message.replace(/\/qurls\/\S+/, '/qurls/<id>');
          causes.set(cause, (causes.get(cause) || 0) + 1);
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
  if (!reclaimInFlight) {
    // Cleared on rejection. Without this, a sweep that threw would be
    // memoized as a rejected promise, and main().catch's "reclaim on fatal
    // error" fallback would re-await that same rejection — reading as a
    // retry while being structurally incapable of retrying.
    reclaimInFlight = reclaim(ledgerPath).catch((e) => {
      reclaimInFlight = null;
      throw e;
    });
  }
  return reclaimInFlight;
}

// Test-only seam. The memo is process-global on purpose — one run, one sweep —
// which also means a suite cannot exercise a second, independent sweep without
// clearing it. No production path calls this; it exists so the memoization and
// its rejection-clearing can be pinned rather than resting on a transcript.
function resetReclaimStateForTests() {
  reclaimInFlight = null;
  stopping = false;
  inFlightCreates = 0;
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
  // Unlike the end-of-run path, this exits immediately rather than setting
  // process.exitCode: a round may still be parked in the inter-round sleep,
  // which would keep the process alive for up to --interval seconds after the
  // operator asked it to stop. The tradeoff is that piped output can lose the
  // final tally line, which is why that line is not the only place the
  // outstanding count and the --reclaim command appear.
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
// Reads through readFlag like every other value-taking flag, but keeps its own
// return shape: --reclaim is a MODE switch, so "not given" and "given without
// a usable value" have to stay distinguishable. readFlag collapses them (both
// yield no value), and acting on that collapse is precisely the failure this
// guard exists to prevent — a bare --reclaim falling through to a full load
// test, minting thousands of resources when the operator asked to delete some.
// So presence is detected here and the value is delegated.
function parseReclaimArg(argv) {
  // Flag name written WITHOUT the dashes, the shape the boolean-literal guard
  // in tests/loadtest-silent-failure.test.js sanctions: a bare '--reclaim'
  // literal here is indistinguishable from the ad-hoc flag reads that guard
  // exists to catch, even though this one is a presence check for a mode
  // switch rather than a value read.
  const FLAG = 'reclaim';
  const requested = argv.some(a => a === `--${FLAG}` || a.startsWith(`--${FLAG}=`));
  if (!requested) return { requested: false, path: null };
  const { defaultValue, defaultLabel } = flagSpec(FLAG, true);
  const { value, error } = readFlag(argv, FLAG, defaultValue, defaultLabel);
  // An error is a missing or flag-shaped value; an empty inline value
  // (`--reclaim=`) arrives as '' and is equally unusable. Both are "asked to
  // reclaim, told us nothing to reclaim", which main() refuses loudly.
  if (error || !value) return { requested: true, path: null };
  return { requested: true, path: value };
}

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
    // Through the shared boolean reader like --location, rather than an
    // inline includes: the two boolean flags reading argv two different ways
    // was the residual of a split #1174 removed, and this is the one that
    // gates a production-target override.
    //
    // `.value === true` is what carries that forward now that the reader can
    // refuse. On the refused path it returns `{ error }` with no `value`, so
    // `--allow-production=1` reads as ABSENT here — fail-closed, the guard
    // refusing the target rather than clearing it. The operator still gets a
    // message, because resolveBooleanArgs checks the same flag's SHAPE and
    // main() exits on that before ever reaching the guard. The error this
    // call would have produced is dropped on purpose: reporting it twice is
    // worse than reporting it once, and this is the value read, not the
    // report.
    //
    // Note this scans the whole process.argv while resolveBooleanArgs scans
    // args (process.argv.slice(2)). They cannot disagree about this token:
    // the two extra entries are the node binary and the RESOLVED ABSOLUTE
    // script path, and neither an equality against `--allow-production` nor a
    // `--allow-production=` prefix can match an absolute path.
    allowProdFlag: readBooleanFlag(argv, 'allow-production').value === true,
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
    // reup= counts ATTEMPTS over time-spent-on-attempts: reuploadMs
    // accumulates outside the try/catch, so counting only successes would put
    // a numerator and a denominator from different populations on one field.
    // reupFail= names the failed subset. Both segments drop out when there is
    // nothing to say — at --count <= TOKENS_PER_RESOURCE the plan is a single
    // batch, so a bare `reup=0/0ms` would be pure noise.
    //
    // Read off `results` with `|| 0` rather than destructured, because this
    // is called with hand-built round objects in the suite and a round from
    // before the re-upload leg existed has no such counters. Absent is zero
    // attempts, which is exactly the "drop the segment" case.
    const reuploads = results.reuploads || 0;
    const reuploadFail = results.reuploadFail || 0;
    const reupAttempts = reuploads + reuploadFail;
    line += `file(upload=${results.uploadMs.toFixed(0)}ms `
      + (reupAttempts > 0 ? `reup=${reupAttempts}/${(results.reuploadMs || 0).toFixed(0)}ms ` : '')
      + (reuploadFail > 0 ? `reupFail=${reuploadFail} ` : '')
      + `mint=${results.mintMs.toFixed(0)}ms ok=${results.fileLinks} fail=${results.fileFail}) `;
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
//
// Read from FLAGS rather than declared here, so this flag's default lives
// where every other flag's does. It is exported and compared as a NUMBER,
// while the table holds the string readFlag would have handed back — hence
// the conversion. The rationale above is the reason for the value; the table
// is where the value is.
const DEFAULT_MAX_FAIL_RATE_PCT = Number(flagSpec('max-fail-rate', true).defaultValue);

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
 * The threshold exactly as it was set. One decimal like the verdict lines, but
 * widened whenever that would round the value away — the echo exists to
 * confirm what was applied, so `--max-fail-rate 0.05` reading back as `0.1%`
 * would defeat the only thing it is there for.
 *
 * Widened against the value's own canonical rendering rather than against
 * `pct` directly, because the percentage does not survive the round trip
 * through a fraction: `0.23 / 100 * 100` is 0.22999999999999998, not 0.23, for
 * about a tenth of the two-decimal thresholds in range. Comparing to `pct`
 * exactly finds no width that matches and falls through to the bound, so
 * `--max-fail-rate 0.23` echoed as `0.230000%` — accurate, but not the value
 * as it was typed, which is the one thing this line owes the operator. Twelve
 * significant digits is far finer than the six-decimal display bound and far
 * coarser than where the noise sits.
 *
 * Bounded at six decimals of a percent. Past that a value is shown at six
 * decimals, except where that would render a real threshold as `0.000000%` —
 * no threshold at all to read — which is reported as below the bound instead.
 */
function formatThresholdPct(rate) {
  const pct = rate * 100;
  const canonical = Number(pct.toPrecision(12));
  for (let digits = 1; digits <= 6; digits++) {
    const text = pct.toFixed(digits);
    if (Number(text) === canonical) return `${text}%`;
  }
  const text = pct.toFixed(6);
  return Number(text) === 0 && pct > 0 ? '<0.000001%' : `${text}%`;
}

/**
 * Render a rate and the threshold it exceeded so the two never print the same
 * string. A rate that only just crosses the line — 1001/10000 against 10% —
 * rounds to the threshold's own spelling, and `10.0% exceeds 10.0%` reads as a
 * bug in the tool rather than a finding about the run. Widen both together
 * until they differ.
 *
 * Bounded at four decimals, which a run large enough can genuinely reach: one
 * failure in ten million is a gap of 0.00001%, below what four decimals
 * resolve, so the two can still print alike. Only the wording is affected —
 * the comparison is on the raw fractions and the counts are printed beside it,
 * so the verdict and the exit code stay right either way.
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
  // "link failures", not "failures": this counts qURLs, and a round that dies
  // in the upload contributes nothing to it. An unqualified label put `Total
  // failures: 0` directly above a FAILED verdict on a run where every round
  // died — a reader scanning for the failure count would find a zero and stop.
  // The Rounds line above carries the other class.
  lines.push(`Total link failures: ${failedLinks}`);
  // Echoed on every run, passing or failing. This is the value that decided
  // the exit code, and printing it only on the FAILED lines meant a run that
  // silently took the default had nothing in its log to say so.
  lines.push(`Failure threshold: ${formatThresholdPct(maxFailRate)} (--max-fail-rate)`);

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
      // attempt failed, or nothing was ever attempted. Reporting the second as
      // "all 0 mint attempt(s) failed" states a failure that did not happen.
      //
      // The second is defensive rather than live since #1171 was merged in:
      // `--count 0` was how a run reached it, and parsePositiveInt now refuses
      // zero at preflight. See the reachability note on 'no qURL was
      // attempted' below.
      const noMintNote = fileFail > 0
        ? `n/a — all ${fileFail} mint attempt(s) failed`
        : 'n/a — no mint was attempted';
      lines.push(mintedRounds.length > 0
        ? `Avg upload: ${avgUpload}ms, avg mint/round: ${mean(mintedRounds, (r) => r.mintMs).toFixed(0)}ms`
        : `Avg upload: ${avgUpload}ms, avg mint/round: ${noMintNote}`);

      // Uploads per round is the headline number for whether this test
      // reproduces a real send's load: one initial upload plus one re-upload
      // per drained pool, i.e. ceil(COUNT / TOKENS_PER_RESOURCE) in total.
      //
      // Counts only rounds that got that far. A failed INITIAL upload throws
      // the round out before it reaches allResults, so unlike reupFail= those
      // failures are invisible here — they are already reported as `Round N
      // FAILED` above, and a round with no resource has nothing to tally.
      const reuploads = fileRounds.reduce((t, r) => t + (r.reuploads || 0), 0);
      const reuploadFail = fileRounds.reduce((t, r) => t + (r.reuploadFail || 0), 0);
      const reuploadMs = fileRounds.reduce((t, r) => t + (r.reuploadMs || 0), 0);
      lines.push(`Uploads: ${fileRounds.length + reuploads} ok (${fileRounds.length} initial + ${reuploads} re-upload)`
        + (reuploadFail > 0 ? `, ${reuploadFail} re-upload failed` : ''));
      // Gated on attempts, not successes: a run whose every re-upload failed
      // still spent time on them, and a failure that sat on the connector's
      // timeout is exactly the latency worth seeing. Averaged over attempts
      // for the same reason the round line counts them.
      if (reuploads + reuploadFail > 0) {
        lines.push(`Avg re-upload: ${(reuploadMs / (reuploads + reuploadFail)).toFixed(0)}ms`);
      }
    }
  }

  const linkFailRate = attemptedLinks > 0 ? failedLinks / attemptedLinks : 0;
  const roundFailRate = roundsAttempted > 0 ? roundsFailed / roundsAttempted : 0;
  const reasons = [];
  // A run that never completed a round measured nothing; reporting that as a
  // pass is the failure this exit code exists to prevent, and no rate above
  // catches it when roundsAttempted is itself 0.
  if (roundsCompleted === 0) reasons.push('no round completed');
  // Rounds ran but no qURL was ever attempted. Both rates are 0/0 and read as
  // a clean run, so this is the same unmeasured-run pass one level down from
  // 'no round completed'.
  //
  // DEFENSIVE, not live, since #1171 was merged into this branch. `--count 0`
  // was the way in and parsePositiveInt now refuses zero at preflight, so
  // COUNT is at least 1 by the time a round runs — and both legs loop from 0
  // to COUNT (`i += 10` and `i++` alike), so every completed round attempts at
  // least one qURL. A round that attempts none threw, which leaves it out of
  // allResults and counted by roundFailRate instead.
  //
  // Kept because the alternative is reporting an unmeasured run as a pass,
  // which is the fault this exit code exists to prevent, and because a future
  // leg that legitimately attempts nothing would land here. runReport is pure
  // and called directly by the suite, so the branch stays covered even though
  // no command line reaches it.
  if (roundsCompleted > 0 && attemptedLinks === 0) reasons.push('no qURL was attempted');
  if (linkFailRate > maxFailRate) {
    const [rate, limit] = formatRatePair(linkFailRate, maxFailRate);
    reasons.push(`link failure rate ${rate} (${failedLinks}/${attemptedLinks}) exceeds --max-fail-rate ${limit}`);
  }
  // Skipped when nothing completed: that case is already named above, and a
  // zero-completed run always has a 100% round rate, so both would fire and
  // report one condition as two findings.
  if (roundsCompleted > 0 && roundFailRate > maxFailRate) {
    const [rate, limit] = formatRatePair(roundFailRate, maxFailRate);
    reasons.push(`round failure rate ${rate} (${roundsFailed}/${roundsAttempted}) exceeds --max-fail-rate ${limit}`);
  }
  for (const reason of reasons) lines.push(`FAILED: ${reason}`);
  return { lines, failed: reasons.length > 0, linkFailRate, roundFailRate };
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

  // Keyed by message and flushed once per leg below, so a round that mixes a
  // systemic failure with transient ones reports both. Scoped per round: the
  // tally answers "what went wrong in THIS round", and a run-long map would
  // reprint every earlier round's messages at every flush.
  const fileErrors = new Map();
  const reuploadErrors = new Map();
  const locErrors = new Map();

  // File pipeline
  if (!stopping && (FILE_PATH || !INCLUDE_LOCATION)) {
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
    // Wrapped in trackCreate so the reclaim drain waits on it: the upload is
    // a create like any other, and one still in flight when a sweep starts
    // would otherwise record its parent after the final pass had run.
    const uploadResult = await trackCreate(async () => {
      const parsed = await reUploadBuffer(
        fileBuffer,
        uploadName,
        'application/octet-stream',
      );
      // Recorded before anything is minted against it. Reclaiming this parent
      // is what reclaims the recipient links, so it has to be on disk first.
      recordResource(parsed.resource_id, 'upload');
      return parsed;
    });
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
    // Two tallies, not one flag apiece. #1173 kept the mint's "log the first
    // error only" behind its own boolean precisely because a failed re-upload
    // charges fileFail too, and keying the mint log off that count would
    // swallow the first mint error on exactly the round most in need of one.
    // Tallying by MESSAGE removes the need for either flag and answers the
    // question behind both: a round mixing a systemic fault with transient
    // 429s reports each, weighted by the attempts it took down.
    //
    // The two stay separate because they fail for different reasons and are
    // read differently — a drained-pool re-upload fault and a mint rejection
    // are not one population, and merging them would put a connector timeout
    // and a quota error under one heading.
    for (const batch of planMintBatches(COUNT)) {
      // A sweep has started: stop before issuing another create it would have
      // to chase, same as the location leg and the round loop.
      if (stopping) break;
      if (batch.reupload) {
        const reStart = performance.now();
        let re = null;
        try {
          // Tracked and recorded exactly like the round's first upload. A
          // re-upload mints a NEW parent resource, and the old one's tokens
          // being spent does not make it go away — an unrecorded re-upload
          // leaks a full resource per batch, which is the failure this ledger
          // exists to prevent and the easiest one to miss when the leg was
          // added for an unrelated reason.
          re = await trackCreate(async () => {
            const parsed = await reUploadBuffer(fileBuffer, uploadName, 'application/octet-stream');
            recordResource(parsed.resource_id, 'upload');
            return parsed;
          });
        } catch (e) {
          tallyFailure(reuploadErrors, e.message, 1);
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
        tallyFailure(fileErrors, e.message, batch.size);
        results.fileFail += batch.size;
      }
      // Accumulated per batch rather than wrapped around the loop, so
      // re-upload time stays out of the mint figure — otherwise the new leg
      // would silently inflate reported mint latency.
      results.mintMs += performance.now() - mintStart;
    }
    // Re-upload first: it runs first, and a mint failure is usually its
    // consequence — a batch charged to fileFail because there was nothing to
    // mint against reads as unexplained otherwise.
    for (const line of errorTallyLines(reuploadErrors, 'File re-upload')) console.error(line);
    for (const line of errorTallyLines(fileErrors, 'File mint')) console.error(line);
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

  installSignalHandlers();

  // The threshold is resolved up here with the other preflight checks: read
  // only at the summary, a mistyped one would surface as a green exit code two
  // hours after the run it was meant to judge.
  //
  // Two rejections, not one: readFlag refuses the shapes that are wrong before
  // a value is even read — the flag as the final token, or followed by another
  // flag — and parseMaxFailRate refuses a value that is present but not a
  // percentage. `--max-fail-rate=` reaches the second as an empty string,
  // which is deliberate on readFlag's part and why parseMaxFailRate checks for
  // blank BEFORE Number, which would read whitespace as the strictest possible
  // threshold rather than an error.
  const { value: maxFailRateRaw, error: maxFailRateShapeError } =
    readFlag(args, 'max-fail-rate', flagSpec('max-fail-rate', true).defaultValue);
  if (maxFailRateShapeError) { console.error(`FATAL: ${maxFailRateShapeError}`); process.exit(1); }
  const { rate: maxFailRate, error: maxFailRateError } = parseMaxFailRate(maxFailRateRaw);
  if (maxFailRateError) { console.error(`FATAL: ${maxFailRateError}`); process.exit(1); }

  // Recovery mode for a previous run that was killed before it could reclaim.
  // Sits after the pure argv validation above — a malformed command line is
  // worth refusing before anything acts on it — but BEFORE the target guard
  // below: revoking resources that already exist is always the safe direction,
  // wherever they were created, and a refused target must not strand a ledger
  // the operator is trying to clean up.
  // No missing-value check here: resolveArgErrors above owns that refusal and
  // has already exited, so a requested reclaim always carries a path by now.
  const { requested, path: reclaimOnly } = parseReclaimArg(args);
  if (requested) {
    activeLedgerPath = reclaimOnly;
    const {
      missing, failed, refused, unreadable,
    } = await reclaimOnce(reclaimOnly);
    // A missing ledger is a failure in recovery mode, not "nothing to do":
    // the operator is here because something already went wrong, and a
    // cheerful exit 0 on a mistyped path is how live resources get abandoned.
    process.exit(missing || refused || unreadable || failed > 0 ? 1 : 0);
  }
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

  // After the guard: no point creating the ledger for a run that is refused.
  preflightLedger();

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

      console.log(roundReportLine({ elapsed, round, results }));
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
  // `round` is the ATTEMPTED count, not the completed one: it starts at 0 and
  // is incremented at the top of the loop, before runRound, so a round that
  // throws still counts. That is the whole reason it is passed separately —
  // allResults holds only rounds that returned, so roundsFailed is the
  // difference. The contract is stated here because it is the one piece no
  // test guards: this loop is unreachable from the suite, so a `for (let round
  // = 1; ...)` rewrite would leave every runReport test green while reporting
  // one failed round too many.
  const summary = runReport({ allResults, roundsAttempted: round, maxFailRate });
  for (const line of summary.lines) console.log(line);
  // exitCode rather than exit: writes to a pipe are asynchronous in Node, and
  // exiting here can truncate the summary that explains the code being set.
  if (summary.failed) process.exitCode = 1;

  // The minted counts above and the sweep's count below measure different
  // things — recipient links versus the parent resources that own them — so
  // say so, or the two numbers look like a leak of the difference.
  //
  // quiet: reclaim reads the ledger again below, and a torn line reported by
  // both reads looks like two torn lines.
  const parents = [...new Set(readLedger(LEDGER_PATH, true) || [])].length;
  const minted = allResults.reduce((s, r) => s + r.fileLinks + r.locLinks, 0);
  console.log(`Reclaimable parents: ${parents} (covering ${minted} minted qURLs)`);
  console.log(`Ledger: ${LEDGER_PATH}`);

  // Set independently of summary.failed: a run whose rounds all succeeded can
  // still leave resources on the tenancy, and that is worth a non-zero exit on
  // its own.
  const { failed } = await reclaimOnce(LEDGER_PATH);
  if (failed > 0) process.exitCode = 1;
}

// Only run the CLI entry point when invoked directly. Imported-from-test loads
// only the exported helpers — see tests/loadtest-target-guard.test.js and
// tests/loadtest-reclaim.test.js.
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

// trackCreate is exported for the suite rather than for callers: the
// in-flight-wait branch of the drain is otherwise unreachable from a test,
// and it is the branch the sweep-vs-run-loop fix turns on.
module.exports = {
  // Reclaim ledger
  LEDGER_PATH,
  readLedger,
  pruneLedger,
  ledgerEndpoints,
  reclaim,
  reclaimOnce,
  resetReclaimStateForTests,
  parseReclaimArg,
  trackCreate,
  recordResource,
  // Reporting decisions, exported for tests/loadtest-reporting.test.js — see
  // the "Run reporting" section for why they are pure rather than inline.
  roundReportLine,
  tallyFailure,
  errorTallyLines,
  parseMaxFailRate,
  formatRatePair,
  formatThresholdPct,
  runReport,
  ERROR_TALLY_LIMIT,
  DEFAULT_MAX_FAIL_RATE_PCT,
  // CLI argument validation
  FLAGS,
  flagSpec,
  readFlag,
  readBooleanFlag,
  resolveBooleanArgs,
  parsePositiveInt,
  resolveNumericArgs,
  resolveFileArg,
  resolveLedgerArg,
  resolveReclaimArg,
  resolveUnknownArgs,
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
