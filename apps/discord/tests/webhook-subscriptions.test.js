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

  // Rotate-drift tiebreaker: when sibling rows disagree on secret,
  // the newest updatedAt MUST win (qurl-service signs with the
  // post-rotate secret; a stale-row pick would 401 every webhook).
  it('picks the secret from the most-recently-updated row on rotate-drift', async () => {
    // Stale row first so first-write-wins regressions fail this test.
    mockScan.mockResolvedValueOnce([
      {
        guildId: 'g1', webhookId: 'wh_v1', webhookSecret: 'sec_stale', webhookOwnerId: 'usr_admin',
        updatedAt: '2026-05-01T00:00:00.000Z',
      },
      {
        guildId: 'g2', webhookId: 'wh_v2', webhookSecret: 'sec_fresh', webhookOwnerId: 'usr_admin',
        updatedAt: '2026-05-21T00:00:00.000Z',
      },
    ]);
    await subs.scanOnce();
    expect(subs.getSecretForOwner('usr_admin')).toBe('sec_fresh');
  });

  it('picks the most-recently-updated row regardless of scan order', async () => {
    mockScan.mockResolvedValueOnce([
      {
        guildId: 'g2', webhookId: 'wh_v2', webhookSecret: 'sec_fresh', webhookOwnerId: 'usr_admin',
        updatedAt: '2026-05-21T00:00:00.000Z',
      },
      {
        guildId: 'g1', webhookId: 'wh_v1', webhookSecret: 'sec_stale', webhookOwnerId: 'usr_admin',
        updatedAt: '2026-05-01T00:00:00.000Z',
      },
    ]);
    await subs.scanOnce();
    expect(subs.getSecretForOwner('usr_admin')).toBe('sec_fresh');
  });

  it('treats missing updatedAt as oldest (legacy row never beats a timestamped one)', async () => {
    mockScan.mockResolvedValueOnce([
      { guildId: 'g_legacy', webhookId: 'wh_legacy', webhookSecret: 'sec_legacy', webhookOwnerId: 'usr_admin' },
      {
        guildId: 'g_new', webhookId: 'wh_new', webhookSecret: 'sec_new', webhookOwnerId: 'usr_admin',
        updatedAt: '2026-05-21T00:00:00.000Z',
      },
    ]);
    await subs.scanOnce();
    expect(subs.getSecretForOwner('usr_admin')).toBe('sec_new');
  });
});

describe('webhook-subscriptions registry — concurrent upsert during scan', () => {
  it('preserves an upsertGuild entry written while scanOnce is awaiting', async () => {
    let resolveScan;
    mockScan.mockImplementationOnce(() => new Promise((resolve) => { resolveScan = resolve; }));
    // Kick off scanOnce — it'll suspend on the scan promise.
    const scanPromise = subs.scanOnce();
    // While scan is suspended, a concurrent /qurl setup writes a row.
    subs.upsertGuild({
      guildId: 'g_race', ownerId: 'usr_race', webhookId: 'wh_race', webhookSecret: 'sec_race',
    });
    // Scan completes — its `rows` doesn't include the racing guild
    // because the row was written after the DDB read started.
    resolveScan([]);
    await scanPromise;
    // The synchronous upsert MUST survive the rebuild. Without the
    // upsertsDuringScan tracker, the clear() would wipe it and this
    // expectation would fail with null.
    expect(subs.getSecretForOwner('usr_race')).toBe('sec_race');
  });

  it('scan supersedes a pre-scan upsert when DDB row is also present', async () => {
    subs.upsertGuild({
      guildId: 'g_pre', ownerId: 'usr_pre', webhookId: 'wh_pre', webhookSecret: 'sec_pre',
    });
    mockScan.mockResolvedValueOnce([
      {
        guildId: 'g_pre', webhookId: 'wh_pre', webhookSecret: 'sec_pre_from_scan',
        webhookOwnerId: 'usr_pre',
        updatedAt: '2026-05-21T00:00:00.000Z',
      },
    ]);
    await subs.scanOnce();
    expect(subs.getSecretForOwner('usr_pre')).toBe('sec_pre_from_scan');
  });

  // Cycle-6 cr regression: scan caught a pre-rotate sibling row for
  // owner X while a concurrent linkGuildWebhookSubscription wrote
  // the post-rotate secret via upsertGuild. Without the cycle-6 fix
  // (upsertsDuringScan overrides scan-result entries), the cache
  // would hold the stale secret until the next 30s tick.
  it('upsert mid-scan overrides scan result for the same owner (rotate-drift race)', async () => {
    let resolveScan;
    mockScan.mockImplementationOnce(() => new Promise((resolve) => { resolveScan = resolve; }));
    const scanPromise = subs.scanOnce();
    // Mid-scan: a concurrent link wrote the POST-rotate secret in
    // memory via upsertGuild. DDB write may or may not be visible
    // to the in-flight scan; assume it is and the scan caught a
    // stale sibling row.
    subs.upsertGuild({
      guildId: 'g_primary', ownerId: 'usr_rot',
      webhookId: 'wh_rot', webhookSecret: 'sec_post_rotate',
    });
    // Scan resolves with the PRE-rotate sibling row (rotate drift).
    resolveScan([
      {
        guildId: 'g_sibling', webhookId: 'wh_rot', webhookSecret: 'sec_pre_rotate',
        webhookOwnerId: 'usr_rot',
        updatedAt: '2026-05-01T00:00:00.000Z',
      },
    ]);
    await scanPromise;
    // The in-memory upsert MUST win — qurl-service signs with the
    // post-rotate secret, so any cached pre-rotate secret would
    // 401 every inbound webhook for this owner.
    expect(subs.getSecretForOwner('usr_rot')).toBe('sec_post_rotate');
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

  // Defensive: pre-discovery (defaultOwnerId is null), removeGuild on
  // a BYOK guild whose owner happens to equal what would BECOME the
  // default owner could otherwise drop the entry. Doesn't matter
  // today (no production caller), but pins the future-/qurl-unlink
  // contract before that caller lands.
  it('removeGuild on a sole-guild owner drops the entry pre-discovery (next scan rediscovers)', async () => {
    subs.upsertGuild({
      guildId: 'g1', ownerId: 'usr_byok', webhookId: 'wh_byok', webhookSecret: 'sec_byok',
    });
    subs.removeGuild({ guildId: 'g1', ownerId: 'usr_byok' });
    expect(subs.getSecretForOwner('usr_byok')).toBeNull();
    // The DDB row is still authoritative — the next 30s tick will
    // restore the entry if the caller didn't also DDB-delete it.
    mockScan.mockResolvedValueOnce([
      { guildId: 'g1', webhookId: 'wh_byok', webhookSecret: 'sec_byok', webhookOwnerId: 'usr_byok' },
    ]);
    await subs.scanOnce();
    expect(subs.getSecretForOwner('usr_byok')).toBe('sec_byok');
  });
});

describe('webhook-subscriptions registry — scanInFlight re-entrancy guard', () => {
  // If a tick takes longer than the 30s interval, the next interval
  // fires while the first is still awaiting. Both calls would
  // .clear() upsertsDuringScan and the second wipes the first's in-
  // flight tracking. The guard drops the overlap. Pin the behavior
  // so a refactor doesn't accidentally re-introduce the race.
  it('drops an overlapping scanOnce while another is in flight', async () => {
    let resolveFirst;
    mockScan.mockImplementationOnce(() => new Promise((resolve) => { resolveFirst = resolve; }));
    mockScan.mockResolvedValueOnce([
      { guildId: 'g_second', webhookId: 'wh_2', webhookSecret: 'sec_2', webhookOwnerId: 'usr_2' },
    ]);
    const first = subs.scanOnce();
    // The second call returns IMMEDIATELY because scanInFlight is true;
    // mockScan is NOT invoked a second time.
    const second = subs.scanOnce();
    await second;
    expect(mockScan).toHaveBeenCalledTimes(1);
    // Now finish the first scan so subsequent tests aren't tainted.
    resolveFirst([]);
    await first;
  });

  // Skipped scans return 'skipped' sentinel; completed scans return
  // 'completed'. refreshTick uses this to avoid resetting the
  // consecutiveFailures counter when a slow scan overlaps the next
  // tick — otherwise an alarm-worthy outage would be masked.
  it('returns "skipped" sentinel when another scan is in flight, "completed" otherwise', async () => {
    let resolveFirst;
    mockScan.mockImplementationOnce(() => new Promise((resolve) => { resolveFirst = resolve; }));
    const first = subs.scanOnce();
    const second = await subs.scanOnce();
    expect(second).toBe('skipped');
    resolveFirst([]);
    const firstResult = await first;
    expect(firstResult).toBe('completed');
  });
});

describe('webhook-subscriptions registry — default-key + BYOK owner collision', () => {
  // Edge case: an admin links a BYOK guild using the SAME auth0 owner
  // as the bot's default key (e.g., bot operator running /qurl setup
  // in their own server). The BYOK row's webhook_owner_id ===
  // discoveredOwner. Without the guard, the default-key fold would
  // overwrite the BYOK entry's guildIds — observationally benign
  // (the secret is the same) but a silent overwrite is dishonest.
  it('does NOT clobber a BYOK entry that shares the default-key owner_id', async () => {
    mockScan.mockResolvedValueOnce([
      {
        guildId: 'g_admin_byok', webhookId: 'wh_byok',
        webhookSecret: 'sec_shared', webhookOwnerId: 'usr_default',
        updatedAt: '2026-05-21T00:00:00.000Z',
      },
    ]);
    // discoverDefaultOwnerId returns 'usr_default' — same as BYOK row.
    await subs.scanOnce();
    expect(subs.isPrimed()).toBe(true);
    // Secret resolves; BYOK entry is preserved (webhookId stays the
    // string 'wh_byok', NOT the DEFAULT_KEY_SENTINEL Symbol).
    expect(subs.getSecretForOwner('usr_default')).toBe('sec_shared');
  });
});

describe('webhook-subscriptions registry — default-key discovery', () => {
  // When GET /v1/webhooks returns an empty list (Lambda hasn't run
  // on a fresh deploy, or QURL_API_KEY/QURL_ENDPOINT are unset),
  // discoverDefaultOwnerId returns null. scanOnce must still flip
  // primed=true (so inbound webhooks for KNOWN owners aren't held in
  // 503), but the default-key entry must NOT be folded in (anything
  // signed by the Lambda's default sub will 401 until discovery
  // succeeds — which is the truthful response, since the bot can't
  // verify those signatures).
  it('still primes the cache when discoverDefaultOwnerId returns null', async () => {
    mockScan.mockResolvedValueOnce([
      { guildId: 'g1', webhookId: 'wh1', webhookSecret: 'sec1', webhookOwnerId: 'usr_byok' },
    ]);
    global.fetch = jest.fn(async () => ({
      ok: true,
      json: async () => ({ data: [] }),
    }));
    await subs.scanOnce();
    expect(subs.isPrimed()).toBe(true);
    // BYOK guild's entry still made it in.
    expect(subs.getSecretForOwner('usr_byok')).toBe('sec1');
    // Default-key entry absent — no usr_default lookup possible.
    expect(subs.getSecretForOwner('usr_default')).toBeNull();
  });

  it('still primes the cache when GET response data field is missing', async () => {
    mockScan.mockResolvedValueOnce([]);
    global.fetch = jest.fn(async () => ({
      ok: true,
      json: async () => ({}),
    }));
    await subs.scanOnce();
    expect(subs.isPrimed()).toBe(true);
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
