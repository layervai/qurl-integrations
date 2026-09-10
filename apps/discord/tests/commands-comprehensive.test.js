

jest.mock('../src/config', () => ({
  QURL_API_KEY: 'test-api-key',
  QURL_ENDPOINT: 'https://api.test.local',
  CONNECTOR_URL: 'https://connector.test.local',
  GOOGLE_MAPS_API_KEY: 'test-google-key',
  MAP_COMMAND_ENABLED: false,
  DETECT_COMMAND_ENABLED: false,
  QURL_SEND_COOLDOWN_MS: 30000,
  QURL_DETECT_COOLDOWN_MS: 30000,
  QURL_SEND_MAX_RECIPIENTS: 50,
  BASE_URL: 'http://localhost:3000',
  GUILD_ID: 'guild-1',
  SHARD_ID: '0:1',
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
  const { makeOptionBuilder, makeComponentChainable } = require('./helpers/discord-mock');
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
    setDefaultValues: jest.fn().mockReturnThis(),
    addDefaultUsers: jest.fn().mockReturnThis(),
  })),
  MentionableSelectMenuBuilder: jest.fn().mockImplementation(() => makeComponentChainable()),
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
    setMinLength: jest.fn().mockReturnThis(),
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
  getGuildApiKey: jest.fn().mockResolvedValue(null),
  setGuildApiKey: jest.fn().mockResolvedValue(undefined),
  getRecentSends: jest.fn(() => []),
  getSendResourceIds: jest.fn(() => []),
  getSendItems: jest.fn(() => []),
  markSendRevoking: jest.fn().mockResolvedValue(true),
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
  uploadToConnector: mockUploadToConnector,
  downloadAndUpload: mockDownloadAndUpload,
  reUploadBuffer: mockReUploadBuffer,
  mintLinks: mockMintLinks,
  uploadJsonToConnector: mockUploadJsonToConnector,
}));

const mockCreateOneTimeLink = jest.fn();
const mockDeleteLink = jest.fn();
const mockGetResourceStatus = jest.fn();
jest.mock('../src/qurl', () => ({
  createOneTimeLink: mockCreateOneTimeLink,
  deleteLink: mockDeleteLink,
  getResourceStatus: mockGetResourceStatus,
}));

const { mockPlacesModule } = require('./helpers/places-mock');
jest.mock('../src/places', () => mockPlacesModule);

const mockCreateFlow = jest.fn().mockResolvedValue({ created: true, version: 1 });
const mockLoadFlow = jest.fn();
const mockDeleteFlow = jest.fn().mockResolvedValue({ deleted: true });
const mockTransitionFlow = jest.fn();
const mockSupersedeOrCreate = jest.fn().mockResolvedValue({ created: true, version: 1 });
jest.mock('../src/flow-state', () => ({
  createFlow: (...args) => mockCreateFlow(...args),
  loadFlow: (...args) => mockLoadFlow(...args),
  deleteFlow: (...args) => mockDeleteFlow(...args),
  transitionFlow: (...args) => mockTransitionFlow(...args),
  supersedeOrCreate: (...args) => mockSupersedeOrCreate(...args),
}));

jest.mock('../src/guild-webhook-link', () => ({
  linkGuildWebhookSubscription: jest.fn().mockResolvedValue({ ok: true, action: 'created' }),
  fireAndForgetLinkGuildWebhookSubscription: jest.fn(),
}));

const crypto = require('crypto');
const originalRandomBytes = crypto.randomBytes;
const MOCK_NONCE = 'deadbeef01234567';
crypto.randomBytes = jest.fn((size) => {
  if (size === 8) return Buffer.from(MOCK_NONCE, 'hex');
  return originalRandomBytes(size);
});
const originalRandomUUID = crypto.randomUUID;
crypto.randomUUID = jest.fn(() => 'mock-uuid-1234');

const { commands, handleCommand, registerCommands, _test } = require('../src/commands');
const {
  isGoogleMapsURL, sanitizeFilename, sanitizeMessage,
  isAllowedFileType, isOnCooldown, setCooldown, batchSettled, expiryToISO,
  sendCooldowns, handleAddRecipients,
} = _test;

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
    guildId: 'guild-1',
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

beforeEach(() => {
  jest.clearAllMocks();
  mockDb.markSendRevoked.mockReset();
  mockDb.markSendRevoked.mockResolvedValue(true);
  embedInstances.length = 0;
  sendCooldowns.clear();
});

const qurlCommand = commands.find(c => c.data.name === 'qurl');
const realQurlExecute = qurlCommand.execute;
function stubQurlExecute(impl) {
  qurlCommand.execute = jest.fn(impl);
  return qurlCommand.execute;
}
afterEach(() => {
  qurlCommand.execute = realQurlExecute;
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
  it('issues rest.put for the global commands endpoint when GUILD_ID is unset', async () => {
    const rest = {
      put: jest.fn().mockResolvedValue([]),
      get: jest.fn().mockResolvedValue([]),
    };
    await registerCommands({ rest, appId: 'app-123', guilds: new Map() });
    expect(rest.put).toHaveBeenCalled();
  });

  it('logs error when rest.put rejects', async () => {
    const logger = require('../src/logger');
    const rest = {
      put: jest.fn().mockRejectedValue(new Error('fail')),
      get: jest.fn().mockResolvedValue([]),
    };
    await registerCommands({ rest, appId: 'app-123', guilds: new Map() });
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

  it('replies "no longer available" for unknown command names (stale-registration path)', async () => {
    const interaction = makeInteraction({
      commandName: 'nonexistent-cmd',
    });
    await handleCommand(interaction);
    expect(interaction.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('no longer available'),
      ephemeral: true,
    }));
  });

  it('handles errors gracefully when command throws and not deferred', async () => {
    stubQurlExecute(() => { throw new Error('db crash'); });
    const interaction = makeInteraction({ commandName: 'qurl' });

    await handleCommand(interaction);

    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('error'), ephemeral: true }),
    );
  });

  it('uses followUp when reply already sent', async () => {
    stubQurlExecute(() => { throw new Error('db crash'); });
    const interaction = makeInteraction({
      commandName: 'qurl',
      replied: true,
    });

    await handleCommand(interaction);

    expect(interaction.followUp).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('error') }),
    );
  });

  it('uses followUp when deferred', async () => {
    stubQurlExecute(() => { throw new Error('db crash'); });
    const interaction = makeInteraction({
      commandName: 'qurl',
      deferred: true,
    });

    await handleCommand(interaction);

    expect(interaction.followUp).toHaveBeenCalled();
  });

  it('handles reply failure in error handler', async () => {
    stubQurlExecute(() => { throw new Error('db crash'); });
    const interaction = makeInteraction({
      commandName: 'qurl',
      reply: jest.fn().mockRejectedValue(new Error('cannot reply')),
    });

    await handleCommand(interaction);
    const logger = require('../src/logger');
    expect(logger.error).toHaveBeenCalled();
  });
});

describe('handleCommand — INTERACTION_HANDLED audit emission', () => {
  const { AUDIT_EVENTS } = require('../src/constants');
  let logger;

  beforeEach(() => {
    logger = require('../src/logger');
    logger.audit.mockClear();
  });

  it('emits success=true when command executes cleanly', async () => {
    stubQurlExecute(async () => {});
    const interaction = makeInteraction({ commandName: 'qurl' });
    await handleCommand(interaction);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.INTERACTION_HANDLED,
      expect.objectContaining({
        command_name: 'qurl',
        success: true,
        failure_type: null,
        handler_duration_ms: expect.any(Number),
      }),
    );
  });

  it('emits failure_type=handler_error when execute() throws', async () => {
    stubQurlExecute(() => { throw new Error('db crash'); });
    const interaction = makeInteraction({ commandName: 'qurl' });
    await handleCommand(interaction);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.INTERACTION_HANDLED,
      expect.objectContaining({ command_name: 'qurl', success: false, failure_type: 'handler_error' }),
    );
  });

  it('emits failure_type=ack_timeout on Discord 10062 (Unknown interaction)', async () => {
    const ackErr = Object.assign(new Error('Unknown interaction'), { code: 10062 });
    stubQurlExecute(() => { throw ackErr; });
    const interaction = makeInteraction({ commandName: 'qurl' });
    await handleCommand(interaction);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.INTERACTION_HANDLED,
      expect.objectContaining({ command_name: 'qurl', success: false, failure_type: 'ack_timeout' }),
    );
  });

  it('emits failure_type=unknown_command for stale-registration path', async () => {
    const interaction = makeInteraction({ commandName: 'no-such-cmd' });
    await handleCommand(interaction);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.INTERACTION_HANDLED,
      expect.objectContaining({ command_name: 'no-such-cmd', success: false, failure_type: 'unknown_command' }),
    );
  });

  it('emits failure_type=unsupported_context when rejecting a DM invocation', async () => {
    const interaction = makeInteraction({ guildId: null });
    await handleCommand(interaction);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.INTERACTION_HANDLED,
      expect.objectContaining({ command_name: 'qurl', success: false, failure_type: 'unsupported_context' }),
    );
  });

  it('emits failure_type=reply_failed when stale-registration reply throws non-ack error', async () => {
    const interaction = makeInteraction({
      commandName: 'no-such-cmd',
      reply: jest.fn().mockRejectedValue(new Error('Missing Permissions')),
    });
    await handleCommand(interaction);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.INTERACTION_HANDLED,
      expect.objectContaining({ command_name: 'no-such-cmd', success: false, failure_type: 'reply_failed' }),
    );
  });

  it('emits failure_type=ack_timeout when stale-registration reply hits Discord 10062', async () => {
    const ackErr = Object.assign(new Error('Unknown interaction'), { code: 10062 });
    const interaction = makeInteraction({
      commandName: 'no-such-cmd',
      reply: jest.fn().mockRejectedValue(ackErr),
    });
    await handleCommand(interaction);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.INTERACTION_HANDLED,
      expect.objectContaining({ command_name: 'no-such-cmd', success: false, failure_type: 'ack_timeout' }),
    );
  });

  it.each([
    ['reply_failed', new Error('Missing Permissions')],
    ['ack_timeout', Object.assign(new Error('Unknown interaction'), { code: 10062 })],
  ])('emits failure_type=%s when the unsupported-context reply fails', async (failureType, error) => {
    const interaction = makeInteraction({
      guildId: null,
      reply: jest.fn().mockRejectedValue(error),
    });

    await handleCommand(interaction);

    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.INTERACTION_HANDLED,
      expect.objectContaining({ command_name: 'qurl', success: false, failure_type: failureType }),
    );
  });

  it('preserves sub-millisecond handler_duration_ms (no BigInt-truncation regression)', async () => {
    const realHrtime = process.hrtime.bigint;
    let callCount = 0;
    process.hrtime.bigint = jest.fn(() => {
      callCount++;
      return callCount === 1 ? 0n : 500_000n;
    });
    try {
      const interaction = makeInteraction({ commandName: 'stats' });
      await handleCommand(interaction);
      const auditCalls = logger.audit.mock.calls.filter(
        c => c[0] === AUDIT_EVENTS.INTERACTION_HANDLED,
      );
      expect(auditCalls).toHaveLength(1);
      expect(auditCalls[0][1].handler_duration_ms).toBe(0.5);
    } finally {
      process.hrtime.bigint = realHrtime;
    }
  });

  it('emits INTERACTION_HANDLED EXACTLY ONCE across each failure scenario (cardinality lock)', async () => {
    logger = require('../src/logger');
    const scenarios = [
      ['success path', () => makeInteraction({ commandName: 'stats' })],
      ['handler_error', () => {
        const i = makeInteraction({ commandName: 'stats' });
        mockDb.getStats.mockImplementationOnce(() => { throw new Error('db crash'); });
        return i;
      }],
      ['unknown_command', () => makeInteraction({ commandName: 'no-such-cmd' })],
      ['reply_failed (stale-reg path)', () => makeInteraction({
        commandName: 'no-such-cmd',
        reply: jest.fn().mockRejectedValue(new Error('Missing Permissions')),
      })],
    ];
    for (const [name, mkInteraction] of scenarios) {
      logger.audit.mockClear();
      await handleCommand(mkInteraction());
      const interactionCalls = logger.audit.mock.calls.filter(
        c => c[0] === AUDIT_EVENTS.INTERACTION_HANDLED,
      );
      expect(interactionCalls).toHaveLength(1);
    }
  });

  it('preserves handler_error when execute throws AND followUp also throws non-ack', async () => {
    stubQurlExecute(() => { throw new Error('db crash'); });
    const interaction = makeInteraction({
      commandName: 'qurl',
      reply: jest.fn().mockRejectedValue(new Error('Missing Permissions')),
    });
    await handleCommand(interaction);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.INTERACTION_HANDLED,
      expect.objectContaining({ command_name: 'qurl', success: false, failure_type: 'handler_error' }),
    );
  });

  describe('isAckTimeoutError direct regex coverage', () => {
    const { isAckTimeoutError } = _test;
    test.each([
      ['discord.js DiscordAPIError code 10062', { code: 10062 }, true],
      ['exact bare message', new Error('Unknown interaction'), true],
      ['wrapped with RESTJSONError prefix', new Error('RESTJSONError: Unknown interaction'), true],
      ['discord.js DiscordAPIError[10062]: prefix shape (typical wrapped)', new Error('DiscordAPIError[10062]: Unknown interaction'), true],
      ['arbitrary class with numeric-bracket prefix', new Error('SomeApiError[42]: Unknown interaction'), true],
      ['wrapped with arbitrary class prefix', new Error('SomeWrapper: Unknown interaction'), true],
      ['rejected: trailing content (Discord type variant)', new Error('Unknown interaction type 5'), false],
      ['rejected: substring inside other message', new Error('Failed to handle Unknown interaction'), false],
      ['rejected: numeric .message', { message: 5 }, false],
      ['rejected: no message and no code', { foo: 'bar' }, false],
      ['rejected: null', null, false],
      ['rejected: undefined', undefined, false],
    ])('%s → %s', (_name, err, expected) => {
      expect(isAckTimeoutError(err)).toBe(expected);
    });
  });

  it('does not emit for autocomplete events (early-return path)', async () => {
    const interaction = makeInteraction({
      isChatInputCommand: jest.fn(() => false),
      isAutocomplete: jest.fn(() => true),
    });
    await handleCommand(interaction);
    const calls = logger.audit.mock.calls.filter(c => c[0] === AUDIT_EVENTS.INTERACTION_HANDLED);
    expect(calls).toHaveLength(0);
  });
});

describe('/qurl help subcommand', () => {
  async function renderHelp({ qurlOAuth, discordInstall }) {
    const config = require('../src/config');
    const originalQurlOAuth = config.isQurlOAuthConfigured;
    const originalDiscordInstall = config.isDiscordInstallConfigured;
    config.isQurlOAuthConfigured = qurlOAuth;
    config.isDiscordInstallConfigured = discordInstall;

    try {
      const cmd = commands.find(c => c.data.name === 'qurl');
      const interaction = makeInteraction({
        commandName: 'qurl',
        options: {
          ...makeInteraction().options,
          getSubcommand: jest.fn(() => 'help'),
        },
      });
      await cmd.execute(interaction);
      return interaction.reply.mock.calls[0][0].content;
    } finally {
      config.isQurlOAuthConfigured = originalQurlOAuth;
      config.isDiscordInstallConfigured = originalDiscordInstall;
    }
  }

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
        content: expect.stringContaining('qURL Bot'),
        ephemeral: true,
      }),
    );
  });

  it('includes the four help-text copy fixes', async () => {
    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'help'),
      },
    });

    await cmd.execute(interaction);

    const { content } = interaction.reply.mock.calls[0][0];

    expect(content).toContain('https://layerv.ai');
    expect(content).toMatch(/self-destructs? on first access.*expiry elapses/);
    expect(content).toContain('protected resource');
    expect(content).toContain('access link');
    expect(content).not.toContain('GUILD_PRESENCES');
  });

  it('does not advertise Add to Discord when only qURL OAuth is configured', async () => {
    const content = await renderHelp({ qurlOAuth: true, discordInstall: false });

    expect(content).toContain('`/qurl setup` — connect qURL via OAuth');
    expect(content).not.toContain('Adding the bot to a new server');
    expect(content).not.toContain('Add to Discord');
  });

  it('keeps legacy setup copy and no install CTA before qURL OAuth is configured', async () => {
    const content = await renderHelp({ qurlOAuth: false, discordInstall: false });

    expect(content).toContain('`/qurl setup` — configure your API key');
    expect(content).not.toContain('connect qURL via OAuth');
    expect(content).not.toContain('Add to Discord');
  });

  it('advertises Add to Discord when the customer install flow is configured', async () => {
    const content = await renderHelp({ qurlOAuth: true, discordInstall: true });

    expect(content).toContain('Adding the bot to a new server');
    expect(content).toContain('Add to Discord');
  });
});

describe('/qurl revoke subcommand', () => {
  beforeEach(() => {
    mockSupersedeOrCreate.mockResolvedValue({ created: true, version: 1 });
  });

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
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
  });

  it('opens a flow row and renders the select menu', async () => {
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

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'revoke'),
      },
    });

    await cmd.execute(interaction);

    expect(mockSupersedeOrCreate).toHaveBeenCalledTimes(1);
    const args = mockSupersedeOrCreate.mock.calls[0][0];
    expect(args.stage).toBe('awaiting_revoke_select');
    expect(args.payload).toBeNull();
    expect(args.flow_id).toMatch(/^0:1#guild-1#ch-1#user-1$/);
    expect(args.ttl_seconds).toEqual(expect.any(Number));

    const menuCall = interaction.editReply.mock.calls.find(
      (c) => c[0]?.components?.length > 0,
    );
    expect(menuCall).toBeDefined();
    expect(menuCall[0].content).toMatch(/Select a send to revoke/);
  });

  it('labels an incomplete prior revoke as retryable in the menu', async () => {
    mockDb.getRecentSends.mockReturnValue([{
      send_id: 'send-pending',
      resource_type: 'file',
      target_type: 'user',
      recipient_count: 2,
      delivered_count: 2,
      expires_in: '24h',
      created_at: new Date().toISOString(),
      revocation_pending: true,
    }]);

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'revoke'),
      },
    });

    await cmd.execute(interaction);

    const menuCall = interaction.editReply.mock.calls.find(
      c => c[0]?.components?.length > 0,
    );
    const option = menuCall[0].components[0].components[0].addOptions.mock.calls[0][0][0];
    expect(option.description).toMatch(/^Retry ·/);
    expect(option.description.length).toBeLessThanOrEqual(100);
  });

  it('caps a retry description at Discord\'s 100-character option limit', async () => {
    mockDb.getRecentSends.mockReturnValue([{
      send_id: 'send-pending-long',
      resource_type: 'file',
      target_type: 'user',
      recipient_count: Number.MAX_SAFE_INTEGER,
      delivered_count: Number.MAX_SAFE_INTEGER,
      expires_in: 'x'.repeat(300),
      created_at: new Date().toISOString(),
      personal_message: 'y'.repeat(300),
      revocation_pending: true,
    }]);
    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'revoke'),
      },
    });

    await cmd.execute(interaction);

    const menuCall = interaction.editReply.mock.calls.find(c => c[0]?.components?.length > 0);
    const option = menuCall[0].components[0].components[0].addOptions.mock.calls[0][0][0];
    expect(option.description).toHaveLength(100);
  });

  it('renders the menu when supersedeOrCreate claims the slot from a stale predecessor', async () => {
    mockSupersedeOrCreate.mockResolvedValueOnce({ created: true, version: 1 });
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

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'revoke'),
      },
    });

    await cmd.execute(interaction);

    expect(mockSupersedeOrCreate).toHaveBeenCalledTimes(1);
    const menuCall = interaction.editReply.mock.calls.find(
      (c) => c[0]?.components?.length > 0,
    );
    expect(menuCall).toBeDefined();
  });

  it('names a sibling setup-modal flow when revoke supersede cannot claim', async () => {
    mockSupersedeOrCreate.mockResolvedValueOnce({
      created: false,
      surviving: { stage: 'awaiting_setup_modal', version: 1 },
    });
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

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'revoke'),
      },
    });

    await cmd.execute(interaction);

    const siblingCall = interaction.editReply.mock.calls.find(
      (c) => /qurl setup/.test(c[0]?.content || ''),
    );
    expect(siblingCall).toBeDefined();
  });

  it('names a sibling setup-button flow when revoke supersede finds one in the channel', async () => {
    mockSupersedeOrCreate.mockResolvedValueOnce({
      created: false,
      surviving: { stage: 'awaiting_setup_button', version: 1 },
    });
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

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'revoke'),
      },
    });

    await cmd.execute(interaction);

    const siblingCall = interaction.editReply.mock.calls.find(
      (c) => /qurl setup.*button/.test(c[0]?.content || ''),
    );
    expect(siblingCall).toBeDefined();
  });

  it('falls through to generic error when the surviving row is at an unregistered stage', async () => {
    mockSupersedeOrCreate.mockResolvedValueOnce({
      created: false,
      surviving: { stage: 'unknown_future_stage', version: 1 },
    });
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

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'revoke'),
      },
    });

    await cmd.execute(interaction);

    const errorCall = interaction.editReply.mock.calls.find(
      (c) => /Could not start a revoke session/.test(c[0]?.content || ''),
    );
    expect(errorCall).toBeDefined();
  });

  it('surfaces an error when supersedeOrCreate throws', async () => {
    mockSupersedeOrCreate.mockRejectedValueOnce(new Error('DDB throttle'));
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

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeInteraction({
      commandName: 'qurl',
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'revoke'),
      },
    });

    await cmd.execute(interaction);

    const errorCall = interaction.editReply.mock.calls.find(
      (c) => /Could not start a revoke session/.test(c[0]?.content || ''),
    );
    expect(errorCall).toBeDefined();
  });
});

describe('handleRevokeSelect (dispatcher path)', () => {
  const { handleRevokeSelect } = require('../src/commands');

  function makeSelectInteraction(overrides = {}) {
    return {
      values: ['send-1'],
      user: { id: 'user-1' },
      guildId: 'guild-1',
      channelId: 'ch-1',
      update: jest.fn().mockResolvedValue(undefined),
      reply: jest.fn().mockResolvedValue(undefined),
      ...overrides,
    };
  }

  beforeEach(() => {
    mockDeleteFlow.mockResolvedValue({ deleted: true });
  });

  it('runs revoke when deleteFlow wins (deleted=true)', async () => {
    mockDb.getSendItems.mockReturnValue([
      { resource_id: 'res-1', recipient_discord_id: 'u-1' },
      { resource_id: 'res-2', recipient_discord_id: 'u-2' },
      { resource_id: 'res-3', recipient_discord_id: 'u-3' },
    ]);
    mockDeleteLink.mockResolvedValue(undefined);
    const interaction = makeSelectInteraction({ values: ['send-99'] });

    await handleRevokeSelect(interaction, { flow_id: '0:1#guild-1#ch-1#user-1' });

    expect(mockDeleteFlow).toHaveBeenCalledWith(
      '0:1#guild-1#ch-1#user-1',
      { stage: 'awaiting_revoke_select', reason: 'terminal' },
    );
    expect(mockDeleteLink).toHaveBeenCalledTimes(3);
    expect(interaction.update).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('3/3') }),
    );
  });

  it('skips revoke when deleteFlow loses the race (deleted=false)', async () => {
    mockDeleteFlow.mockResolvedValueOnce({ deleted: false });
    const interaction = makeSelectInteraction();

    await handleRevokeSelect(interaction, { flow_id: '0:1#guild-1#ch-1#user-1' });

    expect(mockDeleteLink).not.toHaveBeenCalled();
    expect(interaction.update).not.toHaveBeenCalled();
    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringContaining('already processed'),
      }),
    );
  });

  it('updates with an error if apiKey resolution is no longer configured', async () => {
    mockDeleteFlow.mockResolvedValueOnce({ deleted: true });
    mockDb.getGuildApiKey = jest.fn().mockResolvedValue(null);
    const originalQurlApiKey = require('../src/config').QURL_API_KEY;
    require('../src/config').QURL_API_KEY = null;

    const interaction = makeSelectInteraction();
    try {
      await handleRevokeSelect(interaction, { flow_id: '0:1#guild-1#ch-1#user-1' });

      expect(mockDeleteLink).not.toHaveBeenCalled();
      expect(interaction.update).toHaveBeenCalledWith(
        expect.objectContaining({
          content: expect.stringContaining('no longer configured'),
        }),
      );
    } finally {
      require('../src/config').QURL_API_KEY = originalQurlApiKey;
    }
  });

  it('reports a partial revoke as an unconfirmed failure', async () => {
    mockDb.getSendItems.mockReturnValue([
      { resource_id: 'res-1', recipient_discord_id: 'u-1' },
      { resource_id: 'res-2', recipient_discord_id: 'u-2' },
    ]);
    mockDeleteLink
      .mockResolvedValueOnce(undefined)
      .mockRejectedValueOnce(new Error('not found'));

    const interaction = makeSelectInteraction({ values: ['send-partial'] });
    await handleRevokeSelect(interaction, { flow_id: '0:1#guild-1#ch-1#user-1' });

    expect(interaction.update).toHaveBeenCalledWith(
      expect.objectContaining({ content: expect.stringContaining('1/2') }),
    );
  });

  it('reports successful DELETEs truthfully when the final revoked state write fails', async () => {
    mockDb.getSendItems.mockReturnValue([
      { resource_id: 'res-1', recipient_discord_id: 'u-1' },
    ]);
    mockDeleteLink.mockResolvedValue(undefined);
    mockDb.markSendRevoked.mockRejectedValueOnce(new Error('DDB finalize failed'));

    const interaction = makeSelectInteraction({ values: ['send-finalize-fail'] });
    await handleRevokeSelect(interaction, { flow_id: '0:1#guild-1#ch-1#user-1' });

    expect(interaction.update).toHaveBeenCalledWith({
      content: expect.stringContaining('Revoked 1/1 user.'),
      components: [],
    });
    expect(interaction.update).toHaveBeenCalledWith({
      content: expect.stringContaining('could not save the final revocation state'),
      components: [],
    });
  });

  it('does not claim 0/0 success when the durable revoke barrier rejects the send', async () => {
    mockDb.markSendRevoking.mockResolvedValueOnce(false);
    const interaction = makeSelectInteraction({ values: ['foreign-or-finalized'] });

    await handleRevokeSelect(interaction, { flow_id: '0:1#guild-1#ch-1#user-1' });

    expect(mockDeleteLink).not.toHaveBeenCalled();
    expect(interaction.update).toHaveBeenCalledWith({
      content: 'Could not verify this send for revocation. It may already be revoked or unavailable; run `/qurl revoke` to refresh.',
      components: [],
    });
  });

  it('retries a temporary DELETE failure and finalizes after the next selection', async () => {
    mockDb.getSendItems.mockReturnValue([
      { resource_id: 'res-1', recipient_discord_id: 'u-1' },
      { resource_id: 'res-2', recipient_discord_id: 'u-2' },
    ]);
    mockDeleteLink
      .mockResolvedValueOnce(undefined)
      .mockRejectedValueOnce(new Error('temporary qURL 503'))
      .mockResolvedValueOnce(undefined)
      .mockResolvedValueOnce(undefined);

    const first = makeSelectInteraction({ values: ['send-retry'] });
    await handleRevokeSelect(first, { flow_id: '0:1#guild-1#ch-1#user-1' });
    expect(first.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('Could not confirm revocation for 1 user'),
    }));
    expect(mockDb.markSendRevoked).not.toHaveBeenCalled();

    const second = makeSelectInteraction({ values: ['send-retry'] });
    await handleRevokeSelect(second, { flow_id: '0:1#guild-1#ch-1#user-1' });
    expect(second.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('Revoked 2/2 users.'),
    }));
    expect(mockDeleteLink).toHaveBeenCalledTimes(4);
    expect(mockDb.markSendRevoked).toHaveBeenCalledWith('send-retry', 'user-1');
  });
});

describe('/qurl setup subcommand (legacy modal-paste path)', () => {
  const originalKEK = process.env.KEY_ENCRYPTION_KEY;
  const originalAuthDomain = process.env.AUTH0_DOMAIN;
  beforeAll(() => {
    process.env.KEY_ENCRYPTION_KEY = '0'.repeat(64);
    delete process.env.AUTH0_DOMAIN;
  });
  afterAll(() => {
    if (originalKEK === undefined) delete process.env.KEY_ENCRYPTION_KEY;
    else process.env.KEY_ENCRYPTION_KEY = originalKEK;
    if (originalAuthDomain === undefined) delete process.env.AUTH0_DOMAIN;
    else process.env.AUTH0_DOMAIN = originalAuthDomain;
  });
  beforeEach(() => {
    mockSupersedeOrCreate.mockResolvedValue({ created: true, version: 1 });
  });

  function makeSetupInteraction() {
    return makeInteraction({
      commandName: 'qurl',
      memberPermissions: { has: jest.fn().mockReturnValue(true) },
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'setup'),
      },
    });
  }

  it('opens a flow row + renders the configure button', async () => {
    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeSetupInteraction();

    await cmd.execute(interaction);

    expect(mockSupersedeOrCreate).toHaveBeenCalledTimes(1);
    const args = mockSupersedeOrCreate.mock.calls[0][0];
    expect(args.stage).toBe('awaiting_setup_button');
    expect(args.payload).toBeNull();
    expect(args.flow_id).toBe('0:1#guild-1#ch-1#user-1');
    expect(args.ttl_seconds).toEqual(expect.any(Number));

    const buttonCall = interaction.editReply.mock.calls.find(
      (c) => c[0]?.components?.length > 0,
    );
    expect(buttonCall).toBeDefined();
    expect(buttonCall[0].content).toMatch(/Connect qURL to this server/);
  });

  it('refuses if KEK is not set', async () => {
    const savedKEK = process.env.KEY_ENCRYPTION_KEY;
    delete process.env.KEY_ENCRYPTION_KEY;
    try {
      const cmd = commands.find(c => c.data.name === 'qurl');
      const interaction = makeSetupInteraction();
      await cmd.execute(interaction);

      expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
      expect(interaction.reply).toHaveBeenCalledWith(
        expect.objectContaining({
          content: expect.stringContaining('KEY_ENCRYPTION_KEY'),
        }),
      );
    } finally {
      process.env.KEY_ENCRYPTION_KEY = savedKEK;
    }
  });

  it('refuses in DM context', async () => {
    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeSetupInteraction();
    interaction.guildId = null;

    await cmd.execute(interaction);

    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringContaining('server, not in DMs'),
      }),
    );
  });

  it('refuses for non-admin users', async () => {
    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeSetupInteraction();
    interaction.memberPermissions = { has: jest.fn().mockReturnValue(false) };

    await cmd.execute(interaction);

    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringContaining('administrators'),
      }),
    );
  });

  it('renders the button when supersedeOrCreate claims the slot from a stale predecessor', async () => {
    mockSupersedeOrCreate.mockResolvedValueOnce({ created: true, version: 1 });

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeSetupInteraction();
    await cmd.execute(interaction);

    expect(mockSupersedeOrCreate).toHaveBeenCalledTimes(1);
    const buttonCall = interaction.editReply.mock.calls.find(
      (c) => c[0]?.components?.length > 0,
    );
    expect(buttonCall).toBeDefined();
  });

  it('blocks with the modal-open message when a mid-modal flow is in progress', async () => {
    mockSupersedeOrCreate.mockResolvedValueOnce({
      created: false,
      surviving: {
        flow_id: '0:1#guild-1#ch-1#user-1',
        stage: 'awaiting_setup_modal',
        version: 2,
      },
    });

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeSetupInteraction();
    await cmd.execute(interaction);

    const blockedCall = interaction.editReply.mock.calls.find(
      (c) => /already have a `\/qurl setup` modal open/.test(c[0]?.content || ''),
    );
    expect(blockedCall).toBeDefined();
  });

  it('names the sibling revoke flow when supersede surfaces an in-flight revoke menu', async () => {
    mockSupersedeOrCreate.mockResolvedValueOnce({
      created: false,
      surviving: {
        flow_id: '0:1#guild-1#ch-1#user-1',
        stage: 'awaiting_revoke_select',
        version: 1,
      },
    });

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeSetupInteraction();
    await cmd.execute(interaction);

    const revokeMentionCall = interaction.editReply.mock.calls.find(
      (c) => /\/qurl revoke.*menu open/.test(c[0]?.content || ''),
    );
    expect(revokeMentionCall).toBeDefined();
    const modalMsgCall = interaction.editReply.mock.calls.find(
      (c) => /modal open/.test(c[0]?.content || ''),
    );
    expect(modalMsgCall).toBeUndefined();
  });

  it('falls back to generic wording when surviving is null (vanished between collide and peek)', async () => {
    mockSupersedeOrCreate.mockResolvedValueOnce({
      created: false,
      surviving: null,
    });

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeSetupInteraction();
    await cmd.execute(interaction);

    const genericCall = interaction.editReply.mock.calls.find(
      (c) => /Could not start a setup session/.test(c[0]?.content || ''),
    );
    expect(genericCall).toBeDefined();
  });

  it('falls back to generic wording when surviving stage has no registered siblingMessage', async () => {
    mockSupersedeOrCreate.mockResolvedValueOnce({
      created: false,
      surviving: {
        flow_id: '0:1#guild-1#ch-1#user-1',
        stage: 'awaiting_future_unregistered_stage',
        version: 1,
      },
    });

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeSetupInteraction();
    await cmd.execute(interaction);

    const genericCall = interaction.editReply.mock.calls.find(
      (c) => /Could not start a setup session/.test(c[0]?.content || ''),
    );
    expect(genericCall).toBeDefined();
  });

  it('surfaces a recoverable error when supersedeOrCreate throws', async () => {
    mockSupersedeOrCreate.mockRejectedValueOnce(new Error('DDB region timeout'));

    const cmd = commands.find(c => c.data.name === 'qurl');
    const interaction = makeSetupInteraction();

    await cmd.execute(interaction);

    const genericCall = interaction.editReply.mock.calls.find(
      (c) => /Could not start a setup session/.test(c[0]?.content || ''),
    );
    expect(genericCall).toBeDefined();
  });
});

describe('handleSetupButton (dispatcher path)', () => {
  const { handleSetupButton } = require('../src/commands');

  function makeButtonInteraction(overrides = {}) {
    return {
      user: { id: 'user-1' },
      guildId: 'guild-1',
      channelId: 'ch-1',
      showModal: jest.fn().mockResolvedValue(undefined),
      reply: jest.fn().mockResolvedValue(undefined),
      ...overrides,
    };
  }

  it('transitions flow to awaiting_setup_modal + shows modal on success', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'success', version: 2 });
    const interaction = makeButtonInteraction();

    await handleSetupButton(interaction, {
      flow_id: '0:1#guild-1#ch-1#user-1',
      row: { stage: 'awaiting_setup_button', version: 1 },
    });

    const transitionArgs = mockTransitionFlow.mock.calls[0];
    expect(transitionArgs[0]).toBe('0:1#guild-1#ch-1#user-1');
    expect(transitionArgs[1]).toBe(1); // version
    expect(transitionArgs[2].stage_to).toBe('awaiting_setup_modal');
    expect(transitionArgs[2].terminal).toBe(false);
    const { SETUP_BUTTON_TTL_SECONDS, SETUP_MODAL_TTL_SECONDS } = _test;
    const nowSec = Math.floor(Date.now() / 1000);
    expect(transitionArgs[2].set_expires_at).toBeGreaterThan(nowSec + SETUP_MODAL_TTL_SECONDS - 50);
    expect(transitionArgs[2].set_expires_at).toBeLessThanOrEqual(nowSec + SETUP_MODAL_TTL_SECONDS);
    expect(transitionArgs[2].set_expires_at).toBeGreaterThan(nowSec + SETUP_BUTTON_TTL_SECONDS);
    expect(transitionArgs[2].payload).toBeUndefined();

    expect(interaction.showModal).toHaveBeenCalledTimes(1);
    expect(interaction.reply).not.toHaveBeenCalled();

    const { TextInputBuilder } = require('discord.js');
    const lastInputBuilder = TextInputBuilder.mock.results.at(-1).value;
    expect(lastInputBuilder.setMinLength).toHaveBeenCalledWith(28);
    expect(lastInputBuilder.setMaxLength).toHaveBeenCalledWith(64);
  });

  it('replies (no modal) on OCC conflict', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict', version: null });
    const interaction = makeButtonInteraction();

    await handleSetupButton(interaction, {
      flow_id: '0:1#guild-1#ch-1#user-1',
      row: { stage: 'awaiting_setup_button', version: 1 },
    });

    expect(interaction.showModal).not.toHaveBeenCalled();
    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringContaining('Another setup attempt'),
      }),
    );
  });

  it('replies on not_found (TTL race)', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found', version: null });
    const interaction = makeButtonInteraction();

    await handleSetupButton(interaction, {
      flow_id: '0:1#guild-1#ch-1#user-1',
      row: { stage: 'awaiting_setup_button', version: 1 },
    });

    expect(interaction.showModal).not.toHaveBeenCalled();
    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringContaining('expired'),
      }),
    );
  });

  it('propagates throws from transitionFlow (caught by dispatcher safety net)', async () => {
    const ddbErr = new Error('DDB region timeout');
    mockTransitionFlow.mockRejectedValueOnce(ddbErr);
    const interaction = makeButtonInteraction();

    await expect(handleSetupButton(interaction, {
      flow_id: '0:1#guild-1#ch-1#user-1',
      row: { stage: 'awaiting_setup_button', version: 1 },
    })).rejects.toThrow('DDB region timeout');

    expect(interaction.showModal).not.toHaveBeenCalled();
    expect(interaction.reply).not.toHaveBeenCalled();
  });

  it('rolls back the flow row when showModal throws after transitionFlow committed', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'success', version: 2 });
    const showModalErr = new Error('Unknown interaction (token expired during ACK)');
    const interaction = makeButtonInteraction({
      showModal: jest.fn().mockRejectedValue(showModalErr),
    });

    await handleSetupButton(interaction, {
      flow_id: '0:1#guild-1#ch-1#user-1',
      row: { stage: 'awaiting_setup_button', version: 1 },
    });

    expect(interaction.showModal).toHaveBeenCalledTimes(1);
    expect(mockDeleteFlow).toHaveBeenCalledWith(
      '0:1#guild-1#ch-1#user-1',
      {
        stage: 'awaiting_setup_modal',
        reason: 'abort',
        expectedVersion: 2,
      },
    );
    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringContaining('please run `/qurl setup` again'),
      }),
    );
  });
});

describe('SETUP_API_KEY_REGEX shape', () => {
  const { SETUP_API_KEY_REGEX, SETUP_API_KEY_MIN_LENGTH, SETUP_API_KEY_MAX_LENGTH } = _test;

  test.each([
    'lv_live_abcdefghijklmnopqrstuvwxyz12',
    'lv_test_abcdefghijklmnopqrstuvwxyz12',
    'lv_live_aaaaaaaaaaaaaaaaaaaa',                          // exactly 20 chars in suffix
    'lv_test_AaBbCcDdEeFfGgHh-1234_5',                       // mixed-case + - + _
  ])('accepts %s', (key) => {
    expect(SETUP_API_KEY_REGEX.test(key)).toBe(true);
  });

  test.each([
    '',
    'live_abcdefghijklmnopqrstuvwxyz12',                     // missing lv_ prefix
    'lv_sandbox_abcdefghijklmnopqrstuvwxyz12',               // unknown family
    'lv_live_short',                                          // < 20-char suffix
    'lv_live_abcdefghijklmnopqrst!!!!!',                     // disallowed char
    'lv_live_abcdefghijklmnopqrstuvwxyz12 ',                 // trailing whitespace
    'LV_LIVE_abcdefghijklmnopqrstuvwxyz12',                  // wrong case on prefix
  ])('rejects %s', (key) => {
    expect(SETUP_API_KEY_REGEX.test(key)).toBe(false);
  });

  it('min/max length constants form a coherent lockstep with the regex', () => {
    expect(SETUP_API_KEY_MIN_LENGTH).toBe(28);
    expect(SETUP_API_KEY_MAX_LENGTH).toBeGreaterThan(SETUP_API_KEY_MIN_LENGTH);
    const atFloor = 'lv_live_' + 'a'.repeat(SETUP_API_KEY_MIN_LENGTH - 'lv_live_'.length);
    expect(atFloor.length).toBe(SETUP_API_KEY_MIN_LENGTH);
    expect(SETUP_API_KEY_REGEX.test(atFloor)).toBe(true);
  });
});

describe('handleSetupModal (dispatcher path)', () => {
  const { handleSetupModal } = require('../src/commands');
  const VALID_KEY = 'lv_live_abcdefghijklmnopqrstuvwxyz12';

  function makeModalInteraction(overrides = {}) {
    return {
      user: { id: 'user-1' },
      guildId: 'guild-1',
      channelId: 'ch-1',
      fields: {
        getTextInputValue: jest.fn(() => VALID_KEY),
      },
      deferReply: jest.fn().mockResolvedValue(undefined),
      editReply: jest.fn().mockResolvedValue(undefined),
      reply: jest.fn().mockResolvedValue(undefined),
      ...overrides,
    };
  }

  let originalFetch;
  beforeAll(() => {
    originalFetch = global.fetch;
  });
  beforeEach(() => {
    mockDeleteFlow.mockResolvedValue({ deleted: true });
    global.fetch = jest.fn().mockResolvedValue({ ok: true, status: 200 });
  });
  afterAll(() => {
    global.fetch = originalFetch;
  });

  it('validates key + persists + replies success', async () => {
    const interaction = makeModalInteraction();
    await handleSetupModal(interaction, { flow_id: '0:1#guild-1#ch-1#user-1' });

    expect(mockDeleteFlow).toHaveBeenCalledWith(
      '0:1#guild-1#ch-1#user-1',
      { stage: 'awaiting_setup_modal', reason: 'terminal' },
    );
    expect(global.fetch).toHaveBeenCalledTimes(1);
    expect(mockDb.setGuildApiKey).toHaveBeenCalledWith(
      'guild-1', VALID_KEY, 'user-1',
    );
    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringContaining('qURL is now configured'),
      }),
    );
  });

  it('skips work when deleteFlow loses dedup race', async () => {
    mockDeleteFlow.mockResolvedValueOnce({ deleted: false });
    const interaction = makeModalInteraction();

    await handleSetupModal(interaction, { flow_id: '0:1#guild-1#ch-1#user-1' });

    expect(global.fetch).not.toHaveBeenCalled();
    expect(mockDb.setGuildApiKey).not.toHaveBeenCalled();
    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringMatching(/expired or was already processed/),
      }),
    );
  });

  it('rejects malformed API key with rerun hint', async () => {
    const interaction = makeModalInteraction({
      fields: { getTextInputValue: jest.fn(() => 'short-bad-key') },
    });

    await handleSetupModal(interaction, { flow_id: '0:1#guild-1#ch-1#user-1' });

    expect(global.fetch).not.toHaveBeenCalled();
    expect(mockDb.setGuildApiKey).not.toHaveBeenCalled();
    const replyArg = interaction.reply.mock.calls.at(-1)[0];
    expect(replyArg.content).toContain('Invalid API key format');
    expect(replyArg.content).toContain('Run `/qurl setup` again');
  });

  it('surfaces 401 from qURL API as invalid-key message', async () => {
    global.fetch = jest.fn().mockResolvedValue({ ok: false, status: 401 });
    const interaction = makeModalInteraction();

    await handleSetupModal(interaction, { flow_id: '0:1#guild-1#ch-1#user-1' });

    expect(mockDb.setGuildApiKey).not.toHaveBeenCalled();
    const replyArg = interaction.editReply.mock.calls.at(-1)[0];
    expect(replyArg.content).toContain('Double-check your key');
    expect(replyArg.content).not.toMatch(/format/);
    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringContaining('Invalid API key'),
      }),
    );
  });

  it('redacts network-error details from user-facing reply', async () => {
    global.fetch = jest.fn().mockRejectedValue(
      new Error('connect ECONNREFUSED 10.0.0.5:8080'),
    );
    const interaction = makeModalInteraction();

    await handleSetupModal(interaction, { flow_id: '0:1#guild-1#ch-1#user-1' });

    expect(mockDb.setGuildApiKey).not.toHaveBeenCalled();
    const replyContent = interaction.editReply.mock.calls.at(-1)[0].content;
    expect(replyContent).not.toMatch(/10\.0\.0\.5/);
    expect(replyContent).not.toMatch(/ECONNREFUSED/);
    expect(replyContent).toContain('Could not validate key');
  });

  it('surfaces non-2xx non-401 as generic API error', async () => {
    global.fetch = jest.fn().mockResolvedValue({ ok: false, status: 503 });
    const interaction = makeModalInteraction();

    await handleSetupModal(interaction, { flow_id: '0:1#guild-1#ch-1#user-1' });

    expect(mockDb.setGuildApiKey).not.toHaveBeenCalled();
    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringContaining('503'),
      }),
    );
  });

  it('swallows Discord errors on the post-persist success reply', async () => {
    const interaction = makeModalInteraction();
    const { SETUP_SUCCESS_MSG } = _test;
    let successBranchInvoked = false;
    interaction.editReply = jest.fn().mockImplementation(async (arg) => {
      if (arg?.content === SETUP_SUCCESS_MSG) {
        successBranchInvoked = true;
        throw new Error('Unknown interaction (token expired)');
      }
      return undefined;
    });

    await expect(
      handleSetupModal(interaction, { flow_id: '0:1#guild-1#ch-1#user-1' })
    ).resolves.not.toThrow();

    expect(mockDb.setGuildApiKey).toHaveBeenCalled();
    expect(successBranchInvoked).toBe(true);
    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: SETUP_SUCCESS_MSG }),
    );
  });

  it('propagates deferReply throw after deleteFlow (flow row already gone, key never persisted)', async () => {
    const deferErr = new Error('Unknown interaction (token expired)');
    const interaction = makeModalInteraction({
      deferReply: jest.fn().mockRejectedValue(deferErr),
    });

    await expect(
      handleSetupModal(interaction, { flow_id: '0:1#guild-1#ch-1#user-1' })
    ).rejects.toThrow('Unknown interaction');

    expect(mockDeleteFlow).toHaveBeenCalledWith(
      '0:1#guild-1#ch-1#user-1',
      { stage: 'awaiting_setup_modal', reason: 'terminal' },
    );
    expect(global.fetch).not.toHaveBeenCalled();
    expect(mockDb.setGuildApiKey).not.toHaveBeenCalled();
  });
});

describe('MAP_COMMAND_ENABLED=false (flag-off behavior)', () => {
  const { mockSearchPlaces } = require('./helpers/places-mock');

  it('SETUP_SUCCESS_MSG omits /qurl map', () => {
    expect(_test.SETUP_SUCCESS_MSG).not.toContain('/qurl map');
    expect(_test.SETUP_SUCCESS_MSG).toContain('/qurl send');
  });

  it('/qurl help reply omits /qurl map references', async () => {
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'help'),
      },
    });
    await handleCommand(interaction);
    const replyArg = interaction.reply.mock.calls.find(([arg]) => typeof arg?.content === 'string')?.[0];
    expect(replyArg).toBeDefined();
    expect(replyArg.content).not.toContain('/qurl map');
    expect(replyArg.content).toContain('/qurl send');
    expect(replyArg.content).toContain('qURL Bot — Help');
    expect(replyArg.content).toContain('Share files securely');
    expect(replyArg.content).not.toContain('Share resources securely');
  });

  it('dispatcher replies with QURL_MAP_DISABLED_REPLY for /qurl map (stale-client safety net)', async () => {
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'map'),
      },
    });
    await handleCommand(interaction);
    expect(interaction.reply).toHaveBeenCalledWith({
      content: _test.QURL_MAP_DISABLED_REPLY,
      ephemeral: true,
    });
    expect(interaction.deferReply).not.toHaveBeenCalled();
  });

  it('dispatcher replies with QURL_DETECT_DISABLED_REPLY for /qurl detect (stale-client safety net)', async () => {
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'detect'),
      },
    });
    await handleCommand(interaction);
    expect(interaction.reply).toHaveBeenCalledWith({
      content: _test.QURL_DETECT_DISABLED_REPLY,
      ephemeral: true,
    });
    expect(interaction.deferReply).not.toHaveBeenCalled();
  });

  it('autocomplete for /qurl map location does NOT call searchPlaces (Places quota safety)', async () => {
    mockSearchPlaces.mockClear();
    const interaction = makeInteraction({
      commandName: 'qurl',
      isAutocomplete: jest.fn(() => true),
      isChatInputCommand: jest.fn(() => false),
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'map'),
        getFocused: jest.fn(() => ({ name: 'location', value: 'Eiffel Tower' })),
      },
    });
    await handleCommand(interaction);
    expect(interaction.respond).toHaveBeenCalledWith([]);
    expect(mockSearchPlaces).not.toHaveBeenCalled();
  });

  it('dispatcher replies with rename hint for stale `/qurl file` submissions (post-rename to /qurl send)', async () => {
    const interaction = makeInteraction({
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'file'),
      },
    });
    await handleCommand(interaction);
    expect(interaction.reply).toHaveBeenCalledWith({
      content: expect.stringMatching(/`\/qurl file` has been renamed to `\/qurl send`/),
      ephemeral: true,
    });
    expect(interaction.deferReply).not.toHaveBeenCalled();
  });

  it('stale /qurl map in an unconfigured guild hits disabled reply BEFORE the API-key gate (routing order)', async () => {
    const configMock = require('../src/config');
    const origQurlApiKey = configMock.QURL_API_KEY;
    configMock.QURL_API_KEY = '';
    mockDb.getGuildApiKey.mockResolvedValueOnce(null);
    try {
      const interaction = makeInteraction({
        options: {
          ...makeInteraction().options,
          getSubcommand: jest.fn(() => 'map'),
        },
      });
      await handleCommand(interaction);
      const allReplies = interaction.reply.mock.calls.map(([arg]) => arg?.content || '');
      expect(allReplies).toEqual([_test.QURL_MAP_DISABLED_REPLY]);
    } finally {
      configMock.QURL_API_KEY = origQurlApiKey;
    }
  });
});

describe('autocomplete handling', () => {
  it('routes autocomplete to handleAutocomplete (responds with empty for non-/qurl/map/location focuses)', async () => {
    const interaction = makeInteraction({
      commandName: 'qurl',
      isAutocomplete: jest.fn(() => true),
      isChatInputCommand: jest.fn(() => false),
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'help'),
        getFocused: jest.fn(() => ({ name: 'location', value: 'test query' })),
      },
      user: { id: 'autocomplete-user', username: 'TestUser' },
    });
    await handleCommand(interaction);

    expect(interaction.respond).toHaveBeenCalledWith([]);
  });
});
