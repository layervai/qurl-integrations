/**
 * Residual coverage tests for commands.js — narrow branches that aren't
 * covered by the focused specs (qurl-file-map, send-pipeline-back-half,
 * commands-comprehensive, send-pipeline-helpers).
 *
 * Covers:
 * - handleCommand: double error path (reply + followUp both fail)
 * - bulklink: already-linked-to-another-user, forceLink throw
 * - handleCommand autocomplete early-return
 * - isGoogleMapsURL edge cases (goo.gl path-shape rejection)
 */

// ---------------------------------------------------------------------------
// Mocks — set up BEFORE requiring modules
// ---------------------------------------------------------------------------

jest.mock('../src/config', () => ({
  QURL_API_KEY: 'test-api-key',
  QURL_ENDPOINT: 'https://api.test.local',
  CONNECTOR_URL: 'https://connector.test.local',
  GOOGLE_MAPS_API_KEY: 'test-google-key',
  QURL_SEND_COOLDOWN_MS: 30000,
  QURL_SEND_MAX_RECIPIENTS: 50,
  DATABASE_PATH: ':memory:',
  PENDING_LINK_EXPIRY_MINUTES: 30,
  ADMIN_USER_IDS: ['admin-1'],
  BASE_URL: 'http://localhost:3000',
  GUILD_ID: 'guild-1',
  isMultiTenant: false,
  // OpenNHP commands (/link, /stats, /leaderboard, /bulklink, etc.) are
  // only dispatch-active when config.isOpenNHPActive is true. This
  // suite exercises several of them; set it here.
  ENABLE_OPENNHP_FEATURES: true,
  isOpenNHPActive: true,
  STAR_MILESTONES: [10, 25, 50, 100],
  CONTRIBUTOR_ROLE_NAME: 'Contributor',
  ACTIVE_CONTRIBUTOR_ROLE_NAME: 'Active Contributor',
  CORE_CONTRIBUTOR_ROLE_NAME: 'Core Contributor',
  CHAMPION_ROLE_NAME: 'Champion',
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

// Track EmbedBuilder calls
const embedInstances = [];
const makeEmbed = () => {
  const embed = {
    setColor: jest.fn().mockReturnThis(),
    setTitle: jest.fn().mockReturnThis(),
    setDescription: jest.fn().mockReturnThis(),
    setAuthor: jest.fn().mockReturnThis(),
    addFields: jest.fn().mockReturnThis(),
    setFooter: jest.fn().mockReturnThis(),
    setTimestamp: jest.fn().mockReturnThis(),
    setThumbnail: jest.fn().mockReturnThis(),
    setURL: jest.fn().mockReturnThis(),
    _fields: [],
  };
  embed.addFields.mockImplementation(function (...args) {
    embed._fields.push(...args);
    return embed;
  });
  embedInstances.push(embed);
  return embed;
};

jest.mock('discord.js', () => {
  // Shared option-builder chainable from tests/helpers/discord-mock.
  // Same instance used by commands-comprehensive.test.js so a new
  // chained method (setMaxLength, addChoices, etc.) at the
  // discord.js layer only touches one site.
  const { makeOptionBuilder } = require('./helpers/discord-mock');
  return {
  SlashCommandBuilder: jest.fn().mockImplementation(() => {
    const subBuilder = () => ({
      setName: jest.fn().mockReturnThis(),
      setDescription: jest.fn().mockReturnThis(),
      addStringOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return this; }),
      addUserOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return this; }),
      addAttachmentOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return this; }),
      addIntegerOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return this; }),
    });
    const builder = {
      setName: jest.fn(function (n) { builder.name = n; return builder; }),
      setDescription: jest.fn().mockReturnThis(),
      addSubcommand: jest.fn(function (fn) { if (typeof fn === 'function') fn(subBuilder()); return builder; }),
      addStringOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return builder; }),
      addUserOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return builder; }),
      addAttachmentOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return builder; }),
      addIntegerOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return builder; }),
      setDefaultMemberPermissions: jest.fn().mockReturnThis(),
      toJSON: jest.fn().mockReturnValue({}),
    };
    return builder;
  }),
  EmbedBuilder: jest.fn().mockImplementation(makeEmbed),
  PermissionFlagsBits: { ManageRoles: 1n, Administrator: 8n },
  ActionRowBuilder: jest.fn().mockImplementation(() => {
    const row = { components: [], addComponents: jest.fn(function (...args) {
      row.components.push(...args.flat());
      return row;
    }) };
    return row;
  }),
  ButtonBuilder: jest.fn().mockImplementation(() => ({
    setCustomId: jest.fn().mockReturnThis(),
    setLabel: jest.fn().mockReturnThis(),
    setEmoji: jest.fn().mockReturnThis(),
    setStyle: jest.fn().mockReturnThis(),
    setURL: jest.fn().mockReturnThis(),
  })),
  ButtonStyle: { Primary: 1, Secondary: 2, Success: 3, Danger: 4, Link: 5 },
  ChannelType: { GuildText: 0, GuildVoice: 2, GuildStageVoice: 13 },
  ComponentType: { Button: 2, StringSelect: 3, UserSelect: 5 },
  StringSelectMenuBuilder: jest.fn().mockImplementation(() => ({
    setCustomId: jest.fn().mockReturnThis(),
    setPlaceholder: jest.fn().mockReturnThis(),
    addOptions: jest.fn().mockReturnThis(),
  })),
  UserSelectMenuBuilder: jest.fn().mockImplementation(() => ({
    setCustomId: jest.fn().mockReturnThis(),
    setPlaceholder: jest.fn().mockReturnThis(),
    setMinValues: jest.fn().mockReturnThis(),
    setMaxValues: jest.fn().mockReturnThis(),
  })),
  ModalBuilder: jest.fn().mockImplementation(() => ({
    setCustomId: jest.fn().mockReturnThis(),
    setTitle: jest.fn().mockReturnThis(),
    addComponents: jest.fn().mockReturnThis(),
  })),
  TextInputBuilder: jest.fn().mockImplementation(() => ({
    setCustomId: jest.fn().mockReturnThis(),
    setLabel: jest.fn().mockReturnThis(),
    setPlaceholder: jest.fn().mockReturnThis(),
    setStyle: jest.fn().mockReturnThis(),
    setMaxLength: jest.fn().mockReturnThis(),
    setRequired: jest.fn().mockReturnThis(),
  })),
  TextInputStyle: { Short: 1, Paragraph: 2 },
  };
});

const mockDb = {
  getLinkByDiscord: jest.fn(),
  getLinkedDiscordIds: jest.fn(() => new Set()),
  createPendingLink: jest.fn(),
  getLinkByGithub: jest.fn(),
  deleteLink: jest.fn().mockReturnValue({ changes: 1 }),
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
  markSendRevoked: jest.fn(),
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
jest.mock('../src/database', () => mockDb);

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

jest.mock('../src/utils/admin', () => ({
  requireAdmin: jest.fn(async () => true),
  isAdmin: jest.fn(() => true),
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
const mockDeleteLink = jest.fn();
const mockGetResourceStatus = jest.fn();
jest.mock('../src/qurl', () => ({
  createOneTimeLink: mockCreateOneTimeLink,
  deleteLink: mockDeleteLink,
  getResourceStatus: mockGetResourceStatus,
}));

// Shared places-mock — see tests/helpers/places-mock.js.
const { mockPlacesModule } = require('./helpers/places-mock');
jest.mock('../src/places', () => mockPlacesModule);

// ---------------------------------------------------------------------------
// Require modules under test
// ---------------------------------------------------------------------------

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
const { requireAdmin } = require('../src/utils/admin');

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function makeInteraction(overrides = {}) {
  const base = {
    user: { id: 'user-1', username: 'TestUser' },
    options: {
      getSubcommand: jest.fn(() => 'file'),
      getString: jest.fn(() => null),
      getUser: jest.fn(() => null),
      getAttachment: jest.fn(() => null),
      getInteger: jest.fn(() => null),
      getFocused: jest.fn(() => ({ name: 'location', value: '' })),
    },
    reply: jest.fn().mockResolvedValue({
      awaitMessageComponent: jest.fn().mockRejectedValue(new Error('timeout')),
      createMessageComponentCollector: jest.fn(() => ({ on: jest.fn() })),
    }),
    editReply: jest.fn().mockResolvedValue(undefined),
    deferReply: jest.fn().mockResolvedValue(undefined),
    followUp: jest.fn().mockResolvedValue(undefined),
    channel: {
      awaitMessageComponent: jest.fn().mockRejectedValue(new Error('timeout')),
      members: new Map(),
    },
    channelId: 'ch-1',
    guild: {
      members: { fetch: jest.fn().mockResolvedValue(undefined) },
      voiceStates: { cache: new Map() },
    },
    replied: false,
    deferred: false,
    isChatInputCommand: jest.fn(() => true),
    isAutocomplete: jest.fn(() => false),
    commandName: 'qurl',
    respond: jest.fn().mockResolvedValue(undefined),
  };
  return { ...base, ...overrides };
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

beforeEach(() => {
  jest.clearAllMocks();
  embedInstances.length = 0;
  sendCooldowns.clear();
});

describe('handleCommand — double error (reply fail + followUp fail)', () => {
  it('logs error when error response itself fails', async () => {
    const logger = require('../src/logger');
    const interaction = makeInteraction({
      commandName: 'stats',
      replied: true,
      followUp: jest.fn().mockRejectedValue(new Error('Cannot send')),
    });
    mockDb.getStats.mockImplementationOnce(() => { throw new Error('db crash'); });

    await handleCommand(interaction);
    expect(logger.error).toHaveBeenCalledWith('Failed to send error response', expect.objectContaining({ error: 'Cannot send' }));
  });
});

describe('/bulklink — error paths', () => {
  const findCmd = () => commands.find(c => c.data.name === 'bulklink');

  it('reports already-linked-to-another-user', async () => {
    mockDb.getLinkByGithub.mockReturnValue({ discord_id: 'other-user', github_username: 'ghuser' });
    const interaction = makeInteraction({
      commandName: 'bulklink',
      options: { ...makeInteraction().options, getString: jest.fn(() => '111:ghuser') },
    });
    await findCmd().execute(interaction);
    expect(interaction.reply).toHaveBeenCalled();
  });

  it('reports forceLink throw', async () => {
    mockDb.getLinkByGithub.mockReturnValue(null);
    mockDb.forceLink.mockImplementationOnce(() => { throw new Error('DB write fail'); });
    const interaction = makeInteraction({
      commandName: 'bulklink',
      options: { ...makeInteraction().options, getString: jest.fn(() => '111:ghuser') },
    });
    await findCmd().execute(interaction);
    expect(interaction.reply).toHaveBeenCalled();
  });
});

describe('handleCommand — autocomplete edge cases', () => {
  it('routes autocomplete to the /qurl map location handler (which gates on /qurl + map + location)', async () => {
    // Contract changed in the qurl-map-autocomplete rollout: handleCommand
    // now routes autocomplete interactions to handleAutocomplete instead
    // of dropping them. The handler responds with [] for non-location
    // focused options — see handleAutocomplete in src/commands.js. This
    // smoke pins the routing: with a non-map subcommand the response is
    // still []  (so the picker UI doesn't render stale data), but the
    // respond() call DOES fire.
    const interaction = makeInteraction({
      commandName: 'qurl',
      isAutocomplete: jest.fn(() => true),
      isChatInputCommand: jest.fn(() => false),
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'file'),
        getFocused: jest.fn(() => ({ name: 'location', value: 'query' })),
      },
    });
    await handleCommand(interaction);
    expect(interaction.respond).toHaveBeenCalledWith([]);
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

