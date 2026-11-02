function setupSendPhases() {
  const db = {
    recordQURLSendBatch: jest.fn().mockResolvedValue(undefined),
    markSendDMDelivered: jest.fn().mockResolvedValue(undefined),
    updateSendDMStatus: jest.fn().mockResolvedValue(undefined),
    saveSendConfig: jest.fn().mockResolvedValue(undefined),
    getQurlViews: jest.fn().mockResolvedValue(new Map()),
  };
  const connector = {
    downloadAndUpload: jest.fn(),
    reUploadBuffer: jest.fn(),
    uploadJsonToConnector: jest.fn().mockResolvedValue({ resource_id: 'resource-1' }),
    mintLinks: jest.fn().mockResolvedValue([
      { qurl_id: 'q_aaaaaaaaaa1', qurl_link: 'https://q.test/1' },
    ]),
    revokeMintedLinks: jest.fn(),
    isAllowedSourceUrl: (url) => typeof url === 'string' && url.startsWith('https://cdn.discordapp.com/'),
  };
  const logger = Object.fromEntries(['info', 'warn', 'error', 'debug', 'audit'].map(k => [k, jest.fn()]));
  const sendDM = jest.fn().mockResolvedValue({ ok: true, channelId: 'dm-1', messageId: 'message-1' });
  jest.doMock('../../src/config', () => ({ QURL_SEND_MAX_RECIPIENTS: 50 }));
  jest.doMock('../../src/store', () => db);
  jest.doMock('../../src/connector', () => connector);
  jest.doMock('../../src/logger', () => logger);
  jest.doMock('../../src/discord', () => ({ sendDM }));
  jest.doMock('../../src/qurl', () => ({}));
  jest.doMock('../../src/places', () => ({}));
  jest.doMock('../../src/flow-state', () => ({}));
  const { _test } = require('../../src/commands');
  const params = {
    apiKey: 'synthetic-key', resourceType: 'maps',
    locationUrl: 'https://www.google.com/maps/search/?api=1&query=test', locationName: 'Test',
    recipients: [{ id: 'recipient-1', username: 'Recipient' }],
    expiresIn: '24h', selfDestructSeconds: null, personalMessage: null, sendNonce: 'nonce-1',
  };
  const interaction = {
    user: { id: 'sender-1', username: 'Sender' }, guildId: 'guild-1', channelId: 'channel-1',
    editReply: jest.fn().mockResolvedValue(undefined),
  };
  const prepared = {
    sendId: 'send-1', connectorResourceId: 'resource-1',
    qurlLinks: [{ recipientId: 'recipient-1', resourceId: 'resource-1', qurlId: 'q_aaaaaaaaaa1', qurlLink: 'https://q.test/1' }],
  };
  return { ..._test, db, connector, logger, sendDM, params, interaction, prepared };
}

module.exports = { setupSendPhases };
