/**
 * Comprehensive tests for src/commands.js — covers buildDeliveryEmbed,
 * the send-pipeline back-half, monitorLinkStatus, buildConfirmMsg,
 * handleRevoke, revokeAllLinks, handleCommand, and all slash command
 * execute() functions.
 */

// ---------------------------------------------------------------------------
// Mock setup — BEFORE requiring any modules
// ---------------------------------------------------------------------------

jest.mock('../src/config', () => ({
  QURL_API_KEY: 'test-api-key',
  QURL_ENDPOINT: 'https://api.test.local',
  CONNECTOR_URL: 'https://connector.test.local',
  GOOGLE_MAPS_API_KEY: 'test-google-key',
  // /qurl map feature toggle — explicitly false here so the flag-off
  // describe block below tests the documented production default. A
  // missing key would *also* read as falsy (the bot's `=== 'true'`
  // parser), but a future refactor that flipped the default-on
  // semantics would silently keep these tests green; the explicit
  // value pins the contract.
  MAP_COMMAND_ENABLED: false,
  QURL_SEND_COOLDOWN_MS: 30000,
  QURL_SEND_MAX_RECIPIENTS: 50,
  DATABASE_PATH: ':memory:',
  PENDING_LINK_EXPIRY_MINUTES: 30,
  ADMIN_USER_IDS: ['admin-1'],
  BASE_URL: 'http://localhost:3000',
  GUILD_ID: 'guild-1',
  SHARD_ID: '0:1',
  isMultiTenant: false,
  // This suite exercises every slash command (both /qurl and the OpenNHP
  // ones). registerCommands + handleCommand filter to the customer-safe
  // allowlist unless config.isOpenNHPActive is true — set it here to
  // keep the full-command coverage. The flag=false dispatch-filter
  // behavior is covered in multi-tenant.test.js.
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

jest.mock('discord.js', () => {
  // Shared option-builder chainable. Centralized so a new chained
  // method at the discord.js layer (setMaxLength, addChoices, etc.)
  // touches one site for the whole test suite — PR #301 regression
  // surfaced this exact gap when setMaxLength was added.
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
  // Default to "no per-guild key configured" → the revoke + send
  // gates fall through to config.QURL_API_KEY (which is set in
  // the config mock at the top of this file).
  getGuildApiKey: jest.fn().mockResolvedValue(null),
  setGuildApiKey: jest.fn().mockResolvedValue(undefined),
  getRecentSends: jest.fn(() => []),
  getSendResourceIds: jest.fn(() => []),
  getSendItems: jest.fn(() => []),
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

// Shared places-mock — see tests/helpers/places-mock.js.
const { mockPlacesModule } = require('./helpers/places-mock');
jest.mock('../src/places', () => mockPlacesModule);

// flow-state is the DDB-backed harness consumed by /qurl revoke
// post-conversion (PR 5). Mock it here rather than hit DDB.
//
// `supersedeOrCreate` is the consolidated primitive (post-harness-PR)
// used by slash-command paths to open a fresh row, claiming over a
// stale predecessor at the same stage. The harness-internal
// orchestration (createFlow → loadFlow → version-gated deleteFlow →
// retry) is pinned by flow-state.test.js, not here; commands-side
// tests just drive its public return shape.
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
      getSubcommand: jest.fn(() => 'file'),
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

  it('replies "no longer available" for unknown command names (stale-registration path)', async () => {
    // A command name we don't know either doesn't exist globally or is a
    // stale guild-scoped registration from a prior deploy. Either way
    // the user deserves an acknowledgement instead of Discord's
    // "interaction failed" timeout.
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

// Phase 1 monitoring — handleCommand emits a single audit event per
// interaction so the terraform metric filters can derive total /
// failure / per-command latency without needing multiple events. Each
// failure_type maps to a different alarm at the infra layer:
//   - ack_timeout → user-visible "did not respond" cluster (count alarm)
//   - handler_error → backend / dependency degradation (rate alarm)
//   - unknown_command → stale-registration count (informational)
describe('handleCommand — INTERACTION_HANDLED audit emission', () => {
  const { AUDIT_EVENTS } = require('../src/constants');
  let logger;

  beforeEach(() => {
    logger = require('../src/logger');
    logger.audit.mockClear();
  });

  it('emits success=true when command executes cleanly', async () => {
    const interaction = makeInteraction({ commandName: 'stats' });
    await handleCommand(interaction);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.INTERACTION_HANDLED,
      expect.objectContaining({
        command_name: 'stats',
        success: true,
        failure_type: null,
        handler_duration_ms: expect.any(Number),
      }),
    );
  });

  it('emits failure_type=handler_error when execute() throws', async () => {
    const interaction = makeInteraction({ commandName: 'stats' });
    mockDb.getStats.mockImplementationOnce(() => { throw new Error('db crash'); });
    await handleCommand(interaction);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.INTERACTION_HANDLED,
      expect.objectContaining({ command_name: 'stats', success: false, failure_type: 'handler_error' }),
    );
  });

  it('emits failure_type=ack_timeout on Discord 10062 (Unknown interaction)', async () => {
    const interaction = makeInteraction({ commandName: 'stats' });
    const ackErr = Object.assign(new Error('Unknown interaction'), { code: 10062 });
    mockDb.getStats.mockImplementationOnce(() => { throw ackErr; });
    await handleCommand(interaction);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.INTERACTION_HANDLED,
      expect.objectContaining({ command_name: 'stats', success: false, failure_type: 'ack_timeout' }),
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

  it('emits failure_type=reply_failed when stale-registration reply throws non-ack error', async () => {
    // Stale-registration path tries to reply "no longer available" but
    // the reply itself fails for a non-timeout reason (e.g. permission
    // missing in the channel). Tag distinctly from ack_timeout so the
    // dashboard can separate "Discord deadline missed" from "reply
    // call broke for some other reason."
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

  it('preserves sub-millisecond handler_duration_ms (no BigInt-truncation regression)', async () => {
    // Round-3 fix: Number(ns / 1_000_000n) → Number(ns) / 1_000_000.
    // The bigint-division shape would truncate any sub-ms duration to
    // 0 and silently destroy fast-path regression detection. Mock
    // process.hrtime.bigint to return start at 0n and end at 500_000n
    // (= 500 µs delta). handler_duration_ms must be 0.5, NOT 0.
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
    // Pin emission cardinality, not just value. Existing tests use
    // toHaveBeenCalledWith which would still pass on duplicate emits.
    // A future refactor that accidentally splits the event into two
    // emits (e.g. separate "interaction_started" + "interaction_ended"
    // pair) would silently double the alarm count without this assertion.
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
    // Pin the asymmetric precedence rule: in the main path a
    // handler_error tag is preserved over a follow-up reply_failed
    // because the original execute failure is the more meaningful
    // dashboard signal. A future refactor that flips the asymmetry
    // would silently change failure-type attribution; this test
    // catches it.
    const interaction = makeInteraction({
      commandName: 'stats',
      reply: jest.fn().mockRejectedValue(new Error('Missing Permissions')),
    });
    mockDb.getStats.mockImplementationOnce(() => { throw new Error('db crash'); });
    await handleCommand(interaction);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.INTERACTION_HANDLED,
      expect.objectContaining({ command_name: 'stats', success: false, failure_type: 'handler_error' }),
    );
  });

  describe('isAckTimeoutError direct regex coverage', () => {
    const { isAckTimeoutError } = _test;
    test.each([
      // [name, error, expected]
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
      customId: `unlink_confirm_${MOCK_NONCE}`,
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

    mockDb.getLinkedDiscordIds.mockReturnValue(new Set());

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

    mockDb.getLinkedDiscordIds.mockReturnValue(new Set(['u1']));

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
        content: expect.stringContaining('qURL Bot'),
        ephemeral: true,
      }),
    );
  });

  // Positive assertions for the help-text copy. Without these, the only
  // other help-text assertion is `stringContaining('qURL Bot')`, which
  // would stay green if every fix below were reverted. Pinning them here
  // catches accidental regressions on the next edit to this block.
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

    // (1) layerv.ai URL carries the scheme so Discord auto-linkifies it
    expect(content).toContain('https://layerv.ai');
    // (2) self-destruct note covers both access AND expiry
    // PR #134 reworded to singular subject ("a one-time link that
    // self-destructs..."); the regex tolerates both verb forms so a
    // future copy tweak doesn't reflexively break this.
    expect(content).toMatch(/self-destructs? on first access.*expiry elapses/);
    // (3) Terms block disambiguates "protected resource" from "qURL"
    expect(content).toContain('protected resource');
    expect(content).toContain('access link');
    // (4) Help text doesn't leak internal jargon. The "Large servers"
    // section explains the role-fanout caveat to end-users without
    // naming the underlying GUILD_PRESENCES intent.
    expect(content).not.toContain('GUILD_PRESENCES');
  });
});

// The slash command writes a flow_state row to DDB and renders the
// select menu; the actual revoke executes on the menu's selection
// event in the dispatcher path. Tests split accordingly — "menu is
// rendered" lives here, "revoke executes" lives in the
// `handleRevokeSelect (dispatcher)` describe below.
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
    // No flow row should be opened when there's nothing to revoke —
    // an early no-op shouldn't poison the SLI's create-count.
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

    // Menu was rendered to the user.
    const menuCall = interaction.editReply.mock.calls.find(
      (c) => c[0]?.components?.length > 0,
    );
    expect(menuCall).toBeDefined();
    expect(menuCall[0].content).toMatch(/Select a send to revoke/);
  });

  it('renders the menu when supersedeOrCreate claims the slot from a stale predecessor', async () => {
    // supersedeOrCreate encapsulates the create → load → delete →
    // retry orchestration. From the caller's view, a successful
    // claim looks identical to a fresh create — both return
    // { created: true, version: ... }. The harness-internal
    // orchestration is pinned by flow-state.test.js, not here.
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
    // Menu rendered after supersede claim.
    const menuCall = interaction.editReply.mock.calls.find(
      (c) => c[0]?.components?.length > 0,
    );
    expect(menuCall).toBeDefined();
  });

  it('names a sibling setup-modal flow when revoke supersede cannot claim', async () => {
    // supersedeOrCreate returns the surviving row when a non-revoke
    // flow owns this flow_id. The handler resolves the user-visible
    // wording via the dispatcher's siblingMessageForStage registry
    // (populated at registerFlow time).
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

    // Sibling-flow message names the in-flight setup modal — not
    // a generic "could not start" fallback.
    const siblingCall = interaction.editReply.mock.calls.find(
      (c) => /qurl setup/.test(c[0]?.content || ''),
    );
    expect(siblingCall).toBeDefined();
  });

  it('names a sibling setup-button flow when revoke supersede finds one in the channel', async () => {
    // Cross-flow collision: /qurl revoke colliding with an unclicked
    // /qurl setup button at the same flow_id. The setup-button stage
    // has its own registered siblingMessage (different from modal),
    // so revoke sees the "click the button or wait" wording rather
    // than the generic "try again."
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
    // No siblingMessage registered for this fictional stage — the
    // handler should NOT invent wording, just fall through to the
    // generic recoverable message.
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

// Dispatcher-side tests for handleRevokeSelect — the post-conversion
// execution path. Direct-invocation tests rather than going through
// the dispatcher itself; the routing layer is covered in
// flow-dispatch.test.js.
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
    // A duplicate event (SQS redelivery in the future worker tier,
    // or a Discord double-dispatch today) — only the worker whose
    // conditional delete succeeds proceeds. The loser must NOT
    // touch the qURL API.
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
    // Drop the fallback as well — emulates an admin unsetting the
    // key between revoke-init and revoke-execute.
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

  it('reports a partial revoke (some links already opened)', async () => {
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
});

// /qurl setup post-conversion (PR 6): legacy modal-paste path is now
// button-first. The slash command writes a flow row + replies with a
// button; clicking the button (dispatcher path) transitions the flow
// and shows the modal; submitting the modal (dispatcher path)
// validates the key + persists + deletes the flow.
describe('/qurl setup subcommand (legacy modal-paste path)', () => {
  const originalKEK = process.env.KEY_ENCRYPTION_KEY;
  const originalAuthDomain = process.env.AUTH0_DOMAIN;
  beforeAll(() => {
    process.env.KEY_ENCRYPTION_KEY = '0'.repeat(64);
    // Force the legacy path by clearing any Auth0 hints. The config
    // mock at the top of this file doesn't define AUTH0_* and we
    // rely on `config.isQurlOAuthConfigured` being falsy.
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
    // supersedeOrCreate encapsulates the create → load →
    // version-gated delete → retry orchestration. From the caller's
    // view a successful claim is identical to a fresh create —
    // { created: true, version: ... }. The internal orchestration
    // is pinned by flow-state.test.js, not here.
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
    // supersedeOrCreate cannot claim because the surviving row is
    // at awaiting_setup_modal (different stage). It returns the
    // surviving row; the dispatcher's siblingMessageForStage
    // registry resolves the modal-open wording. The registry is
    // populated at registerFlow time — confirmed by this test.
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
    // If the surviving row is awaiting_revoke_select the user sees
    // actionable wording naming the revoke menu instead of generic
    // "try again" which would loop until the revoke's TTL fires.
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
    // supersedeOrCreate already retried internally — a thrown
    // exception at this layer is a real DDB/IAM failure. The
    // handler catches it (one shared try-envelope) and surfaces
    // "could not start" rather than letting it propagate to
    // handleCommand's generic envelope, so the user sees an
    // actionable rerun hint instead of "an error executing this
    // command."
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
    // Pin the modal-stage TTL window against the production constant
    // (NOT a duplicated local literal — if the constant is tuned, a
    // stale local would silently pass).
    const { SETUP_BUTTON_TTL_SECONDS, SETUP_MODAL_TTL_SECONDS } = _test;
    const nowSec = Math.floor(Date.now() / 1000);
    // Lower bound allows ~50s of clock drift / test execution time;
    // upper bound is the modal TTL itself.
    expect(transitionArgs[2].set_expires_at).toBeGreaterThan(nowSec + SETUP_MODAL_TTL_SECONDS - 50);
    expect(transitionArgs[2].set_expires_at).toBeLessThanOrEqual(nowSec + SETUP_MODAL_TTL_SECONDS);
    // `extended: true` audit-flag pin: the new expires_at must
    // exceed the original button-stage TTL window. flow-state.js
    // computes `extended = set_expires_at > priorExpires`; the
    // prior expires_at on a fresh row is at most
    // `now + SETUP_BUTTON_TTL_SECONDS`, so any value strictly
    // greater than that guarantees extended=true at the audit
    // emission.
    expect(transitionArgs[2].set_expires_at).toBeGreaterThan(nowSec + SETUP_BUTTON_TTL_SECONDS);
    // Pin: button-stage transition must NOT write a payload. The
    // setup flow carries no encrypted state — the key itself
    // arrives in interaction.fields on the modal submit, not in
    // flow_state. A future refactor that accidentally persists
    // anything sensitive here would trip this assertion.
    expect(transitionArgs[2].payload).toBeUndefined();

    expect(interaction.showModal).toHaveBeenCalledTimes(1);
    expect(interaction.reply).not.toHaveBeenCalled();

    // Pin the API-key TextInput length bounds. The 28-char floor
    // matches the regex's minimum (8-char prefix + 20-char suffix);
    // the 64-char ceiling carries the TODO(upstream-rebrand) lockstep
    // marker. A regression that drops either bound — exactly the
    // risk the marker is meant to surface — would now trip this
    // assertion instead of sliding through green.
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
    // transitionFlow throws on non-CCFE failures (DDB outage, pre-
    // read errors); it does not synthesize a `result: 'error'`
    // return value. The handler must let the throw propagate so the
    // dispatcher's universal safety net catches it — wrapping it
    // here would swallow the audit signal flow-state already
    // emitted for the failure.
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
    // The transitionFlow commits the row to `awaiting_setup_modal`
    // before showModal fires. If showModal throws (Discord token
    // expiry, REST blip), leaving the row in that stage would force
    // the admin to wait out the full TTL — `/qurl setup` rerun
    // would see the supersede peek find awaiting_setup_modal and
    // surface the misleading "you already have a modal open"
    // wording. Recovery requires the handler to delete the row.
    //
    // expectedVersion gates the rollback on the specific version
    // the transitionFlow just committed (post-bump = 2 from initial
    // version=1). A concurrent supersede that advanced the row past
    // version 2 would fail this delete by design — at that point
    // the row at flow_id is no longer ours to clean up.
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
    // Best-effort reply attempted (may fail itself; that's logged).
    expect(interaction.reply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringContaining('please run `/qurl setup` again'),
      }),
    );
  });
});

// Direct shape coverage of SETUP_API_KEY_REGEX. Round-tripping
// through the handler (via VALID_KEY / 'short-bad-key') exercises
// the format-rejection PATH; this pins the format itself, so adding
// a new prefix family (lv_sandbox_, lv_internal_, etc.) requires a
// deliberate test update rather than silently passing the existing
// handler-level coverage.
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
    // MIN = 28 = 8 (lv_live_/lv_test_) + 20 (regex suffix floor).
    // MAX = 64 — defense-in-depth cap, well above MIN. Adding a
    // new prefix family that changes the prefix length would have
    // to bump MIN to stay coherent.
    expect(SETUP_API_KEY_MIN_LENGTH).toBe(28);
    expect(SETUP_API_KEY_MAX_LENGTH).toBeGreaterThan(SETUP_API_KEY_MIN_LENGTH);
    // The regex itself accepts strings at the MIN boundary.
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
        // Covers both TTL'd and already-processed cases (deleted:false
        // collapses both into the same branch).
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
    // The rerun hint is what closes the UX loop — the admin's flow
    // row was already deleted by deleteFlow, so without this hint
    // they'd be stuck without a path forward.
    expect(replyArg.content).toContain('Run `/qurl setup` again');
  });

  it('surfaces 401 from qURL API as invalid-key message', async () => {
    global.fetch = jest.fn().mockResolvedValue({ ok: false, status: 401 });
    const interaction = makeModalInteraction();

    await handleSetupModal(interaction, { flow_id: '0:1#guild-1#ch-1#user-1' });

    expect(mockDb.setGuildApiKey).not.toHaveBeenCalled();
    // Pin the 401-specific phrasing — the format-rejection branch
    // also includes "Invalid API key", so a substring-only match
    // could silently route through the wrong branch if VALID_KEY
    // ever drifts. `Double-check your key` is unique to the 401
    // path's blurb.
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
    // Internal hostnames or IPs in err.message must NOT leak to
    // Discord. The pre-conversion code carried this guarantee and
    // the dispatcher path preserves it.
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
    // The key is already saved by the time the success editReply
    // fires. If editReply throws (Discord interaction-token expiry,
    // transient API blip), the throw must NOT propagate to the
    // dispatcher's universal safety net — that net replies "run
    // the command again", which would be misleading after a
    // successful persist. Admin can confirm via /qurl status.
    const interaction = makeModalInteraction();
    // First editReply (success path) throws; verify the handler
    // swallows it. Use mockImplementation rather than mockRejected
    // so the prior editReply assertions on other tests aren't
    // disturbed.
    // Identify the success-path editReply by exact match against
    // the production constant — no string drift on copy polish.
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

    // setGuildApiKey did commit before the editReply threw.
    expect(mockDb.setGuildApiKey).toHaveBeenCalled();
    // Guard against a future refactor that augments the success
    // editReply with extra fields (components, embeds, etc.) — the
    // strict `arg.content === SETUP_SUCCESS_MSG` mock would silently
    // miss the new shape and the test would pass green without
    // exercising the swallow logic. Pin: the throw branch DID fire.
    expect(successBranchInvoked).toBe(true);
    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({ content: SETUP_SUCCESS_MSG }),
    );
  });

  it('propagates deferReply throw after deleteFlow (flow row already gone, key never persisted)', async () => {
    // Pin the post-deleteFlow / pre-deferReply window. If
    // deferReply throws (token expired during the DDB round-trip),
    // the flow row is already gone but the qURL API call never
    // fires and setGuildApiKey never runs. Admin sees Discord's
    // generic "interaction failed" and reruns /qurl setup; the
    // supersede path finds no row (deleted) so a fresh button
    // renders cleanly. The throw itself propagates to the
    // dispatcher's safety net.
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

describe('handleCommand — autocomplete', () => {
  it('responds empty for /qurl map location autocomplete (flag-off short-circuit)', async () => {
    // This file's config mock leaves MAP_COMMAND_ENABLED unset — the
    // bot's production default. handleAutocomplete's flag-off guard
    // short-circuits to respond([]) before searchPlaces. The contract
    // here is that respond() IS called (with []), where pre-flag this
    // path would have invoked Places.
    const interaction = makeInteraction({
      commandName: 'qurl',
      isAutocomplete: jest.fn(() => true),
      isChatInputCommand: jest.fn(() => false),
      options: {
        ...makeInteraction().options,
        getSubcommand: jest.fn(() => 'map'),
        getFocused: jest.fn(() => ({ name: 'location', value: 'Eif' })),
      },
    });

    await handleCommand(interaction);

    expect(interaction.respond).toHaveBeenCalledWith([]);
  });
});

// Flag-off coverage. The config mock at the top of this file does NOT
// set MAP_COMMAND_ENABLED, so the bot's strict `=== 'true'` parser
// resolves it to false — matching the production default. Every test
// in this block verifies a surface that should be inert when the flag
// is off. The flag-on path is covered by qurl-file-map.test.js (whose
// config mock sets MAP_COMMAND_ENABLED: true).
describe('MAP_COMMAND_ENABLED=false (flag-off behavior)', () => {
  const { mockSearchPlaces } = require('./helpers/places-mock');

  it('SETUP_SUCCESS_MSG omits /qurl map', () => {
    // Built at module load with the flag snapshot. Pin against the
    // production string via _test so a future copy edit can't drift
    // this assertion from reality.
    expect(_test.SETUP_SUCCESS_MSG).not.toContain('/qurl map');
    expect(_test.SETUP_SUCCESS_MSG).toContain('/qurl file');
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
    // Sanity: the help reply is still rendered (we want absence of
    // map, not absence of help). Catches a regression where the
    // entire help branch goes silent.
    expect(replyArg.content).toContain('/qurl file');
    expect(replyArg.content).toContain('qURL Bot — Help');
    // Pin the flag-off `sectionVerb` swap — a regression that drops
    // the conditional verb would render "Share resources" against a
    // map-disabled deploy. Catches the swap in isolation from the
    // overall mapCopy structure.
    expect(replyArg.content).toContain('Share files securely');
    expect(replyArg.content).not.toContain('Share resources securely');
  });

  it('dispatcher replies with QURL_MAP_DISABLED_REPLY for /qurl map (stale-client safety net)', async () => {
    // Discord won't normally route a `map` submission when the
    // subcommand isn't registered, but a stale client carrying the
    // pre-flip command definition can still submit one. The
    // dispatcher's defensive branch turns that into a clean ephemeral
    // instead of falling through to handleQurlMap → Places.
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
    // handleQurlMap defers + then hits Places; if the dispatcher ever
    // routed through it by mistake, deferReply would fire. Negative
    // assertion guards against that regression.
    expect(interaction.deferReply).not.toHaveBeenCalled();
  });

  it('autocomplete for /qurl map location does NOT call searchPlaces (Places quota safety)', async () => {
    // The earlier "responds empty for /qurl map location" test pins
    // the user-visible contract (respond([])). This one pins the
    // operator-cost contract: we don't burn the GOOGLE_MAPS_API_KEY
    // quota on a submit that the dispatcher will reject anyway.
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

  it('stale /qurl map in an unconfigured guild hits disabled reply BEFORE the API-key gate (routing order)', async () => {
    // The most subtle invariant of this PR: API_KEY_GATED_SUBCOMMANDS
    // intentionally OMITS 'map' when the flag is off. If 'map' had
    // stayed in the set, the dispatcher's API_KEY_GATED_SUBCOMMANDS
    // gate would fire "qURL is not configured for this server"
    // BEFORE the dispatch could route to QURL_MAP_DISABLED_REPLY —
    // a stale client in a never-configured guild would see the
    // wrong copy.
    //
    // Setup: empty per-guild key (mockDb default) AND empty global
    // fallback (mutated config). The gate would otherwise fire if
    // 'map' were still in API_KEY_GATED_SUBCOMMANDS; the disabled
    // reply firing instead is the load-bearing assertion.
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
      // Strict shape: exactly one reply call, exactly the disabled
      // copy. A future refactor that re-adds 'map' to
      // API_KEY_GATED_SUBCOMMANDS would either: (a) reply with
      // "qURL is not configured" instead, OR (b) reply twice (gate
      // copy first, then disabled copy on fall-through). Both
      // regressions are caught by the length + value assertion.
      const allReplies = interaction.reply.mock.calls.map(([arg]) => arg?.content || '');
      expect(allReplies).toEqual([_test.QURL_MAP_DISABLED_REPLY]);
    } finally {
      configMock.QURL_API_KEY = origQurlApiKey;
    }
  });
});

// `revokeAllLinks` coverage lives in send-pipeline-back-half.test.js
// (direct unit tests) and in the `handleRevokeSelect (dispatcher
// path)` block above (integration through the flow).

// connector and qurl tests that require resetModules are in send-pipeline-helpers.test.js

describe('autocomplete handling', () => {
  it('routes autocomplete to handleAutocomplete (responds with empty for non-/qurl/map/location focuses)', async () => {
    // Contract change: handleCommand now dispatches autocomplete to
    // handleAutocomplete instead of dropping it. For a /qurl autocomplete
    // whose subcommand isn't 'map', the handler responds with [] (clears
    // the dropdown) rather than silently dropping the interaction.
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
