
const { planMintBatches, TOKENS_PER_RESOURCE } = require('../scripts/loadtest-standalone');

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
  test('defaults to TOKENS_PER_RESOURCE when no pool depth is passed', () => {
    expect(planMintBatches(25)).toEqual(planMintBatches(25, TOKENS_PER_RESOURCE));
  });
});

describe('planMintBatches — the regression this guards', () => {
  test('the default --count 100 round plans 10 batches, 9 of them re-uploads', () => {
    const plan = planMintBatches(100);
    expect(plan).toHaveLength(10);
    expect(plan.filter((b) => b.reupload)).toHaveLength(9);
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
    [10, [{ size: 10, reupload: false }]],
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
  const counts = [1, 5, 9, 10, 11, 15, 19, 20, 21, 50, 99, 100, 101, 250];

  test.each(counts)('count %i matches the batcher loop shape', (count) => {
    expect(planMintBatches(count)).toEqual(batcherShape(count, TOKENS_PER_RESOURCE));
  });

  test.each([1, 2, 3, 7, 50])('pool depth %i keeps the shapes equal', (depth) => {
    for (const count of counts) {
      expect(planMintBatches(count, depth)).toEqual(batcherShape(count, depth));
    }
  });
});
