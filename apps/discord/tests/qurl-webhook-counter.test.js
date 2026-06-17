// Sender view-counter fast-path tests (feat #60, PR-B) — the
// cross-replica editSenderCounterInBackground path that edits the
// sender's "/qurl send" confirmation to "👀 N viewed / M pending" the
// instant a qurl.accessed view records, from ANY replica, via the
// persisted interaction-webhook token.
//
// Wire contract for the qurl.accessed branch that drives this:
//   {id, type: 'qurl.accessed',
//    data: {qurl_id, resource_id, access_count, consumed},
//    owner_id, timestamp, api_version}
//
// The fast-path runs FIRE-AND-FORGET off the already-returned 200 (the
// view recorded; a counter-edit miss must not make qurl-service retry the
// whole accessed event). It terminates by logging a single
// COUNTER_VERDICT_MSG debug line, so every assertion drains the deferred
// chain via flushCounter() (poll-for-verdict) first.
//
// LOAD-BEARING ORDERING (mirrors the source's numbered steps): the
// monotonic count advance (tryAdvanceRenderedCount) commits ONLY after a
// confirmed edit. The "failed-edit self-heal" test below is the anchor
// that fences the stuck-counter regression — on an edit failure the count
// must NOT advance, so the poll backstop re-renders.

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

const mockRecordQurlView = jest.fn();
const mockFindSendsByQurlId = jest.fn();
const mockGetSendRenderState = jest.fn();
const mockGetSendItems = jest.fn();
const mockGetQurlViews = jest.fn();
const mockTryAdvanceRenderedCount = jest.fn();
jest.mock('../src/store', () => ({
  recordQurlView: (...args) => mockRecordQurlView(...args),
  findSendsByQurlId: (...args) => mockFindSendsByQurlId(...args),
  getSendRenderState: (...args) => mockGetSendRenderState(...args),
  getSendItems: (...args) => mockGetSendItems(...args),
  getQurlViews: (...args) => mockGetQurlViews(...args),
  tryAdvanceRenderedCount: (...args) => mockTryAdvanceRenderedCount(...args),
  // consumed/expired markers are imported by the route module but unused
  // on the not-consumed accessed path these tests drive.
  markConsumedDMEdited: jest.fn(),
  clearConsumedDMEdited: jest.fn(),
  markExpiredDMEdited: jest.fn(),
  clearExpiredDMEdited: jest.fn(),
  isSendRevoked: jest.fn(),
  healthCheck: jest.fn(),
  getStats: jest.fn(() => ({})),
}));

const mockEditInteractionReply = jest.fn();
jest.mock('../src/discord-rest', () => ({
  editDM: jest.fn(async () => ({ ok: true })),
  editInteractionReply: (...args) => mockEditInteractionReply(...args),
  sendChannelMessage: jest.fn(),
}));

// Real renderViewCounter — let the fast-path render a real body so the
// "👀 N viewed" content assertion fences the byte-identity contract.

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

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

process.env.DDB_TABLE_PREFIX = 'qurl-bot-discord-test-';
process.env.AWS_REGION = 'us-east-2';
process.env.BASE_URL = 'http://localhost:3000';

const request = require('supertest');
const { app } = require('../src/server');
const logger = require('../src/logger');

function signBody(rawJson, secret = 'test-qurl-secret') {
  return crypto.createHmac('sha256', secret).update(rawJson).digest('hex');
}

const QURL_ID = 'q_aaaaaaaaaa1';
const SEND_ID = 'snd-1';
const SENDER_ID = 'usr-sender';
const APP_ID = 'app-123';
const TOKEN = 'interaction-tok-live'; // SENSITIVE in prod; a fixture here.

const VALID_PAYLOAD = {
  id: 'evt-counter-1',
  type: 'qurl.accessed',
  // consumed:false so the consumed-flip path stays out of the way — we're
  // testing the view-counter fast-path, which fires on dbResult==='recorded'.
  data: { qurl_id: QURL_ID, resource_id: 'r_111', access_count: 1, consumed: false },
  owner_id: 'usr_test',
  timestamp: '2026-05-19T12:00:00Z',
  api_version: '2024-01-01',
};

function signedRequest(payload = VALID_PAYLOAD) {
  const raw = JSON.stringify(payload);
  return request(app)
    .post('/webhooks/qurl')
    .set('Content-Type', 'application/json')
    .set('QURL-Signature', signBody(raw))
    .send(raw);
}

const VERDICT_MSG = 'qURL webhook sender-counter: fast-path verdict';

// Drain the fire-and-forget chain until the terminal verdict line lands
// (the uniform end signal on every branch), bounded so a path that never
// scheduled the fast-path returns instead of hanging.
async function flushCounter() {
  for (let i = 0; i < 50; i += 1) {
    await new Promise((resolve) => setImmediate(resolve));
    if (logger.debug.mock.calls.some(([msg]) => msg === VERDICT_MSG)) return;
  }
}

// A render state that arms the fast-path (token + appId + baseMsg present,
// not terminal). lastRenderedCount/expectedCount overridable per test.
function armedState(overrides = {}) {
  return {
    interactionToken: TOKEN,
    interactionAppId: APP_ID,
    expectedCount: 3,
    lastRenderedCount: 0,
    baseMsg: 'Sent to 3 users',
    // The send's qurl_id set is persisted on the render-state row, so the
    // fast-path counts views off it directly (getSendItems is only the
    // empty-set fallback). Default: one tracked, viewed → N = 1.
    qurlIds: [QURL_ID],
    terminal: false,
    ...overrides,
  };
}

beforeEach(() => {
  jest.clearAllMocks();
  mockPrimed = true;
  mockWithinLag = false;
  mockOwnerSecrets.clear();
  mockOwnerSecrets.set('usr_test', 'test-qurl-secret');
  mockRecordQurlView.mockResolvedValue('recorded');
  mockFindSendsByQurlId.mockResolvedValue([{ send_id: SEND_ID, sender_discord_id: SENDER_ID }]);
  mockGetSendRenderState.mockResolvedValue(armedState());
  // getSendItems is the empty-qurlIds fallback only — default armedState
  // carries qurlIds, so this normally isn't reached.
  mockGetSendItems.mockResolvedValue([{ qurl_id: QURL_ID, recipient_discord_id: 'r1' }]);
  mockGetQurlViews.mockResolvedValue(new Map([[QURL_ID, { accessCount: 1, consumed: false }]]));
  mockTryAdvanceRenderedCount.mockResolvedValue(true);
  mockEditInteractionReply.mockResolvedValue({ ok: true });
});

describe('sender view-counter fast-path — happy path', () => {
  it('a recorded view edits the confirmation to "👀 1 viewed", THEN advances the rendered count', async () => {
    const res = await signedRequest();
    // 200 reflects the PRIMARY op (view record), not the fast-path edit.
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'recorded' });

    await flushCounter();

    expect(mockEditInteractionReply).toHaveBeenCalledTimes(1);
    const [appId, token, payload] = mockEditInteractionReply.mock.calls[0];
    expect(appId).toBe(APP_ID);
    expect(token).toBe(TOKEN);
    expect(payload.content).toContain('👀 1 viewed');
    // Commit AFTER the edit — a count-before-edit inversion is the
    // stuck-counter regression. Assert the call ORDER.
    expect(mockTryAdvanceRenderedCount).toHaveBeenCalledWith(SEND_ID, 1);
    const editOrder = mockEditInteractionReply.mock.invocationCallOrder[0];
    const advanceOrder = mockTryAdvanceRenderedCount.mock.invocationCallOrder[0];
    expect(editOrder).toBeLessThan(advanceOrder);
  });
});

describe('sender view-counter fast-path — content-only (buttons preserved)', () => {
  it('the editInteractionReply payload carries ONLY content — NO components key', async () => {
    // Discord PATCH .../messages/@original is a PARTIAL update: omitting
    // `components` preserves the Add/Revoke buttons; sending components:[]
    // would clear them. The fast-path must send content alone.
    await signedRequest();
    await flushCounter();
    const payload = mockEditInteractionReply.mock.calls[0][2];
    expect(payload).toEqual({ content: expect.any(String) });
    expect(payload).not.toHaveProperty('components');
  });
});

describe('sender view-counter fast-path — terminal skip', () => {
  it('a terminal (revoked/closed) confirmation is NOT resurrected', async () => {
    mockGetSendRenderState.mockResolvedValue(armedState({ terminal: true }));
    await signedRequest();
    await flushCounter();
    expect(mockEditInteractionReply).not.toHaveBeenCalled();
    expect(mockTryAdvanceRenderedCount).not.toHaveBeenCalled();
    // Terminal is checked BEFORE reading the send items (step 3 < step 5).
    expect(mockGetSendItems).not.toHaveBeenCalled();
  });
});

describe('sender view-counter fast-path — absent-guard', () => {
  it('no token / no base → no edit (the poll backstop covers it)', async () => {
    // Legacy send / pre-feature / token TTL'd away.
    mockGetSendRenderState.mockResolvedValue(armedState({ interactionToken: null, baseMsg: undefined }));
    await signedRequest();
    await flushCounter();
    expect(mockEditInteractionReply).not.toHaveBeenCalled();
    expect(mockTryAdvanceRenderedCount).not.toHaveBeenCalled();
  });
});

describe('sender view-counter fast-path — pre-read compare (N <= L)', () => {
  it('N=1 with lastRenderedCount=2 → no edit (redelivery / higher count already shown)', async () => {
    mockGetSendRenderState.mockResolvedValue(armedState({ lastRenderedCount: 2 }));
    // Only one qurl viewed → N = 1, which is <= L = 2.
    await signedRequest();
    await flushCounter();
    expect(mockEditInteractionReply).not.toHaveBeenCalled();
    expect(mockTryAdvanceRenderedCount).not.toHaveBeenCalled();
  });
});

describe('sender view-counter fast-path — FAILED-EDIT SELF-HEAL (load-bearing)', () => {
  it('editInteractionReply {ok:false} → tryAdvanceRenderedCount is NOT called (count stays; poll re-renders)', async () => {
    // THE invariant that prevents the stuck-counter regression: on a
    // transient edit failure the rendered count must NOT advance, so
    // last_rendered_count stays at L and the poll backstop will re-render
    // and self-heal. If the advance ever moved before/independent of the
    // edit's success, this fails.
    mockEditInteractionReply.mockResolvedValue({ ok: false, status: 500 });
    await signedRequest();
    await flushCounter();
    expect(mockEditInteractionReply).toHaveBeenCalledTimes(1);
    expect(mockTryAdvanceRenderedCount).not.toHaveBeenCalled();
  });
});

describe('sender view-counter fast-path — N is DISTINCT viewed qurl_ids, not the event access_count', () => {
  it('counts qurl_ids with accessCount>0 across the send (2 of 3 viewed → "👀 2 viewed")', async () => {
    // qurl_id set comes off the persisted render state (no getSendItems).
    mockGetSendRenderState.mockResolvedValue(armedState({ qurlIds: ['q_a', 'q_b', 'q_c'] }));
    mockGetQurlViews.mockResolvedValue(new Map([
      ['q_a', { accessCount: 5, consumed: false }], // viewed (count is irrelevant — distinct, not summed)
      ['q_b', { accessCount: 1, consumed: false }], // viewed
      // q_c absent from the map → not viewed
    ]));
    await signedRequest();
    await flushCounter();
    const payload = mockEditInteractionReply.mock.calls[0][2];
    expect(payload.content).toContain('👀 2 viewed');
    expect(mockTryAdvanceRenderedCount).toHaveBeenCalledWith(SEND_ID, 2);
    // qurl_ids came from the render-state row — the recipient-row Query
    // fallback was not needed.
    expect(mockGetSendItems).not.toHaveBeenCalled();
  });

  it('falls back to getSendItems when the render state has no persisted qurl_ids', async () => {
    mockGetSendRenderState.mockResolvedValue(armedState({ qurlIds: [] }));
    mockGetSendItems.mockResolvedValue([
      { qurl_id: 'q_a', recipient_discord_id: 'r1' },
      { qurl_id: 'q_b', recipient_discord_id: 'r2' },
    ]);
    mockGetQurlViews.mockResolvedValue(new Map([['q_a', { accessCount: 1, consumed: false }]]));
    await signedRequest();
    await flushCounter();
    expect(mockGetSendItems).toHaveBeenCalledWith(SEND_ID, SENDER_ID);
    const payload = mockEditInteractionReply.mock.calls[0][2];
    expect(payload.content).toContain('👀 1 viewed');
  });
});

describe('sender view-counter fast-path — defensive row-count skip', () => {
  it('findSendsByQurlId returns != 1 row → no edit', async () => {
    mockFindSendsByQurlId.mockResolvedValue([]); // 0 = pre-rollout / missing
    await signedRequest();
    await flushCounter();
    expect(mockGetSendRenderState).not.toHaveBeenCalled();
    expect(mockEditInteractionReply).not.toHaveBeenCalled();
  });
});
