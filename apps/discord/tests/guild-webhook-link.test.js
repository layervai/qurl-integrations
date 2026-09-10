
const mockEnsureWebhookSubscription = jest.fn();
const mockDeleteSubscription = jest.fn();
jest.mock('../src/qurl-webhook-registrar', () => ({
  ensureWebhookSubscription: mockEnsureWebhookSubscription,
  deleteSubscription: mockDeleteSubscription,
  DISCORD_BOT_VIEW_COUNTER_DESCRIPTION_PREFIX: 'Discord bot view counter',
  isTruthyEnvFlag: (v) => {
    if (typeof v !== 'string' || v.length === 0) return false;
    const n = v.trim().toLowerCase();
    return n === '1' || n === 'true' || n === 'yes' || n === 'on';
  },
}));

const mockSetGuildWebhookSubscription = jest.fn();
const mockPropagateGuildWebhookSubscription = jest.fn();
jest.mock('../src/store', () => ({
  setGuildWebhookSubscription: mockSetGuildWebhookSubscription,
  propagateGuildWebhookSubscription: mockPropagateGuildWebhookSubscription,
  healthCheck: jest.fn(),
}));

const mockUpsertGuild = jest.fn();
const mockRemoveGuild = jest.fn();
jest.mock('../src/webhook-subscriptions', () => ({
  upsertGuild: mockUpsertGuild,
  removeGuild: mockRemoveGuild,
  isPrimed: () => true,
  getSecretForOwner: () => null,
  start: jest.fn(),
  stop: jest.fn(),
  scanOnce: jest.fn(),
  _resetForTesting: jest.fn(),
}));

const mockAudit = jest.fn();
jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
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
  mockPropagateGuildWebhookSubscription.mockResolvedValue({ updated: 0, failed: 0 });
  mockDeleteSubscription.mockResolvedValue();
});

describe('linkGuildWebhookSubscription — partial-failure rollback', () => {
  it('fires bestEffortDeleteSubscription when setGuildWebhookSubscription throws', async () => {
    mockSetGuildWebhookSubscription.mockRejectedValueOnce(new Error('DDB throttled'));
    const result = await linkGuildWebhookSubscription({
      guildId: 'g1', apiKey: 'lv_guild_1',
    });
    expect(result).toEqual({ ok: false, reason: LINK_RESULTS.PERSIST_FAILED });
    expect(mockDeleteSubscription).toHaveBeenCalledWith({
      apiEndpoint: 'https://qurl.example', apiKey: 'lv_guild_1', webhookId: 'wh_ok',
    });
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
    expect(mockDeleteSubscription).not.toHaveBeenCalled();
    expect(mockAudit).toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTER_FAILED,
      expect.objectContaining({ reason: LINK_RESULTS.REGISTER_FAILED }),
    );
  });

  it('happy path emits SUBSCRIPTION_REGISTERED audit and upserts the cache', async () => {
    const result = await linkGuildWebhookSubscription({
      guildId: 'g_happy', apiKey: 'lv_guild_happy',
    });
    expect(result).toEqual({ ok: true, action: 'created' });
    expect(mockUpsertGuild).toHaveBeenCalledWith({
      guildId: 'g_happy', ownerId: 'usr_ok', webhookId: 'wh_ok', webhookSecret: 'sec_ok',
    });
    expect(mockAudit).toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTERED,
      expect.objectContaining({ guild_id: 'g_happy', action: 'created' }),
    );
  });
});

describe('linkGuildWebhookSubscription — URL-migration sweep kill-switch (#827)', () => {
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

  it('emits PROPAGATE_PARTIAL audit (NOT REGISTER_FAILED) when propagate.failed > 0', async () => {
    mockPropagateGuildWebhookSubscription.mockResolvedValueOnce({ updated: 1, failed: 2 });
    const result = await linkGuildWebhookSubscription({
      guildId: 'g_partial', apiKey: 'lv_x',
    });
    expect(result).toEqual({ ok: true, action: 'created' });
    expect(mockAudit).toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_PROPAGATE_PARTIAL,
      expect.objectContaining({
        guild_id: 'g_partial', failed: 2, updated: 1,
      }),
    );
    const registerFailedCalls = mockAudit.mock.calls.filter(
      ([event]) => event === AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTER_FAILED,
    );
    expect(registerFailedCalls).toHaveLength(0);
  });
});

describe('linkGuildWebhookSubscription — bestEffortDeleteSubscription failure', () => {
  it('emits SUBSCRIPTION_DELETE_FAILED audit when rollback DELETE rejects', async () => {
    mockSetGuildWebhookSubscription.mockRejectedValueOnce(new Error('DDB throttled'));
    const dErr = new Error('qurl-service 500');
    dErr.status = 500;
    mockDeleteSubscription.mockRejectedValueOnce(dErr);
    await linkGuildWebhookSubscription({
      guildId: 'g_rollback_fail', apiKey: 'lv_x',
    });
    await new Promise(setImmediate);
    expect(mockAudit).toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_DELETE_FAILED,
      expect.objectContaining({ guild_id: 'g_rollback_fail', path: 'rollback' }),
    );
  });

  it('does NOT emit DELETE_FAILED audit when DELETE 404s (registrar swallows)', async () => {
    mockSetGuildWebhookSubscription.mockRejectedValueOnce(new Error('DDB throttled'));
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
