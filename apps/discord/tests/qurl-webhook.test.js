// qURL webhook receiver tests
//
// Pins the wire contract with qurl-service:
//   header `QURL-Signature` = bare hex HMAC-SHA256 (no `sha256=` prefix)
//   body   {event, event_id, data:{qurl_id, resource_id, access_count, consumed}}
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
  event: 'qurl.accessed',
  event_id: 'evt-1',
  data: { qurl_id: 'q_aaaaaaaaaa1', resource_id: 'r_111', access_count: 1, consumed: false },
};

beforeEach(() => {
  jest.clearAllMocks();
  mockRecordQurlView.mockImplementation(async () => 'recorded');
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
    const payload = { ...VALID_PAYLOAD, event: 'qurl.created' };
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

  it('returns 200 invalid-payload when qurl_id missing (no retry — payload is malformed, not transient)', async () => {
    const payload = { event: 'qurl.accessed', event_id: 'e1', data: { access_count: 1 } };
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
      event: 'qurl.accessed', event_id: 'e1',
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

  it('falls back to a body-hash event_id when payload omits event_id and message_id', async () => {
    // Older qurl-service revs may not include event_id. The body-hash
    // fallback guarantees a literal redelivery still dedups (same body →
    // same hash → conditional update rejects on last_event_id match).
    const payload = {
      event: 'qurl.accessed',
      data: { qurl_id: 'q_aaaaaaaaaa1', access_count: 2, consumed: true },
    };
    const raw = JSON.stringify(payload);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(200);
    const call = mockRecordQurlView.mock.calls[0][0];
    expect(call.eventId).toMatch(/^[0-9a-f]{64}$/); // SHA-256 hex
  });
});

describe('POST /webhooks/qurl — bad-signature rate limit', () => {
  it('returns 429 once an IP crosses BAD_SIG_MAX failed-signature attempts', async () => {
    const raw = JSON.stringify(VALID_PAYLOAD);
    // 30 failed attempts crosses the BAD_SIG_MAX threshold.
    for (let i = 0; i < 30; i++) {
      // eslint-disable-next-line no-await-in-loop
      await request(app)
        .post('/webhooks/qurl')
        .set('Content-Type', 'application/json')
        .set('QURL-Signature', signBody(raw, 'wrong-secret'))
        .send(raw);
    }
    // 31st request hits the rate limit before signature check.
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(429);
  });
});
