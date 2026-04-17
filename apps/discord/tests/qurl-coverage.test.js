/**
 * Additional qurl.js tests for 90%+ coverage.
 * Covers: getResourceStatus function (line 51).
 */

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
}));

const originalFetch = globalThis.fetch;

describe('QURL client — getResourceStatus (line 51)', () => {
  let qurl;

  beforeEach(() => {
    jest.resetModules();
    jest.mock('../src/config', () => ({
      QURL_API_KEY: 'test-api-key',
      QURL_ENDPOINT: 'https://api.test.local',
    }));
    jest.mock('../src/logger', () => ({
      info: jest.fn(),
      warn: jest.fn(),
      error: jest.fn(),
      debug: jest.fn(),
    }));
    qurl = require('../src/qurl');
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  it('sends GET request to /v1/qurls/:resourceId and returns data', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({
        data: {
          resource_id: 'res-123',
          qurls: [{ qurl_id: 'q1', use_count: 0, status: 'active', created_at: '2026-01-01' }],
        },
      }),
    });

    const result = await qurl.getResourceStatus('res-123');

    expect(globalThis.fetch).toHaveBeenCalledTimes(1);
    const [url, opts] = globalThis.fetch.mock.calls[0];
    expect(url).toBe('https://api.test.local/v1/qurls/res-123');
    expect(opts.method).toBe('GET');
    expect(opts.headers.Authorization).toBe('Bearer test-api-key');
    expect(result.resource_id).toBe('res-123');
    expect(result.qurls).toHaveLength(1);
  });

  it('throws on 404 API error', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue({
      ok: false,
      status: 404,
      text: async () => 'Not Found',
    });

    await expect(qurl.getResourceStatus('bad-id'))
      .rejects.toThrow(/QURL API GET.*failed.*404/);
  });

  it('returns null for 204 response (no content)', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue({
      ok: true,
      status: 204,
    });

    const result = await qurl.getResourceStatus('res-empty');
    expect(result).toBeNull();
  });

  it('returns envelope directly when .data is absent', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ resource_id: 'res-direct', qurls: [] }),
    });

    const result = await qurl.getResourceStatus('res-direct');
    expect(result.resource_id).toBe('res-direct');
    expect(result.qurls).toEqual([]);
  });
});

describe('validateResourceId', () => {
  let qurl;

  beforeEach(() => {
    jest.resetModules();
    jest.mock('../src/config', () => ({
      QURL_API_KEY: 'test-api-key',
      QURL_ENDPOINT: 'https://api.test.local',
    }));
    jest.mock('../src/logger', () => ({
      info: jest.fn(),
      warn: jest.fn(),
      error: jest.fn(),
      debug: jest.fn(),
    }));
    qurl = require('../src/qurl');
  });

  it('rejects path traversal attempts', () => {
    expect(() => qurl.validateResourceId('../../../etc/passwd')).toThrow();
    expect(() => qurl.validateResourceId('res%00id')).toThrow();
    expect(() => qurl.validateResourceId('res id spaces')).toThrow();
    expect(() => qurl.validateResourceId('')).toThrow();
    expect(() => qurl.validateResourceId(null)).toThrow();
  });

  it('accepts valid resource IDs', () => {
    expect(() => qurl.validateResourceId('r_abc123')).not.toThrow();
    expect(() => qurl.validateResourceId('res-with-dashes')).not.toThrow();
  });
});
