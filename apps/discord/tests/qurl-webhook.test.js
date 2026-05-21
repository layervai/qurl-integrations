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

// Mock the view-update publisher (feat #60) so the route's
// `if (result === 'recorded') publish(...)` gate is observable.
// Without this mock, publish() would no-op against an unstarted
// publisher and the gate would be untestable.
const mockViewUpdatePublish = jest.fn();
jest.mock('../src/view-update-publisher', () => ({
  publish: mockViewUpdatePublish,
  start: jest.fn(),
  stop: jest.fn(),
}));

// Multi-secret subscription registry. The receiver now looks up the
// secret by `body.owner_id` instead of reading config.QURL_WEBHOOK_SECRET.
// Tests mock the lookup so VALID_PAYLOAD's owner_id resolves to the same
// secret signBody() uses; `mockPrimed` lets individual tests opt into
// the 503-unprimed and 401-unknown-owner paths.
let mockPrimed = true;
const mockOwnerSecrets = new Map();
mockOwnerSecrets.set('usr_test', 'test-qurl-secret');
jest.mock('../src/webhook-subscriptions', () => ({
  isPrimed: () => mockPrimed,
  getSecretForOwner: (ownerId) => mockOwnerSecrets.get(ownerId) || null,
  start: jest.fn(),
  stop: jest.fn(),
  upsertGuild: jest.fn(),
  removeGuild: jest.fn(),
  scanOnce: jest.fn(),
  _resetForTesting: jest.fn(),
}));

// Capture audit emissions so tests can assert that the receiver fires
// the right CloudWatch metric-filter event per failure branch.
const mockAudit = jest.fn();
jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: mockAudit,
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
  mockViewUpdatePublish.mockReset();
  // Reset cache-state mocks to "primed + the usr_test owner is known"
  // so each test starts from a predictable baseline. Tests that need
  // the unprimed / unknown-owner branches flip these explicitly.
  mockPrimed = true;
  mockOwnerSecrets.clear();
  mockOwnerSecrets.set('usr_test', 'test-qurl-secret');
});

describe('POST /webhooks/qurl — view-update push gate (feat #60)', () => {
  // The route calls viewUpdatePublisher.publish() ONLY when
  // recordQurlView returns the literal 'recorded' (real new view) —
  // NOT on dedup hits or any other status. A regression that flipped
  // to truthy-check (`if (result)`) would send SQS messages for every
  // dedup replay too, exhausting the queue + amplifying CloudWatch
  // costs on a high-replay-rate qurl.
  const signedRequest = (payload) => {
    const raw = JSON.stringify(payload);
    return request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
  };

  it('publishes on result === "recorded"', async () => {
    mockRecordQurlView.mockImplementation(async () => 'recorded');
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(mockViewUpdatePublish).toHaveBeenCalledTimes(1);
    expect(mockViewUpdatePublish).toHaveBeenCalledWith({
      qurlId: VALID_PAYLOAD.data.qurl_id,
      accessCount: VALID_PAYLOAD.data.access_count,
      eventId: VALID_PAYLOAD.id,
    });
  });

  it('does NOT publish on dedup result (e.g. "deduped")', async () => {
    mockRecordQurlView.mockImplementation(async () => 'deduped');
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(mockViewUpdatePublish).not.toHaveBeenCalled();
  });

  it('does NOT publish on any non-"recorded" result', async () => {
    // Strict-equality guard catches a future store regression that
    // returned 'updated' or other truthy strings — those should NOT
    // trigger a push.
    for (const result of ['updated', 'noop', 'replayed', '', null, undefined]) {
      jest.clearAllMocks();
      mockRecordQurlView.mockImplementation(async () => result);
      await signedRequest(VALID_PAYLOAD);
      expect(mockViewUpdatePublish).not.toHaveBeenCalled();
    }
  });
});

describe('POST /webhooks/qurl — subscription-registry primed-vs-unprimed semantics', () => {
  // Before the registry's first successful scan completes, ANY unknown
  // owner_id is "transiently unknown" rather than "genuinely unknown"
  // — return 503 (retriable). qurl-service retries 503 (1+2+4+8+16=31s
  // backoff) but NOT 401, so a 401 here would silently drop a guild's
  // very first views post-deploy.
  it('returns 503 when registry is unprimed (cold start / DDB scan in-flight)', async () => {
    // 503 fires when isPrimed()=false AND the owner_id lookup misses.
    // A primed cache where the owner is registered always wins (we
    // serve the request); only the unprimed-AND-miss combination
    // signals "transient gap, retry me".
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

  // After the registry is primed, an unknown owner is the real signal
  // — return 401 (truthful response). This is what an attacker probing
  // the endpoint with a fabricated owner_id would see.
  it('returns 401 when registry is primed but owner_id is unknown', async () => {
    mockPrimed = true;
    mockOwnerSecrets.clear(); // owner_id present in body but no entry registered
    const raw = JSON.stringify(VALID_PAYLOAD);
    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    expect(res.status).toBe(401);
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
    // owner_id must still be present + valid; the receiver verifies
    // HMAC + resolves the registry entry BEFORE checking body
    // payload shape, so an unsigned-or-unowned payload would 401
    // before this branch.
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

  it('returns 200 invalid-payload when access_count is 0 (parity with publisher gate)', async () => {
    // qurl.accessed events always carry access_count >= 1 by
    // contract. Rejecting 0 here keeps the wire boundary as the
    // single source of truth — without this gate, a 0-count event
    // would record in DDB and then trip the publisher's
    // "invalid accessCount" warn, producing an asymmetric log pair.
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
    expect(mockViewUpdatePublish).not.toHaveBeenCalled();
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

describe('POST /webhooks/qurl — unknown-owner limiter (looser threshold)', () => {
  // 150/min is the unknownOwnerLimiter ceiling; OWNER_UNKNOWN traffic
  // beyond that 429s. This pins the contract: an attacker probing the
  // receiver with bogus owner_id from a single IP IS rate-limited
  // (just at a looser threshold than HMAC brute-force), so we can't
  // accidentally regress to no-ceiling-at-all.
  //
  // FRAGILE: depends on jest.mock factory closures surviving
  // jest.resetModules — true today, but a future refactor that moves
  // these mocks into beforeEach would silently break the isolation.
  // Assert below that the isolated registry IS still the mock so a
  // regression fails this test loudly instead of silent 503s.
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
    // 150 burns the unknownOwnerLimiter exactly to its ceiling.
    for (let i = 0; i < 150; i++) {
      // eslint-disable-next-line no-await-in-loop
      await request(isolatedApp)
        .post('/webhooks/qurl')
        .set('Content-Type', 'application/json')
        .set('QURL-Signature', signBody(unknownRaw))
        .send(unknownRaw);
    }
    // 151st OWNER_UNKNOWN event 429s.
    const limited = await request(isolatedApp)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(unknownRaw))
      .send(unknownRaw);
    expect(limited.status).toBe(429);
  });
});

describe('POST /webhooks/qurl — bad-sig limiter scope (only HMAC failures count)', () => {
  // 30 OWNER_UNKNOWN events from one IP must NOT 429 the 31st valid
  // event — limiter is reserved for HMAC failures (attacker signal).
  beforeAll(() => {
    jest.resetModules();
  });
  it('does NOT increment bad-sig limiter on OWNER_UNKNOWN', async () => {
    // eslint-disable-next-line global-require
    const isolatedSubs = require('../src/webhook-subscriptions');
    expect(jest.isMockFunction(isolatedSubs.start)).toBe(true);
    // eslint-disable-next-line global-require
    const isolatedApp = require('../src/server').app;
    // Fresh ownership table: cache primed, owner not registered.
    mockPrimed = true;
    mockOwnerSecrets.clear();
    mockOwnerSecrets.set('usr_test', 'test-qurl-secret');
    const unknownPayload = { ...VALID_PAYLOAD, owner_id: 'usr_unregistered' };
    const unknownRaw = JSON.stringify(unknownPayload);

    // 30 OWNER_UNKNOWN events — if these incremented the bad-sig
    // counter, the 31st request below would 429.
    for (let i = 0; i < 30; i++) {
      // eslint-disable-next-line no-await-in-loop
      const r = await request(isolatedApp)
        .post('/webhooks/qurl')
        .set('Content-Type', 'application/json')
        .set('QURL-Signature', signBody(unknownRaw))
        .send(unknownRaw);
      expect(r.status).toBe(401);
    }

    // Switch to a valid signed event for a known owner; should pass.
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
  // The receiver MUST verify HMAC against the secret registered for the
  // body's owner_id — not against any other owner's secret. A regression
  // that fell back to "any registered secret" would let a guild A's
  // secret accept guild B's webhook, which is observationally fine
  // (HMAC just has to match SOME secret) but would let an attacker who
  // compromised any one guild's API key forge events for all other
  // guilds — security drift, not a bug a test would otherwise catch.
  it('picks the secret matching body.owner_id, not a sibling owner', async () => {
    mockOwnerSecrets.set('usr_test', 'secret-A');
    mockOwnerSecrets.set('usr_sibling', 'secret-B');
    // Sign with usr_sibling's secret but send under usr_test owner_id
    // — receiver should reject because secret-B doesn't match the
    // owner_id-resolved secret-A.
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

describe('POST /webhooks/qurl — body.owner_id parse failure modes', () => {
  // Pre-HMAC body parse is bounded (req.rawBody is 1mb-capped by
  // server.js middleware), so we don't worry about deep-nesting V8
  // exhaustion. The remaining gaps are missing/non-string owner_id
  // and a body that parsed-successfully but lacks the field.
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

  // Pins the metric-filter contract: OWNER_ID_MISSING shares the
  // CACHE_MISS_UNKNOWN_OWNER audit with OWNER_UNKNOWN (operational /
  // payload-shape signal, NOT HMAC-failure signal). A regression
  // that routed it to SIGNATURE_INVALID would mix it with HMAC
  // brute-force alarms.
  it('fires CACHE_MISS_UNKNOWN_OWNER audit on missing body.owner_id', async () => {
    const payload = { ...VALID_PAYLOAD };
    delete payload.owner_id;
    const raw = JSON.stringify(payload);
    await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', signBody(raw))
      .send(raw);
    // The receiver may emit OTHER audit events (rate-limit, cache-miss);
    // we only assert the OWNER_ID_MISSING-specific one fired with
    // the right event key and result value.
    const auditCalls = mockAudit.mock.calls;
    const ownerMissCall = auditCalls.find(([event]) => event === 'qurl_webhook_cache_miss_unknown_owner');
    expect(ownerMissCall).toBeDefined();
    expect(ownerMissCall[1]).toEqual(expect.objectContaining({ result: 'owner_id_missing' }));
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
