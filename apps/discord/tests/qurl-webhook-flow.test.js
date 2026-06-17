// End-to-end flow test: signed payload (qurl-service wire shape) → Express
// route → store → monitor.getFullMsg(). Only the wire-shape + render flow
// is covered here — dedup, out-of-order, and foreign-qurl isolation are
// pinned at the store layer in ddb-store.test.js (they were duplicating
// coverage when previously asserted against the in-memory mock).

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

function makeStore() {
  const rows = new Map();
  return {
    rows,
    async recordQurlView({ qurlId, accessCount, consumed, eventId }) {
      const existing = rows.get(qurlId);
      if (!existing || (existing.lastEventId !== eventId && existing.accessCount < accessCount)) {
        const firstView = !existing || !(existing.accessCount > 0);
        rows.set(qurlId, { accessCount, consumed, lastEventId: eventId });
        return { result: 'recorded', firstView };
      }
      return { result: 'dedup', firstView: false };
    },
    async getQurlViews(qurlIds) {
      const out = new Map();
      for (const k of new Set(qurlIds.filter(Boolean))) {
        const r = rows.get(k);
        if (r) out.set(k, { accessCount: r.accessCount, consumed: r.consumed });
      }
      return out;
    },
    findSendsByQurlId: jest.fn(async () => []),
    healthCheck: jest.fn(),
    getStats: jest.fn(() => ({})),
  };
}

const mockStore = makeStore();
jest.mock('../src/store', () => mockStore);

// Multi-secret receiver: route the flow-test owner_id to the
// flow-test secret via a mocked registry. Without this the receiver
// returns 503 (unprimed) on every webhook delivery.
jest.mock('../src/webhook-subscriptions', () => ({
  isPrimed: () => true,
  getSecretForOwner: (ownerId) => (ownerId === 'usr_flow_test' ? 'flow-test-secret' : null),
  start: jest.fn(),
  stop: jest.fn(),
  upsertGuild: jest.fn(),
  removeGuild: jest.fn(),
  scanOnce: jest.fn(),
  _resetForTesting: jest.fn(),
}));

process.env.QURL_WEBHOOK_SECRET = 'flow-test-secret';
process.env.DDB_TABLE_PREFIX = 'qurl-bot-discord-test-';
process.env.AWS_REGION = 'us-east-2';
process.env.BASE_URL = 'http://localhost:3000';

const request = require('supertest');
const { app } = require('../src/server');
const { _test } = require('../src/commands');
const { monitorLinkStatus, activeMonitors } = _test;

// Reproduces qurl-service's webhook payload signing (HMAC-SHA256 bare
// hex over the raw body), independent of any shared lib so a
// wire-format regression on either side surfaces here, not via a
// coupled import.
function qurlServiceSign(rawBody, secret) {
  return crypto.createHmac('sha256', secret).update(rawBody).digest('hex');
}

// Field shape pinned to qurl-service WebhookEvent JSON tags.
function buildQurlAccessedPayload({ id, qurlId, resourceId, accessCount, consumed }) {
  return {
    id,
    type: 'qurl.accessed',
    data: { qurl_id: qurlId, resource_id: resourceId, access_count: accessCount, consumed },
    owner_id: 'usr_flow_test',
    timestamp: new Date().toISOString(),
    api_version: '2024-01-01',
  };
}

async function deliverWebhook(payload) {
  const raw = JSON.stringify(payload);
  const sig = qurlServiceSign(raw, process.env.QURL_WEBHOOK_SECRET);
  return request(app)
    .post('/webhooks/qurl')
    .set('Content-Type', 'application/json')
    .set('QURL-Signature', sig)
    .send(raw);
}

function makeInteraction() {
  return {
    user: { id: 'sender-1', username: 'Sender' },
    channelId: 'ch-1',
    editReply: jest.fn().mockResolvedValue(undefined),
    member: { displayName: 'Sender' },
  };
}

beforeEach(() => {
  mockStore.rows.clear();
  for (const m of Array.from(activeMonitors)) m.stop();
});

// Construction order matters: the receiver runs under real timers
// (supertest needs setImmediate); the monitor's setInterval is
// installed under fake timers. We populate the store first, then
// switch to fakes for the monitor — never toggle modes once a
// setInterval is registered.
describe('full webhook flow: qurl-service shape → bot receiver → store → monitor UI', () => {
  it('a single delivery flips pending → viewed', async () => {
    const res = await deliverWebhook(buildQurlAccessedPayload({
      id: 'evt_alice_1', qurlId: 'q_aaaaaaaaaa1', resourceId: 'res-1', accessCount: 1, consumed: false,
    }));
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'recorded' });
    expect(mockStore.rows.get('q_aaaaaaaaaa1')).toEqual({
      accessCount: 1, consumed: false, lastEventId: 'evt_alice_1',
    });

    jest.useFakeTimers();
    try {
      const interaction = makeInteraction();
      const qurlLinks = [
        { resourceId: 'res-1', qurlId: 'q_aaaaaaaaaa1', qurlLink: 'https://qurl.site/#at_x', recipientId: 'r1' },
        { resourceId: 'res-1', qurlId: 'q_aaaaaaaaaa2', qurlLink: 'https://qurl.site/#at_y', recipientId: 'r2' },
      ];
      const monitor = monitorLinkStatus(
        'flow-send-1', interaction, qurlLinks,
        [{ id: 'r1', username: 'Alice' }, { id: 'r2', username: 'Bob' }],
        '1m', 'Sent to 2 users', { components: [] }, 2,
      );
      expect(monitor.getFullMsg()).toBe('Sent to 2 users\n👀 0 viewed / 2 pending');

      await jest.advanceTimersByTimeAsync(15000);

      expect(monitor.getFullMsg()).toBe('Sent to 2 users\n👀 1 viewed / 1 pending');
      expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
        content: expect.stringContaining('👀 1 viewed / 1 pending'),
      }));
      monitor.stop();
    } finally {
      jest.useRealTimers();
    }
  });

  it('multiple distinct qurls in one send advance independently', async () => {
    await deliverWebhook(buildQurlAccessedPayload({
      id: 'evt_a', qurlId: 'q_aaaaaaaaaa1', resourceId: 'res-1', accessCount: 1, consumed: false,
    }));
    await deliverWebhook(buildQurlAccessedPayload({
      id: 'evt_c', qurlId: 'q_aaaaaaaaaa3', resourceId: 'res-1', accessCount: 1, consumed: false,
    }));

    jest.useFakeTimers();
    try {
      const interaction = makeInteraction();
      const qurlLinks = [
        { resourceId: 'res-1', qurlId: 'q_aaaaaaaaaa1', qurlLink: 'https://qurl.site/#a', recipientId: 'r1' },
        { resourceId: 'res-1', qurlId: 'q_aaaaaaaaaa2', qurlLink: 'https://qurl.site/#b', recipientId: 'r2' },
        { resourceId: 'res-1', qurlId: 'q_aaaaaaaaaa3', qurlLink: 'https://qurl.site/#c', recipientId: 'r3' },
      ];
      const monitor = monitorLinkStatus(
        'flow-send-3', interaction, qurlLinks,
        [
          { id: 'r1', username: 'Alice' },
          { id: 'r2', username: 'Bob' },
          { id: 'r3', username: 'Carol' },
        ],
        '1m', 'Sent to 3 users', { components: [] }, 3,
      );
      await jest.advanceTimersByTimeAsync(15000);
      expect(monitor.getFullMsg()).toBe('Sent to 3 users\n👀 2 viewed / 1 pending');
      monitor.stop();
    } finally {
      jest.useRealTimers();
    }
  });
});
