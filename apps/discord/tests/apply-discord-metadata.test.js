const path = require('path');

const metadata = require('../discord-metadata.json');
const {
  assertExpectedApplication,
  botTag,
  dataUri,
  summarize,
  validateMetadata,
} = require('../scripts/apply-discord-metadata');

describe('apply-discord-metadata helpers', () => {
  test('accepts the LayerV-owned Discord application identity', () => {
    expect(() => assertExpectedApplication({
      id: metadata.application.id,
      verify_key: metadata.application.public_key,
    }, metadata)).not.toThrow();
  });

  test('rejects a bot token from the retired personal app before writes', () => {
    expect(() => assertExpectedApplication({
      id: '1495050474414411948',
      verify_key: metadata.application.public_key,
    }, metadata)).toThrow(/belongs to application 1495050474414411948/);
  });

  test('rejects a matching app id with the wrong public key', () => {
    expect(() => assertExpectedApplication({
      id: metadata.application.id,
      verify_key: '0'.repeat(64),
    }, metadata)).toThrow(/has public key/);
  });

  test('validates required metadata identity fields', () => {
    expect(() => validateMetadata(metadata)).not.toThrow();
    expect(() => validateMetadata({
      ...metadata,
      application: { ...metadata.application, id: '' },
    })).toThrow(/application\.id/);
    expect(() => validateMetadata({
      ...metadata,
      application: { ...metadata.application, public_key: 'not-a-public-key' },
    })).toThrow(/application\.public_key/);
  });

  test('renders referenced PNG assets as data URIs', () => {
    const uri = dataUri(metadata.application.icon, path.join(__dirname, '..'));
    expect(uri).toMatch(/^data:image\/png;base64,/);
  });

  test('fails with a guided error when an asset is missing', () => {
    expect(() => dataUri('assets/does-not-exist.png', path.join(__dirname, '..'))).toThrow(/does not exist/);
  });

  test('redacts image data in dry-run summaries', () => {
    expect(summarize({
      avatar: 'data:image/png;base64,abc123',
      nested: { keep: 'qURL' },
    })).toEqual({
      avatar: '<image-data>',
      nested: { keep: 'qURL' },
    });
  });

  test('formats bot tags for old and new Discord usernames', () => {
    expect(botTag({ username: 'qURL', discriminator: '0' })).toBe('qURL');
    expect(botTag({ username: 'qURL', discriminator: '1234' })).toBe('qURL#1234');
  });
});
