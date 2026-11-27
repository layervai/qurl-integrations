
const { planMintBatches, MINT_BATCH_SIZE } = require('../scripts/loadtest-standalone');

function batcherShape(recipientCount, tokensPerResource) {
  const shape = [];
  for (let i = 0; i < recipientCount; i += tokensPerResource) {
    shape.push({ size: Math.min(tokensPerResource, recipientCount - i), reupload: false });
  }
  return shape;
}

describe('planMintBatches — pool depth', () => {
  test('defaults to MINT_BATCH_SIZE when no pool depth is passed', () => {
    expect(planMintBatches(25)).toEqual(planMintBatches(25, MINT_BATCH_SIZE));
  });
});

describe('planMintBatches — the regression this guards', () => {
  test('the default --count 100 round plans 10 batches against one resource', () => {
    const plan = planMintBatches(100);
    expect(plan).toHaveLength(10);
    expect(plan.filter((b) => b.reupload)).toHaveLength(0);
    expect(plan.reduce((s, b) => s + b.size, 0)).toBe(100);
  });

  test('no batch re-uploads', () => {
    const plan = planMintBatches(100);
    expect(plan[0].reupload).toBe(false);
    expect(plan.every((b) => !b.reupload)).toBe(true);
  });

  test('no batch ever exceeds the token pool', () => {
    for (const count of [1, 9, 10, 11, 99, 100, 101, 1000]) {
      const plan = planMintBatches(count);
      expect(plan.every((b) => b.size >= 1 && b.size <= MINT_BATCH_SIZE)).toBe(true);
      expect(plan.reduce((s, b) => s + b.size, 0)).toBe(count);
    }
  });
});

describe('planMintBatches — boundaries', () => {
  test.each([
    [1, [{ size: 1, reupload: false }]],
    [9, [{ size: 9, reupload: false }]],
    [10, [{ size: 10, reupload: false }]],
    [11, [{ size: 10, reupload: false }, { size: 1, reupload: false }]],
    [20, [{ size: 10, reupload: false }, { size: 10, reupload: false }]],
  ])('count %i plans %j', (count, expected) => {
    expect(planMintBatches(count)).toEqual(expected);
  });

  test.each([0, -1, -10])('count %i plans nothing', (count) => {
    expect(planMintBatches(count)).toEqual([]);
  });
});

describe('planMintBatches — equivalence with mintLinksInBatches', () => {
  const counts = [1, 5, 9, 10, 11, 15, 19, 20, 21, 50, 99, 100, 101, 250];

  test.each(counts)('count %i matches the batcher loop shape', (count) => {
    expect(planMintBatches(count)).toEqual(batcherShape(count, MINT_BATCH_SIZE));
  });

  test.each([1, 2, 3, 7, 50])('pool depth %i keeps the shapes equal', (depth) => {
    for (const count of counts) {
      expect(planMintBatches(count, depth)).toEqual(batcherShape(count, depth));
    }
  });
});
