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
    // The bot's User-Agent is preserved across the SDK migration (literal wire
    // identifier per CLAUDE.md).
    expect(opts.headers['User-Agent']).toBe('qurl-discord-bot/1.0');
    expect(result.resource_id).toBe('res-123');
    // The SDK renames the API's wire-format `qurls` field to `access_tokens`.
    expect(result.access_tokens).toHaveLength(1);
  });

  it('throws on 404 API error (status-only message, body redacted)', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue(apiError(404, { code: 'not_found' }));

    // callQurl re-throws a status-only error (the old wire-contract shape), not
    // the SDK error whose message would carry the server `detail`.
    await expect(qurl.getResourceStatus('bad-id')).rejects.toThrow(/qURL API GET.*failed.*404/);
  });

  it('re-wraps an unexpected 204 to a code-only error (status-0 redaction allowlist)', async () => {
    // The pre-SDK client returned null here; the SDK treats a body-less GET as a
    // contract violation (status 0, code `unexpected_response`). That code is
    // NOT in SAFE_STATUS0_CODES, so callQurl re-wraps it to a code-only message
    // and the SDK's own text never escapes — exercising the status-0 allowlist's
    // re-wrap branch (the network-error test below exercises the verbatim branch).
    globalThis.fetch = jest.fn().mockResolvedValue(apiOk(204, undefined));

    const thrown = await qurl.getResourceStatus('res-empty').then(
      () => { throw new Error('expected rejection'); },
      (e) => e,
    );
    expect(thrown.message).toMatch(/qURL API GET .*failed \(unexpected_response\)/);
    expect(thrown.message).not.toMatch(/Unexpected 204|No Content/);
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

  it('redacts the error body end-to-end — neither the log nor the thrown error carries it', async () => {
    // REDACTION INVARIANT: a qURL error body can echo request headers or tokens,
    // so the body must never escape this module — not via the breadcrumb, and
    // not via the thrown error's message (the revoke path logs `err.message`
    // unconditionally). Plant a token in the body and assert it reaches neither
    // logger.debug nor the thrown error, while the status still surfaces in both.
    const logger = require('../src/logger');
    logger.debug.mockClear();
    // A clearly-fake stand-in for a body that echoes a token — not a real key
    // pattern, so secret scanners don't flag the fixture.
    const SECRET = 'sensitive-body-marker-do-not-log';
    globalThis.fetch = jest.fn().mockResolvedValue(
      apiError(500, { code: 'server_error', detail: `internal failure near ${SECRET}` }),
    );

    const thrown = await qurl.getResourceStatus('res-redact').then(
      () => { throw new Error('expected rejection'); },
      (e) => e,
    );
    // Thrown error: status-only, never the body.
    expect(thrown.message).toMatch(/500/);
    expect(thrown.message).not.toContain(SECRET);

    // Breadcrumb: status logged, body never logged.
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

  it('does NOT retry GET on 500 or 408 (SDK narrows the retry set)', async () => {
    // The pre-SDK client retried {408, 429, 500, 502, 503, 504}; the SDK's
    // non-mutating set is {429, 502, 503, 504}, so 500 and 408 are attempted
    // exactly once. Pins the narrowing alongside the 503/429 retried-set tests.
    for (const status of [500, 408]) {
      globalThis.fetch = jest.fn().mockResolvedValue(apiError(status));
      await expect(qurl.getResourceStatus(`res-${status}`)).rejects.toThrow(new RegExp(String(status)));
      expect(globalThis.fetch).toHaveBeenCalledTimes(1);
    }
  });

  it('retries DELETE on 503 then succeeds (revoke shares the GET/DELETE retry budget)', async () => {
    // DELETE is idempotent, so unlike POST it does get the {429,502,503,504}
    // retry budget — pin that the revoke path retries a transient 503.
    globalThis.fetch = jest.fn()
      .mockResolvedValueOnce(apiError(503))
      .mockResolvedValueOnce(apiOk(204, undefined));
    await qurl.deleteLink('r_resource1234');
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

  it('does NOT retry the create POST on a transient 503 (mutating-retry policy)', async () => {
    // POST is non-idempotent: the SDK retries it only on 429, never 5xx — so a
    // create is attempted exactly once on a 503, removing the duplicate-create
    // risk the pre-SDK client carried (it retried POST on transient 5xx).
    globalThis.fetch = jest.fn().mockResolvedValue(apiError(503));
    await expect(qurl.createOneTimeLink('https://example.com/file', '1h', 'label'))
      .rejects.toThrow(/qURL API POST.*failed.*503/);
    expect(globalThis.fetch).toHaveBeenCalledTimes(1);
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
