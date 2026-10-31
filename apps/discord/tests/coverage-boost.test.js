

jest.mock('../src/config', () => ({
  QURL_API_KEY: 'test-api-key',
  QURL_ENDPOINT: 'https://api.test.local',
  CONNECTOR_URL: 'https://connector.test.local',
  GOOGLE_MAPS_API_KEY: 'test-google-key',
  QURL_SEND_COOLDOWN_MS: 30000,
  QURL_DETECT_COOLDOWN_MS: 30000,
  QURL_SEND_MAX_RECIPIENTS: 50,
  BASE_URL: 'http://localhost:3000',
  GUILD_ID: 'guild-1',
  isMultiTenant: false,
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

const embedInstances = [];
jest.mock('discord.js', () => require('./helpers/discord-mock').buildDiscordMock(embedInstances));

const mockDb = {
  getLinkByDiscord: jest.fn(),
  getLinkedDiscordIds: jest.fn(() => new Set()),
  createPendingLink: jest.fn(),
  getLinkByGithub: jest.fn(),
  getContributions: jest.fn(() => []),
  getBadges: jest.fn(() => []),
  getStreak: jest.fn(() => null),
  getStats: jest.fn(() => ({
    linkedUsers: 5, totalContributions: 10, uniqueContributors: 3, byRepo: [],
  })),
  getTopContributors: jest.fn(() => []),
  recordQURLSend: jest.fn(),
  recordQURLSendBatch: jest.fn(),
  updateSendDMStatus: jest.fn(),
  getRecentSends: jest.fn(() => []),
  getSendResourceIds: jest.fn(() => []),
  markSendRevoked: jest.fn().mockResolvedValue(true),
  getSendConfig: jest.fn(),
  saveSendConfig: jest.fn(),
  forceLink: jest.fn(),
  hasMilestoneBeenAnnounced: jest.fn(() => false),
  recordMilestone: jest.fn(() => true),
  getContributionCount: jest.fn(() => 0),
  BADGE_INFO: {
    first_pr: { emoji: 'e', name: 'First PR', description: 'desc' },
  },
};
jest.mock('../src/store', () => mockDb);

const mockSendDM = jest.fn().mockResolvedValue(true);
jest.mock('../src/discord', () => ({
  assignContributorRole: jest.fn(),
  notifyPRMerge: jest.fn(),
  notifyBadgeEarned: jest.fn(),
  postGoodFirstIssue: jest.fn(),
  postReleaseAnnouncement: jest.fn(),
  postStarMilestone: jest.fn(),
  postToGitHubFeed: jest.fn(),
  sendDM: mockSendDM,
}));

const mockUploadToConnector = jest.fn();
const mockDownloadAndUpload = jest.fn();
const mockReUploadBuffer = jest.fn();
const mockMintLinks = jest.fn();
const mockUploadJsonToConnector = jest.fn();
jest.mock('../src/connector', () => ({
  downloadAndUpload: mockDownloadAndUpload,
  reUploadBuffer: mockReUploadBuffer,
  mintLinks: mockMintLinks,
  uploadJsonToConnector: mockUploadJsonToConnector,
  isAllowedSourceUrl: (url) => typeof url === 'string' && url.startsWith('https://cdn.discordapp.com'),
}));

const mockCreateOneTimeLink = jest.fn();
const mockRevokeResource = jest.fn();
const mockGetResourceStatus = jest.fn();
jest.mock('../src/qurl', () => ({
  createOneTimeLink: mockCreateOneTimeLink,
  revokeResource: mockRevokeResource,
  getResourceStatus: mockGetResourceStatus,
}));

const { mockPlacesModule } = require('./helpers/places-mock');
jest.mock('../src/places', () => mockPlacesModule);

const crypto = require('crypto');
const originalRandomBytes = crypto.randomBytes;
const MOCK_NONCE = 'deadbeef01234567';
crypto.randomBytes = jest.fn((size) => {
  if (size === 8) return Buffer.from(MOCK_NONCE, 'hex');
  return originalRandomBytes(size);
});
crypto.randomUUID = jest.fn(() => 'mock-uuid-9999');

const { commands, handleCommand, _test } = require('../src/commands');
const { sendCooldowns, setCooldown, isGoogleMapsURL } = _test;

const { makeInteraction } = require('./helpers/discord-mock');

beforeEach(() => {
  jest.clearAllMocks();
  embedInstances.length = 0;
  sendCooldowns.clear();
});

describe('handleCommand — double error (reply fail + followUp fail)', () => {
  it('logs error when error response itself fails', async () => {
    const logger = require('../src/logger');
    const qurlCommand = commands.find(c => c.data.name === 'qurl');
    const realExecute = qurlCommand.execute;
    qurlCommand.execute = jest.fn(() => { throw new Error('db crash'); });
    const interaction = makeInteraction({
      commandName: 'qurl',
      guildId: 'guild-1',
      replied: true,
      followUp: jest.fn().mockRejectedValue(new Error('Cannot send')),
    });

    try {
      await handleCommand(interaction);
    } finally {
      qurlCommand.execute = realExecute;
    }
    expect(logger.error).toHaveBeenCalledWith('Failed to send error response', expect.objectContaining({ error: 'Cannot send' }));
  });
});

describe('handleCommand — autocomplete edge cases', () => {
  it('acknowledges DM autocomplete with an empty choice list, not a command reply', async () => {
    const interaction = makeInteraction({
      commandName: 'qurl',
      guildId: null,
      isAutocomplete: jest.fn(() => true),
      isChatInputCommand: jest.fn(() => false),
    });

    await handleCommand(interaction);

    expect(interaction.respond).toHaveBeenCalledWith([]);
    expect(interaction.reply).not.toHaveBeenCalled();
  });

  it('routes autocomplete to the /qurl map location handler (which gates on /qurl + map + location)', async () => {
    const interaction = makeInteraction({
      commandName: 'qurl',
      guildId: 'guild-1',
      isAutocomplete: jest.fn(() => true),
      isChatInputCommand: jest.fn(() => false),
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getFocused: jest.fn(() => ({ name: 'location', value: 'query' })),
      },
    });
    await handleCommand(interaction);
    expect(interaction.respond).toHaveBeenCalledWith([]);
  });
});

describe('handleCommand — supported guild-install context', () => {
  it('executes /qurl when the guild install authorized the interaction', async () => {
    const qurlCommand = commands.find(c => c.data.name === 'qurl');
    const realExecute = qurlCommand.execute;
    qurlCommand.execute = jest.fn().mockResolvedValue(undefined);
    const interaction = makeInteraction({
      commandName: 'qurl',
      guildId: 'guild-1',
      authorizingIntegrationOwners: { 0: 'guild-1' },
    });

    try {
      await handleCommand(interaction);
      expect(qurlCommand.execute).toHaveBeenCalledWith(interaction);
    } finally {
      qurlCommand.execute = realExecute;
    }
  });
});

describe('isGoogleMapsURL — goo.gl edge cases', () => {
  it('rejects goo.gl link without /maps/ path', () => {
    expect(isGoogleMapsURL('https://goo.gl/abcdef')).toBe(false);
  });
  it('rejects goo.gl link with other path', () => {
    expect(isGoogleMapsURL('https://goo.gl/search/abc')).toBe(false);
  });
});
