
const fs = require('fs');
const path = require('path');
const parser = require('@babel/parser');
const traverseModule = require('@babel/traverse');
const traverse = traverseModule.default || traverseModule;

const {
  readFlag,
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

  it('weights a file batch by the mints it took down', () => {
    const map = tally([['boom', 10], ['boom', 10], ['boom', 5]]);
    expect(map.get('boom')).toBe(25);
  });

  it('says nothing when nothing failed', () => {
    expect(errorTallyLines(new Map(), 'File mint')).toEqual([]);
  });

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
  const resolve = (argv) => {
    const { value, error } = readFlag(argv, 'max-fail-rate', String(DEFAULT_MAX_FAIL_RATE_PCT));
    return error ? { error } : parseMaxFailRate(value);
  };

  it.each([
    ['the equals form', ['--max-fail-rate=100'], 1],
    ['the space form', ['--max-fail-rate', '100'], 1],
    ['a fractional threshold', ['--max-fail-rate=0.5'], 0.005],
  ])('resolves %s', (_label, argv, rate) => {
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
    const { rate, error } = resolve(argv);
    expect(rate).toBeUndefined();
    expect(error).toContain('--max-fail-rate must be a percentage');
  });

  it('refuses the flag left as the last token, naming the default', () => {
    const { error } = resolve(['--count', '20', '--max-fail-rate']);
    expect(error).toBe(`--max-fail-rate was given no value (omit it to use the default of ${DEFAULT_MAX_FAIL_RATE_PCT})`);
  });

  it('refuses a following flag rather than swallowing it as the value', () => {
    const { error } = resolve(['--max-fail-rate', '--location']);
    expect(error).toBe('--max-fail-rate was given no value — the next argument is the flag --location (omit it to use the default of 10)');
  });

  it('rejects an out-of-range threshold by name', () => {
    expect(resolve(['--max-fail-rate=101']).error).toContain("got '101'");
    expect(resolve(['--max-fail-rate=-1']).error).toContain("got '-1'");
  });
});

describe('formatRatePair — a breach never prints as "X% exceeds X%"', () => {
  it('widens both until they differ', () => {
    expect(formatRatePair(1001 / 10000, 0.1)).toEqual(['10.01%', '10.00%']);
  });

  it('stays at one decimal when that already distinguishes them', () => {
    expect(formatRatePair(1, 0.1)).toEqual(['100.0%', '10.0%']);
  });
});

describe('formatThresholdPct — the echo shows the threshold that was set', () => {
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

  it('reports a threshold below the display bound as below it', () => {
    expect(formatThresholdPct(1e-9)).toBe('<0.000001%');
  });

  it('shows a value that just needs more digits at the bound, not below it', () => {
    expect(formatThresholdPct(0.333333333333)).toBe('33.333333%');
  });

  it.each([['0.23', 0.0023, '0.23%'], ['0.45', 0.0045, '0.45%'], ['0.85', 0.0085, '0.85%']])(
    'echoes %s%% as typed despite the float round trip',
    (_label, rate, expected) => {
      expect(formatThresholdPct(rate)).toBe(expected);
    },
  );

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

  it('refuses to report a mint latency when no qURL was minted', () => {
    const lines = report([round({ fileFail: 100, uploadMs: 250, mintMs: 1, totalMs: 300 })]).lines;
    expect(lines).toContain('Avg upload: 250ms, avg mint/round: n/a — all 100 mint attempt(s) failed');
    expect(lines.join('\n')).not.toMatch(/avg mint: \d/);
  });

  it('reports mint latency once qURLs are actually minted', () => {
    expect(report([round({ fileLinks: 100, uploadMs: 250, mintMs: 400, totalMs: 700 })]).lines)
      .toContain('Avg upload: 250ms, avg mint/round: 400ms');
  });

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
      'Uploads: 2 ok (2 initial + 0 re-upload)',
    ]);
  });

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

  it('fails, and says so, when no round completed', () => {
    const result = report([], 5);
    expect(result.failed).toBe(true);
    expect(result.lines[0]).toBe('Rounds: 0 completed, 5 failed');
    expect(result.lines).toContain('FAILED: no round completed');
    expect(result.lines.filter((l) => l.startsWith('FAILED:'))).toHaveLength(1);
  });

  it('fails when the run window was too short to attempt a round', () => {
    const result = report([], 0);
    expect(result.failed).toBe(true);
    expect(result.lines).toContain('FAILED: no round completed');
  });

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
