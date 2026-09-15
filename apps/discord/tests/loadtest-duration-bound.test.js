
const fs = require('fs');
const path = require('path');
const parser = require('@babel/parser');
const traverseModule = require('@babel/traverse');
const traverse = traverseModule.default || traverseModule;

const {
  shouldStopNow,
  runReport,
  roundReportLine,
  DEFAULT_MAX_FAIL_RATE_PCT,
} = require('../scripts/loadtest-standalone');

const round = (over = {}) => ({
  fileLinks: 0, fileFail: 0, locLinks: 0, locFail: 0,
  uploadMs: 0, mintMs: 0, locMs: 0, totalMs: 0,
  partial: false, mintPartial: false, ...over,
});

const report = (over = {}) => runReport({
  allResults: [],
  roundsAttempted: 0,
  maxFailRate: DEFAULT_MAX_FAIL_RATE_PCT / 100,
  ...over,
});

const lineStartingWith = (lines, prefix) => lines.find((l) => l.startsWith(prefix));

describe('shouldStopNow — one predicate for the clock and the signal', () => {
  it('runs on while the clock has time and no sweep has started', () => {
    expect(shouldStopNow({ stopping: false, deadline: 1_000, now: 999 })).toBe(false);
  });

  it('stops when the deadline is reached exactly', () => {
    expect(shouldStopNow({ stopping: false, deadline: 1_000, now: 1_000 })).toBe(true);
  });

  it('stops once the deadline has passed', () => {
    expect(shouldStopNow({ stopping: false, deadline: 1_000, now: 1_001 })).toBe(true);
  });

  it('stops on a signal even with time left on the clock', () => {
    expect(shouldStopNow({ stopping: true, deadline: 1_000, now: 0 })).toBe(true);
  });

  it('stops on a signal past the deadline', () => {
    expect(shouldStopNow({ stopping: true, deadline: 1_000, now: 5_000 })).toBe(true);
  });

  it('never stops on the clock before a run sets a deadline', () => {
    expect(shouldStopNow({ stopping: false, deadline: Infinity, now: 8.64e15 })).toBe(false);
  });
});

describe('runReport — a truncated round counts in totals, not in averages', () => {
  it('excludes the cut-short round from Avg round time', () => {
    const { lines } = report({
      allResults: [
        round({ fileLinks: 100, uploadMs: 200, mintMs: 4_000, totalMs: 10_000 }),
        round({ fileLinks: 100, uploadMs: 200, mintMs: 4_000, totalMs: 10_000 }),
        round({ fileLinks: 7, uploadMs: 200, mintMs: 280, totalMs: 700, partial: true }),
      ],
      roundsAttempted: 3,
    });
    expect(lineStartingWith(lines, 'Avg round time')).toBe('Avg round time: 10.0s');
  });

  it('keeps the cut-short round in the totals', () => {
    const { lines } = report({
      allResults: [
        round({ fileLinks: 100, totalMs: 10_000 }),
        round({ fileLinks: 7, fileFail: 3, totalMs: 700, partial: true }),
      ],
      roundsAttempted: 2,
    });
    expect(lines).toContain('Total links minted: 107');
    expect(lines).toContain('Total link failures: 3');
    expect(lineStartingWith(lines, 'Rounds:')).toBe('Rounds: 2');
  });

  it('names the truncation instead of leaving it to be inferred', () => {
    const { lines } = report({
      allResults: [round({ fileLinks: 100, totalMs: 10_000 }), round({ fileLinks: 7, totalMs: 700, partial: true })],
      roundsAttempted: 2,
    });
    const cut = lineStartingWith(lines, 'Rounds cut short:');
    expect(cut).toContain('Rounds cut short: 1');
    expect(cut).toContain('counted in totals, excluded from per-round averages');
  });

  it('says nothing about truncation when no round was cut short', () => {
    const { lines } = report({
      allResults: [round({ fileLinks: 100, totalMs: 10_000 })],
      roundsAttempted: 1,
    });
    expect(lineStartingWith(lines, 'Rounds cut short:')).toBeUndefined();
  });

  it('treats a round object with no partial field as complete', () => {
    const { lines } = report({
      allResults: [{ fileLinks: 100, fileFail: 0, locLinks: 0, locFail: 0, uploadMs: 200, mintMs: 4_000, locMs: 0, totalMs: 10_000 }],
      roundsAttempted: 1,
    });
    expect(lineStartingWith(lines, 'Avg round time')).toBe('Avg round time: 10.0s');
    expect(lineStartingWith(lines, 'Rounds cut short:')).toBeUndefined();
  });

  it('reports no round time rather than a truncated one when every round was cut short', () => {
    const { lines } = report({
      allResults: [round({ fileLinks: 7, totalMs: 700, partial: true })],
      roundsAttempted: 1,
    });
    expect(lineStartingWith(lines, 'Avg round time')).toBe('Avg round time: n/a — every round was cut short');
  });

  it('excludes a round whose mint leg was cut short from avg mint/round', () => {
    const { lines } = report({
      allResults: [
        round({ fileLinks: 100, uploadMs: 200, mintMs: 4_000, totalMs: 10_000 }),
        round({ fileLinks: 7, uploadMs: 200, mintMs: 280, totalMs: 700, partial: true, mintPartial: true }),
      ],
      roundsAttempted: 2,
    });
    expect(lineStartingWith(lines, 'Avg upload:')).toBe('Avg upload: 200ms, avg mint/round: 4000ms');
  });

  it('keeps the mint sample when only the location leg was cut short', () => {
    const { lines } = report({
      allResults: [
        round({ fileLinks: 100, locLinks: 100, uploadMs: 200, mintMs: 4_000, totalMs: 10_000 }),
        round({
          fileLinks: 100, locLinks: 12, uploadMs: 200, mintMs: 4_100, totalMs: 6_000,
          partial: true, mintPartial: false,
        }),
      ],
      roundsAttempted: 2,
    });
    expect(lineStartingWith(lines, 'Avg upload:')).toBe('Avg upload: 200ms, avg mint/round: 4050ms');
    expect(lineStartingWith(lines, 'Avg round time')).toBe('Avg round time: 10.0s');
  });

  it('reports the mint average from a lone round cut only in its location leg', () => {
    const { lines } = report({
      allResults: [round({
        fileLinks: 100, locLinks: 3, uploadMs: 200, mintMs: 4_000, totalMs: 6_000,
        partial: true, mintPartial: false,
      })],
      roundsAttempted: 1,
    });
    expect(lineStartingWith(lines, 'Avg upload:')).toBe('Avg upload: 200ms, avg mint/round: 4000ms');
  });

  it('blames the truncation, not a failure, when every minting round was cut short', () => {
    const { lines } = report({
      allResults: [round({
        fileLinks: 7, uploadMs: 200, mintMs: 280, totalMs: 700, partial: true, mintPartial: true,
      })],
      roundsAttempted: 1,
    });
    expect(lineStartingWith(lines, 'Avg upload:'))
      .toBe('Avg upload: 200ms, avg mint/round: n/a — every round that minted was cut short');
  });

  it('still blames the failures when nothing minted and nothing was truncated', () => {
    const { lines } = report({
      allResults: [round({ fileFail: 100, uploadMs: 200, mintMs: 50, totalMs: 700 })],
      roundsAttempted: 1,
    });
    expect(lineStartingWith(lines, 'Avg upload:'))
      .toBe('Avg upload: 200ms, avg mint/round: n/a — all 100 mint attempt(s) failed');
  });
});

describe('roundReportLine — a cut-short round says so on its own line', () => {
  it('marks the round that was stopped mid-way', () => {
    const line = roundReportLine({
      elapsed: '60',
      round: 3,
      results: round({ fileLinks: 7, uploadMs: 200, mintMs: 280, totalMs: 700, partial: true }),
    });
    expect(line).toContain('Round 3 (cut short):');
    expect(line).toContain('ok=7');
  });

  it('leaves a complete round unmarked', () => {
    const line = roundReportLine({
      elapsed: '60',
      round: 3,
      results: round({ fileLinks: 100, uploadMs: 200, mintMs: 4_000, totalMs: 10_000 }),
    });
    expect(line).toContain('Round 3:');
    expect(line).not.toContain('cut short');
  });
});

describe('loadtest duration bound — static checks that name the dropped site', () => {
  const source = fs.readFileSync(
    path.join(__dirname, '..', 'scripts', 'loadtest-standalone.js'),
    'utf8',
  );
  const ast = parser.parse(source, { sourceType: 'unambiguous' });

  const countIn = (fnName, nodeType, matches) => {
    let count = 0;
    let found = false;
    const tally = (path) => {
      found = true;
      path.traverse({ [nodeType](c) { if (matches(c.node)) count++; } });
    };
    traverse(ast, {
      FunctionDeclaration(p) {
        if (p.node.id?.name === fnName) tally(p);
      },
      VariableDeclarator(p) {
        if (p.node.id.type !== 'Identifier' || p.node.id.name !== fnName) return;
        const init = p.node.init;
        if (init?.type === 'ArrowFunctionExpression' || init?.type === 'FunctionExpression') {
          tally(p.get('init'));
        }
      },
    });
    expect({ fnName, found }).toEqual({ fnName, found: true });
    return count;
  };

  const shouldStopCallsIn = (fnName) => countIn(
    fnName,
    'CallExpression',
    (n) => n.callee.type === 'Identifier' && n.callee.name === 'shouldStop',
  );

  it('consults the predicate at all four stop points inside runRound', () => {
    expect(shouldStopCallsIn('runRound')).toBe(4);
  });

  const truthAssignmentsIn = (fnName, field) => countIn(fnName, 'AssignmentExpression', (n) => {
    const { left, right } = n;
    if (left.type !== 'MemberExpression') return false;
    if (left.object.type !== 'Identifier' || left.object.name !== 'results') return false;
    if (left.property.type !== 'Identifier' || left.property.name !== field) return false;
    return right.type === 'BooleanLiteral' && right.value === true;
  });

  it('marks the round partial at every one of its four stop points', () => {
    expect(truthAssignmentsIn('runRound', 'partial')).toBe(4);
  });

  it('marks the mint leg partial at both of its stop points, and only those', () => {
    expect(truthAssignmentsIn('runRound', 'mintPartial')).toBe(2);
  });

  it('consults the predicate in main rather than comparing the clock inline', () => {
    expect(shouldStopCallsIn('main')).toBe(2);
  });

  it('keeps the raw stopping flag out of the loops', () => {
    expect(countIn('runRound', 'Identifier', (n) => n.name === 'stopping')).toBe(0);
  });
});
