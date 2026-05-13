/**
 * Tests for /qurl file + /qurl map slash commands (PR 7b.2).
 *
 * Covers the new handlers from src/commands.js — handleQurlFile,
 * handleQurlMap, the shared confirm-card pipeline, the flow-dispatch
 * handlers (UserSelect / Send / Cancel) — plus the pure helpers
 * (resolveRecipientUsers, partitionRecipients, selfDestructOptionToSeconds,
 * renderRecipientWarnings, renderConfirmCardContent).
 *
 * Mocks mirror tests/qurl-send-back-half.test.js so both files share
 * a coherent module surface. The new commands are accessed via the
 * `_test` export. Real flow-state is stubbed — handlers are tested
 * against the harness contract, not against DDB itself (flow-state.js
 * has its own spec).
 */

jest.mock('../src/config', () => ({
  QURL_API_KEY: 'test-api-key',
  QURL_ENDPOINT: 'https://api.test.local',
  CONNECTOR_URL: 'https://connector.test.local',
  GOOGLE_MAPS_API_KEY: 'test-google-key',
  QURL_SEND_COOLDOWN_MS: 30000,
  QURL_SEND_MAX_RECIPIENTS: 25,
  DATABASE_PATH: ':memory:',
  PENDING_LINK_EXPIRY_MINUTES: 30,
  ADMIN_USER_IDS: ['admin-1'],
  BASE_URL: 'http://localhost:3000',
  GUILD_ID: 'guild-1',
  SHARD_ID: '0:1',
  isMultiTenant: false,
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

// The discord.js mocks here just need to keep the SlashCommandBuilder
// chain from throwing when commands.js loads. Tests exercise the
// handler functions directly, not the registration path.
jest.mock('discord.js', () => {
  const makeChainable = (extra = {}) => ({
    setCustomId: jest.fn().mockReturnThis(),
    setLabel: jest.fn().mockReturnThis(),
    setEmoji: jest.fn().mockReturnThis(),
    setStyle: jest.fn().mockReturnThis(),
    setURL: jest.fn().mockReturnThis(),
    setTitle: jest.fn().mockReturnThis(),
    setPlaceholder: jest.fn().mockReturnThis(),
    addOptions: jest.fn().mockReturnThis(),
    addChoices: jest.fn().mockReturnThis(),
    setMinValues: jest.fn().mockReturnThis(),
    setMaxValues: jest.fn().mockReturnThis(),
    addComponents: jest.fn().mockReturnThis(),
    setDisabled: jest.fn().mockReturnThis(),
    setMaxLength: jest.fn().mockReturnThis(),
    setRequired: jest.fn().mockReturnThis(),
    setValue: jest.fn().mockReturnThis(),
    setName: jest.fn().mockReturnThis(),
    setDescription: jest.fn().mockReturnThis(),
    ...extra,
  });
  return {
    SlashCommandBuilder: jest.fn().mockImplementation(() => {
      const subBuilder = () => makeChainable({
        addStringOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeChainable()); return this; }),
        addAttachmentOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeChainable()); return this; }),
        addUserOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeChainable()); return this; }),
        addIntegerOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeChainable()); return this; }),
      });
      const builder = {
        setName: jest.fn().mockReturnThis(),
        setDescription: jest.fn().mockReturnThis(),
        addSubcommand: jest.fn(function (fn) { if (typeof fn === 'function') fn(subBuilder()); return builder; }),
        addStringOption: jest.fn().mockReturnThis(),
        addAttachmentOption: jest.fn().mockReturnThis(),
        addUserOption: jest.fn().mockReturnThis(),
        addIntegerOption: jest.fn().mockReturnThis(),
        setDefaultMemberPermissions: jest.fn().mockReturnThis(),
        toJSON: jest.fn().mockReturnValue({}),
      };
      return builder;
    }),
    EmbedBuilder: jest.fn().mockImplementation(() => makeChainable({
      setColor: jest.fn().mockReturnThis(),
      setDescription: jest.fn().mockReturnThis(),
      addFields: jest.fn().mockReturnThis(),
      setFooter: jest.fn().mockReturnThis(),
      setTimestamp: jest.fn().mockReturnThis(),
    })),
    ActionRowBuilder: jest.fn().mockImplementation(() => {
      const row = { components: [], addComponents: jest.fn(function (...args) { row.components.push(...args.flat()); return row; }) };
      return row;
    }),
    ButtonBuilder: jest.fn().mockImplementation(() => makeChainable()),
    ButtonStyle: { Primary: 1, Secondary: 2, Success: 3, Danger: 4, Link: 5 },
    ChannelType: { GuildText: 0, DM: 1, GuildVoice: 2, GuildStageVoice: 13 },
    ComponentType: { Button: 2, StringSelect: 3, UserSelect: 5 },
    StringSelectMenuBuilder: jest.fn().mockImplementation(() => makeChainable()),
    UserSelectMenuBuilder: jest.fn().mockImplementation(() => makeChainable()),
    ModalBuilder: jest.fn().mockImplementation(() => makeChainable()),
    TextInputBuilder: jest.fn().mockImplementation(() => makeChainable()),
    TextInputStyle: { Short: 1, Paragraph: 2 },
    AttachmentBuilder: jest.fn().mockImplementation((buf, opts) => ({ buf, name: opts?.name })),
    PermissionFlagsBits: { ManageRoles: 1n, Administrator: 8n, ManageGuild: 32n },
  };
});

const mockDb = {
  recordQURLSendBatch: jest.fn(),
  updateSendDMStatus: jest.fn(),
  getRecentSends: jest.fn(() => []),
  getSendResourceIds: jest.fn(() => []),
  getSendItems: jest.fn(() => []),
  markSendRevoked: jest.fn(),
  getSendConfig: jest.fn(),
  saveSendConfig: jest.fn(),
  getGuildApiKey: jest.fn(),
  getGuildConfig: jest.fn(),
};
jest.mock('../src/database', () => mockDb);

jest.mock('../src/discord', () => ({
  sendDM: jest.fn().mockResolvedValue(true),
  getChannelMembers: jest.fn(),
}));

jest.mock('../src/utils/admin', () => ({
  requireAdmin: jest.fn(async () => true),
  isAdmin: jest.fn(() => true),
}));

jest.mock('../src/connector', () => ({
  downloadAndUpload: jest.fn(),
  reUploadBuffer: jest.fn(),
  mintLinks: jest.fn(),
  uploadJsonToConnector: jest.fn(),
  isAllowedSourceUrl: (url) => typeof url === 'string' && url.startsWith('https://cdn.discordapp.com'),
}));

jest.mock('../src/qurl', () => ({
  createOneTimeLink: jest.fn(),
  deleteLink: jest.fn(),
  getResourceStatus: jest.fn(),
}));

jest.mock('../src/places', () => ({ searchPlaces: jest.fn().mockResolvedValue([]) }));

// Flow-state stubs. Each test overrides per-call to assert on the
// supersedeOrCreate/transitionFlow/deleteFlow contracts.
const mockCreateFlow = jest.fn();
const mockLoadFlow = jest.fn();
const mockTransitionFlow = jest.fn();
const mockDeleteFlow = jest.fn();
const mockSupersedeOrCreate = jest.fn();
jest.mock('../src/flow-state', () => ({
  createFlow: (...a) => mockCreateFlow(...a),
  loadFlow: (...a) => mockLoadFlow(...a),
  transitionFlow: (...a) => mockTransitionFlow(...a),
  deleteFlow: (...a) => mockDeleteFlow(...a),
  supersedeOrCreate: (...a) => mockSupersedeOrCreate(...a),
}));

// flow-dispatch is a real module — we want registerFlow's
// idempotent-failure guard to fire on test re-loads.
const commands = require('../src/commands');
const { _test } = commands;
const logger = require('../src/logger');
const {
  handleQurlFile,
  handleQurlMap,
  resolveRecipientUsers,
  partitionRecipients,
  selfDestructOptionToSeconds,
  renderRecipientWarnings,
  renderConfirmCardContent,
  SEND_STAGE_AWAITING_CONFIRM,
  SEND_USER_SELECT_CUSTOM_ID,
  SEND_CONFIRM_SEND_CUSTOM_ID,
  SEND_CONFIRM_CANCEL_CUSTOM_ID,
  SEND_FLOW_TTL_SECONDS,
  SELF_DESTRUCT_NO_TIMER_CHOICE,
  isOnCooldown,
  clearCooldown,
  sendCooldowns,
  executeSendPipeline,
} = _test;

// Flow-dispatch handlers live at module top-level (consumed by
// registerFlow at boot). _test only exports things that aren't already
// at the top level.
const {
  handleSendUserSelect,
  handleSendConfirmClick,
  handleSendCancelClick,
} = commands;

// ──────────────────────────────────────────────────────────────
// Test helpers
// ──────────────────────────────────────────────────────────────

const SENDER_ID = '900000000000000001';

function makeUser(id, { bot = false, username = `user${id.slice(-3)}` } = {}) {
  return { id, bot, username };
}

function makeInteraction({
  guildId = 'guild-1',
  channelId = 'ch-1',
  userId = SENDER_ID,
  options = {},
  guildMembers = {},
  guildFetchByID = null,
} = {}) {
  // memberCache: id → { user: {...} } when present
  const memberCache = new Map();
  for (const [id, attrs] of Object.entries(guildMembers)) {
    memberCache.set(id, { user: makeUser(id, attrs) });
  }
  const guild = guildId ? {
    members: {
      cache: memberCache,
      fetch: jest.fn(async (id) => {
        if (guildFetchByID && Object.prototype.hasOwnProperty.call(guildFetchByID, id)) {
          const r = guildFetchByID[id];
          if (r === 'unknown') {
            const err = new Error('Unknown Member'); err.code = 10007; throw err;
          }
          if (r === 'ratelimit') {
            const err = new Error('rate limited'); err.code = 429; throw err;
          }
          return { user: r };
        }
        const err = new Error('Unknown Member'); err.code = 10007; throw err;
      }),
    },
    roles: { cache: new Map() },
    channels: { cache: new Map() },
    id: guildId,
  } : null;

  const optGetString = jest.fn((name) => {
    const v = options[name];
    return v === undefined ? null : v;
  });
  const optGetAttachment = jest.fn((name) => options[name] ?? null);

  return {
    user: { id: userId, username: 'Sender' },
    guildId,
    channelId,
    guild,
    member: { displayName: 'Sender' },
    options: {
      getString: optGetString,
      getAttachment: optGetAttachment,
      getSubcommand: () => options._sub || 'file',
    },
    reply: jest.fn().mockResolvedValue(undefined),
    editReply: jest.fn().mockResolvedValue(undefined),
    deferReply: jest.fn().mockResolvedValue(undefined),
    followUp: jest.fn().mockResolvedValue(undefined),
    update: jest.fn().mockResolvedValue(undefined),
    deferUpdate: jest.fn().mockResolvedValue(undefined),
    replied: false,
    deferred: false,
  };
}

const VALID_ATTACHMENT = Object.freeze({
  url: 'https://cdn.discordapp.com/attachments/1/2/x.png',
  name: 'x.png',
  contentType: 'image/png',
  size: 1024,
});

beforeEach(() => {
  jest.clearAllMocks();
  sendCooldowns.clear();
  mockSupersedeOrCreate.mockResolvedValue({ created: true, version: 1 });
  mockDeleteFlow.mockResolvedValue({ deleted: true });
  mockTransitionFlow.mockResolvedValue({ result: 'ok', version: 2 });
  // mockResolvedValueOnce queues survive `clearAllMocks` — a queued
  // value left unconsumed by a prior test (e.g. an early-return path)
  // would leak into the next test's first call. Drain the queue with
  // mockReset for the mocks where per-test `mockResolvedValueOnce` is
  // the dominant pattern.
  mockDb.getGuildApiKey.mockReset();
});

// ──────────────────────────────────────────────────────────────
// Pure helpers
// ──────────────────────────────────────────────────────────────

describe('selfDestructOptionToSeconds', () => {
  test.each([
    ['none', null],
    [SELF_DESTRUCT_NO_TIMER_CHOICE, null],
    [null, null],
    [undefined, null],
    ['', null],
    ['60', 60],
    ['3600', 3600],
    ['0', null],
    ['-5', null],
    ['NaN', null],
    ['bogus', null],
    ['1.5', 1],
  ])('value=%j → seconds=%j', (input, expected) => {
    expect(selfDestructOptionToSeconds(input)).toBe(expected);
  });
});

describe('partitionRecipients', () => {
  test('drops bots and sender', () => {
    const users = [
      makeUser('100000000000000001'),
      makeUser('100000000000000002', { bot: true }),
      makeUser(SENDER_ID),
      makeUser('100000000000000003'),
    ];
    const r = partitionRecipients(users, SENDER_ID);
    expect(r.valid.map((u) => u.id)).toEqual(['100000000000000001', '100000000000000003']);
    expect(r.droppedBots).toBe(1);
    expect(r.droppedSelf).toBe(1);
  });

  test('all bots returns valid=[]', () => {
    const users = [makeUser('100000000000000001', { bot: true }), makeUser('100000000000000002', { bot: true })];
    const r = partitionRecipients(users, SENDER_ID);
    expect(r.valid).toEqual([]);
    expect(r.droppedBots).toBe(2);
    expect(r.droppedSelf).toBe(0);
  });

  test('only sender returns valid=[]', () => {
    const r = partitionRecipients([makeUser(SENDER_ID)], SENDER_ID);
    expect(r.valid).toEqual([]);
    expect(r.droppedSelf).toBe(1);
  });

  test('empty input', () => {
    expect(partitionRecipients([], SENDER_ID)).toEqual({ valid: [], droppedBots: 0, droppedSelf: 0 });
  });
});

describe('resolveRecipientUsers', () => {
  test('hits guild cache and skips fetch', async () => {
    const int = makeInteraction({
      guildMembers: { '100000000000000001': {}, '100000000000000002': {} },
    });
    const r = await resolveRecipientUsers(int, ['100000000000000001', '100000000000000002']);
    expect(r.users.map((u) => u.id)).toEqual(['100000000000000001', '100000000000000002']);
    expect(r.unresolvedIds).toEqual([]);
    expect(int.guild.members.fetch).not.toHaveBeenCalled();
  });

  test('falls through to fetch on cache miss', async () => {
    const int = makeInteraction({
      guildMembers: {},
      guildFetchByID: { '100000000000000001': makeUser('100000000000000001') },
    });
    const r = await resolveRecipientUsers(int, ['100000000000000001']);
    expect(r.users.map((u) => u.id)).toEqual(['100000000000000001']);
    expect(r.unresolvedIds).toEqual([]);
    expect(int.guild.members.fetch).toHaveBeenCalledWith('100000000000000001');
  });

  test('10007 unknown member → unresolved', async () => {
    const int = makeInteraction({
      guildMembers: {},
      guildFetchByID: { '100000000000000001': 'unknown' },
    });
    const r = await resolveRecipientUsers(int, ['100000000000000001']);
    expect(r.users).toEqual([]);
    expect(r.unresolvedIds).toEqual(['100000000000000001']);
  });

  test('non-10007 error → unresolved + warn logged', async () => {
    const int = makeInteraction({
      guildMembers: {},
      guildFetchByID: { '100000000000000001': 'ratelimit' },
    });
    const r = await resolveRecipientUsers(int, ['100000000000000001']);
    expect(r.unresolvedIds).toEqual(['100000000000000001']);
    expect(logger.warn).toHaveBeenCalledWith(
      'resolveRecipientUsers: members.fetch failed',
      expect.any(Object),
    );
  });

  test('no guild → everything unresolved', async () => {
    const int = makeInteraction({ guildId: null });
    const r = await resolveRecipientUsers(int, ['100000000000000001', '100000000000000002']);
    expect(r.users).toEqual([]);
    expect(r.unresolvedIds).toEqual(['100000000000000001', '100000000000000002']);
  });

  test('mixed cache + fetch + 10007', async () => {
    const int = makeInteraction({
      guildMembers: { '100000000000000001': {} },
      guildFetchByID: {
        '100000000000000002': makeUser('100000000000000002'),
        '100000000000000003': 'unknown',
      },
    });
    const r = await resolveRecipientUsers(int, [
      '100000000000000001', '100000000000000002', '100000000000000003',
    ]);
    expect(r.users.map((u) => u.id).sort()).toEqual(['100000000000000001', '100000000000000002']);
    expect(r.unresolvedIds).toEqual(['100000000000000003']);
  });
});

describe('renderRecipientWarnings', () => {
  test('returns empty when nothing to surface', () => {
    expect(renderRecipientWarnings({
      invalidTokens: [], cappedCount: 0, unresolvedIds: [],
      droppedBots: 0, droppedSelf: 0,
    })).toBe('');
  });

  test('cappedCount line', () => {
    const out = renderRecipientWarnings({
      invalidTokens: [], cappedCount: 3, unresolvedIds: [],
      droppedBots: 0, droppedSelf: 0,
    });
    expect(out).toMatch(/Capped at 25/);
    expect(out).toMatch(/3 recipient/);
  });

  test('invalidTokens code-fenced + cap at 10', () => {
    const tokens = Array.from({ length: 15 }, (_, i) => `bogus${i}`);
    const out = renderRecipientWarnings({
      invalidTokens: tokens, cappedCount: 0, unresolvedIds: [],
      droppedBots: 0, droppedSelf: 0,
    });
    expect(out).toMatch(/```/);
    expect(out).toMatch(/bogus0/);
    expect(out).toMatch(/bogus9/);
    expect(out).not.toMatch(/bogus10/);
    expect(out).toMatch(/\+5 more/);
  });

  test('combines all signals', () => {
    const out = renderRecipientWarnings({
      invalidTokens: ['<#999>'], cappedCount: 2,
      unresolvedIds: ['100000000000000001'],
      droppedBots: 1, droppedSelf: 1,
    });
    expect(out).toMatch(/Capped/);
    expect(out).toMatch(/Could not parse/);
    expect(out).toMatch(/no longer in this server/);
    expect(out).toMatch(/bot/);
    expect(out).toMatch(/yourself/);
  });
});

describe('renderConfirmCardContent', () => {
  const baseProps = {
    resourceType: 'file',
    resourceLabel: 'report.pdf',
    validRecipients: [makeUser('100000000000000001', { username: 'alice' })],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
    warningsBlock: '',
    needsPicker: false,
  };

  test('file path shows file glyph + label', () => {
    const out = renderConfirmCardContent(baseProps);
    expect(out).toMatch(/Sending file/);
    expect(out).toMatch(/report\.pdf/);
    expect(out).toMatch(/Expires/);
    expect(out).toMatch(/24 hours/);
  });

  test('map path shows map glyph + label', () => {
    const out = renderConfirmCardContent({
      ...baseProps,
      resourceType: 'maps',
      resourceLabel: 'Eiffel Tower',
    });
    expect(out).toMatch(/Sending location/);
    expect(out).toMatch(/Eiffel Tower/);
  });

  test('shows recipient preview (first 5) + remainder count', () => {
    const users = Array.from({ length: 7 }, (_, i) => makeUser(`10000000000000000${i + 1}`, { username: `u${i}` }));
    const out = renderConfirmCardContent({ ...baseProps, validRecipients: users });
    expect(out).toMatch(/7 users/);
    expect(out).toMatch(/u0/);
    expect(out).toMatch(/u4/);
    expect(out).toMatch(/\+2 more/);
  });

  test('needsPicker hides recipient summary and prompts to pick', () => {
    const out = renderConfirmCardContent({ ...baseProps, needsPicker: true, validRecipients: [] });
    expect(out).toMatch(/Pick recipients/);
    expect(out).not.toMatch(/^To:/m);
  });

  test('self-destruct line shown when set', () => {
    const out = renderConfirmCardContent({ ...baseProps, selfDestructSeconds: 300 });
    expect(out).toMatch(/Self-destruct/);
  });

  test('personal-message preview cap at 80 chars', () => {
    const long = 'x'.repeat(120);
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: long });
    // 80 chars of x + ellipsis
    expect(out).toMatch(/x{80}…/);
  });

  test('warningsBlock prepended', () => {
    const out = renderConfirmCardContent({ ...baseProps, warningsBlock: '⚠ warned\n\n' });
    expect(out.startsWith('⚠ warned')).toBe(true);
  });

  test('escapes markdown in recipient username', () => {
    const out = renderConfirmCardContent({
      ...baseProps,
      validRecipients: [makeUser('100000000000000001', { username: '**bob**' })],
    });
    expect(out).not.toMatch(/\*\*bob\*\*/);
    expect(out).toMatch(/\\\*\\\*bob\\\*\\\*/);
  });
});

// ──────────────────────────────────────────────────────────────
// handleQurlFile — front half
// ──────────────────────────────────────────────────────────────

describe('handleQurlFile — slash entry', () => {
  test('rejects in DM context', async () => {
    const int = makeInteraction({
      guildId: null,
      options: { attachment: VALID_ATTACHMENT },
    });
    await handleQurlFile(int, 'apikey');
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/in a server/),
      ephemeral: true,
    }));
  });

  test('rejects when attachment.url is not Discord CDN (SSRF gate)', async () => {
    const int = makeInteraction({
      options: { attachment: { ...VALID_ATTACHMENT, url: 'https://evil.com/x.png' } },
    });
    await handleQurlFile(int, 'apikey');
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/source not allowed/),
      ephemeral: true,
    }));
  });

  test('rejects disallowed file type', async () => {
    const int = makeInteraction({
      options: { attachment: { ...VALID_ATTACHMENT, contentType: 'application/x-evil-macroenabled' } },
    });
    await handleQurlFile(int, 'apikey');
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/File type not allowed/),
    }));
  });

  test('rejects file over size cap', async () => {
    const int = makeInteraction({
      options: { attachment: { ...VALID_ATTACHMENT, size: 999_999_999 } },
    });
    await handleQurlFile(int, 'apikey');
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/too large/),
    }));
  });

  test('happy path with recipients string — supersedeOrCreate called + confirm card rendered', async () => {
    const u1 = '100000000000000001';
    const u2 = '100000000000000002';
    const int = makeInteraction({
      options: {
        attachment: VALID_ATTACHMENT,
        recipients: `<@${u1}> <@${u2}>`,
      },
      guildMembers: { [u1]: {}, [u2]: {} },
    });
    await handleQurlFile(int, 'apikey');
    expect(int.deferReply).toHaveBeenCalled();
    expect(mockSupersedeOrCreate).toHaveBeenCalledWith(expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM,
      ttl_seconds: SEND_FLOW_TTL_SECONDS,
    }));
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.resourceType).toBe('file');
    expect(payload.recipientIds.sort()).toEqual([u1, u2]);
    expect(payload.expiresIn).toBe('24h');
    expect(payload.selfDestructSeconds).toBeNull();
    expect(payload.personalMessage).toBeNull();
    expect(int.editReply).toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Sending file/);
    expect(reply.content).toMatch(/x\.png/);
    expect(reply.components.length).toBeGreaterThan(0);
  });

  test('happy path without recipients → confirm card with picker', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT },
    });
    await handleQurlFile(int, 'apikey');
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Pick recipients/);
  });

  test('all recipients are bots → ephemeral error, no flow row', async () => {
    // recipient-parser silently strips bots at parse time (src/recipient-parser.js:214),
    // so `partitionRecipients` never sees them. The handler's "breakdownEmpty"
    // branch surfaces the actionable hint instead.
    const u1 = '100000000000000001';
    const u2 = '100000000000000002';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@${u1}> <@${u2}>` },
      guildMembers: { [u1]: { bot: true }, [u2]: { bot: true } },
    });
    await handleQurlFile(int, 'apikey');
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/No valid recipients/);
    expect(reply.content).toMatch(/bots and your own user are skipped/);
  });

  test('only sender mentioned → ephemeral error', async () => {
    // Same parser-side filter applies to the sender (src/recipient-parser.js:205).
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@${SENDER_ID}>` },
      guildMembers: { [SENDER_ID]: {} },
    });
    await handleQurlFile(int, 'apikey');
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/No valid recipients/);
    expect(reply.content).toMatch(/bots and your own user are skipped/);
  });

  test('unknown-member ID surfaced as warning but valid users still proceed', async () => {
    const known = '100000000000000001';
    const gone = '100000000000000099';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@${known}> <@${gone}>` },
      guildMembers: { [known]: {} },
      guildFetchByID: { [gone]: 'unknown' },
    });
    await handleQurlFile(int, 'apikey');
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.recipientIds).toEqual([known]);
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/no longer in this server/);
  });

  test('cooldown active rejects', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@100000000000000001>` },
      guildMembers: { '100000000000000001': {} },
    });
    // Force cooldown
    sendCooldowns.set(SENDER_ID, Date.now());
    expect(isOnCooldown(SENDER_ID)).toBe(true);
    await handleQurlFile(int, 'apikey');
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/wait before sending/),
    }));
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
  });

  test('supersedeOrCreate sibling collision → surfaces sibling message', async () => {
    mockSupersedeOrCreate.mockResolvedValueOnce({
      created: false,
      surviving: { stage: 'awaiting_revoke_select' },
    });
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlFile(int, 'apikey');
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/revoke.*menu open/);
  });

  test('forwards expires-in + self-destruct + personal-message into payload', async () => {
    const int = makeInteraction({
      options: {
        attachment: VALID_ATTACHMENT,
        recipients: '<@100000000000000001>',
        'expires-in': '7d',
        'self-destruct': '300',
        'personal-message': 'see you Tuesday',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlFile(int, 'apikey');
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.expiresIn).toBe('7d');
    expect(payload.selfDestructSeconds).toBe(300);
    expect(payload.personalMessage).toBe('see you Tuesday');
  });

  test('rejects off-set expires-in (defense vs forged interaction)', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>', 'expires-in': '99y' },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlFile(int, 'apikey');
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Unrecognized expiry/);
  });
});

// ──────────────────────────────────────────────────────────────
// handleQurlMap — front half
// ──────────────────────────────────────────────────────────────

describe('handleQurlMap — slash entry', () => {
  test('Google Maps URL → URL preserved + name extracted from /place/', async () => {
    const int = makeInteraction({
      options: {
        location: 'https://www.google.com/maps/place/Eiffel+Tower/@48.8,2.3',
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int, 'apikey');
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.resourceType).toBe('maps');
    expect(payload.locationUrl).toMatch(/google\.com\/maps\/place\/Eiffel/);
    expect(payload.locationName).toMatch(/Eiffel Tower/);
  });

  test('arbitrary text → synthesized search URL', async () => {
    const int = makeInteraction({
      options: {
        location: 'Central Park, NYC',
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int, 'apikey');
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.locationUrl).toBe('https://www.google.com/maps/search/Central%20Park%2C%20NYC');
    expect(payload.locationName).toMatch(/Central Park/);
  });

  test('location-name override wins over URL-derived name', async () => {
    const int = makeInteraction({
      options: {
        location: 'https://www.google.com/maps/place/Eiffel+Tower',
        'location-name': 'Custom Label',
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int, 'apikey');
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.locationName).toBe('Custom Label');
  });

  test('empty location string → ephemeral error', async () => {
    const int = makeInteraction({
      options: { location: '   ', recipients: '<@100000000000000001>' },
    });
    await handleQurlMap(int, 'apikey');
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/empty/),
    }));
  });

  test('rejects in DM context', async () => {
    const int = makeInteraction({
      guildId: null,
      options: { location: 'Eiffel', recipients: '<@100000000000000001>' },
    });
    await handleQurlMap(int, 'apikey');
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/in a server/),
    }));
  });
});

// ──────────────────────────────────────────────────────────────
// handleSendConfirmClick — Send button
// ──────────────────────────────────────────────────────────────

describe('handleSendConfirmClick', () => {
  const u1 = '100000000000000001';
  const validPayload = {
    resourceType: 'file',
    attachment: VALID_ATTACHMENT,
    locationUrl: null,
    locationName: null,
    resourceLabel: 'x.png',
    recipientIds: [u1],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
    sendNonce: 'nonce-1',
  };

  test('happy path → deleteFlow + executeSendPipeline invoked', async () => {
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    // The pipeline itself isn't reachable without heavy connector mocking,
    // so we just assert deleteFlow fired AND update() landed on 'Preparing'.
    await handleSendConfirmClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM, reason: 'terminal',
    }));
    expect(int.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Preparing send/),
    }));
  });

  test('deleteFlow dedup loser → "already processed" reply, no pipeline call', async () => {
    mockDeleteFlow.mockResolvedValueOnce({ deleted: false });
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleSendConfirmClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/already processed/),
      ephemeral: true,
    }));
    expect(int.update).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Preparing/),
    }));
  });

  test('all recipients have left guild → terminal, deleteFlow called, no pipeline', async () => {
    const int = makeInteraction({
      guildMembers: {},
      guildFetchByID: { [u1]: 'unknown' },
    });
    // No apiKey mock needed — the all-unresolved branch short-circuits
    // before the Promise.all that resolves the guild API key.
    await handleSendConfirmClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({ reason: 'terminal' }));
    expect(int.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/no longer available/),
    }));
  });

  test('no apiKey resolved → tells user setup is needed', async () => {
    mockDb.getGuildApiKey.mockResolvedValueOnce(null);
    // Override config to also have no fallback.
    const config = require('../src/config');
    const originalKey = config.QURL_API_KEY;
    config.QURL_API_KEY = null;
    try {
      const int = makeInteraction({ guildMembers: { [u1]: {} } });
      await handleSendConfirmClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
      expect(int.update).toHaveBeenCalledWith(expect.objectContaining({
        content: expect.stringMatching(/not configured|setup/i),
      }));
    } finally {
      config.QURL_API_KEY = originalKey;
    }
  });
});

// ──────────────────────────────────────────────────────────────
// handleSendCancelClick
// ──────────────────────────────────────────────────────────────

describe('handleSendCancelClick', () => {
  test('happy path → deleteFlow + cooldown cleared + update', async () => {
    const int = makeInteraction();
    sendCooldowns.set(SENDER_ID, Date.now());
    await handleSendCancelClick(int, { flow_id: 'fid' });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM, reason: 'terminal',
    }));
    expect(isOnCooldown(SENDER_ID)).toBe(false);
    expect(int.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/cancelled/),
    }));
  });

  test('deleteFlow dedup loser → ephemeral "already processed"', async () => {
    mockDeleteFlow.mockResolvedValueOnce({ deleted: false });
    const int = makeInteraction();
    await handleSendCancelClick(int, { flow_id: 'fid' });
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/already processed/),
      ephemeral: true,
    }));
  });
});

// ──────────────────────────────────────────────────────────────
// handleSendUserSelect
// ──────────────────────────────────────────────────────────────

describe('handleSendUserSelect', () => {
  const u1 = '100000000000000001';

  function makeSelectInteraction({ users = [makeUser(u1)], ...rest } = {}) {
    const int = makeInteraction(rest);
    int.users = new Map(users.map((u) => [u.id, u]));
    return int;
  }

  const initialPayload = {
    resourceType: 'file',
    resourceLabel: 'x.png',
    recipientIds: [],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
  };

  test('valid pick → transitionFlow with new recipientIds + update', async () => {
    const int = makeSelectInteraction();
    await handleSendUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: expect.objectContaining({ recipientIds: [u1] }),
    }));
    expect(int.update).toHaveBeenCalled();
    const updated = int.update.mock.calls[int.update.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Sending file/);
  });

  test('empty pick → deferUpdate, no transition', async () => {
    const int = makeSelectInteraction({ users: [] });
    await handleSendUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
  });

  test('all bots picked → re-prompt warning + no transition', async () => {
    const int = makeSelectInteraction({
      users: [makeUser(u1, { bot: true })],
    });
    await handleSendUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/bots/),
    }));
  });

  test('transitionFlow conflict → superseded message', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    const int = makeSelectInteraction();
    await handleSendUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/superseded/),
    }));
  });

  test('transitionFlow not_found → expired message', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    const int = makeSelectInteraction();
    await handleSendUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/expired/),
    }));
  });
});

// ──────────────────────────────────────────────────────────────
// Constants + registration assertions
// ──────────────────────────────────────────────────────────────

describe('constants + exports', () => {
  test('SEND_FLOW_TTL_SECONDS = 180', () => {
    expect(SEND_FLOW_TTL_SECONDS).toBe(180);
  });

  test('all customIds are stable string prefixes', () => {
    expect(SEND_USER_SELECT_CUSTOM_ID).toBe('qurl_send_user_select');
    expect(SEND_CONFIRM_SEND_CUSTOM_ID).toBe('qurl_send_confirm_send');
    expect(SEND_CONFIRM_CANCEL_CUSTOM_ID).toBe('qurl_send_confirm_cancel');
  });

  test('all customIds unique', () => {
    const ids = new Set([SEND_USER_SELECT_CUSTOM_ID, SEND_CONFIRM_SEND_CUSTOM_ID, SEND_CONFIRM_CANCEL_CUSTOM_ID]);
    expect(ids.size).toBe(3);
  });

  test('SEND_STAGE_AWAITING_CONFIRM = "awaiting_send_confirm"', () => {
    expect(SEND_STAGE_AWAITING_CONFIRM).toBe('awaiting_send_confirm');
  });

  test('handlers are functions', () => {
    expect(typeof handleQurlFile).toBe('function');
    expect(typeof handleQurlMap).toBe('function');
    expect(typeof handleSendUserSelect).toBe('function');
    expect(typeof handleSendConfirmClick).toBe('function');
    expect(typeof handleSendCancelClick).toBe('function');
  });

  test('executeSendPipeline still exported (back-half hook)', () => {
    expect(typeof executeSendPipeline).toBe('function');
  });
});
