/**
 * Full webhook flow integration test
 *
 * Exercises every layer of the bot's qURL-webhook pipeline against the
 * actual Express app + actual monitor code, with the DDB store
 * replaced by an in-memory implementation that mirrors the conditional
 * MAX-merge + BatchGet semantics. The payload is signed with the same
 * algorithm qurl-service uses (HMAC-SHA256 bare hex over the raw body,
 * per qurl-service:internal/domain/webhook.go::SignPayload), and field
 * names match qurl-service's WebhookEvent JSON tags exactly.
 *
 * Why this test exists: my pre-merge unit tests in qurl-webhook.test.js
 * used the wrong field names (`event`/`event_id` instead of `type`/`id`)
 * — they passed because the same wrong names lived on both sides. This
 * flow test pins the contract by sending a payload that mirrors what
 * qurl-service actually emits and asserting downstream side effects
 * (monitor UI updates) through real bot code.
 */

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

// In-memory store that mirrors the conditional MAX-merge semantics of
// the real DDB-backed store. The flow test depends on these semantics
// (replay protection, out-of-order rejection) being preserved end-to-
// end — without them, the monitor would see regressed counts on
// retry.
function makeStore() {
  const rows = new Map(); // qurl_id → {accessCount, consumed, lastEventId}
  return {
    rows,
    async recordQurlView({ qurlId, accessCount, consumed, eventId }) {
      const existing = rows.get(qurlId);
      if (!existing) {
        rows.set(qurlId, { accessCount, consumed, lastEventId: eventId });
        return 'recorded';
      }
      // Match the real DDB ConditionExpression:
      //   attribute_not_exists(last_event_id) OR
      //   (last_event_id <> :eid AND access_count < :n)
      if (existing.lastEventId !== eventId && existing.accessCount < accessCount) {
        rows.set(qurlId, { accessCount, consumed, lastEventId: eventId });
        return 'recorded';
      }
      return 'dedup';
    },
    async getQurlViews(qurlIds) {
      const out = new Map();
      const keys = [...new Set(qurlIds.filter(Boolean))];
      for (const k of keys) {
        const r = rows.get(k);
        if (r) out.set(k, { accessCount: r.accessCount, consumed: r.consumed });
      }
      return out;
    },
    healthCheck: jest.fn(),
    getStats: jest.fn(() => ({})),
  };
}

const mockStore = makeStore();
jest.mock('../src/store', () => mockStore);

process.env.QURL_WEBHOOK_SECRET = 'flow-test-secret';
process.env.DDB_TABLE_PREFIX = 'qurl-bot-discord-test-';
process.env.AWS_REGION = 'us-east-2';
process.env.BASE_URL = 'http://localhost:3000';

const request = require('supertest');
const { app } = require('../src/server');
const { _test } = require('../src/commands');
const { monitorLinkStatus, activeMonitors } = _test;

// Mirror qurl-service:internal/domain/webhook.go::SignPayload. Bare hex
// HMAC-SHA256 over the raw body. Reproducing the algorithm verbatim
// (rather than importing a shared lib) is intentional: this test is
// the canonical place a wire-format regression in EITHER side would
// surface, so the algorithm needs to live independently of whatever
// shared crypto module the bot or qurl-service ship next.
function qurlServiceSign(rawBody, secret) {
  return crypto.createHmac('sha256', secret).update(rawBody).digest('hex');
}

// Mirror qurl-service:internal/domain/webhook.go::WebhookEvent exactly.
// Field names + types must match WebhookEvent's `json:` tags. This is
// the test-side contract pin — a mismatch with qurl-service's emit
// shape fails CI loudly rather than going silent in prod.
function buildQurlAccessedPayload({ id, qurlId, resourceId, accessCount, consumed }) {
  return {
    id,
    type: 'qurl.accessed',
    data: {
      qurl_id: qurlId,
      resource_id: resourceId,
      access_count: accessCount,
      consumed,
    },
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
// (supertest needs setImmediate to flush); the monitor's setInterval
// must be set up under fake timers. We populate the store first
// (real timers, via the receiver), then switch to fakes and
// construct + tick the monitor. This avoids the toggle-mid-monitor
// trap where a setInterval registered under one timer mode is
// abandoned when the other mode takes over.
describe('full webhook flow: qurl-service shape → bot receiver → store → monitor UI', () => {
  it('a single qurl.accessed delivery flips one recipient from pending → viewed', async () => {
    // Stage 1: receiver under real timers. Webhook signed with qurl-
    // service's exact algorithm + field shape — the wire-format
    // regression class that pre-merge unit tests missed.
    const res = await deliverWebhook(buildQurlAccessedPayload({
      id: 'evt_alice_1', qurlId: 'q_aaaaaaaaaa1', resourceId: 'res-1', accessCount: 1, consumed: false,
    }));
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'recorded' });
    expect(mockStore.rows.get('q_aaaaaaaaaa1')).toEqual({
      accessCount: 1, consumed: false, lastEventId: 'evt_alice_1',
    });

    // Stage 2: monitor under fake timers. Constructed AFTER the
    // store is populated so a single 15s tick is enough to see the
    // update; we never toggle timer modes once the setInterval is
    // installed.
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

      await jest.advanceTimersByTimeAsync(15000); // 1m expiry → 15s pollInterval

      expect(monitor.getFullMsg()).toBe('Sent to 2 users\n👀 1 viewed / 1 pending');
      expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
        content: expect.stringContaining('👀 1 viewed / 1 pending'),
      }));
      monitor.stop();
    } finally {
      jest.useRealTimers();
    }
  });

  it('redelivery (same event_id) is deduped and the counter does not double-advance', async () => {
    const p = buildQurlAccessedPayload({
      id: 'evt_replay', qurlId: 'q_aaaaaaaaaa1', resourceId: 'res-1', accessCount: 1, consumed: false,
    });
    const first = await deliverWebhook(p);
    const second = await deliverWebhook(p); // exact same payload + signature
    expect(first.body).toEqual({ status: 'recorded' });
    expect(second.body).toEqual({ status: 'dedup' });
    expect(mockStore.rows.get('q_aaaaaaaaaa1').accessCount).toBe(1);
  });

  it('out-of-order delivery (lower count after higher count) is silently rejected', async () => {
    // qurl-service docs explicitly warn that access_count is best-effort
    // under concurrent load. The conditional MAX-merge guarantees a
    // late-arriving lower-count event never regresses the row.
    await deliverWebhook(buildQurlAccessedPayload({
      id: 'evt_high', qurlId: 'q_aaaaaaaaaa1', resourceId: 'res-1', accessCount: 3, consumed: false,
    }));
    const stale = await deliverWebhook(buildQurlAccessedPayload({
      id: 'evt_low', qurlId: 'q_aaaaaaaaaa1', resourceId: 'res-1', accessCount: 1, consumed: false,
    }));
    expect(stale.body).toEqual({ status: 'dedup' });
    expect(mockStore.rows.get('q_aaaaaaaaaa1').accessCount).toBe(3);
  });

  it('multiple distinct qurls in the same send each advance independently', async () => {
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
      // Two of three flipped to opened — Bob's still pending.
      expect(monitor.getFullMsg()).toBe('Sent to 3 users\n👀 2 viewed / 1 pending');
      monitor.stop();
    } finally {
      jest.useRealTimers();
    }
  });

  it('non-tracked qurl_id (foreign send) writes to the store but does not affect this monitor', async () => {
    // Webhook for a qurl_id that belongs to a different send. The
    // store accepts it (the receiver isn't send-aware); the monitor's
    // BatchGet against its own qurlLinks set ignores it.
    await deliverWebhook(buildQurlAccessedPayload({
      id: 'evt_foreign', qurlId: 'q_zzzzzzzzzzz', resourceId: 'res-99', accessCount: 1, consumed: false,
    }));
    expect(mockStore.rows.has('q_zzzzzzzzzzz')).toBe(true);

    jest.useFakeTimers();
    try {
      const interaction = makeInteraction();
      const qurlLinks = [
        { resourceId: 'res-1', qurlId: 'q_aaaaaaaaaa1', qurlLink: 'https://qurl.site/#a', recipientId: 'r1' },
      ];
      const monitor = monitorLinkStatus(
        'flow-send-4', interaction, qurlLinks,
        [{ id: 'r1', username: 'Alice' }],
        '1m', 'Sent to 1 user', { components: [] }, 1,
      );
      await jest.advanceTimersByTimeAsync(15000);
      expect(monitor.getFullMsg()).toBe('Sent to 1 user\n👀 0 viewed / 1 pending');
      monitor.stop();
    } finally {
      jest.useRealTimers();
    }
  });
});
