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

const {
  readArg,
  roundReportLine,
  tallyFailure,
  errorTallyLines,
  parseMaxFailRate,
  formatRatePair,
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

describe('readArg — both flag spellings, so neither falls through silently', () => {
  // The equals form used to miss entirely and hand back the default. Harmless
  // for --count, whose value shows up in the very next line of output; not
  // harmless for --max-fail-rate, which decides the exit code, so an operator
  // waiving the check with `=100` would have been failed by the strict
  // default two hours later.
  it('reads the equals form', () => {
    expect(readArg(['--max-fail-rate=100'], 'max-fail-rate', '10')).toBe('100');
  });

  it('reads the space form', () => {
    expect(readArg(['--max-fail-rate', '100'], 'max-fail-rate', '10')).toBe('100');
  });

  it('falls back to the default when the flag is absent', () => {
    expect(readArg(['--location'], 'max-fail-rate', '10')).toBe('10');
  });

  it('does not match a flag that merely starts with the name', () => {
    expect(readArg(['--max-fail-rate-extra=5'], 'max-fail-rate', '10')).toBe('10');
  });

  // A value can legitimately contain '=' — only the flag token is split on it.
  it('keeps an equals sign inside the value', () => {
    expect(readArg(['--file=/tmp/a=b.bin'], 'file', null)).toBe('/tmp/a=b.bin');
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
  it.each([['not a number', 'ten'], ['a percent sign', '10%'], ['negative', '-1'], ['over 100', '101']])(
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
      'Total failures: 0',
      'Failure threshold: 10.0% (--max-fail-rate)',
      'Avg round time: 0.6s',
      'Avg upload: 200ms, avg mint/round: 300ms',
    ]);
  });

  // The threshold that decided the exit code is echoed on every run, so a
  // typo'd flag that silently took the default is visible in the log.
  it('states the threshold it judged against, even when passing', () => {
    expect(report([round({ fileLinks: 10, uploadMs: 1, mintMs: 1, totalMs: 2 })], 1, 0.25).lines)
      .toContain('Failure threshold: 25.0% (--max-fail-rate)');
  });

  it('does not print a breach as an equality', () => {
    const results = [round({ fileLinks: 8999, fileFail: 1001, uploadMs: 1, mintMs: 1, totalMs: 2 })];
    expect(report(results).lines).toContain(
      'FAILED: link failure rate 10.01% (1001/10000) exceeds --max-fail-rate 10.00%',
    );
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
    expect(result.lines).toContain('Total failures: 100');
    expect(result.linkFailRate).toBe(0.5);
  });

  it('omits the file latency line for a location-only run', () => {
    const lines = report([round({ locLinks: 100, locMs: 800, totalMs: 900 })]).lines;
    expect(lines.join('\n')).not.toContain('Avg upload');
  });
});
