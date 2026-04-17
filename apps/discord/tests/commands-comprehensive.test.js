/**
 * Comprehensive tests for src/commands.js — covers buildDeliveryEmbed, handleSend,
 * monitorLinkStatus, buildConfirmMsg, handleRevoke, revokeAllLinks, handleCommand,
 * and all slash command execute() functions.
 */

// ---------------------------------------------------------------------------
// Mock setup — BEFORE requiring any modules
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

// Track EmbedBuilder instances for assertions
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
  // Track addFields calls so we can inspect
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
      addStringOption: jest.fn(function (fn) { if (typeof fn === 'function') fn({ setName: jest.fn().mockReturnThis(), setDescription: jest.fn().mockReturnThis(), setRequired: jest.fn().mockReturnThis(), addChoices: jest.fn().mockReturnThis() }); return this; }),
      addUserOption: jest.fn(function (fn) { if (typeof fn === 'function') fn({ setName: jest.fn().mockReturnThis(), setDescription: jest.fn().mockReturnThis(), setRequired: jest.fn().mockReturnThis() }); return this; }),
      addAttachmentOption: jest.fn(function (fn) { if (typeof fn === 'function') fn({ setName: jest.fn().mockReturnThis(), setDescription: jest.fn().mockReturnThis(), setRequired: jest.fn().mockReturnThis() }); return this; }),
      addIntegerOption: jest.fn(function (fn) { if (typeof fn === 'function') fn({ setName: jest.fn().mockReturnThis(), setDescription: jest.fn().mockReturnThis(), setRequired: jest.fn().mockReturnThis() }); return this; }),
    });
    const builder = {
      setName: jest.fn(function (n) { builder.name = n; return builder; }),
      setDescription: jest.fn().mockReturnThis(),
      addSubcommand: jest.fn(function (fn) { if (typeof fn === 'function') fn(subBuilder()); return builder; }),
      addStringOption: jest.fn(function (fn) { if (typeof fn === 'function') fn({ setName: jest.fn().mockReturnThis(), setDescription: jest.fn().mockReturnThis(), setRequired: jest.fn().mockReturnThis(), addChoices: jest.fn().mockReturnThis() }); return builder; }),
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
  recordQURLSendBatch: jest.fn(),
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
const mockMintLinks = jest.fn();
jest.mock('../src/connector', () => ({
  uploadToConnector: mockUploadToConnector,
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

// Mock crypto.randomBytes to produce predictable nonces
const crypto = require('crypto');
const originalRandomBytes = crypto.randomBytes;
const MOCK_NONCE = 'deadbeef01234567';
crypto.randomBytes = jest.fn((size) => {
  if (size === 8) return Buffer.from(MOCK_NONCE, 'hex');
  return originalRandomBytes(size);
});
// Also mock crypto.randomUUID
const originalRandomUUID = crypto.randomUUID;
crypto.randomUUID = jest.fn(() => 'mock-uuid-1234');

const { commands, handleCommand, registerCommands, _test } = require('../src/commands');
const {
  isGoogleMapsURL, sanitizeFilename, sanitizeMessage,
  isAllowedFileType, isOnCooldown, setCooldown, batchSettled, expiryToISO,
  sendCooldowns, handleAddRecipients,
} = _test;

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
      createMessageComponentCollector: jest.fn(() => ({
        on: jest.fn(),
      })),
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

describe('commands module exports', () => {
  it('exports commands array', () => {
    expect(Array.isArray(commands)).toBe(true);
    expect(commands.length).toBeGreaterThan(0);
  });

  it('exports handleCommand function', () => {
    expect(typeof handleCommand).toBe('function');
  });

  it('exports registerCommands function', () => {
    expect(typeof registerCommands).toBe('function');
  });
});

describe('registerCommands', () => {
  it('calls client.application.commands.set', async () => {
    const client = {
      application: {
        commands: {
          set: jest.fn().mockResolvedValue([]),
        },
      },
    };
    await registerCommands(client);
    expect(client.application.commands.set).toHaveBeenCalled();
  });

  it('logs error when set() fails', async () => {
    const logger = require('../src/logger');
    const client = {
      application: {
        commands: {
          set: jest.fn().mockRejectedValue(new Error('fail')),
        },
      },
    };
    await registerCommands(client);
    expect(logger.error).toHaveBeenCalledWith(
      'Failed to register commands',
      expect.objectContaining({ error: 'fail' }),
    );
  });
});

describe('handleCommand', () => {
  it('ignores non-chat-input commands', async () => {
    const interaction = makeInteraction({
      isChatInputCommand: jest.fn(() => false),
      isAutocomplete: jest.fn(() => false),
    });
    await handleCommand(interaction);
    expect(interaction.reply).not.toHaveBeenCalled();
  });

  it('ignores unknown command names', async () => {
    const interaction = makeInteraction({
      commandName: 'nonexistent-cmd',
    });
    await handleCommand(interaction);
    expect(interaction.reply).not.toHaveBeenCalled();
  });

  it('handles errors gracefully when command throws and not deferred', async () => {
    // Find the stats command and make it throw
    const interaction = makeInteraction({ commandName: 'stats' });
    mockDb.getStats.mockImplementationOnce(() => { throw new Error('db crash'); });

    await handleCommand(interaction);

    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('error'), ephemeral: true }),
    );
  });

  it('uses followUp when reply already sent', async () => {
    const interaction = makeInteraction({
      commandName: 'stats',
      replied: true,
    });
    mockDb.getStats.mockImplementationOnce(() => { throw new Error('db crash'); });

    await handleCommand(interaction);

    expect(interaction.followUp).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('error') }),
    );
  });

  it('uses followUp when deferred', async () => {
    const interaction = makeInteraction({
      commandName: 'stats',
      deferred: true,
    });
    mockDb.getStats.mockImplementationOnce(() => { throw new Error('db crash'); });

    await handleCommand(interaction);

    expect(interaction.followUp).toHaveBeenCalled();
  });

  it('handles reply failure in error handler', async () => {
    const interaction = makeInteraction({
      commandName: 'stats',
      reply: jest.fn().mockRejectedValue(new Error('cannot reply')),
    });
    mockDb.getStats.mockImplementationOnce(() => { throw new Error('db crash'); });

    await handleCommand(interaction);
    // Should not throw, just log
    const logger = require('../src/logger');
    expect(logger.error).toHaveBeenCalled();
  });
});

describe('/link command', () => {
  it('replies with OAuth link embed when not linked', async () => {
    mockDb.getLinkByDiscord.mockReturnValue(null);
    const cmd = commands.find(c => c.data.name === 'link');
    const interaction = makeInteraction({ commandName: 'link' });

    await cmd.execute(interaction);

    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({ ephemeral: true }),
    );
    expect(mockDb.createPendingLink).toHaveBeenCalled();
  });

  it('replies with re-link embed when already linked', async () => {
    mockDb.getLinkByDiscord.mockReturnValue({ github_username: 'olduser' });
    const cmd = commands.find(c => c.data.name === 'link');
    const interaction = makeInteraction({ commandName: 'link' });

    await cmd.execute(interaction);

    expect(interaction.reply).toHaveBeenCalled();
    expect(mockDb.createPendingLink).toHaveBeenCalled();
  });
});

describe('/unlink command', () => {
  const findCmd = () => commands.find(c => c.data.name === 'unlink');

  it('replies not-linked when no existing link', async () => {
    mockDb.getLinkByDiscord.mockReturnValue(null);
    const interaction = makeInteraction({ commandName: 'unlink' });

    await findCmd().execute(interaction);

    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringContaining("don't have a GitHub"),
        ephemeral: true,
      }),
    );
  });

  it('shows confirmation and unlinks on confirm', async () => {
    mockDb.getLinkByDiscord.mockReturnValue({ github_username: 'testuser' });
    const buttonInteraction = {
      customId: 'unlink_confirm',
      update: jest.fn().mockResolvedValue(undefined),
    };
    const response = {
      awaitMessageComponent: jest.fn().mockResolvedValue(buttonInteraction),
    };
    const interaction = makeInteraction({
      commandName: 'unlink',
      reply: jest.fn().mockResolvedValue(response),
    });

    await findCmd().execute(interaction);

    expect(buttonInteraction.update).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('Unlinked') }),
    );
    expect(mockDb.deleteLink).toHaveBeenCalledWith('user-1');
  });

  it('cancels unlink on cancel button', async () => {
    mockDb.getLinkByDiscord.mockReturnValue({ github_username: 'testuser' });
    const buttonInteraction = {
      customId: 'unlink_cancel',
      update: jest.fn().mockResolvedValue(undefined),
    };
    const response = {
      awaitMessageComponent: jest.fn().mockResolvedValue(buttonInteraction),
    };
    const interaction = makeInteraction({
      commandName: 'unlink',
      reply: jest.fn().mockResolvedValue(response),
    });

    await findCmd().execute(interaction);

    expect(buttonInteraction.update).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('cancelled') }),
    );
  });

  it('handles confirmation timeout', async () => {
    mockDb.getLinkByDiscord.mockReturnValue({ github_username: 'testuser' });
    const response = {
      awaitMessageComponent: jest.fn().mockRejectedValue(new Error('timeout')),
    };
    const interaction = makeInteraction({
      commandName: 'unlink',
      reply: jest.fn().mockResolvedValue(response),
    });

    await findCmd().execute(interaction);

    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('timed out') }),
    );
  });
});

describe('/whois command', () => {
  const findCmd = () => commands.find(c => c.data.name === 'whois');

  it('shows link info when user is linked', async () => {
    mockDb.getLinkByDiscord.mockReturnValue({
      github_username: 'ghuser',
      linked_at: '2025-01-01T00:00:00Z',
    });
    mockDb.getContributions.mockReturnValue([
      { repo: 'OpenNHP/opennhp', pr_number: 1, pr_title: 'Fix stuff' },
    ]);
    mockDb.getBadges.mockReturnValue([{ badge_type: 'first_pr', earned_at: '2025-01-01' }]);
    mockDb.getStreak.mockReturnValue({ current_streak: 2, longest_streak: 3 });

    const interaction = makeInteraction({
      commandName: 'whois',
      options: {
        ...makeInteraction().options,
        getUser: jest.fn(() => null),
      },
    });

    await findCmd().execute(interaction);

    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({ ephemeral: true }),
    );
  });

  it('shows not-linked message for self when not linked', async () => {
    mockDb.getLinkByDiscord.mockReturnValue(null);
    const interaction = makeInteraction({
      commandName: 'whois',
      options: {
        ...makeInteraction().options,
        getUser: jest.fn(() => null),
      },
    });

    await findCmd().execute(interaction);

    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringContaining('/link'),
        ephemeral: true,
      }),
    );
  });

  it('shows not-linked message for another user', async () => {
    mockDb.getLinkByDiscord.mockReturnValue(null);
    const otherUser = { id: 'other-1', username: 'OtherUser' };
    const interaction = makeInteraction({
      commandName: 'whois',
      options: {
        ...makeInteraction().options,
        getUser: jest.fn(() => otherUser),
      },
    });

    await findCmd().execute(interaction);

    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringContaining('OtherUser'),
      }),
    );
  });
});

describe('/contributions command', () => {
  const findCmd = () => commands.find(c => c.data.name === 'contributions');

  it('shows no contributions message when empty', async () => {
    mockDb.getContributions.mockReturnValue([]);
    const interaction = makeInteraction({
      commandName: 'contributions',
      options: {
        ...makeInteraction().options,
        getUser: jest.fn(() => null),
      },
    });

    await findCmd().execute(interaction);

    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('don\'t have any') }),
    );
  });

  it('shows contribution embed when contributions exist', async () => {
    mockDb.getContributions.mockReturnValue([
      { repo: 'OpenNHP/opennhp', pr_number: 1, pr_title: 'Add feature' },
      { repo: 'OpenNHP/opennhp', pr_number: 2, pr_title: 'Fix bug' },
      { repo: 'OpenNHP/StealthDNS', pr_number: 3, pr_title: 'Update docs' },
    ]);
    const interaction = makeInteraction({
      commandName: 'contributions',
      options: {
        ...makeInteraction().options,
        getUser: jest.fn(() => null),
      },
    });

    await findCmd().execute(interaction);

    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({ ephemeral: true }),
    );
  });
});

describe('/stats command', () => {
  it('shows stats embed', async () => {
    mockDb.getStats.mockReturnValue({
      linkedUsers: 5, totalContributions: 10, uniqueContributors: 3,
      byRepo: [{ repo: 'OpenNHP/opennhp', count: 8 }],
    });
    mockDb.getTopContributors.mockReturnValue([
      { discord_id: 'user-1', count: 5 },
      { discord_id: 'user-2', count: 3 },
    ]);

    const cmd = commands.find(c => c.data.name === 'stats');
    const interaction = makeInteraction({ commandName: 'stats' });

    await cmd.execute(interaction);

    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({ ephemeral: true }),
    );
  });
});

describe('/leaderboard command', () => {
  const findCmd = () => commands.find(c => c.data.name === 'leaderboard');

  it('shows no-contributions message when empty', async () => {
    mockDb.getTopContributors.mockReturnValue([]);
    const interaction = makeInteraction({ commandName: 'leaderboard' });

    await findCmd().execute(interaction);

    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('No contributions') }),
    );
  });

  it('shows leaderboard with entries', async () => {
    mockDb.getTopContributors.mockReturnValue([
      { discord_id: 'u1', count: 10 },
      { discord_id: 'u2', count: 8 },
      { discord_id: 'u3', count: 5 },
      { discord_id: 'u4', count: 3 },
    ]);
    const interaction = makeInteraction({ commandName: 'leaderboard' });

    await findCmd().execute(interaction);

    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({ embeds: expect.any(Array) }),
    );
  });
});

describe('/forcelink command', () => {
  const findCmd = () => commands.find(c => c.data.name === 'forcelink');

  it('force-links a user', async () => {
    mockDb.getLinkByGithub.mockReturnValue(null);
    const targetUser = { id: 'target-1' };
    const interaction = makeInteraction({
      commandName: 'forcelink',
      options: {
        ...makeInteraction().options,
        getUser: jest.fn(() => targetUser),
        getString: jest.fn(() => '@ghuser'),
      },
    });

    await findCmd().execute(interaction);

    expect(mockDb.forceLink).toHaveBeenCalledWith('target-1', 'ghuser');
    expect(interaction.reply).toHaveBeenCalled();
  });

  it('rejects when github already linked to another user', async () => {
    mockDb.getLinkByGithub.mockReturnValue({ discord_id: 'other-1', github_username: 'ghuser' });
    const targetUser = { id: 'target-1' };
    const interaction = makeInteraction({
      commandName: 'forcelink',
      options: {
        ...makeInteraction().options,
        getUser: jest.fn(() => targetUser),
        getString: jest.fn(() => 'ghuser'),
      },
    });

    await findCmd().execute(interaction);

    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('already linked') }),
    );
  });

  it('returns early when requireAdmin returns false', async () => {
    requireAdmin.mockResolvedValueOnce(false);
    const interaction = makeInteraction({ commandName: 'forcelink' });

    await findCmd().execute(interaction);

    expect(mockDb.forceLink).not.toHaveBeenCalled();
  });
});

describe('/bulklink command', () => {
  const findCmd = () => commands.find(c => c.data.name === 'bulklink');

  it('bulk links multiple users', async () => {
    mockDb.getLinkByGithub.mockReturnValue(null);
    const interaction = makeInteraction({
      commandName: 'bulklink',
      options: {
        ...makeInteraction().options,
        getString: jest.fn(() => '11111111111111111:userA,22222222222222222:userB'),
      },
    });

    await findCmd().execute(interaction);

    expect(mockDb.forceLink).toHaveBeenCalledTimes(2);
    expect(interaction.reply).toHaveBeenCalled();
  });

  it('handles invalid format pairs', async () => {
    const interaction = makeInteraction({
      commandName: 'bulklink',
      options: {
        ...makeInteraction().options,
        getString: jest.fn(() => 'invalid,also_bad,:'),
      },
    });

    await findCmd().execute(interaction);

    expect(interaction.reply).toHaveBeenCalled();
  });
});

describe('/backfill-milestones command', () => {
  const findCmd = () => commands.find(c => c.data.name === 'backfill-milestones');

  it('backfills milestones for a repo', async () => {
    mockDb.hasMilestoneBeenAnnounced.mockReturnValue(false);
    mockDb.recordMilestone.mockReturnValue(true);
    const interaction = makeInteraction({
      commandName: 'backfill-milestones',
      options: {
        ...makeInteraction().options,
        getString: jest.fn(() => 'OpenNHP/opennhp'),
        getInteger: jest.fn(() => 150),
      },
    });

    await findCmd().execute(interaction);

    expect(interaction.reply).toHaveBeenCalled();
    // Stars >= 10, 25, 50, 100 should be backfilled (4 milestones)
    expect(mockDb.recordMilestone).toHaveBeenCalledTimes(4);
  });

  it('skips already announced milestones', async () => {
    mockDb.hasMilestoneBeenAnnounced.mockReturnValue(true);
    const interaction = makeInteraction({
      commandName: 'backfill-milestones',
      options: {
        ...makeInteraction().options,
        getString: jest.fn(() => 'OpenNHP/opennhp'),
        getInteger: jest.fn(() => 50),
      },
    });

    await findCmd().execute(interaction);

    expect(mockDb.recordMilestone).not.toHaveBeenCalled();
  });
});

describe('/unlinked command', () => {
  const findCmd = () => commands.find(c => c.data.name === 'unlinked');

  it('reports unlinked contributors', async () => {
    const contributorRole = { id: 'role-1', name: 'Contributor' };
    const member1 = {
      id: 'u1',
      user: { tag: 'User1#0001' },
      roles: { cache: { has: jest.fn(() => true) } },
    };
    const members = new Map([['u1', member1]]);
    members.filter = function (fn) {
      const result = new Map();
      for (const [k, v] of this) { if (fn(v, k)) result.set(k, v); }
      return result;
    };

    mockDb.getLinkByDiscord.mockReturnValue(null);

    const interaction = makeInteraction({
      commandName: 'unlinked',
      guild: {
        members: { fetch: jest.fn().mockResolvedValue(members) },
        roles: { cache: { find: jest.fn(() => contributorRole) } },
      },
    });

    await findCmd().execute(interaction);

    expect(interaction.editReply).toHaveBeenCalled();
  });

  it('handles missing contributor role', async () => {
    const interaction = makeInteraction({
      commandName: 'unlinked',
      guild: {
        members: { fetch: jest.fn().mockResolvedValue(new Map()) },
        roles: { cache: { find: jest.fn(() => null) } },
      },
    });

    await findCmd().execute(interaction);

    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('Could not find role') }),
    );
  });

  it('handles all contributors linked', async () => {
    const contributorRole = { id: 'role-1', name: 'Contributor' };
    const member1 = {
      id: 'u1',
      user: { tag: 'User1' },
      roles: { cache: { has: jest.fn(() => true) } },
    };
    const members = new Map([['u1', member1]]);
    members.filter = function (fn) {
      const result = new Map();
      for (const [k, v] of this) { if (fn(v, k)) result.set(k, v); }
      return result;
    };

    mockDb.getLinkByDiscord.mockReturnValue({ github_username: 'ghuser' });

    const interaction = makeInteraction({
      commandName: 'unlinked',
      guild: {
        members: { fetch: jest.fn().mockResolvedValue(members) },
        roles: { cache: { find: jest.fn(() => contributorRole) } },
      },
    });

    await findCmd().execute(interaction);

    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('All contributors') }),
    );
  });

  it('handles error during fetch', async () => {
    const interaction = makeInteraction({
      commandName: 'unlinked',
      guild: {
        members: { fetch: jest.fn().mockRejectedValue(new Error('fetch fail')) },
        roles: { cache: { find: jest.fn(() => ({ id: 'role-1' })) } },
      },
    });

    await findCmd().execute(interaction);

    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('error') }),
    );
  });
});

describe('/qurl help subcommand', () => {
  it('replies with help text', async () => {
    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'help'),
      },
    });

    await cmd.execute(interaction);

    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringContaining('Qurl Bot'),
        ephemeral: true,
      }),
    );
  });
});

describe('/qurl send — cooldown and API key checks', () => {
  it('rejects when on cooldown', async () => {
    setCooldown('user-1');
    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'user';
          return null;
        }),
      },
    });

    await cmd.execute(interaction);

    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('wait'), ephemeral: true }),
    );
  });

  it('rejects when QURL_API_KEY not configured', async () => {
    const config = require('../src/config');
    const origKey = config.QURL_API_KEY;
    config.QURL_API_KEY = '';

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'user';
          return null;
        }),
      },
    });

    await cmd.execute(interaction);

    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('not configured') }),
    );

    config.QURL_API_KEY = origKey;
  });
});

describe('/qurl send — user target flow', () => {
  it('shows user select then cancels on timeout', async () => {
    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'user';
          if (name === 'expiry') return '24h';
          return null;
        }),
        getAttachment: jest.fn(() => null),
      },
      channel: {
        awaitMessageComponent: jest.fn().mockRejectedValue(new Error('timeout')),
      },
    });

    await cmd.execute(interaction);

    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('Select the user') }),
    );
    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('No user selected') }),
    );
  });

  it('rejects bot as target', async () => {
    const botUser = { id: 'bot-1', bot: true, username: 'Bot' };
    const usersMap = new Map([['bot-1', botUser]]);
    usersMap.first = () => botUser;

    const selectInteraction = {
      customId: 'qurl_user_test',
      user: { id: 'user-1' },
      users: usersMap,
      update: jest.fn().mockResolvedValue(undefined),
      deferUpdate: jest.fn().mockResolvedValue(undefined),
    };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'user';
          return null;
        }),
        getAttachment: jest.fn(() => null),
      },
      channel: {
        awaitMessageComponent: jest.fn().mockResolvedValue(selectInteraction),
      },
    });

    await cmd.execute(interaction);

    expect(selectInteraction.update).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('bot') }),
    );
  });

  it('rejects sending to self', async () => {
    const selfUser = { id: 'user-1', bot: false, username: 'Self' };
    const usersMap = new Map([['user-1', selfUser]]);
    usersMap.first = () => selfUser;

    const selectInteraction = {
      customId: 'qurl_user_test',
      user: { id: 'user-1' },
      users: usersMap,
      update: jest.fn().mockResolvedValue(undefined),
      deferUpdate: jest.fn().mockResolvedValue(undefined),
    };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'user';
          return null;
        }),
        getAttachment: jest.fn(() => null),
      },
      channel: {
        awaitMessageComponent: jest.fn().mockResolvedValue(selectInteraction),
      },
    });

    await cmd.execute(interaction);

    expect(selectInteraction.update).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('yourself') }),
    );
  });
});

describe('/qurl send — channel target', () => {
  it('gets text channel members and shows no-members message', async () => {
    mockGetText.mockReturnValue([]);
    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'channel';
          return null;
        }),
        getAttachment: jest.fn(() => null),
      },
    });

    await cmd.execute(interaction);

    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('No other members') }),
    );
  });
});

describe('/qurl send — voice target', () => {
  it('shows not-in-voice error', async () => {
    mockGetVoice.mockReturnValue({ error: 'not_in_voice', members: [] });
    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'voice';
          return null;
        }),
        getAttachment: jest.fn(() => null),
      },
    });

    await cmd.execute(interaction);

    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('voice channel') }),
    );
  });

  it('shows no-members error when voice channel is empty', async () => {
    mockGetVoice.mockReturnValue({ error: null, members: [] });
    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'voice';
          return null;
        }),
        getAttachment: jest.fn(() => null),
      },
    });

    await cmd.execute(interaction);

    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('No other users') }),
    );
  });
});

describe('/qurl send — too many recipients', () => {
  it('rejects when recipients exceed max', async () => {
    const manyRecipients = Array.from({ length: 51 }, (_, i) => ({
      id: `r-${i}`, username: `u${i}`, bot: false,
    }));
    mockGetText.mockReturnValue(manyRecipients);

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'channel';
          return null;
        }),
        getAttachment: jest.fn(() => null),
      },
    });

    await cmd.execute(interaction);

    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('Too many recipients') }),
    );
  });
});

describe('/qurl send — file flow (channel target, full path)', () => {
  it('uploads file, mints links, sends DMs, tracks in DB', async () => {
    const recipients = [
      { id: 'r1', username: 'Alice' },
      { id: 'r2', username: 'Bob' },
    ];
    mockGetText.mockReturnValue(recipients);
    mockUploadToConnector.mockResolvedValue({ resource_id: 'conn-1' });
    mockMintLinks.mockResolvedValue([
      { qurl_link: 'https://q.test/l1' },
      { qurl_link: 'https://q.test/l2' },
    ]);
    mockSendDM.mockResolvedValue(true);

    const attachment = {
      name: 'doc.pdf',
      contentType: 'application/pdf',
      size: 1024,
      url: 'https://cdn.discordapp.com/doc.pdf',
    };

    // The flow: channel target -> deferReply -> resource select -> file button click
    const resInteraction = {
      customId: `qurl_res_file_${MOCK_NONCE}`,
      deferUpdate: jest.fn().mockResolvedValue(undefined),
      update: jest.fn().mockResolvedValue(undefined),
    };

    const collectorObj = { on: jest.fn() };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'channel';
          if (name === 'expiry') return '1h';
          if (name === 'message') return 'Check this out';
          return null;
        }),
        getAttachment: jest.fn(() => attachment),
      },
      channel: {
        awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction),
      },
      editReply: jest.fn().mockResolvedValue({
        createMessageComponentCollector: jest.fn(() => collectorObj),
      }),
    });

    // customId already set to qurl_res_file_${MOCK_NONCE} which matches the nonce

    await cmd.execute(interaction);

    expect(mockUploadToConnector).toHaveBeenCalled();
    expect(mockMintLinks).toHaveBeenCalled();
    expect(mockSendDM).toHaveBeenCalledTimes(2);
    expect(mockDb.recordQURLSendBatch).toHaveBeenCalledTimes(1);
    expect(mockDb.saveSendConfig).toHaveBeenCalled();
  });
});

describe('/qurl send — location flow (channel target)', () => {
  it('creates one-time links for location URL', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockCreateOneTimeLink.mockResolvedValue({
      qurl_link: 'https://q.test/loc1',
      resource_id: 'res-loc-1',
    });
    mockSendDM.mockResolvedValue(true);

    // The flow: channel target -> no file -> location button -> modal -> submit
    const modalSubmit = {
      customId: `qurl_loc_modal_${MOCK_NONCE}`,
      fields: {
        getTextInputValue: jest.fn(() => 'https://maps.app.goo.gl/abc123'),
      },
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
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'channel';
          if (name === 'expiry') return '30m';
          return null;
        }),
        getAttachment: jest.fn(() => null),
      },
      channel: {
        awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction),
      },
      editReply: jest.fn().mockResolvedValue({
        createMessageComponentCollector: jest.fn(() => collectorObj),
      }),
    });

    await cmd.execute(interaction);

    expect(resInteraction.showModal).toHaveBeenCalled();
    expect(mockCreateOneTimeLink).toHaveBeenCalled();
    expect(mockSendDM).toHaveBeenCalledTimes(1);
  });

  it('creates search URL for plain text location', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockCreateOneTimeLink.mockResolvedValue({
      qurl_link: 'https://q.test/loc2',
      resource_id: 'res-loc-2',
    });
    mockSendDM.mockResolvedValue(true);

    const modalSubmit = {
      fields: {
        getTextInputValue: jest.fn(() => 'Eiffel Tower, Paris'),
      },
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
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'channel';
          return null;
        }),
        getAttachment: jest.fn(() => null),
      },
      channel: {
        awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction),
      },
      editReply: jest.fn().mockResolvedValue({
        createMessageComponentCollector: jest.fn(() => collectorObj),
      }),
    });

    await cmd.execute(interaction);

    // Should create a search URL, not a direct maps link
    expect(mockCreateOneTimeLink).toHaveBeenCalledWith(
      expect.stringContaining('google.com/maps/search/'),
      expect.any(String),
      expect.any(String),
    );
  });
});

describe('/qurl send — file validation errors', () => {
  it('rejects disallowed file type', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);

    const attachment = {
      name: 'script.sh',
      contentType: 'application/x-sh',
      size: 100,
      url: 'https://cdn.discordapp.com/script.sh',
    };

    const resInteraction = {
      customId: `qurl_res_file_${MOCK_NONCE}`,
      deferUpdate: jest.fn().mockResolvedValue(undefined),
    };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'channel';
          return null;
        }),
        getAttachment: jest.fn(() => attachment),
      },
      channel: {
        awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction),
      },
    });

    await cmd.execute(interaction);

    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('not allowed') }),
    );
  });

  it('rejects file too large', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);

    const attachment = {
      name: 'huge.pdf',
      contentType: 'application/pdf',
      size: 30 * 1024 * 1024, // 30MB
      url: 'https://cdn.discordapp.com/huge.pdf',
    };

    const resInteraction = {
      customId: `qurl_res_file_${MOCK_NONCE}`,
      deferUpdate: jest.fn().mockResolvedValue(undefined),
    };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'channel';
          return null;
        }),
        getAttachment: jest.fn(() => attachment),
      },
      channel: {
        awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction),
      },
    });

    await cmd.execute(interaction);

    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('too large') }),
    );
  });
});

describe('/qurl send — link creation failure', () => {
  it('handles upload failure gracefully', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockUploadToConnector.mockRejectedValue(new Error('S3 down'));

    const attachment = {
      name: 'doc.pdf',
      contentType: 'application/pdf',
      size: 100,
      url: 'https://cdn.discordapp.com/doc.pdf',
    };

    const resInteraction = {
      customId: `qurl_res_file_${MOCK_NONCE}`,
      deferUpdate: jest.fn().mockResolvedValue(undefined),
    };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'channel';
          return null;
        }),
        getAttachment: jest.fn(() => attachment),
      },
      channel: {
        awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction),
      },
    });

    await cmd.execute(interaction);

    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('Failed to create links') }),
    );
  });
});

describe('/qurl send — DM failures', () => {
  it('reports failed DMs in confirmation', async () => {
    const recipients = [
      { id: 'r1', username: 'Alice' },
      { id: 'r2', username: 'Bob' },
    ];
    mockGetText.mockReturnValue(recipients);
    mockUploadToConnector.mockResolvedValue({ resource_id: 'conn-1' });
    mockMintLinks.mockResolvedValue([
      { qurl_link: 'https://q.test/l1' },
      { qurl_link: 'https://q.test/l2' },
    ]);
    // First DM succeeds, second fails
    mockSendDM
      .mockResolvedValueOnce(true)
      .mockResolvedValueOnce(false);

    const attachment = {
      name: 'doc.pdf',
      contentType: 'application/pdf',
      size: 100,
      url: 'https://cdn.discordapp.com/doc.pdf',
    };

    const resInteraction = {
      customId: `qurl_res_file_${MOCK_NONCE}`,
      deferUpdate: jest.fn().mockResolvedValue(undefined),
    };

    const collectorObj = { on: jest.fn() };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
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
      channel: {
        awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction),
      },
      editReply: jest.fn().mockResolvedValue({
        createMessageComponentCollector: jest.fn(() => collectorObj),
      }),
    });

    await cmd.execute(interaction);

    // Last editReply should mention failures
    const lastCall = interaction.editReply.mock.calls[interaction.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toContain('1 could not be reached');
  });
});

describe('/qurl send — no file attached (null commandAttachment with file button)', () => {
  it('handles no attachment gracefully', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    const selectInteraction = {
      customId: 'qurl_user_test',
      user: { id: 'user-1' },
      users: (() => {
        const m = new Map([['r1', { id: 'r1', bot: false, username: 'Alice' }]]);
        m.first = () => ({ id: 'r1', bot: false, username: 'Alice' });
        return m;
      })(),
      deferUpdate: jest.fn().mockResolvedValue(undefined),
    };

    // After user select, the resource selection happens: file button clicked
    // But commandAttachment was null (no file_optional), so only loc button shows
    // We simulate clicking the loc button but then timing out the modal
    const resInteraction = {
      customId: `qurl_res_loc_${MOCK_NONCE}`,
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockRejectedValue(new Error('timeout')),
    };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'user';
          return null;
        }),
        getAttachment: jest.fn(() => null),
      },
      channel: {
        awaitMessageComponent: jest.fn()
          .mockResolvedValueOnce(selectInteraction)
          .mockResolvedValueOnce(resInteraction),
      },
    });

    await cmd.execute(interaction);

    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('timed out') }),
    );
  });
});

describe('/qurl revoke subcommand', () => {
  it('shows no recent sends message', async () => {
    mockDb.getRecentSends.mockReturnValue([]);
    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'revoke'),
      },
    });

    await cmd.execute(interaction);

    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('No recent sends') }),
    );
  });

  it('shows select menu for recent sends and revokes', async () => {
    mockDb.getRecentSends.mockReturnValue([
      {
        send_id: 'send-1',
        resource_type: 'file',
        target_type: 'user',
        recipient_count: 1,
        delivered_count: 1,
        expires_in: '24h',
        created_at: new Date().toISOString(),
      },
    ]);
    mockDb.getSendResourceIds.mockReturnValue(['res-1']);
    mockDeleteLink.mockResolvedValue(undefined);

    const selectInteraction = {
      values: ['send-1'],
      update: jest.fn().mockResolvedValue(undefined),
    };
    const responseObj = {
      awaitMessageComponent: jest.fn().mockResolvedValue(selectInteraction),
    };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'revoke'),
      },
      editReply: jest.fn().mockResolvedValue(responseObj),
    });

    await cmd.execute(interaction);

    expect(selectInteraction.update).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('Revoked') }),
    );
  });

  it('handles revocation timeout', async () => {
    mockDb.getRecentSends.mockReturnValue([
      {
        send_id: 'send-1',
        resource_type: 'file',
        target_type: 'user',
        recipient_count: 1,
        delivered_count: 1,
        expires_in: '24h',
        created_at: new Date().toISOString(),
      },
    ]);

    const responseObj = {
      awaitMessageComponent: jest.fn().mockRejectedValue(new Error('timeout')),
    };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'revoke'),
      },
      editReply: jest.fn().mockResolvedValue(responseObj),
    });

    await cmd.execute(interaction);

    // Should have called editReply for timeout message
    const lastCall = interaction.editReply.mock.calls[interaction.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toContain('timed out');
  });
});

describe('handleCommand — autocomplete', () => {
  it('returns early for autocomplete interactions', async () => {
    const interaction = makeInteraction({
      commandName: 'qurl',
      isAutocomplete: jest.fn(() => true),
      isChatInputCommand: jest.fn(() => false),
      options: {
        ...makeInteraction().options,
        getFocused: jest.fn(() => ({ name: 'location', value: 'Eif' })),
      },
    });

    await handleCommand(interaction);

    // Autocomplete now returns early without dispatching
    expect(interaction.respond).not.toHaveBeenCalled();
  });
});

describe('revokeAllLinks', () => {
  it('revokes multiple resource IDs', async () => {
    mockDb.getSendResourceIds.mockReturnValue(['res-1', 'res-2', 'res-3']);
    mockDeleteLink.mockResolvedValue(undefined);

    // Access revokeAllLinks indirectly through the revoke flow
    // We test it via the button handler indirectly, but let's also
    // test the direct function if it's exported
    // Since it's not in _test, we test via the revoke subcommand which calls it

    mockDb.getRecentSends.mockReturnValue([
      {
        send_id: 'send-99',
        resource_type: 'file',
        target_type: 'user',
        recipient_count: 3,
        delivered_count: 3,
        expires_in: '24h',
        created_at: new Date().toISOString(),
      },
    ]);

    const selectInteraction = {
      values: ['send-99'],
      update: jest.fn().mockResolvedValue(undefined),
    };
    const responseObj = {
      awaitMessageComponent: jest.fn().mockResolvedValue(selectInteraction),
    };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'revoke'),
      },
      editReply: jest.fn().mockResolvedValue(responseObj),
    });

    await cmd.execute(interaction);

    expect(mockDeleteLink).toHaveBeenCalledTimes(3);
    expect(selectInteraction.update).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('3/3') }),
    );
  });

  it('handles partial revocation failures', async () => {
    mockDb.getSendResourceIds.mockReturnValue(['res-1', 'res-2']);
    mockDeleteLink
      .mockResolvedValueOnce(undefined)
      .mockRejectedValueOnce(new Error('not found'));

    mockDb.getRecentSends.mockReturnValue([
      {
        send_id: 'send-partial',
        resource_type: 'url',
        target_type: 'channel',
        recipient_count: 2,
        delivered_count: 2,
        expires_in: '1h',
        created_at: new Date().toISOString(),
      },
    ]);

    const selectInteraction = {
      values: ['send-partial'],
      update: jest.fn().mockResolvedValue(undefined),
    };
    const responseObj = {
      awaitMessageComponent: jest.fn().mockResolvedValue(selectInteraction),
    };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'revoke'),
      },
      editReply: jest.fn().mockResolvedValue(responseObj),
    });

    await cmd.execute(interaction);

    expect(selectInteraction.update).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('1/2') }),
    );
  });
});

describe('/qurl send — resource selection timeout', () => {
  it('cancels when no resource type is selected', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'channel';
          return null;
        }),
        getAttachment: jest.fn(() => null),
      },
      channel: {
        awaitMessageComponent: jest.fn().mockRejectedValue(new Error('timeout')),
      },
    });

    await cmd.execute(interaction);

    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('cancelled') }),
    );
  });
});

describe('/qurl send — zero created links', () => {
  it('shows failure message when all link creations fail', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockCreateOneTimeLink.mockRejectedValue(new Error('API down'));

    const modalSubmit = {
      fields: { getTextInputValue: jest.fn(() => 'Eiffel Tower') },
      deferUpdate: jest.fn().mockResolvedValue(undefined),
    };
    const resInteraction = {
      customId: `qurl_res_loc_${MOCK_NONCE}`,
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockResolvedValue(modalSubmit),
    };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'channel';
          return null;
        }),
        getAttachment: jest.fn(() => null),
      },
      channel: {
        awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction),
      },
    });

    await cmd.execute(interaction);

    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('Failed to create any links') }),
    );
  });
});

describe('/qurl send — location modal timeout', () => {
  it('cancels when modal submission times out', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);

    const resInteraction = {
      customId: `qurl_res_loc_${MOCK_NONCE}`,
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockRejectedValue(new Error('timeout')),
    };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'channel';
          return null;
        }),
        getAttachment: jest.fn(() => null),
      },
      channel: {
        awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction),
      },
    });

    await cmd.execute(interaction);

    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('timed out') }),
    );
  });
});

describe('/qurl send — Google Maps URL patterns in location value', () => {
  it('detects standard google.com/maps/place URL', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockCreateOneTimeLink.mockResolvedValue({ qurl_link: 'https://q.test/x', resource_id: 'r-1' });
    mockSendDM.mockResolvedValue(true);

    const modalSubmit = {
      fields: {
        getTextInputValue: jest.fn(() => 'https://www.google.com/maps/place/Eiffel+Tower/@48.8,2.29'),
      },
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
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'channel';
          return null;
        }),
        getAttachment: jest.fn(() => null),
      },
      channel: {
        awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction),
      },
      editReply: jest.fn().mockResolvedValue({
        createMessageComponentCollector: jest.fn(() => collectorObj),
      }),
    });

    await cmd.execute(interaction);

    // Should use the detected URL directly
    expect(mockCreateOneTimeLink).toHaveBeenCalledWith(
      expect.stringContaining('google.com/maps/place/Eiffel'),
      expect.any(String),
      expect.any(String),
    );
  });

  it('detects goo.gl short link', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockCreateOneTimeLink.mockResolvedValue({ qurl_link: 'https://q.test/x', resource_id: 'r-1' });
    mockSendDM.mockResolvedValue(true);

    const modalSubmit = {
      fields: {
        getTextInputValue: jest.fn(() => 'Check this: https://goo.gl/maps/abc123 nice place'),
      },
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
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'channel';
          return null;
        }),
        getAttachment: jest.fn(() => null),
      },
      channel: {
        awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction),
      },
      editReply: jest.fn().mockResolvedValue({
        createMessageComponentCollector: jest.fn(() => collectorObj),
      }),
    });

    await cmd.execute(interaction);

    expect(mockCreateOneTimeLink).toHaveBeenCalledWith(
      expect.stringContaining('goo.gl/maps/abc123'),
      expect.any(String),
      expect.any(String),
    );
  });

  it('detects maps.app.goo.gl short link', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockCreateOneTimeLink.mockResolvedValue({ qurl_link: 'https://q.test/x', resource_id: 'r-1' });
    mockSendDM.mockResolvedValue(true);

    const modalSubmit = {
      fields: {
        getTextInputValue: jest.fn(() => 'https://maps.app.goo.gl/xyz789'),
      },
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
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'channel';
          return null;
        }),
        getAttachment: jest.fn(() => null),
      },
      channel: {
        awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction),
      },
      editReply: jest.fn().mockResolvedValue({
        createMessageComponentCollector: jest.fn(() => collectorObj),
      }),
    });

    await cmd.execute(interaction);

    expect(mockCreateOneTimeLink).toHaveBeenCalledWith(
      expect.stringContaining('maps.app.goo.gl'),
      expect.any(String),
      expect.any(String),
    );
  });

  it('detects embed URL', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockCreateOneTimeLink.mockResolvedValue({ qurl_link: 'https://q.test/x', resource_id: 'r-1' });
    mockSendDM.mockResolvedValue(true);

    const modalSubmit = {
      fields: {
        getTextInputValue: jest.fn(() => 'https://www.google.com/maps/embed/v1/place?key=abc&q=Paris'),
      },
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
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'send'),
        getString: jest.fn((name) => {
          if (name === 'target') return 'channel';
          return null;
        }),
        getAttachment: jest.fn(() => null),
      },
      channel: {
        awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction),
      },
      editReply: jest.fn().mockResolvedValue({
        createMessageComponentCollector: jest.fn(() => collectorObj),
      }),
    });

    await cmd.execute(interaction);

    expect(mockCreateOneTimeLink).toHaveBeenCalledWith(
      expect.stringContaining('google.com/maps/embed'),
      expect.any(String),
      expect.any(String),
    );
  });
});

// connector and qurl tests that require resetModules are in qurl-send.test.js

describe('collector button handlers — revoke and expand', () => {
  it('handles revoke button click in collector', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockUploadToConnector.mockResolvedValue({ resource_id: 'conn-col' });
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/c1' }]);
    mockSendDM.mockResolvedValue(true);
    mockDb.getSendResourceIds.mockReturnValue(['conn-col']);
    mockDeleteLink.mockResolvedValue(undefined);

    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 100, url: 'https://cdn.discordapp.com/doc.pdf' };
    const resInteraction = { customId: `qurl_res_file_${MOCK_NONCE}`, deferUpdate: jest.fn().mockResolvedValue(undefined) };

    let collectHandler, endHandler;
    const collectorObj = {
      on: jest.fn((event, handler) => {
        if (event === 'collect') collectHandler = handler;
        if (event === 'end') endHandler = handler;
        return collectorObj;
      }),
      stop: jest.fn(),
    };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const editReplyMock = jest.fn().mockResolvedValue({
      createMessageComponentCollector: jest.fn(() => collectorObj),
    });

    const interaction = makeInteraction({
      commandName: 'qurl',
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
      editReply: editReplyMock,
    });

    await cmd.execute(interaction);

    // Now simulate revoke button click
    expect(collectHandler).toBeDefined();
    const btnInteraction = {
      customId: `qurl_revoke_mock-uuid-1234`,
      user: { id: 'user-1' },
      deferUpdate: jest.fn().mockResolvedValue(undefined),
    };
    await collectHandler(btnInteraction);

    // Should have called deleteLink
    expect(mockDeleteLink).toHaveBeenCalled();
  });

  it('handles expand/collapse button in collector', async () => {
    // Setup a send with > 5 recipients to get the expand button
    const recipients = Array.from({ length: 7 }, (_, i) => ({ id: `r${i}`, username: `U${i}` }));
    mockGetText.mockReturnValue(recipients);
    mockUploadToConnector.mockResolvedValue({ resource_id: 'conn-exp' });
    mockMintLinks.mockResolvedValue(recipients.map((_, i) => ({ qurl_link: `https://q.test/e${i}` })));
    mockSendDM.mockResolvedValue(true);

    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 100, url: 'https://cdn.discordapp.com/doc.pdf' };
    const resInteraction = { customId: `qurl_res_file_${MOCK_NONCE}`, deferUpdate: jest.fn().mockResolvedValue(undefined) };

    let collectHandler;
    const collectorObj = {
      on: jest.fn((event, handler) => {
        if (event === 'collect') collectHandler = handler;
        return collectorObj;
      }),
    };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const editReplyMock = jest.fn().mockResolvedValue({
      createMessageComponentCollector: jest.fn(() => collectorObj),
    });

    const interaction = makeInteraction({
      commandName: 'qurl',
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
      editReply: editReplyMock,
    });

    await cmd.execute(interaction);

    // Simulate expand button click
    const btnInteraction = {
      customId: `qurl_expand_mock-uuid-1234`,
      user: { id: 'user-1' },
      deferUpdate: jest.fn().mockResolvedValue(undefined),
    };
    await collectHandler(btnInteraction);

    // Should have called editReply to show expanded list
    expect(editReplyMock).toHaveBeenCalled();
  });

  it('handles collector end with time reason', async () => {
    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockUploadToConnector.mockResolvedValue({ resource_id: 'conn-end' });
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/end1' }]);
    mockSendDM.mockResolvedValue(true);

    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 100, url: 'https://cdn.discordapp.com/doc.pdf' };
    const resInteraction = { customId: `qurl_res_file_${MOCK_NONCE}`, deferUpdate: jest.fn().mockResolvedValue(undefined) };

    let endHandler;
    const collectorObj = {
      on: jest.fn((event, handler) => {
        if (event === 'end') endHandler = handler;
        return collectorObj;
      }),
    };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const editReplyMock = jest.fn().mockResolvedValue({
      createMessageComponentCollector: jest.fn(() => collectorObj),
    });

    const interaction = makeInteraction({
      commandName: 'qurl',
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
      editReply: editReplyMock,
    });

    await cmd.execute(interaction);

    // Simulate collector end due to time
    expect(endHandler).toBeDefined();
    await endHandler(new Map(), 'time');

    // Should have called editReply with expired message
    const lastCall = editReplyMock.mock.calls[editReplyMock.mock.calls.length - 1];
    expect(lastCall[0].content).toContain('Management window closed');
  });
});

describe('monitorLinkStatus — via full send flow with fake timers', () => {
  it('monitors link status after send, polls for status changes', async () => {
    jest.useFakeTimers();

    const recipients = [{ id: 'r1', username: 'Alice' }];
    mockGetText.mockReturnValue(recipients);
    mockUploadToConnector.mockResolvedValue({ resource_id: 'conn-m1' });
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/m1' }]);
    mockSendDM.mockResolvedValue(true);
    mockGetResourceStatus.mockResolvedValue({
      qurls: [
        { qurl_id: 'q1', use_count: 0, status: 'active', created_at: '2026-01-01T00:00:00Z' },
      ],
    });

    const attachment = {
      name: 'doc.pdf', contentType: 'application/pdf', size: 100,
      url: 'https://cdn.discordapp.com/doc.pdf',
    };

    const resInteraction = {
      customId: `qurl_res_file_${MOCK_NONCE}`,
      deferUpdate: jest.fn().mockResolvedValue(undefined),
    };

    const collectorObj = { on: jest.fn() };

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
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
      channel: {
        awaitMessageComponent: jest.fn().mockResolvedValue(resInteraction),
      },
      editReply: jest.fn().mockResolvedValue({
        createMessageComponentCollector: jest.fn(() => collectorObj),
      }),
    });

    await cmd.execute(interaction);

    // The monitor's setInterval should be running
    // Advance timers to trigger at least one poll
    await jest.advanceTimersByTimeAsync(60000);

    // After the poll, getResourceStatus should have been called
    expect(mockGetResourceStatus).toHaveBeenCalled();

    // Now simulate link being opened
    mockGetResourceStatus.mockResolvedValue({
      qurls: [
        { qurl_id: 'q1', use_count: 1, status: 'used', created_at: '2026-01-01T00:00:00Z' },
      ],
    });

    await jest.advanceTimersByTimeAsync(60000);

    jest.useRealTimers();
  });
});

describe('autocomplete handling', () => {
  it('returns early for all autocomplete interactions', async () => {
    const interaction = makeInteraction({
      commandName: 'qurl',
      isAutocomplete: jest.fn(() => true),
      isChatInputCommand: jest.fn(() => false),
      options: {
        ...makeInteraction().options,
        getFocused: jest.fn(() => ({ name: 'location', value: 'test query' })),
      },
      user: { id: 'autocomplete-user', username: 'TestUser' },
    });
    await handleCommand(interaction);

    // Autocomplete handler was removed; handleCommand returns early
    expect(interaction.respond).not.toHaveBeenCalled();
  });
});
