/**
 * qurl.js tests — the bot's qURL client, now backed by the @layervai/qurl SDK
 * (issue #830). These pin the behaviors qurl.js layers on top of the SDK: the
 * DEPENDENCY_AUTH_FAILURE audit emit on 401/403 (emit-once), error-body
 * redaction, and the 3-attempt retry budget. They also cover getResourceStatus
 * / createOneTimeLink happy paths.
 *
 * The fetch doubles below are richer than the pre-SDK client's: the SDK reads
 * `.json()` for both success and RFC-7807 error envelopes and `.headers.get()`
 * (Retry-After on 429/503), where the old hand-rolled client only read
 * `.text()`. `apiOk` / `apiError` build SDK-parseable doubles.
 */

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

const originalFetch = globalThis.fetch;

// Success Response double. The SDK unwraps the `{ data }` envelope, so wrap the
// payload the same way the API does. 204 callers pass `data: undefined`.
function apiOk(status, data) {
  return {
    ok: true,
    status,
    headers: { get: () => null },
    json: async () => (data === undefined ? {} : { data }),
  };
}

// RFC-7807 error Response double. `headers.get` is present because the SDK
// probes Retry-After on 429/503; `detail` can carry a (fake) sensitive body to
// prove redaction.
function apiError(status, { code = 'error', detail } = {}) {
  return {
    ok: false,
    status,
    headers: { get: () => null },
    json: async () => ({
      error: { status, code, title: `HTTP ${status}`, detail: detail ?? `HTTP ${status}` },
    }),
  };
}

describe('qURL client — getResourceStatus', () => {
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
      audit: jest.fn(),
    }));
    qurl = require('../src/qurl');
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  it('sends GET request to /v1/qurls/:resourceId and returns data', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue(
      apiOk(200, {
        resource_id: 'res-123',
        qurls: [{ qurl_id: 'q1', use_count: 0, status: 'active', created_at: '2026-01-01' }],
      }),
    );

    const result = await qurl.getResourceStatus('res-123');

    expect(globalThis.fetch).toHaveBeenCalledTimes(1);
    const [url, opts] = globalThis.fetch.mock.calls[0];
    expect(url).toBe('https://api.test.local/v1/qurls/res-123');
    expect(opts.method).toBe('GET');
    expect(opts.headers.Authorization).toBe('Bearer test-api-key');
    expect(result.resource_id).toBe('res-123');
    // The SDK renames the API's wire-format `qurls` field to `access_tokens`.
    expect(result.access_tokens).toHaveLength(1);
  });

  it('throws on 404 API error', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue(apiError(404, { code: 'not_found' }));

    await expect(qurl.getResourceStatus('bad-id')).rejects.toThrow(/404/);
  });

  it('throws on an unexpected 204 (a status read expects a body)', async () => {
    // The pre-SDK client returned null here; the SDK treats a body-less GET as
    // a contract violation and throws. A real status read always returns a body.
    globalThis.fetch = jest.fn().mockResolvedValue(apiOk(204, undefined));

    await expect(qurl.getResourceStatus('res-empty')).rejects.toThrow(/204|Unexpected/);
  });
});

describe('qURL client — retry + audit behavior', () => {
  let qurl;
  beforeEach(() => {
    jest.resetModules();
    jest.mock('../src/config', () => ({
      QURL_API_KEY: 'test-api-key',
      QURL_ENDPOINT: 'https://api.test.local',
    }));
    jest.mock('../src/logger', () => ({
      info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(), audit: jest.fn(),
    }));
    qurl = require('../src/qurl');
  });
  afterEach(() => { globalThis.fetch = originalFetch; });

  it('retries on 503 and succeeds on the next attempt', async () => {
    globalThis.fetch = jest.fn()
      .mockResolvedValueOnce(apiError(503))
      .mockResolvedValueOnce(apiOk(200, { ok: true }));
    const r = await qurl.getResourceStatus('res-retry');
    expect(r.ok).toBe(true);
    expect(globalThis.fetch).toHaveBeenCalledTimes(2);
  });

  it('does NOT retry on 401', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue(apiError(401));
    await expect(qurl.getResourceStatus('res-auth')).rejects.toThrow(/401/);
    expect(globalThis.fetch).toHaveBeenCalledTimes(1);
  });

  it('emits dependency_auth_failure audit event on 401 (Justin #193 §5)', async () => {
    const logger = require('../src/logger');
    const { AUDIT_EVENTS } = require('../src/constants');
    logger.audit.mockClear();
    globalThis.fetch = jest.fn().mockResolvedValue(apiError(401));
    await expect(qurl.getResourceStatus('res-auth-401')).rejects.toThrow(/401/);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.DEPENDENCY_AUTH_FAILURE,
      expect.objectContaining({
        dependency: 'qurl_service',
        status: 401,
        method: 'GET',
        path: '/qurls/res-auth-401',
      }),
    );
  });

  it('emits dependency_auth_failure audit event on 403 (Justin #193 §5)', async () => {
    const logger = require('../src/logger');
    const { AUDIT_EVENTS } = require('../src/constants');
    logger.audit.mockClear();
    globalThis.fetch = jest.fn().mockResolvedValue(apiError(403));
    await expect(qurl.getResourceStatus('res-auth-403')).rejects.toThrow(/403/);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.DEPENDENCY_AUTH_FAILURE,
      expect.objectContaining({ dependency: 'qurl_service', status: 403 }),
    );
  });

  it('does NOT emit dependency_auth_failure on retryable 503', async () => {
    // Pin that the audit event only fires on auth-class failures —
    // a transient 503 retry path stays quiet so the alarm count
    // reflects auth issues specifically, not generic API errors.
    const logger = require('../src/logger');
    const { AUDIT_EVENTS } = require('../src/constants');
    logger.audit.mockClear();
    globalThis.fetch = jest.fn().mockResolvedValue(apiError(503));
    await expect(qurl.getResourceStatus('res-503')).rejects.toThrow(/503/);
    const authCalls = logger.audit.mock.calls.filter(
      ([event]) => event === AUDIT_EVENTS.DEPENDENCY_AUTH_FAILURE,
    );
    expect(authCalls).toHaveLength(0);
  });

  it('does NOT emit dependency_auth_failure on non-auth 4xx (400, 404, 409)', async () => {
    // Pin the auth-only scope of the metric. A future match-everything
    // bug or status-list expansion would otherwise leak generic 4xx into
    // the auth-failure alarm and dilute its signal.
    const logger = require('../src/logger');
    const { AUDIT_EVENTS } = require('../src/constants');
    for (const status of [400, 404, 409]) {
      logger.audit.mockClear();
      globalThis.fetch = jest.fn().mockResolvedValue(apiError(status));
      await expect(qurl.getResourceStatus(`res-${status}`)).rejects.toThrow(new RegExp(String(status)));
      const authCalls = logger.audit.mock.calls.filter(
        ([event]) => event === AUDIT_EVENTS.DEPENDENCY_AUTH_FAILURE,
      );
      expect(authCalls).toHaveLength(0);
    }
  });

  it('emits dependency_auth_failure EXACTLY ONCE on 401 (emit-once invariant)', async () => {
    // EMIT-ONCE INVARIANT: the SDK keeps 401/403 out of its retryable set
    // ({429,502,503,504}), so the request fails after a single attempt and
    // the audit emit fires once — not once per attempt. If a future change
    // made auth-class statuses retryable, this assertion fails: the alarm
    // count would multiply on a single auth failure.
    const logger = require('../src/logger');
    const { AUDIT_EVENTS } = require('../src/constants');
    logger.audit.mockClear();
    globalThis.fetch = jest.fn().mockResolvedValue(apiError(401));
    await expect(qurl.getResourceStatus('res-once')).rejects.toThrow(/401/);
    expect(globalThis.fetch).toHaveBeenCalledTimes(1); // no retry on auth-class
    const authCalls = logger.audit.mock.calls.filter(
      ([event]) => event === AUDIT_EVENTS.DEPENDENCY_AUTH_FAILURE,
    );
    expect(authCalls).toHaveLength(1);
  });

  it('redacts the error body — logs status/code only, never the response body', async () => {
    // REDACTION INVARIANT: qurl.js's error breadcrumb must carry status (+ the
    // short error code) and NOTHING from the body. A qURL error body can echo
    // request headers or tokens, so even with the SDK error's `detail`
    // available, we never log it. Pin that a token planted in the body never
    // reaches logger.debug, while the status still does.
    const logger = require('../src/logger');
    logger.debug.mockClear();
    // A clearly-fake stand-in for a body that echoes a token — not a real key
    // pattern, so secret scanners don't flag the fixture.
    const SECRET = 'sensitive-body-marker-do-not-log';
    globalThis.fetch = jest.fn().mockResolvedValue(
      apiError(500, { code: 'server_error', detail: `internal failure near ${SECRET}` }),
    );
    await expect(qurl.getResourceStatus('res-redact')).rejects.toThrow(/500/);

    const leaked = logger.debug.mock.calls.some((args) => JSON.stringify(args).includes(SECRET));
    expect(leaked).toBe(false);
    const loggedStatus = logger.debug.mock.calls.some(
      ([msg, meta]) => msg === 'qURL API error' && meta && meta.status === 500,
    );
    expect(loggedStatus).toBe(true);
  });

  it('gives up after 3 attempts on persistent 503', async () => {
    // 3 total attempts = the budget the pre-SDK client documented
    // (initial + 2 retries); qurl.js pins the SDK to maxRetries:2.
    globalThis.fetch = jest.fn().mockResolvedValue(apiError(503));
    await expect(qurl.getResourceStatus('res-down')).rejects.toThrow(/503/);
    expect(globalThis.fetch).toHaveBeenCalledTimes(3);
  });

  it('retries on network error then succeeds', async () => {
    globalThis.fetch = jest.fn()
      .mockRejectedValueOnce(new Error('ECONNRESET'))
      .mockResolvedValueOnce(apiOk(200, { ok: true }));
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
      .mockResolvedValueOnce(apiError(429))
      .mockResolvedValueOnce(apiOk(200, {}));
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
      info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(), audit: jest.fn(),
    }));
    jest.doMock('dns', () => ({
      promises: { lookup: jest.fn().mockResolvedValue([{ address: '93.184.216.34', family: 4 }]) },
    }));
    qurl = require('../src/qurl');
  });
  afterEach(() => { globalThis.fetch = originalFetch; });

  it('creates a link for a public URL that passes DNS resolution', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue(
      apiOk(200, { resource_id: 'r1', qurl_link: 'https://q.link/abc' }),
    );
    const result = await qurl.createOneTimeLink('https://example.com/file', '1h', 'label');
    expect(result.resource_id).toBe('r1');
  });

  it('rejects when DNS lookup fails', async () => {
    jest.resetModules();
    jest.doMock('../src/config', () => ({ QURL_API_KEY: 'k', QURL_ENDPOINT: 'https://api.test.local' }));
    jest.doMock('../src/logger', () => ({ info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(), audit: jest.fn() }));
    jest.doMock('dns', () => ({
      promises: { lookup: jest.fn().mockRejectedValue(Object.assign(new Error('not found'), { code: 'ENOTFOUND' })) },
    }));
    const q = require('../src/qurl');
    await expect(q.createOneTimeLink('https://nowhere.example/file', '1h', 'label'))
      .rejects.toThrow(/resolved/);
  });
});
