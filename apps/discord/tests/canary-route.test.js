// Tests for the /canary/exec endpoint. Mounts the router on a test
// express app with the same raw-body-capturing JSON middleware that
// server.js uses, then exercises every authn + dispatch branch via
// supertest. Connector + qURL clients are mocked so the tests don't
// hit a real network.

const crypto = require('crypto');

// --- mocks must be set up BEFORE requiring the router ---

const mockUploadJsonToConnector = jest.fn();
const mockMintLinks = jest.fn();
jest.mock('../src/connector', () => ({
  uploadJsonToConnector: mockUploadJsonToConnector,
  mintLinks: mockMintLinks,
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
}));

// config is consumed via require-time imports inside the router. Provide
// a mutable mock so individual tests can override CANARY_SHARED_SECRET
// or QURL_API_KEY without re-requiring the module.
const mockConfig = {
  CANARY_SHARED_SECRET: undefined,
  QURL_API_KEY: undefined,
};
jest.mock('../src/config', () => mockConfig);

const express = require('express');
const request = require('supertest');
const canaryRouter = require('../src/routes/canary');

// Mirror server.js's mount: 4 KB JSON parser with verify-callback that
// captures req.rawBody. The router's HMAC check requires rawBody to be
// a Buffer.
function makeApp() {
  const app = express();
  app.use('/canary', express.json({
    limit: '4kb',
    verify: (req, _res, buf) => { req.rawBody = buf; },
  }));
  app.use('/canary', canaryRouter);
  return app;
}

const SECRET = 'a'.repeat(64); // 32-byte hex
const VALID_BODY = { probe: 'canary-test' };

function signedHeaders(body, secret = SECRET, ts = Math.floor(Date.now() / 1000)) {
  const raw = JSON.stringify(body);
  const sig = crypto.createHmac('sha256', secret).update(`${ts}.${raw}`).digest('hex');
  return { 'X-Canary-Signature': sig, 'X-Canary-Timestamp': String(ts) };
}

beforeEach(() => {
  jest.clearAllMocks();
  mockConfig.CANARY_SHARED_SECRET = SECRET;
  mockConfig.QURL_API_KEY = 'test-api-key';
  mockUploadJsonToConnector.mockResolvedValue({ resource_id: 'res-canary-1' });
  mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/canary-token-abc' }]);
});

describe('/canary/exec — auth', () => {
  it('returns 503 canary_disabled when CANARY_SHARED_SECRET is unset', async () => {
    mockConfig.CANARY_SHARED_SECRET = undefined;
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(signedHeaders(VALID_BODY));
    expect(res.status).toBe(503);
    expect(res.body).toEqual({ ok: false, error: 'canary_disabled' });
    // Ensures the dispatch never ran
    expect(mockUploadJsonToConnector).not.toHaveBeenCalled();
  });

  it('returns 401 missing_signature when X-Canary-Signature header is absent', async () => {
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set('X-Canary-Timestamp', String(Math.floor(Date.now() / 1000)));
    expect(res.status).toBe(401);
    expect(res.body.error).toBe('missing_signature');
  });

  it('returns 401 missing_signature when X-Canary-Timestamp header is absent', async () => {
    const headers = signedHeaders(VALID_BODY);
    delete headers['X-Canary-Timestamp'];
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(headers);
    expect(res.status).toBe(401);
    expect(res.body.error).toBe('missing_signature');
  });

  it('returns 401 bad_timestamp when X-Canary-Timestamp is not numeric', async () => {
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set({ 'X-Canary-Signature': 'a'.repeat(64), 'X-Canary-Timestamp': 'tomorrow' });
    expect(res.status).toBe(401);
    expect(res.body.error).toBe('bad_timestamp');
  });

  it('returns 401 expired_timestamp when timestamp drift exceeds 5 minutes', async () => {
    const tooOld = Math.floor(Date.now() / 1000) - 301;
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(signedHeaders(VALID_BODY, SECRET, tooOld));
    expect(res.status).toBe(401);
    expect(res.body.error).toBe('expired_timestamp');
  });

  it('returns 401 bad_signature when the HMAC was computed with the wrong secret', async () => {
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(signedHeaders(VALID_BODY, 'wrong-secret'));
    expect(res.status).toBe(401);
    expect(res.body.error).toBe('bad_signature');
  });

  it('returns 401 bad_signature when the body was modified after signing (replay-with-mutation)', async () => {
    const headers = signedHeaders(VALID_BODY);
    const tamperedBody = { ...VALID_BODY, extra: 'mutation' };
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(tamperedBody)
      .set(headers);
    expect(res.status).toBe(401);
    expect(res.body.error).toBe('bad_signature');
  });

  it('returns 401 bad_signature with a length-mismatched signature (timing-safe rejection)', async () => {
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set({
        'X-Canary-Signature': 'short',
        'X-Canary-Timestamp': String(Math.floor(Date.now() / 1000)),
      });
    expect(res.status).toBe(401);
    expect(res.body.error).toBe('bad_signature');
  });
});

describe('/canary/exec — dispatch', () => {
  it('returns 503 no_api_key when QURL_API_KEY is unset', async () => {
    mockConfig.QURL_API_KEY = undefined;
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(signedHeaders(VALID_BODY));
    expect(res.status).toBe(503);
    expect(res.body.ok).toBe(false);
    expect(res.body.error).toBe('no_api_key');
    expect(typeof res.body.latency_ms).toBe('number');
    expect(mockUploadJsonToConnector).not.toHaveBeenCalled();
  });

  it('returns 200 ok with link_host on the happy path', async () => {
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(signedHeaders(VALID_BODY));
    expect(res.status).toBe(200);
    expect(res.body.ok).toBe(true);
    expect(res.body.link_host).toBe('q.test');
    expect(res.body.resource_id).toBe('res-canary-1');
    expect(typeof res.body.latency_ms).toBe('number');
    // Did NOT echo the actual link in the response — link is single-use
    // and mustn't end up in CloudWatch logs.
    expect(JSON.stringify(res.body)).not.toContain('canary-token-abc');
  });

  it('passes a synthetic location payload + 60s expiry to the connector + mintLinks', async () => {
    await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(signedHeaders(VALID_BODY));
    expect(mockUploadJsonToConnector).toHaveBeenCalledWith(
      expect.objectContaining({
        type: 'google-map',
        url: expect.stringContaining('canary'),
        name: 'canary',
      }),
      'canary.json',
      'test-api-key',
    );
    expect(mockMintLinks).toHaveBeenCalledWith(
      'res-canary-1',
      expect.any(String),
      1,
      'test-api-key',
    );
    // expiresAt is ~60s in the future — verify it's an ISO date within
    // a generous window (the network call shape is what matters).
    const expiresAt = new Date(mockMintLinks.mock.calls[0][1]);
    const drift = expiresAt.getTime() - Date.now();
    expect(drift).toBeGreaterThan(30_000);
    expect(drift).toBeLessThanOrEqual(60_000);
  });

  it('returns 500 upload_no_resource_id when uploadJsonToConnector resolves without resource_id', async () => {
    mockUploadJsonToConnector.mockResolvedValueOnce({ /* no resource_id */ });
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(signedHeaders(VALID_BODY));
    expect(res.status).toBe(500);
    expect(res.body.error).toBe('upload_no_resource_id');
    expect(mockMintLinks).not.toHaveBeenCalled();
  });

  it('returns 500 no_link_in_mint_response when mintLinks returns an empty array', async () => {
    mockMintLinks.mockResolvedValueOnce([]);
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(signedHeaders(VALID_BODY));
    expect(res.status).toBe(500);
    expect(res.body.error).toBe('no_link_in_mint_response');
  });

  it('returns 500 exec_failed with the upstream reason when uploadJsonToConnector throws', async () => {
    const err = Object.assign(new Error('connector down'), { apiCode: 'connector_unreachable' });
    mockUploadJsonToConnector.mockRejectedValueOnce(err);
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(signedHeaders(VALID_BODY));
    expect(res.status).toBe(500);
    expect(res.body.error).toBe('exec_failed');
    expect(res.body.reason).toBe('connector down');
    expect(res.body.apiCode).toBe('connector_unreachable');
  });

  it('returns 500 exec_failed when mintLinks throws (e.g., qURL API down)', async () => {
    mockMintLinks.mockRejectedValueOnce(new Error('mint API 503'));
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(signedHeaders(VALID_BODY));
    expect(res.status).toBe(500);
    expect(res.body.error).toBe('exec_failed');
    expect(res.body.reason).toBe('mint API 503');
  });

  it('exec_failed responses include latency_ms (operators triage by it)', async () => {
    mockUploadJsonToConnector.mockRejectedValueOnce(new Error('network slow'));
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(signedHeaders(VALID_BODY));
    expect(res.body.latency_ms).toBeDefined();
    expect(typeof res.body.latency_ms).toBe('number');
    expect(res.body.latency_ms).toBeGreaterThanOrEqual(0);
  });
});
