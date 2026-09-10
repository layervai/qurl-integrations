
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
const mockTryClaimRenderAttempt = jest.fn();
jest.mock('../src/store', () => ({
  recordQurlView: (...args) => mockRecordQurlView(...args),
  findSendsByQurlId: (...args) => mockFindSendsByQurlId(...args),
  getSendRenderState: (...args) => mockGetSendRenderState(...args),
  getSendItems: (...args) => mockGetSendItems(...args),
  getQurlViews: (...args) => mockGetQurlViews(...args),
  incrementSendViewedCount: (...args) => mockIncrementSendViewedCount(...args),
  getSendViewedCount: (...args) => mockGetSendViewedCount(...args),
  tryAdvanceRenderedCount: (...args) => mockTryAdvanceRenderedCount(...args),
  touchRenderedAt: (...args) => mockTouchRenderedAt(...args),
  tryClaimRenderAttempt: (...args) => mockTryClaimRenderAttempt(...args),
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
const realDateNow = Date.now.bind(Date);

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

async function flushCounter() {
  for (let i = 0; i < 50; i += 1) {
    await new Promise((resolve) => setImmediate(resolve));
    if (logger.debug.mock.calls.some(([msg]) => msg === VERDICT_MSG)) return;
  }
}

async function waitFor(predicate) {
  for (let i = 0; i < 50; i += 1) {
    if (predicate()) return;
    // eslint-disable-next-line no-await-in-loop
    await new Promise((resolve) => setImmediate(resolve));
  }
  throw new Error('Timed out waiting for test predicate');
}

async function waitForCounterFlush(predicate, timeoutMs = 1000) {
  const deadline = realDateNow() + timeoutMs;
  do {
    // eslint-disable-next-line no-await-in-loop
    await new Promise((resolve) => setTimeout(resolve, 10));
    // eslint-disable-next-line no-await-in-loop
    await flushCounter();
    if (predicate()) return;
  } while (realDateNow() < deadline);
  throw new Error('Timed out waiting for counter flush');
}

function armedState(overrides = {}) {
  return {
    interactionToken: TOKEN,
    interactionAppId: APP_ID,
    expectedCount: 3,
    viewedCount: 1,
    lastRenderedCount: 0,
    lastRenderedAt: 0,
    baseMsg: 'Sent to 3 users',
    qurlIds: [QURL_ID],
    terminal: false,
    ...overrides,
  };
}

beforeEach(() => {
  stopIntervals();
  [
    mockRecordQurlView,
    mockFindSendsByQurlId,
    mockGetSendRenderState,
    mockGetSendItems,
    mockGetQurlViews,
    mockIncrementSendViewedCount,
    mockGetSendViewedCount,
    mockTryAdvanceRenderedCount,
    mockTouchRenderedAt,
    mockTryClaimRenderAttempt,
    mockEditInteractionReply,
  ].forEach((mock) => mock.mockReset());
  jest.clearAllMocks();
  mockPrimed = true;
  mockWithinLag = false;
  mockOwnerSecrets.clear();
  mockOwnerSecrets.set('usr_test', 'test-qurl-secret');
  mockRecordQurlView.mockResolvedValue({ result: 'recorded', firstView: true });
  mockFindSendsByQurlId.mockResolvedValue([{ send_id: SEND_ID, sender_discord_id: SENDER_ID }]);
  mockGetSendRenderState.mockResolvedValue(armedState());
  mockGetSendItems.mockResolvedValue([{ qurl_id: QURL_ID, recipient_discord_id: 'r1' }]);
  mockGetQurlViews.mockResolvedValue(new Map([[QURL_ID, { accessCount: 1, consumed: false }]]));
  mockIncrementSendViewedCount.mockResolvedValue(undefined);
  mockGetSendViewedCount.mockResolvedValue(1);
  mockTryAdvanceRenderedCount.mockResolvedValue(true);
  mockTouchRenderedAt.mockResolvedValue(undefined);
  mockTryClaimRenderAttempt.mockResolvedValue(true);
  mockEditInteractionReply.mockResolvedValue({ ok: true });
});

afterEach(() => {
  stopIntervals();
});

describe('sender view-counter fast-path — happy path', () => {
  it('a recorded view edits the confirmation to "👀 1 viewed", THEN advances the rendered count', async () => {
    const res = await signedRequest();
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
    expect(mockTryAdvanceRenderedCount).toHaveBeenCalledWith(SEND_ID, 1);
    const editOrder = mockEditInteractionReply.mock.invocationCallOrder[0];
    const advanceOrder = mockTryAdvanceRenderedCount.mock.invocationCallOrder[0];
    expect(editOrder).toBeLessThan(advanceOrder);
  });

  it('caches render state inside a burst and refreshes the cached debounce clock after an edit', async () => {
    const now = realDateNow();
    const clock = jest.spyOn(Date, 'now').mockReturnValue(now);
    try {
      await signedRequest();
      await flushCounter();
      expect(mockEditInteractionReply).toHaveBeenCalledTimes(1);
      expect(mockGetSendRenderState).toHaveBeenCalledTimes(1);
      expect(mockGetSendViewedCount).toHaveBeenCalledTimes(1);

      logger.debug.mockClear();
      await signedRequest({
        ...VALID_PAYLOAD,
        id: 'evt-counter-cache-2',
        data: { ...VALID_PAYLOAD.data, qurl_id: 'q_cache_2' },
      });
      await flushCounter();

      expect(mockGetSendRenderState).toHaveBeenCalledTimes(1);
      expect(mockIncrementSendViewedCount).toHaveBeenCalledTimes(2);
      expect(mockGetSendViewedCount).toHaveBeenCalledTimes(1);
      expect(mockEditInteractionReply).toHaveBeenCalledTimes(1);
      expect(logger.debug).toHaveBeenCalledWith(
        'qURL webhook sender-counter: coalesced — scheduled trailing flush',
        expect.objectContaining({ send_id: SEND_ID, qurl_id: 'q_cache_2' }),
      );

      clock.mockReturnValue(now + 51);
      logger.debug.mockClear();
      await signedRequest({
        ...VALID_PAYLOAD,
        id: 'evt-counter-cache-3',
        data: { ...VALID_PAYLOAD.data, qurl_id: 'q_cache_3' },
      });
      await flushCounter();

      expect(mockGetSendRenderState).toHaveBeenCalledTimes(2);
    } finally {
      clock.mockRestore();
    }
  });

  it('coalesces a same-replica first-view while the leading Discord edit is still in flight', async () => {
    let resolveEdit;
    mockEditInteractionReply.mockImplementation(() => new Promise((resolve) => {
      resolveEdit = resolve;
    }));

    await signedRequest();
    await waitFor(() => mockEditInteractionReply.mock.calls.length === 1);

    logger.debug.mockClear();
    await signedRequest({
      ...VALID_PAYLOAD,
      id: 'evt-counter-inflight-2',
      data: { ...VALID_PAYLOAD.data, qurl_id: 'q_inflight_2' },
    });
    await flushCounter();

    expect(mockEditInteractionReply).toHaveBeenCalledTimes(1);
    expect(mockIncrementSendViewedCount).toHaveBeenCalledTimes(2);
    expect(mockGetSendViewedCount).toHaveBeenCalledTimes(1);
    expect(logger.debug).toHaveBeenCalledWith(
      'qURL webhook sender-counter: coalesced — scheduled trailing flush',
      expect.objectContaining({ send_id: SEND_ID, qurl_id: 'q_inflight_2' }),
    );

    logger.debug.mockClear();
    resolveEdit({ ok: true });
    await flushCounter();
    expect(mockTryAdvanceRenderedCount).toHaveBeenCalledWith(SEND_ID, 1);
  });

  it('coalesces when another replica already claimed the shared edit-attempt clock', async () => {
    mockTryClaimRenderAttempt.mockResolvedValue(false);
    await signedRequest();
    await flushCounter();

    expect(mockTryClaimRenderAttempt).toHaveBeenCalledWith(
      SEND_ID,
      expect.any(Number),
    );
    expect(mockGetSendViewedCount).not.toHaveBeenCalled();
    expect(mockEditInteractionReply).not.toHaveBeenCalled();
    expect(mockTryAdvanceRenderedCount).not.toHaveBeenCalled();
    expect(logger.debug).toHaveBeenCalledWith(
      'qURL webhook sender-counter: coalesced — distributed edit attempt already fresh',
      expect.objectContaining({ send_id: SEND_ID, qurl_id: QURL_ID }),
    );
  });
});

describe('sender view-counter fast-path — content-only (buttons preserved)', () => {
  it('the editInteractionReply payload carries ONLY content — NO components key', async () => {
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
    expect(mockGetSendItems).not.toHaveBeenCalled();
  });
});

describe('sender view-counter fast-path — absent-guard', () => {
  it('no token / no base → no edit (the poll backstop covers it)', async () => {
    mockGetSendRenderState.mockResolvedValue(armedState({ interactionToken: null, baseMsg: undefined }));
    await signedRequest();
    await flushCounter();
    expect(mockEditInteractionReply).not.toHaveBeenCalled();
    expect(mockTryAdvanceRenderedCount).not.toHaveBeenCalled();
  });
});

describe('sender view-counter fast-path — pre-read compare (N <= L)', () => {
  it('N=1 with lastRenderedCount=2 → no edit (redelivery / higher count already shown)', async () => {
    mockGetSendRenderState.mockResolvedValue(armedState({ lastRenderedCount: 2, viewedCount: 1 }));
    await signedRequest();
    await flushCounter();
    expect(mockEditInteractionReply).not.toHaveBeenCalled();
    expect(mockTryAdvanceRenderedCount).not.toHaveBeenCalled();
  });

  it('NO BACKWARDS STEP: after the poll advanced the shared floor to 3, a stale fast-path read of N=2 does NOT edit', async () => {
    mockGetSendRenderState.mockResolvedValue(armedState({
      lastRenderedCount: 3,
      viewedCount: 2,
      qurlIds: ['q_a', 'q_b', 'q_c'],
    }));
    await signedRequest();
    await flushCounter();
    expect(mockEditInteractionReply).not.toHaveBeenCalled();
    expect(mockTryAdvanceRenderedCount).not.toHaveBeenCalled();
  });
});

describe('sender view-counter fast-path — repeat access skip', () => {
  it('firstView=false exits before shard-sum reads because the distinct count cannot advance', async () => {
    mockRecordQurlView.mockResolvedValue({ result: 'recorded', firstView: false });
    await signedRequest();
    await flushCounter();
    expect(mockFindSendsByQurlId).not.toHaveBeenCalled();
    expect(mockGetSendRenderState).not.toHaveBeenCalled();
    expect(mockIncrementSendViewedCount).not.toHaveBeenCalled();
    expect(mockGetSendViewedCount).not.toHaveBeenCalled();
    expect(mockGetQurlViews).not.toHaveBeenCalled();
    expect(mockEditInteractionReply).not.toHaveBeenCalled();
    expect(logger.debug).toHaveBeenCalledWith(
      'qURL webhook sender-counter: skip — no distinct-view advance',
      { qurl_id: QURL_ID },
    );
    expect(logger.debug).toHaveBeenCalledWith(
      VERDICT_MSG,
      expect.objectContaining({ qurl_id: QURL_ID, status: 'no-distinct-view' }),
    );
  });
});

describe('sender view-counter fast-path — FAILED-EDIT SELF-HEAL (load-bearing)', () => {
  it('editInteractionReply {ok:false} → tryAdvanceRenderedCount NOT called (count stays), but touchRenderedAt IS (debounce armed)', async () => {
    mockEditInteractionReply.mockResolvedValue({ ok: false, status: 500 });
    await signedRequest();
    await flushCounter();
    expect(mockEditInteractionReply).toHaveBeenCalledTimes(1);
    expect(mockTryAdvanceRenderedCount).not.toHaveBeenCalled();
    expect(mockTouchRenderedAt).toHaveBeenCalledWith(SEND_ID);
  });

  it('COALESCES ON FAILURE: a 2nd view within the window after a failed edit is debounced (not re-attempted)', async () => {
    mockEditInteractionReply.mockResolvedValue({ ok: false, status: 500 });
    await signedRequest();
    await flushCounter();
    expect(mockEditInteractionReply).toHaveBeenCalledTimes(1);
    expect(mockTouchRenderedAt).toHaveBeenCalledTimes(1);

    logger.debug.mockClear();
    mockGetSendRenderState.mockResolvedValue(armedState({ lastRenderedAt: Date.now() }));
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

  it('sharded aggregate increment throttle falls back to qurl views immediately', async () => {
    mockIncrementSendViewedCount.mockRejectedValue(new Error('ProvisionedThroughputExceededException'));
    mockGetQurlViews.mockResolvedValue(new Map([[QURL_ID, { accessCount: 1, consumed: false }]]));
    await signedRequest();
    await flushCounter();
    expect(mockGetSendViewedCount).not.toHaveBeenCalled();
    expect(mockGetQurlViews).toHaveBeenCalledWith([QURL_ID]);
    expect(mockEditInteractionReply).toHaveBeenCalledTimes(1);
    expect(mockTryAdvanceRenderedCount).toHaveBeenCalledWith(SEND_ID, 1);
    expect(logger.warn).toHaveBeenCalledWith(
      'qURL webhook sender-counter: sharded aggregate increment failed; falling back to qurl views',
      expect.objectContaining({ qurl_id: QURL_ID, send_id: SEND_ID }),
    );
    expect(logger.debug).toHaveBeenCalledWith(
      VERDICT_MSG,
      expect.objectContaining({ qurl_id: QURL_ID, status: 'edited' }),
    );
  });

  it('legacy fallback: uses getSendItems/getQurlViews when viewed_count and qurl_ids are absent', async () => {
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

  it('shard-sum read failure falls back to persisted qurlIds/getQurlViews', async () => {
    mockGetSendRenderState.mockResolvedValue(armedState({
      viewedCount: null,
      qurlIds: ['q_a', 'q_b'],
    }));
    mockGetSendViewedCount.mockRejectedValue(new Error('DDB aggregate read failed'));
    mockGetQurlViews.mockResolvedValue(new Map([
      ['q_a', { accessCount: 1, consumed: false }],
      ['q_b', { accessCount: 0, consumed: false }],
    ]));

    await signedRequest();
    await flushCounter();

    expect(mockGetQurlViews).toHaveBeenCalledWith(['q_a', 'q_b']);
    expect(mockGetSendItems).not.toHaveBeenCalled();
    expect(mockEditInteractionReply).toHaveBeenCalledTimes(1);
    const payload = mockEditInteractionReply.mock.calls[0][2];
    expect(payload.content).toContain('👀 1 viewed');
    expect(logger.warn).toHaveBeenCalledWith(
      'qURL webhook sender-counter: sharded aggregate read failed; falling back to qurl views',
      expect.objectContaining({ qurl_id: QURL_ID, send_id: SEND_ID, error: 'DDB aggregate read failed' }),
    );
  });

  it('shard-sum read failure falls back through getSendItems when inline qurlIds are capped empty', async () => {
    mockGetSendRenderState.mockResolvedValue(armedState({
      viewedCount: 0,
      qurlIds: [],
    }));
    mockGetSendViewedCount.mockRejectedValue(new Error('DDB aggregate read failed'));
    mockGetSendItems.mockResolvedValue([
      { qurl_id: 'q_a', recipient_discord_id: 'r1' },
      { qurl_id: 'q_b', recipient_discord_id: 'r2' },
      { qurl_id: 'q_c', recipient_discord_id: 'r3' },
    ]);
    mockGetQurlViews.mockResolvedValue(new Map([
      ['q_a', { accessCount: 1, consumed: false }],
      ['q_b', { accessCount: 1, consumed: false }],
      ['q_c', { accessCount: 0, consumed: false }],
    ]));

    await signedRequest();
    await flushCounter();

    expect(mockGetSendItems).toHaveBeenCalledWith(SEND_ID, SENDER_ID);
    expect(mockGetQurlViews).toHaveBeenCalledWith(['q_a', 'q_b', 'q_c']);
    expect(mockEditInteractionReply).toHaveBeenCalledTimes(1);
    const payload = mockEditInteractionReply.mock.calls[0][2];
    expect(payload.content).toContain('👀 2 viewed');
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
      lastRenderedAt: Date.now() - 700,
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
    await waitForCounterFlush(() => mockEditInteractionReply.mock.calls.length === 1);

    expect(mockEditInteractionReply).toHaveBeenCalledTimes(1);
    const payload = mockEditInteractionReply.mock.calls[0][2];
    expect(payload.content).toContain('👀 2 viewed');
    expect(mockTryAdvanceRenderedCount).toHaveBeenCalledWith(SEND_ID, 2);
  });

  it('cached trailing flush refreshes terminal state before editing', async () => {
    mockGetSendRenderState
      .mockResolvedValueOnce(armedState({
        lastRenderedCount: 1,
        lastRenderedAt: Date.now() - 700,
        viewedCount: 2,
        qurlIds: ['q_a', 'q_b'],
      }))
      .mockResolvedValueOnce(armedState({
        lastRenderedCount: 1,
        lastRenderedAt: Date.now(),
        viewedCount: 2,
        qurlIds: ['q_a', 'q_b'],
        terminal: true,
      }));

    await signedRequest();
    await flushCounter();
    expect(mockEditInteractionReply).not.toHaveBeenCalled();

    logger.debug.mockClear();
    await waitForCounterFlush(() => mockGetSendRenderState.mock.calls.length === 2);

    expect(mockGetSendRenderState).toHaveBeenCalledTimes(2);
    expect(mockEditInteractionReply).not.toHaveBeenCalled();
    expect(logger.debug).toHaveBeenCalledWith(
      'qURL webhook sender-counter: skip — terminal',
      expect.objectContaining({ send_id: SEND_ID }),
    );
  });

  it('aggregate increment failure inside the coalesce window schedules one source fallback flush', async () => {
    mockGetSendRenderState.mockResolvedValue(armedState({
      lastRenderedCount: 1,
      lastRenderedAt: Date.now() - 700,
      viewedCount: 1,
      qurlIds: ['q_a', 'q_b'],
    }));
    mockIncrementSendViewedCount.mockRejectedValue(new Error('ProvisionedThroughputExceededException'));
    mockGetQurlViews.mockResolvedValue(new Map([
      ['q_a', { accessCount: 1, consumed: false }],
      ['q_b', { accessCount: 1, consumed: false }],
    ]));

    await signedRequest();
    await flushCounter();

    expect(mockEditInteractionReply).not.toHaveBeenCalled();
    expect(mockGetSendViewedCount).not.toHaveBeenCalled();
    expect(mockGetQurlViews).not.toHaveBeenCalled();
    expect(logger.debug).toHaveBeenCalledWith(
      'qURL webhook sender-counter: coalesced — scheduled trailing flush',
      expect.objectContaining({ send_id: SEND_ID, force_source: true }),
    );

    logger.debug.mockClear();
    await waitForCounterFlush(() => mockGetQurlViews.mock.calls.length === 1);

    expect(mockGetSendViewedCount).not.toHaveBeenCalled();
    expect(mockGetQurlViews).toHaveBeenCalledWith(['q_a', 'q_b']);
    expect(mockEditInteractionReply).toHaveBeenCalledTimes(1);
    const payload = mockEditInteractionReply.mock.calls[0][2];
    expect(payload.content).toContain('👀 2 viewed');
  });

  it('repair-floor replacement preserves an already-pending source fallback flush', async () => {
    const now = realDateNow();
    const clock = jest.spyOn(Date, 'now').mockReturnValue(now);
    mockGetSendRenderState
      .mockResolvedValueOnce(armedState({
        lastRenderedCount: 1,
        lastRenderedAt: now,
        viewedCount: 1,
        qurlIds: ['q_a', 'q_b'],
      }))
      .mockResolvedValueOnce(armedState({
        lastRenderedCount: 1,
        lastRenderedAt: 0,
        viewedCount: 1,
        qurlIds: ['q_a', 'q_b'],
      }))
      .mockResolvedValueOnce(armedState({
        lastRenderedCount: 2,
        lastRenderedAt: now + 51,
        viewedCount: 2,
        qurlIds: ['q_a', 'q_b'],
      }));
    mockIncrementSendViewedCount
      .mockRejectedValueOnce(new Error('ProvisionedThroughputExceededException'))
      .mockResolvedValue(undefined);
    mockGetSendViewedCount.mockResolvedValue(2);
    mockGetQurlViews.mockResolvedValue(new Map([
      ['q_a', { accessCount: 1, consumed: false }],
      ['q_b', { accessCount: 1, consumed: false }],
    ]));
    mockTryAdvanceRenderedCount.mockResolvedValue(false);

    try {
      await signedRequest({ ...VALID_PAYLOAD, data: { ...VALID_PAYLOAD.data, qurl_id: 'q_a' } });
      await flushCounter();
      expect(mockEditInteractionReply).not.toHaveBeenCalled();
      expect(mockGetSendViewedCount).not.toHaveBeenCalled();
      expect(mockGetQurlViews).not.toHaveBeenCalled();
      expect(logger.debug).toHaveBeenCalledWith(
        'qURL webhook sender-counter: coalesced — scheduled trailing flush',
        expect.objectContaining({ send_id: SEND_ID, force_source: true }),
      );

      logger.debug.mockClear();
      clock.mockReturnValue(now + 51);
      await signedRequest({ ...VALID_PAYLOAD, id: 'evt-repair-source', data: { ...VALID_PAYLOAD.data, qurl_id: 'q_b' } });
      await waitForCounterFlush(() => mockGetQurlViews.mock.calls.length === 1);

      expect(mockGetSendViewedCount).toHaveBeenCalledTimes(1);
      expect(mockGetQurlViews).toHaveBeenCalledTimes(1);
      expect(mockGetQurlViews).toHaveBeenCalledWith(['q_a', 'q_b']);
      expect(mockEditInteractionReply).toHaveBeenCalledTimes(2);
    } finally {
      clock.mockRestore();
    }
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
    await waitForCounterFlush(() => mockEditInteractionReply.mock.calls.length === 2);

    expect(mockEditInteractionReply).toHaveBeenCalledTimes(2);
    expect(mockTryAdvanceRenderedCount).toHaveBeenCalledTimes(2);
    expect(mockEditInteractionReply.mock.calls[1][2].content).toContain('👀 2 viewed');
  });

  it('BURST: many views in a short window → bounded edit count (≤3, not N), final render correct', async () => {
    const TRACKED = Array.from({ length: 30 }, (_, i) => `q_burst_${i}`);
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
    mockTryAdvanceRenderedCount.mockImplementation(async (_sendId, n) => {
      if (n > row.lastRenderedCount) {
        row.lastRenderedCount = n;
        row.lastRenderedAt = Date.now();
        return true;
      }
      return false;
    });

    for (let i = 0; i < TRACKED.length; i += 1) {
      const payload = {
        ...VALID_PAYLOAD,
        id: `evt-burst-${i}`,
        data: { ...VALID_PAYLOAD.data, qurl_id: TRACKED[i] },
      };
      // eslint-disable-next-line no-await-in-loop
      await signedRequest(payload);
    }
    for (let i = 0; i < 200; i += 1) {
      // eslint-disable-next-line no-await-in-loop
      await new Promise((resolve) => setImmediate(resolve));
    }

    expect(mockEditInteractionReply.mock.calls.length).toBeLessThanOrEqual(3);
    expect(mockEditInteractionReply.mock.calls.length).toBeGreaterThanOrEqual(1);
    expect(mockIncrementSendViewedCount).toHaveBeenCalledTimes(TRACKED.length);
    expect(mockGetQurlViews).not.toHaveBeenCalled();

    await waitForCounterFlush(() => (
      mockEditInteractionReply.mock.calls.at(-1)?.[2]?.content.includes(`👀 ${TRACKED.length} viewed`)
    ));
    const lastPayload = mockEditInteractionReply.mock.calls.at(-1)[2];
    expect(lastPayload.content).toContain(`👀 ${TRACKED.length} viewed`);
    expect(row.lastRenderedCount).toBe(TRACKED.length);
  });
});
