
const crypto = require('crypto');
const express = require('express');

jest.mock('../src/discord', () => ({
  assignContributorRole: jest.fn(),
  notifyPRMerge: jest.fn(),
  notifyBadgeEarned: jest.fn(),
  postGoodFirstIssue: jest.fn(),
  postReleaseAnnouncement: jest.fn(),
  postStarMilestone: jest.fn(),
  postToGitHubFeed: jest.fn(),
  sendDM: jest.fn(),
}));

const mockRecordQurlView = jest.fn(async () => ({ result: 'recorded', firstView: true }));
const mockFindSendsByQurlId = jest.fn(async () => []);
jest.mock('../src/store', () => ({
  recordQurlView: mockRecordQurlView,
  findSendsByQurlId: (...args) => mockFindSendsByQurlId(...args),
  getSendRenderState: jest.fn(async () => null),
  getSendItems: jest.fn(async () => []),
  getQurlViews: jest.fn(async () => new Map()),
  tryAdvanceRenderedCount: jest.fn(),
  healthCheck: jest.fn(),
  getStats: jest.fn(() => ({})),
}));

const mockEditInteractionReply = jest.fn(async () => ({ ok: true }));
jest.mock('../src/discord-rest', () => ({
  editDM: jest.fn(async () => ({ ok: true })),
  editInteractionReply: (...args) => mockEditInteractionReply(...args),
  sendChannelMessage: jest.fn(),
}));

let mockPrimed = true;
let mockWithinLag = false;
const mockOwnerSecrets = new Map();
mockOwnerSecrets.set('usr_test', 'test-qurl-secret');
jest.mock('../src/webhook-subscriptions', () => ({
  isPrimed: () => mockPrimed,
  isWithinSiblingLagWindow: () => mockWithinLag,
  getSecretForOwner: (ownerId) => mockOwnerSecrets.get(ownerId) || null,
  start: jest.fn(),
  stop: jest.fn(),
  upsertGuild: jest.fn(),
  removeGuild: jest.fn(),
  scanOnce: jest.fn(),
  _resetForTesting: jest.fn(),
}));

const mockAudit = jest.fn();
const mockLoggerWarn = jest.fn();
jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: mockLoggerWarn,
  error: jest.fn(),
  debug: jest.fn(),
  audit: mockAudit,
}));

process.env.DDB_TABLE_PREFIX = 'qurl-bot-discord-test-';
process.env.AWS_REGION = 'us-east-2';
process.env.BASE_URL = 'http://localhost:3000';

const request = require('supertest');
const { app } = require('../src/server');
const qurlWebhookRouter = require('../src/routes/qurl-webhook');

function signBody(rawJson, secret = 'test-qurl-secret') {
  return crypto.createHmac('sha256', secret).update(rawJson).digest('hex');
}

function buildReqBodyClobberingApp() {
  const testApp = express();
  testApp.set('trust proxy', 1);
  testApp.use('/webhooks', express.json({
    limit: '1mb',
    verify: (req, _res, buf) => { req.rawBody = buf; },
  }));
  testApp.use('/webhooks', (req, _res, next) => {
    req.body = {};
    next();
  });
  testApp.use('/webhooks', qurlWebhookRouter);
  return testApp;
}

function buildRawOnlyApp() {
  const testApp = express();
  testApp.set('trust proxy', 1);
  testApp.use('/webhooks', express.raw({
    type: '*/*',
    limit: '1mb',
    verify: (req, _res, buf) => { req.rawBody = buf; },
  }));
  testApp.use('/webhooks', qurlWebhookRouter);
  return testApp;
}

const VALID_PAYLOAD = {
  id: 'evt-1',
  type: 'qurl.accessed',
  data: { qurl_id: 'q_aaaaaaaaaa1', resource_id: 'r_111', access_count: 1, consumed: false },
  owner_id: 'usr_test',
  timestamp: '2026-05-19T12:00:00Z',
  api_version: '2024-01-01',
};

beforeEach(() => {
  jest.clearAllMocks();
  mockRecordQurlView.mockImplementation(async () => ({ result: 'recorded', firstView: true }));
  mockFindSendsByQurlId.mockImplementation(async () => []);
  mockEditInteractionReply.mockImplementation(async () => ({ ok: true }));
  mockPrimed = true;
  mockWithinLag = false;
  mockOwnerSecrets.clear();
  mockOwnerSecrets.set('usr_test', 'test-qurl-secret');
});

describe('POST /webhooks/qurl — sender view-counter fast-path gate (feat #60, PR-B)', () => {
  const signedRequest = (payload) => {
    const raw = JSON.stringify(payload);
    return request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
  };

  const drain = async (n = 8) => {
    for (let i = 0; i < n; i += 1) {
      await new Promise((resolve) => setImmediate(resolve));
    }
  };

  it('enters the fast-path on result === "recorded"', async () => {
    mockRecordQurlView.mockImplementation(async () => ({ result: 'recorded', firstView: true }));
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    await drain();
    expect(mockFindSendsByQurlId).toHaveBeenCalledTimes(1);
    expect(mockFindSendsByQurlId).toHaveBeenCalledWith(VALID_PAYLOAD.data.qurl_id);
  });

  it('does NOT enter the fast-path on dedup result', async () => {
    mockRecordQurlView.mockImplementation(async () => ({ result: 'dedup', firstView: false }));
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    await drain();
    expect(mockFindSendsByQurlId).not.toHaveBeenCalled();
  });

  it('does NOT enter the fast-path on any non-"recorded" result', async () => {
    for (const result of ['updated', 'noop', 'replayed', '', null, undefined]) {
      jest.clearAllMocks();
      mockRecordQurlView.mockImplementation(async () => ({ result, firstView: false }));
      await signedRequest(VALID_PAYLOAD);
      await drain();
      expect(mockFindSendsByQurlId).not.toHaveBeenCalled();
    }
  });
});

describe('POST /webhooks/qurl — subscription-registry primed-vs-unprimed semantics', () => {
  it('returns 503 when registry is unprimed (cold start / DDB scan in-flight)', async () => {
    mockPrimed = false;
    mockOwnerSecrets.clear();
    const raw = JSON.stringify(VALID_PAYLOAD);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(503);
  });

  it('returns 401 when registry is primed AND outside lag window AND owner_id is unknown', async () => {
    mockPrimed = true;
    mockWithinLag = false;
    mockOwnerSecrets.clear();
    const raw = JSON.stringify(VALID_PAYLOAD);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(401);
  });

  it('returns 503 when registry is primed but owner_id is unknown AND within lag window', async () => {
    mockPrimed = true;
    mockWithinLag = true;
    mockOwnerSecrets.clear();
    const raw = JSON.stringify(VALID_PAYLOAD);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(503);
  });
});

describe('POST /webhooks/qurl — signature verification', () => {
  it('accepts a request with a valid bare-hex HMAC over the raw body', async () => {
    const raw = JSON.stringify(VALID_PAYLOAD);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'recorded' });
    expect(mockRecordQurlView).toHaveBeenCalledWith(expect.objectContaining({
      qurlId: 'q_aaaaaaaaaa1',
      accessCount: 1,
      consumed: false,
      eventId: 'evt-1',
    }));
  });

  it('rejects a request with a wrong signature (401)', async () => {
    const raw = JSON.stringify(VALID_PAYLOAD);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw, 'wrong-secret'))
      .send(raw);
    expect(res.status).toBe(401);
    expect(mockRecordQurlView).not.toHaveBeenCalled();
  });

  it('rejects a request with a missing signature header (401)', async () => {
    const raw = JSON.stringify(VALID_PAYLOAD);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .send(raw);
    expect(res.status).toBe(401);
  });

  it('rejects a malformed signature (wrong length / non-hex)', async () => {
    const raw = JSON.stringify(VALID_PAYLOAD);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', 'not-a-hex-digest')
      .send(raw);
    expect(res.status).toBe(401);
  });

  it('rejects a sha256-prefixed signature (GitHub-style would be wrong wire shape)', async () => {
    const raw = JSON.stringify(VALID_PAYLOAD);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', 'sha256=' + signBody(raw))
      .send(raw);
    expect(res.status).toBe(401);
  });
});

describe('POST /webhooks/qurl — payload handling', () => {
  it('ignores non-qurl.accessed events with 200 (so qurl-service does not retry)', async () => {
    const payload = { ...VALID_PAYLOAD, type: 'qurl.created' };
    const raw = JSON.stringify(payload);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(200);
    expect(res.body).toEqual(expect.objectContaining({ status: 'ignored' }));
    expect(mockRecordQurlView).not.toHaveBeenCalled();
  });

  it('returns 200 invalid-payload when body.id missing (no replay-protection key)', async () => {
    const payload = { ...VALID_PAYLOAD };
    delete payload.id;
    const raw = JSON.stringify(payload);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'invalid-payload' });
    expect(mockRecordQurlView).not.toHaveBeenCalled();
  });

  it('returns 200 invalid-payload when qurl_id missing (no retry — payload is malformed, not transient)', async () => {
    const payload = { id: 'evt-1', type: 'qurl.accessed', owner_id: 'usr_test', data: { access_count: 1 } };
    const raw = JSON.stringify(payload);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'invalid-payload' });
    expect(mockRecordQurlView).not.toHaveBeenCalled();
  });

  it('returns 200 invalid-payload when access_count is negative', async () => {
    const payload = {
      id: 'evt-1', type: 'qurl.accessed', owner_id: 'usr_test',
      data: { qurl_id: 'q_aaaaaaaaaa1', access_count: -3, consumed: false },
    };
    const raw = JSON.stringify(payload);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'invalid-payload' });
  });

  it('returns 200 invalid-payload when access_count is 0 (rejected at the wire boundary)', async () => {
    const payload = {
      id: 'evt-zero', type: 'qurl.accessed', owner_id: 'usr_test',
      data: { qurl_id: 'q_aaaaaaaaaa1', access_count: 0, consumed: false },
    };
    const raw = JSON.stringify(payload);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'invalid-payload' });
    expect(mockRecordQurlView).not.toHaveBeenCalled();
    expect(mockFindSendsByQurlId).not.toHaveBeenCalled();
  });

  it('surfaces store dedup result on replay', async () => {
    mockRecordQurlView.mockResolvedValueOnce({ result: 'dedup', firstView: false });
    const raw = JSON.stringify(VALID_PAYLOAD);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'dedup' });
  });

  it('returns 500 (retriable) when the store throws — qurl-service redelivery is the recovery path', async () => {
    mockRecordQurlView.mockRejectedValueOnce(new Error('DDB throttled'));
    const raw = JSON.stringify(VALID_PAYLOAD);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(500);
  });

  it('passes body.id through verbatim as the eventId replay key', async () => {
    const raw = JSON.stringify(VALID_PAYLOAD);
    await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(mockRecordQurlView).toHaveBeenCalledWith(expect.objectContaining({
      eventId: 'evt-1',
    }));
  });

  it('rejects body.id that is not a string (e.g., an object slipped through)', async () => {
    const payload = { ...VALID_PAYLOAD, id: { weird: true } };
    const raw = JSON.stringify(payload);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'invalid-payload' });
    expect(mockRecordQurlView).not.toHaveBeenCalled();
  });

  it('rejects access_count=null (Number(null)===0 must not slip through)', async () => {
    const payload = { ...VALID_PAYLOAD, data: { ...VALID_PAYLOAD.data, access_count: null } };
    const raw = JSON.stringify(payload);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'invalid-payload' });
    expect(mockRecordQurlView).not.toHaveBeenCalled();
  });

  it('rejects fractional access_count (wire contract is Go int64; floats are a shape regression)', async () => {
    const payload = { ...VALID_PAYLOAD, data: { ...VALID_PAYLOAD.data, access_count: 1.5 } };
    const raw = JSON.stringify(payload);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'invalid-payload' });
    expect(mockRecordQurlView).not.toHaveBeenCalled();
  });

  it('treats consumed as boolean-only — the string "false" does NOT coerce to true', async () => {
    const payload = { ...VALID_PAYLOAD, data: { ...VALID_PAYLOAD.data, consumed: 'false' } };
    const raw = JSON.stringify(payload);
    await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(mockRecordQurlView).toHaveBeenCalledWith(expect.objectContaining({ consumed: false }));
  });
});

describe('POST /webhooks/qurl — unknown-owner limiter (looser threshold)', () => {
  beforeAll(() => {
    jest.resetModules();
  });
  it('returns 429 after 150 OWNER_UNKNOWN events from the same IP', async () => {
    // eslint-disable-next-line global-require
    const isolatedSubs = require('../src/webhook-subscriptions');
    expect(jest.isMockFunction(isolatedSubs.start)).toBe(true);
    // eslint-disable-next-line global-require
    const isolatedApp = require('../src/server').app;
    mockPrimed = true;
    mockOwnerSecrets.clear();
    mockOwnerSecrets.set('usr_test', 'test-qurl-secret');
    const unknownPayload = { ...VALID_PAYLOAD, owner_id: 'usr_unregistered' };
    const unknownRaw = JSON.stringify(unknownPayload);
    for (let i = 0; i < 150; i++) {
      // eslint-disable-next-line no-await-in-loop
      await request(isolatedApp)
        .post('/webhooks/qurl')
        .set('Content-Type', 'application/json')
        .set('QURL-Signature', signBody(unknownRaw))
        .send(unknownRaw);
    }
    const limited = await request(isolatedApp)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(unknownRaw))
      .send(unknownRaw);
    expect(limited.status).toBe(429);
  });
});

describe('POST /webhooks/qurl — bad-sig limiter scope (only HMAC failures count)', () => {
  beforeAll(() => {
    jest.resetModules();
  });
  it('does NOT increment bad-sig limiter on OWNER_UNKNOWN', async () => {
    // eslint-disable-next-line global-require
    const isolatedSubs = require('../src/webhook-subscriptions');
    expect(jest.isMockFunction(isolatedSubs.start)).toBe(true);
    // eslint-disable-next-line global-require
    const isolatedApp = require('../src/server').app;
    mockPrimed = true;
    mockOwnerSecrets.clear();
    mockOwnerSecrets.set('usr_test', 'test-qurl-secret');
    const unknownPayload = { ...VALID_PAYLOAD, owner_id: 'usr_unregistered' };
    const unknownRaw = JSON.stringify(unknownPayload);

    for (let i = 0; i < 30; i++) {
      // eslint-disable-next-line no-await-in-loop
      const r = await request(isolatedApp)
        .post('/webhooks/qurl')
        .set('Content-Type', 'application/json')
        .set('QURL-Signature', signBody(unknownRaw))
        .send(unknownRaw);
      expect(r.status).toBe(401);
    }

    const validRaw = JSON.stringify(VALID_PAYLOAD);
    const valid = await request(isolatedApp)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(validRaw))
      .send(validRaw);
    expect(valid.status).toBe(200);
  });
});

describe('POST /webhooks/qurl — multi-secret HMAC selection (BYOK view counter)', () => {
  it('picks the secret matching body.owner_id, not a sibling owner', async () => {
    mockOwnerSecrets.set('usr_test', 'secret-A');
    mockOwnerSecrets.set('usr_sibling', 'secret-B');
    const payload = { ...VALID_PAYLOAD, owner_id: 'usr_test' };
    const raw = JSON.stringify(payload);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw, 'secret-B'))
      .send(raw);
    expect(res.status).toBe(401);
    expect(mockRecordQurlView).not.toHaveBeenCalled();
  });

  it('accepts a request signed with the owner_id-resolved secret', async () => {
    mockOwnerSecrets.set('usr_byok_guild', 'guild-specific-secret');
    const payload = { ...VALID_PAYLOAD, owner_id: 'usr_byok_guild' };
    const raw = JSON.stringify(payload);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw, 'guild-specific-secret'))
      .send(raw);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'recorded' });
  });
});

describe('POST /webhooks/qurl — raw body owner_id parse failure modes', () => {
  it('extracts owner_id from req.rawBody even if req.body is clobbered before the router', async () => {
    const raw = JSON.stringify(VALID_PAYLOAD);
    const res = await request(buildReqBodyClobberingApp())
      .post('/webhooks/qurl')
      .set('X-Forwarded-For', '198.51.100.42')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'recorded' });
    expect(mockRecordQurlView).toHaveBeenCalledWith(expect.objectContaining({
      qurlId: VALID_PAYLOAD.data.qurl_id,
      eventId: VALID_PAYLOAD.id,
    }));
  });

  it('returns 401 when rawBody JSON parsing rejects a signed out-of-contract wire shape', async () => {
    const raw = Buffer.concat([
      Buffer.from([0xef, 0xbb, 0xbf]),
      Buffer.from(JSON.stringify(VALID_PAYLOAD)),
    ]);
    const res = await request(buildRawOnlyApp())
      .post('/webhooks/qurl')
      .set('X-Forwarded-For', '198.51.100.43')
      .set('Content-Type', 'application/octet-stream')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(401);
    expect(mockLoggerWarn).toHaveBeenCalledWith(
      'qURL webhook raw body JSON parse failed',
      expect.objectContaining({ error: expect.any(String) }),
    );
    expect(mockRecordQurlView).not.toHaveBeenCalled();
  });

  it('returns 401 when body.owner_id is missing entirely', async () => {
    const payload = { ...VALID_PAYLOAD };
    delete payload.owner_id;
    const raw = JSON.stringify(payload);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(401);
    expect(mockRecordQurlView).not.toHaveBeenCalled();
  });

  it('returns 401 when body.owner_id is non-string (e.g. object slipped through)', async () => {
    const payload = { ...VALID_PAYLOAD, owner_id: { weird: true } };
    const raw = JSON.stringify(payload);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(401);
  });

  it('returns 401 when body.owner_id is empty string', async () => {
    const payload = { ...VALID_PAYLOAD, owner_id: '' };
    const raw = JSON.stringify(payload);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(401);
  });

  it('rejects malformed JSON on the real /webhooks parser before the route handler runs', async () => {
    const raw = '{"owner_id":';
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect([400, 500]).toContain(res.status);
    expect(mockLoggerWarn).not.toHaveBeenCalledWith(
      'qURL webhook raw body JSON parse failed',
      expect.any(Object),
    );
    expect(mockRecordQurlView).not.toHaveBeenCalled();
  });

  it('rejects >1mb /webhooks bodies before the route handler runs', async () => {
    const big = JSON.stringify({ x: 'a'.repeat(2 * 1024 * 1024) });
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(big))
      .send(big);
    expect([400, 413, 500]).toContain(res.status);
    expect(mockRecordQurlView).not.toHaveBeenCalled();
  });

  it('fires CACHE_MISS_UNKNOWN_OWNER audit on missing body.owner_id', async () => {
    const payload = { ...VALID_PAYLOAD };
    delete payload.owner_id;
    const raw = JSON.stringify(payload);
    await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    const auditCalls = mockAudit.mock.calls;
    const ownerMissCall = auditCalls.find(([event]) => event === 'qurl_webhook_cache_miss_unknown_owner');
    expect(ownerMissCall).toBeDefined();
    expect(ownerMissCall[1]).toEqual(expect.objectContaining({ result: 'owner_id_missing' }));
  });
});

describe('POST /webhooks/qurl — bad-signature rate limit', () => {
  let isolatedApp;
  beforeAll(() => {
    jest.resetModules();
    // eslint-disable-next-line global-require
    isolatedApp = require('../src/server').app;
  });
  it('returns 429 once an IP crosses BAD_SIG_MAX failed-signature attempts', async () => {
    const raw = JSON.stringify(VALID_PAYLOAD);
    for (let i = 0; i < 30; i++) {
      // eslint-disable-next-line no-await-in-loop
      await request(isolatedApp)
        .post('/webhooks/qurl')
        .set('Content-Type', 'application/json')
        .set('QURL-Signature', signBody(raw, 'wrong-secret'))
        .send(raw);
    }
    const res = await request(isolatedApp)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(429);
  });
});
