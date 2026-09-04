jest.mock('../src/store', () => ({}));
jest.mock('../src/config', () => ({}));
jest.mock('../src/logger', () => ({}));
jest.mock('../src/guild-webhook-link', () => ({}));

const {
  buildGuildScanInput,
  isProvisioningCandidate,
} = require('../scripts/provision-guild-subscriptions');

describe('provision-guild-subscriptions candidate filtering', () => {
  it('excludes both complete subscriptions and owner-only default mappings in DDB', () => {
    expect(buildGuildScanInput('guild-configs', { guild_id: 'cursor' })).toEqual({
      TableName: 'guild-configs',
      ExclusiveStartKey: { guild_id: 'cursor' },
      FilterExpression: 'attribute_exists(qurl_api_key) AND attribute_not_exists(webhook_id) AND attribute_not_exists(webhook_owner_id)',
    });
  });

  test.each([
    ['API-key-only row', { qurl_api_key: 'encrypted-key' }, true],
    ['owner-only mapping', { qurl_api_key: 'encrypted-key', webhook_owner_id: 'usr_default' }, false],
    ['owner attribute with an empty value', { qurl_api_key: 'encrypted-key', webhook_owner_id: null }, false],
    ['complete subscription', { qurl_api_key: 'encrypted-key', webhook_id: 'wh_1' }, false],
    ['webhook attribute with an empty value', { qurl_api_key: 'encrypted-key', webhook_id: null }, false],
    ['present but empty API key', { qurl_api_key: null }, true],
    ['unlinked row', {}, false],
  ])('%s is classified correctly', (_case, row, expected) => {
    expect(isProvisioningCandidate(row)).toBe(expected);
  });
});
