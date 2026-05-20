// qURL webhook receiver tests
//
// Pins the wire contract with qurl-service `domain.WebhookEvent`:
//   header `QURL-Signature` = bare hex HMAC-SHA256 (no `sha256=` prefix)
//   body   {id, type, data:{qurl_id, resource_id, access_count, consumed},
//           owner_id, timestamp, api_version}
//
// Field names `type` and `id` (NOT `event` or `event_id`) match the
// qurl-service emit-side. A rename on either side without the matching
// rename on the other silently 200-ignores every webhook — these tests
// pin the names so the regression fails CI loudly.
//
// Different from routes/webhooks.js (GitHub) — that one uses
// `X-Hub-Signature-256: sha256=…`. Do not let the two routes drift.

const crypto = require('crypto');

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

const mockRecordQurlView = jest.fn(async () => 'recorded');
jest.mock('../src/store', () => ({
  recordQurlView: mockRecordQurlView,
  // No-op stubs for the other store methods server.js touches at boot
  // (healthCheck via /health) so the request flow doesn't reach for
  // real DDB credentials.
  healthCheck: jest.fn(),
  getStats: jest.fn(() => ({})),
}));

process.env.QURL_WEBHOOK_SECRET = 'test-qurl-secret';
process.env.DDB_TABLE_PREFIX = 'qurl-bot-discord-test-';
process.env.AWS_REGION = 'us-east-2';
process.env.BASE_URL = 'http://localhost:3000';

const request = require('supertest');
const { app } = require('../src/server');

function signBody(rawJson, secret = 'test-qurl-secret') {
  return crypto.createHmac('sha256', secret).update(rawJson).digest('hex');
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
  mockRecordQurlView.mockImplementation(async () => 'recorded');
});

describe('POST /webhooks/qurl — boot-race PLACEHOLDER handling', () => {
  // During the brief window between server-listen and auto-register
  // resolving, config.QURL_WEBHOOK_SECRET may still hold the
  // terraform-seeded PLACEHOLDER. The receiver MUST 503 (qurl-service
  // retriable) on this case rather than 401 (non-retriable) — a 401
  // would drop in-flight events permanently.
  it('returns 503 when secret is the literal PLACEHOLDER (boot race)', async () => {
    // eslint-disable-next-line global-require
    const config = require('../src/config');
    const original = config.QURL_WEBHOOK_SECRET;
    // Use the setter (not direct mutation) — single canonical path
    // means a future rename of QURL_WEBHOOK_SECRET → setSecret would
    // fail this test loudly rather than silently mutate the wrong key.
    config.setQurlWebhookSecret('PLACEHOLDER');
    try {
      const res = await request(app)
        .post('/webhooks/qurl')
        .set('Content-Type', 'application/json')
        .set('QURL-Signature', '0'.repeat(64))
        .send('{}');
      expect(res.status).toBe(503);
      expect(res.body).toEqual({ error: 'Webhook receiver not configured' });
    } finally {
      config.setQurlWebhookSecret(original);
    }
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
    // Catches the most likely "copied from GitHub webhook" regression
    // — qurl-service sends BARE hex; a 'sha256=' prefix never matches
    // the strict 64-hex-char regex.
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
    const payload = { id: 'evt-1', type: 'qurl.accessed', data: { access_count: 1 } };
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
      id: 'evt-1', type: 'qurl.accessed',
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

  it('surfaces store dedup result on replay', async () => {
    mockRecordQurlView.mockResolvedValueOnce('dedup');
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
    // Regression guard for the receiver's typeof guard. Without it the
    // non-scalar would persist as the DDB replay key — silent corruption
    // of dedup semantics for downstream events with that id.
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
    // Regression guard for strict === true vs Boolean() coercion.
    // If qurl-service ever JSON-encodes consumed as a string, the
    // receiver must NOT silently flip the wrong way.
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

// Each describe re-loads the module so the per-instance rate-limit Map
// doesn't accumulate bad-sig counts from earlier suites. jest.isolateModules
// would also work; resetModules + reset of the audit-payload mock is the
// minimal-surface choice.
describe('POST /webhooks/qurl — bad-signature rate limit', () => {
  let isolatedApp;
  beforeAll(() => {
    jest.resetModules();
    // eslint-disable-next-line global-require
    isolatedApp = require('../src/server').app;
  });
  it('returns 429 once an IP crosses BAD_SIG_MAX failed-signature attempts', async () => {
    const raw = JSON.stringify(VALID_PAYLOAD);
    // 30 failed attempts crosses the BAD_SIG_MAX threshold.
    for (let i = 0; i < 30; i++) {
      // eslint-disable-next-line no-await-in-loop
      await request(isolatedApp)
        .post('/webhooks/qurl')
        .set('Content-Type', 'application/json')
        .set('QURL-Signature', signBody(raw, 'wrong-secret'))
        .send(raw);
    }
    // 31st request hits the rate limit before signature check.
    const res = await request(isolatedApp)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(429);
  });
});
