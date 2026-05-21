// Tests for the link orchestrator (db + registrar wiring + partial-
// failure rollback). Cache-side scenarios live in
// tests/webhook-subscriptions.test.js.

const mockEnsureWebhookSubscription = jest.fn();
const mockDeleteSubscription = jest.fn();
jest.mock('../src/qurl-webhook-registrar', () => ({
  ensureWebhookSubscription: mockEnsureWebhookSubscription,
  deleteSubscription: mockDeleteSubscription,
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
});
