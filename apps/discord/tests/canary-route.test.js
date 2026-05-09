// Tests for the /canary/exec endpoint. Mounts the router on a test
// express app with the same raw-body-capturing JSON middleware that
// server.js uses, then exercises every authn + dispatch branch via
// supertest. Connector + qURL clients are mocked so the tests don't
// hit a real network.

const crypto = require('crypto');

// --- mocks must be set up BEFORE requiring the router ---

const mockUploadJsonToConnector = jest.fn();
const mockMintLinks = jest.fn();
const mockReUploadBuffer = jest.fn();
jest.mock('../src/connector', () => ({
  uploadJsonToConnector: mockUploadJsonToConnector,
  mintLinks: mockMintLinks,
  reUploadBuffer: mockReUploadBuffer,
}));

const mockSendDM = jest.fn();
jest.mock('../src/discord', () => ({
  sendDM: mockSendDM,
}));

// EmbedBuilder is the only discord.js export the canary route uses.
// Keep the mock minimal — the canary's purpose is exercising the
// connector → mint → DM call chain, not asserting on embed shape.
jest.mock('discord.js', () => ({
  EmbedBuilder: jest.fn().mockImplementation(() => {
    const embed = {
      setColor: jest.fn().mockReturnThis(),
      setTitle: jest.fn().mockReturnThis(),
      setDescription: jest.fn().mockReturnThis(),
      setTimestamp: jest.fn().mockReturnThis(),
    };
    return embed;
  }),
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
  // Allowlist gates the differentiated path. Suite-default contains
  // the canonical test user-ID used across the differentiated-path
  // tests below; allowlist-specific tests override per-case.
  CANARY_RECIPIENT_USER_IDS: ['1483661063835750551'],
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
  // Reset allowlist to suite default — individual tests override
  // (e.g. empty for "unconfigured" case, mismatched for "not allowed").
  mockConfig.CANARY_RECIPIENT_USER_IDS = ['1483661063835750551'];
  mockUploadJsonToConnector.mockResolvedValue({ resource_id: 'res-canary-1' });
  mockReUploadBuffer.mockResolvedValue({ resource_id: 'res-canary-file-1' });
  mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/canary-token-abc' }]);
  mockSendDM.mockResolvedValue(true);
  // Per-IP bad-sig counter is module-level state — reset between
  // tests so the rate-limit doesn't leak across cases (an earlier
  // bad-sig test would otherwise carry counter into a later test
  // and silently shift assertions to "rate-limited" semantics).
  if (canaryRouter._test && canaryRouter._test._resetBadSigState) {
    canaryRouter._test._resetBadSigState();
  }
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

// Differentiated path — Lambda canary sends {test, recipient_user_id}
// in the body. Each test = upload (file or location) → mint → DM.
describe('/canary/exec — differentiated scenario path', () => {
  const VALID_USER_ID = '1483661063835750551';
  const SEND_FILE_BODY      = { test: 'send_file',     recipient_user_id: VALID_USER_ID };
  const SEND_LOCATION_BODY  = { test: 'send_location', recipient_user_id: VALID_USER_ID };

  it('returns 400 invalid_test for an unrecognized test value', async () => {
    const body = { test: 'send_carrier_pigeon', recipient_user_id: VALID_USER_ID };
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(body)
      .set(signedHeaders(body));
    expect(res.status).toBe(400);
    expect(res.body.error).toBe('invalid_test');
    expect(res.body.valid).toEqual(expect.arrayContaining(['send_file', 'send_location']));
  });

  it('returns 400 invalid_recipient_user_id for a non-snowflake recipient', async () => {
    const body = { test: 'send_file', recipient_user_id: 'not-a-snowflake' };
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(body)
      .set(signedHeaders(body));
    expect(res.status).toBe(400);
    expect(res.body.error).toBe('invalid_recipient_user_id');
  });

  it('returns 400 invalid_test when only recipient_user_id is supplied (partial body)', async () => {
    // Either both or neither — partial body is rejected to catch
    // Lambda misconfig early.
    const body = { recipient_user_id: VALID_USER_ID };
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(body)
      .set(signedHeaders(body));
    expect(res.status).toBe(400);
    expect(res.body.error).toBe('invalid_test');
  });

  it('send_file: uploads via reUploadBuffer (NOT uploadJsonToConnector), mints, DMs', async () => {
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(SEND_FILE_BODY)
      .set(signedHeaders(SEND_FILE_BODY));
    expect(res.status).toBe(200);
    expect(res.body.ok).toBe(true);
    expect(res.body.test).toBe('send_file');
    expect(res.body.recipient_user_id).toBe(VALID_USER_ID);
    expect(res.body.dm_status).toBe('sent');
    expect(mockReUploadBuffer).toHaveBeenCalledTimes(1);
    expect(mockUploadJsonToConnector).not.toHaveBeenCalled();
    expect(mockMintLinks).toHaveBeenCalledTimes(1);
    expect(mockSendDM).toHaveBeenCalledWith(VALID_USER_ID, expect.objectContaining({ embeds: expect.any(Array) }));
  });

  it('send_location: uploads via uploadJsonToConnector (NOT reUploadBuffer), mints, DMs', async () => {
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(SEND_LOCATION_BODY)
      .set(signedHeaders(SEND_LOCATION_BODY));
    expect(res.status).toBe(200);
    expect(res.body.ok).toBe(true);
    expect(res.body.test).toBe('send_location');
    expect(res.body.dm_status).toBe('sent');
    expect(mockUploadJsonToConnector).toHaveBeenCalledTimes(1);
    expect(mockReUploadBuffer).not.toHaveBeenCalled();
    expect(mockSendDM).toHaveBeenCalledWith(VALID_USER_ID, expect.objectContaining({ embeds: expect.any(Array) }));
  });

  it('attributes failure to step="upload" when reUploadBuffer rejects', async () => {
    mockReUploadBuffer.mockRejectedValueOnce(new Error('connector 502'));
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(SEND_FILE_BODY)
      .set(signedHeaders(SEND_FILE_BODY));
    expect(res.status).toBe(500);
    expect(res.body.ok).toBe(false);
    expect(res.body.step).toBe('upload');
    expect(res.body.error).toBe('upload_threw');
    // Mint + DM never run when upload fails — pin the early-return.
    expect(mockMintLinks).not.toHaveBeenCalled();
    expect(mockSendDM).not.toHaveBeenCalled();
  });

  it('attributes failure to step="mint" when mintLinks returns no link', async () => {
    mockMintLinks.mockResolvedValueOnce([]);
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(SEND_LOCATION_BODY)
      .set(signedHeaders(SEND_LOCATION_BODY));
    expect(res.status).toBe(500);
    expect(res.body.step).toBe('mint');
    expect(res.body.error).toBe('no_link_in_mint_response');
    expect(mockSendDM).not.toHaveBeenCalled();
  });

  it('attributes failure to step="dm" when sendDM returns false', async () => {
    mockSendDM.mockResolvedValueOnce(false);
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(SEND_FILE_BODY)
      .set(signedHeaders(SEND_FILE_BODY));
    expect(res.status).toBe(500);
    expect(res.body.step).toBe('dm');
    expect(res.body.error).toBe('dm_failed');
    // Upload + mint succeeded — confirm the link_host is still echoed
    // so the failure log lands on the right qURL pool.
    expect(res.body.link_host).toBeDefined();
  });

  it('echoes test + recipient_user_id back to the Lambda for unambiguous metric attribution', async () => {
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(SEND_FILE_BODY)
      .set(signedHeaders(SEND_FILE_BODY));
    expect(res.body.test).toBe('send_file');
    expect(res.body.recipient_user_id).toBe(VALID_USER_ID);
  });

  it('returns 503 canary_recipients_unconfigured when allowlist is empty (server-config state)', async () => {
    mockConfig.CANARY_RECIPIENT_USER_IDS = [];
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(SEND_FILE_BODY)
      .set(signedHeaders(SEND_FILE_BODY));
    expect(res.status).toBe(503);
    expect(res.body.error).toBe('canary_recipients_unconfigured');
    // No connector / DM side-effect when the allowlist gate fires
    expect(mockReUploadBuffer).not.toHaveBeenCalled();
    expect(mockSendDM).not.toHaveBeenCalled();
  });

  it('returns 403 recipient_not_allowed when recipient is not in the allowlist (textbook 403)', async () => {
    mockConfig.CANARY_RECIPIENT_USER_IDS = ['9999999999999999999'];
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(SEND_FILE_BODY)
      .set(signedHeaders(SEND_FILE_BODY));
    expect(res.status).toBe(403);
    expect(res.body.error).toBe('recipient_not_allowed');
    expect(mockSendDM).not.toHaveBeenCalled();
  });

  it('logs a structured warn when a scenario step fails (so on-call has a correlatable log)', async () => {
    const logger = require('../src/logger');
    mockSendDM.mockResolvedValueOnce(false);
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(SEND_FILE_BODY)
      .set(signedHeaders(SEND_FILE_BODY));
    expect(res.status).toBe(500);
    expect(res.body.step).toBe('dm');
    expect(logger.warn).toHaveBeenCalledWith(
      'Canary scenario failed',
      expect.objectContaining({
        test: 'send_file',
        recipient_user_id: VALID_USER_ID,
        step: 'dm',
        error: 'dm_failed',
      })
    );
  });
});

// Per-IP bad-signature throttle. Mirror of the webhooks.js test
// pattern — without this, an attacker can spam invalid signatures
// and burn unbounded HMAC compute on the public endpoint.
describe('/canary/exec — bad-signature rate limit', () => {
  it('rejects with 401 bad_signature when sig shape is non-hex (regex pre-check, no HMAC compute)', async () => {
    const headers = { 'X-Canary-Signature': 'not-hex!!!!' + 'x'.repeat(53), 'X-Canary-Timestamp': String(Math.floor(Date.now() / 1000)) };
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(headers);
    expect(res.status).toBe(401);
    expect(res.body.error).toBe('bad_signature');
    // Must NOT have called HMAC-dependent connector code — regex
    // pre-check should short-circuit before the HMAC verify.
    expect(mockUploadJsonToConnector).not.toHaveBeenCalled();
  });

  it('rejects with 401 bad_signature when sig is the wrong length (regex)', async () => {
    const headers = { 'X-Canary-Signature': 'abc123', 'X-Canary-Timestamp': String(Math.floor(Date.now() / 1000)) };
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(headers);
    expect(res.status).toBe(401);
    expect(res.body.error).toBe('bad_signature');
  });

  it('returns 429 rate_limited after 30 bad-signature attempts in a 60s window', async () => {
    const app = makeApp();
    const wrongSecret = 'b'.repeat(64);
    // Fire 30 bad sigs to fill the per-IP bucket. supertest preserves
    // the same source IP across requests in this jest worker, so the
    // counter accumulates as expected.
    for (let i = 0; i < 30; i++) {
      await request(app)
        .post('/canary/exec')
        .send(VALID_BODY)
        .set(signedHeaders(VALID_BODY, wrongSecret));
    }
    // 31st attempt — even with a VALID sig, rate-limit short-circuits
    // before signature verification (rate limit must precede HMAC
    // compute to actually defend against the attack class).
    const res = await request(app)
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(signedHeaders(VALID_BODY));
    expect(res.status).toBe(429);
    expect(res.body.error).toBe('rate_limited');
  });

  it('does NOT count a successful request against the bad-sig bucket', async () => {
    const app = makeApp();
    // 25 valid requests — under the 30-cap and SHOULD all succeed.
    for (let i = 0; i < 25; i++) {
      const res = await request(app)
        .post('/canary/exec')
        .send(VALID_BODY)
        .set(signedHeaders(VALID_BODY));
      expect(res.status).toBe(200);
    }
    // 26th valid request — would hit 429 if successful requests
    // counted, but they don't.
    const res = await request(app)
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(signedHeaders(VALID_BODY));
    expect(res.status).toBe(200);
  });
});
