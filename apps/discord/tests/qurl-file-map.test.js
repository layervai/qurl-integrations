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
  // Shared chainables from tests/helpers/discord-mock.js:
  //   makeOptionBuilder — option callbacks (addStringOption, etc.)
  //   makeComponentChainable — component builders (Button, Select,
  //     Modal, etc.) with a superset surface.
  // Both inlined inside the jest.mock factory body so the require
  // dodges jest.mock's hoisting. Centralized so a new chained method
  // at the discord.js layer touches one site for the whole test suite.
  const { makeOptionBuilder, makeComponentChainable } = require('./helpers/discord-mock');
  return {
    SlashCommandBuilder: jest.fn().mockImplementation(() => {
      const subBuilder = () => ({
        setName: jest.fn().mockReturnThis(),
        setDescription: jest.fn().mockReturnThis(),
        addStringOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return this; }),
        addAttachmentOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return this; }),
        addUserOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return this; }),
        addIntegerOption: jest.fn(function (fn) { if (typeof fn === 'function') fn(makeOptionBuilder()); return this; }),
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
    EmbedBuilder: jest.fn().mockImplementation(() => makeComponentChainable({
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
    ButtonBuilder: jest.fn().mockImplementation(() => makeComponentChainable()),
    ButtonStyle: { Primary: 1, Secondary: 2, Success: 3, Danger: 4, Link: 5 },
    ChannelType: { GuildText: 0, DM: 1, GuildVoice: 2, GuildStageVoice: 13 },
    ComponentType: { Button: 2, StringSelect: 3, UserSelect: 5 },
    StringSelectMenuBuilder: jest.fn().mockImplementation(() => makeComponentChainable()),
    UserSelectMenuBuilder: jest.fn().mockImplementation(() => makeComponentChainable()),
    ModalBuilder: jest.fn().mockImplementation(() => makeComponentChainable()),
    TextInputBuilder: jest.fn().mockImplementation(() => makeComponentChainable()),
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
  // `clearAllMocks` resets call history but preserves implementations
  // — critical because the discord.js mock factory at the top of the
  // file uses jest.fn().mockReturnThis() chains on builder methods
  // (setCustomId, setLabel, etc.). `resetAllMocks` would wipe those
  // implementations and crash the next call into a non-function.
  jest.clearAllMocks();
  sendCooldowns.clear();
  // Targeted mockReset for mocks where the `mockResolvedValueOnce`
  // queue can leak across tests (an early-return path that doesn't
  // consume the queued value pollutes the next test's first call).
  // Re-seeded with `mockResolvedValue` defaults below; tests
  // override per-call with `mockResolvedValueOnce` as needed.
  mockSupersedeOrCreate.mockReset();
  mockDeleteFlow.mockReset();
  mockTransitionFlow.mockReset();
  mockDb.getGuildApiKey.mockReset();
  mockSupersedeOrCreate.mockResolvedValue({ created: true, version: 1 });
  mockDeleteFlow.mockResolvedValue({ deleted: true });
  mockTransitionFlow.mockResolvedValue({ result: 'ok', version: 2 });
});

// ──────────────────────────────────────────────────────────────
// Pure helpers
// ──────────────────────────────────────────────────────────────

describe('selfDestructOptionToSeconds', () => {
  // SELF_DESTRUCT_PRESETS (utils/time.js): 0.5, 1, 5, 30, 300, 1800, 3600.
  // Anything else falls back to null per the defense-in-depth gate
  // added in round 6 (cr-flagged: forged interactions could otherwise
  // smuggle '999999999' past Discord's server-side choice enforcement).
  test.each([
    ['none', null],
    [SELF_DESTRUCT_NO_TIMER_CHOICE, null],
    [null, null],
    [undefined, null],
    ['', null],
    // Preset values pass through:
    ['5', 5],
    ['30', 30],
    ['300', 300],
    ['1800', 1800],
    ['3600', 3600],
    // Off-set values fall back to null (defense-in-depth):
    ['60', null],
    ['7200', null],
    ['999999999', null],
    // Edge cases:
    ['0', null],
    ['-5', null],
    ['NaN', null],
    ['bogus', null],
    ['1.5', 1],  // Math.floor(1.5)=1 IS a preset
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

  test('non-10007 error → transientFailureIds (NOT unresolvedIds) + warn logged', async () => {
    // Rate-limit / gateway-blip 429s and 500-class errors must land
    // in transientFailureIds so the caller surfaces "try again"
    // copy instead of "they left the server."
    const int = makeInteraction({
      guildMembers: {},
      guildFetchByID: { '100000000000000001': 'ratelimit' },
    });
    const r = await resolveRecipientUsers(int, ['100000000000000001']);
    expect(r.transientFailureIds).toEqual(['100000000000000001']);
    expect(r.unresolvedIds).toEqual([]);
    expect(logger.warn).toHaveBeenCalledWith(
      'resolveRecipientUsers: members.fetch failed (transient)',
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

  test('invalidTokens with embedded backticks are stripped so the code-fence stays intact', () => {
    // recipient-parser.js explicitly does NOT escape invalidTokens —
    // the caller must defend. A token containing ``` would otherwise
    // close the fence early and let a masked link or @-mention reach
    // the Discord renderer.
    const tokens = ['```\n[evil](https://phish.example)\n```', '`code`', 'plain'];
    const out = renderRecipientWarnings({
      invalidTokens: tokens, cappedCount: 0, unresolvedIds: [],
      droppedBots: 0, droppedSelf: 0,
    });
    // Backticks must not appear in the rendered tokens themselves —
    // they'd terminate the surrounding fence.
    const fenceContent = out.split('```')[1] || '';
    expect(fenceContent).not.toMatch(/`/);
    // The non-backtick content survives.
    expect(out).toContain('plain');
    expect(out).toContain('[evil](https://phish.example)');
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

  test('transientFailureIds rendered with neutral copy (not "left the server")', () => {
    // Rate-limit / gateway-blip 429s land in transientFailureIds, NOT
    // unresolvedIds — so the message must encourage retry, not imply
    // the recipient is gone. cr round 4 caught this misdirection.
    const out = renderRecipientWarnings({
      transientFailureIds: ['100000000000000001', '100000000000000002'],
    });
    expect(out).toMatch(/2 user.*couldn't be looked up.*try again/);
    expect(out).not.toMatch(/no longer in this server/);
  });

  test('renderRecipientWarnings tolerates missing fields via defaults', () => {
    // The destructure now defaults each field — a future caller that
    // forgets to pass `transientFailureIds` (or any other bucket)
    // should get an empty warning, not a `.length`-of-undefined crash.
    expect(renderRecipientWarnings({})).toBe('');
    expect(renderRecipientWarnings()).toBe('');
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

  test('unknown resourceType throws — forces an explicit branch for future types', () => {
    // Round-7 (cr) added an explicit else-throw so a future resource
    // type (audio, contact card, etc.) can't silently render as a
    // location. Pins the contract.
    expect(() => renderConfirmCardContent({
      ...baseProps,
      resourceType: 'audio',
    })).toThrow(/unknown resourceType.*audio/);
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

  test('personal-message preview cap at 80 chars, rendered as blockquote', () => {
    const long = 'x'.repeat(120);
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: long });
    // Round-6 (cr) switched from `"..."` to `> ` blockquote so literal
    // `"` chars in the message can't make the rendering look ragged.
    expect(out).toMatch(/> x{80}…/);
    // The previous `"..."` wrap is gone.
    expect(out).not.toMatch(/"x{80}…"/);
  });

  test('personal-message preview backs off the cut when it would land on a markdown escape', () => {
    // sanitizeMessage emits `\*` etc. If a slice lands the cut at a
    // boundary between `\` and `*`, the rendered preview shows a
    // dangling `\`. Round-6 (cr) backs the cut off by 1 when the
    // 80th char is a `\` not preceded by another `\`.
    //
    // Build a 90-char message where char[79] is '\\' and char[80] is '*'.
    // Confirm the truncation drops the trailing `\` before the ellipsis.
    const safePrefix = 'a'.repeat(79);  // 79 chars
    const message = safePrefix + '\\*' + 'rest'.repeat(10);  // 79+2+40 = 121 chars
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: message });
    // The truncated preview ends with 79 chars (no trailing `\`)
    // followed by the ellipsis. Backed-off cut: index 79.
    expect(out).toContain(`> ${safePrefix}…`);
    expect(out).not.toMatch(/\\…/);
  });

  test('personal-message renders pre-sanitized input verbatim (no double-escape)', () => {
    // sanitizeMessage already escapes markdown at the slash-option
    // boundary — a `**bold**` input becomes `\*\*bold\*\*` in the
    // payload. The card must NOT escape again or the user sees
    // `\\\*\\\*bold\\\*\\\*`.
    const presanitized = '\\*\\*emphasis\\*\\*';
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: presanitized });
    expect(out).toContain(`> ${presanitized}`);
    expect(out).not.toMatch(/\\\\\*/);
  });

  test('personal-message with mixed markdown + escape sequences renders without re-escaping the escapes', () => {
    // Composite edge case: a real-world sanitizeMessage output for a
    // user input like `**bold** \\n [link](https://evil)`. The literal
    // backslashes that sanitizeMessage emits for markdown escapes
    // must NOT themselves become `\\\\` in the card.
    const presanitized = '\\*\\*bold\\*\\* \\\\n \\[link\\]\\(https://evil\\)';
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: presanitized });
    expect(out).toContain(`> ${presanitized}`);
    // Defense: no `\\\\` (four backslashes) appears — that'd be the
    // unambiguous double-escape regression signature.
    expect(out).not.toMatch(/\\\\\\\\/);
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
    await handleQurlFile(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/in a server/),
      ephemeral: true,
    }));
  });

  test('rejects when attachment.url is not Discord CDN (SSRF gate)', async () => {
    const int = makeInteraction({
      options: { attachment: { ...VALID_ATTACHMENT, url: 'https://evil.com/x.png' } },
    });
    await handleQurlFile(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/source not allowed/),
      ephemeral: true,
    }));
  });

  test('rejects disallowed file type', async () => {
    const int = makeInteraction({
      options: { attachment: { ...VALID_ATTACHMENT, contentType: 'application/x-evil-macroenabled' } },
    });
    await handleQurlFile(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/File type not allowed/),
    }));
  });

  test('rejects file over size cap', async () => {
    const int = makeInteraction({
      options: { attachment: { ...VALID_ATTACHMENT, size: 999_999_999 } },
    });
    await handleQurlFile(int);
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Unrecognized expiry/);
  });

  test('all-bots cap-skew: cache-miss bots eat cap, real users get squeezed out', async () => {
    // recipient-parser.js:214 strips bots ONLY when the cache reports
    // them. Cache-MISS bots reach members.fetch, get resolved, then
    // partitionRecipients drops them — but they've already eaten cap
    // slots in the parser's `ids` array.
    //
    // Scenario: 26 mentions = 25 cache-miss bots + 1 real user. Parser
    // caps at 25 (QURL_SEND_MAX_RECIPIENTS) → keeps the FIRST 25 IDs.
    // 25 bots get fetched, partition drops them all, user1 was past
    // the cap and never made it.
    //
    // This is the documented v1 cap-skew limitation
    // (commands.js:partitionRecipients comment). Pin the failure mode
    // so a future refactor that flips resolve-then-cap ordering can't
    // silently "fix" it without consciously updating the comment.
    const mkId = (n) => '1000000000000' + String(n).padStart(6, '0');
    const realUser = mkId(25);  // beyond the cap
    const bots = Array.from({ length: 25 }, (_, i) => mkId(i));
    const mentions = [...bots, realUser].map((id) => `<@${id}>`).join(' ');
    const fetchByID = {};
    for (const id of bots) fetchByID[id] = makeUser(id, { bot: true });
    fetchByID[realUser] = makeUser(realUser);  // resolves cleanly, but past cap
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: mentions },
      guildMembers: {},  // ALL cache-miss
      guildFetchByID: fetchByID,
    });
    await handleQurlFile(int);
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/No valid recipients/);
    // 25 cache-miss bots passed the parser (consumed the cap), got
    // resolved via fetch, then partition dropped them — droppedBots
    // breakdown should surface.
    expect(reply.content).toMatch(/bot/);
  });

  test('inner catch on supersedeOrCreate throw clears cooldown + surfaces specific error', async () => {
    // The supersedeOrCreate-specific inner catch is the dominant
    // failure-path test for cooldown release — verifies the canonical
    // "DDB outage" branch lifts the cooldown so the user can retry.
    mockSupersedeOrCreate.mockRejectedValueOnce(new Error('ddb gone'));
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlFile(int);
    expect(isOnCooldown(SENDER_ID)).toBe(false);
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Could not start a send/);
  });

  test('safety-net top-level catch clears cooldown on an unanticipated synchronous throw', async () => {
    // The safety-net's job: catch throws that don't have a targeted
    // inner catch — synchronous bugs in helpers, malformed cache
    // entries, future regex changes that fail. Forcing
    // `deferReply` to throw is the cleanest reproduction: it's the
    // first await inside the try block, NOT wrapped by any inner
    // catch, and forcing it to reject simulates a Discord token
    // exhausted or gateway blip on the entry-time ACK.
    //
    // Without the safety-net, setCooldown ran but no clearCooldown
    // would fire — user is locked out for the full cooldown window
    // despite never seeing a response.
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    int.deferReply.mockRejectedValueOnce(new Error('token expired'));
    await handleQurlFile(int);
    expect(isOnCooldown(SENDER_ID)).toBe(false);
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/unexpected throw/),
      expect.objectContaining({ user_id: SENDER_ID }),
    );
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
    await handleQurlMap(int);
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
    await handleQurlMap(int);
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
    await handleQurlMap(int);
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.locationName).toBe('Custom Label');
  });

  test('empty location string → ephemeral error', async () => {
    const int = makeInteraction({
      options: { location: '   ', recipients: '<@100000000000000001>' },
    });
    await handleQurlMap(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/empty/),
    }));
  });

  test('rejects in DM context', async () => {
    const int = makeInteraction({
      guildId: null,
      options: { location: 'Eiffel', recipients: '<@100000000000000001>' },
    });
    await handleQurlMap(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/in a server/),
    }));
  });

  test('forged interaction missing required `location` → actionable ephemeral, no flow row', async () => {
    // Discord enforces required options server-side; only a forged
    // interaction can hit the `getString('location', true)` throw.
    // Round-6 (cr) added a targeted catch so the user sees an
    // actionable message instead of the dispatcher's generic safety
    // net.
    const int = makeInteraction({ options: {} });
    int.options.getString = jest.fn((name, required) => {
      if (name === 'location' && required) {
        const err = new Error('CommandInteractionOptionNotFound');
        err.code = 'CommandInteractionOptionNotFound';
        throw err;
      }
      return null;
    });
    await handleQurlMap(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/`location:` option is required/),
      ephemeral: true,
    }));
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
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

  test('happy path → deferUpdate + deleteFlow + editReply "Preparing"', async () => {
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleSendConfirmClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    // Defer-ack within the 3s window before any awaits — round-7 (cr)
    // added this so resolveRecipientUsers + getGuildApiKey + deleteFlow
    // can't blow Discord's hard ack deadline on a cold cache.
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM, reason: 'terminal',
    }));
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Preparing send/),
    }));
    // After deferUpdate, the handler must NOT call .update directly
    // (would double-ack). Verify by confirming the legacy update
    // mock got no main-message updates.
    expect(int.update).not.toHaveBeenCalled();
  });

  test('deleteFlow dedup loser → version-fenced "Recipients changed" reply, no pipeline call', async () => {
    // `deleted: false` now collapses BOTH duplicate dispatch (Discord
    // retry / SQS at-least-once redelivery) AND mid-flight picker
    // mutation (UserSelect transitioned the row between dispatcher's
    // loadFlow and our deleteFlow). Same user recovery either way.
    mockDeleteFlow.mockResolvedValueOnce({ deleted: false });
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleSendConfirmClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Recipients changed|re-click Send/i),
      ephemeral: true,
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Preparing/),
    }));
  });

  test('deleteFlow is version-gated to fence the picker-then-Send race', async () => {
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleSendConfirmClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 7 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
      expectedVersion: 7,
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
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/no longer reachable/),
    }));
  });

  test('no apiKey resolved → tells user setup is needed', async () => {
    mockDb.getGuildApiKey.mockResolvedValueOnce(null);
    // jest.replaceProperty restores automatically on test teardown
    // and is parallel-test-safe — beats hand-rolled
    // mutate-restore-in-finally which would silently corrupt the
    // mocked config if a future refactor parallelizes test cases
    // within this file.
    const config = require('../src/config');
    jest.replaceProperty(config, 'QURL_API_KEY', null);
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    await handleSendConfirmClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/not configured|setup/i),
    }));
  });

  test('partial-resolve at Send click — Send proceeds with remaining users, drop surfaced via followUp + info log', async () => {
    // Mid-flight guild churn: row.payload.recipientIds = [u1, gone],
    // at click time gone left the guild. resolveRecipientUsers returns
    // {users:[u1's user], unresolvedIds:[gone]}. partitionRecipients
    // keeps u1. Send fires executeSendPipeline with the remaining
    // user, the silent-drop is announced to the sender via ephemeral
    // followUp, and the forensic log lands at info (not debug) so
    // oncall sees the signal without lowering log verbosity.
    const gone = '100000000000000099';
    const payloadWithGhost = { ...validPayload, recipientIds: [u1, gone] };
    const int = makeInteraction({
      guildMembers: { [u1]: {} },
      guildFetchByID: { [gone]: 'unknown' },
    });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleSendConfirmClick(int, { flow_id: 'fid', row: { payload: payloadWithGhost, version: 1 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({ reason: 'terminal' }));
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Preparing send/),
    }));
    // followUp announces the drop to the sender so they know the
    // delivered count doesn't match the card.
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/1 recipient had left the server/),
      ephemeral: true,
    }));
    // Forensic log at INFO (escalated from debug per cr round 3) —
    // oncall can grep for mid-flight guild churn without dialing
    // verbosity up. Round 5 split the bucket so the log fields
    // distinguish left-the-server (10007) from transient.
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringMatching(/partial drop at click time/),
      expect.objectContaining({ left: 1, transient: 0 }),
    );
  });

  test('partial transient lookup at Send click — Send proceeds with remaining, transient drop surfaced with retry copy', async () => {
    // cr round 5: transientFailureIds were previously dropped silently
    // at click time. They must be surfaced with retry-encouraging
    // copy (NOT "left the server" wording) — the buckets are now
    // split + threaded.
    const flaky = '100000000000000099';
    const payloadWithFlaky = { ...validPayload, recipientIds: [u1, flaky] };
    const int = makeInteraction({
      guildMembers: { [u1]: {} },
      guildFetchByID: { [flaky]: 'ratelimit' },
    });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleSendConfirmClick(int, { flow_id: 'fid', row: { payload: payloadWithFlaky, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Preparing send/),
    }));
    // followUp distinguishes transient from "left" — retry copy.
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/1 couldn't be looked up.*rerun \/qurl file/),
      ephemeral: true,
    }));
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringMatching(/partial drop at click time/),
      expect.objectContaining({ left: 0, transient: 1 }),
    );
  });

  test('getGuildApiKey throw at click time → ephemeral retry, NO deleteFlow (row stays alive)', async () => {
    // Round-6 (cr) reordering: getGuildApiKey runs BEFORE deleteFlow
    // so a DDB blip doesn't burn the flow row. User can re-click
    // Send within the 3-min TTL once the blip clears.
    mockDb.getGuildApiKey.mockRejectedValueOnce(new Error('ddb gone'));
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    await handleSendConfirmClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not look up the qURL API key/),
      ephemeral: true,
    }));
    expect(mockDeleteFlow).not.toHaveBeenCalled();
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/getGuildApiKey threw/),
      expect.objectContaining({ flow_id: 'fid' }),
    );
  });

  test('resolveRecipientUsers throw at click time → ephemeral retry message, NO deleteFlow', async () => {
    // cr round 5: handleSendConfirmClick previously had no try/catch
    // around the click-time resolution. A throw would propagate to the
    // dispatcher's outer catch and leave the user staring at the
    // unchanged card. Targeted catch now surfaces a recoverable
    // ephemeral reply and DOES NOT commit the dedup deleteFlow so the
    // card stays alive for the 3-min TTL.
    const int = makeInteraction({
      guildMembers: {},
      // Force a throw out of `members.fetch` (not a return-value error,
      // a real `throw new Error(...)`).
    });
    int.guild.members.fetch = jest.fn().mockRejectedValue(new Error('catastrophic'));
    // Need to bypass batchSettled's swallow path: rejection inside the
    // callback is caught by Promise.allSettled, then resolveRecipientUsers's
    // own try/catch handles it. A real throw from BEFORE the try (e.g.
    // guild === null) is what we want here. Simulate by deleting guild.
    // Actually simpler — make the entire interaction.guild throw on read.
    Object.defineProperty(int, 'guild', {
      get() { throw new Error('cache exploded'); },
    });
    await handleSendConfirmClick(int, { flow_id: 'fid', row: { payload: { ...validPayload, recipientIds: [u1] }, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not look up recipients/),
      ephemeral: true,
    }));
    expect(mockDeleteFlow).not.toHaveBeenCalled();
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/resolveRecipientUsers threw/),
      expect.objectContaining({ flow_id: 'fid' }),
    );
  });
});

// ──────────────────────────────────────────────────────────────
// handleSendCancelClick
// ──────────────────────────────────────────────────────────────

describe('handleSendCancelClick', () => {
  test('happy path → version-gated deleteFlow + cooldown cleared + update', async () => {
    const int = makeInteraction();
    sendCooldowns.set(SENDER_ID, Date.now());
    await handleSendCancelClick(int, { flow_id: 'fid', row: { version: 3 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
      expectedVersion: 3,
    }));
    expect(isOnCooldown(SENDER_ID)).toBe(false);
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/cancelled/),
    }));
  });

  test('deleteFlow dedup loser → ephemeral message + cooldown PRESERVED', async () => {
    // Critical: when Send won the race, we must NOT clear cooldown
    // mid-send-fanout. Otherwise the user can re-fire /qurl file
    // immediately and bypass the per-user cooldown window while the
    // first send is still DMing recipients.
    mockDeleteFlow.mockResolvedValueOnce({ deleted: false });
    const cooldownAt = Date.now();
    sendCooldowns.set(SENDER_ID, cooldownAt);
    const int = makeInteraction();
    await handleSendCancelClick(int, { flow_id: 'fid', row: { version: 3 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/already processed|card moved/),
      ephemeral: true,
    }));
    // Cooldown is still in place — Cancel-loser must not unlock it.
    expect(isOnCooldown(SENDER_ID)).toBe(true);
    expect(sendCooldowns.get(SENDER_ID)).toBe(cooldownAt);
  });

  test('Cancel deleteFlow is version-fenced against picker race', async () => {
    const int = makeInteraction();
    await handleSendCancelClick(int, { flow_id: 'fid', row: { version: 11 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      expectedVersion: 11,
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
    const beforeSecs = Math.floor(Date.now() / 1000);
    const int = makeSelectInteraction();
    await handleSendUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: expect.objectContaining({ recipientIds: [u1] }),
      terminal: false,
      // TTL refresh is the whole point of staying at the same stage —
      // picker churn must not let the row TTL out from under the user
      // mid-flow.
      set_expires_at: expect.any(Number),
    }));
    const callArgs = mockTransitionFlow.mock.calls[0][2];
    // ±5s clock-skew tolerance keeps the assertion from flaking on
    // slow CI runners while still catching gross drift (e.g., a
    // refactor that forgot to add SEND_FLOW_TTL_SECONDS to nowSecs,
    // or a `set_expires_at: 0` regression).
    const SKEW = 5;
    expect(callArgs.set_expires_at).toBeGreaterThanOrEqual(beforeSecs + SEND_FLOW_TTL_SECONDS - SKEW);
    expect(callArgs.set_expires_at).toBeLessThanOrEqual(Math.floor(Date.now() / 1000) + SEND_FLOW_TTL_SECONDS + SKEW);
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

  test('partial-bot pick → transitionFlow with non-bot users + warning surfaces on card', async () => {
    // Live picker UX: user selects 3 real users + 1 bot. Partition
    // drops the bot, valid=[u1,u2,u3], droppedBots=1. transitionFlow
    // commits the new ids, the re-rendered card shows the warning
    // line so the user knows the bot didn't make the cut.
    const u2 = '100000000000000002';
    const u3 = '100000000000000003';
    const bot1 = '100000000000000099';
    const int = makeSelectInteraction({
      users: [
        makeUser(u1),
        makeUser(u2),
        makeUser(u3),
        makeUser(bot1, { bot: true }),
      ],
    });
    await handleSendUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({ recipientIds: [u1, u2, u3] }),
    }));
    const updated = int.update.mock.calls[int.update.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/bot/);
    expect(updated.content).toMatch(/Sending file/);
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

  test('siblingMessage is keyed by stage so any of the three confirm-card customIds surfaces the same message', () => {
    // siblingMessage is registered only on SEND_USER_SELECT_CUSTOM_ID
    // (commands.js's registerFlow blocks), but flow-dispatch stores
    // siblingMessages keyed by EXPECTED_STAGE — so a /qurl revoke
    // supersede that peeks at a row at SEND_STAGE_AWAITING_CONFIRM
    // gets the same actionable message regardless of which customId
    // was registered. Pin the lookup so a future refactor that
    // accidentally keys siblingMessage by customId breaks here.
    const { siblingMessageForStage } = require('../src/flow-dispatch');
    const msg = siblingMessageForStage(SEND_STAGE_AWAITING_CONFIRM);
    expect(msg).toMatch(/qurl file.*qurl map.*confirm card/i);
    // Defense-in-depth: confirm-card customIds for SEND + CANCEL,
    // although registered without their own siblingMessage, still
    // reach the same registered message through the stage lookup.
    // (Tested indirectly: any customId at this stage maps to the
    // same single registered message.)
    expect(msg).toBeTruthy();
  });

  test('executeSendPipeline still exported (back-half hook)', () => {
    expect(typeof executeSendPipeline).toBe('function');
  });
});
