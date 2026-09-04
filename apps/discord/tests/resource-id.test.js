const {
  hasSafeResourceIdShape,
  maskResourceIdPath,
  qurlPath,
  resourcePath,
  validateResourceId,
} = require('../src/utils/resource-id');
const {
  PUBLIC_KEY_RESOURCE_ID,
  CRID_RESOURCE_ID,
} = require('./helpers/qurl-fixtures');

describe('resource ID transport guard', () => {
  it.each([undefined, null, '', 12345, '../qurls/x'])('rejects an unsafe shape: %p', (resourceId) => {
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
      new Error(`Invalid resource ID format: "${'z'.repeat(64)}" (len=1025)`),
    );
  });

  it('escapes control characters in rejected string diagnostics', () => {
    expect(() => validateResourceId('bad\nresource'))
      .toThrow(new Error('Invalid resource ID format: "bad\\nresource"'));
  });

  it('labels null and unexpected objects without stringifying them', () => {
    expect(() => validateResourceId(null))
      .toThrow(new Error('Invalid resource ID format: <null>'));
    expect(() => validateResourceId(Object.create(null)))
      .toThrow(new Error('Invalid resource ID format: <object>'));
  });

  it('builds both deliberately distinct resource-ID route families', () => {
    expect(resourcePath(CRID_RESOURCE_ID)).toBe(`/resources/${CRID_RESOURCE_ID}`);
    expect(qurlPath(CRID_RESOURCE_ID)).toBe(`/qurls/${CRID_RESOURCE_ID}`);
  });

  it.each([
    ['resourcePath', resourcePath],
    ['qurlPath', qurlPath],
  ])('%s validates structurally', (_name, buildPath) => {
    expect(() => buildPath('../qurls/x')).toThrow(/Invalid resource ID format/);
  });

  it('masks every resource-ID route and handles unusual causes', () => {
    const path = resourcePath(CRID_RESOURCE_ID);
    const message = `first ${path} failed; second ${qurlPath('other-id')} failed`;

    expect(maskResourceIdPath(message))
      .toBe('first /resources/<id> failed; second /qurls/<id> failed');
    expect(maskResourceIdPath('network request failed')).toBe('network request failed');
    expect(maskResourceIdPath('')).toBe('<empty>');
    expect(maskResourceIdPath(undefined)).toBe('<undefined>');
    expect(maskResourceIdPath(Object.create(null))).toBe('<object>');
  });

  it.each([
    'DELETE /resources/',
    'DELETE /resources/ failed',
    'GET /qurls/',
    'GET /qurls/ failed',
  ])('leaves an empty route tail unchanged without looping: %s', (message) => {
    expect(maskResourceIdPath(message)).toBe(message);
  });

  it('masks only the ID segment instead of swallowing a path suffix', () => {
    expect(maskResourceIdPath('DELETE /resources/abc-123/qurls failed'))
      .toBe('DELETE /resources/<id>/qurls failed');
  });
});
