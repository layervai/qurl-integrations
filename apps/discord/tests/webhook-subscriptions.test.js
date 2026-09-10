
const realFetch = global.fetch;

const mockScan = jest.fn();
jest.mock('../src/store', () => ({
  scanGuildSubscriptions: mockScan,
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

process.env.QURL_API_KEY = 'lv_test_abc';
process.env.QURL_ENDPOINT = 'https://qurl.layerv.ai';
process.env.QURL_WEBHOOK_SECRET = 'default-key-secret';
process.env.BASE_URL = 'http://localhost:3000';
process.env.AWS_REGION = 'us-east-2';
process.env.DDB_TABLE_PREFIX = 'qurl-bot-discord-test-';

const subs = require('../src/webhook-subscriptions');

beforeEach(() => {
  subs._resetForTesting();
  mockScan.mockReset();
  global.fetch = jest.fn(async () => {
    const body = { data: [{ owner_id: 'usr_default', webhook_id: 'wh_default' }] };
    return { ok: true, status: 200, text: async () => JSON.stringify(body) };
  });
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
    expect(subs.getSecretForOwner('usr_unknown')).toBeNull();
  });

  it('folds the default-key owner in via GET /v1/webhooks discovery', async () => {
    mockScan.mockResolvedValueOnce([]);
    await subs.scanOnce();
    expect(subs.isPrimed()).toBe(true);
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
    expect(subs.getSecretForOwner('usr_admin')).toBe('sec_shared');
  });

  it('picks the secret from the most-recently-updated row on rotate-drift', async () => {
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
    const scanPromise = subs.scanOnce();
    subs.upsertGuild({
      guildId: 'g_race', ownerId: 'usr_race', webhookId: 'wh_race', webhookSecret: 'sec_race',
    });
    resolveScan([]);
    await scanPromise;
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

  it('upsert mid-scan overrides scan result for the same owner (rotate-drift race)', async () => {
    let resolveScan;
    mockScan.mockImplementationOnce(() => new Promise((resolve) => { resolveScan = resolve; }));
    const scanPromise = subs.scanOnce();
    subs.upsertGuild({
      guildId: 'g_primary', ownerId: 'usr_rot',
      webhookId: 'wh_rot', webhookSecret: 'sec_post_rotate',
    });
    resolveScan([
      {
        guildId: 'g_sibling', webhookId: 'wh_rot', webhookSecret: 'sec_pre_rotate',
        webhookOwnerId: 'usr_rot',
        updatedAt: '2026-05-01T00:00:00.000Z',
      },
    ]);
    await scanPromise;
    expect(subs.getSecretForOwner('usr_rot')).toBe('sec_post_rotate');
  });

  it('keeps a legacy default-owner secret authoritative during the initial scan', async () => {
    let resolveScan;
    mockScan.mockImplementationOnce(() => new Promise((resolve) => { resolveScan = resolve; }));
    const scanPromise = subs.scanOnce();

    const ownerId = await subs.resolveDefaultOwnerForApiKey('lv_test_abc');
    subs.ensureDefaultOwnerCacheEntry(ownerId);
    expect(subs.getSecretForOwner('usr_default')).toBeNull();

    resolveScan([{
      guildId: 'g_legacy', webhookId: 'wh_default',
      webhookSecret: 'whsec_active_rotation', webhookOwnerId: 'usr_default',
    }]);
    await scanPromise;

    expect(subs.getSecretForOwner('usr_default')).toBe('whsec_active_rotation');
  });

  it('keeps a complete legacy row authoritative during a later default-owner upsert', async () => {
    mockScan.mockResolvedValueOnce([]);
    await subs.scanOnce();
    expect(subs.getSecretForOwner('usr_default')).toBe('default-key-secret');

    let resolveScan;
    mockScan.mockImplementationOnce(() => new Promise((resolve) => { resolveScan = resolve; }));
    const scanPromise = subs.scanOnce();
    subs.ensureDefaultOwnerCacheEntry('usr_default');
    resolveScan([{
      guildId: 'g_legacy', webhookId: 'wh_default',
      webhookSecret: 'whsec_active_rotation', webhookOwnerId: 'usr_default',
    }]);
    await scanPromise;

    expect(subs.getSecretForOwner('usr_default')).toBe('whsec_active_rotation');
  });
});

describe('webhook-subscriptions registry — synchronous local update API', () => {
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
    subs.upsertGuild({
      guildId: 'g1', ownerId: 'usr_admin', webhookId: 'wh_shared', webhookSecret: 'sec_shared',
    });
    subs.upsertGuild({
      guildId: 'g2', ownerId: 'usr_admin', webhookId: 'wh_shared', webhookSecret: 'sec_shared',
    });
    subs.removeGuild({ guildId: 'g1', ownerId: 'usr_admin' });
    expect(subs.getSecretForOwner('usr_admin')).toBe('sec_shared');
  });

  it('removeGuild on a sole-guild owner drops the entry pre-discovery (next scan rediscovers)', async () => {
    subs.upsertGuild({
      guildId: 'g1', ownerId: 'usr_byok', webhookId: 'wh_byok', webhookSecret: 'sec_byok',
    });
    subs.removeGuild({ guildId: 'g1', ownerId: 'usr_byok' });
    expect(subs.getSecretForOwner('usr_byok')).toBeNull();
    mockScan.mockResolvedValueOnce([
      { guildId: 'g1', webhookId: 'wh_byok', webhookSecret: 'sec_byok', webhookOwnerId: 'usr_byok' },
    ]);
    await subs.scanOnce();
    expect(subs.getSecretForOwner('usr_byok')).toBe('sec_byok');
  });
});

describe('webhook-subscriptions registry — scanInFlight re-entrancy guard', () => {
  it('drops an overlapping scanOnce while another is in flight', async () => {
    let resolveFirst;
    mockScan.mockImplementationOnce(() => new Promise((resolve) => { resolveFirst = resolve; }));
    mockScan.mockResolvedValueOnce([
      { guildId: 'g_second', webhookId: 'wh_2', webhookSecret: 'sec_2', webhookOwnerId: 'usr_2' },
    ]);
    const first = subs.scanOnce();
    const second = subs.scanOnce();
    await second;
    expect(mockScan).toHaveBeenCalledTimes(1);
    resolveFirst([]);
    await first;
  });

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
  it('does NOT clobber a BYOK entry that shares the default-key owner_id', async () => {
    mockScan.mockResolvedValueOnce([
      {
        guildId: 'g_admin_byok', webhookId: 'wh_byok',
        webhookSecret: 'sec_shared', webhookOwnerId: 'usr_default',
        updatedAt: '2026-05-21T00:00:00.000Z',
      },
    ]);
    await subs.scanOnce();
    expect(subs.isPrimed()).toBe(true);
    expect(subs.getSecretForOwner('usr_default')).toBe('sec_shared');
  });

  it('BYOK row wins over default-key when secrets DIFFER for the same owner (chosen behavior)', async () => {
    mockScan.mockResolvedValueOnce([
      {
        guildId: 'g_admin_byok', webhookId: 'wh_byok',
        webhookSecret: 'sec_byok_only',
        webhookOwnerId: 'usr_default',
        updatedAt: '2026-05-21T00:00:00.000Z',
      },
    ]);
    await subs.scanOnce();
    expect(subs.isPrimed()).toBe(true);
    expect(subs.getSecretForOwner('usr_default')).toBe('sec_byok_only');
  });

  it('refuses to replace a mismatched legacy cache secret during owner-only linking', async () => {
    mockScan.mockResolvedValueOnce([{
      guildId: 'g_legacy', webhookId: 'wh_legacy',
      webhookSecret: 'whsec_active_rotation', webhookOwnerId: 'usr_default',
    }]);
    await subs.scanOnce();

    expect(() => subs.ensureDefaultOwnerCacheEntry('usr_default'))
      .toThrow(/does not match QURL_WEBHOOK_SECRET/);
    expect(subs.getSecretForOwner('usr_default')).toBe('whsec_active_rotation');
  });
});

describe('webhook-subscriptions registry — default-key discovery', () => {
  it('uses the discovered owner directly for the exact default API key', async () => {
    await expect(subs.resolveDefaultOwnerForApiKey('lv_test_abc'))
      .resolves.toBe('usr_default');
    expect(global.fetch).toHaveBeenCalledTimes(1);
    expect(global.fetch.mock.calls[0][1].headers.Authorization).toBe('Bearer lv_test_abc');
  });

  it('rejects cache updates for an owner other than the discovered default', async () => {
    await subs.resolveDefaultOwnerForApiKey('lv_test_abc');

    expect(() => subs.ensureDefaultOwnerCacheEntry('usr_other'))
      .toThrow(/discovered default owner is required/);
    expect(subs.getSecretForOwner('usr_other')).toBeNull();
  });

  it('reports a missing shared secret separately from an owner mismatch', async () => {
    await subs.resolveDefaultOwnerForApiKey('lv_test_abc');
    const config = require('../src/config');
    const original = config.QURL_WEBHOOK_SECRET;
    config.QURL_WEBHOOK_SECRET = undefined;
    try {
      expect(() => subs.ensureDefaultOwnerCacheEntry('usr_default'))
        .toThrow(/QURL_WEBHOOK_SECRET is required/);
    } finally {
      config.QURL_WEBHOOK_SECRET = original;
    }
  });

  it('does not seed a secret while resolving a different owner before the first scan', async () => {
    global.fetch = jest.fn(async (_url, opts) => {
      const ownerId = opts.headers.Authorization === 'Bearer lv_test_abc'
        ? 'usr_default'
        : 'usr_other';
      return {
        ok: true,
        status: 200,
        text: async () => JSON.stringify({ data: [{ owner_id: ownerId }] }),
      };
    });

    await expect(subs.resolveDefaultOwnerForApiKey('lv_other')).resolves.toBeNull();
    expect(subs.getSecretForOwner('usr_default')).toBeNull();
  });

  it('returns null when the candidate key has no existing subscription', async () => {
    global.fetch = jest.fn(async (_url, opts) => {
      const data = opts.headers.Authorization === 'Bearer lv_test_abc'
        ? [{ owner_id: 'usr_default' }]
        : [];
      return { ok: true, status: 200, text: async () => JSON.stringify({ data }) };
    });

    await expect(subs.resolveDefaultOwnerForApiKey('lv_empty')).resolves.toBeNull();
    expect(global.fetch).toHaveBeenCalledTimes(2);
  });

  it('rejects when the default key has no discoverable subscription', async () => {
    global.fetch = jest.fn(async () => ({
      ok: true, status: 200, text: async () => JSON.stringify({ data: [] }),
    }));

    await expect(subs.resolveDefaultOwnerForApiKey('lv_candidate'))
      .rejects.toMatchObject({
        code: 'DEFAULT_WEBHOOK_OWNER_UNDISCOVERED',
        message: expect.stringMatching(/default subscription owner could not be discovered/),
      });
    expect(global.fetch).toHaveBeenCalledTimes(1);
  });

  it('skips discovery when no shared default secret is configured', async () => {
    const config = require('../src/config');
    const originalSecret = config.QURL_WEBHOOK_SECRET;
    const originalPureByok = config.QURL_WEBHOOK_PURE_BYOK;
    config.QURL_WEBHOOK_SECRET = undefined;
    config.QURL_WEBHOOK_PURE_BYOK = true;
    try {
      await expect(subs.resolveDefaultOwnerForApiKey('lv_byok')).resolves.toBeNull();
      expect(global.fetch).not.toHaveBeenCalled();
    } finally {
      config.QURL_WEBHOOK_SECRET = originalSecret;
      config.QURL_WEBHOOK_PURE_BYOK = originalPureByok;
    }
  });

  it('fails closed when the shared secret is absent without an explicit opt-out', async () => {
    const config = require('../src/config');
    const originalSecret = config.QURL_WEBHOOK_SECRET;
    const originalPureByok = config.QURL_WEBHOOK_PURE_BYOK;
    config.QURL_WEBHOOK_SECRET = undefined;
    config.QURL_WEBHOOK_PURE_BYOK = false;
    try {
      await expect(subs.resolveDefaultOwnerForApiKey('lv_byok'))
        .rejects.toMatchObject({ code: 'DEFAULT_WEBHOOK_OWNER_CONFIG' });
      expect(global.fetch).not.toHaveBeenCalled();
    } finally {
      config.QURL_WEBHOOK_SECRET = originalSecret;
      config.QURL_WEBHOOK_PURE_BYOK = originalPureByok;
    }
  });

  test.each(['QURL_API_KEY', 'QURL_ENDPOINT'])(
    'fails closed when the shared secret is configured but %s is unset',
    async (configKey) => {
      const config = require('../src/config');
      const original = config[configKey];
      config[configKey] = undefined;
      try {
        await expect(subs.resolveDefaultOwnerForApiKey('lv_byok'))
          .rejects.toMatchObject({ code: 'DEFAULT_WEBHOOK_OWNER_CONFIG' });
        expect(global.fetch).not.toHaveBeenCalled();
      } finally {
        config[configKey] = original;
      }
    },
  );

  it('revalidates the default subscription while resolving an alias key', async () => {
    global.fetch = jest.fn(async () => ({
      ok: true,
      status: 200,
      text: async () => JSON.stringify({ data: [{ owner_id: 'usr_default' }] }),
    }));

    await expect(subs.resolveDefaultOwnerForApiKey('lv_test_abc'))
      .resolves.toBe('usr_default');
    await expect(subs.resolveDefaultOwnerForApiKey('lv_alias'))
      .resolves.toBe('usr_default');
    expect(global.fetch).toHaveBeenCalledTimes(3);
    expect(global.fetch.mock.calls[1][1].headers.Authorization).toBe('Bearer lv_test_abc');
    expect(global.fetch.mock.calls[2][1].headers.Authorization).toBe('Bearer lv_alias');
  });

  it('primes the receiver from valid rows while linking still rejects a malformed sibling', async () => {
    mockScan.mockResolvedValueOnce([]);
    global.fetch = jest.fn(async () => ({
      ok: true, status: 200,
      text: async () => JSON.stringify({ data: [{}, { owner_id: 'usr_default' }] }),
    }));
    await subs.scanOnce();
    expect(subs.getSecretForOwner('usr_default')).toBe('default-key-secret');
    await expect(subs.resolveDefaultOwnerForApiKey('lv_alias'))
      .rejects.toMatchObject({ code: 'DEFAULT_WEBHOOK_OWNER_CONTRACT' });
  });

  it('fails closed when the cached default owner has no remaining subscriptions', async () => {
    await expect(subs.resolveDefaultOwnerForApiKey('lv_test_abc'))
      .resolves.toBe('usr_default');
    global.fetch = jest.fn(async () => ({
      ok: true, status: 200, text: async () => JSON.stringify({ data: [] }),
    }));

    await expect(subs.resolveDefaultOwnerForApiKey('lv_alias'))
      .rejects.toMatchObject({ code: 'DEFAULT_WEBHOOK_OWNER_UNDISCOVERED' });
    expect(global.fetch).toHaveBeenCalledTimes(1);
  });

  test.each([
    [
      'a non-empty list omits owner_id',
      [{ webhook_id: 'wh_malformed' }],
      /non-empty qurl-service response omitted owner_id/,
    ],
    [
      'any webhook in a mixed list omits owner_id',
      [{ webhook_id: 'wh_malformed' }, { owner_id: 'usr_default' }],
      /non-empty qurl-service response omitted owner_id/,
    ],
    ['the response data is not an array', {}, /response data must be an array/],
  ])('codes an owner contract failure when %s', async (_case, data, message) => {
    global.fetch = jest.fn(async () => ({
      ok: true,
      status: 200,
      text: async () => JSON.stringify({ data }),
    }));

    await expect(subs.resolveDefaultOwnerForApiKey('lv_alias'))
      .rejects.toMatchObject({
        code: 'DEFAULT_WEBHOOK_OWNER_CONTRACT',
        message: expect.stringMatching(message),
      });
  });

  it('fails closed when a webhook list contains conflicting owner IDs', async () => {
    global.fetch = jest.fn(async () => ({
      ok: true,
      status: 200,
      text: async () => JSON.stringify({
        data: [{ owner_id: 'usr_default' }, { owner_id: 'usr_other' }],
      }),
    }));

    await expect(subs.resolveDefaultOwnerForApiKey('lv_alias'))
      .rejects.toMatchObject({
        code: 'DEFAULT_WEBHOOK_OWNER_CONFLICT',
        message: expect.stringMatching(/conflicting owner_id values/),
      });
  });

  test.each([
    ['omits owner_id', { webhook_id: 'wh_malformed' }, 'DEFAULT_WEBHOOK_OWNER_CONTRACT'],
    ['conflicts with page one', { owner_id: 'usr_other' }, 'DEFAULT_WEBHOOK_OWNER_CONFLICT'],
  ])('fails closed when a later webhook page %s', async (_case, secondPageRow, code) => {
    global.fetch = jest.fn(async (url) => {
      const cursor = new URL(url).searchParams.get('cursor');
      const body = cursor
        ? { data: [secondPageRow] }
        : { data: [{ owner_id: 'usr_default' }], meta: { next_cursor: 'page-2' } };
      return { ok: true, status: 200, text: async () => JSON.stringify(body) };
    });

    await expect(subs.resolveDefaultOwnerForApiKey('lv_alias'))
      .rejects.toMatchObject({ code });
    expect(global.fetch).toHaveBeenCalledTimes(2);
    expect(global.fetch.mock.calls[1][0]).toContain('cursor=page-2&limit=100');
  });

  it('attributes a malformed candidate listing to the candidate key', async () => {
    global.fetch = jest.fn()
      .mockResolvedValueOnce({ ok: true, status: 200, text: async () => JSON.stringify({ data: [{ owner_id: 'usr_default' }] }) })
      .mockResolvedValueOnce({ ok: true, status: 200, text: async () => JSON.stringify({ data: [{}] }) });
    await expect(subs.resolveDefaultOwnerForApiKey('lv_alias'))
      .rejects.toMatchObject({ code: 'CANDIDATE_WEBHOOK_OWNER_CONTRACT' });
  });

  it('fails closed at the webhook discovery pagination cap', async () => {
    global.fetch = jest.fn(async () => ({
      ok: true,
      status: 200,
      text: async () => JSON.stringify({
        data: [{ owner_id: 'usr_default' }],
        meta: { next_cursor: 'more' },
      }),
    }));

    await expect(subs.resolveDefaultOwnerForApiKey('lv_alias'))
      .rejects.toMatchObject({
        code: 'DEFAULT_WEBHOOK_OWNER_CONTRACT',
        message: expect.stringMatching(/pagination cap hit/),
      });
    expect(global.fetch).toHaveBeenCalledTimes(50);
  });

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
      ok: true, status: 200, text: async () => JSON.stringify({ data: [] }),
    }));
    await subs.scanOnce();
    expect(subs.isPrimed()).toBe(true);
    expect(subs.getSecretForOwner('usr_byok')).toBe('sec1');
    expect(subs.getSecretForOwner('usr_default')).toBeNull();
  });

  it('still primes the cache when GET response data field is missing', async () => {
    mockScan.mockResolvedValueOnce([]);
    global.fetch = jest.fn(async () => ({
      ok: true, status: 200, text: async () => JSON.stringify({}),
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

  it('does NOT throw on owner-discovery fetch failure (BYOK delivery survives a transient qurl-service blip)', async () => {
    mockScan.mockResolvedValueOnce([
      { guildId: 'g_byok', webhookId: 'wh_byok', webhookSecret: 'sec_byok', webhookOwnerId: 'usr_byok' },
    ]);
    global.fetch = jest.fn(async () => ({
      ok: false, status: 503, text: async () => '',
    }));
    await expect(subs.scanOnce()).resolves.toBe('completed');
    expect(subs.isPrimed()).toBe(true);
    expect(subs.getSecretForOwner('usr_byok')).toBe('sec_byok');
    expect(subs.getSecretForOwner('usr_default')).toBeNull();
  });

  it('seeds the environment secret after lazy discovery recovers on a primed registry', async () => {
    mockScan.mockResolvedValueOnce([]);
    global.fetch = jest.fn(async () => ({
      ok: false, status: 503, text: async () => '',
    }));
    await subs.scanOnce();
    expect(subs.isPrimed()).toBe(true);
    expect(subs.getSecretForOwner('usr_default')).toBeNull();

    global.fetch = jest.fn(async () => ({
      ok: true,
      status: 200,
      text: async () => JSON.stringify({ data: [{ owner_id: 'usr_default' }] }),
    }));
    const ownerId = await subs.resolveDefaultOwnerForApiKey('lv_test_abc');
    subs.ensureDefaultOwnerCacheEntry(ownerId);

    expect(subs.getSecretForOwner('usr_default')).toBe('default-key-secret');
  });

  it('consecutiveFailures survives a "skipped" scan (long-running outage must still escalate)', async () => {
    const mockLogger = require('../src/logger');
    mockLogger.audit.mockClear();
    for (let i = 0; i < 2; i++) {
      mockScan.mockRejectedValueOnce(new Error('fail'));
      // eslint-disable-next-line no-await-in-loop
      await subs._refreshTickForTesting();
    }
    let resolveLong;
    mockScan.mockReturnValueOnce(new Promise(r => { resolveLong = r; }));
    const inflight = subs._refreshTickForTesting();
    await subs._refreshTickForTesting(); // skipped — must NOT reset counter
    resolveLong([]); // complete the long scan so the next assertion is meaningful
    await inflight;
    mockLogger.audit.mockClear();
    for (let i = 0; i < 3; i++) {
      mockScan.mockRejectedValueOnce(new Error('fail'));
      // eslint-disable-next-line no-await-in-loop
      await subs._refreshTickForTesting();
    }
    const calls = mockLogger.audit.mock.calls.filter(
      ([event]) => event === 'qurl_webhook_cache_refresh_fail',
    );
    expect(calls).toHaveLength(1);
  });

  it('emits QURL_WEBHOOK_DEFAULT_DISCOVERY_FAIL audit exactly once at the escalation threshold', async () => {
    const mockLogger = require('../src/logger');
    mockLogger.audit.mockClear();
    for (let i = 0; i < 5; i++) {
      mockScan.mockResolvedValueOnce([]);
      global.fetch = jest.fn(async () => ({
        ok: false, status: 503, text: async () => '',
      }));
      // eslint-disable-next-line no-await-in-loop
      await subs._refreshTickForTesting();
    }
    const discoveryFailCalls = mockLogger.audit.mock.calls.filter(
      ([event]) => event === 'qurl_webhook_default_discovery_fail',
    );
    expect(discoveryFailCalls).toHaveLength(1);
    expect(discoveryFailCalls[0][1]).toEqual(
      expect.objectContaining({ consecutive_failures: 3 }),
    );
  });

  it('emits QURL_WEBHOOK_CACHE_REFRESH_FAIL audit exactly once across N consecutive failures', async () => {
    const mockLogger = require('../src/logger');
    mockLogger.audit.mockClear();
    for (let i = 0; i < 5; i++) {
      mockScan.mockRejectedValueOnce(new Error('DDB throttled'));
      // eslint-disable-next-line no-await-in-loop
      await subs._refreshTickForTesting();
    }
    const refreshFailCalls = mockLogger.audit.mock.calls.filter(
      ([event]) => event === 'qurl_webhook_cache_refresh_fail',
    );
    expect(refreshFailCalls).toHaveLength(1);
    expect(refreshFailCalls[0][1]).toEqual(
      expect.objectContaining({ consecutive_failures: 3 }),
    );
  });

  it('resets failure counter + lets the next outage alarm fresh', async () => {
    const mockLogger = require('../src/logger');
    mockLogger.audit.mockClear();
    for (let i = 0; i < 3; i++) {
      mockScan.mockRejectedValueOnce(new Error('fail'));
      // eslint-disable-next-line no-await-in-loop
      await subs._refreshTickForTesting();
    }
    mockScan.mockResolvedValueOnce([]);
    await subs._refreshTickForTesting();
    for (let i = 0; i < 3; i++) {
      mockScan.mockRejectedValueOnce(new Error('fail'));
      // eslint-disable-next-line no-await-in-loop
      await subs._refreshTickForTesting();
    }
    const refreshFailCalls = mockLogger.audit.mock.calls.filter(
      ([event]) => event === 'qurl_webhook_cache_refresh_fail',
    );
    expect(refreshFailCalls).toHaveLength(2);
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
