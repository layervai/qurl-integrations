// Guard test for the one synchronous caller-supplied step in
// flipRecipientDMToClosed: buildPayload(row). The helper wraps it in
// try/catch so a throw becomes a permanent skip (payload-build-error)
// instead of rejecting up to the caller — on the expired path that
// rejection would escape into Express v4 (no async-rejection catch).
//
// Neither real builder throws today (the expired builder validates +
// returns null, the consumed builder is static), so to exercise the
// guard we mock dm-payloads so buildConsumedDMPayload THROWS, then drive
// a consumed event through the route and assert the flip skips cleanly:
// no marker claimed, no editDM, verdict = payload-build-error, and
// crucially no unhandled rejection escapes.

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

const mockFindSendsByQurlId = jest.fn();
const mockMarkConsumedDMEdited = jest.fn();
const mockClearConsumedDMEdited = jest.fn();
const mockIsSendRevoked = jest.fn();
const mockRecordQurlView = jest.fn();
jest.mock('../src/store', () => ({
  findSendsByQurlId: (...args) => mockFindSendsByQurlId(...args),
  markConsumedDMEdited: (...args) => mockMarkConsumedDMEdited(...args),
  clearConsumedDMEdited: (...args) => mockClearConsumedDMEdited(...args),
  isSendRevoked: (...args) => mockIsSendRevoked(...args),
  recordQurlView: (...args) => mockRecordQurlView(...args),
  markExpiredDMEdited: jest.fn(),
  clearExpiredDMEdited: jest.fn(),
  healthCheck: jest.fn(),
  getStats: jest.fn(() => ({})),
}));

const mockEditDM = jest.fn();
jest.mock('../src/discord-rest', () => ({
  editDM: (...args) => mockEditDM(...args),
  sendChannelMessage: jest.fn(),
}));

// Force buildConsumedDMPayload to throw so the route's
// `buildPayload: () => buildConsumedDMPayload()` exercises the helper's
// guard. Keep buildExpiredDMPayload real (unused on this path).
jest.mock('../src/dm-payloads', () => {
  const actual = jest.requireActual('../src/dm-payloads');
  return {
    ...actual,
    buildConsumedDMPayload: () => {
      throw new Error('simulated builder throw');
    },
  };
});

jest.mock('../src/view-update-publisher', () => ({
  publish: jest.fn(),
  start: jest.fn(),
  stop: jest.fn(),
}));

const mockOwnerSecrets = new Map();
mockOwnerSecrets.set('usr_test', 'test-qurl-secret');
jest.mock('../src/webhook-subscriptions', () => ({
  isPrimed: () => true,
  isWithinSiblingLagWindow: () => false,
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
const flip = require('./helpers/consumed-flip');

const QURL_ID = 'q_aaaaaaaaaa1';
const SEND_ID = 'snd-1';
const READY_ROW = {
  send_id: SEND_ID,
  recipient_discord_id: 'usr-recipient',
  qurl_id: QURL_ID,
  dm_channel_id: 'dm-channel-1',
  dm_message_id: 'dm-message-1',
  dm_status: 'sent',
  created_at: '2026-05-19T12:00:00.000Z',
  expires_in: '30m',
};

function signBody(rawJson, secret = 'test-qurl-secret') {
  return crypto.createHmac('sha256', secret).update(rawJson).digest('hex');
}

function signedRequest(payload) {
  const raw = JSON.stringify(payload);
  return request(app)
    .post('/webhooks/qurl')
    .set('Content-Type', 'application/json')
    .set('QURL-Signature', signBody(raw))
    .send(raw);
}

const { drainTicks } = flip;
const flipVerdict = () => flip.flipVerdict(logger);

beforeEach(() => {
  jest.clearAllMocks();
  mockRecordQurlView.mockResolvedValue({ result: 'recorded', firstView: true });
  mockFindSendsByQurlId.mockResolvedValue([READY_ROW]);
  mockMarkConsumedDMEdited.mockResolvedValue(true);
  mockClearConsumedDMEdited.mockResolvedValue(undefined);
  mockIsSendRevoked.mockResolvedValue(false);
  mockEditDM.mockResolvedValue({ ok: true });
});

describe('POST /webhooks/qurl — consumed flip when buildPayload throws', () => {
  it('skips cleanly (payload-build-error) without claiming the marker or editing, and does not escape', async () => {
    const res = await signedRequest({
      id: 'evt-throw-1',
      type: 'qurl.accessed',
      data: { qurl_id: QURL_ID, resource_id: 'r_1', access_count: 1, consumed: true },
      owner_id: 'usr_test',
      timestamp: '2026-05-19T12:00:00Z',
      api_version: '2024-01-01',
    });
    // Primary op (view) still returns 200 — the throw is contained to
    // the background flip.
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'recorded' });

    await drainTicks();

    // Guard ran BEFORE the marker claim, so nothing to roll back.
    expect(flipVerdict()).toEqual({ status: 'payload-build-error', transient: false });
    expect(mockMarkConsumedDMEdited).not.toHaveBeenCalled();
    expect(mockClearConsumedDMEdited).not.toHaveBeenCalled();
    expect(mockEditDM).not.toHaveBeenCalled();
    // The throw surfaced as the guard's error log, NOT the outer
    // flip-threw .catch (which would mean it escaped flipRecipientDMToClosed).
    const errorMsgs = logger.error.mock.calls.map(([m]) => m);
    expect(errorMsgs).toContain('qURL webhook qurl.accessed-consumed: buildPayload threw — skipping');
    expect(errorMsgs).not.toContain('qURL webhook qurl.accessed-consumed: flip threw');
  });
});
