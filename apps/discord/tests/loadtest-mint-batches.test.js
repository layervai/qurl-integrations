const { planMintBatches, MINT_BATCH_SIZE } = require('../scripts/loadtest-standalone');

describe('bounded mint request plan', () => {
  test('the default hundred-link round makes ten bounded requests', () => {
    expect(MINT_BATCH_SIZE).toBe(10);
    expect(planMintBatches(100)).toEqual(Array.from({ length: 10 }, () => ({ size: 10 })));
  });

  test.each([
    [0, []], [-1, []], [1, [{ size: 1 }]], [10, [{ size: 10 }]],
    [11, [{ size: 10 }, { size: 1 }]],
    [25, [{ size: 10 }, { size: 10 }, { size: 5 }]],
  ])('plans %i links', (count, expected) => {
    expect(planMintBatches(count)).toEqual(expected);
  });

  test('a caller can select a smaller request size without changing the total', () => {
    expect(planMintBatches(8, 3)).toEqual([{ size: 3 }, { size: 3 }, { size: 2 }]);
  });
});
