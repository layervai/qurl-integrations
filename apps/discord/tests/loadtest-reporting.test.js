/**
 * Tests for the run reporting in scripts/loadtest-standalone.js.
 *
 * Every assertion here exists because the reporting was inverted: it was at
 * its quietest exactly when the run was at its worst. A file leg that issued
 * zero successful mint requests (#1168) printed no failure counts, printed a
 * latency block anyway, and exited 0 — so the bug survived the file's entire
 * lifetime without a single run looking wrong.
 *
 * That makes these tests the only signal on this code. main() and runRound()
 * cannot be called from a suite (they run the load test), and scripts/ is
 * outside jest's collectCoverageFrom, so an untested reporting decision is
 * reported by nothing at all. The decisions are pure functions for that
 * reason, matching targetGuardReport.
 *
 * Requiring the script does NOT run the load test: its CLI entry point is
 * behind `require.main === module`.
 */

const fs = require('fs');
const path = require('path');
const parser = require('@babel/parser');
const traverseModule = require('@babel/traverse');
const traverse = traverseModule.default || traverseModule;

const {
  readFlag,
  valuedBooleanFlags,
  BOOLEAN_FLAGS,
  resolveGuardInputs,
  roundReportLine,
  tallyFailure,
  errorTallyLines,
  parseMaxFailRate,
  formatRatePair,
  formatThresholdPct,
  runReport,
  ERROR_TALLY_LIMIT,
  DEFAULT_MAX_FAIL_RATE_PCT,
} = require('../scripts/loadtest-standalone');

/** A round's counters, defaulted to a round that did nothing. */
const round = (over = {}) => ({
  fileLinks: 0, fileFail: 0, locLinks: 0, locFail: 0,
  uploadMs: 0, mintMs: 0, locMs: 0, totalMs: 0, ...over,
});

const tally = (pairs) => {
  const map = new Map();
  for (const [message, weight] of pairs) tallyFailure(map, message, weight);
  return map;
};

describe('roundReportLine — the round line is gated on attempts, not successes', () => {
  // The regression that matters. Under the old `if (results.fileLinks > 0)`
  // gate this exact round printed `[30s] Round 1: total=0.3s` and nothing
  // else: 100 failed mints rendered as a fast, quiet round.
  it('prints the file segment when every file mint failed', () => {
    const line = roundReportLine({
      elapsed: '30',
      round: 1,
      results: round({ fileFail: 100, uploadMs: 250, mintMs: 1, totalMs: 300 }),
    });
    expect(line).toBe('[30s] Round 1: file(upload=250ms mint=1ms ok=0 fail=100) total=0.3s');
    expect(line).not.toBe('[30s] Round 1: total=0.3s');
  });

  it('prints the location segment when every location mint failed', () => {
    expect(roundReportLine({
      elapsed: '60',
      round: 2,
      results: round({ locFail: 40, locMs: 900, totalMs: 1000 }),
    })).toBe('[60s] Round 2: location(900ms ok=0 fail=40) total=1.0s');
  });

  // Partial failure was always visible — it is total failure that vanished.
  // Asserted so a "fix" that merely reworded the partial case is not mistaken
  // for one that inverted the gate.
  it('prints partial failure, as it always did', () => {
    expect(roundReportLine({
      elapsed: '30',
      round: 1,
      results: round({ fileLinks: 90, fileFail: 10, uploadMs: 200, mintMs: 400, totalMs: 700 }),
    })).toBe('[30s] Round 1: file(upload=200ms mint=400ms ok=90 fail=10) total=0.7s');
  });

  it('reports both legs when both ran', () => {
    expect(roundReportLine({
      elapsed: '90',
      round: 3,
      results: round({
        fileLinks: 0, fileFail: 50, uploadMs: 100, mintMs: 5,
        locLinks: 50, locFail: 0, locMs: 800, totalMs: 1000,
      }),
    })).toBe('[90s] Round 3: file(upload=100ms mint=5ms ok=0 fail=50) location(800ms ok=50 fail=0) total=1.0s');
  });

  // The gate still has to stay silent about a leg that never ran, or a
  // location-only run grows a permanent `file(... ok=0 fail=0)` segment.
  it('omits a leg that attempted nothing', () => {
    const line = roundReportLine({
      elapsed: '10',
      round: 1,
      results: round({ locLinks: 5, locMs: 50, totalMs: 60 }),
    });
    expect(line).toBe('[10s] Round 1: location(50ms ok=5 fail=0) total=0.1s');
    expect(line).not.toContain('file(');
  });
});

describe('errorTallyLines — failures are deduped by message, not by count', () => {
  // The old `if (results.fileFail === 0) console.error(...)` printed the first
  // message of a round and dropped every later one. A round mixing a systemic
  // bug with transient throttling reported whichever arrived first, hiding the
  // distinction an operator is actually trying to draw.
  it('reports every distinct message in a round', () => {
    const lines = errorTallyLines(
      tally([['mintLinks is not a function', 40], ['HTTP 429', 1], ['HTTP 429', 1]]),
      'File mint',
    );
    expect(lines).toEqual([
      '  File mint error x40: mintLinks is not a function',
      '  File mint error x2: HTTP 429',
    ]);
  });

  it('ranks by attempts taken down, most damaging first', () => {
    expect(errorTallyLines(tally([['rare', 1], ['common', 9]]), 'File mint')).toEqual([
      '  File mint error x9: common',
      '  File mint error x1: rare',
    ]);
  });

  // Weighting by batch size is what lets the tally be read against the round
  // line: these counts sum to that line's `fail=`.
  it('weights a file batch by the mints it took down', () => {
    const map = tally([['boom', 10], ['boom', 10], ['boom', 5]]);
    expect(map.get('boom')).toBe(25);
  });

  it('says nothing when nothing failed', () => {
    expect(errorTallyLines(new Map(), 'File mint')).toEqual([]);
  });

  // An error message carrying a unique id makes every failure its own key.
  // Uncapped, a 200-recipient round prints 200 stderr lines; the omitted
  // volume still has to be named rather than silently dropped.
  it('caps distinct messages and accounts for the remainder', () => {
    const many = Array.from({ length: ERROR_TALLY_LIMIT + 3 }, (_, i) => [`request ${i} failed`, 1]);
    const lines = errorTallyLines(tally(many), 'Location mint');
    expect(lines).toHaveLength(ERROR_TALLY_LIMIT + 1);
    expect(lines[ERROR_TALLY_LIMIT])
      .toBe('  Location mint error: 3 further distinct message(s) covering 3 attempt(s)');
  });

  it('keeps the heaviest messages when it caps', () => {
    const noise = Array.from({ length: ERROR_TALLY_LIMIT }, (_, i) => [`noise ${i}`, 1]);
    const lines = errorTallyLines(tally([...noise, ['the real one', 500]]), 'File mint');
    expect(lines[0]).toBe('  File mint error x500: the real one');
    expect(lines).toHaveLength(ERROR_TALLY_LIMIT + 1);
  });
});

describe('--max-fail-rate — the flag that decides the exit code, end to end', () => {
  // Pins the WIRING, not either half. readFlag's own mechanics — both
  // spellings, last-wins, prefix non-matching, refusing a dropped value — are
  // covered against --file in tests/loadtest-silent-failure.test.js. What only
  // this flag can lose is the connection between them: it is the one whose
  // value decides an exit code two hours after the run, so a value that never
  // reached parseMaxFailRate would surface as a green run at the strict
  // default, with nothing in the log naming the threshold that applied.
  //
  // Mirrors main()'s two steps in order, so a reordering there shows up here.
  const resolve = (argv) => {
    const { value, error } = readFlag(argv, 'max-fail-rate', String(DEFAULT_MAX_FAIL_RATE_PCT));
    return error ? { error } : parseMaxFailRate(value);
  };

  it.each([
    ['the equals form', ['--max-fail-rate=100'], 1],
    ['the space form', ['--max-fail-rate', '100'], 1],
    ['a fractional threshold', ['--max-fail-rate=0.5'], 0.005],
  ])('resolves %s', (_label, argv, rate) => {
    // `=100` is the waiver spelling an operator reaches for to turn the check
    // off; it used to miss the reader entirely and run at the strict default.
    expect(resolve(argv)).toEqual({ rate });
  });

  it.each([
    ['the flag is absent', ['--location']],
    ['a longer flag merely starts with the name', ['--max-fail-rate-extra=5']],
  ])('takes the default when %s', (_label, argv) => {
    expect(resolve(argv)).toEqual({ rate: DEFAULT_MAX_FAIL_RATE_PCT / 100 });
  });

  it.each([
    ['the equals form carries nothing', ['--max-fail-rate=']],
    ['the value is an empty token', ['--max-fail-rate', '', '--location']],
    ['the value is whitespace', ['--max-fail-rate', '   ']],
  ])('refuses a threshold when %s', (_label, argv) => {
    // Refused by parseMaxFailRate rather than by the reader: readFlag hands an
    // empty inline value on deliberately, leaving "what counts as empty" to
    // the caller. Whitespace is the one that matters most — Number('   ') is
    // 0, so a blank value would otherwise become the STRICTEST possible
    // threshold rather than an error.
    const { rate, error } = resolve(argv);
    expect(rate).toBeUndefined();
    expect(error).toContain('--max-fail-rate must be a percentage');
  });

  it('refuses the flag left as the last token, naming the default', () => {
    const { error } = resolve(['--count', '20', '--max-fail-rate']);
    expect(error).toBe(`--max-fail-rate was given no value (omit it to use the default of ${DEFAULT_MAX_FAIL_RATE_PCT})`);
  });

  it('refuses a following flag rather than swallowing it as the value', () => {
    // Changed with the #1174 merge and worth pinning at the new answer: the
    // old reader took '--location' as the value and let parseMaxFailRate
    // reject it by name, which reported a threshold fault for what was really
    // a forgotten value — and consumed a real flag on the way.
    const { error } = resolve(['--max-fail-rate', '--location']);
    expect(error).toBe('--max-fail-rate was given no value — the next argument is the flag --location (omit it to use the default of 10)');
  });

  it('rejects an out-of-range threshold by name', () => {
    expect(resolve(['--max-fail-rate=101']).error).toContain("got '101'");
    expect(resolve(['--max-fail-rate=-1']).error).toContain("got '-1'");
  });
});

describe('valuedBooleanFlags — a boolean written with a value is not absence', () => {
  // hasFlag matches the bare token, so `--location=true` reads as the flag
  // being absent — and the file leg runs whenever location does not, so the
  // run silently measures the opposite of what was typed. Worth rejecting
  // once `--count=20` works and an operator has reason to try `=` here.
  it('catches a valued --location', () => {
    expect(valuedBooleanFlags(['--count', '20', '--location=true'])).toEqual(['location']);
  });

  it('catches a valued --allow-production', () => {
    expect(valuedBooleanFlags(['--allow-production=1'])).toEqual(['allow-production']);
  });

  it('reports every offender, not just the first', () => {
    expect(valuedBooleanFlags(['--location=1', '--allow-production=1']))
      .toEqual(['location', 'allow-production']);
  });

  it.each([
    ['the bare flags', ['--location', '--allow-production']],
    ['a valued flag that does take a value', ['--max-fail-rate=100', '--count=20']],
    ['no flags at all', []],
  ])('passes %s', (_label, argv) => {
    expect(valuedBooleanFlags(argv)).toEqual([]);
  });

  // Guards the pairing for real, by reading the script's own boolean-reader
  // call sites rather than restating the constant: a boolean flag added to the
  // parser but not to BOOLEAN_FLAGS gets no rejection, which is the
  // silent-absence trap all over again. Parsed statically because the calls
  // sit at module scope in a file whose main() a suite cannot run — the same
  // approach as tests/ddb-reserved-words-static.test.js.
  //
  // Two call shapes, because #1176 put an indirection between the flag names
  // and the reader. resolveGuardInputs still names its flag literally
  // (`readBooleanFlag(argv, 'allow-production')`), but resolveBooleanArgs
  // funnels its own through a local `read` helper, so the readBooleanFlag call
  // inside it carries that helper's PARAMETER — an Identifier, not a literal.
  // Collecting only direct literals would have found one flag; collecting
  // `read`'s arguments as well is what keeps every name covered.
  //
  // `read` is scoped to resolveBooleanArgs by source range rather than matched
  // by name: resolveNumericArgs has a local helper of exactly the same name,
  // and picking its `read('count', '100')` up here would demand that a NUMERIC
  // flag appear in BOOLEAN_FLAGS.
  it('covers every flag the boolean readers read', () => {
    const source = fs.readFileSync(
      path.join(__dirname, '..', 'scripts', 'loadtest-standalone.js'), 'utf8',
    );
    const ast = parser.parse(source, { sourceType: 'script' });
    // Matches a FunctionDeclaration only. That is fail-closed rather than a
    // gap: converting resolveBooleanArgs to an arrow-const leaves this null
    // and trips the assertion below, so the failure is a prompt to widen this
    // traversal, not a bug in the code under test.
    let resolveBoolean = null;
    traverse(ast, {
      FunctionDeclaration(p) {
        if (p.node.id?.name === 'resolveBooleanArgs') resolveBoolean = p.node;
      },
    });
    expect(resolveBoolean).not.toBeNull();
    const inResolveBoolean = (node) =>
      node.start >= resolveBoolean.start && node.end <= resolveBoolean.end;

    const names = [];
    traverse(ast, {
      CallExpression({ node }) {
        if (node.callee.type !== 'Identifier') return;
        let arg;
        if (node.callee.name === 'readBooleanFlag') {
          // The NAME is the second argument: #1174 made these readers take
          // argv first, like every other reader here, so the flag moved along
          // one. Reading argument 0 yields the `args`/`argv` identifier, which
          // is what the StringLiteral assertion below caught when that landed.
          arg = node.arguments[1];
          // The one call site that legitimately passes a variable is the one
          // inside resolveBooleanArgs' `read` helper; its names are collected
          // from `read`'s own call sites just below, so skipping it here drops
          // nothing — for as long as that helper is still named `read`. Rename
          // it and this branch keeps skipping while the `read` branch stops
          // matching, which drops `location` and leaves both assertions below
          // green. That is the same silent narrowing #1176 caused, one level
          // in; the collected-set equality below is what makes it loud.
          if (arg && arg.type === 'Identifier' && inResolveBoolean(node)) return;
        } else if (node.callee.name === 'read' && inResolveBoolean(node)) {
          arg = node.arguments[0];
        } else {
          return;
        }
        // A computed argument would silently contribute nothing, so fail
        // rather than let the check quietly stop covering that call.
        expect(arg && arg.type).toBe('StringLiteral');
        names.push(arg.value);
      },
    });
    // Fails closed on a rename: this is what caught #1176 replacing hasFlag
    // with readBooleanFlag, which left the old traversal matching nothing and
    // the whole check passing over an empty set.
    expect(names.length).toBeGreaterThan(0);
    // And fails closed on a PARTIAL loss, which the count alone cannot see.
    // Equality here is on what the traversal COLLECTED — deliberately not the
    // same assertion as the subset one on BOOLEAN_FLAGS below, which stays a
    // subset for the reason given there. Adding a boolean flag is meant to
    // fail this line: the failure is the prompt to confirm the new flag also
    // reaches BOOLEAN_FLAGS, which is the pairing this whole test exists for.
    expect(new Set(names)).toEqual(new Set(['location', 'allow-production']));
    // Still a subset rather than an equality, but for a weaker reason than it
    // used to be. Both boolean flags now reach a boolean reader — #1174 moved
    // --allow-production onto one inside resolveGuardInputs — so the two sets
    // happen to coincide today. Pinning equality would then fail the moment a
    // value-less flag is added that some other reader owns, which is a design
    // choice this check has no business forcing.
    expect(BOOLEAN_FLAGS).toEqual(expect.arrayContaining([...new Set(names)]));
  });

  // resolveGuardInputs is the reader for --allow-production. It went through
  // the shared boolean reader in #1174, so the traversal above does now see
  // it — but the traversal only proves the flag is SPELLED somewhere, and this
  // proves the guard actually honours it. Those are different failures.
  it('covers --allow-production, which resolveGuardInputs reads off argv', () => {
    expect(resolveGuardInputs({}, ['--allow-production']).allowProdFlag).toBe(true);
    expect(BOOLEAN_FLAGS).toContain('allow-production');
  });
});

describe('formatRatePair — a breach never prints as "X% exceeds X%"', () => {
  // 1001/10000 is 10.01%: over a 10% threshold, but identical to it at one
  // decimal place. Printed that way the line reads as a bug in the tool
  // rather than a finding about the run.
  it('widens both until they differ', () => {
    expect(formatRatePair(1001 / 10000, 0.1)).toEqual(['10.01%', '10.00%']);
  });

  it('stays at one decimal when that already distinguishes them', () => {
    expect(formatRatePair(1, 0.1)).toEqual(['100.0%', '10.0%']);
  });
});

describe('formatThresholdPct — the echo shows the threshold that was set', () => {
  // At a fixed one decimal the confirmation line rounds away exactly the
  // values an operator would double-check: 0.05% reads as 0.1%, twice what
  // was asked for, and 0.01% reads as 0.0%, which is not a threshold at all.
  it.each([
    ['a whole percentage', 0.1, '10.0%'],
    ['one decimal', 0.025, '2.5%'],
    ['the waiver', 1, '100.0%'],
    ['zero', 0, '0.0%'],
    ['two decimals', 0.0005, '0.05%'],
    ['three decimals', 0.0001, '0.01%'],
    ['four decimals', 0.00001, '0.001%'],
    ['a repeating value', 0.33333, '33.333%'],
  ])('renders %s', (_label, rate, expected) => {
    expect(formatThresholdPct(rate)).toBe(expected);
  });

  it('never renders a non-zero threshold as 0.0%', () => {
    expect(formatThresholdPct(0.0001)).not.toBe('0.0%');
  });

  // Past the display bound the two cases diverge, and conflating them would
  // reintroduce the defect this function exists to fix: a value too fine to
  // show must not read as no threshold, and a value that is merely
  // long-running must not read as too fine to show.
  it('reports a threshold below the display bound as below it', () => {
    expect(formatThresholdPct(1e-9)).toBe('<0.000001%');
  });

  it('shows a value that just needs more digits at the bound, not below it', () => {
    expect(formatThresholdPct(0.333333333333)).toBe('33.333333%');
  });

  // A percentage does not survive the round trip through a fraction:
  // `0.23 / 100 * 100` is 0.22999999999999998. Comparing the widened text
  // against that finds no width that matches, so these echoed as `0.230000%`
  // — accurate, but not the value as it was typed, which is the one thing
  // this line owes the operator.
  it.each([['0.23', 0.0023, '0.23%'], ['0.45', 0.0045, '0.45%'], ['0.85', 0.0085, '0.85%']])(
    'echoes %s%% as typed despite the float round trip',
    (_label, rate, expected) => {
      expect(formatThresholdPct(rate)).toBe(expected);
    },
  );

  // The sweep the three cases above were found by. Every threshold an
  // operator can type in hundredths renders as typed — no trailing-zero
  // padding, and none mistaken for below the display bound.
  it('renders every hundredth from 0.01% to 100.00% as typed', () => {
    const wrong = [];
    for (let i = 1; i <= 10000; i++) {
      const pct = i / 100;
      const text = formatThresholdPct(pct / 100);
      if (text !== `${pct.toFixed(String(pct).split('.')[1]?.length || 1)}%`) wrong.push([pct, text]);
    }
    expect(wrong).toEqual([]);
  });
});

describe('parseMaxFailRate — a malformed threshold fails closed', () => {
  it.each([
    ['the default', String(DEFAULT_MAX_FAIL_RATE_PCT), 0.1],
    ['zero', '0', 0],
    ['a hundred', '100', 1],
    ['a fraction', '2.5', 0.025],
  ])('accepts %s', (_label, raw, expected) => {
    expect(parseMaxFailRate(raw)).toEqual({ rate: expected });
  });

  // Each of these is NaN or out of range under a bare `Number`, and NaN
  // compares false against every rate — so an unvalidated parse would disable
  // the threshold silently and report the run as a pass.
  // Whitespace is the sharp one: `Number('   ')` is 0, so an unguarded parse
  // turns a blank value into the strictest possible threshold rather than an
  // error — silently failing runs the operator meant to be lenient about.
  it.each([
    ['not a number', 'ten'], ['a percent sign', '10%'], ['negative', '-1'],
    ['over 100', '101'], ['whitespace', '   '], ['empty', ''],
  ])(
    'rejects %s',
    (_label, raw) => {
      const parsed = parseMaxFailRate(raw);
      expect(parsed.rate).toBeUndefined();
      expect(parsed.error).toContain('--max-fail-rate');
    },
  );
});

describe('runReport — the summary and the exit code', () => {
  const report = (allResults, roundsAttempted = allResults.length, maxFailRate = 0.1) =>
    runReport({ allResults, roundsAttempted, maxFailRate });

  // The second half of #1168's invisibility. `allResults[0].uploadMs > 0` is
  // set before the first mint is attempted, so it held while every mint
  // failed and the summary reported how long the failures took as though it
  // were how long the successes took — a fast failure reading as a fast
  // success, which is the shape of a headline result.
  it('refuses to report a mint latency when no qURL was minted', () => {
    const lines = report([round({ fileFail: 100, uploadMs: 250, mintMs: 1, totalMs: 300 })]).lines;
    expect(lines).toContain('Avg upload: 250ms, avg mint/round: n/a — all 100 mint attempt(s) failed');
    expect(lines.join('\n')).not.toMatch(/avg mint: \d/);
  });

  it('reports mint latency once qURLs are actually minted', () => {
    expect(report([round({ fileLinks: 100, uploadMs: 250, mintMs: 400, totalMs: 700 })]).lines)
      .toContain('Avg upload: 250ms, avg mint/round: 400ms');
  });

  // The same blend one granularity down. A round whose every mint failed
  // times only failures, which are far faster than a real mint, so averaging
  // it in reports a latency neither the healthy nor the dead rounds ever saw
  // — here 201ms, from a 400ms round and a 2ms one.
  it('averages mint latency over the rounds that actually minted', () => {
    const lines = report([
      round({ fileLinks: 100, uploadMs: 250, mintMs: 400, totalMs: 700 }),
      round({ fileFail: 100, uploadMs: 250, mintMs: 2, totalMs: 300 }),
    ]).lines;
    expect(lines).toContain('Avg upload: 250ms, avg mint/round: 400ms');
    expect(lines.join('\n')).not.toContain('201ms');
  });

  it('passes a clean run', () => {
    const result = report([
      round({ fileLinks: 100, uploadMs: 200, mintMs: 300, totalMs: 600 }),
      round({ fileLinks: 100, uploadMs: 200, mintMs: 300, totalMs: 600 }),
    ]);
    expect(result.failed).toBe(false);
    expect(result.lines).toEqual([
      'Rounds: 2',
      'Total links minted: 200',
      'Total link failures: 0',
      'Failure threshold: 10.0% (--max-fail-rate)',
      'Avg round time: 0.6s',
      'Avg upload: 200ms, avg mint/round: 300ms',
      // One upload per round and no re-uploads: these rounds are built with
      // --count at or below a pool's depth, so the plan is a single batch.
      // The line prints anyway — uploads-per-round is how a reader judges
      // whether the run reproduced a real send's load, and it is exactly as
      // informative when the answer is "no re-uploads were needed".
      'Uploads: 2 ok (2 initial + 0 re-upload)',
    ]);
  });

  // The threshold that decided the exit code is echoed on every run, so a
  // typo'd flag that silently took the default is visible in the log.
  it('states the threshold it judged against, even when passing', () => {
    expect(report([round({ fileLinks: 10, uploadMs: 1, mintMs: 1, totalMs: 2 })], 1, 0.25).lines)
      .toContain('Failure threshold: 25.0% (--max-fail-rate)');
  });

  it('echoes a sub-tenth-of-a-percent threshold without rounding it away', () => {
    expect(report([round({ fileLinks: 10, uploadMs: 1, mintMs: 1, totalMs: 2 })], 1, 0.0005).lines)
      .toContain('Failure threshold: 0.05% (--max-fail-rate)');
  });

  it('does not print a breach as an equality', () => {
    const results = [round({ fileLinks: 8999, fileFail: 1001, uploadMs: 1, mintMs: 1, totalMs: 2 })];
    expect(report(results).lines).toContain(
      'FAILED: link failure rate 10.01% (1001/10000) exceeds --max-fail-rate 10.00%',
    );
  });

  // Rounds ran, nothing was attempted. Both rates are 0/0, so without this
  // the run reads as clean while having measured nothing.
  //
  // Defensive rather than live since #1171 merged in — `--count 0` was the
  // way in and parsePositiveInt refuses zero at preflight now. Kept, and
  // exercised through runReport directly, because the guarantee it rests on
  // lives in a different function: the branch has to survive that one
  // changing. See the reachability note in runReport.
  it('fails a run that attempted no qURL, and does not call it a failure', () => {
    const result = report([round({ uploadMs: 200, totalMs: 300 })]);
    expect(result.failed).toBe(true);
    expect(result.lines).toContain('FAILED: no qURL was attempted');
    expect(result.lines).toContain('Avg upload: 200ms, avg mint/round: n/a — no mint was attempted');
    expect(result.lines.join('\n')).not.toContain('all 0 mint attempt(s) failed');
  });

  it('fails a run in which every mint failed', () => {
    const result = report([round({ fileFail: 100, uploadMs: 250, mintMs: 1, totalMs: 300 })]);
    expect(result.failed).toBe(true);
    expect(result.lines).toContain(
      'FAILED: link failure rate 100.0% (100/100) exceeds --max-fail-rate 10.0%',
    );
  });

  it('tolerates transient failures under the threshold', () => {
    const result = report([round({ fileLinks: 99, fileFail: 1, uploadMs: 200, mintMs: 300, totalMs: 600 })]);
    expect(result.failed).toBe(false);
    expect(result.lines.join('\n')).not.toContain('FAILED');
  });

  // Every round threw before recording anything. allResults is empty, so
  // without roundsAttempted this printed `Rounds: 0` and exited 0 — the
  // per-round line's invisibility one level up, and the worst version of it.
  it('fails, and says so, when no round completed', () => {
    const result = report([], 5);
    expect(result.failed).toBe(true);
    expect(result.lines[0]).toBe('Rounds: 0 completed, 5 failed');
    expect(result.lines).toContain('FAILED: no round completed');
    // One condition, one finding: a zero-completed run always has a 100%
    // round rate, so naming both would report it twice.
    expect(result.lines.filter((l) => l.startsWith('FAILED:'))).toHaveLength(1);
  });

  it('fails when the run window was too short to attempt a round', () => {
    const result = report([], 0);
    expect(result.failed).toBe(true);
    expect(result.lines).toContain('FAILED: no round completed');
  });

  // The two rates are judged separately because they dilute each other: the
  // rounds that survive here mint everything they attempt, so a single blended
  // rate would be 0% and the run would pass with four rounds dead.
  it('fails on round failures even when every completed round was perfect', () => {
    const result = report([round({ fileLinks: 100, uploadMs: 200, mintMs: 300, totalMs: 600 })], 5);
    expect(result.failed).toBe(true);
    expect(result.linkFailRate).toBe(0);
    expect(result.lines).toContain(
      'FAILED: round failure rate 80.0% (4/5) exceeds --max-fail-rate 10.0%',
    );
  });

  it('reports both rates when both are breached', () => {
    const result = report([round({ fileFail: 100, uploadMs: 250, mintMs: 1, totalMs: 300 })], 4);
    expect(result.lines.filter((l) => l.startsWith('FAILED:'))).toHaveLength(2);
  });

  // --max-fail-rate 100 is the documented escape hatch — the comparison is
  // strict, so a 100% rate does not exceed it. A run that measured nothing is
  // still a failure: there is no rate to be lenient about.
  it('lets --max-fail-rate 100 waive the rates but not an unmeasured run', () => {
    expect(report([round({ fileFail: 100, uploadMs: 250, mintMs: 1, totalMs: 300 })], 1, 1).failed)
      .toBe(false);
    expect(report([], 3, 1).failed).toBe(true);
  });

  it('counts both legs toward the failure rate', () => {
    const result = report([round({
      fileLinks: 50, fileFail: 50, uploadMs: 100, mintMs: 200,
      locLinks: 50, locFail: 50, locMs: 300, totalMs: 600,
    })]);
    expect(result.lines).toContain('Total links minted: 100');
    expect(result.lines).toContain('Total link failures: 100');
    expect(result.linkFailRate).toBe(0.5);
  });

  it('omits the file latency line for a location-only run', () => {
    const lines = report([round({ locLinks: 100, locMs: 800, totalMs: 900 })]).lines;
    expect(lines.join('\n')).not.toContain('Avg upload');
  });
});
