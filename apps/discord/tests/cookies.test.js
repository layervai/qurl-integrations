const { timingSafeStringEqual } = require('../src/utils/cookies');

describe('utils/cookies', () => {
  describe('timingSafeStringEqual', () => {
    it.each([
      ['same', 'same', true],
      ['same', 'different', false],
      // CSRF bindings must fail closed when both inputs are absent/empty.
      ['', '', false],
      ['é', 'é', true],
      ['é', 'e', false],
      [null, 'same', false],
      ['same', undefined, false],
    ])('compares %p and %p as %p', (left, right, expected) => {
      expect(timingSafeStringEqual(left, right)).toBe(expected);
    });
  });
});
