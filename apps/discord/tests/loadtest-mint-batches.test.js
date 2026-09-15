const { planMintBatches } = require('../scripts/loadtest-standalone');

describe('planMintBatches — bounded requests on one resource', () => {
  test.each([
    [1, [{ size: 1 }]],
    [10, [{ size: 10 }]],
    [11, [{ size: 10 }, { size: 1 }]],
    [20, [{ size: 10 }, { size: 10 }]],
  ])('count %i plans %j', (count, expected) => {
    expect(planMintBatches(count)).toEqual(expected);
  });

  test('supports more than one hundred qURLs while preserving the request ceiling', () => {
    const plan = planMintBatches(101);
    expect(plan).toHaveLength(11);
    expect(plan.slice(0, 10)).toEqual(Array(10).fill({ size: 10 }));
    expect(plan[10]).toEqual({ size: 1 });
  });

  test.each([0, -1, -10])('count %i plans nothing', count => {
    expect(planMintBatches(count)).toEqual([]);
  });
});
