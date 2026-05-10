// Tests for the /canary/exec endpoint. Mounts the router on a test
// express app with the same 4 KB JSON middleware that server.js uses,
// then exercises every authn + dispatch branch via supertest. Connector
// + qURL clients are mocked so the tests don't hit a real network.
//
// Auth model: the qURL/NHP knock provides authentication at the
// network layer (Lambda mints + resolves a qURL targeting the bot,
// triggering the NHP knock for its egress IP). The route itself
// only checks an X-Canary-Timestamp header to bound replay.

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
// a mutable mock so individual tests can override QURL_API_KEY or the
// allowlist without re-requiring the module.
const mockConfig = {
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

// Mirror server.js's mount: 4 KB JSON parser. No raw-body capture
// needed (no HMAC).
function makeApp() {
  const app = express();
  app.use('/canary', express.json({ limit: '4kb' }));
  app.use('/canary', canaryRouter);
  return app;
}

// Empty body — the legacy back-end-only path, mirrors what an
// operator would post (or a pre-extension Lambda). Differentiated-
// path tests use their own scenario-shaped bodies.
const VALID_BODY = {};

function tsHeaders(ts = Math.floor(Date.now() / 1000)) {
  return { 'X-Canary-Timestamp': String(ts) };
}

beforeEach(() => {
  jest.clearAllMocks();
  mockConfig.QURL_API_KEY = 'test-api-key';
  // Reset allowlist to suite default — individual tests override
  // (e.g. empty for "unconfigured" case, mismatched for "not allowed").
  mockConfig.CANARY_RECIPIENT_USER_IDS = ['1483661063835750551'];
  mockUploadJsonToConnector.mockResolvedValue({ resource_id: 'res-canary-1' });
  mockReUploadBuffer.mockResolvedValue({ resource_id: 'res-canary-file-1' });
  mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/canary-token-abc' }]);
  mockSendDM.mockResolvedValue(true);
});

describe('/canary/exec — timestamp replay-window', () => {
  it('returns 401 missing_timestamp when X-Canary-Timestamp header is absent (and warns for triage)', async () => {
    const logger = require('../src/logger');
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY);
    expect(res.status).toBe(401);
    expect(res.body).toEqual({ ok: false, error: 'missing_timestamp' });
    // Pre-auth 401s deliberately omit latency_ms (no triage value
    // pre-auth, no benefit to echoing timing info in unauthenticated
    // responses). Pin the absence so a future "uniformity" refactor
    // doesn't silently re-add it.
    expect(res.body.latency_ms).toBeUndefined();
    expect(logger.warn).toHaveBeenCalledWith(
      'Canary timestamp rejected',
      expect.objectContaining({ reason: 'missing_timestamp' }),
    );
  });

  it('returns 401 missing_timestamp when X-Canary-Timestamp is empty string', async () => {
    // Empty header is falsy and falls into the missing_timestamp
    // branch — pin it so a future refactor that swaps `if (!ts)` for
    // a stricter `if (ts === undefined)` doesn't silently accept it.
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set('X-Canary-Timestamp', '');
    expect(res.status).toBe(401);
    expect(res.body.error).toBe('missing_timestamp');
  });

  it('returns 401 bad_timestamp when X-Canary-Timestamp is not numeric', async () => {
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set('X-Canary-Timestamp', 'tomorrow');
    expect(res.status).toBe(401);
    expect(res.body.error).toBe('bad_timestamp');
    expect(res.body.latency_ms).toBeUndefined();
  });

  it('returns 401 expired_timestamp when timestamp drift exceeds 5 minutes (past)', async () => {
    const tooOld = Math.floor(Date.now() / 1000) - 301;
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(tsHeaders(tooOld));
    expect(res.status).toBe(401);
    expect(res.body.error).toBe('expired_timestamp');
    expect(res.body.latency_ms).toBeUndefined();
  });

  it('returns 401 expired_timestamp when timestamp drift exceeds 5 minutes (future-skewed)', async () => {
    // The middleware's drift check is symmetric (Math.abs); a
    // far-future timestamp must reject too — pins the symmetry so
    // a refactor that drops Math.abs is caught.
    const tooNew = Math.floor(Date.now() / 1000) + 400;
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(tsHeaders(tooNew));
    expect(res.status).toBe(401);
    expect(res.body.error).toBe('expired_timestamp');
  });

  it('accepts timestamp just inside the 5-minute window (drift ~299s)', async () => {
    // Bracket the rejection boundary from below. Combined with the
    // 301s-past test above and the +400s-future test, this pins the
    // window at "just under 5 min OK, more than 5 min reject" without
    // the second-boundary flake risk a `drift === 300` exact-match
    // assertion would carry.
    const justInside = Math.floor(Date.now() / 1000) - 299;
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(tsHeaders(justInside));
    expect(res.status).toBe(200);
  });
});

describe('/canary/exec — dispatch', () => {
  it('returns 503 no_api_key when QURL_API_KEY is unset', async () => {
    mockConfig.QURL_API_KEY = undefined;
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(tsHeaders());
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
      .set(tsHeaders());
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
      .set(tsHeaders());
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
      .set(tsHeaders());
    expect(res.status).toBe(500);
    expect(res.body.error).toBe('upload_no_resource_id');
    expect(mockMintLinks).not.toHaveBeenCalled();
  });

  it('returns 500 no_link_in_mint_response when mintLinks returns an empty array', async () => {
    mockMintLinks.mockResolvedValueOnce([]);
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(tsHeaders());
    expect(res.status).toBe(500);
    expect(res.body.error).toBe('no_link_in_mint_response');
  });

  it('returns 500 exec_failed with the upstream reason when uploadJsonToConnector throws', async () => {
    const err = Object.assign(new Error('connector down'), { apiCode: 'connector_unreachable' });
    mockUploadJsonToConnector.mockRejectedValueOnce(err);
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(tsHeaders());
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
      .set(tsHeaders());
    expect(res.status).toBe(500);
    expect(res.body.error).toBe('exec_failed');
    expect(res.body.reason).toBe('mint API 503');
  });

  it('exec_failed responses include latency_ms (operators triage by it)', async () => {
    mockUploadJsonToConnector.mockRejectedValueOnce(new Error('network slow'));
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(VALID_BODY)
      .set(tsHeaders());
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
      .set(tsHeaders());
    expect(res.status).toBe(400);
    expect(res.body.error).toBe('invalid_test');
    expect(res.body.valid).toEqual(expect.arrayContaining(['send_file', 'send_location']));
  });

  it('returns 400 invalid_recipient_user_id for a non-snowflake recipient', async () => {
    const body = { test: 'send_file', recipient_user_id: 'not-a-snowflake' };
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(body)
      .set(tsHeaders());
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
      .set(tsHeaders());
    expect(res.status).toBe(400);
    expect(res.body.error).toBe('invalid_test');
  });

  it('send_file: uploads via reUploadBuffer (NOT uploadJsonToConnector), mints, DMs', async () => {
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(SEND_FILE_BODY)
      .set(tsHeaders());
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
      .set(tsHeaders());
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
      .set(tsHeaders());
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
      .set(tsHeaders());
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
      .set(tsHeaders());
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
      .set(tsHeaders());
    expect(res.body.test).toBe('send_file');
    expect(res.body.recipient_user_id).toBe(VALID_USER_ID);
  });

  it('returns 503 canary_recipients_unconfigured when allowlist is empty (server-config state)', async () => {
    mockConfig.CANARY_RECIPIENT_USER_IDS = [];
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(SEND_FILE_BODY)
      .set(tsHeaders());
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
      .set(tsHeaders());
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
      .set(tsHeaders());
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
