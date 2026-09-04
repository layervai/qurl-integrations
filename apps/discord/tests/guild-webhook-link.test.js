// Tests for the link orchestrator (db + registrar wiring + partial-
// failure rollback). Cache-side scenarios live in
// tests/webhook-subscriptions.test.js.

const mockEnsureWebhookSubscription = jest.fn();
const mockDeleteSubscription = jest.fn();
jest.mock('../src/qurl-webhook-registrar', () => ({
  ...jest.requireActual('../src/qurl-webhook-registrar'),
  ensureWebhookSubscription: mockEnsureWebhookSubscription,
  deleteSubscription: mockDeleteSubscription,
}));

const mockSetGuildWebhookSubscription = jest.fn();
const mockSetGuildDefaultWebhookOwner = jest.fn();
const mockPropagateGuildWebhookSubscription = jest.fn();
jest.mock('../src/store', () => ({
  setGuildWebhookSubscription: mockSetGuildWebhookSubscription,
  setGuildDefaultWebhookOwner: mockSetGuildDefaultWebhookOwner,
  propagateGuildWebhookSubscription: mockPropagateGuildWebhookSubscription,
  healthCheck: jest.fn(),
}));

const mockUpsertGuild = jest.fn();
const mockEnsureDefaultOwnerCacheEntry = jest.fn();
const mockRemoveGuild = jest.fn();
const mockResolveDefaultOwnerForApiKey = jest.fn();
jest.mock('../src/webhook-subscriptions', () => ({
  upsertGuild: mockUpsertGuild,
  ensureDefaultOwnerCacheEntry: mockEnsureDefaultOwnerCacheEntry,
  removeGuild: mockRemoveGuild,
  resolveDefaultOwnerForApiKey: mockResolveDefaultOwnerForApiKey,
  isPrimed: () => true,
  getSecretForOwner: () => null,
  start: jest.fn(),
  stop: jest.fn(),
  scanOnce: jest.fn(),
  _resetForTesting: jest.fn(),
}));

const mockAudit = jest.fn();
const mockWarn = jest.fn();
const mockInfo = jest.fn();
jest.mock('../src/logger', () => ({
  info: mockInfo,
  warn: mockWarn,
  error: jest.fn(),
  debug: jest.fn(),
  audit: mockAudit,
}));

process.env.QURL_API_KEY = 'lv_test_link';
process.env.QURL_ENDPOINT = 'https://qurl.example';
process.env.QURL_WEBHOOK_SECRET = 'wsec_test';
process.env.BASE_URL = 'http://localhost:3000';
process.env.AWS_REGION = 'us-east-2';
process.env.DDB_TABLE_PREFIX = 'qurl-bot-discord-test-';

const {
  linkGuildWebhookSubscription, LINK_RESULTS,
} = require('../src/guild-webhook-link');
const { AUDIT_EVENTS } = require('../src/constants');

beforeEach(() => {
  jest.clearAllMocks();
  mockEnsureWebhookSubscription.mockResolvedValue({
    webhookId: 'wh_ok',
    secret: 'sec_ok',
    action: 'created',
    ownerId: 'usr_ok',
  });
  mockSetGuildWebhookSubscription.mockResolvedValue();
  mockSetGuildDefaultWebhookOwner.mockResolvedValue();
  mockPropagateGuildWebhookSubscription.mockResolvedValue({ updated: 0, failed: 0, skipped: 0 });
  mockDeleteSubscription.mockResolvedValue();
  mockResolveDefaultOwnerForApiKey.mockResolvedValue(null);
});

describe('linkGuildWebhookSubscription — partial-failure rollback', () => {
  it('fires bestEffortDeleteSubscription when setGuildWebhookSubscription throws', async () => {
    mockSetGuildWebhookSubscription.mockRejectedValueOnce(new Error('DDB throttled'));
    const result = await linkGuildWebhookSubscription({
      guildId: 'g1', apiKey: 'lv_guild_1',
    });
    expect(result).toEqual({ ok: false, reason: LINK_RESULTS.PERSIST_FAILED });
    // Rollback DELETE attempted with the freshly-created webhookId.
    expect(mockDeleteSubscription).toHaveBeenCalledWith({
      apiEndpoint: 'https://qurl.example', apiKey: 'lv_guild_1', webhookId: 'wh_ok',
    });
    // Failure audit fires (cycle-2 cr concern #6).
    expect(mockAudit).toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTER_FAILED,
      expect.objectContaining({ reason: LINK_RESULTS.PERSIST_FAILED, guild_id: 'g1' }),
    );
  });

  it('rolls back + emits OWNER_MISSING failure audit when registrar response lacks ownerId', async () => {
    mockEnsureWebhookSubscription.mockResolvedValueOnce({
      webhookId: 'wh_no_owner', secret: 'sec_x', action: 'created', ownerId: undefined,
    });
    const result = await linkGuildWebhookSubscription({
      guildId: 'g2', apiKey: 'lv_guild_2',
    });
    expect(result).toEqual({ ok: false, reason: LINK_RESULTS.OWNER_MISSING });
    expect(mockDeleteSubscription).toHaveBeenCalledWith(expect.objectContaining({
      webhookId: 'wh_no_owner',
    }));
    expect(mockAudit).toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTER_FAILED,
      expect.objectContaining({ reason: LINK_RESULTS.OWNER_MISSING }),
    );
  });

  it('emits REGISTER_FAILED audit when ensureWebhookSubscription throws', async () => {
    mockEnsureWebhookSubscription.mockRejectedValueOnce(new Error('qurl-service 502'));
    const result = await linkGuildWebhookSubscription({
      guildId: 'g3', apiKey: 'lv_guild_3',
    });
    expect(result).toEqual({ ok: false, reason: LINK_RESULTS.REGISTER_FAILED });
    // No rollback DELETE: nothing was created.
    expect(mockDeleteSubscription).not.toHaveBeenCalled();
    expect(mockAudit).toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTER_FAILED,
      expect.objectContaining({ reason: LINK_RESULTS.REGISTER_FAILED }),
    );
  });

  it('keeps a different owner on the per-guild path and publishes the result', async () => {
    const result = await linkGuildWebhookSubscription({
      guildId: 'g_happy', apiKey: 'lv_guild_happy',
    });
    expect(result).toEqual({ ok: true, action: 'created' });
    expect(mockResolveDefaultOwnerForApiKey).toHaveBeenCalledWith('lv_guild_happy');
    expect(mockEnsureWebhookSubscription).toHaveBeenCalledTimes(1);
    expect(mockEnsureWebhookSubscription).toHaveBeenCalledWith(
      expect.objectContaining({ apiKey: 'lv_guild_happy' }),
    );
    expect(mockSetGuildWebhookSubscription).toHaveBeenCalledWith('g_happy', {
      webhookId: 'wh_ok', webhookSecret: 'sec_ok', webhookOwnerId: 'usr_ok',
    });
    expect(mockSetGuildDefaultWebhookOwner).not.toHaveBeenCalled();
    expect(mockUpsertGuild).toHaveBeenCalledWith({
      guildId: 'g_happy', ownerId: 'usr_ok', webhookId: 'wh_ok', webhookSecret: 'sec_ok',
    });
    expect(mockEnsureDefaultOwnerCacheEntry).not.toHaveBeenCalled();
    expect(mockAudit).toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTERED,
      expect.objectContaining({ guild_id: 'g_happy', action: 'created' }),
    );
  });
});

describe('linkGuildWebhookSubscription — default-owner failures', () => {
  it('fails closed when owner resolution throws', async () => {
    const resolutionError = Object.assign(new Error('qurl-service 502'), {
      code: 'DEFAULT_WEBHOOK_OWNER_UNDISCOVERED',
    });
    mockResolveDefaultOwnerForApiKey.mockRejectedValueOnce(resolutionError);

    const result = await linkGuildWebhookSubscription({ guildId: 'g_resolve', apiKey: 'lv_x' });

    expect(result).toEqual({ ok: false, reason: LINK_RESULTS.REGISTER_FAILED });
    expect(mockEnsureWebhookSubscription).not.toHaveBeenCalled();
    expect(mockSetGuildDefaultWebhookOwner).not.toHaveBeenCalled();
    expect(mockSetGuildWebhookSubscription).not.toHaveBeenCalled();
    expect(mockEnsureDefaultOwnerCacheEntry).not.toHaveBeenCalled();
    expect(mockUpsertGuild).not.toHaveBeenCalled();
    expect(mockAudit).toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTER_FAILED,
      expect.objectContaining({
        guild_id: 'g_resolve',
        reason: LINK_RESULTS.REGISTER_FAILED,
        stage: 'owner-resolution',
        error_code: 'DEFAULT_WEBHOOK_OWNER_UNDISCOVERED',
      }),
    );
    expect(mockAudit).not.toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTERED,
      expect.anything(),
    );
    expect(mockWarn).toHaveBeenCalledWith(
      'Per-guild webhook owner resolution failed',
      { error: 'qurl-service 502', guildId: 'g_resolve' },
    );
  });

  it('reports persistence failure without applying the guild cache association', async () => {
    mockResolveDefaultOwnerForApiKey.mockResolvedValueOnce('usr_default');
    const persistenceError = Object.assign(new Error('default secret conflict'), {
      code: 'DEFAULT_WEBHOOK_SECRET_CONFLICT',
    });
    mockSetGuildDefaultWebhookOwner.mockRejectedValueOnce(persistenceError);

    const result = await linkGuildWebhookSubscription({ guildId: 'g_persist', apiKey: 'lv_x' });

    expect(result).toEqual({ ok: false, reason: LINK_RESULTS.PERSIST_FAILED });
    expect(mockEnsureWebhookSubscription).not.toHaveBeenCalled();
    expect(mockEnsureDefaultOwnerCacheEntry).not.toHaveBeenCalled();
    expect(mockSetGuildDefaultWebhookOwner).toHaveBeenCalledWith(
      'g_persist', {
        webhookOwnerId: 'usr_default',
        expectedDefaultWebhookSecret: 'wsec_test',
        expectedApiKey: 'lv_x',
      },
    );
    expect(mockAudit).toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTER_FAILED,
      expect.objectContaining({
        guild_id: 'g_persist',
        reason: LINK_RESULTS.PERSIST_FAILED,
        stage: 'default-owner-persist',
        error_code: 'DEFAULT_WEBHOOK_SECRET_CONFLICT',
      }),
    );
    expect(mockAudit).not.toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTERED,
      expect.anything(),
    );
    expect(mockWarn).toHaveBeenCalledWith(
      'Default webhook owner mapping persist failed',
      { error: 'default secret conflict', guildId: 'g_persist' },
    );
  });

  it('keeps a successful owner-only write when the local cache update rejects', async () => {
    mockResolveDefaultOwnerForApiKey.mockResolvedValueOnce('usr_default');
    mockEnsureDefaultOwnerCacheEntry.mockImplementationOnce(() => { throw new Error('cache rejected'); });

    const result = await linkGuildWebhookSubscription({ guildId: 'g_cache', apiKey: 'lv_x' });

    expect(result).toEqual({ ok: true, action: 'reused' });
    expect(mockWarn).toHaveBeenCalledWith(
      'subs.ensureDefaultOwnerCacheEntry rejected (existing cache retained; registry scan remains authoritative)',
      expect.objectContaining({ guildId: 'g_cache', error: 'cache rejected' }),
    );
    expect(mockAudit).toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTERED,
      { guild_id: 'g_cache', action: 'reused', default_owner: true },
    );
    expect(mockAudit).not.toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTER_FAILED,
      expect.anything(),
    );
  });
});

describe('linkGuildWebhookSubscription — URL-migration sweep kill-switch (#827)', () => {
  // Bot HTTP fleet is exactly the active-active multi-region topology
  // that would cannibalize healthy peers if the sweep ran on every
  // replica. The Lambda wrapper reads QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP;
  // the bot path MUST honor the same env-var so a single flip
  // disables the sweep everywhere.
  it('passes urlMigrationSweepEnabled=true when the env var is unset (default safe for single-host)', async () => {
    const oldEnv = process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP;
    delete process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP;
    try {
      await linkGuildWebhookSubscription({ guildId: 'g_default', apiKey: 'lv_x' });
      const call = mockEnsureWebhookSubscription.mock.calls[0][0];
      expect(call.urlMigrationSweepEnabled).toBe(true);
    } finally {
      if (oldEnv === undefined) delete process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP;
      else process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP = oldEnv;
    }
  });

  it.each([
    ['1', false],     // disable
    ['true', false],  // disable
    ['TRUE', false],  // case-insensitive
    ['yes', false],   // disable
    ['on', false],    // disable
    [' 1 ', false],   // whitespace tolerated
  ])('passes urlMigrationSweepEnabled=false when env var is %s (kill-switch covers the bot path)', async (envValue, expectedEnabled) => {
    const oldEnv = process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP;
    process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP = envValue;
    try {
      await linkGuildWebhookSubscription({ guildId: 'g_disabled', apiKey: 'lv_x' });
      const call = mockEnsureWebhookSubscription.mock.calls[0][0];
      expect(call.urlMigrationSweepEnabled).toBe(expectedEnabled);
    } finally {
      if (oldEnv === undefined) delete process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP;
      else process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP = oldEnv;
    }
  });

  it.each([
    ['0', true],      // intuitive "disabled=0" actually KEEPS sweep enabled (footgun guard)
    ['false', true],
    ['no', true],
    ['off', true],
    ['', true],
    ['random-string', true], // not in truthy allowlist
  ])('treats env var %s as ENABLED (no surprise from non-truthy literals)', async (envValue, expectedEnabled) => {
    const oldEnv = process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP;
    process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP = envValue;
    try {
      await linkGuildWebhookSubscription({ guildId: 'g_kept', apiKey: 'lv_x' });
      const call = mockEnsureWebhookSubscription.mock.calls[0][0];
      expect(call.urlMigrationSweepEnabled).toBe(expectedEnabled);
    } finally {
      if (oldEnv === undefined) delete process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP;
      else process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP = oldEnv;
    }
  });

  it('interpolates the shared DISCORD_BOT_VIEW_COUNTER_DESCRIPTION_PREFIX into the description (drift safety)', async () => {
    await linkGuildWebhookSubscription({ guildId: 'g_desc', apiKey: 'lv_x', descriptionContext: 'via=test' });
    const call = mockEnsureWebhookSubscription.mock.calls[0][0];
    expect(call.description).toBe('Discord bot view counter (guild=g_desc, via=test)');
  });
});

describe('linkGuildWebhookSubscription — propagation parameter', () => {
  it('passes the just-linked guildId to propagate so primary is skipped', async () => {
    await linkGuildWebhookSubscription({
      guildId: 'g_primary', apiKey: 'lv_test',
    });
    expect(mockPropagateGuildWebhookSubscription).toHaveBeenCalledWith(
      'usr_ok',
      { webhookId: 'wh_ok', webhookSecret: 'sec_ok', excludeGuildId: 'g_primary' },
    );
  });

  // Partial propagate failure (e.g., one sibling throttled) MUST fire
  // an audit. Without this, the sibling cache entry holds the stale
  // secret for up to 30s and 401s every webhook silently.
  it('emits PROPAGATE_PARTIAL audit (NOT REGISTER_FAILED) when propagate.failed > 0', async () => {
    mockPropagateGuildWebhookSubscription.mockResolvedValueOnce({ updated: 1, failed: 2, skipped: 3 });
    const result = await linkGuildWebhookSubscription({
      guildId: 'g_partial', apiKey: 'lv_x',
    });
    // Registration itself still succeeded — partial propagate is a
    // secondary signal, not a hard rollback. Distinct event keeps
    // the REGISTER_FAILED dashboard line unambiguously "the link
    // failed for the user."
    expect(result).toEqual({ ok: true, action: 'created' });
    expect(mockAudit).toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_PROPAGATE_PARTIAL,
      expect.objectContaining({
        guild_id: 'g_partial', failed: 2, updated: 1, skipped: 3,
      }),
    );
    // And REGISTER_FAILED must NOT be fired on this path.
    const registerFailedCalls = mockAudit.mock.calls.filter(
      ([event]) => event === AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTER_FAILED,
    );
    expect(registerFailedCalls).toHaveLength(0);
  });

  it('logs CAS-skipped siblings without emitting a partial-failure audit', async () => {
    mockPropagateGuildWebhookSubscription.mockResolvedValueOnce({ updated: 0, failed: 0, skipped: 1 });

    const result = await linkGuildWebhookSubscription({ guildId: 'g_raced', apiKey: 'lv_x' });

    expect(result).toEqual({ ok: true, action: 'created' });
    expect(mockInfo).toHaveBeenCalledWith(
      'Per-guild webhook secret propagation skipped concurrently changed siblings',
      expect.objectContaining({ guildId: 'g_raced', skipped: 1 }),
    );
    expect(mockAudit).not.toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_PROPAGATE_PARTIAL,
      expect.anything(),
    );
    expect(mockAudit).not.toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTER_FAILED,
      expect.anything(),
    );
  });
});

describe('linkGuildWebhookSubscription — bestEffortDeleteSubscription failure', () => {
  // When DDB write throws AFTER subscription creation, the rollback
  // DELETE fires fire-and-forget. If qurl-service rejects the DELETE
  // (e.g., 500 — not 401/404 which deleteSubscription itself swallows),
  // the helper must still emit DELETE_FAILED audit so an orphan-
  // subscription metric filter catches it. Without this test, the
  // .catch branch was unexercised.
  it('emits SUBSCRIPTION_DELETE_FAILED audit when rollback DELETE rejects', async () => {
    mockSetGuildWebhookSubscription.mockRejectedValueOnce(new Error('DDB throttled'));
    const dErr = new Error('qurl-service 500');
    dErr.status = 500;
    mockDeleteSubscription.mockRejectedValueOnce(dErr);
    await linkGuildWebhookSubscription({
      guildId: 'g_rollback_fail', apiKey: 'lv_x',
    });
    // Fire-and-forget — wait a microtask for the .catch handler.
    await new Promise(setImmediate);
    expect(mockAudit).toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_DELETE_FAILED,
      expect.objectContaining({ guild_id: 'g_rollback_fail', path: 'rollback' }),
    );
  });

  // 404 is swallowed inside the registrar (concurrent-delete race).
  // The .catch in bestEffortDeleteSubscription never fires → no
  // audit. Pins the contract so future refactor doesn't widen the
  // alarm-noise surface.
  it('does NOT emit DELETE_FAILED audit when DELETE 404s (registrar swallows)', async () => {
    mockSetGuildWebhookSubscription.mockRejectedValueOnce(new Error('DDB throttled'));
    // Simulate registrar's swallow: deleteSubscription resolves silently.
    mockDeleteSubscription.mockResolvedValueOnce(undefined);
    await linkGuildWebhookSubscription({
      guildId: 'g_404', apiKey: 'lv_x',
    });
    await new Promise(setImmediate);
    expect(mockAudit).not.toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_DELETE_FAILED,
      expect.any(Object),
    );
  });

  // 401 is the routine re-key signal (admin revoked the key on
  // layerv.ai before our DELETE landed). Log only — auditing every
  // re-key would flood the alarm channel.
  it('does NOT emit DELETE_FAILED audit when DELETE 401s (routine re-key)', async () => {
    mockSetGuildWebhookSubscription.mockRejectedValueOnce(new Error('DDB throttled'));
    const dErr = new Error('qurl-service 401');
    dErr.status = 401;
    mockDeleteSubscription.mockRejectedValueOnce(dErr);
    await linkGuildWebhookSubscription({
      guildId: 'g_401', apiKey: 'lv_revoked',
    });
    await new Promise(setImmediate);
    expect(mockAudit).not.toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_DELETE_FAILED,
      expect.any(Object),
    );
  });
});
