/**
 * Tests for planMintBatches in scripts/loadtest-standalone.js — the load test's
 * mirror of mintLinksInBatches (src/commands.js).
 *
 * Why this file exists: scripts/ is outside this app's jest
 * `collectCoverageFrom` (`src/**` only), so batching logic left inline in
 * runRound is enforced by nothing. The bug this guards against shipped exactly
 * that way — runRound minted every batch against one resource_id and never
 * re-uploaded, so a --count 100 round drained the initial 10-token pool on
 * batch 1 and took `quota_exceeded` for the other 9. It reported ok=10 fail=90
 * and issued 1 upload per round where a real 100-recipient send issues 10.
 *
 * That was invisible until PR #1168 fixed a positional-argument bug that made
 * mintLinks throw client-side before any request was issued.
 *
 * The equivalence these tests pin is against mintLinksInBatches' loop:
 *   for (i = 0; i < recipientCount; i += TOKENS_PER_RESOURCE)
 *     if (tokensUsed >= TOKENS_PER_RESOURCE && i > 0) reupload
 *     batchSize = min(TOKENS_PER_RESOURCE, recipientCount - i)
 *
 * Requiring the script does NOT run the load test: its CLI entry point is
 * behind `require.main === module`, matching tests/loadtest-target-guard.test.js.
 */

const { planMintBatches, TOKENS_PER_RESOURCE } = require('../scripts/loadtest-standalone');

// Transcription of mintLinksInBatches' loop, kept statement-for-statement:
// the `tokensUsed = 0` reset on re-upload and the `tokensUsed += batchSize`
// accumulation are written the way commands.js writes them, not collapsed,
// so this is an oracle rather than a third copy of planMintBatches' shortcut.
// planMintBatches reduces the guard to `i > 0`; keeping both halves here is
// what makes the equivalence tested instead of assumed.
//
// Deliberately NOT importing the batcher: require('../src/commands') throws
// at module load in a bare process (it pulls in ./store, which demands
// DDB_TABLE_PREFIX), so no test can drive the real loop directly from here.
function batcherShape(recipientCount, tokensPerResource) {
  const shape = [];
  let tokensUsed = 0;
  for (let i = 0; i < recipientCount; i += tokensPerResource) {
    let reupload = false;
    if (tokensUsed >= tokensPerResource && i > 0) {
      reupload = true;
      tokensUsed = 0;
    }
    const batchSize = Math.min(tokensPerResource, recipientCount - i);
    shape.push({ size: batchSize, reupload });
    tokensUsed += batchSize;
  }
  return shape;
}

describe('planMintBatches — pool depth', () => {
  // No "does the script's copy still match commands.js?" test here on purpose:
  // both now read src/constants.js, so there is no second copy to drift. The
  // assertion that would have gone here could only have compared the shared
  // constant against a hard-coded 10 — green whatever qurl-service's real cap
  // turns out to be, which is worse than no test at all.
  test('defaults to TOKENS_PER_RESOURCE when no pool depth is passed', () => {
    expect(planMintBatches(25)).toEqual(planMintBatches(25, TOKENS_PER_RESOURCE));
  });
});

describe('planMintBatches — the regression this guards', () => {
  test('the default --count 100 round plans 10 batches, 9 of them re-uploads', () => {
    const plan = planMintBatches(100);
    expect(plan).toHaveLength(10);
    expect(plan.filter((b) => b.reupload)).toHaveLength(9);
    // 1 initial upload + 9 re-uploads = the 10 uploads a real 100-recipient
    // send issues. The pre-fix loop issued 1.
    expect(plan.reduce((s, b) => s + b.size, 0)).toBe(100);
  });

  test('every batch after the first re-uploads; the first never does', () => {
    const plan = planMintBatches(100);
    expect(plan[0].reupload).toBe(false);
    expect(plan.slice(1).every((b) => b.reupload)).toBe(true);
  });

  test('no batch ever exceeds the token pool', () => {
    for (const count of [1, 9, 10, 11, 99, 100, 101, 1000]) {
      const plan = planMintBatches(count);
      expect(plan.every((b) => b.size >= 1 && b.size <= TOKENS_PER_RESOURCE)).toBe(true);
      expect(plan.reduce((s, b) => s + b.size, 0)).toBe(count);
    }
  });
});

describe('planMintBatches — boundaries', () => {
  test.each([
    [1, [{ size: 1, reupload: false }]],
    [9, [{ size: 9, reupload: false }]],
    // Exactly one pool: still a single batch, so still no re-upload. An
    // off-by-one in the guard would add a second, empty batch here.
    [10, [{ size: 10, reupload: false }]],
    // One past the pool: the short trailing batch is what forces a re-upload.
    [11, [{ size: 10, reupload: false }, { size: 1, reupload: true }]],
    [20, [{ size: 10, reupload: false }, { size: 10, reupload: true }]],
  ])('count %i plans %j', (count, expected) => {
    expect(planMintBatches(count)).toEqual(expected);
  });

  test.each([0, -1, -10])('count %i plans nothing', (count) => {
    expect(planMintBatches(count)).toEqual([]);
  });
});

describe('planMintBatches — equivalence with mintLinksInBatches', () => {
  // Sweep across pool boundaries: exact multiples, one-under, one-over.
  const counts = [1, 5, 9, 10, 11, 15, 19, 20, 21, 50, 99, 100, 101, 250];

  test.each(counts)('count %i matches the batcher loop shape', (count) => {
    expect(planMintBatches(count)).toEqual(batcherShape(count, TOKENS_PER_RESOURCE));
  });

  // The two guards collapse only because a short batch can appear last and
  // nowhere else. Re-run the sweep at other pool depths so the equivalence is
  // shown to be structural, not a coincidence of the number 10.
  // Depth 10 is deliberately absent: the sweep above already runs it, and
  // repeating it here would be the one depth that proves nothing.
  test.each([1, 2, 3, 7, 50])('pool depth %i keeps the shapes equal', (depth) => {
    for (const count of counts) {
      expect(planMintBatches(count, depth)).toEqual(batcherShape(count, depth));
    }
  });
});
