const {
  hasSafeResourceIdShape,
  maskResourceIdPath,
  resourcePath,
  validateResourceId,
} = require('../src/utils/resource-id');
const {
  PUBLIC_KEY_RESOURCE_ID,
  CRID_RESOURCE_ID,
} = require('./helpers/qurl-fixtures');

describe('resource ID transport guard', () => {
  it.each([undefined, '', 12345, '../qurls/x'])('rejects an unsafe shape: %p', (resourceId) => {
    expect(hasSafeResourceIdShape(resourceId)).toBe(false);
  });

  it('accepts 1024 characters and rejects 1025', () => {
    expect(hasSafeResourceIdShape('a'.repeat(1024))).toBe(true);
    expect(hasSafeResourceIdShape('a'.repeat(1025))).toBe(false);
  });

  it('accepts the real public resource ID shapes', () => {
    expect(hasSafeResourceIdShape(PUBLIC_KEY_RESOURCE_ID)).toBe(true);
    expect(hasSafeResourceIdShape(CRID_RESOURCE_ID)).toBe(true);
  });

  it('bounds the rejected value included in its diagnostic', () => {
    const resourceId = 'z'.repeat(1025);

    expect(() => validateResourceId(resourceId)).toThrow(
      new Error(`Invalid resource ID format: ${'z'.repeat(64)}`),
    );
  });

  it('does not stringify unexpected objects in diagnostics', () => {
    expect(() => validateResourceId(Object.create(null)))
      .toThrow(new Error('Invalid resource ID format: <object>'));
  });

  it('builds and masks resource paths from the shared route contract', () => {
    const path = resourcePath(CRID_RESOURCE_ID);

    expect(path).toBe(`/resources/${CRID_RESOURCE_ID}`);
    expect(maskResourceIdPath(`qURL API DELETE ${path} failed (401)`))
      .toBe('qURL API DELETE /resources/<id> failed (401)');
    expect(maskResourceIdPath('network request failed')).toBe('network request failed');
  });
});
