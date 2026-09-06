
const {
  CRID_RESOURCE_ID,
  PUBLIC_KEY_RESOURCE_ID,
} = require('./helpers/qurl-fixtures');

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

const originalFetch = globalThis.fetch;

function apiOk(status, data) {
  return {
    ok: true,
    status,
    headers: { get: () => null },
    json: async () => (data === undefined ? {} : { data }),
  };
}

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

  it.each([
    ['public key', PUBLIC_KEY_RESOURCE_ID],
    ['CRID', CRID_RESOURCE_ID],
  ])('sends a real-shaped %s ID to GET /v1/qurls/:resourceId', async (_, resourceId) => {
    globalThis.fetch = jest.fn().mockResolvedValue(
      apiOk(200, {
        resource_id: PUBLIC_KEY_RESOURCE_ID,
        qurls: [{ qurl_id: 'q1', use_count: 0, status: 'active', created_at: '2026-01-01' }],
      }),
    );

    const result = await qurl.getResourceStatus(resourceId);

    expect(globalThis.fetch).toHaveBeenCalledTimes(1);
    const [url, opts] = globalThis.fetch.mock.calls[0];
    expect(url).toBe(`https://api.test.local/v1/qurls/${resourceId}`);
    expect(opts.method).toBe('GET');
    expect(opts.headers.Authorization).toBe('Bearer test-api-key');
    expect(opts.headers['User-Agent']).toBe('qurl-discord-bot/1.0');
    expect(result.resource_id).toBe(PUBLIC_KEY_RESOURCE_ID);
    expect(result.access_tokens).toHaveLength(1);
  });

  it.each([
    ['path separators', '../resources/x'],
    ['an overlong value', 'a'.repeat(1025)],
  ])('rejects %s before status network work', async (_kind, resourceId) => {
    globalThis.fetch = jest.fn();

    await expect(qurl.getResourceStatus(resourceId)).rejects
      .toThrow(/Invalid resource ID format/);
    expect(globalThis.fetch).not.toHaveBeenCalled();
  });

  it('throws on 404 API error (status-only message, body redacted)', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue(apiError(404, { code: 'not_found' }));

    await expect(qurl.getResourceStatus('bad-id')).rejects.toThrow(/qURL API GET.*failed.*404/);
  });

  it('re-wraps an unexpected 204 to a code-only error (status-0 redaction allowlist)', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue(apiOk(204, undefined));

    const thrown = await qurl.getResourceStatus('res-empty').then(
      () => { throw new Error('expected rejection'); },
      (e) => e,
    );
    expect(thrown.message).toMatch(/qURL API GET .*failed \(unexpected_response\)/);
    expect(thrown.message).not.toMatch(/Unexpected 204|No Content/);
  });

  it.each([
    ['GET status', resourceId => qurl.getResourceStatus(resourceId)],
    ['DELETE revoke', resourceId => qurl.deleteLink(resourceId)],
  ])('rejects an access token passed as a resource ID without logging or echoing it (%s)', async (_label, invoke) => {
    const logger = require('../src/logger');
    const accessToken = ['at', 'sensitive-access-marker'].join('_');
    globalThis.fetch = jest.fn();

    const thrown = await invoke(accessToken).then(
      () => { throw new Error('expected rejection'); },
      error => error,
    );

    expect(globalThis.fetch).not.toHaveBeenCalled();
    expect(thrown.message).toBe('Invalid resource ID format');
    expect(thrown.message).not.toContain(accessToken);
    const allLogs = JSON.stringify([
      logger.debug.mock.calls,
      logger.info.mock.calls,
      logger.warn.mock.calls,
      logger.error.mock.calls,
      logger.audit.mock.calls,
    ]);
    expect(allLogs).not.toContain(accessToken);
  });

  it.each([undefined, null, 123, {}, ['r_resource']])(
    'rejects a non-string resource ID without coercing it (%p)',
    (resourceId) => {
      expect(() => qurl.validateResourceId(resourceId)).toThrow('Invalid resource ID format');
    },
  );

  it('rejects a malformed resource ID with a generic, non-echoing error', async () => {
    const logger = require('../src/logger');
    const malformedId = 'bad/id#sensitive-marker';
    globalThis.fetch = jest.fn();

    const thrown = await qurl.getResourceStatus(malformedId).catch(error => error);

    expect(globalThis.fetch).not.toHaveBeenCalled();
    expect(thrown.message).toBe('Invalid resource ID format');
    expect(thrown.message).not.toContain(malformedId);
    expect(JSON.stringify([
      logger.debug.mock.calls,
      logger.info.mock.calls,
      logger.warn.mock.calls,
      logger.error.mock.calls,
      logger.audit.mock.calls,
    ])).not.toContain(malformedId);
  });

  it('does not broaden the access-token check to public IDs beginning with "at"', async () => {
    const publicId = `at${'a'.repeat(105)}`;
    globalThis.fetch = jest.fn().mockResolvedValue(apiOk(200, {
      resource_id: publicId,
      qurls: [],
    }));

    await expect(qurl.getResourceStatus(publicId)).resolves.toBeDefined();
    expect(globalThis.fetch).toHaveBeenCalledTimes(1);
  });

  it('re-wraps SDK client-validation errors without echoing the rejected identifier', async () => {
    const logger = require('../src/logger');
    const { QURLClient, ERROR_CODE_CLIENT_VALIDATION } = require('@layervai/qurl');
    const unknownCredential = 'ak_sensitive-future-credential';
    const clientError = Object.assign(
      new Error(`delete rejected ${unknownCredential}`),
      { status: 0, code: ERROR_CODE_CLIENT_VALIDATION },
    );
    const deleteSpy = jest.spyOn(QURLClient.prototype, 'deleteResource').mockRejectedValueOnce(clientError);

    try {
      const thrown = await qurl.deleteLink(unknownCredential).catch(error => error);

      expect(thrown.message).toBe(
        'qURL API DELETE /resources/:resourceId failed (client_validation)',
      );
      expect(thrown.message).not.toContain(unknownCredential);
      expect(JSON.stringify(logger.debug.mock.calls)).not.toContain(unknownCredential);
    } finally {
      deleteSpy.mockRestore();
    }
  });

  it('re-wraps an uncoded SDK throw without echoing a resource credential', async () => {
    const logger = require('../src/logger');
    const { QURLClient } = require('@layervai/qurl');
    const unknownCredential = 'ak_sensitive-uncoded-credential';
    const deleteSpy = jest.spyOn(QURLClient.prototype, 'deleteResource')
      .mockRejectedValueOnce(new TypeError(`delete rejected ${unknownCredential}`));

    try {
      const thrown = await qurl.deleteLink(unknownCredential).catch(error => error);

      expect(thrown.message).toBe(
        'qURL API DELETE /resources/:resourceId failed (unknown_error)',
      );
      expect(JSON.stringify([
        thrown.message,
        logger.debug.mock.calls,
        logger.audit.mock.calls,
      ])).not.toContain(unknownCredential);
    } finally {
      deleteSpy.mockRestore();
    }
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
        path: '/qurls/:resourceId',
      }),
    );
    expect(JSON.stringify(logger.debug.mock.calls)).not.toContain('res-auth-401');
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
    const logger = require('../src/logger');
    logger.debug.mockClear();
    const SECRET = 'sensitive-body-marker-do-not-log';
    globalThis.fetch = jest.fn().mockResolvedValue(
      apiError(500, { code: 'server_error', detail: `internal failure near ${SECRET}` }),
    );

    const thrown = await qurl.getResourceStatus('res-redact').then(
      () => { throw new Error('expected rejection'); },
      (e) => e,
    );
    expect(thrown.message).toMatch(/500/);
    expect(thrown.message).not.toContain(SECRET);

    const leaked = logger.debug.mock.calls.some((args) => JSON.stringify(args).includes(SECRET));
    expect(leaked).toBe(false);
    const loggedStatus = logger.debug.mock.calls.some(
      ([msg, meta]) => msg === 'qURL API error' && meta && meta.status === 500,
    );
    expect(loggedStatus).toBe(true);
  });

  it('gives up after 3 attempts on persistent 503', async () => {
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
    for (const status of [500, 408]) {
      globalThis.fetch = jest.fn().mockResolvedValue(apiError(status));
      await expect(qurl.getResourceStatus(`res-${status}`)).rejects.toThrow(new RegExp(String(status)));
      expect(globalThis.fetch).toHaveBeenCalledTimes(1);
    }
  });

  it('retries DELETE on 503 then succeeds (revoke shares the GET/DELETE retry budget)', async () => {
    globalThis.fetch = jest.fn()
      .mockResolvedValueOnce(apiError(503))
      .mockResolvedValueOnce(apiOk(204, undefined));
    await qurl.deleteLink(PUBLIC_KEY_RESOURCE_ID);
    expect(globalThis.fetch).toHaveBeenCalledTimes(2);
  });

  it('redacts the resource ID from DELETE error logs and auth audit metadata', async () => {
    const logger = require('../src/logger');
    const { AUDIT_EVENTS } = require('../src/constants');
    const { resourceIdLogRef } = require('../src/utils/resource-id');
    const resourceId = 'r_sensitive_resource_marker';
    globalThis.fetch = jest.fn().mockResolvedValue(apiError(401));

    const thrown = await qurl.deleteLink(resourceId).then(
      () => { throw new Error('expected rejection'); },
      error => error,
    );

    expect(thrown.message).not.toContain(resourceId);
    expect(JSON.stringify(logger.debug.mock.calls)).not.toContain(resourceId);
    expect(logger.debug).toHaveBeenCalledWith(
      'qURL API error',
      expect.objectContaining({ resource_ref: resourceIdLogRef(resourceId) }),
    );
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.DEPENDENCY_AUTH_FAILURE,
      expect.objectContaining({ method: 'DELETE', path: '/resources/:resourceId' }),
    );
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
    globalThis.fetch = jest.fn().mockResolvedValue(apiError(503));
    await expect(qurl.createOneTimeLink('https://example.com/file', '1h', 'label'))
      .rejects.toThrow(/qURL API POST.*failed.*503/);
    expect(globalThis.fetch).toHaveBeenCalledTimes(1);
  });

  it('redacts uncoded SDK validation text that could echo the target URL', async () => {
    const logger = require('../src/logger');
    const { QURLClient } = require('@layervai/qurl');
    const targetUrl = 'https://example.com/file?secret=sensitive-target-marker';
    const createSpy = jest.spyOn(QURLClient.prototype, 'create')
      .mockRejectedValueOnce(new Error(`invalid target_url: ${targetUrl}`));

    try {
      const thrown = await qurl.createOneTimeLink(targetUrl, '1h', 'label')
        .catch(error => error);

      expect(thrown.message).toBe('qURL API POST /qurls failed (unknown_error)');
      expect(JSON.stringify([
        thrown.message,
        logger.debug.mock.calls,
        logger.audit.mock.calls,
      ])).not.toContain(targetUrl);
    } finally {
      createSpy.mockRestore();
    }
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
