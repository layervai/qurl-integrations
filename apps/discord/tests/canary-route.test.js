// Tests for the /canary/exec endpoint. Mounts the router on a test
// express app with the same 4 KB JSON middleware that server.js uses,
// then exercises every dispatch branch via supertest. Connector + qURL
// clients are mocked so the tests don't hit a real network.
//
// Auth model: NHP knock at the network layer. The Lambda calls
// /nhp/internal/knock directly; AC opens the iptables hole for the
// Lambda's egress IP within OpenTime. The route does no app-layer
// auth — reaching it means the AC already authorized the caller.

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
// needed (NHP gates at the network layer; no app-layer signature
// or timestamp).
function makeApp() {
  const app = express();
  app.use('/canary', express.json({ limit: '4kb' }));
  app.use('/canary', canaryRouter);
  return app;
}

// Every request must carry both `test` and `recipient_user_id` —
// the legacy empty-body path was dropped before this PR landed
// (consolidation PR for the canary surface). Tests use these two
// shared bodies for the happy paths; failure-mode tests build
// their own.
const VALID_USER_ID = '1483661063835750551';
const SEND_FILE_BODY     = { test: 'send_file',     recipient_user_id: VALID_USER_ID };
const SEND_LOCATION_BODY = { test: 'send_location', recipient_user_id: VALID_USER_ID };

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

// Early gates — config-level rejections that run before any
// upload/mint/dm work. Body shape is the same as the differentiated
// path below.
describe('/canary/exec — early gates', () => {
  it('returns 503 no_api_key when QURL_API_KEY is unset', async () => {
    mockConfig.QURL_API_KEY = undefined;
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(SEND_FILE_BODY);
    expect(res.status).toBe(503);
    expect(res.body.ok).toBe(false);
    expect(res.body.error).toBe('no_api_key');
    expect(typeof res.body.latency_ms).toBe('number');
    expect(mockUploadJsonToConnector).not.toHaveBeenCalled();
    expect(mockReUploadBuffer).not.toHaveBeenCalled();
  });
});

// Differentiated path — Lambda canary sends {test, recipient_user_id}
// in the body. Each test = upload (file or location) → mint → DM.
describe('/canary/exec — differentiated scenario path', () => {
  it('returns 400 invalid_test for an unrecognized test value', async () => {
    const body = { test: 'send_carrier_pigeon', recipient_user_id: VALID_USER_ID };
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(body);
    expect(res.status).toBe(400);
    expect(res.body.error).toBe('invalid_test');
    expect(res.body.valid).toEqual(expect.arrayContaining(['send_file', 'send_location']));
  });

  it('returns 400 invalid_recipient_user_id for a non-snowflake recipient', async () => {
    const body = { test: 'send_file', recipient_user_id: 'not-a-snowflake' };
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(body);
    expect(res.status).toBe(400);
    expect(res.body.error).toBe('invalid_recipient_user_id');
  });

  it('returns 400 invalid_test when only recipient_user_id is supplied (partial body)', async () => {
    // Either both or neither — partial body is rejected to catch
    // Lambda misconfig early.
    const body = { recipient_user_id: VALID_USER_ID };
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(body);
    expect(res.status).toBe(400);
    expect(res.body.error).toBe('invalid_test');
  });

  it('send_file: uploads via reUploadBuffer (NOT uploadJsonToConnector), mints, DMs', async () => {
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(SEND_FILE_BODY);
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
      .send(SEND_LOCATION_BODY);
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
      .send(SEND_FILE_BODY);
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
      .send(SEND_LOCATION_BODY);
    expect(res.status).toBe(500);
    expect(res.body.step).toBe('mint');
    expect(res.body.error).toBe('no_link_in_mint_response');
    expect(mockSendDM).not.toHaveBeenCalled();
  });

  it('attributes failure to step="dm" when sendDM returns false', async () => {
    mockSendDM.mockResolvedValueOnce(false);
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(SEND_FILE_BODY);
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
      .send(SEND_FILE_BODY);
    expect(res.body.test).toBe('send_file');
    expect(res.body.recipient_user_id).toBe(VALID_USER_ID);
  });

  it('returns 503 canary_recipients_unconfigured when allowlist is empty (server-config state)', async () => {
    mockConfig.CANARY_RECIPIENT_USER_IDS = [];
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(SEND_FILE_BODY);
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
      .send(SEND_FILE_BODY);
    expect(res.status).toBe(403);
    expect(res.body.error).toBe('recipient_not_allowed');
    expect(mockSendDM).not.toHaveBeenCalled();
  });

  it('logs a structured warn when a scenario step fails (so on-call has a correlatable log)', async () => {
    const logger = require('../src/logger');
    mockSendDM.mockResolvedValueOnce(false);
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(SEND_FILE_BODY);
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

  it('attributes failure to step="mint" with apiCode propagated to logs when mintLinks rejects', async () => {
    // Differentiated-path mint-threw branch (canary.js:81-87) —
    // covers the apiCode-propagation contract on-call relies on for
    // alarm attribution. Symmetric to the upload-threw test above
    // which asserts the same on the upload step.
    const logger = require('../src/logger');
    const err = Object.assign(new Error('mint API 503'), { apiCode: 'qurl_unreachable' });
    mockMintLinks.mockRejectedValueOnce(err);
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(SEND_FILE_BODY);
    expect(res.status).toBe(500);
    expect(res.body.step).toBe('mint');
    expect(res.body.error).toBe('mint_threw');
    // reason + apiCode MUST NOT appear in the response body — they
    // live in logs only. The route strips them via destructuring;
    // pin the contract here.
    expect(res.body.reason).toBeUndefined();
    expect(res.body.apiCode).toBeUndefined();
    // … but they MUST appear in the structured log so on-call can
    // attribute alarms to a specific upstream failure mode.
    expect(logger.warn).toHaveBeenCalledWith(
      'Canary scenario failed',
      expect.objectContaining({
        step: 'mint',
        error: 'mint_threw',
        reason: 'mint API 503',
        apiCode: 'qurl_unreachable',
      })
    );
  });

  it('returns 500 scenario_threw when runScenario itself throws (sendDM rejects)', async () => {
    // The outer try/catch in canary.js:69-79 catches throws that
    // escape runScenario's inner per-step catches. sendDM swallows
    // its own errors and returns boolean today; a future refactor
    // that bubbles a throw must not silently break the synthesized-
    // failure path. Pin the contract.
    const logger = require('../src/logger');
    mockSendDM.mockRejectedValueOnce(new Error('discord client crash'));
    const res = await request(makeApp())
      .post('/canary/exec')
      .send(SEND_FILE_BODY);
    expect(res.status).toBe(500);
    expect(res.body.ok).toBe(false);
    expect(res.body.error).toBe('scenario_threw');
    expect(res.body.step).toBeNull();
    expect(res.body.reason).toBeUndefined();
    expect(logger.warn).toHaveBeenCalledWith(
      'Canary scenario threw',
      expect.objectContaining({
        test: 'send_file',
        recipient_user_id: VALID_USER_ID,
        error: 'discord client crash',
      })
    );
  });

  it('returns 400 invalid_recipient_user_id when only `test` is supplied (no recipient)', async () => {
    // Symmetric to the only-recipient case above. Pins that the
    // route validates both fields independently — a refactor that
    // collapses the checks into a single combined-presence guard
    // would regress this.
    const res = await request(makeApp())
      .post('/canary/exec')
      .send({ test: 'send_file' });
    expect(res.status).toBe(400);
    expect(res.body.error).toBe('invalid_recipient_user_id');
  });
});
