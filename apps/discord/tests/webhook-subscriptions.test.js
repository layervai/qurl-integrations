// In-process subscription registry — tests for cache priming, refresh,
// failure escalation, multi-guild-shared-owner ref-counting, and the
// synchronous local-update API. Mocks db.scanGuildSubscriptions +
// fetch (used for default-key owner_id discovery) so the registry
// loop is exercised against a deterministic fake.
//
// Heavily isolated via jest.resetModules in beforeEach — every test
// starts with a fresh registry. The module's _resetForTesting helper
// is the supported way to wipe in-process state without re-requiring;
// we use both belt-and-suspenders.

const realFetch = global.fetch;

const mockScan = jest.fn();
jest.mock('../src/store', () => ({
  scanGuildSubscriptions: mockScan,
}));

process.env.QURL_API_KEY = 'lv_test_abc';
process.env.QURL_ENDPOINT = 'https://qurl.layerv.ai';
process.env.QURL_WEBHOOK_SECRET = 'default-key-secret';
process.env.BASE_URL = 'http://localhost:3000';
// AWS_REGION + DDB_TABLE_PREFIX are required by config.js validators
// even though this test never touches DDB through the real path.
process.env.AWS_REGION = 'us-east-2';
process.env.DDB_TABLE_PREFIX = 'qurl-bot-discord-test-';

const subs = require('../src/webhook-subscriptions');

beforeEach(() => {
  subs._resetForTesting();
  mockScan.mockReset();
  // Default fetch mock returns the default-key owner_id discovery
  // shape so the refresh tick can fold it in.
  global.fetch = jest.fn(async () => ({
    ok: true,
    json: async () => ({ data: [{ owner_id: 'usr_default', webhook_id: 'wh_default' }] }),
  }));
});

afterAll(() => {
  global.fetch = realFetch;
});

describe('webhook-subscriptions registry — priming + lookup', () => {
  it('returns null + isPrimed()=false before any scan completes', () => {
    expect(subs.isPrimed()).toBe(false);
    expect(subs.getSecretForOwner('usr_test')).toBeNull();
  });

  it('populates the map + flips isPrimed after a successful scanOnce', async () => {
    mockScan.mockResolvedValueOnce([
      { guildId: 'g1', webhookId: 'wh_g1', webhookSecret: 'sec_g1', webhookOwnerId: 'usr_a' },
    ]);
    await subs.scanOnce();
    expect(subs.isPrimed()).toBe(true);
    expect(subs.getSecretForOwner('usr_a')).toBe('sec_g1');
    // Unrelated owner_id still resolves to null; primed means
    // "I've completed a scan", not "everyone is registered".
    expect(subs.getSecretForOwner('usr_unknown')).toBeNull();
  });

  it('folds the default-key owner in via GET /v1/webhooks discovery', async () => {
    mockScan.mockResolvedValueOnce([]);
    await subs.scanOnce();
    expect(subs.isPrimed()).toBe(true);
    // discoverDefaultOwnerId returned usr_default + QURL_WEBHOOK_SECRET
    // env is wired as the default secret.
    expect(subs.getSecretForOwner('usr_default')).toBe('default-key-secret');
  });

  it('rebuilds (not merges) on each scan — a removed row drops from the cache', async () => {
    mockScan.mockResolvedValueOnce([
      { guildId: 'g1', webhookId: 'wh_g1', webhookSecret: 'sec_a', webhookOwnerId: 'usr_a' },
      { guildId: 'g2', webhookId: 'wh_g2', webhookSecret: 'sec_b', webhookOwnerId: 'usr_b' },
    ]);
    await subs.scanOnce();
    expect(subs.getSecretForOwner('usr_a')).toBe('sec_a');
    expect(subs.getSecretForOwner('usr_b')).toBe('sec_b');

    mockScan.mockResolvedValueOnce([
      { guildId: 'g2', webhookId: 'wh_g2', webhookSecret: 'sec_b', webhookOwnerId: 'usr_b' },
    ]);
    await subs.scanOnce();
    expect(subs.getSecretForOwner('usr_a')).toBeNull();
    expect(subs.getSecretForOwner('usr_b')).toBe('sec_b');
  });
});

describe('webhook-subscriptions registry — multi-guild-shared-owner', () => {
  // When one auth0 admin installs the bot in N guilds, every row
  // shares the same owner_id + webhook_id + secret (qurl-service
  // dedupes on (owner_id, url) per ensureWebhookSubscription). The
  // cache entries collide on identical content; the GuildIds set
  // tracks all guilds that point at this entry. Important for the
  // unlink path's reference-counting.
  it('coalesces N rows sharing an owner_id into one cache entry', async () => {
    mockScan.mockResolvedValueOnce([
      { guildId: 'g1', webhookId: 'wh_shared', webhookSecret: 'sec_shared', webhookOwnerId: 'usr_admin' },
      { guildId: 'g2', webhookId: 'wh_shared', webhookSecret: 'sec_shared', webhookOwnerId: 'usr_admin' },
      { guildId: 'g3', webhookId: 'wh_shared', webhookSecret: 'sec_shared', webhookOwnerId: 'usr_admin' },
    ]);
    await subs.scanOnce();
    // One owner → one cached secret. The receiver doesn't need to
    // know about guild_ids — that's a DDB-layer concern for the
    // unlink ref-count path.
    expect(subs.getSecretForOwner('usr_admin')).toBe('sec_shared');
  });
});

describe('webhook-subscriptions registry — synchronous local update API', () => {
  // upsertGuild / removeGuild are called from setGuildApiKey-adjacent
  // flows so the registering replica is immediately correct. Sibling
  // replicas converge on the next tick.
  it('upsertGuild makes the secret immediately resolvable', () => {
    subs.upsertGuild({
      guildId: 'g_new', ownerId: 'usr_new', webhookId: 'wh_new', webhookSecret: 'sec_new',
    });
    expect(subs.getSecretForOwner('usr_new')).toBe('sec_new');
  });

  it('upsertGuild on an existing owner updates the secret (last-write-wins)', () => {
    subs.upsertGuild({
      guildId: 'g1', ownerId: 'usr_a', webhookId: 'wh_a', webhookSecret: 'sec_v1',
    });
    subs.upsertGuild({
      guildId: 'g1', ownerId: 'usr_a', webhookId: 'wh_a', webhookSecret: 'sec_v2',
    });
    expect(subs.getSecretForOwner('usr_a')).toBe('sec_v2');
  });

  it('removeGuild drops the last guild + clears the cache entry', () => {
    subs.upsertGuild({
      guildId: 'g1', ownerId: 'usr_solo', webhookId: 'wh1', webhookSecret: 'sec1',
    });
    subs.removeGuild({ guildId: 'g1', ownerId: 'usr_solo' });
    expect(subs.getSecretForOwner('usr_solo')).toBeNull();
  });

  it('removeGuild keeps the entry when sibling guilds remain', () => {
    // Multi-guild-shared-owner: removing g1 doesn't kill g2's
    // delivery. The receiver's secret lookup stays valid for usr_admin.
    subs.upsertGuild({
      guildId: 'g1', ownerId: 'usr_admin', webhookId: 'wh_shared', webhookSecret: 'sec_shared',
    });
    subs.upsertGuild({
      guildId: 'g2', ownerId: 'usr_admin', webhookId: 'wh_shared', webhookSecret: 'sec_shared',
    });
    subs.removeGuild({ guildId: 'g1', ownerId: 'usr_admin' });
    expect(subs.getSecretForOwner('usr_admin')).toBe('sec_shared');
  });
});

describe('webhook-subscriptions registry — first-scan-failure semantics', () => {
  it('throws on scanOnce DDB failure (caller increments failure counter)', async () => {
    mockScan.mockRejectedValueOnce(new Error('DDB throttled'));
    await expect(subs.scanOnce()).rejects.toThrow(/DDB throttled/);
    expect(subs.isPrimed()).toBe(false);
  });

  it('throws on owner-discovery fetch failure (caller increments failure counter)', async () => {
    mockScan.mockResolvedValueOnce([]);
    global.fetch = jest.fn(async () => ({ ok: false, status: 503, json: async () => ({}) }));
    await expect(subs.scanOnce()).rejects.toThrow(/GET \/v1\/webhooks returned 503/);
    expect(subs.isPrimed()).toBe(false);
  });

  it('after recovery, a successful scan flips primed back to true', async () => {
    mockScan.mockRejectedValueOnce(new Error('DDB throttled'));
    await expect(subs.scanOnce()).rejects.toThrow();
    expect(subs.isPrimed()).toBe(false);
    mockScan.mockResolvedValueOnce([
      { guildId: 'g1', webhookId: 'wh1', webhookSecret: 'sec1', webhookOwnerId: 'usr_x' },
    ]);
    await subs.scanOnce();
    expect(subs.isPrimed()).toBe(true);
    expect(subs.getSecretForOwner('usr_x')).toBe('sec1');
  });
});
