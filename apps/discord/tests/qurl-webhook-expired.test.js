
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
const mockMarkExpiredDMEdited = jest.fn();
const mockClearExpiredDMEdited = jest.fn();
const mockIsSendRevoked = jest.fn();
jest.mock('../src/store', () => ({
  findSendsByQurlId: (...args) => mockFindSendsByQurlId(...args),
  markExpiredDMEdited: (...args) => mockMarkExpiredDMEdited(...args),
  clearExpiredDMEdited: (...args) => mockClearExpiredDMEdited(...args),
  isSendRevoked: (...args) => mockIsSendRevoked(...args),
  recordQurlView: jest.fn(async () => ({ result: 'recorded', firstView: true })),
  healthCheck: jest.fn(),
  getStats: jest.fn(() => ({})),
}));

const mockEditDM = jest.fn();
jest.mock('../src/discord-rest', () => ({
  editDM: (...args) => mockEditDM(...args),
  sendChannelMessage: jest.fn(),
}));

const { buildExpiredDMPayload } = require('../src/dm-payloads');

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

function signBody(rawJson, secret = 'test-qurl-secret') {
  return crypto.createHmac('sha256', secret).update(rawJson).digest('hex');
}

const QURL_ID = 'q_aaaaaaaaaa1';
const RESOURCE_ID = 'r_111';
const SEND_ID = 'snd-1';
const RECIPIENT_ID = 'usr-recipient';
const DM_CHANNEL_ID = 'dm-channel-1';
const DM_MESSAGE_ID = 'dm-message-1';

const CREATED_AT_ISO = '2026-05-19T12:00:00.000Z';
const EXPIRES_IN = '24h';
const EXPECTED_EXPIRES_AT_SECONDS = Math.floor((Date.parse(CREATED_AT_ISO) + 24 * 3600 * 1000) / 1000);

const VALID_PAYLOAD = {
  id: 'evt-expired-1',
  type: 'qurl.expired',
  data: { qurl_id: QURL_ID, resource_id: RESOURCE_ID },
  owner_id: 'usr_test',
  timestamp: '2026-05-19T12:00:00Z',
  api_version: '2024-01-01',
};

const READY_ROW = {
  send_id: SEND_ID,
  recipient_discord_id: RECIPIENT_ID,
  qurl_id: QURL_ID,
  dm_channel_id: DM_CHANNEL_ID,
  dm_message_id: DM_MESSAGE_ID,
  dm_status: 'sent',
  created_at: CREATED_AT_ISO,
  expires_in: EXPIRES_IN,
};

function signedRequest(payload) {
  const raw = JSON.stringify(payload);
  return request(app)
    .post('/webhooks/qurl')
    .set('Content-Type', 'application/json')
    .set('QURL-Signature', signBody(raw))
    .send(raw);
}

beforeEach(() => {
  jest.clearAllMocks();
  mockPrimed = true;
  mockWithinLag = false;
  mockOwnerSecrets.clear();
  mockOwnerSecrets.set('usr_test', 'test-qurl-secret');
  mockFindSendsByQurlId.mockResolvedValue([READY_ROW]);
  mockMarkExpiredDMEdited.mockResolvedValue(true);
  mockClearExpiredDMEdited.mockResolvedValue(undefined);
  mockIsSendRevoked.mockResolvedValue(false);
  mockEditDM.mockResolvedValue({ ok: true });
});

describe('POST /webhooks/qurl — qurl.expired happy path', () => {
  it('looks up the recipient row by qurl_id and PATCHes the DM with the tense-flipped payload', async () => {
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'edited' });
    expect(mockFindSendsByQurlId).toHaveBeenCalledWith(QURL_ID);
    expect(mockIsSendRevoked).toHaveBeenCalledWith(SEND_ID);
    expect(mockMarkExpiredDMEdited).toHaveBeenCalledWith(SEND_ID, RECIPIENT_ID);
    const markCallOrder = mockMarkExpiredDMEdited.mock.invocationCallOrder[0];
    const editCallOrder = mockEditDM.mock.invocationCallOrder[0];
    expect(markCallOrder).toBeLessThan(editCallOrder);
    expect(mockEditDM).toHaveBeenCalledWith(DM_CHANNEL_ID, DM_MESSAGE_ID, expect.any(Object));
    const payload = mockEditDM.mock.calls[0][2];
    expect(payload).toMatchObject({
      embeds: expect.any(Array),
      components: [],
    });
    expect(payload.embeds.length).toBe(1);
  });

  it('renders the <t:N:R> relative-time marker against the reconstructed expiry (Discord auto-tense-flips)', () => {
    const payload = buildExpiredDMPayload({ expiresAtSeconds: EXPECTED_EXPIRES_AT_SECONDS });
    expect(payload).not.toBeNull();
    const embedJson = payload.embeds[0].toJSON();
    expect(embedJson.description).toContain(`<t:${EXPECTED_EXPIRES_AT_SECONDS}:R>`);
  });

  it('reconstructs expires_at from row.created_at + row.expires_in for the wire payload (no expires_at)', async () => {
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'edited' });
    const payload = mockEditDM.mock.calls[0][2];
    const desc = payload.embeds[0].toJSON().description;
    expect(desc).toContain(`<t:${EXPECTED_EXPIRES_AT_SECONDS}:R>`);
  });
});

describe('POST /webhooks/qurl — qurl.expired short-circuits', () => {
  it('200/no-recipient-row when GSI returns nothing (pre-rollout qurl, no qurl_id stored)', async () => {
    mockFindSendsByQurlId.mockResolvedValue([]);
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'no-recipient-row' });
    expect(mockEditDM).not.toHaveBeenCalled();
    expect(mockMarkExpiredDMEdited).not.toHaveBeenCalled();
  });

  it('200/ambiguous-recipient when GSI returns multiple rows (write-path invariant breach)', async () => {
    mockFindSendsByQurlId.mockResolvedValue([READY_ROW, { ...READY_ROW, recipient_discord_id: 'usr-other' }]);
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'ambiguous-recipient' });
    expect(mockEditDM).not.toHaveBeenCalled();
  });

  it('200/send-revoked when the sender already ran /qurl revoke (DM already says "closed the door")', async () => {
    mockIsSendRevoked.mockResolvedValue(true);
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'send-revoked' });
    expect(mockEditDM).not.toHaveBeenCalled();
    expect(mockMarkExpiredDMEdited).not.toHaveBeenCalled();
  });

  it('200/already-edited on the idempotency marker collision (retry / dual-emission)', async () => {
    mockMarkExpiredDMEdited.mockResolvedValue(false);
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'already-edited' });
    expect(mockEditDM).not.toHaveBeenCalled();
  });

  it('200/sibling-already-flipped when the consumed-flip already closed the DM (cross-marker skip)', async () => {
    mockFindSendsByQurlId.mockResolvedValue([{ ...READY_ROW, consumed_edited_at: '2026-05-19T12:05:00.000Z' }]);
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'sibling-already-flipped' });
    expect(mockMarkExpiredDMEdited).not.toHaveBeenCalled();
    expect(mockEditDM).not.toHaveBeenCalled();
  });

  it('200/dm-not-editable when dm_status !== sent (DM never delivered)', async () => {
    mockFindSendsByQurlId.mockResolvedValue([{ ...READY_ROW, dm_status: 'failed' }]);
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'dm-not-editable' });
    expect(mockEditDM).not.toHaveBeenCalled();
    expect(mockMarkExpiredDMEdited).not.toHaveBeenCalled();
  });

  it('200/dm-not-editable when dm_channel_id is missing (legacy pre-ref-capture row)', async () => {
    mockFindSendsByQurlId.mockResolvedValue([{ ...READY_ROW, dm_channel_id: undefined }]);
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'dm-not-editable' });
    expect(mockEditDM).not.toHaveBeenCalled();
  });
});

describe('POST /webhooks/qurl — qurl.expired payload validation', () => {
  it('200/invalid-payload when qurl_id is missing', async () => {
    const res = await signedRequest({ ...VALID_PAYLOAD, data: { resource_id: RESOURCE_ID } });
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'invalid-payload' });
    expect(mockFindSendsByQurlId).not.toHaveBeenCalled();
  });

  it('200/invalid-payload when qurl_id is non-string', async () => {
    for (const bad of [123, null, {}, [], true]) {
      jest.clearAllMocks();
      mockFindSendsByQurlId.mockResolvedValue([READY_ROW]);
      const res = await signedRequest({ ...VALID_PAYLOAD, data: { qurl_id: bad, resource_id: RESOURCE_ID } });
      expect(res.status).toBe(200);
      expect(res.body).toEqual({ status: 'invalid-payload' });
      expect(mockEditDM).not.toHaveBeenCalled();
    }
  });
});

describe('POST /webhooks/qurl — qurl.expired expiry reconstruction', () => {
  it('200/cannot-reconstruct-expiry when row.created_at is missing', async () => {
    mockFindSendsByQurlId.mockResolvedValue([{ ...READY_ROW, created_at: undefined }]);
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'cannot-reconstruct-expiry' });
    expect(mockMarkExpiredDMEdited).not.toHaveBeenCalled();
    expect(mockEditDM).not.toHaveBeenCalled();
  });

  it('200/cannot-reconstruct-expiry when row.created_at is unparseable', async () => {
    mockFindSendsByQurlId.mockResolvedValue([{ ...READY_ROW, created_at: 'not-a-date' }]);
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'cannot-reconstruct-expiry' });
    expect(mockEditDM).not.toHaveBeenCalled();
  });

  it('200/cannot-reconstruct-expiry when row.expires_in is missing', async () => {
    mockFindSendsByQurlId.mockResolvedValue([{ ...READY_ROW, expires_in: undefined }]);
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'cannot-reconstruct-expiry' });
    expect(mockEditDM).not.toHaveBeenCalled();
  });

  it('200/cannot-reconstruct-expiry when row.expires_in is an off-set non-empty string (corrupt row, NOT silently defaulted to 24h)', async () => {
    for (const bad of ['garbage', '99x', '7y', '5', '24', '24h ', ' 24h', '24H']) {
      jest.clearAllMocks();
      mockMarkExpiredDMEdited.mockResolvedValue(true);
      mockIsSendRevoked.mockResolvedValue(false);
      mockEditDM.mockResolvedValue({ ok: true });
      mockFindSendsByQurlId.mockResolvedValue([{ ...READY_ROW, expires_in: bad }]);
      const res = await signedRequest(VALID_PAYLOAD);
      expect(res.status).toBe(200);
      expect(res.body).toEqual({ status: 'cannot-reconstruct-expiry' });
      expect(mockEditDM).not.toHaveBeenCalled();
      expect(mockMarkExpiredDMEdited).not.toHaveBeenCalled();
    }
  });

  it('renders the expected absolute marker for each EXPIRY_LABELS preset', async () => {
    const presets = [
      ['30m', 30 * 60],
      ['1h', 3600],
      ['6h', 6 * 3600],
      ['24h', 24 * 3600],
      ['7d', 7 * 24 * 3600],
    ];
    for (const [label, seconds] of presets) {
      jest.clearAllMocks();
      mockMarkExpiredDMEdited.mockResolvedValue(true);
      mockIsSendRevoked.mockResolvedValue(false);
      mockEditDM.mockResolvedValue({ ok: true });
      mockFindSendsByQurlId.mockResolvedValue([{ ...READY_ROW, expires_in: label }]);
      const res = await signedRequest(VALID_PAYLOAD);
      expect(res.status).toBe(200);
      expect(res.body).toEqual({ status: 'edited' });
      const expected = Math.floor((Date.parse(CREATED_AT_ISO) + seconds * 1000) / 1000);
      const desc = mockEditDM.mock.calls[0][2].embeds[0].toJSON().description;
      expect(desc).toContain(`<t:${expected}:R>`);
    }
  });
});

describe('POST /webhooks/qurl — qurl.expired error handling', () => {
  it('503/lookup-error when GSI Query throws (PRE-marker transient → qurl-service retries)', async () => {
    mockFindSendsByQurlId.mockRejectedValue(new Error('throttle'));
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(503);
    expect(res.body).toEqual({ status: 'lookup-error' });
  });

  it('503/mark-error when markExpiredDMEdited throws on a non-CCFE error (PRE-marker transient)', async () => {
    mockMarkExpiredDMEdited.mockRejectedValue(new Error('ProvisionedThroughputExceededException'));
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(503);
    expect(res.body).toEqual({ status: 'mark-error' });
    expect(mockEditDM).not.toHaveBeenCalled();
  });

  it('200/edit-failed-expected when editDM returns ok:false with expected:true (recipient blocked/deleted DM) — marker stays', async () => {
    mockEditDM.mockResolvedValue({ ok: false, expected: true });
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'edit-failed-expected' });
    expect(mockMarkExpiredDMEdited).toHaveBeenCalledTimes(1);
    expect(mockClearExpiredDMEdited).not.toHaveBeenCalled();
  });

  it('503/edit-failed-transient when editDM returns ok:false with expected:false (Discord 5xx) — marker rolled back', async () => {
    mockEditDM.mockResolvedValue({ ok: false, expected: false });
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(503);
    expect(res.body).toEqual({ status: 'edit-failed-transient' });
    expect(mockMarkExpiredDMEdited).toHaveBeenCalledTimes(1);
    expect(mockClearExpiredDMEdited).toHaveBeenCalledWith(SEND_ID, RECIPIENT_ID);
  });

  it('503/edit-failed-transient when editDM throws (treat as expected:false) — marker rolled back', async () => {
    mockEditDM.mockRejectedValue(new Error('network'));
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(503);
    expect(res.body).toEqual({ status: 'edit-failed-transient' });
    expect(mockMarkExpiredDMEdited).toHaveBeenCalledTimes(1);
    expect(mockClearExpiredDMEdited).toHaveBeenCalledWith(SEND_ID, RECIPIENT_ID);
  });

  it('200/edit-failed-rollback-failed when transient editDM + marker rollback both fail (terminal — falls back to S3 lifecycle)', async () => {
    mockEditDM.mockResolvedValue({ ok: false, expected: false });
    mockClearExpiredDMEdited.mockRejectedValue(new Error('throttle'));
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'edit-failed-rollback-failed' });
    expect(mockMarkExpiredDMEdited).toHaveBeenCalledTimes(1);
    expect(mockClearExpiredDMEdited).toHaveBeenCalledTimes(1);
  });

  it('continues to edit when isSendRevoked throws transiently (best-effort gate)', async () => {
    mockIsSendRevoked.mockRejectedValue(new Error('throttle'));
    const res = await signedRequest(VALID_PAYLOAD);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'edited' });
    expect(mockEditDM).toHaveBeenCalled();
  });
});

describe('buildExpiredDMPayload', () => {
  it('returns null on non-finite / non-positive expires_at (defensive against wire drift)', () => {
    for (const bad of [undefined, null, NaN, Infinity, -Infinity, 0, -1, '123', {}]) {
      expect(buildExpiredDMPayload({ expiresAtSeconds: bad })).toBeNull();
    }
  });

  it('floors a fractional value rather than emitting <t:N.M:R>', () => {
    const payload = buildExpiredDMPayload({ expiresAtSeconds: 1717000000.9 });
    expect(payload).not.toBeNull();
    const desc = payload.embeds[0].toJSON().description;
    expect(desc).toContain('<t:1717000000:R>');
    expect(desc).not.toMatch(/<t:\d+\.\d+:R>/);
  });
});
