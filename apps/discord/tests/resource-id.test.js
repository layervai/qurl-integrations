const {
  hasSafeResourceIdShape,
  validateResourceId,
} = require('../src/utils/resource-id');

describe('resource ID transport guard', () => {
  it.each([undefined, '', 12345, '../qurls/x'])('rejects an unsafe shape: %p', (resourceId) => {
    expect(hasSafeResourceIdShape(resourceId)).toBe(false);
  });

  it('accepts 1024 characters and rejects 1025', () => {
    expect(hasSafeResourceIdShape('a'.repeat(1024))).toBe(true);
    expect(hasSafeResourceIdShape('a'.repeat(1025))).toBe(false);
  });

  it('bounds the rejected value included in its diagnostic', () => {
    const resourceId = 'z'.repeat(1025);

    expect(() => validateResourceId(resourceId)).toThrow(
      new Error(`Invalid resource ID format: ${'z'.repeat(64)}`),
    );
  });
});
