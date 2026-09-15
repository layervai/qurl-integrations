
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

const mockScanGuildSubscriptions = jest.fn();
jest.mock('../src/store', () => ({
  scanGuildSubscriptions: mockScanGuildSubscriptions,
  recordQurlView: jest.fn(async () => ({ result: 'recorded', firstView: true })),
  findSendsByQurlId: jest.fn(async () => []),
  getQurlViews: jest.fn(async () => new Map()),
  healthCheck: jest.fn(),
  getStats: jest.fn(() => ({})),
}));

process.env.QURL_WEBHOOK_SECRET = '';
process.env.QURL_API_KEY = '';
process.env.QURL_ENDPOINT = '';
process.env.DDB_TABLE_PREFIX = 'qurl-bot-discord-test-';
process.env.AWS_REGION = 'us-east-2';
process.env.BASE_URL = 'http://localhost:3000';

const request = require('supertest');
const { app } = require('../src/server');
const subs = require('../src/webhook-subscriptions');

function qurlServiceSign(rawBody, secret) {
  return crypto.createHmac('sha256', secret).update(rawBody).digest('hex');
}

function buildPayload({ ownerId }) {
  return {
    id: 'evt_seam_1',
    type: 'qurl.accessed',
    data: { qurl_id: 'q_seam_aaaaaa', resource_id: 'res-1', access_count: 1, consumed: false },
    owner_id: ownerId,
    timestamp: new Date().toISOString(),
    api_version: '2024-01-01',
  };
}

beforeEach(() => {
  subs._resetForTesting();
  mockScanGuildSubscriptions.mockReset();
});

describe('webhook-subscriptions → receiver integration (seam contract)', () => {
  it('a row returned by scanGuildSubscriptions can be HMAC-verified by the receiver via owner_id lookup', async () => {
    mockScanGuildSubscriptions.mockResolvedValueOnce([
      {
        guildId: 'g_integration',
        webhookId: 'wh_integration',
        webhookSecret: 'sec_integration',
        webhookOwnerId: 'usr_integration',
        updatedAt: '2026-05-22T00:00:00.000Z',
      },
    ]);
    await subs.scanOnce();
    expect(subs.isPrimed()).toBe(true);

    const payload = buildPayload({ ownerId: 'usr_integration' });
    const raw = JSON.stringify(payload);
    const sig = qurlServiceSign(raw, 'sec_integration');

    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', sig)
      .send(raw);
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'recorded' });
  });

  it('within the sibling-lag window, an unknown owner_id returns 503 (retriable — the just-linked-on-peer case)', async () => {
    mockScanGuildSubscriptions.mockResolvedValueOnce([
      {
        guildId: 'g_known',
        webhookId: 'wh_known',
        webhookSecret: 'sec_known',
        webhookOwnerId: 'usr_known',
        updatedAt: '2026-05-22T00:00:00.000Z',
      },
    ]);
    await subs.scanOnce();

    const payload = buildPayload({ ownerId: 'usr_unknown' });
    const raw = JSON.stringify(payload);
    const sig = qurlServiceSign(raw, 'sec_known');

    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', sig)
      .send(raw);
    expect(res.status).toBe(503);
  });

  it('past the sibling-lag window, an unknown owner_id returns 401 (truthful — owner is genuinely absent)', async () => {
    mockScanGuildSubscriptions.mockResolvedValueOnce([
      {
        guildId: 'g_known',
        webhookId: 'wh_known',
        webhookSecret: 'sec_known',
        webhookOwnerId: 'usr_known',
        updatedAt: '2026-05-22T00:00:00.000Z',
      },
    ]);
    await subs.scanOnce();
    subs._setLastScanCompletedAtForTesting(Date.now() - 10 * 60_000);

    const payload = buildPayload({ ownerId: 'usr_unknown' });
    const raw = JSON.stringify(payload);
    const sig = qurlServiceSign(raw, 'sec_known');

    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', sig)
      .send(raw);
    expect(res.status).toBe(401);
  });
});
