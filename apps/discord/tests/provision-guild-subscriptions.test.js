jest.mock('../src/store', () => ({}));
jest.mock('../src/config', () => ({}));
jest.mock('../src/logger', () => ({}));
jest.mock('../src/guild-webhook-link', () => ({}));

const {
  buildGuildScanInput, getProvisioningConfigError,
} = require('../scripts/provision-guild-subscriptions');

describe('provision-guild-subscriptions candidate filtering', () => {
  it('excludes both complete subscriptions and owner-only default mappings in DDB', () => {
    expect(buildGuildScanInput('guild-configs', { guild_id: 'cursor' })).toEqual({
      TableName: 'guild-configs',
      ExclusiveStartKey: { guild_id: 'cursor' },
      FilterExpression: 'attribute_exists(qurl_api_key) AND attribute_not_exists(webhook_id) AND attribute_not_exists(webhook_owner_id)',
    });
  });
});

describe('provision-guild-subscriptions configuration', () => {
  const validConfig = {
    BASE_URL: 'https://discord.example',
    QURL_ENDPOINT: 'https://qurl.example',
    QURL_WEBHOOK_SECRET: 'whsec_default',
    QURL_API_KEY: 'lv_default',
    DDB_TABLE_PREFIX: 'qurl-bot-discord-test-',
  };

  it('fails closed when the default secret is absent without an explicit opt-out', () => {
    expect(getProvisioningConfigError({
      ...validConfig, QURL_WEBHOOK_SECRET: undefined,
    })).toMatch(/QURL_WEBHOOK_SECRET must be set/);
  });

  it('accepts an explicitly pure-BYOK deployment without default credentials', () => {
    expect(getProvisioningConfigError({
      ...validConfig,
      QURL_WEBHOOK_SECRET: undefined,
      QURL_API_KEY: undefined,
      QURL_WEBHOOK_PURE_BYOK: true,
    })).toBeNull();
  });

  it('requires the default API key when the shared secret is configured', () => {
    expect(getProvisioningConfigError({
      ...validConfig, QURL_API_KEY: undefined,
    })).toMatch(/QURL_API_KEY must be set/);
  });
});
