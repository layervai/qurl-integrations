// Regression coverage for the default-owner guild-link path. Keep the
// registrar + subscription registry real so this test observes the qURL HTTP
// method and the secret the receiver will actually use; DDB, fetch, and
// logging are replaced at their external boundaries.

const mockScanGuildSubscriptions = jest.fn();
const mockSetGuildDefaultWebhookOwner = jest.fn();
const mockSetGuildWebhookSubscription = jest.fn();
const mockPropagateGuildWebhookSubscription = jest.fn();

jest.mock('../src/store', () => ({
  scanGuildSubscriptions: mockScanGuildSubscriptions,
  getConnectorWebhookBinding: jest.fn(),
  getGuildConfigWithApiKey: jest.fn(),
  setDefaultGuildOwner: jest.fn().mockResolvedValue(undefined),
  setGuildDefaultWebhookOwner: mockSetGuildDefaultWebhookOwner,
  setGuildWebhookSubscription: mockSetGuildWebhookSubscription,
  propagateGuildWebhookSubscription: mockPropagateGuildWebhookSubscription,
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

process.env.QURL_API_KEY = 'lv_default';
process.env.QURL_ENDPOINT = 'https://qurl.example';
process.env.QURL_WEBHOOK_SECRET = 'whsec_default';
process.env.BASE_URL = 'https://discord.example';
process.env.AWS_REGION = 'us-east-2';
process.env.DDB_TABLE_PREFIX = 'qurl-bot-discord-test-';

const logger = require('../src/logger');
const db = require('../src/store');
const { AUDIT_EVENTS } = require('../src/constants');
const subscriptions = require('../src/webhook-subscriptions');
const { linkGuildWebhookSubscription } = require('../src/guild-webhook-link');

const realFetch = global.fetch;
const requests = [];

function jsonResponse(data, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    text: async () => JSON.stringify(data),
  };
}

beforeEach(() => {
  subscriptions._resetForTesting();
  jest.clearAllMocks();
  requests.length = 0;
  db.getConnectorWebhookBinding.mockResolvedValue(null);
  mockScanGuildSubscriptions.mockResolvedValue([]);
  mockSetGuildDefaultWebhookOwner.mockResolvedValue();
  mockSetGuildWebhookSubscription.mockResolvedValue();
  mockPropagateGuildWebhookSubscription.mockResolvedValue({ updated: 0, failed: 0, skipped: 0 });

  global.fetch = jest.fn(async (url, opts) => {
    const method = opts?.method || 'GET';
    const pathname = new URL(url).pathname;
    const authorization = opts?.headers?.Authorization;
    requests.push({ method, pathname, authorization });

    if (method === 'GET' && pathname === '/v1/webhooks') {
      if (authorization === 'Bearer lv_other') {
        return jsonResponse({ data: [{
          webhook_id: 'wh_other',
          owner_id: 'usr_other',
          url: 'https://discord.example/webhooks/qurl',
          events: ['qurl.accessed', 'qurl.expired'],
        }] });
      }
      if (!['Bearer lv_default', 'Bearer lv_alias_for_default_owner'].includes(authorization)) {
        return jsonResponse({ data: [] });
      }
      return jsonResponse({ data: [{
        webhook_id: 'wh_default',
        owner_id: 'usr_default',
        url: 'https://discord.example/webhooks/qurl',
        events: ['qurl.accessed', 'qurl.expired'],
      }] });
    }
    if (method === 'POST' && pathname === '/v1/webhooks/wh_default/secret') {
      return jsonResponse({ data: {
        webhook_id: 'wh_default',
        owner_id: 'usr_default',
        secret: 'whsec_rotated',
      } });
    }
    if (method === 'POST'
        && pathname === '/v1/webhooks/wh_other/secret'
        && authorization === 'Bearer lv_other') {
      return jsonResponse({ data: {
        webhook_id: 'wh_other',
        owner_id: 'usr_other',
        secret: 'whsec_other',
      } });
    }
    throw new Error(`Unexpected qURL request: ${method} ${pathname}`);
  });
});

afterAll(() => {
  global.fetch = realFetch;
});

it('reuses the discovered default owner without rotating or shadowing its environment secret', async () => {
  expect(subscriptions.isPrimed()).toBe(false);
  await subscriptions.scanOnce();
  expect(subscriptions.isPrimed()).toBe(true);
  requests.length = 0;

  const result = await linkGuildWebhookSubscription({
    guildId: 'g_default',
    apiKey: 'lv_alias_for_default_owner',
  });

  expect(requests.filter(({ method, pathname }) => (
    method === 'POST' && pathname === '/v1/webhooks/wh_default/secret'
  ))).toHaveLength(0);
  expect(result).toEqual({ ok: true, action: 'reused' });
  expect(mockSetGuildDefaultWebhookOwner).toHaveBeenCalledWith(
    'g_default', {
      webhookOwnerId: 'usr_default',
      expectedDefaultWebhookSecret: 'whsec_default',
      expectedApiKey: 'lv_alias_for_default_owner',
    },
  );
  expect(mockSetGuildDefaultWebhookOwner).toHaveBeenCalledTimes(1);
  expect(mockSetGuildWebhookSubscription).not.toHaveBeenCalled();
  expect(mockPropagateGuildWebhookSubscription).not.toHaveBeenCalled();
  expect(subscriptions.getSecretForOwner('usr_default')).toBe('whsec_default');

  expect(requests.filter(({ method, authorization }) => (
    method === 'GET' && authorization === 'Bearer lv_default'
  ))).toHaveLength(1);
  expect(requests.filter(({ method, authorization }) => (
    method === 'GET' && authorization === 'Bearer lv_alias_for_default_owner'
  ))).toHaveLength(1);

  // The owner-only DDB state is absent from scanGuildSubscriptions by
  // design. A later refresh must therefore rebuild the default entry from
  // QURL_WEBHOOK_SECRET rather than resurrecting a guild-owned shadow.
  await subscriptions.scanOnce();
  expect(subscriptions.getSecretForOwner('usr_default')).toBe('whsec_default');
});

it('keeps a different owner on the existing per-guild rotate path', async () => {
  const result = await linkGuildWebhookSubscription({
    guildId: 'g_other',
    apiKey: 'lv_other',
  });

  expect(result).toEqual({ ok: true, action: 'rotated' });
  expect(requests.filter(({ method, pathname, authorization }) => (
    method === 'POST'
    && pathname === '/v1/webhooks/wh_other/secret'
    && authorization === 'Bearer lv_other'
  ))).toHaveLength(1);
  expect(requests.filter(({ method, pathname }) => (
    method === 'POST' && pathname === '/v1/webhooks/wh_default/secret'
  ))).toHaveLength(0);
  expect(mockSetGuildWebhookSubscription).toHaveBeenCalledWith('g_other', {
    webhookId: 'wh_other',
    webhookSecret: 'whsec_other',
    webhookOwnerId: 'usr_other',
  });
  expect(mockSetGuildDefaultWebhookOwner).not.toHaveBeenCalled();
  expect(mockPropagateGuildWebhookSubscription).toHaveBeenCalledWith('usr_other', {
    webhookId: 'wh_other',
    webhookSecret: 'whsec_other',
    excludeGuildId: 'g_other',
  });
  expect(subscriptions.getSecretForOwner('usr_other')).toBe('whsec_other');
});

it('does not publish the environment secret when owner-only persistence rejects', async () => {
  mockSetGuildDefaultWebhookOwner.mockRejectedValueOnce(new Error('guild was re-keyed'));
  expect(subscriptions.isPrimed()).toBe(false);

  const result = await linkGuildWebhookSubscription({
    guildId: 'g_raced',
    apiKey: 'lv_alias_for_default_owner',
  });

  expect(result).toEqual({ ok: false, reason: 'persist-failed' });
  expect(subscriptions.getSecretForOwner('usr_default')).toBeNull();
  expect(subscriptions.isPrimed()).toBe(false);
  expect(requests.filter(({ method }) => method === 'POST')).toHaveLength(0);
});

it('fails closed when the default subscription delivers to another deployment', async () => {
  const config = require('../src/config');
  const originalBaseUrl = config.BASE_URL;
  config.BASE_URL = 'https://staging-discord.example';
  try {
    const result = await linkGuildWebhookSubscription({
      guildId: 'g_shared_account',
      apiKey: 'lv_alias_for_default_owner',
    });

    expect(result).toEqual({ ok: false, reason: 'register-failed' });
    expect(requests.filter(({ method }) => method === 'POST')).toHaveLength(0);
    expect(mockSetGuildDefaultWebhookOwner).not.toHaveBeenCalled();
    expect(mockSetGuildWebhookSubscription).not.toHaveBeenCalled();
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.QURL_WEBHOOK_SUBSCRIPTION_REGISTER_FAILED,
      expect.objectContaining({
        guild_id: 'g_shared_account',
        stage: 'owner-resolution',
        error_code: 'DEFAULT_WEBHOOK_OWNER_URL_MISMATCH',
      }),
    );
  } finally {
    config.BASE_URL = originalBaseUrl;
  }
});

it('fails closed before the registrar when the default secret is unexpectedly absent', async () => {
  const config = require('../src/config');
  const originalSecret = config.QURL_WEBHOOK_SECRET;
  const originalPureByok = config.QURL_WEBHOOK_PURE_BYOK;
  config.QURL_WEBHOOK_SECRET = undefined;
  config.QURL_WEBHOOK_PURE_BYOK = false;
  try {
    const result = await linkGuildWebhookSubscription({
      guildId: 'g_misconfigured',
      apiKey: 'lv_alias_for_default_owner',
    });

    expect(result).toEqual({ ok: false, reason: 'register-failed' });
    expect(requests).toHaveLength(0);
    expect(mockSetGuildDefaultWebhookOwner).not.toHaveBeenCalled();
    expect(mockSetGuildWebhookSubscription).not.toHaveBeenCalled();
  } finally {
    config.QURL_WEBHOOK_SECRET = originalSecret;
    config.QURL_WEBHOOK_PURE_BYOK = originalPureByok;
  }
});

it('reuses the default owner while unprimed and lets the first scan publish its secret', async () => {
  const result = await linkGuildWebhookSubscription({
    guildId: 'g_unprimed',
    apiKey: 'lv_alias_for_default_owner',
  });

  expect(result).toEqual({ ok: true, action: 'reused' });
  expect(requests.filter(({ method }) => method === 'POST')).toHaveLength(0);
  expect(mockSetGuildDefaultWebhookOwner).toHaveBeenCalledWith(
    'g_unprimed', {
      webhookOwnerId: 'usr_default',
      expectedDefaultWebhookSecret: 'whsec_default',
      expectedApiKey: 'lv_alias_for_default_owner',
    },
  );
  expect(mockSetGuildWebhookSubscription).not.toHaveBeenCalled();
  expect(mockPropagateGuildWebhookSubscription).not.toHaveBeenCalled();
  expect(subscriptions.isPrimed()).toBe(false);
  expect(subscriptions.getSecretForOwner('usr_default')).toBeNull();

  await subscriptions.scanOnce();
  expect(subscriptions.isPrimed()).toBe(true);
  expect(subscriptions.getSecretForOwner('usr_default')).toBe('whsec_default');
});


describe('connector callback survives a different bot default owner', () => {
  const binding = { ownerId: 'usr_other', webhookId: 'wh_other', webhookSecret: 'whsec_connector' };
  beforeEach(() => {
    db.getConnectorWebhookBinding.mockResolvedValue(binding);
    db.getGuildConfigWithApiKey.mockResolvedValue({ qurl_api_key: 'lv_other', qurl_owner_id: 'usr_other' });
    mockScanGuildSubscriptions.mockResolvedValue([{ guildId: '__connector_webhook__', webhookOwnerId: binding.ownerId, webhookId: binding.webhookId, webhookSecret: binding.webhookSecret }]);
  });
  it('keeps both receiver owners without rotating the connector subscription', async () => {
    await subscriptions.scanOnce();
    requests.length = 0;
    expect(await linkGuildWebhookSubscription({ guildId: 'g_connector', apiKey: 'lv_other' }))
      .toEqual({ ok: true, action: 'reused' });
    expect(requests.length).toBeGreaterThan(0);
    expect(requests.every(request => request.method === 'GET')).toBe(true);
    expect(mockSetGuildDefaultWebhookOwner).toHaveBeenCalledWith('g_connector', {
      webhookOwnerId: 'usr_other', expectedDefaultWebhookSecret: 'whsec_connector', expectedApiKey: 'lv_other',
    });
    expect(mockSetGuildWebhookSubscription).not.toHaveBeenCalled();
    expect(subscriptions.getSecretForOwner('usr_other')).toBe('whsec_connector');
    expect(subscriptions.getSecretForOwner('usr_default')).toBe('whsec_default');
  });
  it.each(['missing subscription', 'changed key', 'missing verified owner', 'conflicting secret', 'changed subscription'])(
    'fails closed without webhook mutations: %s', async (failure) => {
      await subscriptions.scanOnce();
      if (failure === 'missing subscription') global.fetch.mockResolvedValue(jsonResponse({ data: [] }));
      if (failure === 'changed key') db.getGuildConfigWithApiKey.mockResolvedValue({ qurl_api_key: 'lv_new', qurl_owner_id: 'usr_other' });
      if (failure === 'missing verified owner') db.getGuildConfigWithApiKey.mockResolvedValue({ qurl_api_key: 'lv_other' });
      if (failure === 'changed subscription') db.getConnectorWebhookBinding.mockResolvedValue({ ...binding, webhookId: 'wh_replaced' });
      if (failure === 'conflicting secret') db.getConnectorWebhookBinding.mockResolvedValue({ ...binding, webhookSecret: 'whsec_wrong' });
      requests.length = 0;
      expect(await linkGuildWebhookSubscription({ guildId: 'g_connector', apiKey: 'lv_other' }))
        .toEqual({ ok: false, reason: 'register-failed' });
      expect(requests.every(request => request.method === 'GET')).toBe(true);
      expect(mockSetGuildDefaultWebhookOwner).not.toHaveBeenCalled();
      expect(mockSetGuildWebhookSubscription).not.toHaveBeenCalled();
      expect(subscriptions.getSecretForOwner('usr_other')).toBe('whsec_connector');
    },
  );
});
