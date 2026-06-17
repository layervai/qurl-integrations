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
const mockIncrementSendViewedCount = jest.fn();
const mockGetSendViewedCount = jest.fn();
const mockTryAdvanceRenderedCount = jest.fn();
const mockTouchRenderedAt = jest.fn();
jest.mock('../src/store', () => ({
  recordQurlView: (...args) => mockRecordQurlView(...args),
  findSendsByQurlId: (...args) => mockFindSendsByQurlId(...args),
  getSendRenderState: (...args) => mockGetSendRenderState(...args),
  getSendItems: (...args) => mockGetSendItems(...args),
  getQurlViews: (...args) => mockGetQurlViews(...args),
  incrementSendViewedCount: (...args) => mockIncrementSendViewedCount(...args),
  getSendViewedCount: (...args) => mockGetSendViewedCount(...args),
  tryAdvanceRenderedCount: (...args) => mockTryAdvanceRenderedCount(...args),
  // Failure-path debounce stamp — MUST be on the mock or the new
  // touchRenderedAt call on the !r.ok branch throws into the fast-path's
  // .catch and the coalescing-on-failure test goes vacuous.
  touchRenderedAt: (...args) => mockTouchRenderedAt(...args),
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
const { app, stopIntervals } = require('../src/server');
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
    viewedCount: 1,
    lastRenderedCount: 0,
    // 0 = never edited → always older than the coalesce window, so the
    // first edit of a send is never debounced. Tests that exercise the
    // cooldown override this with a recent epoch-MS instant.
    lastRenderedAt: 0,
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
  mockRecordQurlView.mockResolvedValue({ result: 'recorded', firstView: true });
  mockFindSendsByQurlId.mockResolvedValue([{ send_id: SEND_ID, sender_discord_id: SENDER_ID }]);
  mockGetSendRenderState.mockResolvedValue(armedState());
  // getSendItems is the empty-qurlIds fallback only — default armedState
  // carries qurlIds, so this normally isn't reached.
  mockGetSendItems.mockResolvedValue([{ qurl_id: QURL_ID, recipient_discord_id: 'r1' }]);
  mockGetQurlViews.mockResolvedValue(new Map([[QURL_ID, { accessCount: 1, consumed: false }]]));
  mockIncrementSendViewedCount.mockResolvedValue(undefined);
  mockGetSendViewedCount.mockResolvedValue(1);
  mockTryAdvanceRenderedCount.mockResolvedValue(true);
  mockTouchRenderedAt.mockResolvedValue(undefined);
  mockEditInteractionReply.mockResolvedValue({ ok: true });
});

afterEach(() => {
  stopIntervals();
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
    expect(mockIncrementSendViewedCount).toHaveBeenCalledWith(SEND_ID, QURL_ID, 3);
    expect(mockGetSendViewedCount).toHaveBeenCalledWith(SEND_ID, 3);
    expect(mockGetQurlViews).not.toHaveBeenCalled();
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
    mockRecordQurlView.mockResolvedValue({ result: 'recorded', firstView: false });
    mockGetSendRenderState.mockResolvedValue(armedState({ lastRenderedCount: 2, viewedCount: 1 }));
    // Only one qurl viewed → N = 1, which is <= L = 2.
    await signedRequest();
    await flushCounter();
    expect(mockEditInteractionReply).not.toHaveBeenCalled();
    expect(mockTryAdvanceRenderedCount).not.toHaveBeenCalled();
  });

  it('NO BACKWARDS STEP: after the poll advanced the shared floor to 3, a stale fast-path read of N=2 does NOT edit', async () => {
    // cr-found gap, now closed by the shared monotonic floor: the poll
    // renders the settled count (e.g. 3) and advances last_rendered_count
    // to 3 (monitorLinkStatus → tryAdvanceRenderedCount). A later
    // fast-path event whose eventually-consistent getQurlViews reads a
    // stale N=2 must NOT PATCH "2 viewed" over the displayed "3 viewed".
    // The step-6 N<=L guard (L=3 from the persisted floor the poll wrote)
    // is exactly what fences it — without the poll sharing the floor, L
    // would lag at the last FAST-PATH-rendered value and this would edit
    // backwards.
    mockGetSendRenderState.mockResolvedValue(armedState({
      lastRenderedCount: 3,
      viewedCount: 2,
      qurlIds: ['q_a', 'q_b', 'q_c'],
    }));
    mockRecordQurlView.mockResolvedValue({ result: 'recorded', firstView: false });
    await signedRequest();
    await flushCounter();
    expect(mockEditInteractionReply).not.toHaveBeenCalled();
    expect(mockTryAdvanceRenderedCount).not.toHaveBeenCalled();
  });
});

describe('sender view-counter fast-path — FAILED-EDIT SELF-HEAL (load-bearing)', () => {
  it('editInteractionReply {ok:false} → tryAdvanceRenderedCount NOT called (count stays), but touchRenderedAt IS (debounce armed)', async () => {
    // THE invariant that prevents the stuck-counter regression: on a
    // transient edit failure the rendered COUNT must NOT advance, so
    // last_rendered_count stays at L and the poll backstop will re-render
    // and self-heal. If the advance ever moved before/independent of the
    // edit's success, this fails.
    mockEditInteractionReply.mockResolvedValue({ ok: false, status: 500 });
    await signedRequest();
    await flushCounter();
    expect(mockEditInteractionReply).toHaveBeenCalledTimes(1);
    expect(mockTryAdvanceRenderedCount).not.toHaveBeenCalled();
    // …but the debounce CLOCK is still stamped (touchRenderedAt), so a
    // burst against an erroring Discord coalesces instead of re-attempting
    // a PATCH per view.
    expect(mockTouchRenderedAt).toHaveBeenCalledWith(SEND_ID);
  });

  it('COALESCES ON FAILURE: a 2nd view within the window after a failed edit is debounced (not re-attempted)', async () => {
    // The failure-path bug the gate would otherwise have: last_rendered_at
    // is success-only, so during an edit outage every burst view sees
    // last_rendered_at=0 and re-PATCHes — the exact 429 storm the gate
    // prevents. touchRenderedAt arms the clock on the failed attempt; the
    // 2nd view (render state now reflects the stamp) must skip.
    mockEditInteractionReply.mockResolvedValue({ ok: false, status: 500 });
    // View 1: lastRenderedAt=0 → gate passes → edit fails → touchRenderedAt.
    await signedRequest();
    await flushCounter();
    expect(mockEditInteractionReply).toHaveBeenCalledTimes(1);
    expect(mockTouchRenderedAt).toHaveBeenCalledTimes(1);

    // Now the row's last_rendered_at is fresh (the touch landed). Model
    // that: the next getSendRenderState reflects a recent stamp.
    logger.debug.mockClear();
    mockGetSendRenderState.mockResolvedValue(armedState({ lastRenderedAt: Date.now() }));
    // View 2 within the window → coalesced: no immediate 2nd edit, no BatchGet.
    await signedRequest({ ...VALID_PAYLOAD, id: 'evt-counter-2' });
    await flushCounter();
    expect(mockEditInteractionReply).toHaveBeenCalledTimes(1); // still just the 1st
    expect(mockTouchRenderedAt).toHaveBeenCalledTimes(1);      // no 2nd attempt to stamp
  });
});

describe('sender view-counter fast-path — N is the distinct viewed-count aggregate, not the event access_count', () => {
  it('renders the sharded viewed-count aggregate (2 of 3 viewed → "👀 2 viewed")', async () => {
    mockGetSendRenderState.mockResolvedValue(armedState({ viewedCount: 0, qurlIds: ['q_a', 'q_b', 'q_c'] }));
    mockGetSendViewedCount.mockResolvedValue(2);
    await signedRequest();
    await flushCounter();
    const payload = mockEditInteractionReply.mock.calls[0][2];
    expect(payload.content).toContain('👀 2 viewed');
    expect(mockTryAdvanceRenderedCount).toHaveBeenCalledWith(SEND_ID, 2);
    expect(mockGetSendViewedCount).toHaveBeenCalledWith(SEND_ID, 3);
    expect(mockGetQurlViews).not.toHaveBeenCalled();
    expect(mockGetSendItems).not.toHaveBeenCalled();
  });

  it('sharded aggregate increment throttle skips the fast-path and leaves the poll backstop to count qurl views', async () => {
    mockIncrementSendViewedCount.mockRejectedValue(new Error('ProvisionedThroughputExceededException'));
    await signedRequest();
    await flushCounter();
    expect(mockEditInteractionReply).not.toHaveBeenCalled();
    expect(mockTryAdvanceRenderedCount).not.toHaveBeenCalled();
    expect(mockGetQurlViews).not.toHaveBeenCalled();
    expect(logger.warn).toHaveBeenCalledWith(
      'qURL webhook sender-counter: sharded aggregate increment failed; poll backstop will count qurl views',
      expect.objectContaining({ qurl_id: QURL_ID, send_id: SEND_ID }),
    );
    expect(logger.debug).toHaveBeenCalledWith(
      VERDICT_MSG,
      expect.objectContaining({ qurl_id: QURL_ID, status: 'aggregate-update-error' }),
    );
  });

  it('legacy fallback: uses getSendItems/getQurlViews when viewed_count and qurl_ids are absent', async () => {
    mockRecordQurlView.mockResolvedValue({ result: 'recorded', firstView: false });
    mockGetSendRenderState.mockResolvedValue(armedState({ viewedCount: null, qurlIds: [] }));
    mockGetSendViewedCount.mockResolvedValue(0);
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

describe('sender view-counter fast-path — edit coalescing (leading-edge debounce)', () => {
  it('a 2nd view within the coalesce window is SKIPPED before the BatchGet and schedules a trailing flush', async () => {
    // The send already rendered ~200ms ago (well inside the sub-second
    // window). A fresh view must NOT edit — it would storm Discord on a
    // high-fan-out send. Crucially the skip happens BEFORE any aggregate
    // read or getQurlViews fallback; the first-view shard write is enough
    // to know a trailing flush has work to render.
    mockGetSendRenderState.mockResolvedValue(armedState({
      lastRenderedCount: 1,
      lastRenderedAt: Date.now() - 200,
      viewedCount: 2,
      qurlIds: ['q_a', 'q_b', 'q_c'],
    }));
    await signedRequest();
    await flushCounter();
    expect(mockEditInteractionReply).not.toHaveBeenCalled();
    expect(mockTryAdvanceRenderedCount).not.toHaveBeenCalled();
    expect(mockGetQurlViews).not.toHaveBeenCalled();
    expect(mockGetSendViewedCount).not.toHaveBeenCalled();
    expect(mockGetSendItems).not.toHaveBeenCalled();
    expect(logger.debug).toHaveBeenCalledWith(
      'qURL webhook sender-counter: coalesced — scheduled trailing flush',
      expect.objectContaining({ send_id: SEND_ID }),
    );
  });

  it('a view OLDER than the coalesce window edits normally', async () => {
    // 10s since the last edit is well past the sub-second window → not debounced.
    mockGetSendRenderState.mockResolvedValue(armedState({
      lastRenderedCount: 0,
      lastRenderedAt: Date.now() - 10_000,
      viewedCount: 1,
    }));
    await signedRequest();
    await flushCounter();
    expect(mockEditInteractionReply).toHaveBeenCalledTimes(1);
    expect(mockTryAdvanceRenderedCount).toHaveBeenCalledWith(SEND_ID, 1);
  });

  it('scheduled trailing flush fires and edits the settled aggregate without waiting for the poll', async () => {
    mockGetSendRenderState.mockResolvedValue(armedState({
      lastRenderedCount: 1,
      lastRenderedAt: Date.now() - 850,
      viewedCount: 2,
      qurlIds: ['q_a', 'q_b'],
    }));
    mockGetSendViewedCount.mockResolvedValue(2);

    await signedRequest();
    await flushCounter();
    expect(mockEditInteractionReply).not.toHaveBeenCalled();
    expect(logger.debug).toHaveBeenCalledWith(
      'qURL webhook sender-counter: coalesced — scheduled trailing flush',
      expect.objectContaining({ send_id: SEND_ID }),
    );

    logger.debug.mockClear();
    await new Promise((resolve) => setTimeout(resolve, 75));
    await flushCounter();

    expect(mockEditInteractionReply).toHaveBeenCalledTimes(1);
    const payload = mockEditInteractionReply.mock.calls[0][2];
    expect(payload.content).toContain('👀 2 viewed');
    expect(mockTryAdvanceRenderedCount).toHaveBeenCalledWith(SEND_ID, 2);
  });

  it('CAS-lost lower edit schedules exactly one repair-floor re-render', async () => {
    mockGetSendRenderState
      .mockResolvedValueOnce(armedState({
        expectedCount: 3,
        lastRenderedCount: 1,
        lastRenderedAt: 0,
        viewedCount: 1,
        qurlIds: ['q_a', 'q_b', 'q_c'],
      }))
      .mockResolvedValueOnce(armedState({
        expectedCount: 3,
        lastRenderedCount: 2,
        lastRenderedAt: Date.now(),
        viewedCount: 2,
        qurlIds: ['q_a', 'q_b', 'q_c'],
      }));
    mockGetSendViewedCount.mockResolvedValue(2);
    mockTryAdvanceRenderedCount.mockResolvedValue(false);

    await signedRequest();
    await new Promise((resolve) => setTimeout(resolve, 5));
    await flushCounter();

    expect(mockEditInteractionReply).toHaveBeenCalledTimes(2);
    expect(mockTryAdvanceRenderedCount).toHaveBeenCalledTimes(2);
    expect(mockEditInteractionReply.mock.calls[1][2].content).toContain('👀 2 viewed');
  });

  it('BURST: many views in a short window → bounded edit count (≤3, not N), final render correct', async () => {
    // Simulate a high-fan-out send: a wave of qurl.accessed webhooks for
    // DISTINCT recipients arriving inside one coalesce window. The store
    // mocks share in-test state that mirrors the real split: render floor
    // and coalesce clock live on qurl_send_configs, while distinct first
    // views advance sharded qurl_views counters. This exercises the REAL
    // leading-edge gate end-to-end across replicas, not a mocked verdict.
    const TRACKED = Array.from({ length: 30 }, (_, i) => `q_burst_${i}`);
    // The row as the store sees it. last_rendered_at starts 0 (never
    // edited) so the first webhook is NOT debounced.
    const row = { shardedCount: 0, lastRenderedCount: 0, lastRenderedAt: 0 };

    mockFindSendsByQurlId.mockResolvedValue([{ send_id: SEND_ID, sender_discord_id: SENDER_ID }]);
    mockGetSendRenderState.mockImplementation(async () => armedState({
      expectedCount: TRACKED.length,
      viewedCount: 0,
      lastRenderedCount: row.lastRenderedCount,
      lastRenderedAt: row.lastRenderedAt,
      qurlIds: TRACKED,
    }));
    mockIncrementSendViewedCount.mockImplementation(async () => {
      row.shardedCount += 1;
    });
    mockGetSendViewedCount.mockImplementation(async () => row.shardedCount);
    // Commit-after-edit CAS: advance only if strictly higher; stamp the
    // debounce clock to "now" so subsequent webhooks in the window skip.
    mockTryAdvanceRenderedCount.mockImplementation(async (_sendId, n) => {
      if (n > row.lastRenderedCount) {
        row.lastRenderedCount = n;
        row.lastRenderedAt = Date.now();
        return true;
      }
      return false;
    });

    // Fire 30 distinct-recipient views back-to-back within one window.
    // incrementSendViewedCount advances a shard BEFORE each render
    // decision, so every chain would see a strictly higher N than the
    // last committed last_rendered_count if it performed the shard sum.
    // Only the 4b coalesce gate suppresses most edit attempts before
    // that read.
    // That makes this a genuine discriminator for the GATE, not the
    // pre-existing dedup: with the gate disabled this asserts 30 edits
    // (verified red), with it ~1. (If a future edit lets the N<=L skip
    // carry this test, the gate-disabled red-check stops failing — re-pin.)
    for (let i = 0; i < TRACKED.length; i += 1) {
      const payload = {
        ...VALID_PAYLOAD,
        id: `evt-burst-${i}`,
        data: { ...VALID_PAYLOAD.data, qurl_id: TRACKED[i] },
      };
      // eslint-disable-next-line no-await-in-loop
      await signedRequest(payload);
    }
    // Drain every scheduled fast-path chain.
    for (let i = 0; i < 200; i += 1) {
      // eslint-disable-next-line no-await-in-loop
      await new Promise((resolve) => setImmediate(resolve));
    }

    // Coalesced: the leading edge edits (lastRenderedAt was 0), then the
    // rest of the burst lands inside the window and is debounced. With
    // synchronous in-test timing the whole burst falls in one window, so
    // exactly ONE edit fires — assert the bound holds (<= 3 << 30) rather
    // than pinning the exact count, since real wall-clock could straddle
    // the window and admit a second leading edge.
    //
    // SCOPE OF THIS BOUND: the mock makes tryAdvanceRenderedCount's stamp
    // synchronously visible to the next getSendRenderState, so this proves
    // single-replica SEQUENTIAL coalescing. It does NOT prove the
    // distributed bound — in prod, M replicas reading an eventually-
    // consistent last_rendered_at before any commits can each fire one
    // leading edge per window (~M/window, see the route's COALESCING
    // header). Do not read <=3 here as a cross-replica guarantee.
    expect(mockEditInteractionReply.mock.calls.length).toBeLessThanOrEqual(3);
    expect(mockEditInteractionReply.mock.calls.length).toBeGreaterThanOrEqual(1);
    // The whole point: the normal fast-path is constant-shape now. It
    // increments sharded counters and never BatchGets the full qurl_id
    // set, so max-size sends keep the same latency shape as small sends.
    expect(mockIncrementSendViewedCount).toHaveBeenCalledTimes(TRACKED.length);
    expect(mockGetQurlViews).not.toHaveBeenCalled();

    // The webhook trailing flush (not the poll backstop) renders the
    // SETTLED final count after the burst — simulate it by running the
    // fast-path once more past the window.
    row.lastRenderedAt = Date.now() - 10_000; // window elapsed
    mockRecordQurlView.mockResolvedValue({ result: 'recorded', firstView: false });
    await signedRequest({
      ...VALID_PAYLOAD,
      id: 'evt-burst-flush',
      data: { ...VALID_PAYLOAD.data, qurl_id: TRACKED[0] },
    });
    await flushCounter();
    const lastPayload = mockEditInteractionReply.mock.calls.at(-1)[2];
    expect(lastPayload.content).toContain(`👀 ${TRACKED.length} viewed`);
    // And the persisted count converged to the true total.
    expect(row.lastRenderedCount).toBe(TRACKED.length);
  });
});
