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

describe('qURL client — getResourceStatus (line 51)', () => {
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
      .rejects.toThrow(/qURL API GET.*failed.*404/);
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

describe('qURL client — retry logic on transient failures', () => {
  let qurl;
  beforeEach(() => {
    jest.resetModules();
    jest.mock('../src/config', () => ({
      QURL_API_KEY: 'test-api-key',
      QURL_ENDPOINT: 'https://api.test.local',
    }));
    jest.mock('../src/logger', () => ({
      info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(),
    }));
    qurl = require('../src/qurl');
  });
  afterEach(() => { globalThis.fetch = originalFetch; });

  it('retries on 503 and succeeds on the next attempt', async () => {
    globalThis.fetch = jest.fn()
      .mockResolvedValueOnce({ ok: false, status: 503, text: async () => '' })
      .mockResolvedValueOnce({ ok: true, status: 200, json: async () => ({ data: { ok: true } }) });
    const r = await qurl.getResourceStatus('res-retry');
    expect(r.ok).toBe(true);
    expect(globalThis.fetch).toHaveBeenCalledTimes(2);
  });

  it('does NOT retry on 401', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue({ ok: false, status: 401, text: async () => '' });
    await expect(qurl.getResourceStatus('res-auth')).rejects.toThrow(/401/);
    expect(globalThis.fetch).toHaveBeenCalledTimes(1);
  });

  it('gives up after 3 attempts on persistent 503', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue({ ok: false, status: 503, text: async () => '' });
    await expect(qurl.getResourceStatus('res-down')).rejects.toThrow(/503/);
    expect(globalThis.fetch).toHaveBeenCalledTimes(3);
  });

  it('retries on network error then succeeds', async () => {
    globalThis.fetch = jest.fn()
      .mockRejectedValueOnce(new Error('ECONNRESET'))
      .mockResolvedValueOnce({ ok: true, status: 200, json: async () => ({ data: { ok: true } }) });
    const r = await qurl.getResourceStatus('res-net');
    expect(r.ok).toBe(true);
    expect(globalThis.fetch).toHaveBeenCalledTimes(2);
  });

  it('throws after persistent network errors', async () => {
    globalThis.fetch = jest.fn().mockRejectedValue(new Error('ECONNRESET'));
    await expect(qurl.getResourceStatus('res-netdown')).rejects.toThrow(/ECONNRESET/);
    expect(globalThis.fetch).toHaveBeenCalledTimes(3);
  });

  it('retries on 429', async () => {
    globalThis.fetch = jest.fn()
      .mockResolvedValueOnce({ ok: false, status: 429, text: async () => '' })
      .mockResolvedValueOnce({ ok: true, status: 200, json: async () => ({ data: {} }) });
    await qurl.getResourceStatus('res-429');
    expect(globalThis.fetch).toHaveBeenCalledTimes(2);
  });
});

describe('qURL client — createOneTimeLink happy path', () => {
  let qurl;
  beforeEach(() => {
    jest.resetModules();
    jest.doMock('../src/config', () => ({
      QURL_API_KEY: 'test-api-key',
      QURL_ENDPOINT: 'https://api.test.local',
    }));
    jest.doMock('../src/logger', () => ({
      info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(),
    }));
    jest.doMock('dns', () => ({
      promises: { lookup: jest.fn().mockResolvedValue([{ address: '93.184.216.34', family: 4 }]) },
    }));
    qurl = require('../src/qurl');
  });
  afterEach(() => { globalThis.fetch = originalFetch; });

  it('creates a link for a public URL that passes DNS resolution', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue({
      ok: true, status: 200,
      json: async () => ({ data: { resource_id: 'r1', qurl_link: 'https://q.link/abc' } }),
    });
    const result = await qurl.createOneTimeLink('https://example.com/file', '1h', 'desc');
    expect(result.resource_id).toBe('r1');
  });

  it('rejects when DNS lookup fails', async () => {
    jest.resetModules();
    jest.doMock('../src/config', () => ({ QURL_API_KEY: 'k', QURL_ENDPOINT: 'https://api.test.local' }));
    jest.doMock('../src/logger', () => ({ info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn() }));
    jest.doMock('dns', () => ({
      promises: { lookup: jest.fn().mockRejectedValue(Object.assign(new Error('not found'), { code: 'ENOTFOUND' })) },
    }));
    const q = require('../src/qurl');
    await expect(q.createOneTimeLink('https://nowhere.example/file', '1h', 'd'))
      .rejects.toThrow(/resolved/);
  });
});
