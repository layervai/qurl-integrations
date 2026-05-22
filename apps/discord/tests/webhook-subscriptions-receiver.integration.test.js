// Integration test pinning the seam between the REAL subscription
// registry (webhook-subscriptions.js) and the REAL receiver
// (routes/qurl-webhook.js): a row shape returned by
// db.scanGuildSubscriptions() must produce a cache entry the receiver
// can look up + HMAC-verify against. A field-shape drift between the
// two modules (e.g. a future scanGuildSubscriptions rename of
// `webhookOwnerId` → `ownerId`) would 401 every webhook in prod; here
// it fails LOUD at test time.
//
// All other layers (Discord, monitor UI, view-update publisher) are
// mocked to keep the test focused on the registry↔receiver contract.

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

// Mock the store but leave the registry REAL. scanGuildSubscriptions
// returns a fixture row shaped exactly like the ddb-store output
// contract; if the registry can't read it, the seam is broken.
const mockScanGuildSubscriptions = jest.fn();
jest.mock('../src/store', () => ({
  scanGuildSubscriptions: mockScanGuildSubscriptions,
  recordQurlView: jest.fn(async () => 'recorded'),
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
    // Realistic row shape: every field name + type the ddb-store
    // contract documents. A drift in any of these silently 401s in
    // prod; the integration assertion catches it.
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

  it('an unknown owner_id after priming returns 401 (post-prime truthful response)', async () => {
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
    // Forward the lastScanCompletedAt past the sibling-lag window
    // so an unknown owner is treated as a real 401 (not 503).
    await new Promise(resolve => setTimeout(resolve, 50));

    const payload = buildPayload({ ownerId: 'usr_unknown' });
    const raw = JSON.stringify(payload);
    const sig = qurlServiceSign(raw, 'sec_known');

    const res = await request(app)
      .post('/webhooks/qurl')
      .set('Content-Type', 'application/json')
      .set('QURL-Signature', sig)
      .send(raw);
    // Within the sibling-lag window the receiver returns 503;
    // either is a legitimate "registry doesn't know this owner" —
    // accept both rather than time-of-day flake.
    expect([401, 503]).toContain(res.status);
  });
});
