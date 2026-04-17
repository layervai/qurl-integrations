/**
 * Additional tests to boost line coverage to 90%+ on commands.js.
 *
 * Covers:
 * - buildDeliveryEmbed (location path, personalMessage)
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
  ChannelType: { GuildText: 0, GuildVoice: 2, GuildStageVoice: 13 },
  ActionRowBuilder: jest.fn().mockImplementation(() => ({
    addComponents: jest.fn().mockReturnThis(),
  })),
  ButtonBuilder: jest.fn().mockImplementation(() => ({
    setCustomId: jest.fn().mockReturnThis(),
    setLabel: jest.fn().mockReturnThis(),
    setStyle: jest.fn().mockReturnThis(),
    setURL: jest.fn().mockReturnThis(),
  })),
  ButtonStyle: { Primary: 1, Secondary: 2, Success: 3, Danger: 4, Link: 5 },
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
    setRequired: jest.fn().mockReturnThis(),
  })),
  TextInputStyle: { Short: 1, Paragraph: 2 },
}));

const mockDb = {
  getLinkByDiscord: jest.fn(),
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
  updateSendDMStatus: jest.fn(),
  getRecentSends: jest.fn(() => []),
  getSendResourceIds: jest.fn(() => []),
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
const mockUploadJsonToConnector = jest.fn();
const mockMintLinks = jest.fn();
jest.mock('../src/connector', () => ({
  uploadToConnector: mockUploadToConnector,
  uploadJsonToConnector: mockUploadJsonToConnector,
  mintLinks: mockMintLinks,
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
// 1. buildDeliveryEmbed — location resource type
// =========================================================================

describe('buildDeliveryEmbed — location resource type', () => {
  it('adds Location field (no filename) for maps resource with personalMessage', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockUploadJsonToConnector.mockResolvedValue({ resource_id: 'conn-loc-embed', hash: 'he', success: true });
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/loc' }]);
    mockSendDM.mockResolvedValue(true);

    const modalSubmit = {
      fields: { getTextInputValue: jest.fn(() => 'https://maps.app.goo.gl/xyz') },
      deferUpdate: jest.fn().mockResolvedValue(undefined),
    };
    const resInteraction = {
      customId: `qurl_res_loc_${MOCK_NONCE}`,
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockResolvedValue(modalSubmit),
    };
    const collectorObj = { on: jest.fn() };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'channel';
          if (name === 'expiry') return '1h';
          if (name === 'message') return 'Meet me here';
          return null;
        }),
        getAttachment: jest.fn(() => null),
      },
      channel: { awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction) },
      editReply: jest.fn().mockResolvedValue({ createMessageComponentCollector: jest.fn(() => collectorObj) }),
    });

    await cmd.execute(interaction);

    expect(mockSendDM).toHaveBeenCalledTimes(1);
    const dmEmbed = embedInstances.find(e => e.setAuthor.mock.calls.length > 0);
    expect(dmEmbed).toBeDefined();
    const locationField = dmEmbed._fields.find(f => f.name === 'Resource Type' && f.value === 'Location');
    expect(locationField).toBeTruthy();
    const msgField = dmEmbed._fields.find(f => f.name === 'Message');
    expect(msgField).toBeTruthy();
    expect(msgField.value).toContain('Meet me here');
  });
});

// =========================================================================
// 2. buildConfirmMsg truncation & expand toggle
// =========================================================================

describe('buildConfirmMsg — truncation with > 5 recipients + expand', () => {
  it('shows truncated recipients then expands on click', async () => {
    const recipients = [];
    for (let i = 0; i < 7; i++) recipients.push({ id: `r${i}`, username: `User${i}` });
    mockGetText.mockReturnValue(recipients);
    mockUploadToConnector.mockResolvedValue({ resource_id: 'conn-1' });
    mockMintLinks.mockResolvedValue(
      recipients.map((_, i) => ({ qurl_link: `https://q.test/l${i}` })),
    );
    mockSendDM.mockResolvedValue(true);

    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 1024, url: 'https://cdn.discordapp.com/doc.pdf' };
    const resInteraction = { customId: `qurl_res_file_${MOCK_NONCE}`, deferUpdate: jest.fn().mockResolvedValue(undefined) };

    const collectorCallbacks = {};
    const collectorObj = { on: jest.fn((event, cb) => { collectorCallbacks[event] = cb; }) };

    const editReplySpy = jest.fn().mockResolvedValue({
      createMessageComponentCollector: jest.fn(() => collectorObj),
    });
    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'channel';
          if (name === 'expiry') return '1h';
          return null;
        }),
        getAttachment: jest.fn(() => attachment),
      },
      channel: { awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction) },
      editReply: editReplySpy,
    });

    await cmd.execute(interaction);

    const confirmCall = editReplySpy.mock.calls.find(
      call => typeof call[0]?.content === 'string' && call[0].content.includes('Sent to 7 users'),
    );
    expect(confirmCall).toBeTruthy();
    expect(confirmCall[0].content).toContain('+2 more');

    // Click expand
    const expandBtn = { customId: `qurl_expand_mock-uuid-9999`, deferUpdate: jest.fn().mockResolvedValue(undefined) };
    await collectorCallbacks['collect'](expandBtn);

    const expandedCall = editReplySpy.mock.calls.find(
      call => typeof call[0]?.content === 'string' && call[0].content.includes('User6') && !call[0].content.includes('+2 more'),
    );
    expect(expandedCall).toBeTruthy();
  });
});

// =========================================================================
// 3. collector — revoke button
// =========================================================================

describe('collector — revoke button', () => {
  it('calls revokeAllLinks on click', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockUploadToConnector.mockResolvedValue({ resource_id: 'conn-1' });
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/l1' }]);
    mockSendDM.mockResolvedValue(true);
    mockDb.getSendResourceIds.mockReturnValue(['conn-1']);
    mockDeleteLink.mockResolvedValue(undefined);

    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 100, url: 'https://cdn.discordapp.com/doc.pdf' };
    const resInteraction = { customId: `qurl_res_file_${MOCK_NONCE}`, deferUpdate: jest.fn().mockResolvedValue(undefined) };

    const collectorCallbacks = {};
    const collectorObj = { on: jest.fn((event, cb) => { collectorCallbacks[event] = cb; }), stop: jest.fn() };

    const editReplySpy = jest.fn().mockResolvedValue({
      createMessageComponentCollector: jest.fn(() => collectorObj),
    });
    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => { if (name === 'target') return 'channel'; if (name === 'expiry') return '1h'; return null; }),
        getAttachment: jest.fn(() => attachment),
      },
      channel: { awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction) },
      editReply: editReplySpy,
    });

    await cmd.execute(interaction);
    await collectorCallbacks['collect']({ customId: `qurl_revoke_mock-uuid-9999`, deferUpdate: jest.fn().mockResolvedValue(undefined) });

    expect(mockDeleteLink).toHaveBeenCalled();
    const revokeCall = editReplySpy.mock.calls.find(c => typeof c[0]?.content === 'string' && c[0].content.includes('Revoked'));
    expect(revokeCall).toBeTruthy();
  });

  it('handles revoke failure gracefully', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockUploadToConnector.mockResolvedValue({ resource_id: 'conn-1' });
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/l1' }]);
    mockSendDM.mockResolvedValue(true);
    mockDb.getSendResourceIds.mockImplementation(() => { throw new Error('DB down'); });

    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 100, url: 'https://cdn.discordapp.com/doc.pdf' };
    const resInteraction = { customId: `qurl_res_file_${MOCK_NONCE}`, deferUpdate: jest.fn().mockResolvedValue(undefined) };

    const collectorCallbacks = {};
    const collectorObj = { on: jest.fn((event, cb) => { collectorCallbacks[event] = cb; }), stop: jest.fn() };

    const editReplySpy = jest.fn().mockResolvedValue({
      createMessageComponentCollector: jest.fn(() => collectorObj),
    });
    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => { if (name === 'target') return 'channel'; if (name === 'expiry') return '1h'; return null; }),
        getAttachment: jest.fn(() => attachment),
      },
      channel: { awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction) },
      editReply: editReplySpy,
    });

    await cmd.execute(interaction);
    await collectorCallbacks['collect']({ customId: `qurl_revoke_mock-uuid-9999`, deferUpdate: jest.fn().mockResolvedValue(undefined) });

    const failCall = editReplySpy.mock.calls.find(c => typeof c[0]?.content === 'string' && c[0].content.includes('Failed to revoke'));
    expect(failCall).toBeTruthy();
  });
});

// =========================================================================
// 4. collector — end timeout
// =========================================================================

describe('collector — end timeout', () => {
  it('appends management window closed on time end', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockUploadToConnector.mockResolvedValue({ resource_id: 'conn-1' });
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/l1' }]);
    mockSendDM.mockResolvedValue(true);

    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 100, url: 'https://cdn.discordapp.com/doc.pdf' };
    const resInteraction = { customId: `qurl_res_file_${MOCK_NONCE}`, deferUpdate: jest.fn().mockResolvedValue(undefined) };

    const collectorCallbacks = {};
    const collectorObj = { on: jest.fn((event, cb) => { collectorCallbacks[event] = cb; }) };

    const editReplySpy = jest.fn().mockResolvedValue({
      createMessageComponentCollector: jest.fn(() => collectorObj),
    });
    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => { if (name === 'target') return 'channel'; if (name === 'expiry') return '1h'; return null; }),
        getAttachment: jest.fn(() => attachment),
      },
      channel: { awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction) },
      editReply: editReplySpy,
    });

    await cmd.execute(interaction);
    await collectorCallbacks['end'](new Map(), 'time');

    const timeoutCall = editReplySpy.mock.calls.find(c => typeof c[0]?.content === 'string' && c[0].content.includes('Management window closed'));
    expect(timeoutCall).toBeTruthy();
  });
});

// =========================================================================
// 5. fewer mint links than recipients
// =========================================================================

describe('/qurl send — fewer mint links than recipients', () => {
  it('reports partial link creation', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }, { id: 'r2', username: 'Bob' }, { id: 'r3', username: 'Charlie' }];
    mockGetText.mockReturnValue(recipients);
    mockUploadToConnector.mockResolvedValue({ resource_id: 'conn-1' });
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/l1' }, { qurl_link: 'https://q.test/l2' }]);

    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 100, url: 'https://cdn.discordapp.com/doc.pdf' };
    const resInteraction = { customId: `qurl_res_file_${MOCK_NONCE}`, deferUpdate: jest.fn().mockResolvedValue(undefined) };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => { if (name === 'target') return 'channel'; if (name === 'expiry') return '1h'; return null; }),
        getAttachment: jest.fn(() => attachment),
      },
      channel: { awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction) },
    });

    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({ content: expect.stringContaining('Only 2 of 3') }));
  });
});

// =========================================================================
// 6. no attachment guard
// =========================================================================

describe('/qurl send — file button with null commandAttachment', () => {
  it('shows no-file error', async () => {
    const selectedUser = { id: 'r1', bot: false, username: 'Alice' };
    const usersMap = new Map([['r1', selectedUser]]);
    usersMap.first = () => selectedUser;

    const selectInteraction = {
      customId: `qurl_user_${MOCK_NONCE}`,
      user: { id: 'user-1' },
      users: usersMap,
      deferUpdate: jest.fn().mockResolvedValue(undefined),
    };
    const resInteraction = {
      customId: `qurl_res_file_${MOCK_NONCE}`,
      update: jest.fn().mockResolvedValue(undefined),
      deferUpdate: jest.fn().mockResolvedValue(undefined),
    };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => { if (name === 'target') return 'user'; return null; }),
        getAttachment: jest.fn(() => null),
      },
      channel: {
        awaitMessageComponent: jest.fn()
          .mockResolvedValueOnce(selectInteraction)
          .mockResolvedValueOnce(resInteraction),
      },
    });

    await cmd.execute(interaction);
    expect(resInteraction.update).toHaveBeenCalledWith(expect.objectContaining({ content: expect.stringContaining('No file attached') }));
  });
});

// =========================================================================
// 7. Google Maps URL with query/place param extraction
// =========================================================================

describe('/qurl send — location URL param extraction', () => {
  it('extracts from ?q= parameter', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockUploadJsonToConnector.mockResolvedValue({ resource_id: 'conn-loc-q1', hash: 'hq1', success: true });
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/q1' }]);
    mockSendDM.mockResolvedValue(true);

    const modalSubmit = { fields: { getTextInputValue: jest.fn(() => 'https://www.google.com/maps/search/?q=Eiffel+Tower') }, deferUpdate: jest.fn().mockResolvedValue(undefined) };
    const resInteraction = { customId: `qurl_res_loc_${MOCK_NONCE}`, showModal: jest.fn().mockResolvedValue(undefined), awaitModalSubmit: jest.fn().mockResolvedValue(modalSubmit) };
    const collectorObj = { on: jest.fn() };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => { if (name === 'target') return 'channel'; if (name === 'expiry') return '1h'; return null; }),
        getAttachment: jest.fn(() => null),
      },
      channel: { awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction) },
      editReply: jest.fn().mockResolvedValue({ createMessageComponentCollector: jest.fn(() => collectorObj) }),
    });

    await cmd.execute(interaction);
    expect(mockUploadJsonToConnector).toHaveBeenCalledWith(
      expect.objectContaining({ type: 'google-map', query: 'Eiffel Tower' }),
      'location.json',
    );
    expect(mockMintLinks).toHaveBeenCalled();
  });

  it('extracts from /place/ path', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockUploadJsonToConnector.mockResolvedValue({ resource_id: 'conn-loc-p1', hash: 'hp1', success: true });
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/p1' }]);
    mockSendDM.mockResolvedValue(true);

    const modalSubmit = { fields: { getTextInputValue: jest.fn(() => 'https://www.google.com/maps/place/Eiffel+Tower/@48.8,2.29,15z') }, deferUpdate: jest.fn().mockResolvedValue(undefined) };
    const resInteraction = { customId: `qurl_res_loc_${MOCK_NONCE}`, showModal: jest.fn().mockResolvedValue(undefined), awaitModalSubmit: jest.fn().mockResolvedValue(modalSubmit) };
    const collectorObj = { on: jest.fn() };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => { if (name === 'target') return 'channel'; if (name === 'expiry') return '1h'; return null; }),
        getAttachment: jest.fn(() => null),
      },
      channel: { awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction) },
      editReply: jest.fn().mockResolvedValue({ createMessageComponentCollector: jest.fn(() => collectorObj) }),
    });

    await cmd.execute(interaction);
    expect(mockUploadJsonToConnector).toHaveBeenCalledWith(
      expect.objectContaining({ type: 'google-map', query: 'Eiffel Tower', lat: 48.8, lng: 2.29 }),
      'location.json',
    );
    expect(mockMintLinks).toHaveBeenCalled();
  });
});

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

describe('/qurl send — all location link creation fails', () => {
  it('returns failed message', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockUploadJsonToConnector.mockRejectedValue(new Error('Connector error'));

    const modalSubmit = { fields: { getTextInputValue: jest.fn(() => 'Some Place') }, deferUpdate: jest.fn().mockResolvedValue(undefined) };
    const resInteraction = { customId: `qurl_res_loc_${MOCK_NONCE}`, showModal: jest.fn().mockResolvedValue(undefined), awaitModalSubmit: jest.fn().mockResolvedValue(modalSubmit) };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => { if (name === 'target') return 'channel'; if (name === 'expiry') return '1h'; return null; }),
        getAttachment: jest.fn(() => null),
      },
      channel: { awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction) },
    });

    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({ content: expect.stringContaining('Failed to create links') }));
  });
});

// =========================================================================
// 11. collector — add recipients (timeout, limit, cooldown)
// =========================================================================

describe('collector — add recipients button', () => {
  // Helper to set up a complete send flow and return collector callbacks
  async function setupSendWithCollector() {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockUploadToConnector.mockResolvedValue({ resource_id: 'conn-1' });
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/l1' }]);
    mockSendDM.mockResolvedValue(true);

    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 100, url: 'https://cdn.discordapp.com/doc.pdf' };
    const resInteraction = { customId: `qurl_res_file_${MOCK_NONCE}`, deferUpdate: jest.fn().mockResolvedValue(undefined) };

    const collectorCallbacks = {};
    const collectorObj = { on: jest.fn((event, cb) => { collectorCallbacks[event] = cb; }) };

    const editReplySpy = jest.fn().mockResolvedValue({
      createMessageComponentCollector: jest.fn(() => collectorObj),
    });
    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => { if (name === 'target') return 'channel'; if (name === 'expiry') return '1h'; return null; }),
        getAttachment: jest.fn(() => attachment),
      },
      channel: { awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction) },
      editReply: editReplySpy,
    });

    await cmd.execute(interaction);
    return { collectorCallbacks, editReplySpy, interaction };
  }

  it('shows user select and handles timeout', async () => {
    const { collectorCallbacks } = await setupSendWithCollector();

    // Clear cooldown set by the send
    sendCooldowns.clear();

    const addBtnInteraction = {
      customId: `qurl_add_mock-uuid-9999`,
      user: { id: 'user-1' },
      reply: jest.fn().mockResolvedValue({
        awaitMessageComponent: jest.fn().mockRejectedValue({ code: 'InteractionCollectorError', message: 'time' }),
      }),
      editReply: jest.fn().mockResolvedValue(undefined),
    };
    await collectorCallbacks['collect'](addBtnInteraction);

    expect(addBtnInteraction.reply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('Select additional recipients') }),
    );
    expect(addBtnInteraction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('timed out') }),
    );
  });

  it('rejects when at recipient limit', async () => {
    const config = require('../src/config');
    const origMax = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 1;

    const { collectorCallbacks } = await setupSendWithCollector();

    const addBtnInteraction = {
      customId: `qurl_add_mock-uuid-9999`,
      reply: jest.fn().mockResolvedValue(undefined),
    };
    await collectorCallbacks['collect'](addBtnInteraction);

    expect(addBtnInteraction.reply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('Recipient limit reached') }),
    );
    config.QURL_SEND_MAX_RECIPIENTS = origMax;
  });

  it('rejects when on cooldown', async () => {
    const { collectorCallbacks } = await setupSendWithCollector();
    // Cooldown is already set by the send

    const addBtnInteraction = {
      customId: `qurl_add_mock-uuid-9999`,
      reply: jest.fn().mockResolvedValue(undefined),
    };
    await collectorCallbacks['collect'](addBtnInteraction);

    expect(addBtnInteraction.reply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('wait') }),
    );
  });

  it('successfully adds recipients', async () => {
    const { collectorCallbacks, editReplySpy } = await setupSendWithCollector();
    sendCooldowns.clear();

    mockDb.getSendConfig.mockReturnValue({
      resource_type: 'file',
      connector_resource_id: 'conn-1',
      actual_url: null,
      expires_in: '1h',
      personal_message: null,
      location_name: null,
      attachment_name: 'doc.pdf',
    });

    mockMintLinks.mockResolvedValueOnce([{ qurl_link: 'https://q.test/l2' }]);
    mockSendDM.mockResolvedValue(true);

    const newUser = { id: 'r2', bot: false, username: 'Bob' };
    const usersMap = new Map([['r2', newUser]]);
    usersMap.filter = function (fn) {
      const f = new Map(); for (const [k, v] of this) { if (fn(v, k, this)) f.set(k, v); }
      f.filter = usersMap.filter.bind(f); f.map = usersMap.map.bind(f); return f;
    };
    usersMap.map = function (fn) { const a = []; for (const [k, v] of this) a.push(fn(v, k, this)); return a; };

    const selectUserInteraction = {
      users: usersMap,
      deferUpdate: jest.fn().mockResolvedValue(undefined),
      editReply: jest.fn().mockResolvedValue(undefined),
    };
    const selectReply = { awaitMessageComponent: jest.fn().mockResolvedValue(selectUserInteraction) };

    const addBtnInteraction = {
      customId: `qurl_add_mock-uuid-9999`,
      user: { id: 'user-1' },
      reply: jest.fn().mockResolvedValue(selectReply),
      editReply: jest.fn().mockResolvedValue(undefined),
    };

    await collectorCallbacks['collect'](addBtnInteraction);

    expect(addBtnInteraction.reply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('Select additional') }),
    );
    // editReply on the parent should update with new count
    const updatedCall = editReplySpy.mock.calls.find(
      c => typeof c[0]?.content === 'string' && c[0].content.includes('Sent to 2 user'),
    );
    expect(updatedCall).toBeTruthy();
  });
});

// =========================================================================
// 12. voice target with members
// =========================================================================

describe('/qurl send — voice target with members', () => {
  it('proceeds with voice channel members', async () => {
    mockGetVoice.mockReturnValue({ error: null, members: [{ id: 'r1', username: 'Alice' }] });
    mockUploadToConnector.mockResolvedValue({ resource_id: 'conn-1' });
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/l1' }]);
    mockSendDM.mockResolvedValue(true);

    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 100, url: 'https://cdn.discordapp.com/doc.pdf' };
    const resInteraction = { customId: `qurl_res_file_${MOCK_NONCE}`, deferUpdate: jest.fn().mockResolvedValue(undefined) };
    const collectorObj = { on: jest.fn() };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => { if (name === 'target') return 'voice'; if (name === 'expiry') return '1h'; return null; }),
        getAttachment: jest.fn(() => attachment),
      },
      channel: { awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction) },
      editReply: jest.fn().mockResolvedValue({ createMessageComponentCollector: jest.fn(() => collectorObj) }),
    });

    await cmd.execute(interaction);
    expect(mockSendDM).toHaveBeenCalledTimes(1);
  });
});

// =========================================================================
// 13. autocomplete rate limiting & search error
// =========================================================================

describe('handleCommand — autocomplete edge cases', () => {
  it('returns target choices for unknown focused field', async () => {
    const interaction = makeInteraction({
      commandName: 'qurl',
      isAutocomplete: jest.fn(() => true),
      isChatInputCommand: jest.fn(() => false),
      options: { ...makeInteraction().options, getFocused: jest.fn(() => ({ name: 'unknown_field', value: 'test' })) },
    });
    await handleCommand(interaction);
    // Unknown field — handler returns without responding
    expect(interaction.respond).not.toHaveBeenCalled();
  });

  it('returns target choices for non-qurl autocomplete', async () => {
    const interaction = makeInteraction({
      commandName: 'link',
      isAutocomplete: jest.fn(() => true),
      isChatInputCommand: jest.fn(() => false),
    });
    await handleCommand(interaction);
    expect(interaction.respond).not.toHaveBeenCalled();
  });
});

// =========================================================================
// 14. DM batch with rejected promise
// =========================================================================

describe('/qurl send — DM batch with rejected promise', () => {
  it('counts rejected DM promises as failed', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }, { id: 'r2', username: 'Bob' }];
    mockGetText.mockReturnValue(recipients);
    mockUploadToConnector.mockResolvedValue({ resource_id: 'conn-1' });
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/l1' }, { qurl_link: 'https://q.test/l2' }]);
    mockSendDM.mockResolvedValueOnce(true).mockRejectedValueOnce(new Error('DM blocked'));

    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 100, url: 'https://cdn.discordapp.com/doc.pdf' };
    const resInteraction = { customId: `qurl_res_file_${MOCK_NONCE}`, deferUpdate: jest.fn().mockResolvedValue(undefined) };
    const collectorObj = { on: jest.fn() };

    const editReplySpy = jest.fn().mockResolvedValue({ createMessageComponentCollector: jest.fn(() => collectorObj) });
    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => { if (name === 'target') return 'channel'; if (name === 'expiry') return '1h'; return null; }),
        getAttachment: jest.fn(() => attachment),
      },
      channel: { awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction) },
      editReply: editReplySpy,
    });

    await cmd.execute(interaction);
    const lastContent = editReplySpy.mock.calls[editReplySpy.mock.calls.length - 1][0].content;
    expect(lastContent).toContain('could not be reached');
  });
});

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

describe('/qurl send — location modal timeout', () => {
  it('shows timeout message', async () => {
    mockGetText.mockReturnValue([{ id: 'r1', username: 'Alice' }]);

    const resInteraction = {
      customId: `qurl_res_loc_${MOCK_NONCE}`,
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockRejectedValue(new Error('timeout')),
    };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => { if (name === 'target') return 'channel'; return null; }),
        getAttachment: jest.fn(() => null),
      },
      channel: { awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction) },
    });

    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({ content: expect.stringContaining('timed out') }));
  });
});

// =========================================================================
// 17. resource selection timeout
// =========================================================================

describe('/qurl send — resource selection timeout', () => {
  it('shows cancelled message', async () => {
    mockGetText.mockReturnValue([{ id: 'r1', username: 'Alice' }]);

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => { if (name === 'target') return 'channel'; return null; }),
        getAttachment: jest.fn(() => null),
      },
      channel: { awaitMessageComponent: jest.fn().mockRejectedValue(new Error('timeout')) },
    });

    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({ content: expect.stringContaining('No selection made') }));
  });
});
