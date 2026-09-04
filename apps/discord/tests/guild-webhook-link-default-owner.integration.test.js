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
  mockScanGuildSubscriptions.mockResolvedValue([]);
  mockSetGuildDefaultWebhookOwner.mockResolvedValue();
  mockSetGuildWebhookSubscription.mockResolvedValue();
  mockPropagateGuildWebhookSubscription.mockResolvedValue({ updated: 0, failed: 0 });

  global.fetch = jest.fn(async (url, opts) => {
    const method = opts?.method || 'GET';
    const pathname = new URL(url).pathname;
    const authorization = opts?.headers?.Authorization;
    requests.push({ method, pathname, authorization });

    if (method === 'GET' && pathname === '/v1/webhooks') {
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
    throw new Error(`Unexpected qURL request: ${method} ${pathname}`);
  });
});

afterEach(() => {
  subscriptions._resetForTesting();
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
  ))).toHaveLength(0);
  expect(requests.filter(({ method, authorization }) => (
    method === 'GET' && authorization === 'Bearer lv_alias_for_default_owner'
  ))).toHaveLength(1);

  // The owner-only DDB state is absent from scanGuildSubscriptions by
  // design. A later refresh must therefore rebuild the default entry from
  // QURL_WEBHOOK_SECRET rather than resurrecting a guild-owned shadow.
  await subscriptions.scanOnce();
  expect(subscriptions.getSecretForOwner('usr_default')).toBe('whsec_default');
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
