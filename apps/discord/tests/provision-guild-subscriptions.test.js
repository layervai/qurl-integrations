jest.mock('../src/store', () => ({}));
jest.mock('../src/config', () => ({}));
jest.mock('../src/logger', () => ({}));
jest.mock('../src/guild-webhook-link', () => ({}));

const { buildGuildScanInput } = require('../scripts/provision-guild-subscriptions');

describe('provision-guild-subscriptions candidate filtering', () => {
  it('excludes both complete subscriptions and owner-only default mappings in DDB', () => {
    expect(buildGuildScanInput('guild-configs', { guild_id: 'cursor' })).toEqual({
      TableName: 'guild-configs',
      ExclusiveStartKey: { guild_id: 'cursor' },
      FilterExpression: 'attribute_exists(qurl_api_key) AND attribute_not_exists(webhook_id) AND attribute_not_exists(webhook_owner_id)',
    });
  });
});
