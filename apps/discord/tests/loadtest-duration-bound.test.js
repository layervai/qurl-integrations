/**
 * Tests for --duration being a HARD bound in scripts/loadtest-standalone.js.
 *
 * The defect: the deadline was consulted only between rounds, in main()'s
 * `while (Date.now() < endTime)`. A round in progress always ran its full
 * --count first, so `--count 20000 --duration 60` issued 2000 sequential mint
 * batches plus 20000 sequential location creates — potentially hours of
 * traffic against a target — before the 60s bound was next looked at. The
 * per-request timeouts bound each CALL, not the round: 30s for a mint batch,
 * and up to three attempts of 30s for a location create.
 *
 * These tests are the only signal on it. runRound() and main() cannot be
 * called from a suite (they run the load test), and scripts/ sits outside this
 * app's jest `collectCoverageFrom`, so the loops themselves are unenforced.
 * The rule is therefore a pure function (shouldStopNow) and the reporting is a
 * pure function (runReport), matching how targetGuardReport and
 * parsePositiveInt are covered — with a static check below for the one thing
 * neither can reach: that the loops actually consult the predicate.
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
  shouldStopNow,
  runReport,
  roundReportLine,
  DEFAULT_MAX_FAIL_RATE_PCT,
} = require('../scripts/loadtest-standalone');

/**
 * A round's counters, defaulted to a round that did nothing. Mirrors the
 * helper in loadtest-reporting.test.js, plus `partial`.
 */
const round = (over = {}) => ({
  fileLinks: 0, fileFail: 0, locLinks: 0, locFail: 0,
  uploadMs: 0, mintMs: 0, locMs: 0, totalMs: 0, partial: false, ...over,
});

const report = (over = {}) => runReport({
  allResults: [],
  roundsAttempted: 0,
  maxFailRate: DEFAULT_MAX_FAIL_RATE_PCT / 100,
  ...over,
});

const lineStartingWith = (lines, prefix) => lines.find((l) => l.startsWith(prefix));

describe('shouldStopNow — one predicate for the clock and the signal', () => {
  // The reason this is one function rather than two conditions in eight loop
  // headers: both reasons mean "issue no further creates", they arrive in
  // either order, and a loop consulting only one keeps minting for the other.
  it('runs on while the clock has time and no sweep has started', () => {
    expect(shouldStopNow({ stopping: false, deadline: 1_000, now: 999 })).toBe(false);
  });

  it('stops when the deadline is reached exactly', () => {
    // `>=`, not `>`. At `now === deadline` the run's time is spent; letting
    // that tick through issues one more request than --duration allows, and
    // on the location leg that request can take 90s (three 30s attempts).
    expect(shouldStopNow({ stopping: false, deadline: 1_000, now: 1_000 })).toBe(true);
  });

  it('stops once the deadline has passed', () => {
    expect(shouldStopNow({ stopping: false, deadline: 1_000, now: 1_001 })).toBe(true);
  });

  it('stops on a signal even with time left on the clock', () => {
    // The pre-existing `stopping` behaviour, which this predicate absorbed
    // rather than replaced — a reclaim sweep must not race rounds that keep
    // appending ids behind it.
    expect(shouldStopNow({ stopping: true, deadline: 1_000, now: 0 })).toBe(true);
  });

  it('stops on a signal past the deadline', () => {
    expect(shouldStopNow({ stopping: true, deadline: 1_000, now: 5_000 })).toBe(true);
  });

  it('never stops on the clock before a run sets a deadline', () => {
    // The module-load default is Infinity. --reclaim never sets a deadline, so
    // a finite default here would make its sweep read as out of time.
    expect(shouldStopNow({ stopping: false, deadline: Infinity, now: 8.64e15 })).toBe(false);
  });
});

describe('runReport — a truncated round counts in totals, not in averages', () => {
  // The cost of cutting a round short, and the reason (b) — refusing at
  // startup — was considered instead: a partial round did less work in less
  // time, so averaging it in reports a figure no complete round ever took.
  it('excludes the cut-short round from Avg round time', () => {
    const { lines } = report({
      allResults: [
        round({ fileLinks: 100, uploadMs: 200, mintMs: 4_000, totalMs: 10_000 }),
        round({ fileLinks: 100, uploadMs: 200, mintMs: 4_000, totalMs: 10_000 }),
        round({ fileLinks: 7, uploadMs: 200, mintMs: 280, totalMs: 700, partial: true }),
      ],
      roundsAttempted: 3,
    });
    // Mean of the two complete rounds is 10.0s. Including the truncated round
    // would report 6.9s — faster than any round actually ran.
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
    // It really did mint those 7 and fail those 3 — excluding them would
    // under-report the load actually placed on the target.
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
    // Says which way each figure treats it, because the totals and the
    // averages deliberately disagree about this round.
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
    // Backward compatibility with round shapes built before this flag existed
    // — including the fixtures in loadtest-reporting.test.js, which omit it.
    // `undefined` must read as "not truncated", never as "unknown".
    const { lines } = report({
      allResults: [{ fileLinks: 100, fileFail: 0, locLinks: 0, locFail: 0, uploadMs: 200, mintMs: 4_000, locMs: 0, totalMs: 10_000 }],
      roundsAttempted: 1,
    });
    expect(lineStartingWith(lines, 'Avg round time')).toBe('Avg round time: 10.0s');
    expect(lineStartingWith(lines, 'Rounds cut short:')).toBeUndefined();
  });

  it('reports no round time rather than a truncated one when every round was cut short', () => {
    // A `--duration` shorter than one round. Printing the mean of truncated
    // rounds under "Avg round time" would state a round duration that is
    // purely an artefact of where the clock landed.
    const { lines } = report({
      allResults: [round({ fileLinks: 7, totalMs: 700, partial: true })],
      roundsAttempted: 1,
    });
    expect(lineStartingWith(lines, 'Avg round time')).toBe('Avg round time: n/a — every round was cut short');
  });

  it('excludes the cut-short round from avg mint/round', () => {
    const { lines } = report({
      allResults: [
        round({ fileLinks: 100, uploadMs: 200, mintMs: 4_000, totalMs: 10_000 }),
        round({ fileLinks: 7, uploadMs: 200, mintMs: 280, totalMs: 700, partial: true }),
      ],
      roundsAttempted: 2,
    });
    // mintMs is a per-ROUND figure, so a round cut off mid-plan drags it
    // toward however far the clock let it get.
    expect(lineStartingWith(lines, 'Avg upload:')).toBe('Avg upload: 200ms, avg mint/round: 4000ms');
  });

  it('blames the truncation, not a failure, when every minting round was cut short', () => {
    // The note that would otherwise be reached here is 'no mint was attempted'
    // — plainly false, since this round minted 7. The other, 'all N mint
    // attempt(s) failed', blames a failure for what was the clock.
    const { lines } = report({
      allResults: [round({ fileLinks: 7, uploadMs: 200, mintMs: 280, totalMs: 700, partial: true })],
      roundsAttempted: 1,
    });
    expect(lineStartingWith(lines, 'Avg upload:'))
      .toBe('Avg upload: 200ms, avg mint/round: n/a — every round that minted was cut short');
  });

  it('still blames the failures when nothing minted and nothing was truncated', () => {
    // The pre-existing note has to survive the new branch above it.
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
    // Tailing the output is how a soak is watched. Without this, a round
    // reporting ok=7 among rounds reporting ok=100 reads as a catastrophic
    // failure rather than as the clock running out.
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

describe('loadtest duration bound — static checks on loops no test can reach', () => {
  // runRound's loops are the entire point of this change and are unreachable
  // from a suite. shouldStopNow being correct proves nothing about them
  // consulting it, so the call sites are pinned structurally — the same reason
  // loadtest-silent-failure.test.js pins `callsNamed('readFlag')`.
  const source = fs.readFileSync(
    path.join(__dirname, '..', 'scripts', 'loadtest-standalone.js'),
    'utf8',
  );
  const ast = parser.parse(source, { sourceType: 'unambiguous' });

  /** Every `shouldStop()` call inside the named function declaration. */
  const shouldStopCallsIn = (fnName) => {
    let count = 0;
    traverse(ast, {
      FunctionDeclaration(p) {
        if (p.node.id?.name !== fnName) return;
        p.traverse({
          CallExpression(c) {
            if (c.node.callee.type === 'Identifier' && c.node.callee.name === 'shouldStop') count++;
          },
        });
      },
    });
    return count;
  };

  it('consults the predicate at all four stop points inside runRound', () => {
    // Both leg entries and both per-recipient loops. Dropping any one of them
    // restores the unbounded behaviour for that leg while every test above
    // stays green — the file leg's batch loop is the one that used to issue
    // ceil(COUNT/10) sequential requests regardless of the clock.
    expect(shouldStopCallsIn('runRound')).toBe(4);
  });

  it('consults the predicate in main rather than comparing the clock inline', () => {
    // The round loop and the inter-round sleep. A `Date.now() < endTime`
    // rewritten inline here would drop the signal half of the predicate and
    // reinstate the race the `stopping` flag was added to close.
    expect(shouldStopCallsIn('main')).toBe(2);
  });

  it('keeps the raw stopping flag out of the loops', () => {
    // Every loop reads the predicate, not the flag. A `&& !stopping` added
    // back to a loop header is a second, competing condition that a deadline
    // would not reach — exactly the shape this change removed.
    const runRoundSource = source.slice(
      source.indexOf('async function runRound'),
      source.indexOf('async function main'),
    );
    expect(runRoundSource).not.toContain('!stopping');
  });
});
