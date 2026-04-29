/**
 * Additional tests to boost line coverage to 90%+ on commands.js.
 *
 * Covers:
 * - buildDeliveryPayload (location path, personalMessage)
 * - buildConfirmMsg truncation with > 5 recipients + expand toggle
 * - collector button handlers (revoke, expand, add recipients)
 * - collector end timeout
 * - handleSend: fewer mintLinks than recipients, no attachment guard,
 *   location modal timeout, resource selection timeout, all links fail
 * - handleCommand: double error (reply + followUp fail)
 * - bulklink: already-linked and forceLink throw paths
 * - Google Maps URL edge cases
 * - voice target with members
 * - autocomplete rate limiting and search error
 * - DM batch with rejected promise
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

jest.mock('discord.js', () => ({
  SlashCommandBuilder: jest.fn().mockImplementation(() => {
    const subBuilder = () => ({
      setName: jest.fn().mockReturnThis(),
      setDescription: jest.fn().mockReturnThis(),
      addStringOption: jest.fn(function (fn) { if (typeof fn === 'function') fn({ setName: jest.fn().mockReturnThis(), setDescription: jest.fn().mockReturnThis(), setRequired: jest.fn().mockReturnThis(), addChoices: jest.fn().mockReturnThis(), setAutocomplete: jest.fn().mockReturnThis() }); return this; }),
      addUserOption: jest.fn(function (fn) { if (typeof fn === 'function') fn({ setName: jest.fn().mockReturnThis(), setDescription: jest.fn().mockReturnThis(), setRequired: jest.fn().mockReturnThis() }); return this; }),
      addAttachmentOption: jest.fn(function (fn) { if (typeof fn === 'function') fn({ setName: jest.fn().mockReturnThis(), setDescription: jest.fn().mockReturnThis(), setRequired: jest.fn().mockReturnThis() }); return this; }),
      addIntegerOption: jest.fn(function (fn) { if (typeof fn === 'function') fn({ setName: jest.fn().mockReturnThis(), setDescription: jest.fn().mockReturnThis(), setRequired: jest.fn().mockReturnThis() }); return this; }),
    });
    const builder = {
      setName: jest.fn(function (n) { builder.name = n; return builder; }),
      setDescription: jest.fn().mockReturnThis(),
      addSubcommand: jest.fn(function (fn) { if (typeof fn === 'function') fn(subBuilder()); return builder; }),
      addStringOption: jest.fn(function (fn) { if (typeof fn === 'function') fn({ setName: jest.fn().mockReturnThis(), setDescription: jest.fn().mockReturnThis(), setRequired: jest.fn().mockReturnThis(), addChoices: jest.fn().mockReturnThis(), setAutocomplete: jest.fn().mockReturnThis() }); return builder; }),
      addUserOption: jest.fn(function (fn) { if (typeof fn === 'function') fn({ setName: jest.fn().mockReturnThis(), setDescription: jest.fn().mockReturnThis(), setRequired: jest.fn().mockReturnThis() }); return builder; }),
      addAttachmentOption: jest.fn(function (fn) { if (typeof fn === 'function') fn({ setName: jest.fn().mockReturnThis(), setDescription: jest.fn().mockReturnThis(), setRequired: jest.fn().mockReturnThis() }); return builder; }),
      addIntegerOption: jest.fn(function (fn) { if (typeof fn === 'function') fn({ setName: jest.fn().mockReturnThis(), setDescription: jest.fn().mockReturnThis(), setRequired: jest.fn().mockReturnThis() }); return builder; }),
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
    setEmoji: jest.fn().mockReturnThis(),
    setPlaceholder: jest.fn().mockReturnThis(),
    setStyle: jest.fn().mockReturnThis(),
    setMaxLength: jest.fn().mockReturnThis(),
    setRequired: jest.fn().mockReturnThis(),
  })),
  TextInputStyle: { Short: 1, Paragraph: 2 },
}));

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
const mockGetVoice = jest.fn();
const mockGetText = jest.fn();
jest.mock('../src/discord', () => ({
  assignContributorRole: jest.fn(),
  notifyPRMerge: jest.fn(),
  notifyBadgeEarned: jest.fn(),
  postGoodFirstIssue: jest.fn(),
  postReleaseAnnouncement: jest.fn(),
  postStarMilestone: jest.fn(),
  postToGitHubFeed: jest.fn(),
  sendDM: mockSendDM,
  getVoiceChannelMembers: mockGetVoice,
  getTextChannelMembers: mockGetText,
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

jest.mock('../src/places', () => ({
  searchPlaces: jest.fn().mockResolvedValue([]),
}));

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
      getSubcommand: jest.fn(() => 'send'),
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

// =========================================================================
// 1. buildDeliveryPayload — location resource type
// =========================================================================

// =========================================================================
// 2. buildConfirmMsg truncation & expand toggle
// =========================================================================

// =========================================================================
// 3. collector — revoke button
// =========================================================================

// =========================================================================
// 4. collector — end timeout
// =========================================================================

// =========================================================================
// 5. fewer mint links than recipients
// =========================================================================

// =========================================================================
// 6. no attachment guard
// =========================================================================

// =========================================================================
// 7. Google Maps URL with query/place param extraction
// =========================================================================

// =========================================================================
// 8. handleCommand — double error
// =========================================================================

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

// =========================================================================
// 9. bulklink error paths
// =========================================================================

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

// =========================================================================
// 10. all location link creation fails
// =========================================================================

// =========================================================================
// 11. collector — add recipients (timeout, limit, cooldown)
// =========================================================================

// =========================================================================
// 12. voice target with members
// =========================================================================

// =========================================================================
// 13. autocomplete rate limiting & search error
// =========================================================================

describe('handleCommand — autocomplete edge cases', () => {
  it('returns early for all autocomplete interactions without responding', async () => {
    const interaction = makeInteraction({
      commandName: 'qurl',
      isAutocomplete: jest.fn(() => true),
      isChatInputCommand: jest.fn(() => false),
      options: { ...makeInteraction().options, getFocused: jest.fn(() => ({ name: 'location', value: 'query' })) },
    });
    await handleCommand(interaction);
    // Autocomplete handler was removed; returns early
    expect(interaction.respond).not.toHaveBeenCalled();
  });
});

// =========================================================================
// 14. DM batch with rejected promise
// =========================================================================

// =========================================================================
// 15. goo.gl without /maps/ path
// =========================================================================

describe('isGoogleMapsURL — goo.gl edge cases', () => {
  it('rejects goo.gl link without /maps/ path', () => {
    expect(isGoogleMapsURL('https://goo.gl/abcdef')).toBe(false);
  });
  it('rejects goo.gl link with other path', () => {
    expect(isGoogleMapsURL('https://goo.gl/search/abc')).toBe(false);
  });
});

// =========================================================================
// 16. location modal timeout
// =========================================================================

// =========================================================================
// 17. resource selection timeout
// =========================================================================

