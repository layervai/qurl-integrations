
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
  getSendRenderState: jest.fn(async () => null),
  getSendItems: jest.fn(async () => []),
  getQurlViews: jest.fn(async () => new Map()),
  tryAdvanceRenderedCount: jest.fn(),
  healthCheck: jest.fn(),
  getStats: jest.fn(() => ({})),
}));

const mockEditDM = jest.fn();
jest.mock('../src/discord-rest', () => ({
  editDM: (...args) => mockEditDM(...args),
  editInteractionReply: jest.fn(async () => ({ ok: true })),
  sendChannelMessage: jest.fn(),
}));

const { buildConsumedDMPayload } = require('../src/dm-payloads');

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
const flip = require('./helpers/consumed-flip');

function signBody(rawJson, secret = 'test-qurl-secret') {
  return crypto.createHmac('sha256', secret).update(rawJson).digest('hex');
}

const QURL_ID = 'q_aaaaaaaaaa1';
const RESOURCE_ID = 'r_111';
const SEND_ID = 'snd-1';
const RECIPIENT_ID = 'usr-recipient';
const DM_CHANNEL_ID = 'dm-channel-1';
const DM_MESSAGE_ID = 'dm-message-1';
const EVENT_ID = 'evt-accessed-consumed-1';

const READY_ROW = {
  send_id: SEND_ID,
  recipient_discord_id: RECIPIENT_ID,
  qurl_id: QURL_ID,
  dm_channel_id: DM_CHANNEL_ID,
  dm_message_id: DM_MESSAGE_ID,
  dm_status: 'sent',
  created_at: '2026-05-19T12:00:00.000Z',
  expires_in: '30m',
};

const VALID_PAYLOAD = {
  id: EVENT_ID,
  type: 'qurl.accessed',
  data: { qurl_id: QURL_ID, resource_id: RESOURCE_ID, access_count: 1, consumed: true },
  owner_id: 'usr_test',
  timestamp: '2026-05-19T12:00:00Z',
  api_version: '2024-01-01',
};

function signedRequest(payload) {
  const raw = JSON.stringify(payload);
  return request(app)
    .post('/webhooks/qurl')
    .set('Content-Type', 'application/json')
    .set('QURL-Signature', signBody(raw))
    .send(raw);
}

const flushFlip = () => flip.flushFlip(logger);
const flipVerdictLog = () => flip.flipVerdict(logger);
const { drainTicks } = flip;

beforeEach(() => {
  jest.clearAllMocks();
  mockPrimed = true;
  mockWithinLag = false;
  mockOwnerSecrets.clear();
  mockOwnerSecrets.set('usr_test', 'test-qurl-secret');
  mockRecordQurlView.mockResolvedValue({ result: 'recorded', firstView: true });
  mockFindSendsByQurlId.mockResolvedValue([READY_ROW]);
  mockMarkConsumedDMEdited.mockResolvedValue(true);
  mockClearConsumedDMEdited.mockResolvedValue(undefined);
  mockIsSendRevoked.mockResolvedValue(false);
  mockEditDM.mockResolvedValue({ ok: true });
});

describe('POST /webhooks/qurl — qurl.accessed consumed-flip happy path', () => {
  it('returns 200 immediately on the view, then flips the recipient DM to the consumed payload', async () => {
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'recorded' });

    await flushFlip();

    expect(mockFindSendsByQurlId).toHaveBeenCalledWith(QURL_ID);
    expect(mockIsSendRevoked).toHaveBeenCalledWith(SEND_ID);
    expect(mockMarkConsumedDMEdited).toHaveBeenCalledWith(SEND_ID, RECIPIENT_ID);
    const markOrder = mockMarkConsumedDMEdited.mock.invocationCallOrder[0];
    const editOrder = mockEditDM.mock.invocationCallOrder[0];
    expect(markOrder).toBeLessThan(editOrder);
    expect(mockEditDM).toHaveBeenCalledWith(DM_CHANNEL_ID, DM_MESSAGE_ID, expect.any(Object));
    const payload = mockEditDM.mock.calls[0][2];
    expect(payload).toMatchObject({ embeds: expect.any(Array), components: [] });
    expect(payload.embeds.length).toBe(1);
  });

  it('the flipped DM carries past-tense copy with NO future expiry marker (the actual fix)', async () => {
    await signedRequest(VALID_PAYLOAD);
    await flushFlip();
    const desc = mockEditDM.mock.calls[0][2].embeds[0].toJSON().description;
    expect(desc).toMatch(/opened|no longer active|used/i);
    expect(desc).not.toMatch(/<t:\d+:[a-zA-Z]>/);
  });

  it('still flips when recordQurlView dedups the event (gated on consumed, NOT dbResult)', async () => {
    mockRecordQurlView.mockResolvedValue({ result: 'dedup', firstView: false });
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'dedup' });
    await flushFlip();
    expect(flipVerdictLog()).toEqual({ status: 'edited', transient: false });
    expect(mockMarkConsumedDMEdited).toHaveBeenCalledWith(SEND_ID, RECIPIENT_ID);
    expect(mockEditDM).toHaveBeenCalledTimes(1);
  });
});

describe('POST /webhooks/qurl — qurl.accessed does NOT flip when not consumed', () => {
  it('consumed: false records the view and never touches the DM', async () => {
    const res = await signedRequest({
      ...VALID_PAYLOAD,
      data: { qurl_id: QURL_ID, resource_id: RESOURCE_ID, access_count: 1, consumed: false },
    });
    expect(res.status).toBe(200);
    await drainTicks();
    expect(flipVerdictLog()).toBeNull(); // flip never even scheduled
    expect(mockMarkConsumedDMEdited).not.toHaveBeenCalled();
    expect(mockEditDM).not.toHaveBeenCalled();
  });

  it('a stringified "true" does NOT flip (strict-equality gate, no coercion)', async () => {
    const res = await signedRequest({
      ...VALID_PAYLOAD,
      data: { qurl_id: QURL_ID, resource_id: RESOURCE_ID, access_count: 1, consumed: 'true' },
    });
    expect(res.status).toBe(200);
    await drainTicks();
    expect(flipVerdictLog()).toBeNull();
    expect(mockEditDM).not.toHaveBeenCalled();
  });

  it('split emission: a consumed:false event then a consumed:true event on the same qURL flips ONLY on the burn', async () => {
    const res1 = await signedRequest({
      ...VALID_PAYLOAD,
      id: 'evt-split-1',
      data: { qurl_id: QURL_ID, resource_id: RESOURCE_ID, access_count: 1, consumed: false },
    });
    expect(res1.status).toBe(200);
    await drainTicks();
    expect(flipVerdictLog()).toBeNull();
    expect(mockEditDM).not.toHaveBeenCalled();

    const res2 = await signedRequest({
      ...VALID_PAYLOAD,
      id: 'evt-split-2',
      data: { qurl_id: QURL_ID, resource_id: RESOURCE_ID, access_count: 1, consumed: true },
    });
    expect(res2.status).toBe(200);
    await flushFlip();
    expect(flipVerdictLog()).toEqual({ status: 'edited', transient: false });
    expect(mockEditDM).toHaveBeenCalledTimes(1);
  });
});

describe('POST /webhooks/qurl — qurl.accessed consumed-flip skips', () => {
  it('skips when no recipient row matches the qurl_id (pre-rollout / missing-from-mint)', async () => {
    mockFindSendsByQurlId.mockResolvedValue([]);
    await signedRequest(VALID_PAYLOAD);
    await flushFlip();
    expect(flipVerdictLog()).toEqual({ status: 'no-recipient-row', transient: false });
    expect(mockMarkConsumedDMEdited).not.toHaveBeenCalled();
    expect(mockEditDM).not.toHaveBeenCalled();
  });

  it('skips when the GSI returns multiple rows (write-path invariant breach)', async () => {
    mockFindSendsByQurlId.mockResolvedValue([READY_ROW, { ...READY_ROW, recipient_discord_id: 'usr-other' }]);
    await signedRequest(VALID_PAYLOAD);
    await flushFlip();
    expect(flipVerdictLog()).toEqual({ status: 'ambiguous-recipient', transient: false });
    expect(mockEditDM).not.toHaveBeenCalled();
  });

  it('skips (does NOT clobber the revoke copy) when the send was already revoked', async () => {
    mockIsSendRevoked.mockResolvedValue(true);
    await signedRequest(VALID_PAYLOAD);
    await flushFlip();
    expect(flipVerdictLog()).toEqual({ status: 'send-revoked', transient: false });
    expect(mockMarkConsumedDMEdited).not.toHaveBeenCalled();
    expect(mockEditDM).not.toHaveBeenCalled();
  });

  it('skips when the expired flip already closed the DM (sibling-marker cross-check)', async () => {
    mockFindSendsByQurlId.mockResolvedValue([{ ...READY_ROW, expired_edited_at: '2026-05-19T12:25:00.000Z' }]);
    await signedRequest(VALID_PAYLOAD);
    await flushFlip();
    expect(flipVerdictLog()).toEqual({ status: 'sibling-already-flipped', transient: false });
    expect(mockMarkConsumedDMEdited).not.toHaveBeenCalled();
    expect(mockEditDM).not.toHaveBeenCalled();
  });

  it('skips (idempotent) when the consumed marker is already claimed (redelivery)', async () => {
    mockMarkConsumedDMEdited.mockResolvedValue(false);
    await signedRequest(VALID_PAYLOAD);
    await flushFlip();
    expect(flipVerdictLog()).toEqual({ status: 'already-edited', transient: false });
    expect(mockEditDM).not.toHaveBeenCalled();
  });

  it('skips when dm_status !== sent (DM never delivered)', async () => {
    mockFindSendsByQurlId.mockResolvedValue([{ ...READY_ROW, dm_status: 'failed' }]);
    await signedRequest(VALID_PAYLOAD);
    await flushFlip();
    expect(flipVerdictLog()).toEqual({ status: 'dm-not-editable', transient: false });
    expect(mockMarkConsumedDMEdited).not.toHaveBeenCalled();
    expect(mockEditDM).not.toHaveBeenCalled();
  });
});

describe('POST /webhooks/qurl — qurl.accessed consumed-flip edit failure handling', () => {
  it('keeps the marker on a permanent editDM failure (recipient blocked / deleted DM)', async () => {
    mockEditDM.mockResolvedValue({ ok: false, expected: true });
    await signedRequest(VALID_PAYLOAD);
    await flushFlip();
    expect(flipVerdictLog()).toEqual({ status: 'edit-failed-expected', transient: false });
    expect(mockMarkConsumedDMEdited).toHaveBeenCalledTimes(1);
    expect(mockClearConsumedDMEdited).not.toHaveBeenCalled();
  });

  it('rolls the marker back on a transient editDM failure so a redelivery / expired-backstop can recover', async () => {
    mockEditDM.mockResolvedValue({ ok: false, expected: false });
    await signedRequest(VALID_PAYLOAD);
    await flushFlip();
    expect(flipVerdictLog()).toEqual({ status: 'edit-failed-transient', transient: true });
    expect(mockMarkConsumedDMEdited).toHaveBeenCalledTimes(1);
    expect(mockClearConsumedDMEdited).toHaveBeenCalledWith(SEND_ID, RECIPIENT_ID);
  });

  it('rolls the marker back when editDM throws (treated as transient)', async () => {
    mockEditDM.mockRejectedValue(new Error('network'));
    await signedRequest(VALID_PAYLOAD);
    await flushFlip();
    expect(flipVerdictLog()).toEqual({ status: 'edit-failed-transient', transient: true });
    expect(mockClearConsumedDMEdited).toHaveBeenCalledWith(SEND_ID, RECIPIENT_ID);
  });

  it('does not double-edit when the flip fails transiently — the marker rollback leaves the door open for retry', async () => {
    mockEditDM.mockResolvedValueOnce({ ok: false, expected: false });
    await signedRequest(VALID_PAYLOAD);
    await flushFlip();
    expect(mockClearConsumedDMEdited).toHaveBeenCalledWith(SEND_ID, RECIPIENT_ID);
  });

  it('GSI lookup throw → lookup-error verdict, no marker claimed (recoverable by redelivery)', async () => {
    mockFindSendsByQurlId.mockRejectedValue(new Error('throttle'));
    await signedRequest(VALID_PAYLOAD);
    await flushFlip();
    expect(flipVerdictLog()).toEqual({ status: 'lookup-error', transient: true });
    expect(mockMarkConsumedDMEdited).not.toHaveBeenCalled();
    expect(mockEditDM).not.toHaveBeenCalled();
  });

  it('marker-claim throw (non-CCFE) → mark-error verdict, edit not attempted (recoverable by redelivery)', async () => {
    mockMarkConsumedDMEdited.mockRejectedValue(new Error('ProvisionedThroughputExceededException'));
    await signedRequest(VALID_PAYLOAD);
    await flushFlip();
    expect(flipVerdictLog()).toEqual({ status: 'mark-error', transient: true });
    expect(mockEditDM).not.toHaveBeenCalled();
  });

  it('transient editDM failure + rollback ALSO fails → edit-failed-rollback-failed (non-transient terminal; falls back to expired backstop)', async () => {
    mockEditDM.mockResolvedValue({ ok: false, expected: false });
    mockClearConsumedDMEdited.mockRejectedValue(new Error('throttle'));
    await signedRequest(VALID_PAYLOAD);
    await flushFlip();
    expect(flipVerdictLog()).toEqual({ status: 'edit-failed-rollback-failed', transient: false });
    expect(mockMarkConsumedDMEdited).toHaveBeenCalledTimes(1);
    expect(mockClearConsumedDMEdited).toHaveBeenCalledTimes(1);
  });
});

describe('buildConsumedDMPayload', () => {
  it('renders a single embed with components cleared', () => {
    const payload = buildConsumedDMPayload();
    expect(payload.embeds.length).toBe(1);
    expect(payload.components).toEqual([]);
  });

  it('uses past/perfect-tense copy and carries NO relative-time marker', () => {
    const desc = buildConsumedDMPayload().embeds[0].toJSON().description;
    expect(desc).toMatch(/opened|no longer active|used/i);
    expect(desc).not.toMatch(/<t:\d+/);
  });
});
