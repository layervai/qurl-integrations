const {
  hasSafeResourceIdShape,
  LEGACY_RESOURCE_ID_PREFIX,
  maskResourceIdPath,
  qurlPath,
  resourceIdLogRef,
  resourcePath,
  validateResourceId,
} = require('../src/utils/resource-id');
const {
  PUBLIC_KEY_RESOURCE_ID,
  CRID_RESOURCE_ID,
} = require('./helpers/qurl-fixtures');

describe('resource ID transport guard', () => {
  it('pins the retired private-ID prefix used by reclaim diagnostics', () => {
    expect(LEGACY_RESOURCE_ID_PREFIX).toBe('r_');
  });

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

  it.each([
    '',
    'z'.repeat(1025),
    'bad\nresource',
    null,
    Object.create(null),
    'at_sensitive-access-token',
  ])('rejects without echoing a potentially sensitive value: %p', (resourceId) => {
    expect(() => validateResourceId(resourceId))
      .toThrow(new Error('Invalid resource ID format'));
  });

  it('keeps the bearer-token prefix case-exact', () => {
    expect(() => validateResourceId('AT_public-opaque-id')).not.toThrow();
  });

  it('derives a stable non-echoing correlation reference', () => {
    const sensitive = 'at_sensitive-access-token';
    const ref = resourceIdLogRef(sensitive);

    expect(ref).toMatch(/^sha256:[a-f0-9]{12}$/);
    expect(ref).toBe(resourceIdLogRef(sensitive));
    expect(ref).not.toContain(sensitive);
    expect(resourceIdLogRef(null)).toBe('<null>');
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
