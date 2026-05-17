/**
 * Tests for /qurl send + /qurl map slash commands (PR 7b.2;
 * `/qurl file` renamed to `/qurl send` in a later PR).
 *
 * Covers the new handlers from src/commands.js — handleQurlSend,
 * handleQurlMap, the shared confirm-card pipeline, the flow-dispatch
 * handlers (UserSelect / Send / Cancel) — plus the pure helpers
 * (resolveRecipientUsers, partitionRecipients, selfDestructOptionToSeconds,
 * renderRecipientWarnings, renderConfirmCardContent).
 *
 * Mocks mirror tests/send-pipeline-back-half.test.js so both files share
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
  // /qurl map feature toggle — these tests cover the enabled-feature
  // path (handleAutocomplete → searchPlaces, handleQurlMap dispatch,
  // confirm-card MAPS flow). Production default is off.
  MAP_COMMAND_ENABLED: true,
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
    MentionableSelectMenuBuilder: jest.fn().mockImplementation(() => makeComponentChainable()),
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

// Shared places-mock — see tests/helpers/places-mock.js for the
// single source of truth for the encode/decode/shape contract.
const {
  mockPlacesModule,
  mockSearchPlaces,
  mockFindPlaceFromText,
  mockGetPlaceDetails,
} = require('./helpers/places-mock');
jest.mock('../src/places', () => mockPlacesModule);

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

// flow-dispatch (registerFlow + customId routing) is loaded for real —
// only flow-state's DDB-backed primitives are mocked above. Loading
// the real registerFlow lets the idempotent-failure guard fire on
// test re-loads and pins the customId → handler contract.
const commands = require('../src/commands');
const { _test } = commands;
const logger = require('../src/logger');
const {
  handleQurlSend,
  handleQurlMap,
  resolveRecipientUsers,
  partitionRecipients,
  selfDestructOptionToSeconds,
  renderRecipientWarnings,
  renderConfirmCardContent,
  resolveMentionableSelection,
  parseLocationInput,
  resolveLocation,
  RESOLVE_REASON,
  handleAutocomplete,
  _resetAutocompleteFailureBurst,
  AUTOCOMPLETE_FAILURE_LOG_BURST,
  safeDecodeURIComponent,
  softenCooldown,
  SEND_STAGE_AWAITING_CONFIRM,
  CONFIRM_USER_SELECT_CUSTOM_ID,
  CONFIRM_SEND_CUSTOM_ID,
  CONFIRM_CANCEL_CUSTOM_ID,
  CONFIRM_EXPIRY_SELECT_CUSTOM_ID,
  CONFIRM_SELF_DESTRUCT_SELECT_CUSTOM_ID,
  CONFIRM_NOTE_BUTTON_CUSTOM_ID,
  CONFIRM_NOTE_MODAL_CUSTOM_ID,
  CONFIRM_VOICE_EVERYONE_BUTTON_CUSTOM_ID,
  CONFIRM_PICK_MANUAL_BUTTON_CUSTOM_ID,
  RECIPIENT_MODE_PICKER,
  RECIPIENT_MODE_VOICE,
  RECIPIENT_MODE_EVERYONE,
  normalizeRecipientMode,
  SEND_FLOW_TTL_SECONDS,
  SELF_DESTRUCT_NO_TIMER_CHOICE,
  isOnCooldown,
  setCooldown,
  clearCooldown,
  sendCooldowns,
  executeSendPipeline,
  getActiveFileSends,
  setActiveFileSends,
  resolveRoleNames,
} = _test;

// Flow-dispatch handlers live at module top-level (consumed by
// registerFlow at boot). _test only exports things that aren't already
// at the top level.
const {
  handleConfirmUserSelect,
  handleConfirmSendClick,
  handleConfirmCancelClick,
  handleConfirmExpirySelect,
  handleConfirmSelfDestructSelect,
  handleConfirmNoteButton,
  handleConfirmNoteModal,
  handleConfirmVoiceEveryone,
  handleConfirmPickManual,
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

  const interaction = {
    user: { id: userId, username: 'Sender' },
    guildId,
    channelId,
    guild,
    member: { displayName: 'Sender' },
    options: {
      getString: optGetString,
      getAttachment: optGetAttachment,
      getSubcommand: () => options._sub || 'send',
    },
    reply: jest.fn().mockResolvedValue(undefined),
    editReply: jest.fn().mockResolvedValue(undefined),
    followUp: jest.fn().mockResolvedValue(undefined),
    update: jest.fn().mockResolvedValue(undefined),
    deferUpdate: jest.fn().mockResolvedValue(undefined),
    replied: false,
    deferred: false,
  };
  // deferReply MUST flip `deferred` so the no-double-defer contract in
  // handleQurlSlashSend (`if (!interaction.deferred)`) is actually
  // exercised — otherwise handleQurlMap → handleQurlSlashSend would
  // double-defer and the test would silently pass on a path that throws
  // in production with "Already replied or deferred".
  interaction.deferReply = jest.fn(async () => { interaction.deferred = true; });
  return interaction;
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
  mockSearchPlaces.mockReset().mockResolvedValue([]);
  mockFindPlaceFromText.mockReset().mockResolvedValue(null);
  mockGetPlaceDetails.mockReset().mockResolvedValue(null);
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
  // against forged interactions that could otherwise smuggle
  // '999999999' past Discord's server-side choice enforcement.
  test.each([
    ['none', null],
    [SELF_DESTRUCT_NO_TIMER_CHOICE, null],
    [null, null],
    [undefined, null],
    ['', null],
    // Preset values pass through:
    ['0.5', 0.5],  // "1/2 second" preset — Math.floor would have downgraded this to "no timer"
    ['1', 1],
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
    ['1.5', null],   // 1.5 is not in the preset set — no downgrade-to-floor
    ['0.25', null],  // off-preset fractional
  ])('value=%j → seconds=%j', (input, expected) => {
    expect(selfDestructOptionToSeconds(input)).toBe(expected);
  });
});

describe('partitionRecipients', () => {
  test('drops bots, keeps sender, flags selfIncluded=true', () => {
    const users = [
      makeUser('100000000000000001'),
      makeUser('100000000000000002', { bot: true }),
      makeUser(SENDER_ID),
      makeUser('100000000000000003'),
    ];
    const r = partitionRecipients(users, SENDER_ID);
    expect(r.valid.map((u) => u.id))
      .toEqual(['100000000000000001', SENDER_ID, '100000000000000003']);
    expect(r.droppedBots).toBe(1);
    expect(r.selfIncluded).toBe(true);
  });

  test('all bots returns valid=[]', () => {
    const users = [makeUser('100000000000000001', { bot: true }), makeUser('100000000000000002', { bot: true })];
    const r = partitionRecipients(users, SENDER_ID);
    expect(r.valid).toEqual([]);
    expect(r.droppedBots).toBe(2);
    expect(r.selfIncluded).toBe(false);
  });

  test('only sender is a legitimate single-recipient self-send', () => {
    // Self-send: sender alone is a valid recipient list.
    // valid=[sender], selfIncluded=true, no drops.
    const r = partitionRecipients([makeUser(SENDER_ID)], SENDER_ID);
    expect(r.valid.map((u) => u.id)).toEqual([SENDER_ID]);
    expect(r.droppedBots).toBe(0);
    expect(r.selfIncluded).toBe(true);
  });

  test('empty input', () => {
    expect(partitionRecipients([], SENDER_ID))
      .toEqual({ valid: [], droppedBots: 0, selfIncluded: false });
  });

  test('sender NOT in input → selfIncluded=false', () => {
    const r = partitionRecipients(
      [makeUser('100000000000000001'), makeUser('100000000000000002')],
      SENDER_ID,
    );
    expect(r.selfIncluded).toBe(false);
    expect(r.valid.length).toBe(2);
  });

  test('contract: does NOT re-dedup — dedup is upstream (parseRecipientMentions Set + Discord picker gateway-event)', () => {
    // Confirm duplicates passed in are preserved verbatim:
    // verbatim. parseRecipientMentions already dedupes via Set
    // (recipient-parser.js:197-198) and Discord's UserSelectMenu
    // gateway-event surfaces each picked user at most once. If a future
    // refactor breaks the parser's dedup, the divergence between
    // input.length and partition.valid.length must fail loudly here —
    // silently re-deduping in partitionRecipients would mask the bug.
    const dup = makeUser('100000000000000001');
    const r = partitionRecipients([dup, dup, makeUser('100000000000000002')], SENDER_ID);
    expect(r.valid.length).toBe(3);
  });

  test('excludeSender:true drops the sender pre-validity (selfIncluded stays false)', () => {
    // Voice-everyone semantics. The sender is dropped silently — no
    // droppedBots-style accounting, and `selfIncluded` cannot be true
    // on this path. The sender exclusion is inferred from voice-mode
    // UI semantics rather than surfaced in user-visible copy.
    const users = [
      makeUser('100000000000000001'),
      makeUser(SENDER_ID),
      makeUser('100000000000000002'),
    ];
    const r = partitionRecipients(users, SENDER_ID, { excludeSender: true });
    expect(r.valid.map((u) => u.id))
      .toEqual(['100000000000000001', '100000000000000002']);
    expect(r.droppedBots).toBe(0);
    expect(r.selfIncluded).toBe(false);
  });

  test('excludeSender:true + sender-only input → valid=[] (caller handles fallback copy)', () => {
    const r = partitionRecipients([makeUser(SENDER_ID)], SENDER_ID, { excludeSender: true });
    expect(r.valid).toEqual([]);
    expect(r.droppedBots).toBe(0);
    expect(r.selfIncluded).toBe(false);
  });

  test('excludeSender default (false) preserves legacy self-send behavior', () => {
    // Picker / text paths call without the option and rely on the
    // sender appearing in `valid` with `selfIncluded:true`. Pin that
    // omitting the option doesn't accidentally activate exclusion.
    const r = partitionRecipients([makeUser(SENDER_ID)], SENDER_ID);
    expect(r.valid.map((u) => u.id)).toEqual([SENDER_ID]);
    expect(r.selfIncluded).toBe(true);
  });
});

describe('resolveMentionableSelection', () => {
  // Picker-path helper (Mentionable select returns BOTH users + roles in
  // one interaction): expands the role members → flat user list, filters
  // bots, dedupes against directly picked users, caps at
  // QURL_SEND_MAX_RECIPIENTS, gates the @everyone role on canMentionEveryone.
  const GUILD_ID = 'guild-1';

  // `mentionable` defaults to `true` so the existing test corpus
  // continues to expand picked roles after the #326 gate landed.
  // Per-test overrides (`mentionable: false`) exercise the deny path.
  // `name` is consumed by the caller's `guild.roles.cache.get(id)
  // ?.name` lookup (renderRecipientWarnings bullet text).
  function makeRole({ id, members = [], mentionable = true, name }) {
    // role.members is a Discord.js Collection but only `.entries()`
    // (iterable of [id, member]) is read; a Map is shape-compatible.
    const memberMap = new Map(members.map((m) => [m.user.id, m]));
    return [id, { id, name: name ?? `role-${id}`, members: memberMap, mentionable }];
  }

  function makeMentionableInteraction({
    pickedUsers = [],
    pickedRoles = [],
    guildMemberCache = new Map(),
    inDM = false,
  } = {}) {
    // Mirror picked roles into guild.roles.cache so the caller-side
    // name lookup (`guild.roles.cache.get(id)?.name`) used by
    // renderRecipientWarnings to render denied-role bullets has
    // something to find. Stays a no-op when no roles are picked.
    const roleCache = new Map();
    for (const [id, role] of pickedRoles) {
      roleCache.set(id, role);
    }
    const guild = inDM ? null : {
      id: GUILD_ID,
      members: { cache: guildMemberCache },
      roles: { cache: roleCache },
    };
    return {
      guild,
      users: new Map(pickedUsers.map((u) => [u.id, u])),
      roles: new Map(pickedRoles),
    };
  }

  test('users only (no roles) → returns those users verbatim, no denial', () => {
    const u1 = makeUser('100000000000000001');
    const u2 = makeUser('100000000000000002');
    const int = makeMentionableInteraction({ pickedUsers: [u1, u2] });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.map((u) => u.id)).toEqual([u1.id, u2.id]);
    expect(r.massMentionDenied).toBe(false);
  });

  test('filters out malformed user entries lacking .id (defense against partial User payload)', () => {
    // The seed loop guards with `if (u && u.id) userMap.set(u.id, u);`
    // — discord.js shouldn't ever surface a partial User, but the
    // Collection→Map duck-typing in the test harness leaves room for
    // a future regression to drop the guard. Pin the defense.
    const u1 = makeUser('100000000000000001');
    const partial = { id: undefined, username: 'partial' };
    const int = makeMentionableInteraction({ pickedUsers: [u1, partial] });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.map((u) => u.id)).toEqual([u1.id]);
  });

  test('named role expands its members onto the user list, filters bots', () => {
    const u1 = makeUser('100000000000000001');
    const bot1 = makeUser('100000000000000099', { bot: true });
    const u2 = makeUser('100000000000000002');
    const int = makeMentionableInteraction({
      pickedRoles: [
        makeRole({
          id: 'role-eng',
          members: [{ user: u1 }, { user: bot1 }, { user: u2 }],
        }),
      ],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.map((u) => u.id).sort()).toEqual([u1.id, u2.id].sort());
    expect(r.massMentionDenied).toBe(false);
  });

  test('user + role overlap dedupes (same id appears once)', () => {
    const u1 = makeUser('100000000000000001');
    const int = makeMentionableInteraction({
      pickedUsers: [u1],
      pickedRoles: [makeRole({ id: 'role-eng', members: [{ user: u1 }] })],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.map((u) => u.id)).toEqual([u1.id]);
  });

  test('@everyone role (role.id === guild.id) WITHOUT MENTION_EVERYONE → denied, no expansion', () => {
    const u1 = makeUser('100000000000000001');
    const guildMembers = new Map([[u1.id, { user: u1 }]]);
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ id: GUILD_ID, members: [] })],
      guildMemberCache: guildMembers,
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users).toEqual([]);
    expect(r.massMentionDenied).toBe(true);
  });

  test('@everyone role WITH MENTION_EVERYONE → expands via guild.members.cache (NOT role.members)', () => {
    // discord.js may not surface all members through @everyone's
    // role.members; resolveMentionableSelection has a guard that
    // routes @everyone-role expansion to guild.members.cache instead.
    // Pin that branch.
    const u1 = makeUser('100000000000000001');
    const u2 = makeUser('100000000000000002');
    const bot1 = makeUser('100000000000000099', { bot: true });
    const guildMembers = new Map([
      [u1.id, { user: u1 }],
      [u2.id, { user: u2 }],
      [bot1.id, { user: bot1 }],
    ]);
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ id: GUILD_ID, members: [] })],
      guildMemberCache: guildMembers,
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: true });
    expect(r.users.map((u) => u.id).sort()).toEqual([u1.id, u2.id].sort());
    expect(r.massMentionDenied).toBe(false);
  });

  test('cap short-circuits role expansion at QURL_SEND_MAX_RECIPIENTS (25) — does not over-collect 10k members', () => {
    // Mirrors the text-path @everyone cap-short-circuit. Defends
    // against a fleet of 1k+ guilds where role.members iteration would
    // otherwise build a 10k-entry userMap before the downstream
    // partition cap kicked in.
    const members = Array.from({ length: 60 }, (_, i) => ({
      user: makeUser(`100000000000000${String(i).padStart(3, '0')}`),
    }));
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ id: 'role-eng', members })],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.length).toBe(25);
  });

  test('cap priority: explicit user picks survive when role expansion would otherwise evict them', () => {
    // Picker-path analog of the text-path #323 round-4 fix
    // (recipient-parser.js: explicit `<@id>` mentions get cap priority
    // over `@everyone` expansion). resolveMentionableSelection seeds
    // `userMap` with picked users FIRST, then role expansion runs the
    // `if (userMap.size >= cap) break;` short-circuit — so the 5
    // explicit picks must occupy 5 of the 25 slots before any role
    // member can be added. A future refactor that flips the order, or
    // adds an inline cap inside the user-pick loop, would silently
    // break the invariant and let cache-iteration order decide which
    // picks survive — pin the fix here.
    const explicitIds = Array.from({ length: 5 }, (_, i) => `300000000000000${String(i).padStart(3, '0')}`);
    const explicitUsers = explicitIds.map((id) => makeUser(id));
    const cacheMembers = new Map(
      Array.from({ length: 30 }, (_, i) => {
        const id = `100000000000000${String(i).padStart(3, '0')}`;
        return [id, { user: makeUser(id) }];
      }),
    );
    const int = makeMentionableInteraction({
      pickedUsers: explicitUsers,
      pickedRoles: [makeRole({ id: GUILD_ID, members: [] })],
      guildMemberCache: cacheMembers,
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: true });
    const resultIds = r.users.map((u) => u.id);
    expect(resultIds.length).toBe(25);
    // All 5 explicit picks survived — none were evicted by role
    // expansion filling the cap. Field-fidelity check: also pin
    // OBJECT IDENTITY so the picked-user reference (which may carry
    // optional fields like `globalName` not present on the cache's
    // `member.user`) is the one that survives, not the cache view.
    // A regression that dropped the `userMap.has(memberId) continue`
    // would still pass the .toContain() check above.
    for (let i = 0; i < explicitUsers.length; i++) {
      expect(r.users.find((u) => u.id === explicitIds[i])).toBe(explicitUsers[i]);
    }
  });

  test('DM context (no guild) → empty users, no denial flag (no roles surface at all)', () => {
    // In a DM, interaction.roles never carries the @everyone role; the
    // helper should return cleanly with no expansion attempted.
    const int = makeMentionableInteraction({ inDM: true });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users).toEqual([]);
    expect(r.massMentionDenied).toBe(false);
  });

  test('pathological all-bot role iteration bounded at exactly 4× cap (=100, pins the multiplier)', () => {
    // The userMap.size cap-break never fires for an all-bot role
    // because bots only increment droppedFromRolesSet. Without the
    // iteration-count guard, a pathological 10k-bot role would
    // iterate all 10k entries. Bound at 4× QURL_SEND_MAX_RECIPIENTS
    // (=100); pin the multiplier exactly so a future halving back
    // to 2× would fail this test (≤100 would silently still pass).
    const config = require('../src/config');
    const ITER_BOUND = 4 * config.QURL_SEND_MAX_RECIPIENTS;
    const bots = Array.from({ length: 300 }, (_, i) => ({
      user: makeUser(`100000000000000${String(i).padStart(3, '0')}`, { bot: true }),
    }));
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ id: 'role-bots', members: bots })],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users).toEqual([]);
    // Exactly ITER_BOUND distinct bot IDs landed in the Set before
    // the iteration counter tripped the break. With unique bot IDs
    // (the fixture), set.size === inspectedFromRoles.
    expect(r.droppedFromRoles).toBe(ITER_BOUND);
  });

  test('ITER_BOUND accumulates ACROSS roles (function-scoped, not per-role)', () => {
    // The inspectedFromRoles counter is function-scoped intentionally
    // so a pathological role A can pre-truncate role B's expansion.
    // Without this test, a future refactor moving `let
    // inspectedFromRoles = 0;` inside the outer for-loop would silently
    // flip the semantics to per-role bounds — passing the single-role
    // test above but allowing N × 100-iteration grinds. Pin the
    // cross-role accumulation behavior here.
    const config = require('../src/config');
    const ITER_BOUND = 4 * config.QURL_SEND_MAX_RECIPIENTS;
    // Role A: 100 bots (will fully exhaust ITER_BOUND).
    const botsA = Array.from({ length: 100 }, (_, i) => ({
      user: makeUser(`100000000000000${String(i).padStart(3, '0')}`, { bot: true }),
    }));
    // Role B: 10 humans (would normally land in userMap, but role A
    // exhausted the iteration budget first).
    const humansB = Array.from({ length: 10 }, (_, i) => ({
      user: makeUser(`200000000000000${String(i).padStart(3, '0')}`),
    }));
    const int = makeMentionableInteraction({
      pickedRoles: [
        makeRole({ id: 'role-a-bots', members: botsA }),
        makeRole({ id: 'role-b-humans', members: humansB }),
      ],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    // Role B's humans were never reached — ITER_BOUND tripped during
    // role A's iteration.
    expect(r.users).toEqual([]);
    expect(r.droppedFromRoles).toBe(ITER_BOUND);
  });

  test('iteration cost vs count semantic: counter-before-dedupe causes overlap to consume an iter slot, blocking a later human', () => {
    // Pin the documented intent at commands.js: counter increments
    // BEFORE the dedupe check, so a bot in two picked roles costs 2
    // iteration slots even though droppedFromRolesSet has 1 entry.
    // A regression that hoisted the dedupe check above the counter
    // (in pursuit of "symmetry") would let a contrived "same N
    // members across M roles" pick grind for free.
    //
    // Earlier formulation of this test (just counted distinct bots
    // across 2 roles) couldn't distinguish the two orderings since
    // both produce the same .size. The fixture below DOES
    // distinguish:
    //
    //   Role A: 99 unique bots → counter=99, set.size=99
    //   Role B: 1 overlap bot, then 1 human
    //
    // Counter-before-dedupe (CURRENT):
    //   - B iter 1 (overlap): counter→100, dedupe-skip via set.
    //   - B iter 2 (human): bound check trips (100 >= 100), break.
    //   - users=[], droppedFromRoles=99.
    //
    // Hoisted-dedupe (THE REGRESSION):
    //   - B iter 1: dedupe-skip BEFORE counter, counter stays 99.
    //   - B iter 2: bound passes (99 < 100), counter→100, human added.
    //   - users=[human], droppedFromRoles=99.
    //
    // Asserting users.length === 0 is what catches the refactor.
    const roleABots = Array.from({ length: 99 }, (_, i) => ({
      user: makeUser(`200000000000000${String(i).padStart(3, '0')}`, { bot: true }),
    }));
    const overlapBot = roleABots[0].user; // same User ref as role A's first bot
    const human = makeUser('100000000000000001');
    const roleA = makeRole({ id: 'role-a', members: roleABots });
    const roleB = makeRole({
      id: 'role-b',
      members: [{ user: overlapBot }, { user: human }],
    });
    const int = makeMentionableInteraction({ pickedRoles: [roleA, roleB] });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    // Human blocked because the overlap bot consumed iter slot 100
    // before the human could be inspected — the hoisted-dedupe
    // regression would land the human instead.
    expect(r.users).toEqual([]);
    expect(r.droppedFromRoles).toBe(99);
  });

  test('overlap dedup: same bot in two picked roles counted once in droppedFromRoles', () => {
    // Set-backed droppedFromRoles means the same bot ID across two
    // picked roles contributes 1 to the user-visible count, not 2.
    // Pin against a future revert to counter semantics that would
    // surface "2 bot(s)" when only 1 distinct bot existed.
    const bot1 = makeUser('100000000000000099', { bot: true });
    const u1 = makeUser('100000000000000001');
    const roleA = makeRole({
      id: 'role-a',
      members: [{ user: bot1 }, { user: u1 }],
    });
    const roleB = makeRole({
      id: 'role-b',
      members: [{ user: bot1 }],
    });
    const int = makeMentionableInteraction({
      pickedRoles: [roleA, roleB],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.map((u) => u.id)).toEqual([u1.id]);
    expect(r.droppedFromRoles).toBe(1);
  });

  test('named-role overlap: directly-picked user object identity preserved (cap-priority parity with @everyone)', () => {
    // Symmetric with the @everyone-via-guild.members.cache cap-priority
    // test, but for a named role. The directly-picked User object must
    // survive when the same id also appears in a picked role's
    // .members — userMap.has(memberId) continue skips the role-side
    // overwrite, so optional fields like `globalName` on the picker's
    // User stick around for downstream alias rendering.
    const u1Picked = makeUser('100000000000000001');
    // A separate, distinguishable User object with the SAME id — what
    // role.members would surface (it's a different object reference).
    const u1FromRole = { ...makeUser('100000000000000001'), tag: 'from-role-view' };
    const role = makeRole({
      id: 'role-eng',
      members: [{ user: u1FromRole }],
    });
    const int = makeMentionableInteraction({
      pickedUsers: [u1Picked],
      pickedRoles: [role],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.length).toBe(1);
    // Object identity preserved: it's u1Picked, NOT u1FromRole.
    expect(r.users[0]).toBe(u1Picked);
    expect(r.users[0]).not.toBe(u1FromRole);
  });

  test('directly-picked bot + same bot in role → droppedFromRoles 0 (partition reports it via droppedBots)', () => {
    // The role-side dedupe-before-bot-check skip means a bot that's
    // already in userMap (from interaction.users) doesn't ALSO tick
    // the role counter. Downstream partitionRecipients reports it via
    // droppedBots; surfacing two warnings for the same bot would be
    // a UX nit.
    const bot1 = makeUser('100000000000000099', { bot: true });
    const role = makeRole({
      id: 'role-with-bot',
      members: [{ user: bot1 }],
    });
    const int = makeMentionableInteraction({
      pickedUsers: [bot1],
      pickedRoles: [role],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    // Bot lands in users array (caller's partitionRecipients drops it).
    expect(r.users.map((u) => u.id)).toEqual([bot1.id]);
    // Role-side did NOT also tick the role-bot counter.
    expect(r.droppedFromRoles).toBe(0);
  });

  test('bot-only role pick → droppedFromRoles counts the filtered bots', () => {
    // Without this signal the call site has no way to differentiate a
    // truly empty pick (silent return) from a role-of-bots pick (where
    // the user clicked a role and the bot filtered everything) — the
    // user would get zero feedback. Pin the count.
    const bot1 = makeUser('100000000000000091', { bot: true });
    const bot2 = makeUser('100000000000000092', { bot: true });
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({
        id: 'role-bots',
        members: [{ user: bot1 }, { user: bot2 }],
      })],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users).toEqual([]);
    expect(r.droppedFromRoles).toBe(2);
    expect(r.massMentionDenied).toBe(false);
  });

  test('mixed role: non-bots survive, bots increment droppedFromRoles', () => {
    const u1 = makeUser('100000000000000001');
    const bot1 = makeUser('100000000000000091', { bot: true });
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({
        id: 'role-eng',
        members: [{ user: u1 }, { user: bot1 }],
      })],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.map((u) => u.id)).toEqual([u1.id]);
    expect(r.droppedFromRoles).toBe(1);
  });

  test('everyoneCacheCold: @everyone WITH perm but missing guild.members.cache → flag set, no expansion', () => {
    // Surfaces the "cold cache, try again" UX signal so the caller
    // can render a "Member cache not yet ready" reason instead of a
    // silent deferUpdate-only no-op.
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ id: GUILD_ID, members: [] })],
      // guildMemberCache omitted → guild.members.cache is undefined
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: true });
    expect(r.users).toEqual([]);
    expect(r.everyoneCacheCold).toBe(true);
    expect(r.massMentionDenied).toBe(false);
  });

  test('everyoneCacheCold: @everyone WITH perm but EMPTY guild.members.cache → flag set (cache defined but no entries)', () => {
    // The "cache exists but is empty" case looks identical to "cache
    // missing" from the user's perspective — both yield zero expansion.
    // The helper treats them the same.
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ id: GUILD_ID, members: [] })],
      guildMemberCache: new Map(), // defined but size === 0
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: true });
    expect(r.users).toEqual([]);
    expect(r.everyoneCacheCold).toBe(true);
  });

  test('everyoneCacheCold stays false when @everyone is DENIED (cache state irrelevant)', () => {
    // When MENTION_EVERYONE is denied, the cache state is irrelevant
    // because we never look at it. massMentionDenied is the relevant
    // signal; everyoneCacheCold should NOT also fire and clutter the
    // warnings.
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ id: GUILD_ID, members: [] })],
      // No guildMemberCache (would be cold if perm were granted)
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.massMentionDenied).toBe(true);
    expect(r.everyoneCacheCold).toBe(false);
  });

  test('defense: role member with undefined .user is skipped (partial GuildMember from sparse fetch)', () => {
    // In standard discord.js, `member.user` is always populated. A
    // partial GuildMember from a sparse `members.fetch({ withPresences:
    // false, time: 100 })` can carry an undefined `.user`. Without the
    // defense, downstream partitionRecipients would deref `u.bot` /
    // `u.id` on undefined and throw. Pin the skip.
    const u1 = makeUser('100000000000000001');
    // Build members map directly (makeRole helper requires m.user.id).
    const role = ['role-eng', {
      id: 'role-eng',
      mentionable: true,
      members: new Map([
        ['100000000000000091', { user: undefined }],
        [u1.id, { user: u1 }],
        ['100000000000000092', { /* no .user property at all */ }],
      ]),
    }];
    const int = makeMentionableInteraction({
      pickedRoles: [role],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users.map((u) => u.id)).toEqual([u1.id]);
    expect(r.droppedFromRoles).toBe(0);
  });

  test('returns the documented shape: { users, massMentionDenied, droppedFromRoles, everyoneCacheCold, roleMentionsDenied }', () => {
    // Pinning test: a new return field added without updating this
    // assertion will fail loudly here. If you're hitting this after
    // adding a field, update the sorted-keys list AND verify every
    // caller of resolveMentionableSelection handles the new field
    // (handleConfirmUserSelect at minimum).
    const int = makeMentionableInteraction({});
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(Object.keys(r).sort()).toEqual(['droppedFromRoles', 'everyoneCacheCold', 'massMentionDenied', 'roleMentionsDenied', 'users']);
  });

  describe('role-mention permission gate (#326)', () => {
    // Picker-path parity with the parser-path #326 gate. Discord's
    // picker filters non-mentionable roles client-side, but a forged
    // interaction would otherwise bypass the gate — defense-in-depth.

    test('mentionable: false WITHOUT canMentionEveryone → roleMentionsDenied entry, members NOT expanded', () => {
      const u1 = makeUser('100000000000000001');
      const u2 = makeUser('100000000000000002');
      const role = makeRole({
        id: 'role-admin',
        members: [{ user: u1 }, { user: u2 }],
        mentionable: false,
      });
      const int = makeMentionableInteraction({ pickedRoles: [role] });
      const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
      expect(r.users).toEqual([]);
      expect(r.roleMentionsDenied).toEqual(['role-admin']);
    });

    test('mentionable: false WITH canMentionEveryone → expands normally, no deny', () => {
      const u1 = makeUser('100000000000000001');
      const role = makeRole({
        id: 'role-admin',
        members: [{ user: u1 }],
        mentionable: false,
      });
      const int = makeMentionableInteraction({ pickedRoles: [role] });
      const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: true });
      expect(r.users.map((u) => u.id)).toEqual([u1.id]);
      expect(r.roleMentionsDenied).toEqual([]);
    });

    test('mentionable: true WITHOUT canMentionEveryone → expands normally (per-role bypass)', () => {
      const u1 = makeUser('100000000000000001');
      const role = makeRole({
        id: 'role-public',
        members: [{ user: u1 }],
        mentionable: true,
      });
      const int = makeMentionableInteraction({ pickedRoles: [role] });
      const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
      expect(r.users.map((u) => u.id)).toEqual([u1.id]);
      expect(r.roleMentionsDenied).toEqual([]);
    });

    test('multiple denied roles surface independently (array, not boolean)', () => {
      const u1 = makeUser('100000000000000001');
      const u2 = makeUser('100000000000000002');
      const roleA = makeRole({ id: 'role-a', members: [{ user: u1 }], mentionable: false });
      const roleB = makeRole({ id: 'role-b', members: [{ user: u2 }], mentionable: false });
      const int = makeMentionableInteraction({ pickedRoles: [roleA, roleB] });
      const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
      expect(r.roleMentionsDenied.sort()).toEqual(['role-a', 'role-b']);
      expect(r.users).toEqual([]);
    });

    test('mix of denied + allowed roles → only denied lands in roleMentionsDenied', () => {
      const u1 = makeUser('100000000000000001');
      const u2 = makeUser('100000000000000002');
      const allowed = makeRole({ id: 'role-allowed', members: [{ user: u1 }], mentionable: true });
      const denied = makeRole({ id: 'role-denied', members: [{ user: u2 }], mentionable: false });
      const int = makeMentionableInteraction({ pickedRoles: [allowed, denied] });
      const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
      expect(r.users.map((u) => u.id)).toEqual([u1.id]);
      expect(r.roleMentionsDenied).toEqual(['role-denied']);
    });

    test('denied role does NOT increment droppedFromRoles (gate fires before bot filter)', () => {
      // droppedFromRoles is "bots filtered from picked roles"; a denied
      // role short-circuits before the bot filter loop runs, so it
      // contributes 0 to that counter. Pin so a regression that moved
      // the gate AFTER the bot filter (and double-counted bot members)
      // surfaces here.
      const bot = makeUser('100000000000000091', { bot: true });
      const role = makeRole({
        id: 'role-denied-bot',
        members: [{ user: bot }],
        mentionable: false,
      });
      const int = makeMentionableInteraction({ pickedRoles: [role] });
      const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
      expect(r.droppedFromRoles).toBe(0);
      expect(r.roleMentionsDenied).toEqual(['role-denied-bot']);
    });

    test('undefined role object (partial-fetch edge) → skipped, NOT routed through deny path', () => {
      // Theoretical edge: `interaction.roles.entries()` carries `[id,
      // roleObject]` pairs in production, but a partial-fetch shape
      // could deliver `[id, undefined]`. Without the
      // `if (!isEveryoneRole && !role) continue;` short-circuit, the
      // per-role gate (`role?.mentionable !== true`) would route the
      // cache-miss through the deny path and surface
      // "Non-mentionable role" copy for what's actually a missing
      // object. Symmetric with the text-path parser's
      // `if (!role) { pushInvalidIfNew(...) }` invalid-role branch.
      const int = makeMentionableInteraction({
        pickedRoles: [['orphan-id', undefined]],
      });
      const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
      expect(r.users).toEqual([]);
      expect(r.roleMentionsDenied).toEqual([]);
      expect(r.massMentionDenied).toBe(false);
    });

    test('@everyone-role (role.id === guild.id) NOT routed to roleMentionsDenied — uses massMentionDenied', () => {
      // The @everyone branch above (isEveryoneRole) catches role.id ===
      // guild.id BEFORE the per-role gate fires, so massMentionDenied
      // (not roleMentionsDenied) carries the @everyone-specific signal.
      // A regression that ran the per-role gate FIRST would split the
      // copy across two surfaces and confuse the user.
      const int = makeMentionableInteraction({
        pickedRoles: [[GUILD_ID, { id: GUILD_ID, members: new Map(), mentionable: false }]],
      });
      const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
      expect(r.massMentionDenied).toBe(true);
      expect(r.roleMentionsDenied).toEqual([]);
    });
  });
});

describe('resolveRoleNames (#326 helper)', () => {
  // Closed-contract pin for the cache-lookup-with-fallback helper
  // used by both the text-path (`handleQurlSlashSend`) and picker-
  // path (`handleConfirmUserSelect`) `roleMentionsDeniedNames`
  // resolution. Centralizing tests here means a future refactor of
  // the helper signature surfaces in one place rather than via
  // brittle handler-level integration tests.

  function makeGuild(rolesById = {}) {
    const cache = new Map(Object.entries(rolesById));
    return { roles: { cache } };
  }

  test('returns [] for null / undefined / empty ids (defensive contract)', () => {
    const guild = makeGuild({});
    expect(resolveRoleNames(guild, null)).toEqual([]);
    expect(resolveRoleNames(guild, undefined)).toEqual([]);
    expect(resolveRoleNames(guild, [])).toEqual([]);
  });

  test('guild=null/undefined with non-empty ids → unknown-role fallback per entry (DM context shouldn\'t reach here, but optional chains carry through)', () => {
    // Defensive: the text-path call site is reachable from DM context
    // (where `interaction.guild` is null) even though the parser's
    // role loop won't actually populate `parsed.roleMentionsDenied`
    // without a guild. If a future caller path bypasses that
    // invariant, the optional chain `guild?.roles?.cache?.get(id)`
    // returns undefined and the `||` falls through to `unknown-role`
    // — symmetric with the cache-miss behavior, not a hard crash.
    expect(resolveRoleNames(null, ['7000'])).toEqual(['unknown-role']);
    expect(resolveRoleNames(undefined, ['7000'])).toEqual(['unknown-role']);
  });

  test('resolves cached role IDs to their names', () => {
    const guild = makeGuild({
      '7000': { id: '7000', name: 'admin' },
      '7001': { id: '7001', name: 'mods' },
    });
    expect(resolveRoleNames(guild, ['7000', '7001'])).toEqual(['admin', 'mods']);
  });

  test('cache miss → `unknown-role` fallback (deleted-mid-flow race)', () => {
    const guild = makeGuild({});  // role 7000 not in cache
    expect(resolveRoleNames(guild, ['7000'])).toEqual(['unknown-role']);
  });

  test('empty-string role name → `unknown-role` fallback (pins `||` vs `??` rationale)', () => {
    // Discord enforces 1–100 char role names server-side, so empty
    // names shouldn't surface in legitimate flows — but a forged
    // interaction or future API edge could carry one. The `||` (not
    // `??`) in `resolveRoleNames` ensures an empty name falls through
    // to `unknown-role` rather than rendering `@` (the empty backtick
    // block would be visually broken). Pin the rationale.
    const guild = makeGuild({
      '7000': { id: '7000', name: '' },
    });
    expect(resolveRoleNames(guild, ['7000'])).toEqual(['unknown-role']);
  });

  test('mixed cache-hit / cache-miss / empty-name in one batch → fallback applies per-entry', () => {
    const guild = makeGuild({
      '7000': { id: '7000', name: 'admin' },
      '7002': { id: '7002', name: '' },
    });
    expect(resolveRoleNames(guild, ['7000', '7001', '7002'])).toEqual([
      'admin',
      'unknown-role',  // cache miss
      'unknown-role',  // empty name
    ]);
  });
});

describe('softenCooldown', () => {
  // Cancel-path helper: leave `residualMs` of cooldown remaining,
  // monotonically SHRINK (never extend). Tests use the real
  // QURL_SEND_COOLDOWN_MS via config (mocked in the test setup).
  const config = require('../src/config');
  const COOLDOWN_MS = config.QURL_SEND_COOLDOWN_MS;

  beforeEach(() => sendCooldowns.clear());

  test('no-op when no cooldown is set', () => {
    softenCooldown('u1', 5000);
    expect(sendCooldowns.has('u1')).toBe(false);
  });

  test('softens a fresh cooldown so ~5s remain', () => {
    const now = Date.now();
    sendCooldowns.set('u1', now);
    softenCooldown('u1', 5000);
    expect(sendCooldowns.has('u1')).toBe(true);
    // Resulting `last` should be approximately now - (COOLDOWN - 5000)
    // so remaining time is 5000ms.
    const last = sendCooldowns.get('u1');
    expect(last).toBeLessThanOrEqual(now - (COOLDOWN_MS - 5000) + 5);
    expect(last).toBeGreaterThanOrEqual(now - (COOLDOWN_MS - 5000) - 5);
  });

  test('does NOT extend an already-short remaining cooldown', () => {
    // existing `last` is older than the soften target → remaining is
    // already less than 5s. Soften must NOT bump it back up.
    const ancient = Date.now() - (COOLDOWN_MS - 1000);  // 1s remaining
    sendCooldowns.set('u1', ancient);
    softenCooldown('u1', 5000);
    expect(sendCooldowns.get('u1')).toBe(ancient);
  });

  test('residualMs=0 effectively clears (last pushed far enough back that remaining=0)', () => {
    sendCooldowns.set('u1', Date.now());
    softenCooldown('u1', 0);
    expect(isOnCooldown('u1')).toBe(false);
  });

  test('soften reorders the Map: softened entry moves to the end of iteration order', () => {
    // LRU iteration contract: setCooldown's bulk eviction (commands.js:~248)
    // drops the oldest N keys in iteration order. softenCooldown deletes-
    // then-sets so a soften refreshes the user's iteration position to
    // the end, keeping them resident through bulk evictions. Without
    // this, a Cancel-then-Cancel sequence would still drop the active
    // user during the next eviction wave.
    sendCooldowns.set('uA', Date.now());
    sendCooldowns.set('uB', Date.now());
    sendCooldowns.set('uC', Date.now());
    // Initial iteration order: A, B, C.
    softenCooldown('uA', 5000);
    // After softening A, iteration order should be: B, C, A.
    expect(Array.from(sendCooldowns.keys())).toEqual(['uB', 'uC', 'uA']);
  });
});

describe('parseLocationInput', () => {
  test('Google Maps short URL passes through verbatim', () => {
    const r = parseLocationInput('https://goo.gl/maps/abc123');
    expect(r.locationUrl).toBe('https://goo.gl/maps/abc123');
    expect(r.placeId).toBeUndefined();
    expect(r.text).toBeUndefined();
  });

  test('Google Maps place URL passes through with derived name', () => {
    const r = parseLocationInput('https://www.google.com/maps/place/Eiffel+Tower/@48.85,2.29,17z');
    expect(r.locationUrl).toContain('google.com/maps/place');
    expect(r.locationName).toBeTruthy();
  });

  test('api=1&query= form extracts the name (round-trip for re-shared qURL map URLs)', () => {
    // The URL form resolveLocation constructs: `/maps/search/?api=1&
    // query=<name>&query_place_id=<id>`. If a sender re-shares one of
    // these URLs through /qurl map, parseLocationInput needs to pull
    // the name out so the recipient embed has a label — without this
    // branch, only `?q=<name>` and `/place/<name>` were extracted.
    const url = 'https://www.google.com/maps/search/?api=1&query=Eiffel+Tower&query_place_id=ChIJxxx';
    const r = parseLocationInput(url);
    expect(r.locationUrl).toBe(url);
    expect(r.locationName).toBe('Eiffel Tower');
  });

  test('place_id sentinel parses into a placeId branch (no URL synthesized)', () => {
    // Wire contract: the autocomplete handler encodes selected places
    // as `qurl_place:<placeId>` so the slash submit can route through
    // a Places Details lookup instead of synthesizing a per-viewer
    // /maps/search/<text> URL. The sentinel prefix MUST match
    // PLACE_ID_SENTINEL_PREFIX in places.js — a drift here breaks
    // every in-flight autocomplete pick.
    const r = parseLocationInput('qurl_place:ChIJ37FjGE63t4kRD2_jXSF1F9o');
    expect(r.placeId).toBe('ChIJ37FjGE63t4kRD2_jXSF1F9o');
    expect(r.locationUrl).toBeNull();
    expect(r.locationName).toBeNull();
  });

  test('plain place name returns text branch for server-side resolution', () => {
    // The free-text branch no longer synthesizes a /maps/search/<text>
    // URL — that URL was the source of the per-recipient geo-bias bug.
    // parseLocationInput now defers resolution to resolveLocation,
    // which hits Places Find Place at send time and pins to a place_id.
    const r = parseLocationInput('Eiffel Tower, Paris');
    expect(r.locationUrl).toBeNull();
    expect(r.locationName).toBeNull();
    expect(r.text).toBe('Eiffel Tower, Paris');
  });

  test('plain non-URL text returns text branch', () => {
    const r = parseLocationInput('not a url just plain text input');
    expect(r.text).toBe('not a url just plain text input');
    expect(r.locationUrl).toBeNull();
  });

  test('https URL that does NOT match MAPS_URL_PATTERNS falls through to text branch', () => {
    // A non-Google https URL fails every MAPS_URL_PATTERNS regex, so
    // parseLocationInput hands it to resolveLocation as free text.
    // resolveLocation will then ask Places to find a real place — the
    // spoofed host never lands in locationUrl. (Previously the spoofed
    // URL was URL-encoded into a synth /maps/search/<text> URL; the
    // recipient-visible behavior is equivalent — google.com host —
    // but now goes through a place_id resolution rather than a
    // per-viewer-biased search query.)
    const r = parseLocationInput('https://evil.example.com/maps/place/x');
    expect(r.locationUrl).toBeNull();
    expect(r.text).toBe('https://evil.example.com/maps/place/x');
  });

  test('malformed %-encoding in the input does not throw', () => {
    expect(() => parseLocationInput('https://www.google.com/maps/place/%ZZ-broken')).not.toThrow();
  });

  test('spoofed host (google.com.evil.com) fails the regex AND falls through to text branch', () => {
    // Defense-in-depth contract: MAPS_URL_PATTERNS pins the literal
    // `google.com/` token (slash forces an end-of-host boundary), so a
    // spoofed host like `google.com.evil.com/maps/place/x` cannot match
    // any pattern. parseLocationInput therefore routes the whole input
    // to the text branch — isGoogleMapsURL never gets a chance to look
    // at the spoofed host. The conditional `if (detectedUrl &&
    // isGoogleMapsURL(detectedUrl))` remains as defense-in-depth in
    // case a future pattern relaxes the host pin; this test pins the
    // current contract.
    //
    // Downstream: resolveLocation feeds the spoofed text to Places
    // Find Place. Whatever Places returns becomes the locationUrl —
    // a google.com host, place_id-pinned — so the spoofed host never
    // becomes the link target regardless of what Places interprets.
    const spoofed = 'https://google.com.evil.com/maps/place/Eiffel-Tower';
    const r = parseLocationInput(spoofed);
    expect(r.locationUrl).toBeNull();
    expect(r.text).toBe(spoofed);
  });
});

describe('resolveLocation', () => {
  // resolveLocation is the server-side hop that turns a parsed
  // location input into a place_id-pinned URL. URL inputs short-
  // circuit (no API call). Sentinel + free-text inputs hit Places
  // — these tests pin the three reason codes and the success shapes.
  beforeEach(() => {
    mockSearchPlaces.mockReset().mockResolvedValue([]);
    mockFindPlaceFromText.mockReset().mockResolvedValue(null);
    mockGetPlaceDetails.mockReset().mockResolvedValue(null);
  });

  test('URL branch passes through without an API call', async () => {
    const r = await resolveLocation({
      locationUrl: 'https://goo.gl/maps/abc123',
      locationName: 'My Place',
    });
    expect(r.ok).toBe(true);
    expect(r.locationUrl).toBe('https://goo.gl/maps/abc123');
    expect(r.locationName).toBe('My Place');
    expect(mockFindPlaceFromText).not.toHaveBeenCalled();
    expect(mockGetPlaceDetails).not.toHaveBeenCalled();
  });

  test('placeId branch calls getPlaceDetails and builds a place_id-pinned URL', async () => {
    mockGetPlaceDetails.mockResolvedValueOnce({
      placeId: 'ChIJ37FjGE63t4kRD2_jXSF1F9o',
      name: 'The White House',
      address: '1600 Pennsylvania Ave NW, Washington, DC',
    });
    const r = await resolveLocation({ placeId: 'ChIJ37FjGE63t4kRD2_jXSF1F9o' });
    expect(r.ok).toBe(true);
    expect(r.locationName).toBe('The White House');
    // The canonical URL must carry both the human-readable query and
    // the place_id pin. The place_id is what eliminates per-viewer
    // geo-bias — without it, the URL would degrade to /maps/search/<text>
    // behavior which is the bug we're fixing.
    expect(r.locationUrl).toContain('query_place_id=ChIJ37FjGE63t4kRD2_jXSF1F9o');
    expect(mockGetPlaceDetails).toHaveBeenCalledWith('ChIJ37FjGE63t4kRD2_jXSF1F9o');
  });

  test('text branch calls findPlaceFromText and pins to the top result', async () => {
    mockFindPlaceFromText.mockResolvedValueOnce({
      placeId: 'ChIJxxx',
      name: 'The White House',
      address: '1600 Pennsylvania Ave NW',
    });
    const r = await resolveLocation({ text: 'the whitehouse' });
    expect(r.ok).toBe(true);
    expect(r.locationUrl).toContain('query_place_id=ChIJxxx');
    expect(r.locationName).toBe('The White House');
  });

  test('text branch returns not_found when Places has no candidates', async () => {
    mockFindPlaceFromText.mockResolvedValueOnce(null);
    const r = await resolveLocation({ text: 'asdfasdfasdf' });
    expect(r.ok).toBe(false);
    expect(r.reason).toBe(RESOLVE_REASON.NOT_FOUND);
  });

  test('placeId branch returns not_found when Place Details returns null', async () => {
    mockGetPlaceDetails.mockResolvedValueOnce(null);
    const r = await resolveLocation({ placeId: 'ChIJ-deleted-place' });
    expect(r.ok).toBe(false);
    expect(r.reason).toBe(RESOLVE_REASON.NOT_FOUND);
  });

  test('text branch returns error when the Places call throws', async () => {
    mockFindPlaceFromText.mockRejectedValueOnce(new Error('upstream timeout'));
    const r = await resolveLocation({ text: 'somewhere' });
    expect(r.ok).toBe(false);
    expect(r.reason).toBe(RESOLVE_REASON.ERROR);
  });

  test('hard-fails with no_api_key when GOOGLE_MAPS_API_KEY is unset', async () => {
    // Mutate the already-mocked config in place rather than
    // jest.resetModules — resetting the module registry would force
    // commands.js to re-execute, which re-runs registerFlow on a
    // fresh dispatcher and breaks every downstream "duplicate-register
    // throws" test in the suite.
    const configMock = require('../src/config');
    const orig = configMock.GOOGLE_MAPS_API_KEY;
    delete configMock.GOOGLE_MAPS_API_KEY;
    try {
      const r = await resolveLocation({ text: 'eiffel tower' });
      expect(r.ok).toBe(false);
      expect(r.reason).toBe(RESOLVE_REASON.NO_API_KEY);
    } finally {
      configMock.GOOGLE_MAPS_API_KEY = orig;
    }
  });
});

describe('handleAutocomplete', () => {
  // The autocomplete dispatcher must (a) gate on commandName + subcommand
  // + focused-option-name, (b) honor the min-length cap, (c) skip Places
  // for URL inputs (where suggestions would just clutter), and (d) build
  // sentinel `qurl_place:<placeId>` values that the slash submit can
  // round-trip through resolveLocation. Together these contracts ensure
  // the autocomplete UX layer feeds the same per-place URL pinning that
  // the server-side fallback uses.
  beforeEach(() => {
    mockSearchPlaces.mockReset().mockResolvedValue([]);
    // The autocomplete-failure burst counter is module-level state;
    // reset between tests so a leftover count can't trip the sampled
    // warn on a later test's first failure.
    _resetAutocompleteFailureBurst();
  });

  function makeAutocompleteInteraction({
    subcommand = 'map',
    focused = { name: 'location', value: 'whitehouse' },
    guildId = 'guild-1',
  } = {}) {
    const respond = jest.fn().mockResolvedValue(undefined);
    return {
      commandName: 'qurl',
      guildId,
      respond,
      options: {
        getSubcommand: () => subcommand,
        getFocused: () => focused,
      },
    };
  }

  // Generate a place_id-shaped string for tests. The autocomplete
  // handler filters out entries that don't match the documented
  // place_id char class + length floor, so any fake fixture must
  // mimic the shape (>=16 chars of [A-Za-z0-9_-]).
  function fakePlaceId(seed) {
    const s = String(seed);
    return s.length >= 16 ? s : `ChIJ${'a'.repeat(16 - s.length)}${s}`;
  }

  test('responds empty for non-qurl commands', async () => {
    const int = makeAutocompleteInteraction();
    int.commandName = 'link';
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
    expect(mockSearchPlaces).not.toHaveBeenCalled();
  });

  test('responds empty for DM autocomplete (no guildId)', async () => {
    // handleQurlMap rejects DMs at submit time, but Discord could
    // still deliver an autocomplete interaction without a guildId.
    // Without this gate a DM-typed query would burn the operator's
    // global GOOGLE_MAPS_API_KEY quota for a send that's about to
    // be rejected anyway.
    const int = makeAutocompleteInteraction({ guildId: null });
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
    expect(mockSearchPlaces).not.toHaveBeenCalled();
  });

  test('responds empty for /qurl send (only /qurl map has suggestions)', async () => {
    const int = makeAutocompleteInteraction({ subcommand: 'send' });
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
    expect(mockSearchPlaces).not.toHaveBeenCalled();
  });

  test('responds empty for /qurl map with a non-location focused option', async () => {
    const int = makeAutocompleteInteraction({ focused: { name: 'personal-message', value: 'hi' } });
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
    expect(mockSearchPlaces).not.toHaveBeenCalled();
  });

  test('skips Places call for partial queries below the min-length cap', async () => {
    // Cuts per-keystroke Places cost. The user typically pauses for the
    // dropdown to populate; without this gate single-letter prefixes
    // would fire a Places call on every keystroke.
    const int = makeAutocompleteInteraction({ focused: { name: 'location', value: 'a' } });
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
    expect(mockSearchPlaces).not.toHaveBeenCalled();
  });

  test('skips Places call when input already looks like a URL', async () => {
    // URLs are already stable identifiers — autocomplete suggestions
    // would just clutter the dropdown. The slash submit's URL branch
    // passes them through verbatim.
    const int = makeAutocompleteInteraction({ focused: { name: 'location', value: 'https://goo.gl/maps/abc' } });
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
    expect(mockSearchPlaces).not.toHaveBeenCalled();
  });

  test('returns sentinel-encoded choices with name + address labels', async () => {
    mockSearchPlaces.mockResolvedValueOnce([
      { placeId: fakePlaceId('whitehouse_dc_id'), name: 'The White House', address: '1600 Pennsylvania Ave NW, Washington, DC' },
      { placeId: fakePlaceId('whitehouse_uk_id'), name: 'Whitehouse Pub', address: 'Manchester, UK' },
    ]);
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    expect(mockSearchPlaces).toHaveBeenCalledWith('whitehouse');
    expect(int.respond).toHaveBeenCalledTimes(1);
    const choices = int.respond.mock.calls[0][0];
    expect(choices).toHaveLength(2);
    expect(choices[0]).toEqual({
      name: 'The White House — 1600 Pennsylvania Ave NW, Washington, DC',
      value: `qurl_place:${fakePlaceId('whitehouse_dc_id')}`,
    });
    expect(choices[1].value).toBe(`qurl_place:${fakePlaceId('whitehouse_uk_id')}`);
    // Disambiguation is the whole point: the user-visible label has
    // to differentiate "White House DC" from "Whitehouse Pub UK",
    // otherwise the autocomplete picker is no better than free text.
    expect(choices[0].name).not.toBe(choices[1].name);
  });

  test('truncates a label exceeding the 100-char Discord cap (UTF-16 units)', async () => {
    const longAddress = '1234 Very Long Street Name, Somewhere Far Away, In A Large City With A Long Name, Region, Country 99999';
    mockSearchPlaces.mockResolvedValueOnce([{ placeId: fakePlaceId('longlabel'), name: 'Place', address: longAddress }]);
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    const choice = int.respond.mock.calls[0][0][0];
    expect(choice.name.length).toBeLessThanOrEqual(100);
    // The sentinel value is short enough to never need truncation
    // (qurl_place: + 27-char place_id ≈ 38). Pin this — if a future
    // change starts packing more into the value we want a test to fail.
    expect(choice.value.length).toBeLessThanOrEqual(100);
  });

  test('boundary — exactly 100 UTF-16 units ending in a lone high surrogate gets backed off', async () => {
    // Defense-in-depth: a label that's exactly at the cap AND ends
    // mid-surrogate-pair would otherwise slip past a truncation-only
    // gate. The check runs on every choice, not just truncated ones.
    // 98 ASCII + 1 high surrogate + 1 low surrogate = 100 UTF-16
    // units; the final pair is the boundary. We then trim the source
    // to 98 + 1 high surrogate = 99 units, ending in a lone high
    // surrogate (this is contrived — Places wouldn't return this — but
    // pins the always-check contract).
    const malformed = 'a'.repeat(99) + '\uD83D'; // lone high surrogate at index 99 → length 100
    expect(malformed.length).toBe(100);
    mockSearchPlaces.mockResolvedValueOnce([{ placeId: fakePlaceId('boundary1'), name: malformed, address: '' }]);
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    const choice = int.respond.mock.calls[0][0][0];
    // Backed off by 1 — no lone high surrogate at the boundary.
    expect(choice.name.length).toBe(99);
    const loneHigh = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])/;
    expect(choice.name).not.toMatch(loneHigh);
  });

  test('truncation does not split a surrogate pair (emoji-heavy label stays valid UTF-16)', async () => {
    // Discord measures name length in UTF-16 code units; a naïve
    // codepoint slice could ship a string whose .length > 100 if the
    // first 100 codepoints contain many surrogate pairs, OR could
    // leave a lone high surrogate at the boundary. The UTF-16 slice
    // + surrogate-backoff must produce a string that's <= 100 units
    // AND has no orphan surrogate.
    const emoji = '🏛️'; // 🏛 + variation selector — 3 UTF-16 units
    const name = (emoji + 'X').repeat(40); // 160 UTF-16 units of mixed surrogate + ASCII
    mockSearchPlaces.mockResolvedValueOnce([{ placeId: fakePlaceId('emojiplace'), name, address: '' }]);
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    const choice = int.respond.mock.calls[0][0][0];
    expect(choice.name.length).toBeLessThanOrEqual(100);
    // No lone high surrogate at the boundary.
    const loneHigh = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])/;
    expect(choice.name).not.toMatch(loneHigh);
  });

  test('drops a choice whose value would exceed the 100-char Discord cap', async () => {
    // Defensive: Google docs leave place_id length open-ended. If a
    // future result ships an >89-char place_id, we'd produce a value
    // > 100 chars, which would fail Discord's API for the whole
    // response. Drop just that choice so the rest of the dropdown
    // still works.
    const good1 = fakePlaceId('good1_id');
    const good2 = fakePlaceId('good2_id');
    mockSearchPlaces.mockResolvedValueOnce([
      { placeId: good1, name: 'Good', address: 'addr' },
      { placeId: 'x'.repeat(95), name: 'Bad (too long)', address: 'addr' },
      { placeId: good2, name: 'Also Good', address: 'addr' },
    ]);
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    const choices = int.respond.mock.calls[0][0];
    expect(choices).toHaveLength(2);
    expect(choices.map(c => c.value)).toEqual([`qurl_place:${good1}`, `qurl_place:${good2}`]);
  });

  test('drops a choice whose name is missing (Places returned no main_text + no description)', async () => {
    // Places marks both main_text and description as optional. If both
    // are missing, searchPlaces yields { name: undefined }, and a
    // naive label would render as the literal string "undefined".
    // Discord also rejects empty/whitespace names, so the choice must
    // be skipped — pin both the skip behavior and that valid entries
    // around it still render.
    const valid = fakePlaceId('valid_for_label');
    mockSearchPlaces.mockResolvedValueOnce([
      { placeId: valid, name: 'Valid', address: 'addr' },
      { placeId: fakePlaceId('no_name_entry'), name: undefined, address: 'addr2' },
      { placeId: fakePlaceId('empty_name_xx'), name: '', address: 'addr3' },
    ]);
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    const choices = int.respond.mock.calls[0][0];
    expect(choices).toHaveLength(1);
    expect(choices[0].value).toBe(`qurl_place:${valid}`);
  });

  test('outer-catch handles a rejection from an early-return respond() (return await contract)', async () => {
    // The early-return guards use `return await interaction.respond([])`
    // (not bare `return`) so a rejected promise is routed through the
    // outer try/catch instead of leaking out of the async function. A
    // bare `return` would propagate the rejection to the dispatch
    // caller (which would surface as "this command is unresponsive"
    // to the user). Pin: the rejection IS caught + recovery fires.
    const int = makeAutocompleteInteraction({ guildId: null });
    let respondCallCount = 0;
    int.respond = jest.fn(async () => {
      respondCallCount += 1;
      if (respondCallCount === 1) throw new Error('Unknown interaction');
      // The outer-catch's best-effort fallback respond([]) — let this
      // one resolve so we don't double-throw.
    });
    await handleAutocomplete(int);
    // Outer catch fired its fallback respond([]) on the rejection.
    expect(respondCallCount).toBe(2);
  });

  test('drops a choice whose place_id fails the documented shape check', async () => {
    // Mirror of the decodePlaceIdSentinel shape gate at encode time.
    // If Google ever ships a malformed place_id (chars outside
    // [A-Za-z0-9_-] or shorter than 16 chars), skip that entry rather
    // than render a dud choice that submit-time decode would reject.
    const valid = fakePlaceId('valid_id_one');
    mockSearchPlaces.mockResolvedValueOnce([
      { placeId: valid, name: 'Valid', address: 'addr' },
      { placeId: 'tooshort', name: 'Bad short', address: 'addr' },
      { placeId: 'has spaces in it just bad', name: 'Bad chars', address: 'addr' },
    ]);
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    const choices = int.respond.mock.calls[0][0];
    expect(choices).toHaveLength(1);
    expect(choices[0].value).toBe(`qurl_place:${valid}`);
  });

  test('caps results at 25 (Discord choice limit)', async () => {
    mockSearchPlaces.mockResolvedValueOnce(
      Array.from({ length: 40 }, (_, i) => ({
        placeId: fakePlaceId(`place_id_${i}_padding_xyz`),
        name: `Place ${i}`,
        address: 'addr',
      })),
    );
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    expect(int.respond.mock.calls[0][0]).toHaveLength(25);
  });

  test('responds empty (does not throw) when Places API throws', async () => {
    // Autocomplete must not surface "this command failed" toasts on a
    // transient Places hiccup. Empty response keeps the dropdown UX
    // graceful — the user can still type free text and the server-side
    // fallback in resolveLocation will retry at send time.
    mockSearchPlaces.mockRejectedValueOnce(new Error('Places API status: OVER_QUERY_LIMIT'));
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
  });

  test('failure burst counter emits one warn per BURST failures (SRE outage signal)', async () => {
    // Per-call log is `debug` to avoid keystroke-rate spam. The sampled
    // `warn` is the visible signal for SRE that autocomplete is degraded
    // (vs. just no traffic). Pins the contract: one warn fires when the
    // burst counter crosses AUTOCOMPLETE_FAILURE_LOG_BURST, and resets.
    const logger = require('../src/logger');
    logger.warn.mockClear();
    for (let i = 0; i < AUTOCOMPLETE_FAILURE_LOG_BURST - 1; i++) {
      mockSearchPlaces.mockRejectedValueOnce(new Error('Places API status: UNKNOWN_ERROR'));
      await handleAutocomplete(makeAutocompleteInteraction());
    }
    // Just below the burst threshold — no warn yet.
    const burstWarns = () => logger.warn.mock.calls.filter(
      (call) => call[0] === 'autocomplete handler failure burst',
    ).length;
    expect(burstWarns()).toBe(0);

    // The BURST-th failure trips the sampled warn.
    mockSearchPlaces.mockRejectedValueOnce(new Error('Places API status: UNKNOWN_ERROR'));
    await handleAutocomplete(makeAutocompleteInteraction());
    expect(burstWarns()).toBe(1);
    expect(logger.warn).toHaveBeenCalledWith(
      'autocomplete handler failure burst',
      expect.objectContaining({ count: AUTOCOMPLETE_FAILURE_LOG_BURST }),
    );

    // Counter reset — next burst starts fresh.
    for (let i = 0; i < AUTOCOMPLETE_FAILURE_LOG_BURST - 1; i++) {
      mockSearchPlaces.mockRejectedValueOnce(new Error('Places API status: UNKNOWN_ERROR'));
      await handleAutocomplete(makeAutocompleteInteraction());
    }
    expect(burstWarns()).toBe(1);
  });

  test('failure burst counter does not increment when the early-return respond() throws', async () => {
    // Narrow-catch contract: the burst counter is a "Places is
    // degraded" signal, not a generic handler-failure counter. If an
    // early-return `interaction.respond([])` throws (e.g. expired
    // interaction token), that's a Discord-side issue, not a Places
    // problem — the counter must NOT advance.
    const logger = require('../src/logger');
    logger.warn.mockClear();
    for (let i = 0; i < AUTOCOMPLETE_FAILURE_LOG_BURST + 5; i++) {
      const int = makeAutocompleteInteraction({ guildId: null }); // hits DM gate early-return
      int.respond = jest.fn(async () => { throw new Error('Unknown interaction'); });
      await handleAutocomplete(int);
    }
    const burstWarns = logger.warn.mock.calls.filter(
      (call) => call[0] === 'autocomplete handler failure burst',
    ).length;
    expect(burstWarns).toBe(0);
  });

  test('failure burst counter does not increment on the success path', async () => {
    // A successful autocomplete must NOT advance the burst counter,
    // otherwise the sampled warn would fire eventually during normal
    // operation and look like an outage in logs.
    const logger = require('../src/logger');
    logger.warn.mockClear();
    mockSearchPlaces.mockResolvedValue([{ placeId: 'ChIJ1', name: 'X', address: 'Y' }]);
    for (let i = 0; i < AUTOCOMPLETE_FAILURE_LOG_BURST + 5; i++) {
      await handleAutocomplete(makeAutocompleteInteraction());
    }
    const burstWarns = logger.warn.mock.calls.filter(
      (call) => call[0] === 'autocomplete handler failure burst',
    ).length;
    expect(burstWarns).toBe(0);
  });
});

describe('safeDecodeURIComponent', () => {
  test('decodes normal percent-encoding', () => {
    expect(safeDecodeURIComponent('Hello%20World')).toBe('Hello World');
  });

  test('returns raw input on malformed encoding (does not throw)', () => {
    expect(safeDecodeURIComponent('%ZZ')).toBe('%ZZ');
    expect(safeDecodeURIComponent('%')).toBe('%');
    expect(safeDecodeURIComponent('valid%20but%incomplete')).toBe('valid%20but%incomplete');
  });

  test('handles control chars passing through', () => {
    expect(safeDecodeURIComponent('plain')).toBe('plain');
  });
});

describe('cross-command cooldown contract', () => {
  // /qurl send and /qurl map share the sendCooldowns Map. setCooldown
  // from one MUST block the other — without this contract, a user
  // could bypass the per-user throttle by alternating entry points.
  beforeEach(() => sendCooldowns.clear());

  test('setCooldown via one user blocks isOnCooldown for the same user across all entry points', () => {
    setCooldown('uA');
    // The shared Map key is the user ID — no per-command bucket. All
    // three slash handlers consult the same isOnCooldown helper.
    expect(isOnCooldown('uA')).toBe(true);
  });

  test('clearCooldown unlocks all entry points for that user', () => {
    setCooldown('uA');
    clearCooldown('uA');
    expect(isOnCooldown('uA')).toBe(false);
  });

  test('cooldown is per-user, not global', () => {
    setCooldown('uA');
    expect(isOnCooldown('uA')).toBe(true);
    expect(isOnCooldown('uB')).toBe(false);
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
      droppedBots: 0,
    })).toBe('');
  });

  test('cappedCount line', () => {
    const out = renderRecipientWarnings({
      invalidTokens: [], cappedCount: 3, unresolvedIds: [],
      droppedBots: 0,
    });
    expect(out).toMatch(/Capped at 25/);
    expect(out).toMatch(/3 recipient/);
  });

  test('invalidTokens code-fenced + cap at 10', () => {
    const tokens = Array.from({ length: 15 }, (_, i) => `bogus${i}`);
    const out = renderRecipientWarnings({
      invalidTokens: tokens, cappedCount: 0, unresolvedIds: [],
      droppedBots: 0,
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
      droppedBots: 0,
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
      droppedBots: 1,
    });
    expect(out).toMatch(/Capped/);
    expect(out).toMatch(/Could not parse/);
    expect(out).toMatch(/no longer in this server/);
    expect(out).toMatch(/bot/);
    // Self-send is supported, so renderRecipientWarnings emits NO
    // sender-related text. The confirm-card renderer surfaces a
    // neutral "Send includes you." notice instead (asserted in the
    // renderConfirmCardContent suite below).
    expect(out).not.toMatch(/yourself/);
  });

  test('transientFailureIds rendered with neutral copy (not "left the server")', () => {
    // Rate-limit / gateway-blip 429s land in transientFailureIds, NOT
    // unresolvedIds — so the message must encourage retry, not imply
    // the recipient is gone — distinct copy avoids misdirection.
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

  test('caps each shown invalidToken at 80 codepoints with an ellipsis indicator', () => {
    // recipient-parser.js caps each token at 256 chars, so worst-case
    // 10 tokens × 256 = 2.5KB of code-fenced text before any other
    // warning lines render. The 80-codepoint per-token cap keeps the
    // warnings block legible and shrinks the worst case meaningfully.
    const longToken = 'a'.repeat(200);
    const out = renderRecipientWarnings({ invalidTokens: [longToken] });
    // The fence-content slice (between the two ```s) carries the
    // truncated token — confirm it ends with the ellipsis indicator
    // and that no 81+-char run of `a` survives.
    const fence = out.split('```')[1] || '';
    expect(fence).toMatch(/a{80}…/);
    expect(fence).not.toMatch(/a{81}/);
  });

  test('does NOT add ellipsis when the token already fits under the cap', () => {
    // Short tokens render untruncated — pin that the 80-codepoint
    // cap is not eagerly appending the indicator to every token.
    const out = renderRecipientWarnings({ invalidTokens: ['shorttoken'] });
    expect(out).toContain('shorttoken');
    const fence = out.split('```')[1] || '';
    expect(fence).not.toContain('…');
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
    // Explicit else-throw so a future resource type (audio, contact
    // card, etc.) can't silently render as a location.
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

  test('selfIncluded=true surfaces "Send includes you." neutral notice', () => {
    // Self-send is supported — the renderer surfaces a NEUTRAL notice
    // (not a warning) so the user sees confirmation that the sender
    // made it into the recipient list.
    const out = renderConfirmCardContent({ ...baseProps, selfIncluded: true });
    expect(out).toMatch(/Send includes you/);
    // Not in the warning block (the leading ⚠ "Some recipients were
    // dropped" header).
    expect(out).not.toMatch(/Some recipients were dropped/);
  });

  test('selfIncluded omitted (default false) → no notice', () => {
    const out = renderConfirmCardContent(baseProps);
    expect(out).not.toMatch(/Send includes/);
  });

  test('voice-mode + selfIncluded=true → notice suppressed (forged/drifted payload defense)', () => {
    // Every production voice-mode write path sets selfIncluded:false,
    // so this state is structurally unreachable. The renderer guard
    // exists for a forged or schema-drifted payload that would
    // otherwise stack a "Send includes you." notice on top of a
    // voice-mode "To:" line whose semantics already exclude the
    // sender. Pin the guard so a future refactor can't silently let
    // the contradiction surface.
    const u1 = { id: '100000000000000001', username: 'alice' };
    const out = renderConfirmCardContent({
      ...baseProps,
      validRecipients: [u1],
      selfIncluded: true,
      recipientMode: 'voice',
      voiceChannelId: 'voice-ch',
    });
    expect(out).not.toMatch(/Send includes you/);
    // Voice-mode "To:" still renders correctly with the channel mention.
    expect(out).toMatch(/<#voice-ch>/);
  });

  test('personal-message preview cap at 80 chars, rendered as blockquote', () => {
    const long = 'x'.repeat(120);
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: long });
    // Blockquote form (`> `) instead of `"..."` wrap so literal `"`
    // chars in the message can't make the rendering look ragged.
    expect(out).toMatch(/> x{80}…/);
    // The previous `"..."` wrap is gone.
    expect(out).not.toMatch(/"x{80}…"/);
  });

  test('personal-message preview backs off the cut when it would land on a markdown escape', () => {
    // sanitizeMessage emits `\*` etc. If a slice lands the cut at a
    // boundary between `\` and `*`, the rendered preview shows a
    // dangling `\`. The slice backs off by 1 when the 80th char
    // is a `\` not preceded by another `\`.
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

  test('personal-message preview slices by codepoint, not UTF-16 code units (surrogate-pair safe)', () => {
    // Bare String.prototype.slice operates on UTF-16 code units, so a
    // slice that lands mid-surrogate emits a lone surrogate that
    // Discord renders as tofu. Build a message where the 80th
    // codepoint is an emoji (surrogate pair) and verify the rendered
    // preview contains no lone surrogate.
    const message = 'a'.repeat(79) + '\u{1F600}' + 'rest'.repeat(20);
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: message });
    const lone = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/;
    expect(out).not.toMatch(lone);
  });

  test('personal-message preview back-off handles odd-count multi-backslash boundary', () => {
    // The previous single-backslash heuristic only checked the last
    // two chars (`\` at cut-1 + non-`\` at cut-2). With THREE consecutive
    // `\` at cut-3..cut-1, the last one starts a fresh escape sequence
    // but cut-2 also `\` would fool the old check into NOT backing off.
    // The fix counts trailing `\` and backs off when the count is odd.
    //
    // Build: 77 chars + '\\\\\\*' (3 backslashes then `*`) + tail. The
    // first `\\` is a literal-pair, the third `\` starts the `\*`
    // escape. Cut lands at position 80 (= '*'); we want a back-off to
    // 79 so the trailing escape-starter `\` is dropped.
    const prefix = 'a'.repeat(77);  // 77 chars
    const message = prefix + '\\\\\\*' + 'rest'.repeat(20);  // 77+4+80 = 161 chars
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: message });
    // Backed-off cut: index 79. Preview ends with the literal-pair `\\`
    // (positions 77-78) and the ellipsis. The escape-starter `\` at
    // position 79 must be dropped.
    expect(out).toContain(`> ${prefix}\\\\…`);
    // Defense: the rendered preview must NOT end with three backslashes
    // before the ellipsis — that would mean the escape-starter survived.
    expect(out).not.toMatch(/\\\\\\…/);
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

  test('preview prefers guild member displayName over username when interaction is supplied', () => {
    // Pin the resolveRecipientAlias path so a future refactor can't
    // silently drift back to raw `u.username` (which skips the
    // nickname/globalName resolution and would diverge from the
    // post-send confirmation wording).
    const u = makeUser('100000000000000001', { username: 'alice' });
    const int = {
      guild: {
        members: {
          cache: new Map([[u.id, { displayName: 'Alice in Wonderland', user: u }]]),
        },
      },
    };
    const out = renderConfirmCardContent({ ...baseProps, validRecipients: [u], interaction: int });
    expect(out).toMatch(/Alice in Wonderland/);
    expect(out).not.toMatch(/\balice\b/);
  });

  test('preview falls back to username when interaction is omitted (no guild member lookup)', () => {
    // resolveRecipientAlias handles `interaction == null` via optional
    // chaining (commands.js:~310) and falls through to r.username, so
    // tests / callers without an interaction object still produce a
    // usable preview rather than throwing.
    const u = makeUser('100000000000000001', { username: 'alice' });
    const out = renderConfirmCardContent({ ...baseProps, validRecipients: [u] });
    // No `interaction` passed → no guild member cache to consult →
    // falls back to the user's username.
    expect(out).toMatch(/alice/);
  });

  test('personal-message blockquote collapses embedded newlines + unicode line/paragraph separators to spaces', () => {
    // Discord blockquotes are per-LINE — only the line starting with
    // `> ` gets the left-bar. A multi-line message would render with
    // a quoted first line and flush-left subsequent lines.
    // formatPersonalMessagePreview collapses `\n` / `\r\n` / U+2028
    // (line sep) / U+2029 (paragraph sep) into single spaces so the
    // preview is a clean one-liner. The Unicode separators matter
    // because Discord renders them as line breaks too.
    const out = renderConfirmCardContent({
      ...baseProps,
      personalMessage: 'first line\nsecond\r\nthird fourth fifth',
    });
    expect(out).toContain('> first line second third fourth fifth\n');
  });

  test('caps total rendered content below Discord\'s 2000-char limit + adds truncation indicator', () => {
    // Worst-case render: a maximal warningsBlock + a long resourceLabel
    // + a long personalMessage pre-sanitized preview can plausibly
    // cross 2000 chars in adversarial inputs. Without a cap, Discord
    // rejects editReply with a 400 and the throw orphans the flow
    // row (now cleaned up by the safety-net catch, but the user still
    // sees a generic error rather than the confirm card).
    //
    // Pin: a 3000-char warningsBlock forces the cap to fire, and the
    // total output stays under 2000 chars with a visible truncation
    // marker.
    const bigWarnings = 'WARN ' + 'x'.repeat(3000) + '\n\n';
    const out = renderConfirmCardContent({
      ...baseProps,
      warningsBlock: bigWarnings,
    });
    expect(out.length).toBeLessThanOrEqual(2000);
    expect(out).toMatch(/…\(truncated\)$/);
  });

  test('does NOT truncate when content fits under the cap (referential-equality fast path)', () => {
    // The cap path runs `safeCodepointSlice` which returns the input
    // unchanged when below the cap. Reference equality (`===`) gates
    // the truncation indicator — pin that the typical case has
    // neither marker nor extra suffix.
    const out = renderConfirmCardContent({ ...baseProps });
    expect(out).not.toMatch(/…\(truncated\)/);
    expect(out.length).toBeLessThan(2000);
  });
});

// ──────────────────────────────────────────────────────────────
// handleQurlSend — front half
// ──────────────────────────────────────────────────────────────

describe('handleQurlSend — slash entry', () => {
  test('rejects in DM context', async () => {
    const int = makeInteraction({
      guildId: null,
      options: { attachment: VALID_ATTACHMENT },
    });
    await handleQurlSend(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/in a server/),
      ephemeral: true,
    }));
  });

  test('rejects when activeFileSends is at cap (UX fast-fail) — cooldown NOT burned (server-side backpressure)', async () => {
    // activeFileSends ≥ MAX_CONCURRENT_FILE_SENDS short-circuits at slash
    // entry with a "bot too busy" reply. The actual concurrency-slot
    // claim happens inside executeSendPipeline; this entry-time check
    // is a UX fast-fail. Server-side backpressure is not user fault →
    // cooldown is intentionally NOT set so the user can retry as soon
    // as a slot frees.
    const originalActive = getActiveFileSends();
    try {
      setActiveFileSends(99);  // any value ≥ MAX_CONCURRENT_FILE_SENDS
      const int = makeInteraction({ options: { attachment: VALID_ATTACHMENT } });
      await handleQurlSend(int);
      expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
        content: expect.stringMatching(/too many file sends/i),
        ephemeral: true,
      }));
      // No cooldown set on this branch.
      expect(isOnCooldown(SENDER_ID)).toBe(false);
      // And no flow row created.
      expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    } finally {
      setActiveFileSends(originalActive);
    }
  });

  test('rejects when attachment.url is not Discord CDN (SSRF gate) — cooldown PRESERVED (probing defense)', async () => {
    const int = makeInteraction({
      options: { attachment: { ...VALID_ATTACHMENT, url: 'https://evil.com/x.png' } },
    });
    await handleQurlSend(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/source not allowed/),
      ephemeral: true,
    }));
    // SSRF gate is the one rejection that KEEPS the cooldown — probing
    // the allow-list is an abuse signal, not an honest user error.
    expect(isOnCooldown(SENDER_ID)).toBe(true);
  });

  test('rejects disallowed file type — cooldown CLEARED (honest user error, not abuse)', async () => {
    const int = makeInteraction({
      options: { attachment: { ...VALID_ATTACHMENT, contentType: 'application/x-evil-macroenabled' } },
    });
    await handleQurlSend(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/File type not allowed/),
    }));
    // Honest user error → unlock retry immediately. Don't strand the
    // user for 30s on a wrong-file-extension mistake.
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('rejects file over size cap — cooldown CLEARED (honest user error)', async () => {
    const int = makeInteraction({
      options: { attachment: { ...VALID_ATTACHMENT, size: 999_999_999 } },
    });
    await handleQurlSend(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/too large/),
    }));
    expect(isOnCooldown(SENDER_ID)).toBe(false);
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
    await handleQurlSend(int);
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
    // Resolved aliases persist into payload so menu-handler reruns
    // (rerenderConfirmCard) can render names even if the member-cache
    // entry is evicted before the user picks expiry/self-destruct.
    expect(payload.recipientAliases).toEqual(
      expect.objectContaining({ [u1]: expect.any(String), [u2]: expect.any(String) })
    );
    // warningsBlock present (empty here — no dropped tokens) so menu
    // interactions can re-render with the same surface as slash entry.
    expect(payload).toHaveProperty('warningsBlock');
    expect(int.editReply).toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Sending file/);
    expect(reply.content).toMatch(/x\.png/);
    expect(reply.components.length).toBeGreaterThan(0);
  });

  test('/qurl map slash entry persists recipientAliases (parity with /qurl send)', async () => {
    // Both entry points share handleQurlSlashSend's payload-construction
    // path. This sanity test pins that the alias-persistence guarantee
    // covers /qurl map as well — without it, a regression that only
    // skipped aliases on one entry point would leave map-flow Edit
    // notes / menu interactions falling back to the raw snowflake on
    // cache miss.
    const u1 = '100000000000000001';
    const int = makeInteraction({
      options: {
        _sub: 'map',
        location: 'https://maps.app.goo.gl/abcXYZ',
        recipients: `<@${u1}>`,
      },
      guildMembers: { [u1]: {} },
    });
    await handleQurlMap(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.resourceType).toBe('maps');
    expect(payload.recipientAliases).toEqual(
      expect.objectContaining({ [u1]: expect.any(String) })
    );
  });

  test('happy path without recipients → confirm card with picker', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT },
    });
    await handleQurlSend(int);
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
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/No valid recipients/);
    expect(reply.content).toMatch(/bots are skipped/);
  });

  test('only sender mentioned → confirm card with self-included notice', async () => {
    // Self-send: a single `@me` mention is a legitimate recipient list.
    // No "no valid recipients" error; flow advances to confirm card and
    // the renderer surfaces the "Send includes you." neutral notice.
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@${SENDER_ID}>` },
      guildMembers: { [SENDER_ID]: {} },
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Send includes you/);
    expect(reply.content).not.toMatch(/No valid recipients/);
    // Payload persists selfIncluded so menu re-renders keep the notice.
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.selfIncluded).toBe(true);
    expect(payload.recipientIds).toEqual([SENDER_ID]);
  });

  test('DM context (no guild) suppresses the @everyone permission warning', async () => {
    // @everyone has no meaning in a DM (Discord doesn't expand it),
    // so the permission-warning copy ("requires the Mention Everyone
    // permission") reads strangely if it fires there. The handler
    // suppresses massMentionDenied surfacing when interaction.guild
    // is null/undefined. Pin that this suppression is in place so a
    // future caller refactor that drops the `!isDmContext` check
    // surfaces here, not as confusing UX.
    const int = makeInteraction({
      guildId: null, // → guild = null in makeInteraction
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${SENDER_ID}>` },
    });
    await handleQurlSend(int);
    // Whatever editReply lands, the @everyone permission warning
    // must not appear. (The flow itself may hard-fail downstream
    // because resolveRecipientUsers needs guild context — that's a
    // separate concern; this test only pins the warning suppression.)
    const calls = int.editReply.mock.calls;
    for (const [arg] of calls) {
      expect(arg.content || '').not.toMatch(/Mention Everyone permission/);
    }
  });

  test('guild context + no MENTION_EVERYONE → @everyone warning renders + Alice still parses', async () => {
    // Positive-case companion to the DM-suppression test above. In
    // guild context, when the sender lacks MENTION_EVERYONE and types
    // `@everyone <@alice>`, the parser surfaces massMentionDenied and
    // the handler renders the permission-specific warning. Alice
    // expands normally. Pin the visible UX so a future refactor that
    // accidentally collapses the warning surfaces here.
    const aliceId = '400000000000000001';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    // Default memberPermissions is undefined (no Mention Everyone) —
    // matches the existing "no permission" assumption across this
    // test suite.
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const lastEdit = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastEdit.content).toMatch(/Mention Everyone\b/);
    // Alice still made it into the recipient list.
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.recipientIds).toEqual([aliceId]);
  });

  // ── Issue #326 text-path handler tests ──
  test('text path: <@&roleId> for non-mentionable role WITHOUT MENTION_EVERYONE → warning with role name, no expansion', async () => {
    // Parser surfaces parsed.roleMentionsDenied with the role ID; the
    // handler resolves the name via guild.roles.cache.get(id)?.name
    // and renderRecipientWarnings emits "@<name> requires …" copy.
    // Pin the end-to-end wiring.
    const aliceId = '400000000000000010';
    const bobId = '400000000000000011';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@${aliceId}> <@&7000>` },
      guildMembers: { [aliceId]: {}, [bobId]: {} },
    });
    // Inject role-7000 as non-mentionable with Bob as a member. Without
    // the gate, Bob would land in recipientIds via role expansion.
    int.guild.roles.cache.set('7000', {
      id: '7000',
      name: 'admin-team',
      mentionable: false,
      members: new Map([[bobId, { user: { id: bobId, bot: false } }]]),
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    // Alice (directly mentioned) IS in recipients; Bob (role member) is NOT.
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.recipientIds).toEqual([aliceId]);
    const lastEdit = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastEdit.content).toMatch(/@admin-team/);
    expect(lastEdit.content).toMatch(/Mention Everyone/);
    expect(lastEdit.content).toMatch(/role\.mentionable: true/);
  });

  test('text path: every recipient denied-role-only → "no valid recipients" but NOT misleading bot-only log nor transient-retry copy', async () => {
    // Pin both predicate updates at commands.js (breakdownEmpty,
    // transientOnly): a recipients string consisting only of denied
    // <@&roleId> mentions must NOT log the "bot-only-or-self mention
    // list" signal AND must NOT show "Could not look up recipients
    // right now. Try again in a moment." copy — both would mislead
    // the user. Without the `roleMentionsDeniedNames.length === 0`
    // term in both predicates, a future refactor that drops it
    // silently regresses to the bot-only log + retry copy.
    const aliceId = '400000000000000030';
    const logger = require('../src/logger');
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@&7002>' },
      guildMembers: { [aliceId]: {} },
    });
    int.guild.roles.cache.set('7002', {
      id: '7002',
      name: 'private-team',
      mentionable: false,
      members: new Map([[aliceId, { user: { id: aliceId, bot: false } }]]),
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    // Permission-specific copy present.
    expect(reply.content).toMatch(/@private-team/);
    expect(reply.content).toMatch(/No valid recipients/);
    // Transient-retry copy must NOT fire.
    expect(reply.content).not.toMatch(/Could not look up recipients right now/);
    // bot-only-or-self log must NOT fire (would mislead operators
    // analyzing the cap-skew metric).
    const infoLogCalls = logger.info.mock.calls.filter(
      ([msg]) => typeof msg === 'string' && msg.includes('bot-only-or-self mention list'),
    );
    expect(infoLogCalls).toEqual([]);
  });

  test('text path: <@&roleId> for non-mentionable role WITH MENTION_EVERYONE → expands normally, no warning', async () => {
    // OR-gate: MENTION_EVERYONE bypasses role.mentionable. Pin the
    // bypass at the handler boundary (parser-level coverage is in
    // recipient-parser.test.js).
    const aliceId = '400000000000000020';
    const bobId = '400000000000000021';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@&7001>` },
      guildMembers: { [aliceId]: {}, [bobId]: {} },
    });
    int.guild.roles.cache.set('7001', {
      id: '7001',
      name: 'admin-team',
      mentionable: false,
      members: new Map([
        [aliceId, { user: { id: aliceId, bot: false } }],
        [bobId, { user: { id: bobId, bot: false } }],
      ]),
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.recipientIds.sort()).toEqual([aliceId, bobId].sort());
    const lastEdit = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastEdit.content).not.toMatch(/role\.mentionable/);
  });

  test('guild context + MENTION_EVERYONE permission → @everyone expands, no warning', async () => {
    // Channel-overwrite pinning: the handler reads
    // `interaction.memberPermissions.has(MentionEveryone)` (channel-
    // effective perms). A future refactor that switched to
    // `interaction.member.permissions.has(...)` (guild-wide only)
    // would silently lose channel-overwrite respect. Pin the
    // property contract by mocking `memberPermissions.has`.
    const aliceId = '400000000000000002';
    const bobId = '400000000000000003';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {}, [bobId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const lastEdit = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    // Permission warning must NOT fire.
    expect(lastEdit.content).not.toMatch(/Mention Everyone permission/);
    // Both Alice + Bob expanded from @everyone are in the payload.
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.recipientIds.sort()).toEqual([aliceId, bobId].sort());
  });

  test('all mentioned recipients hit transient lookup failure → retry copy, not "no valid recipients"', async () => {
    // transient-only path: the user's mentions were VALID but every
    // members.fetch hit a 429 or gateway blip. Generic "no valid
    // recipients" misleads — they didn't make any mistake. Encourage
    // retry instead.
    const flaky1 = '100000000000000099';
    const flaky2 = '100000000000000098';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@${flaky1}> <@${flaky2}>` },
      guildMembers: {},
      guildFetchByID: { [flaky1]: 'ratelimit', [flaky2]: 'ratelimit' },
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Could not look up recipients right now.*Try again/i);
    expect(reply.content).not.toMatch(/No valid recipients to send to/);
  });

  test('unknown-member ID surfaced as warning but valid users still proceed', async () => {
    const known = '100000000000000001';
    const gone = '100000000000000099';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@${known}> <@${gone}>` },
      guildMembers: { [known]: {} },
      guildFetchByID: { [gone]: 'unknown' },
    });
    await handleQurlSend(int);
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
    await handleQurlSend(int);
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
    await handleQurlSend(int);
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
    await handleQurlSend(int);
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
    await handleQurlSend(int);
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
    await handleQurlSend(int);
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
    await handleQurlSend(int);
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
    await handleQurlSend(int);
    expect(isOnCooldown(SENDER_ID)).toBe(false);
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/unexpected throw/),
      expect.objectContaining({ user_id: SENDER_ID }),
    );
  });

  test('safety-net catch deletes orphan flow row when post-supersede throw fires', async () => {
    // When supersedeOrCreate succeeds and then a downstream call
    // (renderConfirmCardContent, renderConfirmCardRows, the final
    // editReply, etc.) throws, the DDB row we just claimed would
    // sit orphaned until TTL eviction — blocking the user's next
    // /qurl send or /qurl map under the sibling-flow guard.
    //
    // Reproduce: let supersedeOrCreate resolve `created: true`, then
    // force editReply to throw on its first call (the
    // renderConfirmCardContent + components delivery). The catch's
    // version-checked deleteFlow should fire with the correct
    // stage so a racing confirm-click can't be silently revoked.
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    int.editReply.mockRejectedValueOnce(new Error('Discord 500'));
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    expect(mockDeleteFlow).toHaveBeenCalledWith(
      expect.any(String),
      expect.objectContaining({
        stage: 'awaiting_send_confirm',
        reason: 'terminal',
      }),
    );
    expect(isOnCooldown(SENDER_ID)).toBe(false);
    // Pin `flow_id` in the error-log payload so a refactor that drops
    // it (and breaks correlating logs ↔ DDB rows during a post-mortem)
    // surfaces as a test failure rather than silently regressing.
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/unexpected throw/),
      expect.objectContaining({ flow_id: expect.any(String) }),
    );
  });

  test('safety-net catch does NOT call deleteFlow when throw fires before supersedeOrCreate', async () => {
    // If the throw lands before we ever called supersedeOrCreate
    // there is no DDB row to clean up — calling deleteFlow would be
    // a wasted round-trip (and noise in DDB metrics). The
    // `orphanFlowCreated` flag must gate the cleanup precisely.
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    int.deferReply.mockRejectedValueOnce(new Error('token expired'));
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    expect(mockDeleteFlow).not.toHaveBeenCalled();
  });

  test('safety-net catch does NOT call deleteFlow when supersedeOrCreate returned created:false (sibling flow)', async () => {
    // A sibling-flow supersede returns `created: false` — the row
    // belongs to a different in-flight flow, not us. Deleting it
    // would silently revoke another user's open confirm card.
    mockSupersedeOrCreate.mockResolvedValueOnce({
      created: false,
      surviving: { stage: 'awaiting_confirm', flow_id: 'other_flow' },
    });
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    expect(mockDeleteFlow).not.toHaveBeenCalled();
  });

  describe('voice-channel slash entry (auto voice-everyone default)', () => {
    // When `/qurl send` is invoked from a voice channel WITHOUT a
    // `recipients:` value, the front half auto-resolves the voice-
    // connected members (minus sender + bots) and lands in voice-mode.
    // These tests pin (a) the recipient set excludes the sender,
    // (b) the persisted payload carries recipientMode:'voice', and
    // (c) the fall-back to picker-mode is silent when voice is empty
    // / sender-only / over-cap.

    const VOICE_CH = 'voice-ch-slash-1';
    const u1 = '100000000000000011';
    const u2 = '100000000000000012';
    const bot = '100000000000000099';

    function makeVoiceEntryInteraction({ members = [], botIds = [] } = {}) {
      const chanMembers = new Map();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT },
      });
      // Stamp the voice channel as the invocation channel + cache row.
      int.channel = { id: VOICE_CH, type: 2 };
      for (const mid of members) {
        const isBot = botIds.includes(mid);
        const member = { user: { id: mid, bot: isBot } };
        int.guild.members.cache.set(mid, member);
        chanMembers.set(mid, member);
      }
      int.guild.channels.cache.set(VOICE_CH, {
        id: VOICE_CH, type: 2, name: 'general', members: chanMembers,
      });
      return int;
    }

    test('happy path: voice members minus sender land in payload, recipientMode:"voice"', async () => {
      const int = makeVoiceEntryInteraction({ members: [SENDER_ID, u1, u2] });
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('voice');
      expect(payload.recipientIds.sort()).toEqual([u1, u2].sort());
      expect(payload.recipientIds).not.toContain(SENDER_ID);
      expect(payload.selfIncluded).toBe(false);
    });

    test('bots in voice are filtered before voice-mode is committed', async () => {
      const int = makeVoiceEntryInteraction({
        members: [u1, bot, u2],
        botIds: [bot],
      });
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('voice');
      expect(payload.recipientIds.sort()).toEqual([u1, u2].sort());
      expect(payload.recipientIds).not.toContain(bot);
    });

    test('bots-only voice → picker fallback WITH bot-drop banner (not silent)', async () => {
      // Distinction from sender-only / truly-empty: those silently
      // fall back because there's nothing actionable to surface, but
      // a voice channel populated entirely by bots is the kind of
      // "wait, didn't it know I was in voice?" state where the
      // bot-drop accounting clarifies WHY voice-mode didn't take.
      const int = makeVoiceEntryInteraction({
        members: [bot],
        botIds: [bot],
      });
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('picker');
      expect(payload.recipientIds).toEqual([]);
      expect(payload.warningsBlock).toMatch(/bot/i);
    });

    test('sender-only voice → falls back to picker-mode (no auto voice)', async () => {
      // After excludeSender the voice set is empty. Don't surface a
      // warning; the user didn't ask for voice-everyone, so falling
      // back to the picker UX is the quiet path.
      const int = makeVoiceEntryInteraction({ members: [SENDER_ID] });
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('picker');
      expect(payload.recipientIds).toEqual([]);
    });

    test('empty voice channel → falls back to picker-mode', async () => {
      const int = makeVoiceEntryInteraction({ members: [] });
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('picker');
      expect(payload.recipientIds).toEqual([]);
    });

    test('over-cap voice → falls back to picker-mode WITH banner explaining why', async () => {
      // Unreachable under default config (20k cap vs Discord's 99-member
      // voice cap), but a shrunk env override would trip this. Silent
      // fallback would leave the user wondering why voice-mode didn't
      // take. Banner + info log document the degraded state. Mirrors
      // the button-handler's hard-reject copy at handleConfirmVoiceEveryone.
      const config = require('../src/config');
      const originalCap = config.QURL_SEND_MAX_RECIPIENTS;
      config.QURL_SEND_MAX_RECIPIENTS = 1;  // force over-cap with 2 members
      try {
        const int = makeVoiceEntryInteraction({ members: [u1, u2] });
        await handleQurlSend(int);
        const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
        expect(payload.recipientMode).toBe('picker');
        expect(payload.recipientIds).toEqual([]);
        // User sees the "why" rather than a silent voice→picker switch.
        // Wording is "eligible recipients" (post-filter count) not raw
        // "connected" — sender + bots are already filtered out by this
        // point, so phrasing as "connected" would diverge from what
        // Discord's voice panel shows.
        expect(payload.warningsBlock).toMatch(/Voice channel has 2 eligible recipients/);
        expect(payload.warningsBlock).toMatch(/max 1/);
      } finally {
        config.QURL_SEND_MAX_RECIPIENTS = originalCap;
      }
    });

    test('voice channel cache miss → picker-mode with "Couldn\'t read voice channel" banner', async () => {
      // Cache miss simulates the GuildVoiceStates intent dropping or
      // the channel being evicted between command receipt and our
      // lookup. Banner surfaces the degraded state instead of silently
      // landing in picker-mode.
      const int = makeInteraction({ options: { attachment: VALID_ATTACHMENT } });
      int.channel = { id: VOICE_CH, type: 2 };
      // Inject the channel id WITHOUT registering it in the cache —
      // makes guild.channels.cache.get(id) return undefined, the cache-
      // miss shape.
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('picker');
      expect(payload.warningsBlock).toMatch(/Couldn't read voice channel members/);
    });

    test('explicit `recipients:` overrides voice-mode default (manual selection wins)', async () => {
      // A user who typed `recipients:` clearly meant THOSE people, not
      // "everyone in voice." Voice-mode auto-default only fires when
      // recipients is omitted entirely.
      const int = makeVoiceEntryInteraction({ members: [u1, u2] });
      // Re-implement the jest.fn() stub rather than replacing the
      // property — keeps the call-tracking behavior that the rest of
      // the suite relies on.
      int.options.getString.mockImplementation((key) =>
        (key === 'recipients' ? `<@${u1}>` : null)
      );
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('picker');
      expect(payload.recipientIds).toEqual([u1]);
    });

    test('text `@everyone` in recipients → EVERYONE mode (picker hidden, no auto-fill)', async () => {
      // Mirror of the 📢 @everyone button-click path. When the user
      // types `@everyone` in the recipients field and has
      // MENTION_EVERYONE, the parser's `massMentionExpanded` flag
      // lands the card in EVERYONE mode so the picker stays hidden —
      // auto-filling it would either truncate at Discord's 25-entry
      // default_values cap or invite a picker re-interaction that
      // silently replaces the fan-out via handleConfirmUserSelect.
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '@everyone' },
        guildMembers: {
          [SENDER_ID]: {},
          '100000000000000051': {},
          '100000000000000052': {},
        },
      });
      int.memberPermissions = { has: jest.fn(() => true) };
      int.guild.memberCount = 3;
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('everyone');
      // Fan-out includes the sender (selfIncluded=true), matching
      // the button-click path's semantics.
      expect(payload.selfIncluded).toBe(true);
      expect(payload.recipientIds.length).toBeGreaterThanOrEqual(2);
    });

    test('text `@everyone` with sender MISSING from members.cache post-prewarm → selfIncluded:false (documented divergence from button-click path)', async () => {
      // Pins the divergence documented at commands.js:~4296 (slash-text
      // EVERYONE branch). In the narrow shard-resume / partial-chunk
      // race where prewarm runs but the sender's row never lands in
      // `members.cache`, the text path expands @everyone over whatever
      // IS in cache and yields `selfIncluded:false`. The button-click
      // path (handleConfirmEveryone at commands.js:5694) would
      // defensively push `interaction.user` for the same cache state
      // and yield `selfIncluded:true`.
      //
      // A future contributor "fixing" the asymmetry by mirroring the
      // defensive push on the text path would flip this assertion and
      // be forced to revisit the documented divergence — that's the
      // point of pinning it.
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '@everyone' },
        // SENDER_ID intentionally omitted: simulates the post-prewarm
        // race where the chunk-fetch landed everyone EXCEPT the sender.
        guildMembers: {
          '100000000000000051': {},
          '100000000000000052': {},
        },
      });
      int.memberPermissions = { has: jest.fn(() => true) };
      int.guild.memberCount = 3;
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('everyone');
      expect(payload.selfIncluded).toBe(false);
      expect(payload.recipientIds).not.toContain(SENDER_ID);
    });

    test('text `@everyone` WITHOUT MENTION_EVERYONE → stays PICKER (parser denied expansion)', async () => {
      // Counter-test: `massMentionDenied:true` does NOT set the
      // EVERYONE-mode trigger — `massMentionExpanded` is mutually
      // exclusive with `massMentionDenied`. The slash-entry hits the
      // permission-denied warning banner and renders the picker
      // normally for the user to choose recipients manually.
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '@everyone' },
        guildMembers: { [SENDER_ID]: {} },
      });
      // Default permission shape → no MENTION_EVERYONE.
      await handleQurlSend(int);
      const supersedeCalls = mockSupersedeOrCreate.mock.calls;
      // No confirm card persisted (recipientsOmitted=false + valid=0 →
      // the "no valid recipients" early-return fires before
      // supersedeOrCreate). This is the existing behavior; the EVERYONE-
      // mode trigger isn't reached because the parser denied the
      // expansion. Pin via the absence of a supersedeOrCreate call so
      // a future refactor that changes the denied-path UX surfaces here.
      expect(supersedeCalls.length).toBe(0);
    });
  });
});

// ──────────────────────────────────────────────────────────────
// guild.members cache pre-warm for @everyone / role expansion
// ──────────────────────────────────────────────────────────────
// discord.js v14 leaves `guild.members.cache` empty by default (no
// `chunkOnStartup`, no `ws.large_threshold` override) on our multi-
// tenant gateway, so the parser's `@everyone` branch and `role.members`
// filtering for `<@&id>` both silently collapse to just the interacting
// user. Pre-warming via `members.fetch()` is the fix; these tests pin
// the trigger conditions so a future refactor that drops the pre-warm
// regresses here, not in production.

// Identifies the pre-warm call by its options-object shape: a bulk
// chunk fetch carries `{ time }` ONLY — no `user`/`query`/`limit`/
// `force`. Disambiguates from the per-ID `members.fetch(id)` calls in
// resolveRecipientUsers AND from a future bounded per-user fetch like
// `members.fetch({ user: id, time: 2000 })` that would happen to
// carry a `time` field. Asserting absence of the per-user keys makes
// the disambiguation explicit so the helper stays accurate as the
// discord.js API surface grows.
const isPrewarmCall = ([arg]) =>
  arg && typeof arg === 'object'
  && typeof arg.time === 'number'
  && arg.user === undefined
  && arg.query === undefined
  && arg.limit === undefined;

describe('handleQurlSlashSend — guild.members cache pre-warm', () => {
  test('@everyone in recipients string → members.fetch() pre-warm fires', async () => {
    const aliceId = '500000000000000001';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    await handleQurlSend(int);
    expect(int.guild.members.fetch.mock.calls.find(isPrewarmCall)).toBeTruthy();
  });

  test('<@&roleId> in recipients string → members.fetch() pre-warm fires', async () => {
    // role.members for non-@everyone roles is a filtered view of
    // guild.members.cache, so the trigger has to include arbitrary
    // role-mention wire shapes, not just literal @everyone.
    const aliceId = '500000000000000002';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@&7100>` },
      guildMembers: { [aliceId]: {} },
    });
    int.guild.roles.cache.set('7100', {
      id: '7100', name: 'team', mentionable: true,
      members: new Map([[aliceId, { user: { id: aliceId, bot: false } }]]),
    });
    await handleQurlSend(int);
    expect(int.guild.members.fetch.mock.calls.find(isPrewarmCall)).toBeTruthy();
  });

  test('plain <@userId> mentions only → members.fetch() pre-warm does NOT fire', async () => {
    // Defends the common case (a few user mentions) against paying the
    // pre-warm cost. Gate is the mass-mention shape regex, not the
    // existence of any mention.
    const aliceId = '500000000000000003';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    await handleQurlSend(int);
    expect(int.guild.members.fetch.mock.calls.filter(isPrewarmCall)).toEqual([]);
  });

  test('empty recipients string → members.fetch() pre-warm does NOT fire', async () => {
    // Defense against a `recipientsRaw` of `null` / `''` triggering the
    // regex via the `|| ''` fallback. Empty input → no mentions → no fetch.
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT }, // no recipients
    });
    await handleQurlSend(int);
    expect(int.guild.members.fetch.mock.calls.filter(isPrewarmCall)).toEqual([]);
  });

  test('@everyone WITHOUT MENTION_EVERYONE → pre-warm does NOT fire (parser will deny anyway)', async () => {
    // Pin the asymmetric gate in handleQurlSlashSend: `@everyone` alone
    // typed by a sender without MENTION_EVERYONE → the parser hits
    // `massMentionDenied` and never expands, so the chunk request
    // would be wasted. A future refactor that drops the perm-gate
    // would silently regress the chunk-budget defense.
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '@everyone' },
    });
    // No memberPermissions set → has(MentionEveryone) returns undefined → falsy.
    await handleQurlSend(int);
    expect(int.guild.members.fetch.mock.calls.filter(isPrewarmCall)).toEqual([]);
  });

  test('@everyone + <@&roleId> WITHOUT MENTION_EVERYONE → pre-warm STILL fires (role path)', async () => {
    // Counter-test to the above: when the input ALSO contains a role
    // mention, the prewarm fires regardless of MENTION_EVERYONE because
    // role expansion gates on `role.mentionable === true` per-role, not
    // the global perm. The role-mention path needs the cache.
    const aliceId = '500000000000000077';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@&7200>` },
      guildMembers: { [aliceId]: {} },
    });
    int.guild.roles.cache.set('7200', {
      id: '7200', name: 'team', mentionable: true,
      members: new Map([[aliceId, { user: { id: aliceId, bot: false } }]]),
    });
    // No memberPermissions set → has(MentionEveryone) returns undefined → falsy.
    await handleQurlSend(int);
    expect(int.guild.members.fetch.mock.calls.find(isPrewarmCall)).toBeTruthy();
  });

  test('@everyonefoo (no word boundary) → pre-warm does NOT fire', async () => {
    // MASS_MENTION_HINT_RE uses `(?<![\p{L}\p{N}_])@everyone(?![\p{L}\p{N}_])`
    // — same word-boundary class as recipient-parser.js's
    // EVERYONE_TOKEN_RE. Without the boundary, a typo / paste like
    // `@everyonefoo` would burn a chunk fetch even though the parser
    // ignores the token. A future simplification to `/@everyone|<@&\d+>/u`
    // would silently regress the budget defense — this test pins it.
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '@everyonefoo' },
    });
    await handleQurlSend(int);
    expect(int.guild.members.fetch.mock.calls.filter(isPrewarmCall)).toEqual([]);
  });

  test('cache already at memberCount → members.fetch() pre-warm short-circuits', async () => {
    // Hot-cache short-circuit defends against re-fetching when a prior
    // invocation in the same process lifetime already populated the
    // cache. Without this, every @everyone send burns the full chunk
    // round-trip.
    const aliceId = '500000000000000005';
    const bobId = '500000000000000006';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone` },
      guildMembers: { [aliceId]: {}, [bobId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    int.guild.memberCount = 2; // matches cache.size from guildMembers above
    await handleQurlSend(int);
    expect(int.guild.members.fetch.mock.calls.filter(isPrewarmCall)).toEqual([]);
  });

  test('concurrent invocations in the same guild share one in-flight fetch', async () => {
    // Two simultaneous /qurl send @everyone calls against a cold cache
    // should NOT each fire their own chunk request. The prewarm helper's
    // in-flight Map<guildId, Promise> coalesces them — abuse via
    // concurrent invocations otherwise burns chunk-request budget
    // linearly. discord.js may also coalesce GUILD_REQUEST_MEMBERS
    // internally, but we don't rely on that.
    const aliceId = '500000000000000010';
    // A controllable fetch — caller resolves `release` after the
    // assertion so both handlers complete cleanly. Without the resolve,
    // the two awaiting `handleQurlSend` calls would leak through to
    // process exit and Jest's `--detectOpenHandles` would surface them.
    let release;
    const fetchGate = new Promise((r) => { release = r; });
    const sharedFetch = jest.fn(() => fetchGate);
    const sharedGuild = {
      id: 'shared-guild',
      members: { cache: new Map(), fetch: sharedFetch },
      roles: { cache: new Map() },
      channels: { cache: new Map() },
      memberCount: 10,
    };
    function makeShared() {
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
        guildMembers: { [aliceId]: {} },
      });
      int.guild = sharedGuild;
      int.guildId = sharedGuild.id;
      int.memberPermissions = { has: jest.fn(() => true) };
      return int;
    }
    const p1 = handleQurlSend(makeShared());
    const p2 = handleQurlSend(makeShared());
    // Microtask flush so both handlers reach the prewarm await.
    await new Promise((r) => setImmediate(r));
    const prewarmCalls = sharedFetch.mock.calls.filter(isPrewarmCall);
    expect(prewarmCalls.length).toBe(1);
    // Release the gate so handlers settle and Jest doesn't carry an
    // open handle past the test.
    release(new Map());
    await Promise.all([p1, p2]);
  });

  test('members.fetch() rejection is swallowed — flow continues in degraded mode', async () => {
    // 429 / gateway blip on the pre-warm must not crash the handler.
    // The catch logs a warn and the parser proceeds against whatever
    // the cache currently holds (potentially empty → @everyone silently
    // expands to 0). Worst case the user re-runs.
    const aliceId = '500000000000000004';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    int.guild.members.fetch = jest.fn(async (arg) => {
      if (isPrewarmCall([arg])) {
        const err = new Error('rate limited'); err.code = 429; throw err;
      }
      return { user: makeUser(arg) };
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const logger = require('../src/logger');
    const warnCall = logger.warn.mock.calls.find(
      ([msg]) => typeof msg === 'string' && msg.includes('members.fetch pre-warm failed'),
    );
    expect(warnCall).toBeTruthy();
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

  test('deferReply throws (expired token) → cooldown cleared, no flow row, no editReply', async () => {
    // Regression: if Discord's interaction token expires between
    // setCooldown and deferReply (or Discord transiently degrades),
    // we must not strand the user in a 30s cooldown window with no
    // visible response. The catch clears cooldown and returns
    // without attempting an editReply that would also fail.
    const int = makeInteraction({
      options: { location: 'somewhere', recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    // Override the mock to throw on defer.
    int.deferReply = jest.fn(async () => { const e = new Error('Unknown interaction'); e.code = 10062; throw e; });
    await handleQurlMap(int);
    expect(isOnCooldown(SENDER_ID)).toBe(false);
    expect(int.editReply).not.toHaveBeenCalled();
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
  });

  test('defers ONCE — handleQurlSlashSend skips its own defer when already deferred', async () => {
    // Regression: handleQurlMap defers BEFORE resolveLocation so a slow
    // Places call can't blow Discord's 3s ACK window. handleQurlSlashSend
    // then guards its own deferReply on `!interaction.deferred`. Without
    // that guard, the second deferReply would throw "Already replied or
    // deferred" in production (in tests the mock just resolves twice
    // silently — see makeInteraction's deferReply override).
    mockFindPlaceFromText.mockResolvedValueOnce({ placeId: 'ChIJ1', name: 'Place', address: '' });
    const int = makeInteraction({
      options: { location: 'somewhere', recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    expect(int.deferReply).toHaveBeenCalledTimes(1);
  });

  test('arbitrary text → resolved through Places to a place_id-pinned URL', async () => {
    // Free-text inputs route through findPlaceFromText so every
    // recipient opens the same destination — Google's per-viewer
    // geo-bias on /maps/search/<text> is the bug this whole change
    // is fixing.
    mockFindPlaceFromText.mockResolvedValueOnce({
      placeId: 'ChIJ4zGFAZpYwokRGUGph3Mf37k',
      name: 'Central Park',
      address: 'New York, NY',
    });
    const int = makeInteraction({
      options: {
        location: 'Central Park, NYC',
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    expect(mockFindPlaceFromText).toHaveBeenCalledWith('Central Park, NYC');
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.locationUrl).toContain('query_place_id=ChIJ4zGFAZpYwokRGUGph3Mf37k');
    expect(payload.locationName).toMatch(/Central Park/);
  });

  test('free-text input is trimmed + 500-char-capped before reaching Places', async () => {
    // handleQurlMap does `trim().slice(0, 500)` on the slash option;
    // pin the boundary so a forged interaction can't smuggle a
    // longer-than-the-server-side-cap query through. Whitespace at the
    // boundary is trimmed FIRST (so the content slice gets the full
    // 500 chars, not whitespace + 450 content chars).
    mockFindPlaceFromText.mockResolvedValueOnce({
      placeId: 'ChIJ4zGFAZpYwokRGUGph3Mf37k', name: 'X', address: 'Y',
    });
    const padding = '  '.repeat(40); // 80 chars of leading whitespace, trimmed first
    const content = 'a'.repeat(600); // 600 chars of content, slice() caps at 500
    const int = makeInteraction({
      options: {
        location: padding + content + padding,
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    const calledWith = mockFindPlaceFromText.mock.calls[0][0];
    expect(calledWith.length).toBe(500);
    expect(calledWith.startsWith('a')).toBe(true);
    expect(calledWith.endsWith('a')).toBe(true);
  });

  test('place_id sentinel from autocomplete → resolved through Place Details', async () => {
    // When the sender picks a suggestion from the autocomplete
    // dropdown, the `location:` value arrives as the sentinel form
    // `qurl_place:<placeId>`. handleQurlMap routes that through
    // Place Details (cheap, one API call) to hydrate the canonical
    // name + address, then pins the URL to the chosen place_id.
    mockGetPlaceDetails.mockResolvedValueOnce({
      placeId: 'ChIJ37FjGE63t4kRD2_jXSF1F9o',
      name: 'The White House',
      address: '1600 Pennsylvania Ave NW',
    });
    const int = makeInteraction({
      options: {
        location: 'qurl_place:ChIJ37FjGE63t4kRD2_jXSF1F9o',
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    expect(mockGetPlaceDetails).toHaveBeenCalledWith('ChIJ37FjGE63t4kRD2_jXSF1F9o');
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.locationUrl).toContain('query_place_id=ChIJ37FjGE63t4kRD2_jXSF1F9o');
    expect(payload.locationName).toBe('The White House');
  });

  test('Places returns no match → actionable ephemeral, cooldown cleared, no flow row', async () => {
    mockFindPlaceFromText.mockResolvedValueOnce(null);
    const int = makeInteraction({
      options: {
        location: 'zzzz-no-such-place',
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    // Defers BEFORE the Places call so a slow lookup can't blow
    // Discord's 3s ACK window — resolveLocation errors land as
    // editReply, not reply.
    expect(int.deferReply).toHaveBeenCalledWith(expect.objectContaining({ ephemeral: true }));
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Couldn't find/),
    }));
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    // Honest user error (no matching place) — don't strand them
    // for 30s. Same shape as the empty-location + DM branches.
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('stale-sentinel NOT_FOUND → place-specific message (does NOT echo the wire sentinel)', async () => {
    // Reviewer-flagged contract: when a sender picks a suggestion from
    // autocomplete and the place is deleted upstream between pick and
    // submit, the error message must not read `Couldn't find a place
    // matching "qurl_place:ChIJ37FjGE63t4kRD2_jXSF1F9o..."` — that's user-hostile and leaks
    // the wire format. Branch on parsedLocation.placeId to a place-
    // specific message instead.
    mockGetPlaceDetails.mockResolvedValueOnce(null);
    const int = makeInteraction({
      options: {
        location: 'qurl_place:ChIJ-deleted-place',
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    const editReplyCall = int.editReply.mock.calls[0][0];
    expect(editReplyCall.content).toMatch(/no longer available/);
    expect(editReplyCall.content).not.toContain('qurl_place:');
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('Places call throws → actionable ephemeral, cooldown cleared, no flow row', async () => {
    mockFindPlaceFromText.mockRejectedValueOnce(new Error('upstream timeout'));
    const int = makeInteraction({
      options: {
        location: 'somewhere',
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/lookup failed/),
    }));
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    expect(isOnCooldown(SENDER_ID)).toBe(false);
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

  test('empty location string → ephemeral error, cooldown CLEARED (honest user error)', async () => {
    const int = makeInteraction({
      options: { location: '   ', recipients: '<@100000000000000001>' },
    });
    await handleQurlMap(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/empty/),
    }));
    // Honest user error (whitespace-only paste) — don't strand them
    // for 30s. Same shape as the file-type / size-cap branches in
    // handleQurlSend.
    expect(isOnCooldown(SENDER_ID)).toBe(false);
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

  test('locationName strips bidi/zero-width control chars (RLO spoof defense)', async () => {
    // /qurl map's slash option is a new attack surface
    // that bypasses the modal's natural friction. A crafted U+202E
    // (RLO) in `location-name` would otherwise flip text direction
    // in the rendered confirm card. sanitizeContentLabel strips it
    // via NFKC + bidi/ZWS regex before markdown-escape.
    const int = makeInteraction({
      options: {
        location: 'https://www.google.com/maps/place/Cafe',
        // U+202E + visible text — bidi-reversed in any naive renderer.
        'location-name': '‮Backwards Cafe',
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.locationName).toBe('Backwards Cafe');
    expect(payload.locationName).not.toContain('‮');
  });

  test('locationName 256-cap is codepoint-aware (no surrogate split)', async () => {
    // Build a 257-char string whose char at index 254 is the first
    // half of a surrogate pair. Naive .slice(0, 256) would land on
    // the high surrogate alone and produce invalid UTF-16. The new
    // sanitizeContentLabel uses Array.from + slice by codepoint.
    // 4-byte emoji (e.g. 😀 = U+1F600) is a surrogate pair in UTF-16.
    const name = 'a'.repeat(254) + '😀' + 'extra';  // 254 + 2 surrogates + 5 = 261 code units
    const int = makeInteraction({
      options: {
        location: 'https://www.google.com/maps/place/Cafe',
        'location-name': name,
        recipients: '<@100000000000000001>',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    // Result must NOT contain a lone surrogate. Validity check:
    // Buffer encoding round-trips clean only when surrogates pair.
    const lone = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/;
    expect(payload.locationName).not.toMatch(lone);
  });

  test('forged interaction missing required `location` → actionable ephemeral, no flow row, cooldown cleared', async () => {
    // Discord enforces required options server-side; the realistic
    // cause of a hit is a client/schema desync during a redeploy,
    // not abuse. Cooldown clears so the user can retry once the
    // deploy stabilizes.
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
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });
});

// ──────────────────────────────────────────────────────────────
// handleConfirmSendClick — Send button
// ──────────────────────────────────────────────────────────────

describe('handleConfirmSendClick', () => {
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
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    // Defer-ack within the 3s window before any awaits — without this,
    // resolveRecipientUsers + getGuildApiKey + deleteFlow can blow
    // Discord's hard ack deadline on a cold cache.
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
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
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
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 7 } });
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
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({ reason: 'terminal' }));
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/no longer reachable/),
    }));
  });

  test('no apiKey resolved → tells user setup is needed, cooldown cleared (admin action recovers)', async () => {
    mockDb.getGuildApiKey.mockResolvedValueOnce(null);
    // jest.replaceProperty restores automatically on test teardown
    // and is parallel-test-safe — beats hand-rolled
    // mutate-restore-in-finally which would silently corrupt the
    // mocked config if a future refactor parallelizes test cases
    // within this file.
    const config = require('../src/config');
    jest.replaceProperty(config, 'QURL_API_KEY', null);
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    sendCooldowns.set(SENDER_ID, Date.now());
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/not configured|setup/i),
    }));
    // Admin action required to recover → don't strand the user for
    // 30s after `/qurl setup` completes.
    expect(isOnCooldown(SENDER_ID)).toBe(false);
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
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: payloadWithGhost, version: 1 } });
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
    // Forensic log at INFO so oncall can grep for mid-flight guild
    // churn without dialing verbosity up. Split bucket: log fields
    // distinguish left-the-server (10007) from transient.
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringMatching(/partial drop at click time/),
      expect.objectContaining({ left: 1, transient: 0 }),
    );
  });

  test('partial transient lookup at Send click — Send proceeds with remaining, transient drop surfaced with retry copy (/qurl send)', async () => {
    // transientFailureIds must be surfaced with retry-encouraging
    // copy (NOT "left the server" wording) — the buckets are split
    // + threaded so a 429/gateway blip doesn't read as "they're gone".
    const flaky = '100000000000000099';
    const payloadWithFlaky = { ...validPayload, recipientIds: [u1, flaky] };
    const int = makeInteraction({
      guildMembers: { [u1]: {} },
      guildFetchByID: { [flaky]: 'ratelimit' },
    });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: payloadWithFlaky, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Preparing send/),
    }));
    // followUp distinguishes transient from "left" — retry copy. The
    // rerun command name is derived from payload.resourceType.
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/1 couldn't be looked up.*rerun \/qurl send/),
      ephemeral: true,
    }));
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringMatching(/partial drop at click time/),
      expect.objectContaining({ left: 0, transient: 1 }),
    );
  });

  test('partial transient lookup at Send click — /qurl map payload produces /qurl map rerun hint', async () => {
    // The same handler serves /qurl send and /qurl map. A user who
    // invoked /qurl map should NOT be told to "rerun /qurl send" in
    // the transient-lookup followUp. resourceType=MAPS in the payload
    // drives the hint.
    const flaky = '100000000000000099';
    const mapPayload = {
      ...validPayload,
      resourceType: 'maps',
      attachment: null,
      locationUrl: 'https://google.com/maps/place/x',
      locationName: 'x',
      resourceLabel: 'x',
      recipientIds: [u1, flaky],
    };
    const int = makeInteraction({
      guildMembers: { [u1]: {} },
      guildFetchByID: { [flaky]: 'ratelimit' },
    });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: mapPayload, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/rerun \/qurl map/),
      ephemeral: true,
    }));
    // Must NOT say /qurl send when the user invoked /qurl map.
    expect(int.followUp).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/rerun \/qurl send/),
    }));
  });

  test('getGuildApiKey throw at click time → ephemeral retry, NO deleteFlow (row stays alive), cooldown cleared', async () => {
    // getGuildApiKey runs BEFORE deleteFlow so a DDB blip doesn't
    // burn the flow row. User can re-click Send within the 3-min TTL
    // once the blip clears. clearCooldown unlocks retry so the user
    // isn't stranded for the full 30s window on a transient blip.
    mockDb.getGuildApiKey.mockRejectedValueOnce(new Error('ddb gone'));
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    sendCooldowns.set(SENDER_ID, Date.now());
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not look up the qURL API key/),
      ephemeral: true,
    }));
    expect(mockDeleteFlow).not.toHaveBeenCalled();
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/getGuildApiKey threw/),
      expect.objectContaining({ flow_id: 'fid' }),
    );
    // Zero side effects → cooldown cleared for immediate retry.
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('resolveRecipientUsers throw at click time → ephemeral retry message, NO deleteFlow', async () => {
    // Targeted catch on resolveRecipientUsers surfaces a recoverable
    // ephemeral reply and does NOT commit the dedup deleteFlow, so
    // the card stays alive for the 3-min TTL. Without it the throw
    // would hit the dispatcher's outer catch and leave the user
    // staring at an unchanged card.
    const int = makeInteraction({
      guildMembers: {},
      // Force a throw out of `members.fetch` (not a return-value error,
      // a real `throw new Error(...)`).
    });
    int.guild.members.fetch = jest.fn().mockRejectedValue(new Error('catastrophic'));
    // Need to bypass batchSettled's swallow path: rejection inside the
    // callback is caught by Promise.allSettled, then resolveRecipientUsers's
    // own try/catch handles it. Simulate a synchronous blow-up inside
    // resolveRecipientUsers by making `interaction.guild.members`
    // throw on read — that lands *after* handleConfirmSendClick's
    // bot-kicked guard (which only checks `interaction.guild`).
    Object.defineProperty(int.guild, 'members', {
      get() { throw new Error('cache exploded'); },
    });
    sendCooldowns.set(SENDER_ID, Date.now());
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: { ...validPayload, recipientIds: [u1] }, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not look up recipients/),
      ephemeral: true,
    }));
    expect(mockDeleteFlow).not.toHaveBeenCalled();
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/resolveRecipientUsers threw/),
      expect.objectContaining({ flow_id: 'fid' }),
    );
    // Zero side effects → cooldown cleared for immediate retry.
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('Send click with sender-only recipientIds → legitimate self-send, proceeds to dispatch', async () => {
    // Self-send is a supported recipient list. recipientIds=[SENDER_ID]
    // partitions to valid=[sender] (selfIncluded=true), so the empty-
    // valid branch is bypassed and the click proceeds to the send
    // pipeline. Positive-path assertions pin the dispatch entry (the
    // "Preparing send" editReply + the terminal deleteFlow that fires
    // on the happy path) so a future regression that silently no-ops
    // for self-only sends shows up here, not just as the absence of
    // the legacy error copies.
    const int = makeInteraction({ guildMembers: { [SENDER_ID]: {} } });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleConfirmSendClick(int, {
      flow_id: 'fid',
      row: { payload: { ...validPayload, recipientIds: [SENDER_ID] }, version: 1 },
    });
    // Positive: dispatch was entered.
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Preparing send/),
    }));
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      reason: 'terminal',
    }));
    // Negative: legacy error copies must NOT surface.
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Invalid recipient list/i),
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/all left the server/i),
    }));
  });

  test('forged Send click with empty recipientIds → distinct copy + deleteFlow (not the "all left" copy)', async () => {
    // A legitimate Send click only lands when the card has at least
    // one recipient (Send is disabled in the empty state). A click
    // with payload.recipientIds === [] therefore implies a fabricated
    // interaction. The all-unresolved branch's "they left the server"
    // copy is wrong here — nobody left, nobody was ever selected.
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    await handleConfirmSendClick(int, {
      flow_id: 'fid',
      row: { payload: { ...validPayload, recipientIds: [] }, version: 1 },
    });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/No recipients were selected/i),
      components: [],
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/no longer reachable/i),
    }));
    // Version-gated delete: pin expectedVersion so a concurrent
    // UserSelect transition doesn't silently wipe the more-current row.
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
      expectedVersion: 1,
    }));
  });

  test('empty recipientIds + concurrent picker race (deleteFlow returns deleted:false) → "card moved" followUp, no editReply wipe', async () => {
    // Stale-view scenario: dispatcher's loadFlow saw row v1 with
    // recipientIds=[], but a UserSelect transition advanced the row
    // to v2 between then and this Send click. The version-gated
    // deleteFlow returns deleted:false, surfacing the "card moved"
    // recovery instead of silently wiping v2.
    mockDeleteFlow.mockResolvedValueOnce({ deleted: false });
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    await handleConfirmSendClick(int, {
      flow_id: 'fid',
      row: { payload: { ...validPayload, recipientIds: [] }, version: 1 },
    });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/card moved/i),
      ephemeral: true,
    }));
    // The fixed "No recipients were selected" editReply must NOT
    // fire on the dedup-loser path.
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/No recipients were selected/i),
    }));
  });

  test('all-invalid recipients + concurrent picker race (deleteFlow returns deleted:false) → "card moved" followUp', async () => {
    // Same shape as the empty-recipients race, but the dispatcher
    // loaded a row whose recipientIds resolved to all-bots. Version-
    // gated delete catches the concurrent picker advance and surfaces
    // the recovery. Self-only is no longer all-invalid — self-send is
    // supported — so the bot-only path is the only remaining trigger.
    mockDeleteFlow.mockResolvedValueOnce({ deleted: false });
    const botId = '100000000000000999';
    const int = makeInteraction({ guildMembers: { [botId]: { bot: true } } });
    await handleConfirmSendClick(int, {
      flow_id: 'fid',
      row: { payload: { ...validPayload, recipientIds: [botId] }, version: 5 },
    });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/card moved/i),
      ephemeral: true,
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Invalid recipient list/i),
    }));
    // expectedVersion threaded through.
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      expectedVersion: 5,
    }));
  });

  test('bot kicked between confirm and Send → distinct ephemeral, flow row deleted, cooldown cleared', async () => {
    // When the bot is removed from the guild between confirm and
    // Send, `interaction.guild` is null. Without an explicit guard,
    // resolveRecipientUsers returns every recipientId in unresolvedIds
    // and the user sees "recipients left the server" — misleading.
    // Pin the dedicated copy + deleteFlow so the flow row doesn't
    // linger. Cooldown clears so the user can re-run immediately
    // after the admin re-invites the bot.
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    int.guild = null;
    sendCooldowns.set(SENDER_ID, Date.now());
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/bot is no longer in this server/i),
      components: [],
    }));
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
    }));
    expect(isOnCooldown(SENDER_ID)).toBe(false);
    // The misleading "left the server" copy must NOT fire here.
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/no longer reachable/i),
    }));
  });
});

// ──────────────────────────────────────────────────────────────
// handleConfirmCancelClick
// ──────────────────────────────────────────────────────────────

describe('handleConfirmCancelClick', () => {
  test('happy path → version-gated deleteFlow + cooldown softened to ~5s residual + update', async () => {
    // softenCooldown leaves 5s of throttle so a user can't spam
    // /qurl send → Cancel → /qurl send → Cancel and rack up
    // supersedeOrCreate DDB writes with zero cost. A legitimate
    // "I changed my mind" still has the cooldown softened from full
    // QURL_SEND_COOLDOWN_MS down to 5s.
    const int = makeInteraction();
    const cooldownStart = Date.now();
    sendCooldowns.set(SENDER_ID, cooldownStart);
    await handleConfirmCancelClick(int, { flow_id: 'fid', row: { version: 3 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
      expectedVersion: 3,
    }));
    // Cooldown ENTRY still exists (not deleted) — softening pushes
    // `last` to a value that leaves ~5s remaining. isOnCooldown is
    // therefore still true (within the 5s residual window).
    expect(sendCooldowns.has(SENDER_ID)).toBe(true);
    expect(isOnCooldown(SENDER_ID)).toBe(true);
    // The new `last` should be older than the original cooldownStart
    // (softening pushed it back so remaining time is now ~5s).
    expect(sendCooldowns.get(SENDER_ID)).toBeLessThan(cooldownStart);
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/cancelled/),
    }));
  });

  test('deleteFlow dedup loser → ephemeral message + cooldown PRESERVED (no soften)', async () => {
    // Critical: when Send won the race, we must NOT touch the cooldown
    // — Send is fanning out DMs and a soften would let the user
    // re-fire /qurl send within 5s of clicking Cancel, before the
    // first send finishes. The Cancel-loser branch leaves the
    // original cooldown timestamp intact (no softening).
    mockDeleteFlow.mockResolvedValueOnce({ deleted: false });
    const cooldownAt = Date.now();
    sendCooldowns.set(SENDER_ID, cooldownAt);
    const int = makeInteraction();
    await handleConfirmCancelClick(int, { flow_id: 'fid', row: { version: 3 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/card moved/i),
      ephemeral: true,
    }));
    // Cooldown is exactly as the test set it — Cancel-loser must not
    // soften OR clear it.
    expect(isOnCooldown(SENDER_ID)).toBe(true);
    expect(sendCooldowns.get(SENDER_ID)).toBe(cooldownAt);
  });

  test('Cancel deleteFlow is version-fenced against picker race', async () => {
    const int = makeInteraction();
    await handleConfirmCancelClick(int, { flow_id: 'fid', row: { version: 11 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      expectedVersion: 11,
    }));
  });

  test('deleteFlow throw → ephemeral retry, cooldown preserved (Send may still be in flight)', async () => {
    // Targeted catch around the Cancel deleteFlow
    // for symmetry with handleConfirmSendClick. A DDB blip during a
    // Cancel click now surfaces an actionable ephemeral instead of
    // the dispatcher's generic safety net. Cooldown stays set on the
    // throw path — Send may still be in flight, the user's cooldown
    // should honor the original Send invocation.
    mockDeleteFlow.mockRejectedValueOnce(new Error('ddb gone'));
    sendCooldowns.set(SENDER_ID, Date.now());
    const int = makeInteraction();
    await handleConfirmCancelClick(int, { flow_id: 'fid', row: { version: 3 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not cancel right now/),
      ephemeral: true,
    }));
    expect(isOnCooldown(SENDER_ID)).toBe(true);
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/deleteFlow threw/),
      expect.objectContaining({ flow_id: 'fid' }),
    );
  });
});

// ──────────────────────────────────────────────────────────────
// handleConfirmUserSelect
// ──────────────────────────────────────────────────────────────

describe('handleConfirmUserSelect', () => {
  const u1 = '100000000000000001';

  function makeSelectInteraction({
    users = [makeUser(u1)],
    roles = [],
    canMentionEveryone = false,
    guildMemberCache = null,
    ...rest
  } = {}) {
    const int = makeInteraction(rest);
    int.users = new Map(users.map((u) => [u.id, u]));
    // Mentionable picker also surfaces roles on interaction.roles.
    // Default empty so existing user-only tests stay shape-compatible
    // with resolveMentionableSelection's iteration guard.
    int.roles = new Map(roles);
    int.memberPermissions = {
      has: jest.fn(() => canMentionEveryone),
    };
    if (guildMemberCache && int.guild) {
      // Replace the default empty member cache so @everyone-role
      // expansion has members to iterate.
      int.guild.members.cache = guildMemberCache;
    }
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
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
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
    expect(int.editReply).toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Sending file/);
  });

  test('empty pick → deferUpdate, no transition, no editReply', async () => {
    const int = makeSelectInteraction({ users: [] });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).not.toHaveBeenCalled();
  });

  test('deferUpdate fires before transitionFlow await — protects Discord 3s ack budget on slow DDB', async () => {
    // Without this guard the transitionFlow DDB OCC call can blow
    // Discord's hard ack deadline on tail-latency, surfacing as an
    // "interaction failed" toast. Mirror handleConfirmSendClick /
    // handleConfirmCancelClick: ack first, then do the work.
    let deferAckedBeforeTransition = false;
    const int = makeSelectInteraction();
    mockTransitionFlow.mockImplementationOnce(async () => {
      deferAckedBeforeTransition = int.deferUpdate.mock.calls.length > 0;
      return { result: 'ok', version: 2 };
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(deferAckedBeforeTransition).toBe(true);
  });

  test('pick combining a bot AND sender → sender survives, bot is dropped, flow advances', async () => {
    // Self-send: a pick of [bot, sender] partitions to valid=[sender]
    // (selfIncluded=true). Bot is the only drop. valid.length > 0 so
    // the flow advances to the confirm card with the self-included
    // notice; the bot-drop warning still appears.
    const bot1 = '100000000000000099';
    const int = makeSelectInteraction({
      users: [makeUser(bot1, { bot: true }), makeUser(SENDER_ID)],
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientIds).toEqual([SENDER_ID]);
    expect(payload.selfIncluded).toBe(true);
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/bot/);
    expect(updated.content).toMatch(/Send includes you/);
    expect(updated.content).toMatch(/Sending file/);
  });

  test('all bots picked → re-prompt warning prepended to full confirm card (resource header preserved)', async () => {
    // An invalid pick re-renders the full confirm card with the
    // warning banner prepended via warningsBlock; needsPicker:true
    // keeps the pick prompt and the picker stays attached. Replacing
    // the content with just the warning string would strip the
    // "Sending file: report.pdf / Expires: 24h" header the user
    // chose at /qurl send time.
    const int = makeSelectInteraction({
      users: [makeUser(u1, { bot: true })],
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/bots/);
    // Resource header survives — pin the preserved-context contract.
    expect(updated.content).toMatch(/Sending file/);
    expect(updated.content).toMatch(/Expires/);
  });

  test('defense-in-depth: cap-exceeded pick rejected even though picker setMaxValues makes it unreachable today', async () => {
    // Picker caps at 25; pin the post-pick guard against a forged
    // interaction or future cap drift. Mocked QURL_SEND_MAX_RECIPIENTS
    // is 25, so a pick of 26 trips the branch.
    const users = Array.from({ length: 26 }, (_, i) => makeUser(`1000000000000000${String(i).padStart(2, '0')}`));
    const int = makeSelectInteraction({ users });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Pick at most/);
    // Same preserved-context contract as the all-bots branch.
    expect(updated.content).toMatch(/Sending file/);
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
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({ recipientIds: [u1, u2, u3] }),
    }));
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/bot/);
    expect(updated.content).toMatch(/Sending file/);
  });

  test('transitionFlow conflict → superseded message', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    const int = makeSelectInteraction();
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/superseded/),
    }));
  });

  test('transitionFlow not_found → expired message', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    const int = makeSelectInteraction();
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/expired/),
    }));
  });

  test('transitionFlow throw → targeted ephemeral retry, NOT generic "superseded" copy', async () => {
    // Without the targeted catch, a DDB blip during transitionFlow
    // bubbles to the dispatcher's outer catch which surfaces a
    // generic "superseded" message — wrong, since nothing was actually
    // superseded. Symmetric with handleConfirmSendClick /
    // handleConfirmCancelClick's DDB-call guards.
    mockTransitionFlow.mockRejectedValueOnce(new Error('ddb gone'));
    const int = makeSelectInteraction();
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not save your pick/i),
      ephemeral: true,
    }));
    // Generic "superseded" copy must NOT fire here.
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/superseded/i),
    }));
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/transitionFlow threw/),
      expect.objectContaining({ flow_id: 'fid' }),
    );
  });

  test('mentionable picker: role pick → role members expanded, flow advances with merged recipientIds', async () => {
    // Mentionable picker can surface BOTH users and roles in one
    // interaction. The role's non-bot members get merged into the
    // recipient list, deduped against directly picked users.
    const u2 = '100000000000000002';
    const u3 = '100000000000000003';
    const role = ['role-eng', {
      id: 'role-eng',
      mentionable: true,
      members: new Map([
        [u2, { user: makeUser(u2) }],
        [u3, { user: makeUser(u3) }],
      ]),
    }];
    const int = makeSelectInteraction({
      users: [makeUser(u1)],
      roles: [role],
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({
        recipientIds: expect.arrayContaining([u1, u2, u3]),
      }),
    }));
    expect(mockTransitionFlow.mock.calls[0][2].payload.recipientIds.length).toBe(3);
  });

  test('mentionable picker: @everyone role WITHOUT MENTION_EVERYONE → all-invalid branch surfaces gated warning', async () => {
    // Parallel to the text-path #323 gate: picking the @everyone role
    // (role.id === guild.id) when the user lacks MENTION_EVERYONE
    // surfaces `massMentionDenied: true` from resolveMentionableSelection,
    // hits the all-invalid branch, and renders the permission-specific
    // reason on the rejection banner. No transitionFlow fires.
    const int = makeSelectInteraction({
      users: [],
      roles: [],
      canMentionEveryone: false,
    });
    // Derive @everyone role id from int.guild.id rather than a literal —
    // a future change to the makeInteraction default would otherwise
    // silently break the role-id === guild-id detection.
    const everyoneId = int.guild.id;
    int.roles = new Map([[everyoneId, { id: everyoneId, members: new Map() }]]);
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/@everyone/);
    expect(updated.content).toMatch(/Mention Everyone/);
    // Resource header survives — same preserved-context contract.
    expect(updated.content).toMatch(/Sending file/);
  });

  test('mentionable picker: @everyone role WITH MENTION_EVERYONE → expands via guild.members.cache, flow advances', async () => {
    // With the perm, @everyone-role expansion routes through
    // guild.members.cache (not role.members — discord.js doesn't
    // reliably surface all members through @everyone's role.members).
    const u2 = '100000000000000002';
    const u3 = '100000000000000003';
    const bot1 = '100000000000000099';
    const cache = new Map([
      [u2, { user: makeUser(u2) }],
      [u3, { user: makeUser(u3) }],
      [bot1, { user: makeUser(bot1, { bot: true }) }],
    ]);
    const int = makeSelectInteraction({
      users: [],
      roles: [],
      canMentionEveryone: true,
      guildMemberCache: cache,
    });
    const everyoneId = int.guild.id;
    int.roles = new Map([[everyoneId, { id: everyoneId, members: new Map() }]]);
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const recipientIds = mockTransitionFlow.mock.calls[0][2].payload.recipientIds;
    expect(recipientIds.sort()).toEqual([u2, u3].sort());
    // Bot members filtered out by resolveMentionableSelection.
    expect(recipientIds).not.toContain(bot1);
  });

  test('mentionable picker: bot-only role pick → all-invalid branch with role-specific reason (no silent swallow)', async () => {
    // The UX regression cr #328 flagged: a picker-only flow where the
    // user picks a role of bots would early-return silently (the helper
    // filtered everything to zero, but `droppedBots` from
    // partitionRecipients stayed 0 because nothing reached it). Without
    // surfacing the role-specific reason, the user clicks the role and
    // sees nothing — reads as "the bot is broken." Helper now tracks
    // droppedFromRoles and the all-invalid branch surfaces the case.
    const bot1 = makeUser('100000000000000091', { bot: true });
    const bot2 = makeUser('100000000000000092', { bot: true });
    const botRole = ['role-bots', {
      id: 'role-bots',
      mentionable: true,
      members: new Map([[bot1.id, { user: bot1 }], [bot2.id, { user: bot2 }]]),
    }];
    const int = makeSelectInteraction({
      users: [],
      roles: [botRole],
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/no non-bot members/i);
    // Resource header survives — same preserved-context contract.
    expect(updated.content).toMatch(/Sending file/);
  });

  test('mentionable picker: sender via role pick → flow advances, selfIncluded flips on (#322 parity)', async () => {
    // Defense against a future refactor that adds a sender-filter
    // inside resolveMentionableSelection — partitionRecipients OWNS
    // the selfIncluded detection (commands.js contract). A
    // sender-only role pick lands the sender in `selected`, partition
    // sees them, valid.length === 1, selfIncluded === true.
    const senderUser = makeUser(SENDER_ID);
    const senderRole = ['role-sender', {
      id: 'role-sender',
      mentionable: true,
      members: new Map([[SENDER_ID, { user: senderUser }]]),
    }];
    const int = makeSelectInteraction({
      users: [],
      roles: [senderRole],
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientIds).toEqual([SENDER_ID]);
    expect(payload.selfIncluded).toBe(true);
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Send includes you/);
  });

  test('mentionable picker: sender via @everyone-cache expansion → selfIncluded flips on (parity with named-role path)', async () => {
    // Pin the @everyone branch as well as named-role: a sender-filter
    // regression inside resolveMentionableSelection on the
    // guild.members.cache iteration branch would slip past the
    // named-role test alone. The @everyone path iterates a different
    // collection (guild.members.cache vs role.members) so it needs
    // its own coverage.
    const senderMember = { user: makeUser(SENDER_ID) };
    const cache = new Map([[SENDER_ID, senderMember]]);
    const int = makeSelectInteraction({
      users: [],
      roles: [],
      canMentionEveryone: true,
      guildMemberCache: cache,
    });
    const everyoneId = int.guild.id;
    int.roles = new Map([[everyoneId, { id: everyoneId, members: new Map() }]]);
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientIds).toEqual([SENDER_ID]);
    expect(payload.selfIncluded).toBe(true);
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Send includes you/);
  });

  test('mentionable picker: missing interaction.memberPermissions → canMentionEveryone defaults false, @everyone denied', async () => {
    // Discord normally populates `memberPermissions` on guild
    // interactions, but the `?.has(...) === true` chain handles
    // undefined defensively. Pin that defense against a future
    // simplification that drops the optional chain.
    const int = makeSelectInteraction({
      users: [],
      roles: [],
      canMentionEveryone: false,
    });
    int.memberPermissions = undefined;
    const everyoneId = int.guild.id;
    int.roles = new Map([[everyoneId, { id: everyoneId, members: new Map() }]]);
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/@everyone/);
    expect(updated.content).toMatch(/Mention Everyone/);
  });

  test('mentionable picker: partial-valid pick with cold-cache @everyone → flow advances AND everyoneCacheCold warning surfaces', async () => {
    // UX asymmetry parallel to the droppedFromRoles partial-valid
    // surfacing: a user with MENTION_EVERYONE who picks one named
    // user + @everyone (cache cold post-restart) gets the named user
    // through partition (selected.length === 1, valid.length === 1)
    // BUT @everyone silently expanded to zero. Without surfacing
    // everyoneCacheCold in renderRecipientWarnings, the user has no
    // way to know @everyone failed and would assume the bot honored
    // their pick correctly.
    const u1 = makeUser('100000000000000001');
    const int = makeSelectInteraction({
      users: [u1],
      roles: [],
      canMentionEveryone: true,
    });
    // Strip the cache — guild remains, cache undefined.
    int.guild.members = {};
    const everyoneId = int.guild.id;
    int.roles = new Map([[everyoneId, { id: everyoneId, members: new Map() }]]);
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    // Flow advances with the named user.
    expect(mockTransitionFlow).toHaveBeenCalled();
    expect(mockTransitionFlow.mock.calls[0][2].payload.recipientIds).toEqual([u1.id]);
    // But the warning surfaces so the user knows @everyone failed.
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Member cache not yet ready/);
    expect(updated.content).toMatch(/expanded to 0 members/);
  });

  test('mentionable picker: guild without members.cache (cold cache after restart) → surfaces "Member cache not yet ready" reason', async () => {
    // Right after a bot restart, guild.members.cache may not yet be
    // populated. Without a user-visible signal, the user picks
    // @everyone and sees nothing happen — reads as "the bot is
    // broken." The helper sets everyoneCacheCold, and the all-invalid
    // branch surfaces a "try again" reason so the user has recourse.
    const int = makeSelectInteraction({
      users: [],
      roles: [],
      canMentionEveryone: true,
    });
    // Strip the cache entirely — guild remains, but `.members.cache`
    // is undefined.
    int.guild.members = {};
    const everyoneId = int.guild.id;
    int.roles = new Map([[everyoneId, { id: everyoneId, members: new Map() }]]);
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Member cache not yet ready/);
    // Resource header survives — preserved-context contract.
    expect(updated.content).toMatch(/Sending file/);
  });

  test('mentionable picker: partial-valid role (humans + bots) → flow advances AND droppedFromRoles warning surfaces', async () => {
    // Symmetric with the per-user droppedBots warning: a role pick
    // that mixes humans and bots advances the flow with the humans
    // BUT also surfaces a warning line so the user knows the role
    // was partially filtered. Without this, the partial case is
    // silent — the user sees a smaller recipient count than they
    // expected with no explanation.
    const u1 = makeUser('100000000000000001');
    const bot1 = makeUser('100000000000000091', { bot: true });
    const mixedRole = ['role-mixed', {
      id: 'role-mixed',
      mentionable: true,
      members: new Map([[u1.id, { user: u1 }], [bot1.id, { user: bot1 }]]),
    }];
    const int = makeSelectInteraction({
      users: [],
      roles: [mixedRole],
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    expect(mockTransitionFlow.mock.calls[0][2].payload.recipientIds).toEqual([u1.id]);
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/bot\(s\) filtered from picked role/i);
    expect(updated.content).toMatch(/Sending file/);
  });

  test('mentionable picker: bot user + bot-only-role pick → BOTH reasons surface (independent signals)', async () => {
    // Pin against the earlier gating that hid the role-bots reason
    // when an individual bot was also picked. Both reasons describe
    // independent picker actions and should both surface.
    const directBot = makeUser('100000000000000099', { bot: true });
    const roleBot = makeUser('100000000000000091', { bot: true });
    const botRole = ['role-bots', {
      id: 'role-bots',
      mentionable: true,
      members: new Map([[roleBot.id, { user: roleBot }]]),
    }];
    const int = makeSelectInteraction({
      users: [directBot],
      roles: [botRole],
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Cannot send to bots/);
    expect(updated.content).toMatch(/no non-bot members/i);
  });

  test('mentionable picker: multi-signal pick (bot user + denied non-mentionable role) → banner reasons follow renderRecipientWarnings ordering', async () => {
    // Pin the reason-ordering parity between the all-invalid
    // rejection banner and the warnings-block bullets. Both surfaces
    // sequence multi-signal picks as droppedBots → droppedFromRoles →
    // everyoneCacheCold → massMentionDenied → roleMentionsDenied, so
    // a future refactor that reshuffles one side without the other
    // silently breaks the COPY PARITY contract in
    // renderRecipientWarnings's docstring. Two signals (droppedBots
    // + roleMentionsDenied) are the minimum to expose ordering.
    const directBot = makeUser('100000000000000099', { bot: true });
    const u1 = makeUser('100000000000000001');
    const deniedRole = ['role-admin', {
      id: 'role-admin',
      name: 'admin',
      mentionable: false,
      members: new Map([[u1.id, { user: u1 }]]),
    }];
    const int = makeSelectInteraction({
      users: [directBot],
      roles: [deniedRole],
      canMentionEveryone: false,
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    // Index-of comparison pins ordering without depending on the
    // exact joiner between reasons (currently ". ").
    const botIdx = updated.content.indexOf('Cannot send to bots');
    const roleIdx = updated.content.indexOf('Non-mentionable role');
    expect(botIdx).toBeGreaterThanOrEqual(0);
    expect(roleIdx).toBeGreaterThanOrEqual(0);
    expect(botIdx).toBeLessThan(roleIdx);
  });

  test('re-pick preserves personalMessageRaw + personalMessage through the spread', async () => {
    // Picker's newPayload is `{ ...payload, recipientIds, recipientAliases,
    // warningsBlock }` — the spread carries personalMessage and
    // personalMessageRaw through. A regression that destructured
    // `personalMessage` (without `Raw`) into newPayload would silently
    // drop the Edit-pre-fill on the next click. Symmetric with the
    // expiry-handler's "preserves all other payload fields" test.
    const payloadWithNote = {
      ...initialPayload,
      personalMessage: '\\*\\*hi\\*\\*',
      personalMessageRaw: '**hi**',
    };
    const int = makeSelectInteraction();
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: payloadWithNote, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({
        personalMessage: '\\*\\*hi\\*\\*',
        personalMessageRaw: '**hi**',
      }),
    }));
  });

  // ── Issue #326 picker-path gate (handleConfirmUserSelect wiring) ──
  test('mentionable picker: non-mentionable role WITHOUT MENTION_EVERYONE → all-invalid banner with role-specific reason', async () => {
    // Picker-path parallel to the parser-path #326 gate. The role's
    // members do NOT expand; the all-invalid branch surfaces a
    // "Non-mentionable role" reason on the rejection banner (NOT
    // "@everyone" copy — distinct gate, distinct reason).
    const u1 = makeUser('100000000000000001');
    const adminRole = ['role-admin', {
      id: 'role-admin',
      name: 'admin',
      mentionable: false,
      members: new Map([[u1.id, { user: u1 }]]),
    }];
    const int = makeSelectInteraction({
      users: [],
      roles: [adminRole],
      canMentionEveryone: false,
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    // Singular noun + singular verb stay in lockstep — most one-role
    // picks hit this banner, so "role requires" (not "role require")
    // is the user-visible default.
    expect(updated.content).toMatch(/Non-mentionable role requires/i);
    expect(updated.content).toMatch(/Mention Everyone/);
    // Per-role-bypass mention surfaces inline on the banner so the
    // user doesn't have to find the per-role bullet copy (which only
    // the partial-valid surface renders) to learn the workaround.
    // Phrasing reflects indirect agency — the user lacks MENTION_EVERYONE
    // (by definition reaching this banner) and likely lacks Manage Roles,
    // so "have the role marked" is more accurate than the imperative
    // "mark the role" would be.
    expect(updated.content).toMatch(/have the role marked as mentionable/i);
    // Resource header survives — preserved-context contract.
    expect(updated.content).toMatch(/Sending file/);
  });

  test('mentionable picker: multiple non-mentionable roles → banner uses plural noun + verb ("roles require")', async () => {
    // Sibling to the singular-form pin above. Two denied roles in
    // one pick — the banner reason builder must flip both noun
    // ("role" → "roles") AND verb ("requires" → "require") in
    // lockstep. A regression that bumped only one would render
    // either "roles requires" or "role require." Pin both.
    const u1 = makeUser('100000000000000001');
    const u2 = makeUser('100000000000000002');
    const roleA = ['role-a', {
      id: 'role-a', name: 'admin-a', mentionable: false,
      members: new Map([[u1.id, { user: u1 }]]),
    }];
    const roleB = ['role-b', {
      id: 'role-b', name: 'admin-b', mentionable: false,
      members: new Map([[u2.id, { user: u2 }]]),
    }];
    const int = makeSelectInteraction({
      users: [],
      roles: [roleA, roleB],
      canMentionEveryone: false,
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Non-mentionable roles require/i);
  });

  test('mentionable picker: non-mentionable role + valid user pick → partial-valid, warnings block lists role NAME', async () => {
    // Mixed pick: a directly-picked user lands in recipients (flow
    // advances), but the denied role surfaces in the warnings block
    // with its NAME (per-role copy from renderRecipientWarnings). Pin
    // the name resolution so a regression that dropped guild.roles.cache
    // -> name fallback would surface here.
    const u1 = makeUser('100000000000000001');
    const u2 = makeUser('100000000000000002');
    const adminRole = ['role-admin', {
      id: 'role-admin',
      name: 'admin-team',
      mentionable: false,
      members: new Map([[u2.id, { user: u2 }]]),
    }];
    const int = makeSelectInteraction({
      users: [u1],
      roles: [adminRole],
      canMentionEveryone: false,
    });
    // Mirror the picked role into guild.roles.cache so the caller's
    // name lookup (`guild.roles.cache.get(id)?.name`) finds it.
    int.guild.roles.cache.set('role-admin', { id: 'role-admin', name: 'admin-team', mentionable: false });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    // u2 (role member) NOT expanded; u1 (directly picked) IS.
    expect(mockTransitionFlow.mock.calls[0][2].payload.recipientIds).toEqual([u1.id]);
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/@admin-team/);
    expect(updated.content).toMatch(/Mention Everyone/);
    expect(updated.content).toMatch(/role\.mentionable: true/);
  });

  test('mentionable picker: denied role with deleted-from-cache name → fallback "unknown-role" renders', async () => {
    // Race condition: role denied at resolveMentionableSelection time
    // but removed from guild.roles.cache before renderRecipientWarnings
    // (e.g., admin deleted the role mid-flow). `guild.roles.cache.get
    // (id)?.name` returns undefined; caller falls back to
    // "unknown-role" so the bullet renders rather than collapsing
    // to `@undefined`.
    const u1 = makeUser('100000000000000001');
    const denied = ['role-ghost', {
      id: 'role-ghost',
      name: 'ghost',
      mentionable: false,
      members: new Map(),
    }];
    const int = makeSelectInteraction({
      users: [u1],
      roles: [denied],
      canMentionEveryone: false,
    });
    // Picker registers the role, but guild.roles.cache stays empty
    // (the role was deleted from the guild between pick and render).
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    // Fallback name appears in the bullet so the user still sees the
    // permission copy rather than a broken `@undefined` line.
    expect(updated.content).toMatch(/@unknown-role/);
  });

  test('mentionable picker: non-mentionable role + canMentionEveryone → expands normally (no deny)', async () => {
    // The gate is OR-ed: MENTION_EVERYONE permission bypasses the
    // role.mentionable check. Pin the bypass — without it, even an
    // admin couldn't pick a non-mentionable role through the picker.
    const u1 = makeUser('100000000000000001');
    const role = ['role-admin', {
      id: 'role-admin',
      name: 'admin',
      mentionable: false,
      members: new Map([[u1.id, { user: u1 }]]),
    }];
    const int = makeSelectInteraction({
      users: [],
      roles: [role],
      canMentionEveryone: true,
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    expect(mockTransitionFlow.mock.calls[0][2].payload.recipientIds).toEqual([u1.id]);
  });

  // ── guild.members cache pre-warm (picker path) ──
  // Symmetric with the text-path pre-warm tests above handleQurlMap:
  // resolveMentionableSelection reads guild.members.cache for both
  // `@everyone` (direct iteration) and arbitrary roles (`role.members`
  // is a filtered view of the same cache). Without the pre-warm, any
  // role pick silently expands to just the interacting user.
  test('role pick → members.fetch() pre-warm fires', async () => {
    const role = { id: 'roleA', name: 'team', mentionable: true, members: new Map([[u1, { user: makeUser(u1) }]]) };
    const int = makeSelectInteraction({ users: [], roles: [['roleA', role]] });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.guild.members.fetch.mock.calls.find(isPrewarmCall)).toBeTruthy();
  });

  test('users-only pick → members.fetch() pre-warm does NOT fire', async () => {
    // Pure user picks don't touch guild.members.cache — Discord
    // ships the User object directly in interaction.users. Skipping
    // the pre-warm keeps the common-case cost at zero.
    const int = makeSelectInteraction({ users: [makeUser(u1)], roles: [] });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.guild.members.fetch.mock.calls.filter(isPrewarmCall)).toEqual([]);
  });

  test('role pick + members.fetch() rejection is swallowed — flow continues', async () => {
    // Picker analog of the text-path degraded-mode test: a 429 on the
    // pre-warm must not crash the handler. Expansion falls back to
    // whatever the cache currently holds.
    const role = { id: 'roleB', name: 'team', mentionable: true, members: new Map([[u1, { user: makeUser(u1) }]]) };
    const int = makeSelectInteraction({ users: [], roles: [['roleB', role]] });
    int.guild.members.fetch = jest.fn(async (arg) => {
      if (isPrewarmCall([arg])) {
        const err = new Error('rate limited'); err.code = 429; throw err;
      }
      return { user: makeUser(arg) };
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const logger = require('../src/logger');
    const warnCall = logger.warn.mock.calls.find(
      ([msg]) => typeof msg === 'string' && msg.includes('members.fetch pre-warm failed'),
    );
    expect(warnCall).toBeTruthy();
  });
});

// ──────────────────────────────────────────────────────────────
// handleConfirmVoiceEveryone — "Everyone on voice"
// button on the confirm card, rendered only when the slash command
// was invoked from a voice / stage-voice channel. Resolves voice-
// connected non-bot members AT CLICK TIME from the voice-state
// cache rather than trusting a render-time snapshot.
// ──────────────────────────────────────────────────────────────

describe('handleConfirmVoiceEveryone', () => {
  const VOICE_CH = 'voice-ch-1';
  const u1 = '100000000000000001';
  const u2 = '100000000000000002';
  const bot1 = '100000000000000099';

  // Build a guild whose channels.cache contains a voice channel with
  // the supplied member list. Bots in `members` get bot:true on the
  // member-cache user so partitionRecipients drops them.
  function makeVoiceInteraction({ members = [], channelType = 2, botIds = [] } = {}) {
    const int = makeInteraction();
    int.guild.channels.cache = new Map();
    const chanMembers = new Map();
    for (const mid of members) {
      const isBot = botIds.includes(mid);
      const member = { user: { id: mid, bot: isBot } };
      int.guild.members.cache.set(mid, member);
      chanMembers.set(mid, member);
    }
    int.guild.channels.cache.set(VOICE_CH, {
      id: VOICE_CH, type: channelType, members: chanMembers,
    });
    return int;
  }

  const basePayload = {
    resourceType: 'file',
    resourceLabel: 'x.png',
    recipientIds: [],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
    voiceChannelId: VOICE_CH,
  };

  test('happy path: voice-connected non-bot members populate recipientIds and advance the flow', async () => {
    const int = makeVoiceInteraction({ members: [u1, u2] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: expect.objectContaining({
        recipientIds: expect.arrayContaining([u1, u2]),
        voiceChannelId: VOICE_CH,
      }),
      terminal: false,
    }));
    const ids = mockTransitionFlow.mock.calls[0][2].payload.recipientIds;
    expect(ids).toHaveLength(2);
  });

  test('happy path: bots are filtered out of the connected set', async () => {
    const int = makeVoiceInteraction({ members: [u1, bot1, u2], botIds: [bot1] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const ids = mockTransitionFlow.mock.calls[0][2].payload.recipientIds;
    expect(ids.sort()).toEqual([u1, u2].sort());
    expect(ids).not.toContain(bot1);
  });

  test('missing voiceChannelId in payload: re-renders card WITHOUT the voice button (visible feedback)', async () => {
    // The button should not have rendered without voiceChannelId; a
    // click here means a forged interaction or schema drift. Handler
    // must give the user VISIBLE feedback that something changed —
    // a silent ack after deferUpdate would leave the user staring at
    // an unchanged card wondering if their click registered. The
    // re-render strips the broken affordance (renderConfirmCardRows
    // conditions on voiceChannelId being set); the user can pick
    // recipients through the still-visible UserSelectMenu.
    const int = makeVoiceInteraction({ members: [u1] });
    const payloadWithoutVoice = { ...basePayload, voiceChannelId: null };
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: payloadWithoutVoice, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    // Card content re-rendered (not just components: []) so the
    // user sees the resource header still — they didn't lose context.
    expect(lastCall.content).toBeTruthy();
    expect(Array.isArray(lastCall.components)).toBe(true);
    expect(lastCall.components.length).toBeGreaterThan(0);
  });

  test('missing voiceChannelId WITH previously-picked recipients: re-render preserves recipients (no UI/state drift)', async () => {
    // Bug-guard: the inline-rebuild alternative would render the
    // card with validRecipients:[] while the persisted payload still
    // carried the old recipientIds. The handler routes through
    // rerenderConfirmCard, which reads recipientIds from the payload
    // and reconstructs validRecipients — so the UI matches state.
    const int = makeVoiceInteraction({ members: [u1] });
    // Seed cache so rerenderConfirmCard can resolve the recipient.
    int.guild.members.cache.set(u1, { user: makeUser(u1) });
    const payloadWithoutVoice = {
      ...basePayload,
      voiceChannelId: null,
      recipientIds: [u1],
      recipientAliases: { [u1]: 'alice' },
    };
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: payloadWithoutVoice, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    // Resource header still rendered — user retains context.
    expect(lastCall.content).toMatch(/Sending file/);
    // Previously-picked recipient surfaces in the content summary
    // ("**To:** 1 user (…)"), not just the persisted-but-invisible
    // state. The inline-rebuild alternative would have rendered
    // validRecipients:[] here, masking the persisted state drift.
    expect(lastCall.content).toMatch(/\*\*To:\*\* 1 user/);
  });

  test('channel deleted between render and click: rejectVoice path runs (warning re-render, no transition)', async () => {
    // Cache miss simulates the channel being deleted (or the voice
    // intent dropping mid-flow). The handler should NOT call
    // transitionFlow — it re-renders the card with a warning banner.
    const int = makeInteraction();
    int.guild.channels.cache = new Map(); // no entry for VOICE_CH
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/Couldn't read the voice channel/i);
  });

  test('empty voice channel: rejectVoice path runs (no-one-connected copy, no transition)', async () => {
    const int = makeVoiceInteraction({ members: [] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/No one is connected/i);
  });

  test('bots-only voice channel: surfaces "Cannot send to bots" (NOT the empty-channel copy)', async () => {
    // Pre-DE-pass had an inline bot pre-filter here that zeroed out
    // droppedBots, forcing the bots-only case into the misleading
    // "No one is connected" branch. Letting partitionRecipients own
    // the bot accounting end-to-end is what produces the accurate
    // copy. This test pins that behavioral contract.
    const int = makeVoiceInteraction({ members: [bot1], botIds: [bot1] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/Cannot send to bots/i);
    expect(lastCall.content).not.toMatch(/No one is connected/i);
  });

  test('sender is voice-connected → excluded from recipientIds + selfIncluded:false', async () => {
    // Voice-everyone semantically means "everyone else in the room,"
    // not "and CC myself." partitionRecipients's `excludeSender: true`
    // option drops the sender pre-validity, so the new payload
    // structurally cannot carry selfIncluded:true on this path. Pins
    // the contract — a future refactor that read recipientIds from
    // channel.members.keys() directly would silently re-include the
    // sender.
    const int = makeVoiceInteraction({ members: [SENDER_ID, u1] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientIds).toEqual([u1]);
    expect(payload.recipientIds).not.toContain(SENDER_ID);
    expect(payload.selfIncluded).toBe(false);
  });

  test('sender NOT in voice channel → selfIncluded:false on the new payload', async () => {
    const int = makeVoiceInteraction({ members: [u1, u2] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.selfIncluded).toBe(false);
  });

  test('new payload carries recipientMode:"voice" — commits the layout switch', async () => {
    // Voice-mode hides the picker row and swaps the bottom button to
    // "👥 Pick people instead." Without persisting the mode, a
    // subsequent expiry/note interaction's re-render would re-derive
    // picker-mode layout and snap back, leaving the user in a
    // confusing "did my click take?" state.
    const int = makeVoiceInteraction({ members: [u1, u2] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientMode).toBe('voice');
  });

  test('sender-only voice channel → reject path with "you\'re the only one" copy', async () => {
    // After excludeSender, a channel containing only the sender yields
    // valid:[]. The reject copy surfaces the actual reason rather than
    // the misleading "No one is connected" branch (which is reserved
    // for an actually-empty channel).
    const int = makeVoiceInteraction({ members: [SENDER_ID] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/only one in this voice channel/i);
  });

  test('transitionFlow conflict → superseded message (OCC race with sibling interaction)', async () => {
    // Mirrors handleConfirmUserSelect's conflict-path test. The
    // version-checked transitionFlow can lose to a concurrent picker
    // click / menu change / cancel; the handler must surface
    // "Send was superseded" rather than the generic editReply.
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    const int = makeVoiceInteraction({ members: [u1, u2] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/superseded/);
    expect(lastCall.components).toEqual([]);
  });

  test('partial-cache row (member missing .user) emits a debug log per send (silent-shrinkage telemetry)', async () => {
    // Pins the round-9 telemetry contract: silent shrinkage of the
    // recipient set due to partial-cache rows is hard to diagnose
    // post-hoc without a log. The debug-level emission is per-send
    // (not per-member) so volume tracks frequency of the degraded
    // state, not the count of dropped members. A future refactor
    // that drops the log fails this spec.
    const logger = require('../src/logger');
    logger.debug.mockClear();
    const int = makeVoiceInteraction({ members: [u1] });
    // Inject a partial-cache row: member entry exists but has no .user.
    const channel = int.guild.channels.cache.get(VOICE_CH);
    channel.members.set('partial-cache-id', {});  // no .user → drop
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringMatching(/partial-cache/i),
      expect.objectContaining({
        flow_id: 'fid',
        voice_channel_id: VOICE_CH,
        dropped: 1,
      })
    );
    // Send still proceeds with the .user-populated member.
    expect(mockTransitionFlow).toHaveBeenCalled();
  });

  test('voice-everyone button succeeds when sender lacks MENTION_EVERYONE (no asymmetric gate)', async () => {
    // Pins the intentional asymmetry documented at the button block:
    // ViewChannel-only gating (not MENTION_EVERYONE) is the right
    // posture for a co-presence surface. If a future security pass
    // adds the MENTION_EVERYONE gate to one path and forgets the
    // other, this fails loud.
    const int = makeVoiceInteraction({ members: [u1, u2] });
    // memberPermissions.has(MENTION_EVERYONE) returns false (default
    // makeInteraction shape — no permissions populated). Voice path
    // must still succeed.
    int.memberPermissions = { has: () => false };
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientIds.sort()).toEqual([u1, u2].sort());
  });

  test('transitionFlow not_found → expired message (row TTL elapsed between click and write)', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    const int = makeVoiceInteraction({ members: [u1, u2] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/expired/);
    expect(lastCall.components).toEqual([]);
  });

  test('cap overshoot (env-overridden small cap): hard-rejects with subset-prompt copy', async () => {
    // With the production 20k default, voice-channel capacity (99
    // for voice, larger for stage) never trips this branch — it's
    // defense-in-depth against a misconfigured env override (e.g.,
    // a guild operator dialing the cap down to constrain blast
    // radius). The reject copy steers the user toward picker /
    // @-mentions for a subset selection.
    const config = require('../src/config');
    const origCap = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 1;
    try {
      const int = makeVoiceInteraction({ members: [u1, u2] });
      await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
      expect(mockTransitionFlow).not.toHaveBeenCalled();
      const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
      // "eligible recipients" wording is shared with the slash-entry
      // over-cap banner — both counts are post-partition (sender +
      // bots filtered), so "connected" would diverge from Discord's
      // voice panel.
      expect(lastCall.content).toMatch(/Voice channel has 2 eligible recipients \(max 1\)/i);
      expect(lastCall.content).toMatch(/picker or @mentions/i);
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = origCap;
    }
  });

  test('corrupt payload (unknown resourceType): deleteFlow + actionable re-run copy', async () => {
    // Mirrors handleConfirmUserSelect's resourceType guard — a
    // corrupt/stale row would otherwise crash the renderer with a
    // generic "superseded" toast.
    const int = makeVoiceInteraction({ members: [u1] });
    const corruptPayload = { ...basePayload, resourceType: 'bogus' };
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: corruptPayload, version: 1 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
    }));
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/Card data is corrupted/);
  });

  test('success-path emits info-level audit log with counts', async () => {
    // Mirror handleConfirmEveryone's success-log test — a successful
    // voice-channel fan-out is a load-bearing audit signal ("did
    // someone fan to N members of #voice-channel?") and should be
    // findable in logs without a DDB scan of qurl_send_configs.
    //
    // Exact-value asserts (vs expect.any) catch regressions where
    // the bot filter double-counts, partial-cache rows leak into the
    // valid set, or voice_member_count drifts away from
    // channel.members.size. guild_id + user_id pinned so a future
    // forensics consumer that greps by them is protected by the spec.
    const int = makeVoiceInteraction({ members: [u1, u2] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const logger = require('../src/logger');
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringContaining('voice @everyone expansion succeeded'),
      expect.objectContaining({
        flow_id: 'fid',
        guild_id: int.guildId,
        user_id: int.user.id,
        voice_channel_id: VOICE_CH,
        valid_count: 2,
        dropped_bots: 0,
        partial_cache_drops: 0,
        self_included: false,
        voice_member_count: 2,
      }),
    );
  });

  test('success-log: voice_member_count tracks channel.members.size, NOT valid.length, under partial-cache drops', async () => {
    // Locks down the voice_member_count semantic: it's the *raw* size
    // of channel.members BEFORE the partial-cache + bot filter drop
    // members. A regression that swaps it to `valid.length` would
    // pass the happy-path test (counts match when no drops) but lose
    // the audit signal — operators would no longer see the shrinkage
    // gap (voice_member_count - valid_count = drops).
    const int = makeVoiceInteraction({ members: [u1, u2] });
    // Inject a partial-cache row so channel.members.size = 3 but
    // valid.length stays at 2 after the partial-cache drop. The
    // empty object `{}` triggers the no-`.user` branch in the voice
    // resolution loop (commands.js: `if (m?.user) ... else partialCacheDrops++`).
    const channel = int.guild.channels.cache.get(VOICE_CH);
    channel.members.set('partial-cache-id', {});
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const logger = require('../src/logger');
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringContaining('voice @everyone expansion succeeded'),
      expect.objectContaining({
        valid_count: 2,
        partial_cache_drops: 1,
        voice_member_count: 3,
      }),
    );
  });

  test('success-log does NOT fire on transitionFlow conflict / not_found / throw', async () => {
    // The audit log is reserved for the actual flow-advancement path.
    // Surfacing it on conflict / not_found / throw would mislead
    // operators auditing fan-outs ("did this admin send to N members?")
    // — the answer is NO on those branches. Pin the negative spec so
    // a future log-placement refactor that moves the .info() ahead of
    // the early-returns regresses loudly.
    const logger = require('../src/logger');

    // conflict (OCC race)
    logger.info.mockClear();
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    await handleConfirmVoiceEveryone(
      makeVoiceInteraction({ members: [u1, u2] }),
      { flow_id: 'fid', row: { payload: basePayload, version: 1 } }
    );
    expect(logger.info).not.toHaveBeenCalledWith(
      expect.stringContaining('voice @everyone expansion succeeded'),
      expect.anything(),
    );

    // not_found (row TTL elapsed)
    logger.info.mockClear();
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    await handleConfirmVoiceEveryone(
      makeVoiceInteraction({ members: [u1, u2] }),
      { flow_id: 'fid', row: { payload: basePayload, version: 1 } }
    );
    expect(logger.info).not.toHaveBeenCalledWith(
      expect.stringContaining('voice @everyone expansion succeeded'),
      expect.anything(),
    );

    // synchronous throw (DDB blip)
    logger.info.mockClear();
    mockTransitionFlow.mockRejectedValueOnce(new Error('DDB unavailable'));
    await handleConfirmVoiceEveryone(
      makeVoiceInteraction({ members: [u1, u2] }),
      { flow_id: 'fid', row: { payload: basePayload, version: 1 } }
    );
    expect(logger.info).not.toHaveBeenCalledWith(
      expect.stringContaining('voice @everyone expansion succeeded'),
      expect.anything(),
    );
  });
});

// ──────────────────────────────────────────────────────────────
// handleConfirmPickManual — "Pick people instead" button on the
// confirm card. Voice-mode escape hatch: flips recipientMode back to
// 'picker', clears the voice-resolved recipientIds, and re-renders.
// ──────────────────────────────────────────────────────────────

describe('handleConfirmPickManual', () => {
  const u1 = '100000000000000001';
  const u2 = '100000000000000002';
  const VOICE_CH = 'voice-ch-pm';

  const voicePayload = {
    resourceType: 'file',
    resourceLabel: 'x.png',
    recipientIds: [u1, u2],
    recipientAliases: { [u1]: 'alice', [u2]: 'bob' },
    recipientMode: 'voice',
    voiceChannelId: VOICE_CH,
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
    warningsBlock: '',
  };

  test('clears recipientIds + recipientAliases and flips recipientMode → "picker"', async () => {
    const int = makeInteraction();
    await handleConfirmPickManual(int, { flow_id: 'fid', row: { payload: voicePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: expect.objectContaining({
        recipientMode: 'picker',
        recipientIds: [],
        recipientAliases: {},
        selfIncluded: false,
      }),
      terminal: false,
    }));
  });

  test('preserves resourceType / expiresIn / personalMessage from the original payload', async () => {
    // The toggle is purely UI mode; resource + send-config fields
    // carry through unchanged so the user keeps their context.
    const int = makeInteraction();
    const payloadWithExtras = {
      ...voicePayload,
      expiresIn: '7d',
      selfDestructSeconds: 60,
      personalMessage: 'hi team',
    };
    await handleConfirmPickManual(int, { flow_id: 'fid', row: { payload: payloadWithExtras, version: 1 } });
    const newPayload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(newPayload.resourceType).toBe('file');
    expect(newPayload.expiresIn).toBe('7d');
    expect(newPayload.selfDestructSeconds).toBe(60);
    expect(newPayload.personalMessage).toBe('hi team');
    expect(newPayload.voiceChannelId).toBe(VOICE_CH);
  });

  test('corrupt resourceType → deleteFlow + "Card data is corrupted" copy', async () => {
    const int = makeInteraction();
    const corrupt = { ...voicePayload, resourceType: 'audio' };
    await handleConfirmPickManual(int, { flow_id: 'fid', row: { payload: corrupt, version: 1 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
    }));
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/Card data is corrupted/);
  });

  test('transitionFlow conflict → superseded message', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    const int = makeInteraction();
    await handleConfirmPickManual(int, { flow_id: 'fid', row: { payload: voicePayload, version: 1 } });
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/superseded/);
    expect(lastCall.components).toEqual([]);
  });

  test('transitionFlow not_found → expired message', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    const int = makeInteraction();
    await handleConfirmPickManual(int, { flow_id: 'fid', row: { payload: voicePayload, version: 1 } });
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/expired/);
    expect(lastCall.components).toEqual([]);
  });

  test('transitionFlow synchronous throw → ephemeral retry followUp (DDB blip recovery)', async () => {
    // Symmetric with the expiry / self-destruct handler throw tests.
    // A DDB outage during the mode-toggle write would otherwise bubble
    // through the dispatcher's generic "superseded" copy — wrong, since
    // nothing was actually superseded. The targeted followUp keeps the
    // user's interaction acked and surfaces actionable retry copy.
    mockTransitionFlow.mockRejectedValueOnce(new Error('DDB blip'));
    const int = makeInteraction();
    await handleConfirmPickManual(int, { flow_id: 'fid', row: { payload: voicePayload, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not switch to manual picker/i),
      ephemeral: true,
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/superseded/i),
    }));
  });
});

// ──────────────────────────────────────────────────────────────
// handleConfirmEveryone — @everyone button click handler
// ──────────────────────────────────────────────────────────────
// Workaround for Discord's MentionableSelectMenu filtering @everyone
// out of its dropdown. Click-time semantics mirror the picker's
// @everyone-role branch: pre-warm cache → expand to non-bot members →
// partition → transition flow.

describe('handleConfirmEveryone', () => {
  const u1 = '100000000000000001';
  const u2 = '100000000000000002';
  const bot1 = '100000000000000099';

  function makeEveryoneInteraction({
    canMentionEveryone = true,
    guildMembers = {
      [u1]: {},
      [u2]: {},
      [bot1]: { bot: true },
      [SENDER_ID]: {},
    },
    memberCount = 4,
    ...rest
  } = {}) {
    const int = makeInteraction({ guildMembers, ...rest });
    int.memberPermissions = { has: jest.fn(() => canMentionEveryone) };
    if (int.guild) int.guild.memberCount = memberCount;
    return int;
  }

  const initialPayload = {
    resourceType: 'file',
    resourceLabel: 'x.png',
    recipientIds: [],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
    recipientMode: 'picker',
  };

  const { handleConfirmEveryone } = require('../src/commands');

  test('happy path → expands to all non-bot members, transitions flow', async () => {
    const int = makeEveryoneInteraction();
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientIds.sort()).toEqual([u1, u2, SENDER_ID].sort());
    expect(payload.selfIncluded).toBe(true);
    // Mode switches to EVERYONE so the re-render hides the picker
    // row. Auto-filling the picker would either silently truncate at
    // Discord's 25-entry default_values cap or read back through the
    // picker handler and replace the @everyone fan-out with a subset.
    expect(payload.recipientMode).toBe('everyone');
  });

  test('without MENTION_EVERYONE → reject with permission warning, no transition', async () => {
    // Defense-in-depth re-check against forged interactions. Render-
    // time gate already hides the button; this branch defends against
    // an HTTP interaction crafted with the custom_id directly.
    const int = makeEveryoneInteraction({ canMentionEveryone: false });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Mention Everyone/);
    const logger = require('../src/logger');
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('without MENTION_EVERYONE'),
      expect.any(Object),
    );
  });

  test('cache stays empty after prewarm despite populated guild → reject with "try again" copy', async () => {
    // The production-relevant scenario: prewarm completed but landed
    // zero members in cache (gateway blip, partial chunk delivery,
    // discord.js cache eviction race). memberCount > 0 distinguishes
    // this from the degenerate empty-guild case (which can't reach
    // the click handler at all — slash commands need a guild context).
    const int = makeEveryoneInteraction({ guildMembers: {}, memberCount: 5 });
    int.guild.members.fetch = jest.fn().mockResolvedValue(new Map());
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/member cache not ready/i);
  });

  test('sender-only-in-cache (no other non-bots) → reject with "matched only you" copy', async () => {
    // Click-intent for 📢 @everyone is "fan out to others." If the
    // guild has just the sender + bots, falling through to partition
    // would silently send to just the sender — defensible self-send
    // semantics, but misleading for an @everyone click. Reject
    // explicitly; self-send via the picker is still possible.
    const int = makeEveryoneInteraction({
      // Sender in cache, only bots otherwise (no other humans).
      guildMembers: { [SENDER_ID]: {}, [bot1]: { bot: true } },
      memberCount: 2,
    });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/matched only you/i);
  });

  test('valid.length === cap (exactly at boundary) → proceeds (cap-reject is strictly >)', async () => {
    // Boundary test: the cap-reject is `valid.length > cap`, so exactly
    // at cap should proceed. Voice-everyone shares the same
    // partitionRecipients cap surface; cheap insurance against an
    // off-by-one drift on either handler.
    const config = require('../src/config');
    const originalCap = config.QURL_SEND_MAX_RECIPIENTS;
    try {
      Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: 3, configurable: true, writable: true });
      const int = makeEveryoneInteraction({
        guildMembers: {  // 3 non-bots exactly = cap
          [SENDER_ID]: {},
          '100000000000000010': {},
          '100000000000000011': {},
        },
        memberCount: 3,
      });
      await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
      expect(mockTransitionFlow).toHaveBeenCalled();
      const payload = mockTransitionFlow.mock.calls[0][2].payload;
      expect(payload.recipientIds.length).toBe(3);
    } finally {
      Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: originalCap, configurable: true, writable: true });
    }
  });

  test('forged-interaction warn log includes guild_id for forensics correlation', async () => {
    // The success info log carries guild_id; the forged warn log
    // should too, so ops can correlate "is one user forging across
    // guilds?" without joining lines.
    const int = makeEveryoneInteraction({ canMentionEveryone: false });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    const logger = require('../src/logger');
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('without MENTION_EVERYONE'),
      expect.objectContaining({ flow_id: 'fid', guild_id: expect.any(String) }),
    );
  });

  test('only bots in cache → reject with bots-dropped copy', async () => {
    const int = makeEveryoneInteraction({
      guildMembers: { [bot1]: { bot: true } },
      memberCount: 1,
    });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/No usable recipients|bot/i);
  });

  test('cache size > QURL_SEND_MAX_RECIPIENTS → hard reject (no truncation)', async () => {
    const config = require('../src/config');
    const originalCap = config.QURL_SEND_MAX_RECIPIENTS;
    try {
      Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: 2, configurable: true, writable: true });
      const int = makeEveryoneInteraction();  // 3 non-bots > cap 2
      await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
      expect(mockTransitionFlow).not.toHaveBeenCalled();
      const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
      expect(reply.content).toMatch(/max 2/);
      expect(reply.content).toMatch(/picker|@mentions/i);
    } finally {
      Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: originalCap, configurable: true, writable: true });
    }
  });

  test('corrupt resourceType → deleteFlow + actionable error, no transition', async () => {
    const int = makeEveryoneInteraction();
    await handleConfirmEveryone(int, {
      flow_id: 'fid',
      row: { payload: { ...initialPayload, resourceType: 'mystery' }, version: 1 },
    });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM, reason: 'terminal',
    }));
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/corrupted/i);
  });

  test('deferUpdate fires before transitionFlow — Discord 3s ack guard', async () => {
    let deferAckedBeforeTransition = false;
    const int = makeEveryoneInteraction();
    mockTransitionFlow.mockImplementationOnce(async () => {
      deferAckedBeforeTransition = int.deferUpdate.mock.calls.length > 0;
      return { result: 'ok', version: 2 };
    });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(deferAckedBeforeTransition).toBe(true);
  });

  test('transitionFlow returns conflict → "Send was superseded" copy, no followup', async () => {
    const int = makeEveryoneInteraction();
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/superseded/i);
    expect(reply.components).toEqual([]);
    expect(int.followUp).not.toHaveBeenCalled();
  });

  test('transitionFlow returns not_found → "send expired" copy, no followup', async () => {
    const int = makeEveryoneInteraction();
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/expired/i);
    expect(reply.components).toEqual([]);
    expect(int.followUp).not.toHaveBeenCalled();
  });

  test('transitionFlow throws → ephemeral followUp with retry copy', async () => {
    const int = makeEveryoneInteraction();
    mockTransitionFlow.mockRejectedValueOnce(new Error('ddb blip'));
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not save.*Try again/i),
      ephemeral: true,
    }));
    const logger = require('../src/logger');
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('handleConfirmEveryone: transitionFlow threw'),
      expect.any(Object),
    );
  });

  test('sender row missing from cache + other non-bots present → defensively pushed into recipients', async () => {
    // Rare race: prewarm completes but the sender's own member entry
    // happened to be missing from the cache (fresh shard resume).
    // Without the defensive push, clicking 📢 @everyone would silently
    // drop the sender (`selfIncluded: false`) — they'd be missing from
    // the recipient set they explicitly triggered.
    const otherUser = '100000000000000077';
    const int = makeEveryoneInteraction({
      // Sender deliberately ABSENT from cache; one other non-bot present.
      guildMembers: { [otherUser]: {} },
      memberCount: 2,  // sender + otherUser
    });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    // Sender + otherUser should both land in recipients (sender via
    // defensive push, otherUser via cache).
    expect(payload.recipientIds.sort()).toEqual([SENDER_ID, otherUser].sort());
    expect(payload.selfIncluded).toBe(true);
  });

  test('sender missing + warm cache with degraded .user-less row + other non-bots → defensive push fires, partial-cache drops counted', async () => {
    // Positive coverage for the combination of: warm cache with at
    // least one valid non-bot AND at least one degraded row (`.user`
    // missing — partial GUILD_MEMBERS_CHUNK shape) AND sender's own
    // row absent. The defensive push fires (other non-bots present),
    // the degraded row is counted as partialCacheDrops, and the
    // success log captures the right shape.
    const otherUser = '100000000000000088';
    const int = makeEveryoneInteraction({
      // Sender absent; one other non-bot present.
      guildMembers: { [otherUser]: {} },
      memberCount: 3,  // sender + otherUser + the degraded row below
    });
    // Inject a degraded row (no .user) into the warm cache.
    int.guild.members.cache.set('degraded-1', { /* no .user */ });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    // Sender (defensive push) + otherUser (cache) — degraded row
    // filtered.
    expect(payload.recipientIds.sort()).toEqual([SENDER_ID, otherUser].sort());
    expect(payload.selfIncluded).toBe(true);
    const logger = require('../src/logger');
    // partialCacheDrops debug log fires with `dropped: 1`.
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('partial-cache rows dropped'),
      expect.objectContaining({ dropped: 1 }),
    );
    // Success log carries the cache shape.
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringContaining('@everyone expansion succeeded'),
      expect.objectContaining({
        valid_count: 2,
        partial_cache_drops: 1,
        cache_size: 2,  // otherUser + degraded-1 = 2 entries
        member_count: 3,
      }),
    );
  });

  test('sender row missing + bots-only cache → still rejects (defensive push gated)', async () => {
    // Counter-test: the defensive push should NOT fire on a bots-only
    // cache. Silently expanding @everyone to "send to just me" would
    // misrepresent the user's all-or-nothing intent. The gate on
    // `hasOtherNonBotInCache` preserves the bots-only reject path.
    const bot2 = '100000000000000098';
    const int = makeEveryoneInteraction({
      // Sender ABSENT; only bots in cache.
      guildMembers: { [bot1]: { bot: true }, [bot2]: { bot: true } },
      memberCount: 3,  // sender + 2 bots
    });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/No usable recipients|bot/i);
  });

  test('forged interaction with no guild → reject without crash + permission warning', async () => {
    // Forged HTTP interaction crafted with the @everyone custom_id but
    // no guild (DM context). canMentionEveryone derives `false` via
    // `!!interaction.guild`, so the perm re-check fires and surfaces
    // the rejection. The reject path re-renders via
    // renderConfirmCardRows; the @everyone block there dereferences
    // `interaction.memberPermissions?.has?.(...)` and must not crash
    // on the null-guild interaction.
    const int = makeEveryoneInteraction({ guildId: null });  // → guild = null
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Mention Everyone/);
  });

  test('success-path emits info-level audit log with counts', async () => {
    // Successful @everyone expansion is a load-bearing audit signal —
    // "did someone fan out to N users?" should be findable in logs
    // without a DDB scan. Pin the structured-log shape so a future
    // refactor doesn't silently drop the breadcrumb.
    const int = makeEveryoneInteraction();
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    const logger = require('../src/logger');
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringContaining('@everyone expansion succeeded'),
      expect.objectContaining({
        flow_id: 'fid',
        valid_count: expect.any(Number),
        dropped_bots: expect.any(Number),
        partial_cache_drops: expect.any(Number),
        self_included: true,
      }),
    );
  });

  test('partial-cache rows (member without .user) → counted in debug log + filtered from selection', async () => {
    // Mirror handleConfirmVoiceEveryone's partialCacheDrops telemetry.
    // Degraded GuildMembersChunk shapes occasionally land entries with
    // no .user — they're filtered silently, but the count is logged so
    // a future "@everyone underresolved" report has a forensic hook.
    const int = makeEveryoneInteraction();
    // Inject one degraded row alongside the valid ones.
    int.guild.members.cache.set('degraded-1', { /* no .user */ });
    int.guild.memberCount = 5;
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    const logger = require('../src/logger');
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('partial-cache rows dropped'),
      expect.objectContaining({ dropped: 1 }),
    );
    // Send still proceeds with the valid subset.
    expect(mockTransitionFlow).toHaveBeenCalled();
  });

  test('mid-deploy forward: legacy picker-mode row with pre-filled recipientIds → click @everyone → lands in EVERYONE mode', async () => {
    // Forward-direction mid-deploy contract. A pre-PR bot would have
    // auto-filled the picker after an @everyone click and written
    // `recipientMode: 'picker'` with populated `recipientIds`. When a
    // post-PR bot processes that row and the user clicks 📢 @everyone
    // again, the transition MUST overwrite to `'everyone'` so the
    // re-render hides the picker (rather than spreading the legacy
    // 'picker' mode forward). Without this contract, the user would
    // see the auto-fill leak persist post-deploy.
    const int = makeEveryoneInteraction();
    // Legacy row shape: picker-mode + a pre-filled subset (the old
    // auto-fill behavior would have written a truncated set here).
    const legacyPayload = {
      ...initialPayload,
      recipientMode: 'picker',
      recipientIds: [u1],  // legacy picker-mode auto-fill subset
      recipientAliases: { [u1]: 'alice' },
    };
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: legacyPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    // Mode overwrites — not spread-leaked from the legacy row.
    expect(payload.recipientMode).toBe('everyone');
    // Recipient set is fresh from cache iteration, not the legacy
    // truncated subset.
    expect(payload.recipientIds.sort()).toEqual([u1, u2, SENDER_ID].sort());
  });
});

// ──────────────────────────────────────────────────────────────
// handleConfirmExpirySelect — inline expiry edit on confirm card
// ──────────────────────────────────────────────────────────────

describe('handleConfirmExpirySelect', () => {
  const u1 = '100000000000000001';
  const basePayload = {
    resourceType: 'file',
    resourceLabel: 'x.png',
    recipientIds: [u1],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
  };

  function makeSelectInteraction({ value = '7d', ...rest } = {}) {
    const int = makeInteraction(rest);
    int.values = [value];
    return int;
  }

  test('happy path persists new expiresIn + re-renders', async () => {
    const int = makeSelectInteraction({ value: '7d' });
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: expect.objectContaining({ expiresIn: '7d', recipientIds: [u1] }),
      terminal: false,
      set_expires_at: expect.any(Number),
    }));
    expect(int.editReply).toHaveBeenCalled();
    // Pin the user-facing render — verifies the new value reaches the
    // confirm-card content, not just DDB. A refactor landing the right
    // value in flow state but rendering the old label would slip past
    // the wire-protocol assertion above.
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    expect(lastEdit.content).toMatch(/7 days/);
  });

  test('selfIncluded notice survives an expiry change re-render', async () => {
    // Non-recipient menu transitions (expiry, self-destruct, note
    // modal) must keep the "Send includes you." notice when the
    // payload was minted with selfIncluded=true. `rerenderConfirmCard`
    // reads `newPayload.selfIncluded === true`, so an expiry change
    // that drops the field by accident would surface here as the
    // notice disappearing on a non-recipient menu click.
    const int = makeSelectInteraction({ value: '7d' });
    const payloadWithSelf = { ...basePayload, selfIncluded: true };
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload: payloadWithSelf, version: 1 } });
    // Persisted in the new payload (transitionFlow spreads ...payload).
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({ selfIncluded: true, expiresIn: '7d' }),
    }));
    // Re-rendered content still shows the notice.
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    expect(lastEdit.content).toMatch(/Send includes you/);
  });

  test('no-op re-pick (same value as payload) → skip transitionFlow + version bump, still re-render', async () => {
    // Pin the no-op optimization: re-picking the current value
    // doesn't fence concurrent sibling interactions via a needless
    // version bump. Visible feedback (rerender) still fires so the
    // user knows their click registered.
    const int = makeSelectInteraction({ value: '24h' });  // same as basePayload.expiresIn
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalled();
  });

  test('forged off-set expiry value → reply warn BEFORE defer, NO transitionFlow', async () => {
    // Defense-in-depth: Discord enforces the choice set, but a forged
    // interaction could land an arbitrary string. Validate BEFORE
    // deferUpdate so the forgery branch uses the cheaper single-call
    // `reply` ack instead of `followUp` after a wasted defer.
    const int = makeSelectInteraction({ value: '999d' });
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.deferUpdate).not.toHaveBeenCalled();
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Unrecognized expiry/i),
      ephemeral: true,
    }));
  });

  test('conflict result → superseded copy', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    const int = makeSelectInteraction({ value: '7d' });
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/superseded/i),
      components: [],
    }));
  });

  test('not_found result → expired copy', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    const int = makeSelectInteraction({ value: '7d' });
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/expired/i),
      components: [],
    }));
  });

  test('transitionFlow throw → ephemeral retry followUp, no superseded copy', async () => {
    mockTransitionFlow.mockRejectedValueOnce(new Error('DDB blip'));
    const int = makeSelectInteraction({ value: '7d' });
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not save/i),
      ephemeral: true,
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/superseded/i),
    }));
  });

  test('preserves all other payload fields across transition + warns on corrupted selfDestructSeconds', async () => {
    // `selfDestructSeconds: 60` is OFF-PRESET (presets are
    // [0.5, 1, 5, 30, 300, 1800, 3600]). The rerender path's
    // renderConfirmCardRows logs a warn on this case (cr round-13)
    // — pin that the expiry-handler entry trips the same forensic
    // surface, not just the rerenderConfirmCard cache-miss tests.
    const logger = require('../src/logger');
    logger.warn.mockClear();
    const int = makeSelectInteraction({ value: '7d' });
    const payload = {
      ...basePayload,
      selfDestructSeconds: 60,
      personalMessage: 'hi',
      // Explicitly include personalMessageRaw to pin the round-trip
      // invariant: a regression that destructured `personalMessage`
      // (without `Raw`) into newPayload would silently drop it and
      // break the next Edit-note pre-fill. The spread covers it
      // today; this assertion locks the contract.
      personalMessageRaw: 'hi',
    };
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({
        expiresIn: '7d',
        recipientIds: [u1],
        selfDestructSeconds: 60,
        personalMessage: 'hi',
        personalMessageRaw: 'hi',
        resourceType: 'file',
      }),
    }));
    // Corruption warn fired from the rerender pass.
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringMatching(/off-preset selfDestructSeconds/i),
      expect.objectContaining({ selfDestructSeconds: '60' })
    );
  });
});

// ──────────────────────────────────────────────────────────────
// handleConfirmSelfDestructSelect — inline self-destruct edit
// ──────────────────────────────────────────────────────────────

describe('handleConfirmSelfDestructSelect', () => {
  const u1 = '100000000000000001';
  const basePayload = {
    resourceType: 'file',
    resourceLabel: 'x.png',
    recipientIds: [u1],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
  };

  function makeSelectInteraction({ value = '60', ...rest } = {}) {
    const int = makeInteraction(rest);
    int.values = [value];
    return int;
  }

  test('happy path persists new selfDestructSeconds + re-renders', async () => {
    // Use 30 — SELF_DESTRUCT_PRESETS only contains [0.5, 1, 5, 30, 300, 1800, 3600].
    // selfDestructSelectValueToSeconds rejects (returns null) anything off-preset
    // so the test value must match an existing preset for this assertion.
    const int = makeSelectInteraction({ value: '30' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: expect.objectContaining({ selfDestructSeconds: 30 }),
      terminal: false,
    }));
    expect(int.editReply).toHaveBeenCalled();
    // Pin the user-facing render — wire-protocol assertion above proves
    // we persisted the new value; this proves the rerendered content
    // shows it back to the user.
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    expect(lastEdit.content).toMatch(/30 seconds/);
  });

  test('no-op re-pick (same selfDestructSeconds as payload) → skip transitionFlow + version bump, still re-render', async () => {
    // Symmetric with the expiry handler's no-op test. `no-timer`
    // sentinel maps to null; basePayload.selfDestructSeconds is null
    // so this is a no-op.
    const int = makeSelectInteraction({ value: 'no-timer' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalled();
  });

  test('"no-timer" form-side sentinel → null (when changing FROM a previous timer)', async () => {
    // Form value-space uses SELF_DESTRUCT_NO_TIMER_VALUE (distinct
    // from the slash-option side's 'none'). The helper maps it to
    // null. Pin so a refactor that conflates the two value-spaces
    // fails this test. Use a non-null current value so the no-op
    // short-circuit doesn't skip the write (separate test pins the
    // no-op path).
    const payloadWithTimer = { ...basePayload, selfDestructSeconds: 30 };
    const int = makeSelectInteraction({ value: 'no-timer' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: payloadWithTimer, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({ selfDestructSeconds: null }),
    }));
  });

  test('unknown forged value → reply warn BEFORE defer, NO transitionFlow', async () => {
    // Symmetric with the expiry handler. The previous round had a
    // silent-fallback path that mapped forged values to null (no
    // timer) — that would silently clear a user's previously-set
    // timer on every forgery probe. Reject + warn + no save matches
    // the realistic threat model: Discord enforces the option set
    // server-side, so an off-set value here is forgery, not a
    // legitimate UI bug.
    const logger = require('../src/logger');
    logger.warn.mockClear();
    const int = makeSelectInteraction({ value: '999999' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.deferUpdate).not.toHaveBeenCalled();
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Unrecognized self-destruct/i),
      ephemeral: true,
    }));
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringMatching(/forged off-set self-destruct/i),
      expect.objectContaining({ flow_id: 'fid', value: '999999' })
    );
  });

  test('legitimate "no-timer" value does NOT trigger forgery warn', async () => {
    // The forgery-warn must not false-positive on the no-timer
    // sentinel (which also maps to null via the helper but is the
    // legitimate way to clear a timer).
    const logger = require('../src/logger');
    logger.warn.mockClear();
    const int = makeSelectInteraction({ value: 'no-timer' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(logger.warn).not.toHaveBeenCalled();
  });

  test('conflict → superseded copy', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    // '30' is in SELF_DESTRUCT_PRESETS. Forged values now reject
    // BEFORE deferUpdate, so we need a legitimate preset to reach
    // the transitionFlow → result-handling branches.
    const int = makeSelectInteraction({ value: '30' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/superseded/i),
    }));
  });

  test('not_found → expired copy', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    // '30' is in SELF_DESTRUCT_PRESETS. Forged values now reject
    // BEFORE deferUpdate, so we need a legitimate preset to reach
    // the transitionFlow → result-handling branches.
    const int = makeSelectInteraction({ value: '30' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/expired/i),
    }));
  });
});

// ──────────────────────────────────────────────────────────────
// handleConfirmNoteButton — opens modal, no flow mutation
// ──────────────────────────────────────────────────────────────

describe('handleConfirmNoteButton', () => {
  const basePayload = {
    resourceType: 'file',
    resourceLabel: 'x.png',
    recipientIds: [],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
  };

  function makeButtonInteraction(rest = {}) {
    const int = makeInteraction(rest);
    int.showModal = jest.fn().mockResolvedValue(undefined);
    return int;
  }

  test('opens modal — does NOT mutate flow state (no transitionFlow / deleteFlow)', async () => {
    const int = makeButtonInteraction();
    await handleConfirmNoteButton(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.showModal).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(mockDeleteFlow).not.toHaveBeenCalled();
  });

  test('modal pre-filled with RAW input (not sanitized) for round-trip safety', async () => {
    // The discord.js component mocks don't preserve state, so we
    // inspect builder method invocations to verify the pre-fill.
    // Each TextInputBuilder instance has its own jest.fn-based
    // setValue; the most-recent call captures what the modal passes
    // to the TextInput.
    //
    // CRITICAL: pre-fill uses payload.personalMessageRaw (the raw
    // user input), NOT payload.personalMessage (the sanitized form).
    // Without this distinction, pre-filling with `\*\*bold\*\*` and
    // resubmitting unchanged would re-sanitize → `\\\*\\\*bold\\\*\\\*`
    // (double-escape), and every Edit cycle would escalate.
    const { TextInputBuilder } = require('discord.js');
    TextInputBuilder.mockClear();
    const int = makeButtonInteraction();
    const payload = {
      ...basePayload,
      personalMessage: '\\*\\*bold\\*\\*',  // sanitized form (would render literally)
      personalMessageRaw: '**bold**',         // what the user actually typed
    };
    await handleConfirmNoteButton(int, { flow_id: 'fid', row: { payload, version: 1 } });
    expect(int.showModal).toHaveBeenCalled();
    // setValue receives the RAW form so the user sees `**bold**` in
    // the input, not the escaped `\*\*bold\*\*`.
    const builder = TextInputBuilder.mock.results[0].value;
    expect(builder.setValue).toHaveBeenCalledWith('**bold**');
  });

  test('modal pre-fills empty string when no personalMessage is set', async () => {
    const { TextInputBuilder } = require('discord.js');
    TextInputBuilder.mockClear();
    const int = makeButtonInteraction();
    await handleConfirmNoteButton(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const builder = TextInputBuilder.mock.results[0].value;
    expect(builder.setValue).toHaveBeenCalledWith('');
  });

  test('modal pre-fills empty for legacy flow rows missing personalMessageRaw', async () => {
    // Forward-compat: a flow row created before the personalMessageRaw
    // field was added has personalMessage but no Raw counterpart.
    // Falling back to empty string is the safe choice (user sees blank,
    // can re-type) — pre-filling from the sanitized personalMessage
    // would re-trigger the double-escape bug this field exists to fix.
    const { TextInputBuilder } = require('discord.js');
    TextInputBuilder.mockClear();
    const int = makeButtonInteraction();
    const legacyPayload = { ...basePayload, personalMessage: '\\*\\*bold\\*\\*' };
    await handleConfirmNoteButton(int, { flow_id: 'fid', row: { payload: legacyPayload, version: 1 } });
    const builder = TextInputBuilder.mock.results[0].value;
    expect(builder.setValue).toHaveBeenCalledWith('');
  });

  test('safe when clicked after recipients fully chosen (idempotent — no transitionFlow)', async () => {
    // The "Note button after picking everything" case from the plan:
    // safe-by-construction because this handler never touches flow
    // state — the user can repeatedly open + close the modal without
    // bumping the version or fencing out other interactions.
    const int = makeButtonInteraction();
    const payload = { ...basePayload, recipientIds: ['100000000000000001', '100000000000000002'] };
    await handleConfirmNoteButton(int, { flow_id: 'fid', row: { payload, version: 5 } });
    expect(int.showModal).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
  });

  test('showModal failure → ephemeral reply fallback (no silent "interaction failed" toast)', async () => {
    // showModal failure leaves the button-click unacknowledged. The
    // pre-fix behavior swallowed the error in .catch with only a
    // warn log, leaving the user with Discord's generic "interaction
    // failed" toast and no remediation. The fallback ack closes that
    // gap symmetrically with the menu handlers' error paths.
    const int = makeButtonInteraction();
    int.showModal.mockRejectedValueOnce(new Error('Discord 500'));
    await handleConfirmNoteButton(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not open the note editor/i),
      ephemeral: true,
    }));
    // Flow state must remain untouched — showModal failure is a
    // pure UX surface; we don't bump the version on it.
    expect(mockTransitionFlow).not.toHaveBeenCalled();
  });

  test('showModal failure → if fallback reply ALSO rejects, no unhandled rejection escapes', async () => {
    // Dual-500 case: showModal fails (Discord blip) AND the ephemeral
    // reply fallback fails (interaction already acked by some other
    // path, or another Discord blip). The .catch(logIgnoredDiscordErr)
    // on the fallback should absorb the rejection. Without that
    // safety net, an unhandled rejection escapes and could crash the
    // event loop or trigger node's `unhandledRejection` listeners.
    const int = makeButtonInteraction();
    int.showModal.mockRejectedValueOnce(new Error('Discord 500'));
    int.reply.mockRejectedValueOnce(new Error('Already acked'));
    let unhandled = null;
    const listener = (reason) => { unhandled = reason; };
    process.on('unhandledRejection', listener);
    try {
      await handleConfirmNoteButton(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
      // Microtask flush — any unhandled rejection from the catch
      // chain would have been queued by now.
      await new Promise((resolve) => setImmediate(resolve));
      expect(unhandled).toBeNull();
    } finally {
      process.off('unhandledRejection', listener);
    }
    expect(int.showModal).toHaveBeenCalled();
    expect(int.reply).toHaveBeenCalled();
  });
});

// ──────────────────────────────────────────────────────────────
// handleConfirmNoteModal — modal submit persists sanitized note
// ──────────────────────────────────────────────────────────────

describe('handleConfirmNoteModal', () => {
  const basePayload = {
    resourceType: 'file',
    resourceLabel: 'x.png',
    recipientIds: ['100000000000000001'],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
  };

  function makeModalInteraction({ inputValue = 'hello', ...rest } = {}) {
    const int = makeInteraction(rest);
    int.fields = { getTextInputValue: jest.fn(() => inputValue) };
    return int;
  }

  test('happy path: defers, trims/sanitizes, persists, editReply re-renders', async () => {
    const int = makeModalInteraction({ inputValue: '  **bold** message  ' });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    // deferUpdate guards the 3s ack deadline — without it, a slow
    // DDB conditional write could push past Discord's hard limit and
    // both update() and reply() would fail.
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({
        // sanitizeMessage runs markdown-escape — the `**` becomes `\\*\\*`
        // so the note renders as literal text in the recipient DM.
        personalMessage: expect.stringMatching(/\\\*\\\*bold\\\*\\\* message/),
        // Raw form retained for the next Edit cycle's pre-fill —
        // without this, opening Edit would show the escaped form
        // and a no-op resubmit would re-sanitize to double-escape.
        personalMessageRaw: '**bold** message',
      }),
    }));
    expect(int.editReply).toHaveBeenCalled();
    // Pin the user-facing render — note shows up in the rerendered
    // confirm card content, not just DDB.
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    expect(lastEdit.content).toMatch(/\\\*\\\*bold\\\*\\\* message/);
  });

  test('round-trip no-op: re-submit unchanged input → skip transitionFlow + version bump, still re-render', async () => {
    // Two invariants in one test:
    //  1. Round-trip is idempotent — re-sanitizing the same raw
    //     produces the same sanitized form (no double-escape).
    //  2. No-op submit short-circuits the DDB write + version bump
    //     (symmetric with expiry / self-destruct handlers). The
    //     idempotence is what MAKES the no-op short-circuit safe:
    //     sanitize semantics guarantee the derived forms match the
    //     stored forms, so the equality check fires correctly.
    // Simulates: user typed `**bold**`, clicked Edit (pre-fill shows
    // raw via the button test), submitted unchanged.
    const int = makeModalInteraction({ inputValue: '**bold**' });
    const existingPayload = {
      ...basePayload,
      personalMessage: '\\*\\*bold\\*\\*',
      personalMessageRaw: '**bold**',
    };
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: existingPayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    // No-op skip — no DDB write, no version bump, no sibling fence.
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    // Visible feedback fires.
    expect(int.editReply).toHaveBeenCalled();
  });

  // The clear-note tests use a payload with a PREVIOUS note so the
  // round-17 no-op short-circuit doesn't skip the write (it would
  // for an already-null personalMessage on an empty submit). Each
  // test exercises a different "stripped to empty" input shape.
  const payloadWithNote = { ...basePayload, personalMessage: 'old note', personalMessageRaw: 'old note' };

  test('empty input on a payload with an existing note → personalMessage: null (clear)', async () => {
    const int = makeModalInteraction({ inputValue: '' });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: payloadWithNote, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({ personalMessage: null, personalMessageRaw: null }),
    }));
  });

  test('whitespace-only input on a payload with an existing note → personalMessage: null', async () => {
    const int = makeModalInteraction({ inputValue: '   \n  \t' });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: payloadWithNote, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({ personalMessage: null, personalMessageRaw: null }),
    }));
  });

  test('ZWSP-only input on a payload with an existing note → both fields null in lockstep', async () => {
    // The trim-vs-sanitize gap: zero-width space (U+200B) isn't in
    // ECMAScript's WhiteSpace set, so .trim() leaves it. But
    // sanitizeMessage's bidi/zero-width strip removes it, producing
    // an empty personalMessage. Without the "null out raw if
    // sanitize stripped to empty" fix, personalMessageRaw would
    // retain the invisible char — the button would label as "Add a
    // note" (empty personalMessage is falsy) while the modal pre-
    // fills invisible chars on the next Edit. Both fields must go
    // to null in lockstep.
    const zwsp = String.fromCharCode(0x200B);
    const int = makeModalInteraction({ inputValue: zwsp.repeat(3) });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: payloadWithNote, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({
        personalMessage: null,
        personalMessageRaw: null,
      }),
    }));
  });

  test('empty submit on a payload with NO existing note → no-op skip (already cleared)', async () => {
    // Symmetric with the menu no-op tests. basePayload.personalMessage
    // is null; submitting empty produces null → no actual change →
    // skip the write + version bump.
    const int = makeModalInteraction({ inputValue: '' });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalled();
  });

  test('conflict → superseded copy via editReply (post-deferUpdate)', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    const int = makeModalInteraction({ inputValue: 'hi' });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/superseded/i),
      components: [],
    }));
  });

  test('not_found → expired copy via editReply', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    const int = makeModalInteraction({ inputValue: 'hi' });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/expired/i),
    }));
  });

  test('getTextInputValue throws → ephemeral followUp ("could not read"), NO transitionFlow, existing note preserved', async () => {
    // Customs-id allowlist drift: getTextInputValue throws for an
    // unknown field id. The previous defensive try/catch silently
    // routed to clear-note, dropping the user's existing note with no
    // feedback. New behavior: log + surface an ephemeral followUp
    // making the failure visible; existing note stays put (no
    // transitionFlow fired).
    const int = makeModalInteraction({ inputValue: 'hi' });
    int.fields.getTextInputValue = jest.fn(() => {
      throw new Error('Unknown custom_id');
    });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not read your note input/i),
      ephemeral: true,
    }));
    // Existing payload untouched — no clear-note silently applied.
    expect(mockTransitionFlow).not.toHaveBeenCalled();
  });

  test('transitionFlow throw → ephemeral followUp (NOT update/reply post-defer)', async () => {
    mockTransitionFlow.mockRejectedValueOnce(new Error('DDB blip'));
    const int = makeModalInteraction({ inputValue: 'hi' });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not save your note/i),
      ephemeral: true,
    }));
    // Negative assertion — once deferUpdate has fired, reply() and
    // update() would 409 (interaction already acked). The throw path
    // MUST surface the error via followUp, not either of those.
    expect(int.reply).not.toHaveBeenCalled();
    expect(int.update).not.toHaveBeenCalled();
  });

  test('sibling-mutation merge: row carries an expiry change made during typing → persisted alongside the note', async () => {
    // Lock in the merge semantics for the "user types in modal while
    // another desktop window changes expiry" race. The dispatcher
    // loads the freshest row at submit time, so row.payload carries
    // the sibling's mutation. `{ ...row.payload, personalMessage }`
    // must preserve that mutation — without it, the modal submit
    // would silently undo whatever the sibling set.
    //
    // Setup: row.payload has expiresIn: '7d' (the sibling's change)
    // and the original selfDestructSeconds. Modal submit's note must
    // land alongside both.
    const int = makeModalInteraction({ inputValue: 'hello' });
    const rowAfterSiblingMenu = {
      ...basePayload,
      expiresIn: '7d',          // sibling-changed
      selfDestructSeconds: 30,  // sibling-changed
      version: 2,                // version bumped by the sibling
    };
    await handleConfirmNoteModal(int, {
      flow_id: 'fid',
      row: { payload: rowAfterSiblingMenu, version: 2 },
    });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 2, expect.objectContaining({
      payload: expect.objectContaining({
        personalMessage: expect.stringMatching(/hello/),
        personalMessageRaw: 'hello',
        // Sibling's changes must survive the merge.
        expiresIn: '7d',
        selfDestructSeconds: 30,
      }),
    }));
  });
});

// ──────────────────────────────────────────────────────────────
// rerenderConfirmCard cache-miss fallback (verified end-to-end via
// the expiry handler — rerenderConfirmCard isn't exported)
// ──────────────────────────────────────────────────────────────

describe('rerenderConfirmCard cache-miss recipient fallback', () => {
  const u1 = '100000000000000001';
  const persistedAlias = 'Alice (display)';

  function makeSelectInteraction({ value = '7d', ...rest } = {}) {
    const int = makeInteraction(rest);
    int.values = [value];
    return int;
  }

  test('renders persisted alias when member-cache is empty', async () => {
    // The bug this guards: a member-cache miss between pick and menu
    // change would render the raw 18-digit snowflake. Persisting
    // resolvedAliases at pick-time + reading them in rerenderConfirmCard
    // means the user always sees a name (cached when available, the
    // persisted alias otherwise).
    const payload = {
      resourceType: 'file',
      resourceLabel: 'x.png',
      recipientIds: [u1],
      recipientAliases: { [u1]: persistedAlias },
      expiresIn: '24h',
      selfDestructSeconds: null,
      personalMessage: null,
    };
    // No guildMembers — cache miss. The fallback chain should hit
    // payload.recipientAliases.
    const int = makeSelectInteraction({ value: '7d', guildMembers: {} });
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload, version: 1 } });
    expect(int.editReply).toHaveBeenCalled();
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    // Alias renders (markdown chars not present in this alias, so the
    // escape pass is a no-op). The raw snowflake MUST NOT appear in the
    // recipient preview line.
    expect(lastEdit.content).toMatch(/Alice/);
    expect(lastEdit.content).not.toMatch(new RegExp(u1));
  });

  test('warningsBlock from payload carries across menu interactions', async () => {
    // The other half of the cr finding: when slash entry records
    // "Skipped bots: 1" warnings, changing expiry shouldn't drop
    // them. Verifies the payload-persisted warningsBlock flows
    // through rerenderConfirmCard.
    const payload = {
      resourceType: 'file',
      resourceLabel: 'x.png',
      recipientIds: [u1],
      recipientAliases: { [u1]: 'Alice' },
      expiresIn: '24h',
      selfDestructSeconds: null,
      personalMessage: null,
      warningsBlock: '⚠️ Skipped bots: 1\n\n',
    };
    const int = makeSelectInteraction({ value: '7d', guildMembers: { [u1]: {} } });
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload, version: 1 } });
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    expect(lastEdit.content).toMatch(/Skipped bots/);
  });

  test('off-EXPIRY_LABELS expiresIn in payload → renderer defaults the 24h option (no first-option misrepresentation)', async () => {
    // Defense-in-depth: a corrupted DDB row carrying expiresIn outside
    // EXPIRY_LABELS (e.g. left over from a deprecated value, or written
    // by a misbehaving migration) would leave every option un-defaulted
    // in the StringSelectMenu — Discord then renders the first option's
    // label in the collapsed header, MISREPRESENTING the actual stored
    // value to the user. The renderer falls back to defaulting '24h'
    // in that case so the card still shows SOMETHING meaningful AND
    // matches the codebase default.
    const { StringSelectMenuBuilder } = require('discord.js');
    StringSelectMenuBuilder.mockClear();
    const payload = {
      resourceType: 'file',
      resourceLabel: 'x.png',
      recipientIds: ['100000000000000001'],
      recipientAliases: { '100000000000000001': 'Alice' },
      expiresIn: '999d',  // corrupted: not in EXPIRY_LABELS
      selfDestructSeconds: null,
      personalMessage: null,
    };
    // Use the self-destruct handler to trigger a rerender WITHOUT
    // overwriting the corrupted expiresIn (the expiry handler would
    // replace it with the user's pick, masking the bug). Self-destruct
    // touches only selfDestructSeconds, so the rerender carries the
    // corrupted expiresIn unchanged through to the renderer.
    const int = makeInteraction({ guildMembers: { '100000000000000001': {} } });
    int.values = ['30'];  // legit self-destruct preset
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload, version: 1 } });
    // The rerender pass calls StringSelectMenuBuilder twice (self-
    // destruct row + expiry row). Inspect the addOptions calls and
    // verify the EXPIRY select has exactly one default-true option,
    // and that option's value is '24h'.
    const expirySelectCalls = StringSelectMenuBuilder.mock.results
      .filter((r) => {
        const calls = r.value.setCustomId.mock.calls;
        return calls.length && calls[0][0] === 'qurl_confirm_expiry';
      });
    expect(expirySelectCalls.length).toBeGreaterThan(0);
    const expiryAddOptionsArgs = expirySelectCalls[expirySelectCalls.length - 1]
      .value.addOptions.mock.calls[0];
    const defaultedExpiryOptions = expiryAddOptionsArgs.filter((o) => o.default);
    expect(defaultedExpiryOptions).toHaveLength(1);
    expect(defaultedExpiryOptions[0].value).toBe('24h');
  });

  test('voice-mode + empty recipientIds → "0 users in #voice" + Send disabled (no auto-revert to picker)', async () => {
    // Reachable when a voice channel empties out between renders (every
    // non-sender member leaves voice). Voice-mode is sticky: rather
    // than silently snapping back to picker-mode mid-flow, the card
    // shows the honest empty state and the user clicks "Pick people
    // instead" to recover. Send stays disabled (recipientIds=[]) so a
    // wayward click can't fan out to zero recipients.
    const { ButtonBuilder } = require('discord.js');
    ButtonBuilder.mockClear();
    const payload = {
      resourceType: 'file',
      resourceLabel: 'x.png',
      recipientIds: [],
      recipientAliases: {},
      recipientMode: 'voice',
      voiceChannelId: 'voice-empty',
      expiresIn: '24h',
      selfDestructSeconds: null,
      personalMessage: null,
    };
    const int = makeInteraction();
    int.values = ['7d'];
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload, version: 1 } });
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    // Voice-mode rendering still applies — content shows the channel
    // mention even with zero recipients.
    expect(lastEdit.content).toMatch(/0 users in <#voice-empty>/);
    // The "(you not included)" disclosure used to ride along here;
    // dropped per UX call. Sender exclusion stays inferred.
    expect(lastEdit.content).not.toMatch(/you not included/);
    // The picker prompt MUST NOT appear (we're in voice-mode).
    expect(lastEdit.content).not.toMatch(/Pick recipients below/);
    // Send is disabled (no recipients), pick-manual is rendered as
    // the recovery path.
    const sendBtn = ButtonBuilder.mock.results.find(
      (r) => r.value.setCustomId.mock.calls[0]?.[0] === 'qurl_confirm_send'
    );
    expect(sendBtn.value.setDisabled).toHaveBeenCalledWith(true);
    const customIds = ButtonBuilder.mock.results.map(
      (r) => r.value.setCustomId.mock.calls[0]?.[0]
    );
    expect(customIds).toContain('qurl_confirm_pick_manual');
  });
});

// ──────────────────────────────────────────────────────────────
// renderConfirmCardRows — row layout + label flips
// ──────────────────────────────────────────────────────────────

describe('renderConfirmCardRows', () => {
  // The renderer lives at module scope but isn't exported directly.
  // We assert via a behavioral round-trip through handleQurlSlashSend
  // or by inspecting what renderConfirmCardRows would attach (via the
  // editReply calls). Simpler: drive via the entry path and inspect.

  test('slash-entry WITHOUT recipients → 4 rows, Send disabled, expiry/self-destruct/note interactable', async () => {
    // Renderer always produces 4 rows (picker + self-destruct +
    // expiry + bottom button row). On a no-recipients initial render,
    // Send must be DISABLED (no recipients to send to) while the
    // expiry/self-destruct/note selects+button stay INTERACTABLE so
    // the user can pre-configure their send before picking
    // recipients. Inspect the ButtonBuilder mock for the Send button's
    // setDisabled call.
    const { ButtonBuilder } = require('discord.js');
    ButtonBuilder.mockClear();
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT },  // no `recipients` → needsPicker
    });
    await handleQurlSend(int);
    const editReplyCalls = int.editReply.mock.calls;
    const lastCall = editReplyCalls[editReplyCalls.length - 1][0];
    expect(lastCall.components).toHaveLength(4);
    // 3 ButtonBuilders: Note, Send, Cancel. The Send button is the
    // one with custom id 'qurl_confirm_send'. Find it and assert
    // setDisabled(true) was called.
    const sendBuilder = ButtonBuilder.mock.results.find(
      (r) => r.value.setCustomId.mock.calls[0]?.[0] === 'qurl_confirm_send'
    );
    expect(sendBuilder).toBeDefined();
    expect(sendBuilder.value.setDisabled).toHaveBeenCalledWith(true);
  });

  test('slash-entry WITH recipients → 4 rows (picker still attached so layout is stable across menu interactions)', async () => {
    // Round-4 cr surfaced: the picker was conditionally attached on
    // initial render (3 rows when recipients supplied) but
    // rerenderConfirmCard always rendered 4. The row count jumped
    // from 3 to 4 the first time the user clicked any menu, which
    // was a visible mid-flow layout shift.
    //
    // Round-13 cr removed the conditional entirely: renderer now
    // always produces 4 rows. The user sees the same layout from
    // frame 0 through every menu interaction. Matches
    // handleConfirmUserSelect's post-pick contract — re-picks stay
    // possible after a successful pick.
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlSend(int);
    const editReplyCalls = int.editReply.mock.calls;
    const lastCall = editReplyCalls[editReplyCalls.length - 1][0];
    expect(lastCall.components).toHaveLength(4);
  });

  test('button row carries Note + Send + Cancel in that order (3 buttons, identifiable customIds)', async () => {
    // The discord.js mock's ButtonBuilder doesn't preserve state, but
    // each instance's `setCustomId` jest.fn captures the call. The
    // three ButtonBuilder instances created during this render are
    // the Note, Send, and Cancel buttons — in that left-to-right
    // construction order, matching the bottom row layout.
    const { ButtonBuilder } = require('discord.js');
    ButtonBuilder.mockClear();
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlSend(int);
    // 3 buttons constructed for the bottom row of the confirm card.
    expect(ButtonBuilder).toHaveBeenCalledTimes(3);
    const customIds = ButtonBuilder.mock.results.map(
      (r) => r.value.setCustomId.mock.calls[0][0]
    );
    expect(customIds).toEqual([
      'qurl_confirm_note_btn',
      'qurl_confirm_send',
      'qurl_confirm_cancel',
    ]);
  });

  test('Note button label is "Add a note (optional)" when no personal-message set', async () => {
    const { ButtonBuilder } = require('discord.js');
    ButtonBuilder.mockClear();
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlSend(int);
    const noteBtn = ButtonBuilder.mock.results[0].value;
    expect(noteBtn.setLabel).toHaveBeenCalledWith(expect.stringMatching(/Add a note/));
  });

  test('Note button label is "Edit note" when personal-message IS set', async () => {
    // Split from the "Add a note" test because both share `sendCooldowns`
    // module state via `setCooldown`; running both in the same test
    // would put the second invocation on cooldown and short-circuit
    // before renderConfirmCardRows. beforeEach calls sendCooldowns.clear()
    // so each test starts fresh.
    const { ButtonBuilder } = require('discord.js');
    ButtonBuilder.mockClear();
    const int = makeInteraction({
      options: {
        attachment: VALID_ATTACHMENT,
        recipients: '<@100000000000000001>',
        'personal-message': 'hello',
      },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlSend(int);
    const noteBtn = ButtonBuilder.mock.results[0].value;
    expect(noteBtn.setLabel).toHaveBeenCalledWith(expect.stringMatching(/Edit note/));
  });

  test('slash-entry WITH recipients → picker pre-checks the text-resolved ids via addDefaultUsers', async () => {
    // Power-user bug: typing `recipients:@a @b` then opening the picker
    // showed an empty dropdown — the originally-selected users were not
    // pre-checked. Fix routes the resolved recipientIds through
    // renderConfirmCardRows → MentionableSelectMenuBuilder.addDefaultUsers
    // so Discord pre-checks them on dropdown open. The send still
    // proceeds with the same set; this is purely a re-open UX fix.
    // Roles in text get expanded to users by parseRecipientMentions, so
    // we never pre-check roles here (addDefaultRoles would be wrong).
    const { MentionableSelectMenuBuilder } = require('discord.js');
    MentionableSelectMenuBuilder.mockClear();
    const int = makeInteraction({
      options: {
        attachment: VALID_ATTACHMENT,
        recipients: '<@100000000000000001> <@100000000000000002>',
      },
      guildMembers: {
        '100000000000000001': {},
        '100000000000000002': {},
      },
    });
    await handleQurlSend(int);
    expect(MentionableSelectMenuBuilder).toHaveBeenCalledTimes(1);
    const builder = MentionableSelectMenuBuilder.mock.results[0].value;
    expect(builder.addDefaultUsers).toHaveBeenCalledWith(
      '100000000000000001',
      '100000000000000002',
    );
  });

  test('slash-entry WITHOUT recipients → picker does NOT call addDefaultUsers', async () => {
    // When there are no resolved recipients (needsPicker:true), the
    // renderer must skip the addDefaultUsers call entirely — Discord
    // rejects a select menu where default_values is empty-but-present,
    // and the picker stays empty by design for first-pick UX.
    const { MentionableSelectMenuBuilder } = require('discord.js');
    MentionableSelectMenuBuilder.mockClear();
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT },  // no recipients → needsPicker
    });
    await handleQurlSend(int);
    expect(MentionableSelectMenuBuilder).toHaveBeenCalledTimes(1);
    const builder = MentionableSelectMenuBuilder.mock.results[0].value;
    expect(builder.addDefaultUsers).not.toHaveBeenCalled();
  });

  test('slash-entry with pre-resolved recipients pre-checks all defaults via addDefaultUsers', async () => {
    // Picker always opens at the full per-pick cap (25) regardless of
    // defaults.length — the prior fit-to-defaults behavior is gone with
    // the widen branch removal. All 12 pre-resolved defaults stay
    // pre-checked because addDefaultUsers honors any list ≤25.
    const { MentionableSelectMenuBuilder } = require('discord.js');
    MentionableSelectMenuBuilder.mockClear();
    const ids = Array.from({ length: 12 }, (_, i) => `1000000000000000${String(i + 10)}`);
    const mentionList = ids.map((id) => `<@${id}>`).join(' ');
    const guildMembers = Object.fromEntries(ids.map((id) => [id, {}]));
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: mentionList },
      guildMembers,
    });
    await handleQurlSend(int);
    const builder = MentionableSelectMenuBuilder.mock.results[0].value;
    expect(builder.setMaxValues).toHaveBeenCalledWith(25);
    expect(builder.addDefaultUsers).toHaveBeenCalledWith(...ids);
  });

  test('renderConfirmCardRows pluralizes the placeholder text correctly when QURL_SEND_MAX_RECIPIENTS clamps to 1', async () => {
    // An operator dialing the per-tenant cap to 1 (allowed by the
    // minPositive validator) would otherwise see "Pick up to 1
    // users/roles" — wrong English. Singular branch keeps the
    // placeholder grammatical at the only QSMR value where it matters.
    const config = require('../src/config');
    const origCap = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 1;
    try {
      const { MentionableSelectMenuBuilder } = require('discord.js');
      MentionableSelectMenuBuilder.mockClear();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT },
      });
      await handleQurlSend(int);
      const builder = MentionableSelectMenuBuilder.mock.results[0].value;
      expect(builder.setMaxValues).toHaveBeenCalledWith(1);
      expect(builder.setPlaceholder).toHaveBeenCalledWith('Pick up to 1 user/role');
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = origCap;
    }
  });

  test('renderConfirmCardRows clamps maxValues to QURL_SEND_MAX_RECIPIENTS when env override is tighter than the per-pick cap', async () => {
    // With a tenant-configured QURL_SEND_MAX_RECIPIENTS below 25, the
    // three-way Math.min in renderConfirmCardRows must clamp the picker
    // down so the placeholder and setMaxValues both reflect the system
    // cap. Without this test the clamp could silently rot — the other
    // tests run with mocked QSMR=25, where min(25,25,25)=25 makes the
    // QSMR leg invisible.
    const config = require('../src/config');
    const origCap = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 15;
    try {
      const { MentionableSelectMenuBuilder } = require('discord.js');
      MentionableSelectMenuBuilder.mockClear();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT },  // no recipients → needsPicker
      });
      await handleQurlSend(int);
      const builder = MentionableSelectMenuBuilder.mock.results[0].value;
      expect(builder.setMaxValues).toHaveBeenCalledWith(15);
      expect(builder.setPlaceholder).toHaveBeenCalledWith('Pick up to 15 users/roles');
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = origCap;
    }
  });

  test('pre-resolved defaults beyond the per-pick cap truncate to the first 25 via addDefaultUsers, but the full set persists in payload.recipientIds', async () => {
    // The slice on `defaults.slice(0, maxValues)` enforces Discord's
    // default_values.length ≤ max_values invariant when text-resolved
    // recipientIds overflow the picker's 25-slot cap. Overflow ids stay
    // in payload.recipientIds and still reach the send — pin both halves
    // so a future refactor can't silently drop the overflow. Mock QSMR=30
    // to let parseRecipientMentions surface all 30 ids; without the mock
    // parse would cap at the default 25 and there'd be nothing to slice.
    const config = require('../src/config');
    const origCap = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 30;
    try {
      const { MentionableSelectMenuBuilder } = require('discord.js');
      MentionableSelectMenuBuilder.mockClear();
      const ids = Array.from({ length: 30 }, (_, i) => `1000000000000000${String(i + 10).padStart(2, '0')}`);
      const mentionList = ids.map((id) => `<@${id}>`).join(' ');
      const guildMembers = Object.fromEntries(ids.map((id) => [id, {}]));
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: mentionList },
        guildMembers,
      });
      await handleQurlSend(int);
      const builder = MentionableSelectMenuBuilder.mock.results[0].value;
      expect(builder.setMaxValues).toHaveBeenCalledWith(25);
      expect(builder.addDefaultUsers).toHaveBeenCalledWith(...ids.slice(0, 25));
      const persistedPayload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(persistedPayload.recipientIds.sort()).toEqual([...ids].sort());
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = origCap;
    }
  });

  test('voice button label is the fixed "Everyone on voice" form, independent of channel name', async () => {
    // The label used to interpolate `#{channelName}` (with UTF-16-
    // aware truncation against Discord's 80-unit button-label cap),
    // and also carried a live `(N)` count. It's now a fixed string
    // with no count and no channel name. Pin the new shape so a
    // future refactor that re-introduces either has to update this
    // test deliberately (and reconsider the markdown-injection surface
    // area that came with channel-name interpolation).
    const { ButtonBuilder } = require('discord.js');
    ButtonBuilder.mockClear();
    // An adversarial channel name that previously had to be UTF-16-
    // truncated. Now it should not appear in the label at all.
    const adversarialName = '🎉'.repeat(46) + '**bold**<@123>';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000099>' },
      guildMembers: { '100000000000000099': {} },
    });
    int.channel = { id: 'voice-fixed', type: 2 };
    int.guild.channels.cache.set('voice-fixed', {
      id: 'voice-fixed', name: adversarialName, type: 2,
      members: new Map([['111', { user: { id: '111', bot: false } }]]),
    });
    await handleQurlSend(int);
    const voiceBtn = ButtonBuilder.mock.results.find(
      (r) => r.value.setCustomId.mock.calls[0]?.[0] === 'qurl_confirm_voice_everyone'
    );
    expect(voiceBtn).toBeDefined();
    const label = voiceBtn.value.setLabel.mock.calls[0][0];
    // Fixed-shape contract — no count, no channel name.
    expect(label).toBe('\u{1F50A} Everyone on voice');
    // Discord 80-UTF-16-unit cap stays trivially satisfied.
    expect(label.length).toBeLessThanOrEqual(80);
    // Channel name (including the markdown-injection payload) is not
    // interpolated.
    expect(label).not.toContain('🎉');
    expect(label).not.toContain('**');
    expect(label).not.toContain('<@');
    expect(label).not.toContain('#');
    expect(label).not.toContain('…');
    // No live count suffix anymore.
    expect(label).not.toMatch(/\(\d+\)/);
  });

  describe('voice-mode layout (recipientMode:"voice")', () => {
    // Round-trip the render layout via handleQurlSend invoked from a
    // voice channel WITHOUT `recipients:`. The slash-entry voice-mode
    // auto-default lands the card in voice-mode, which:
    //   - HIDES the MentionableSelect picker row
    //   - SHOWS a "👥 Pick people instead" button
    //   - DOES NOT show the "🔊 Everyone on voice" button (that's
    //     the entry point INTO voice mode; once you're in, it's gone)
    // Without these pins, a future refactor that re-enables the picker
    // in voice-mode (or drops the pick-manual button) passes every
    // other test in this file.

    const VOICE_CH = 'voice-ch-layout';
    const u1 = '100000000000000031';

    function setupVoice(int) {
      int.channel = { id: VOICE_CH, type: 2 };
      const member = { user: { id: u1, bot: false } };
      int.guild.members.cache.set(u1, member);
      int.guild.channels.cache.set(VOICE_CH, {
        id: VOICE_CH, type: 2, name: 'general',
        members: new Map([[u1, member]]),
      });
    }

    test('picker row is NOT rendered (MentionableSelectMenuBuilder never instantiated)', async () => {
      const { MentionableSelectMenuBuilder } = require('discord.js');
      MentionableSelectMenuBuilder.mockClear();
      const int = makeInteraction({ options: { attachment: VALID_ATTACHMENT } });
      setupVoice(int);
      await handleQurlSend(int);
      // The literal "one or the other" contract — voice-mode kills the
      // picker. A test that asserted on `editReply.components.length`
      // alone would silently drift if rows were added/removed elsewhere.
      expect(MentionableSelectMenuBuilder).not.toHaveBeenCalled();
    });

    test('"Pick people instead" button IS rendered; "Everyone on voice" button is NOT', async () => {
      const { ButtonBuilder } = require('discord.js');
      ButtonBuilder.mockClear();
      const int = makeInteraction({ options: { attachment: VALID_ATTACHMENT } });
      setupVoice(int);
      await handleQurlSend(int);
      const customIds = ButtonBuilder.mock.results.map(
        (r) => r.value.setCustomId.mock.calls[0]?.[0]
      );
      expect(customIds).toContain('qurl_confirm_pick_manual');
      expect(customIds).not.toContain('qurl_confirm_voice_everyone');
    });

    test('bottom row has exactly 4 buttons in order: Pick-manual, Note, Send, Cancel', async () => {
      // Layout pin. Voice-mode bottom row carries the escape hatch +
      // the standard note/send/cancel trio. The pick-manual button is
      // leftmost (replacing the voice-everyone affordance in picker
      // mode); any future refactor that re-orders these should update
      // this test deliberately.
      const { ButtonBuilder } = require('discord.js');
      ButtonBuilder.mockClear();
      const int = makeInteraction({ options: { attachment: VALID_ATTACHMENT } });
      setupVoice(int);
      await handleQurlSend(int);
      const customIds = ButtonBuilder.mock.results.map(
        (r) => r.value.setCustomId.mock.calls[0]?.[0]
      );
      expect(customIds).toEqual([
        'qurl_confirm_pick_manual',
        'qurl_confirm_note_btn',
        'qurl_confirm_send',
        'qurl_confirm_cancel',
      ]);
    });

    test('voice-mode "To:" line uses a native channel mention (not raw channel.name) — markdown-injection safe', async () => {
      // Regression pin: channel names like `**spoiler**` or `||hide||`
      // would otherwise inject markdown into the confirm card content.
      // Rendering via `<#channelId>` lets Discord resolve the mention
      // client-side without going through name interpolation.
      const int = makeInteraction({ options: { attachment: VALID_ATTACHMENT } });
      int.channel = { id: VOICE_CH, type: 2 };
      const member = { user: { id: u1, bot: false } };
      int.guild.members.cache.set(u1, member);
      int.guild.channels.cache.set(VOICE_CH, {
        id: VOICE_CH, type: 2,
        name: '**inject** _under_ ||hide||',
        members: new Map([[u1, member]]),
      });
      await handleQurlSend(int);
      const editReplyCalls = int.editReply.mock.calls;
      const lastCall = editReplyCalls[editReplyCalls.length - 1][0];
      expect(lastCall.content).toContain(`<#${VOICE_CH}>`);
      expect(lastCall.content).not.toContain('**inject**');
      expect(lastCall.content).not.toContain('||hide||');
    });

    test('voice-mode survives an unrelated menu interaction (expiry change) without decaying to picker', async () => {
      // Belt-and-braces: after the slash-entry voice-mode auto-default
      // commits, an expiry-change re-renders the card through
      // rerenderConfirmCard. If the rerender path defaulted to
      // picker-mode (instead of reading payload.recipientMode), the
      // card would silently snap layout mid-flow. The dedicated
      // voice-mode-with-empty-recipients test covers the degenerate
      // case; this pins the happy path round-trip.
      const { MentionableSelectMenuBuilder, ButtonBuilder } = require('discord.js');
      // Construct a post-slash-entry voice-mode payload (what
      // handleQurlSlashSend would have written to flow_state).
      const payload = {
        resourceType: 'file',
        resourceLabel: 'x.png',
        recipientIds: [u1],
        recipientAliases: { [u1]: 'alice' },
        recipientMode: 'voice',
        voiceChannelId: VOICE_CH,
        expiresIn: '24h',
        selfDestructSeconds: null,
        personalMessage: null,
        warningsBlock: '',
      };
      MentionableSelectMenuBuilder.mockClear();
      ButtonBuilder.mockClear();
      const int = makeInteraction({ guildMembers: { [u1]: {} } });
      int.values = ['7d'];  // change expiry from 24h → 7d
      await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload, version: 1 } });
      // Layout pin: picker still hidden, pick-manual still rendered,
      // voice-everyone still NOT rendered. The "To:" line still uses
      // the channel mention.
      expect(MentionableSelectMenuBuilder).not.toHaveBeenCalled();
      const customIds = ButtonBuilder.mock.results.map(
        (r) => r.value.setCustomId.mock.calls[0]?.[0]
      );
      expect(customIds).toContain('qurl_confirm_pick_manual');
      expect(customIds).not.toContain('qurl_confirm_voice_everyone');
      const lastCall = int.editReply.mock.calls.slice(-1)[0][0];
      expect(lastCall.content).toContain(`<#${VOICE_CH}>`);
      // "(you not included)" disclosure was dropped per UX call;
      // sender exclusion is inferred from voice-mode semantics.
      expect(lastCall.content).not.toMatch(/you not included/);
    });

    test('forged voice-mode without voiceChannelId still renders the escape hatch (defensive)', () => {
      // Production never pairs RECIPIENT_MODE_VOICE without voiceChannelId,
      // but a forged or schema-drifted payload could. Without the
      // defensive `voiceChannelId`-less branch in renderConfirmCardRows,
      // such a payload would skip the escape-hatch branch entirely AND
      // skip the @everyone entry branch (gated on PICKER), leaving the
      // user with no path back to manual selection. Pin recovery.
      const { MentionableSelectMenuBuilder, ButtonBuilder } = require('discord.js');
      MentionableSelectMenuBuilder.mockClear();
      ButtonBuilder.mockClear();
      const interaction = {
        guild: {
          id: 'g-forged-voice', members: { cache: new Map() }, memberCount: 1,
          channels: { cache: new Map() },
        },
        memberPermissions: { has: jest.fn(() => false) },
      };
      const { renderConfirmCardRows } = commands._test;
      renderConfirmCardRows({
        sendDisabled: false,
        expiresIn: '24h',
        selfDestructSeconds: null,
        personalMessage: null,
        voiceChannelId: null,  // ← forged/drifted state
        interaction,
        recipientIds: ['100000000000000001'],
        recipientMode: 'voice',
      });
      const customIds = ButtonBuilder.mock.results.map(
        (r) => r.value.setCustomId.mock.calls[0]?.[0]
      );
      expect(customIds).toContain('qurl_confirm_pick_manual');
      expect(MentionableSelectMenuBuilder).not.toHaveBeenCalled();
    });
  });

  describe('everyone-mode layout (recipientMode:"everyone")', () => {
    // Mirror of the voice-mode layout block above. After handleConfirmEveryone
    // commits a fan-out, the card re-renders in everyone-mode, which:
    //   - HIDES the MentionableSelect picker row (so a stray picker
    //     interaction can't replace the fan-out with a 25-cap subset)
    //   - SHOWS a "👥 Pick people instead" button as the escape hatch
    //   - DOES NOT show the "📢 @everyone" or "🔊 Everyone on voice"
    //     entry buttons (you're already past them)
    // Driven through the renderer directly so the layout is asserted
    // without re-running the full slash → click round trip.
    const renderEveryoneRows = (overrides = {}) => {
      const { MentionableSelectMenuBuilder, ButtonBuilder } = require('discord.js');
      MentionableSelectMenuBuilder.mockClear();
      ButtonBuilder.mockClear();
      const memberCache = new Map([
        ['100000000000000001', { user: { id: '100000000000000001', bot: false } }],
        ['100000000000000002', { user: { id: '100000000000000002', bot: false } }],
      ]);
      const interaction = {
        guild: {
          id: 'g-everyone-layout',
          members: { cache: memberCache },
          memberCount: 2,
          channels: { cache: new Map() },
        },
        memberPermissions: { has: jest.fn(() => true) },
      };
      const { renderConfirmCardRows } = commands._test;
      renderConfirmCardRows({
        sendDisabled: false,
        expiresIn: '24h',
        selfDestructSeconds: null,
        personalMessage: null,
        voiceChannelId: null,
        interaction,
        recipientIds: ['100000000000000001', '100000000000000002'],
        recipientMode: 'everyone',
        ...overrides,
      });
      return { MentionableSelectMenuBuilder, ButtonBuilder };
    };

    test('picker row is NOT rendered', () => {
      const { MentionableSelectMenuBuilder } = renderEveryoneRows();
      expect(MentionableSelectMenuBuilder).not.toHaveBeenCalled();
    });

    test('"Pick people instead" button IS rendered', () => {
      const { ButtonBuilder } = renderEveryoneRows();
      const customIds = ButtonBuilder.mock.results.map(
        (r) => r.value.setCustomId.mock.calls[0]?.[0]
      );
      expect(customIds).toContain('qurl_confirm_pick_manual');
    });

    test('"@everyone" entry button is NOT rendered (already past the entry point)', () => {
      const { ButtonBuilder } = renderEveryoneRows();
      const customIds = ButtonBuilder.mock.results.map(
        (r) => r.value.setCustomId.mock.calls[0]?.[0]
      );
      expect(customIds).not.toContain('qurl_confirm_everyone');
    });

    test('"Everyone on voice" entry button is NOT rendered (even with voiceChannelId set)', () => {
      // The voiceChannelId snapshot survives on the payload across modes;
      // make sure the mode gate still hides the voice-everyone entry
      // button so the user doesn't see a stale affordance.
      const { ButtonBuilder } = renderEveryoneRows({ voiceChannelId: 'voice-ch-1' });
      const customIds = ButtonBuilder.mock.results.map(
        (r) => r.value.setCustomId.mock.calls[0]?.[0]
      );
      expect(customIds).not.toContain('qurl_confirm_voice_everyone');
    });

    test('bottom row has exactly 4 buttons in order: Pick-manual, Note, Send, Cancel', () => {
      // Matches the voice-mode layout exactly — everyone-mode shares the
      // same escape-hatch row shape.
      const { ButtonBuilder } = renderEveryoneRows();
      const customIds = ButtonBuilder.mock.results.map(
        (r) => r.value.setCustomId.mock.calls[0]?.[0]
      );
      expect(customIds).toEqual([
        'qurl_confirm_pick_manual',
        'qurl_confirm_note_btn',
        'qurl_confirm_send',
        'qurl_confirm_cancel',
      ]);
    });
  });

  // ── @everyone button rendering ──
  // Workaround for Discord's MentionableSelectMenu filtering @everyone
  // out of its UI. Render-time gating on MENTION_EVERYONE + picker
  // mode + guild context. Click-time resolution handled separately
  // by handleConfirmEveryone tests.
  describe('@everyone button', () => {
    test('renders when sender has MENTION_EVERYONE in guild (picker mode)', async () => {
      const { ButtonBuilder } = require('discord.js');
      ButtonBuilder.mockClear();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
        guildMembers: { '100000000000000001': {} },
      });
      int.memberPermissions = { has: jest.fn(() => true) };
      int.guild.memberCount = 5;
      await handleQurlSend(int);
      const customIds = ButtonBuilder.mock.results.map((r) => r.value.setCustomId.mock.calls[0]?.[0]);
      expect(customIds).toContain('qurl_confirm_everyone');
    });

    test('does NOT render without MENTION_EVERYONE', async () => {
      // Default permission shape falls through the gate. Symmetric with
      // Discord's MentionableSelectMenu, which also hides @everyone for
      // non-permitted users.
      const { ButtonBuilder } = require('discord.js');
      ButtonBuilder.mockClear();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
        guildMembers: { '100000000000000001': {} },
      });
      await handleQurlSend(int);
      const customIds = ButtonBuilder.mock.results.map((r) => r.value.setCustomId.mock.calls[0]?.[0]);
      expect(customIds).not.toContain('qurl_confirm_everyone');
    });

    test('label is the fixed "📢 @everyone" form — no live count, no overcap suffix', async () => {
      // The label used to carry a `(N)` count and an `— exceeds N cap`
      // suffix; both were dropped per UX call so the label stays terse
      // and the disabled+greyed-out button is the only state signal.
      // `computeEveryoneDisplayCount` still runs for the disable check
      // (see counts-and-disable tests below); only the label-surface
      // changed.
      const { ButtonBuilder } = require('discord.js');
      ButtonBuilder.mockClear();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
        guildMembers: {
          '100000000000000001': {},
          '100000000000000002': {},
          '100000000000000099': { bot: true },
        },
      });
      int.memberPermissions = { has: jest.fn(() => true) };
      int.guild.memberCount = 3;
      await handleQurlSend(int);
      const everyoneBtn = ButtonBuilder.mock.results.find(
        (r) => r.value.setCustomId.mock.calls[0]?.[0] === 'qurl_confirm_everyone'
      );
      expect(everyoneBtn).toBeDefined();
      expect(everyoneBtn.value.setLabel).toHaveBeenCalledWith('\u{1F4E2} @everyone');
      // Defense-in-depth: pin the missing pieces directly so a future
      // refactor that re-introduces them surfaces here.
      const label = everyoneBtn.value.setLabel.mock.calls[0][0];
      expect(label).not.toMatch(/\(\d+\)/);
      expect(label).not.toMatch(/exceeds/);
      expect(label).not.toMatch(/\(\?\)/);
    });

    test('disabled when memberCount unavailable AND cache cold (displayCount null)', async () => {
      // Edge case: cold cache + missing memberCount → can't compute a
      // count → button stays disabled so the user sees the button can't
      // act rather than getting a silent no-op click. The label no
      // longer carries a `(?)` indicator (UX call); the disabled state
      // is communicated by the greyed-out button alone.
      const { ButtonBuilder } = require('discord.js');
      ButtonBuilder.mockClear();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
        guildMembers: { '100000000000000001': {} },
      });
      int.memberPermissions = { has: jest.fn(() => true) };
      // Leave memberCount unset → undefined.
      delete int.guild.memberCount;
      await handleQurlSend(int);
      const everyoneBtn = ButtonBuilder.mock.results.find(
        (r) => r.value.setCustomId.mock.calls[0]?.[0] === 'qurl_confirm_everyone'
      );
      expect(everyoneBtn).toBeDefined();
      expect(everyoneBtn.value.setDisabled).toHaveBeenCalledWith(true);
    });

    test('does NOT render in DM context — direct renderer assertion', () => {
      // Pin the renderer's `interaction.guild` gate directly (not via
      // handleQurlSend's entry-point DM-rejection). Without a direct
      // assertion, a future refactor that loosens the renderer gate
      // would only fail tests via the entry-point guard, leaving the
      // renderer-only contract under-pinned.
      const { ButtonBuilder } = require('discord.js');
      const commands = require('../src/commands');
      // _test exports are only available in non-production (NODE_ENV !==
      // 'production'). Jest runs without setting NODE_ENV → test mode.
      const { renderConfirmCardRows } = commands._test;
      ButtonBuilder.mockClear();
      // DM-shaped interaction: no `guild`, but MENTION_EVERYONE is
      // (defensively) granted on memberPermissions to prove the
      // renderer doesn't lean on perms alone.
      const dmInteraction = {
        guild: null,
        memberPermissions: { has: jest.fn(() => true) },
      };
      renderConfirmCardRows({
        sendDisabled: false,
        expiresIn: '24h',
        selfDestructSeconds: null,
        personalMessage: null,
        voiceChannelId: null,
        interaction: dmInteraction,
        recipientIds: [],
        recipientMode: 'picker',
      });
      const customIds = ButtonBuilder.mock.results.map((r) => r.value.setCustomId.mock.calls[0]?.[0]);
      expect(customIds).not.toContain('qurl_confirm_everyone');
    });

    test('non-bot count is memoized across re-renders with stable cache', () => {
      // Confirm cards re-render on every picker change / expiry select /
      // note edit. For a large guild the per-render O(N) bot filter
      // would compound; the memo keyed on `cache.size:memberCount` lets
      // a single flow's re-renders share one count computation. Pin
      // memoization by counting cache iterations directly — without a
      // memo, every render would iterate; with the memo, only the
      // first render does. Behavioral label assertion alone passes
      // under both implementations, so the iteration counter is
      // load-bearing.
      const commands = require('../src/commands');
      const { renderConfirmCardRows, _everyoneCountMemo } = commands._test;
      const guildId = 'guild-memo-iter';
      // Wrap the cache Map to count `[Symbol.iterator]` invocations.
      // discord.js's `Collection` and a plain `Map` both delegate the
      // `for ... of` to `[Symbol.iterator]`, so this captures every
      // full-pass enumeration.
      const memberCache = new Map([
        ['u1', { user: { id: 'u1', bot: false } }],
        ['u2', { user: { id: 'u2', bot: false } }],
        ['b1', { user: { id: 'b1', bot: true } }],
      ]);
      let iterations = 0;
      const iterCountingCache = new Proxy(memberCache, {
        get(target, prop) {
          if (prop === Symbol.iterator) {
            iterations++;
            return target[Symbol.iterator].bind(target);
          }
          return Reflect.get(target, prop);
        },
      });
      const guild = {
        id: guildId,
        members: { cache: iterCountingCache },
        memberCount: 3,
        channels: { cache: new Map() },
      };
      // Defensive: clear any stale memo entry from previous tests.
      _everyoneCountMemo.delete(guild);
      const interaction = {
        guild,
        memberPermissions: { has: jest.fn(() => true) },
      };
      const args = {
        sendDisabled: false,
        expiresIn: '24h',
        selfDestructSeconds: null,
        personalMessage: null,
        voiceChannelId: null,
        interaction,
        recipientIds: [],
        recipientMode: 'picker',
      };
      for (let i = 0; i < 5; i++) renderConfirmCardRows(args);
      // First render walks the cache to compute non-bot count → 1
      // iteration. Subsequent renders hit the memo → 0 additional
      // iterations. Total across 5 renders: 1.
      expect(iterations).toBe(1);
    });

    test('memo busts on cache.size change', () => {
      // Member join/leave changes `cache.size` (or `memberCount`),
      // which fingerprints the memo entry and forces re-computation.
      // Verified via direct iteration counter — the label is now
      // fixed and can't observe the recomputation, but the memo is
      // still load-bearing for the disable check (over-cap / zero /
      // null branches), so we pin it via the same Proxy iterator
      // pattern as the memoization test above.
      const commands = require('../src/commands');
      const { renderConfirmCardRows, _everyoneCountMemo } = commands._test;
      const memberCache = new Map([
        ['u1', { user: { id: 'u1', bot: false } }],
        ['u2', { user: { id: 'u2', bot: false } }],
      ]);
      let iterations = 0;
      const iterCountingCache = new Proxy(memberCache, {
        get(target, prop) {
          if (prop === Symbol.iterator) {
            iterations++;
            return target[Symbol.iterator].bind(target);
          }
          return Reflect.get(target, prop);
        },
      });
      const guild = {
        id: 'guild-memo-bust',
        members: { cache: iterCountingCache },
        memberCount: 2,
        channels: { cache: new Map() },
      };
      _everyoneCountMemo.delete(guild);
      const args = {
        sendDisabled: false,
        expiresIn: '24h',
        selfDestructSeconds: null,
        personalMessage: null,
        voiceChannelId: null,
        interaction: { guild, memberPermissions: { has: jest.fn(() => true) } },
        recipientIds: [],
        recipientMode: 'picker',
      };
      renderConfirmCardRows(args);  // memo populated, 1 iteration
      expect(iterations).toBe(1);
      // Member joins → cache grows + memberCount grows. Fingerprint
      // flips, memo busts on next render → cache re-walked.
      memberCache.set('u3', { user: { id: 'u3', bot: false } });
      guild.memberCount = 3;
      renderConfirmCardRows(args);
      expect(iterations).toBe(2);
    });

    test('disabled when warm-cache non-bot count > cap (no overcap suffix in label)', async () => {
      // Render-time over-cap disable fires ONLY when the count is
      // accurate (warm cache + bot-filtered). Cold-cache over-cap
      // defers to click-time hard-reject — see counter-test below.
      // The label used to carry an "— exceeds N cap" suffix in this
      // branch; dropped per UX call. The disabled+greyed-out button
      // is the only state signal now.
      const config = require('../src/config');
      const { ButtonBuilder } = require('discord.js');
      const originalCap = config.QURL_SEND_MAX_RECIPIENTS;
      try {
        Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: 2, configurable: true, writable: true });
        ButtonBuilder.mockClear();
        const int = makeInteraction({
          options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
          guildMembers: {  // 4 cached, all non-bot → count=4 > cap=2
            '100000000000000001': {},
            '100000000000000002': {},
            '100000000000000003': {},
            '100000000000000004': {},
          },
        });
        int.memberPermissions = { has: jest.fn(() => true) };
        int.guild.memberCount = 4;  // matches cache.size → warm + accurate
        await handleQurlSend(int);
        const everyoneBtn = ButtonBuilder.mock.results.find(
          (r) => r.value.setCustomId.mock.calls[0]?.[0] === 'qurl_confirm_everyone'
        );
        expect(everyoneBtn).toBeDefined();
        expect(everyoneBtn.value.setLabel).toHaveBeenCalledWith('\u{1F4E2} @everyone');
        expect(everyoneBtn.value.setDisabled).toHaveBeenCalledWith(true);
      } finally {
        Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: originalCap, configurable: true, writable: true });
      }
    });

    test('cold-cache memberCount > cap does NOT disable (avoid bot-overcount false-positive)', async () => {
      // Counter-test: a guild with `memberCount = 500` but only 1
      // cached member is cold; `displayCount = memberCount` is an
      // over-count by bot population. Disabling on it would false-
      // positive on a near-cap guild whose actual non-bot count is
      // under cap. Defer to click-time hard-reject (which runs against
      // the prewarmed cache).
      const config = require('../src/config');
      const { ButtonBuilder } = require('discord.js');
      const originalCap = config.QURL_SEND_MAX_RECIPIENTS;
      try {
        Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: 100, configurable: true, writable: true });
        ButtonBuilder.mockClear();
        const int = makeInteraction({
          options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
          guildMembers: { '100000000000000001': {} },  // cache.size=1
        });
        int.memberPermissions = { has: jest.fn(() => true) };
        int.guild.memberCount = 500;  // cache.size < memberCount → cold
        await handleQurlSend(int);
        const everyoneBtn = ButtonBuilder.mock.results.find(
          (r) => r.value.setCustomId.mock.calls[0]?.[0] === 'qurl_confirm_everyone'
        );
        expect(everyoneBtn).toBeDefined();
        // Label shows raw count without "exceeds" suffix; button is
        // enabled because we don't trust the cold-path number for
        // disable decisions.
        expect(everyoneBtn.value.setLabel).toHaveBeenCalledWith(expect.not.stringContaining('exceeds'));
        expect(everyoneBtn.value.setDisabled).toHaveBeenCalledWith(false);
      } finally {
        Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: originalCap, configurable: true, writable: true });
      }
    });

    test('both @everyone AND voice-everyone buttons render together when invoked from voice + MENTION_EVERYONE', async () => {
      // Pin the 5-component-row invariant in the worst-case render:
      // [🔊 Voice] + [📢 @everyone] + Note + Send + Cancel = exactly 5,
      // hitting Discord's hard ActionRow limit. A future refactor that
      // shifts any of those into a second row (or adds a 6th) would
      // break this assertion.
      const { ButtonBuilder, ChannelType } = require('discord.js');
      ButtonBuilder.mockClear();
      const voiceChannelId = 'voice-room-1';
      const int = makeInteraction({
        channelId: voiceChannelId,
        options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
        guildMembers: { '100000000000000001': {} },
      });
      int.memberPermissions = { has: jest.fn(() => true) };
      int.guild.memberCount = 5;
      // Drop the slash invocation INTO a voice channel by attaching a
      // voice-channel shape to the interaction. The slash-entry's
      // voice-detection branch reads `interaction.channel.type`.
      int.channel = {
        id: voiceChannelId, name: 'general', type: ChannelType.GuildVoice,
        members: new Map([['100000000000000001', { user: makeUser('100000000000000001') }]]),
      };
      int.guild.channels.cache.set(voiceChannelId, int.channel);
      await handleQurlSend(int);
      const customIds = ButtonBuilder.mock.results.map((r) => r.value.setCustomId.mock.calls[0]?.[0]);
      // Both affordances present; full bottom row = Voice + @everyone +
      // Note + Send + Cancel = 5 components (Discord's hard cap).
      expect(customIds).toContain('qurl_confirm_voice_everyone');
      expect(customIds).toContain('qurl_confirm_everyone');
      expect(customIds).toContain('qurl_confirm_note_btn');
      expect(customIds).toContain('qurl_confirm_send');
      expect(customIds).toContain('qurl_confirm_cancel');
      expect(customIds.length).toBe(5);
    });

    test('does NOT render in voice-mode (recipientMode === RECIPIENT_MODE_VOICE)', () => {
      // Voice-mode already targets the voice-channel population. The
      // @everyone button there would confuse "everyone" semantics —
      // does it mean voice or guild? Gate stays at picker-mode only.
      // Direct renderer assertion (matches the DM-context test above)
      // — couples this contract to the renderer's gate, not to expiry-
      // select's internals.
      const { ButtonBuilder } = require('discord.js');
      const { renderConfirmCardRows } = commands._test;
      ButtonBuilder.mockClear();
      const memberCache = new Map([['100000000000000001', { user: makeUser('100000000000000001') }]]);
      const interaction = {
        guild: {
          id: 'g-voice', members: { cache: memberCache }, memberCount: 5,
          channels: { cache: new Map() },
        },
        memberPermissions: { has: jest.fn(() => true) },
      };
      renderConfirmCardRows({
        sendDisabled: false,
        expiresIn: '24h',
        selfDestructSeconds: null,
        personalMessage: null,
        voiceChannelId: 'voice-ch-1',
        interaction,
        recipientIds: ['100000000000000001'],
        recipientMode: 'voice',
      });
      const customIds = ButtonBuilder.mock.results.map((r) => r.value.setCustomId.mock.calls[0]?.[0]);
      expect(customIds).not.toContain('qurl_confirm_everyone');
      // Sanity check: voice-mode renders the "Pick people instead" button.
      expect(customIds).toContain('qurl_confirm_pick_manual');
    });
  });
});

// ──────────────────────────────────────────────────────────────
// computeEveryoneDisplayCount — direct unit tests for the helper that
// drives the @everyone button's label count. Exported via `_test` so
// the WeakMap+fingerprint contract can be pinned in isolation rather
// than only through `renderConfirmCardRows`.
// ──────────────────────────────────────────────────────────────

describe('computeEveryoneDisplayCount', () => {
  const { computeEveryoneDisplayCount, _everyoneCountMemo } = commands._test;

  test('warm cache (cache.size === memberCount) → accurate non-bot count', () => {
    const guild = {
      id: 'g-warm',
      memberCount: 3,
      members: {
        cache: new Map([
          ['u1', { user: { id: 'u1', bot: false } }],
          ['u2', { user: { id: 'u2', bot: false } }],
          ['b1', { user: { id: 'b1', bot: true } }],
        ]),
      },
    };
    _everyoneCountMemo.delete(guild);
    expect(computeEveryoneDisplayCount(guild)).toEqual({ count: 2, accurate: true });
  });

  test('cold cache (cache.size < memberCount) → memberCount fallback, NOT accurate', () => {
    // Fallback to raw memberCount over-counts by bot population; the
    // `accurate: false` flag tells callers (render-time over-cap
    // disable) not to trust the number for safety decisions.
    const guild = {
      id: 'g-cold',
      memberCount: 50,
      members: { cache: new Map([['u1', { user: { id: 'u1', bot: false } }]]) },
    };
    _everyoneCountMemo.delete(guild);
    expect(computeEveryoneDisplayCount(guild)).toEqual({ count: 50, accurate: false });
  });

  test('memberCount undefined + warm-shape cache → cold fallback returns {count: null, accurate: false}', () => {
    // The `cache.size >= memberCount` test reads as `cache.size >=
    // undefined`, which evaluates to false → cold branch. memberCount
    // missing also fails the cold-branch's `typeof === 'number'`
    // check → final return is `{count: null, accurate: false}`. Pin
    // this since the comparison's evaluation is non-obvious.
    const guild = {
      id: 'g-no-mc',
      members: { cache: new Map([['u1', { user: { id: 'u1', bot: false } }]]) },
      // memberCount intentionally absent
    };
    _everyoneCountMemo.delete(guild);
    expect(computeEveryoneDisplayCount(guild)).toEqual({ count: null, accurate: false });
  });

  test('cache missing → memberCount fallback when present', () => {
    const guild = { id: 'g-no-cache', memberCount: 7, members: undefined };
    _everyoneCountMemo.delete(guild);
    expect(computeEveryoneDisplayCount(guild)).toEqual({ count: 7, accurate: false });
  });

  test('cache and memberCount both missing → null', () => {
    const guild = { id: 'g-bare', members: undefined };
    _everyoneCountMemo.delete(guild);
    expect(computeEveryoneDisplayCount(guild)).toEqual({ count: null, accurate: false });
  });

  test('partial-cache row (no .user) does not inflate count', () => {
    // The `m?.user && !isBotMember(m)` guard aligns the render-time
    // count with the click-time partition filter — a degraded row
    // counts as 0 here AND lands in `partialCacheDrops` there.
    const guild = {
      id: 'g-partial',
      memberCount: 2,
      members: {
        cache: new Map([
          ['u1', { user: { id: 'u1', bot: false } }],
          ['degraded', { /* no .user */ }],
        ]),
      },
    };
    _everyoneCountMemo.delete(guild);
    expect(computeEveryoneDisplayCount(guild)).toEqual({ count: 1, accurate: true });
  });
});

// ──────────────────────────────────────────────────────────────
// largeSendThreshold formula — covers the floor / half-cap branches +
// degenerate cap guard.
// ──────────────────────────────────────────────────────────────

describe('largeSendThreshold', () => {
  const { largeSendThreshold, LARGE_SEND_RECIPIENT_FLOOR } = commands._test;
  const config = require('../src/config');

  test('default cap (20k): floor wins (1000)', () => {
    const orig = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 20000;
    try {
      expect(largeSendThreshold()).toBe(LARGE_SEND_RECIPIENT_FLOOR);
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = orig;
    }
  });

  test('small override (cap=500): half-cap wins (250)', () => {
    const orig = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 500;
    try {
      expect(largeSendThreshold()).toBe(250);
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = orig;
    }
  });

  test('degenerate override (cap=1): floors at 1 (NOT 0 — would fire every send)', () => {
    // Bug-guard: Math.floor(1/2) = 0 would make `>= threshold` true
    // for every send, including 0-recipient ones. The `|| 1`
    // substitution floors at 1 so the threshold is always a positive
    // integer.
    const orig = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 1;
    try {
      expect(largeSendThreshold()).toBe(1);
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = orig;
    }
  });

  test('boundary (cap=2): floor(2/2)=1 wins WITHOUT the substitution — pins discontinuity', () => {
    // Round-15 cr: a 2-recipient send on cap=2 warns; a 1-recipient
    // send on cap=2 does not (threshold=1). The substitution-vs-
    // half-cap boundary is at cap=2 — pin the intentional shape so
    // a future refactor (e.g., switching from `half || 1` to
    // `Math.max(2, half)`) breaks this test instead of silently
    // moving the boundary.
    const orig = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 2;
    try {
      expect(largeSendThreshold()).toBe(1);
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = orig;
    }
  });

  test('cap exactly at floor (1000): half-cap (500) wins', () => {
    const orig = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 1000;
    try {
      expect(largeSendThreshold()).toBe(500);
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = orig;
    }
  });
});

// ──────────────────────────────────────────────────────────────
// Constants + registration assertions
// ──────────────────────────────────────────────────────────────

describe('constants + exports', () => {
  // customId pins catch wire-protocol drift. They look tautological
  // (the constants are imported AND asserted against literals here)
  // but the value is the string Discord routes button clicks to —
  // a rename in production would invalidate every confirm card that
  // shipped before the redeploy. The literal pin makes that
  // breaking-change visible at test time. We do NOT pin
  // SEND_FLOW_TTL_SECONDS / SEND_STAGE_AWAITING_CONFIRM as literals
  // because those values are internal (not exposed to Discord) and
  // the behavioral assertions in the handler describe blocks
  // (`expect(mockSupersedeOrCreate).toHaveBeenCalledWith({stage:
  // SEND_STAGE_AWAITING_CONFIRM, ...})`) already pin them by
  // contract.
  test('customIds match the wire-protocol values Discord routes against', () => {
    // Pin EVERY confirm-card customId constant. A typo in any one of
    // these silently breaks routing for in-flight cards across a
    // deploy, so the contract test asserts against the literal wire
    // value rather than just the JS constant binding.
    expect(CONFIRM_USER_SELECT_CUSTOM_ID).toBe('qurl_confirm_user_select');
    expect(CONFIRM_SEND_CUSTOM_ID).toBe('qurl_confirm_send');
    expect(CONFIRM_CANCEL_CUSTOM_ID).toBe('qurl_confirm_cancel');
    expect(CONFIRM_EXPIRY_SELECT_CUSTOM_ID).toBe('qurl_confirm_expiry');
    expect(CONFIRM_SELF_DESTRUCT_SELECT_CUSTOM_ID).toBe('qurl_confirm_self_destruct');
    expect(CONFIRM_NOTE_BUTTON_CUSTOM_ID).toBe('qurl_confirm_note_btn');
    expect(CONFIRM_NOTE_MODAL_CUSTOM_ID).toBe('qurl_confirm_note_modal');
    expect(CONFIRM_VOICE_EVERYONE_BUTTON_CUSTOM_ID).toBe('qurl_confirm_voice_everyone');
    expect(CONFIRM_PICK_MANUAL_BUTTON_CUSTOM_ID).toBe('qurl_confirm_pick_manual');
  });

  test('all customIds unique', () => {
    const ids = new Set([
      CONFIRM_USER_SELECT_CUSTOM_ID,
      CONFIRM_SEND_CUSTOM_ID,
      CONFIRM_CANCEL_CUSTOM_ID,
      CONFIRM_EXPIRY_SELECT_CUSTOM_ID,
      CONFIRM_SELF_DESTRUCT_SELECT_CUSTOM_ID,
      CONFIRM_NOTE_BUTTON_CUSTOM_ID,
      CONFIRM_NOTE_MODAL_CUSTOM_ID,
      CONFIRM_VOICE_EVERYONE_BUTTON_CUSTOM_ID,
      CONFIRM_PICK_MANUAL_BUTTON_CUSTOM_ID,
    ]);
    expect(ids.size).toBe(9);
  });

  test('recipientMode tokens are stable wire values (persisted in flow_state rows)', () => {
    // The mode token gets serialized into DDB along with the rest of
    // the payload. Renaming the literal would orphan in-flight cards
    // across a deploy — the dispatcher reads the field at click time
    // and would silently mis-route. Pin the literals here.
    expect(RECIPIENT_MODE_PICKER).toBe('picker');
    expect(RECIPIENT_MODE_VOICE).toBe('voice');
    expect(RECIPIENT_MODE_EVERYONE).toBe('everyone');
  });

  test('normalizeRecipientMode: closed set {voice, everyone, picker}; everything else picker', () => {
    // Stale flow_state rows (created before this field existed) read
    // as undefined. Off-set values (forged interaction, schema drift,
    // typo in a future refactor) also fall back to picker. Pin the
    // table so a future refactor that flips the default — or
    // accidentally collapses 'everyone' back to 'picker' — can't slip
    // through.
    expect(normalizeRecipientMode('voice')).toBe('voice');
    expect(normalizeRecipientMode('everyone')).toBe('everyone');
    expect(normalizeRecipientMode('picker')).toBe('picker');
    expect(normalizeRecipientMode(undefined)).toBe('picker');
    expect(normalizeRecipientMode(null)).toBe('picker');
    expect(normalizeRecipientMode('')).toBe('picker');
    expect(normalizeRecipientMode('VOICE')).toBe('picker'); // case-sensitive
    expect(normalizeRecipientMode('EVERYONE')).toBe('picker'); // case-sensitive
    expect(normalizeRecipientMode('unknown')).toBe('picker');
  });

  test('siblingMessage is keyed by stage so any of the three confirm-card customIds surfaces the same message', () => {
    // siblingMessage is registered only on CONFIRM_USER_SELECT_CUSTOM_ID
    // (commands.js's registerFlow blocks), but flow-dispatch stores
    // siblingMessages keyed by EXPECTED_STAGE — so a /qurl revoke
    // supersede that peeks at a row at SEND_STAGE_AWAITING_CONFIRM
    // gets the same actionable message regardless of which customId
    // was registered. Pin the lookup so a future refactor that
    // accidentally keys siblingMessage by customId breaks here.
    const { siblingMessageForStage } = require('../src/flow-dispatch');
    const msg = siblingMessageForStage(SEND_STAGE_AWAITING_CONFIRM);
    expect(msg).toMatch(/qurl send.*qurl map.*confirm card/i);
    // Defense-in-depth: confirm-card customIds for SEND + CANCEL,
    // although registered without their own siblingMessage, still
    // reach the same registered message through the stage lookup.
    // (Tested indirectly: any customId at this stage maps to the
    // same single registered message.)
    expect(msg).toBeTruthy();
  });

  test('all four new confirm-card menu customIds are registered (duplicate-register throws)', () => {
    // Pins the registration of expiry / self-destruct / note button /
    // note modal customIds. Their siblingMessage is inherited from the
    // SEND_STAGE_AWAITING_CONFIRM keyed entry above — so a refactor
    // that fails to register one of these would surface as an
    // unrouted dispatch, not a wrong siblingMessage. This test exists
    // to catch the registration-omission shape directly: a duplicate
    // registerFlow on each customId must throw "already registered".
    const { registerFlow } = require('../src/flow-dispatch');
    const newCustomIds = [
      'qurl_confirm_expiry',
      'qurl_confirm_self_destruct',
      'qurl_confirm_note_btn',
      'qurl_confirm_note_modal',
    ];
    for (const id of newCustomIds) {
      expect(() => registerFlow(id, {
        expectedStage: 'noop_stage_for_collision_check',
        handler: () => undefined,
      })).toThrow(/already registered/);
    }
  });

  test('voice-everyone + pick-manual buttons are registered at the confirm-card stage', () => {
    // Same shape as the "four new confirm-card menu customIds" check.
    // Catches the registration-omission shape for the voice-mode pair
    // — a missed registerFlow surfaces as an unrouted dispatch in
    // production, which is harder to debug than a duplicate-register
    // throw at startup.
    const { registerFlow } = require('../src/flow-dispatch');
    for (const id of ['qurl_confirm_voice_everyone', 'qurl_confirm_pick_manual']) {
      expect(() => registerFlow(id, {
        expectedStage: 'noop_stage_for_collision_check',
        handler: () => undefined,
      })).toThrow(/already registered/);
    }
  });

  test('executeSendPipeline still exported (back-half hook)', () => {
    expect(typeof executeSendPipeline).toBe('function');
  });

  test('CONTRACT: executeSendPipeline never reads personalMessageRaw', () => {
    // Static guard against future bit-rot. `personalMessageRaw` is
    // for modal-prefill only — it's the trimmed user input WITHOUT
    // NFKC + bidi-strip + markdown-escape passes. A future refactor
    // that accidentally reads it from any rendering path or the
    // pipeline would bypass sanitization and could render injected
    // markdown / masked links into recipient DMs.
    //
    // The contract is preserved structurally today: executeSendPipeline
    // destructures by explicit field name (not `...payload` spread).
    // This test extracts the function body from source and pins that
    // the substring `personalMessageRaw` does not appear inside it.
    //
    // **If this test fails:** the substring scan is intentionally
    // coarse — even a comment like `// don't read personalMessageRaw
    // here` inside executeSendPipeline trips it. Document the
    // invariant at the field declaration in handleQurlSlashSend
    // (search commands.js for "CONTRACT: `personalMessageRaw` is for
    // modal-prefill ONLY"), not inside executeSendPipeline's body.
    const fs = require('fs');
    const path = require('path');
    const src = fs.readFileSync(path.resolve(__dirname, '../src/commands.js'), 'utf8');
    const startMarker = 'async function executeSendPipeline(';
    const startIdx = src.indexOf(startMarker);
    expect(startIdx).toBeGreaterThanOrEqual(0);
    // Walk forward tracking brace depth; first time depth returns to
    // 0 after entering the body marks the end of the function.
    // LIMITATION: braces inside string literals / template literals /
    // regex literals would mis-balance the count. The current
    // executeSendPipeline body is free of those constructs — if a
    // future change adds one (e.g. `const re = /\{[^}]*\}/;`), this
    // walker needs upgrading to a real tokenizer.
    //
    // ALSO LIMITATION: destructured default params with object
    // literals (e.g. `personalMessage = { fallback: null }`) trip
    // the same brace-count failure mode — the first `{` after the
    // signature would be the default's, not the body's. The runtime
    // Proxy CONTRACT test below is the load-bearing safety net for
    // these cases; this static check is belt-and-suspenders against
    // the common refactor shape.
    let i = src.indexOf('{', startIdx);
    expect(i).toBeGreaterThanOrEqual(0);
    let depth = 0;
    let end = -1;
    for (; i < src.length; i++) {
      if (src[i] === '{') depth++;
      else if (src[i] === '}') {
        depth--;
        if (depth === 0) { end = i; break; }
      }
    }
    expect(end).toBeGreaterThan(startIdx);
    const fnBody = src.slice(startIdx, end + 1);
    expect(fnBody).not.toContain('personalMessageRaw');
  });

  test('CONTRACT (runtime): handleConfirmSendClick never reads payload.personalMessageRaw on the Send path', async () => {
    // Runtime complement to the static brace-walker above. Wraps
    // row.payload in a Proxy that throws on any get('personalMessageRaw')
    // access, then drives handleConfirmSendClick through to the
    // executeSendPipeline call. If the handler (or anything downstream
    // it triggers through this payload reference) reads the field, the
    // Proxy throws and the test fails — survives future syntax changes
    // the brace-walker can't (string/regex literals containing braces).
    const u1 = '100000000000000001';
    const basePayload = {
      resourceType: 'file',
      attachment: VALID_ATTACHMENT,
      locationUrl: null,
      locationName: null,
      resourceLabel: 'x.png',
      recipientIds: [u1],
      recipientAliases: { [u1]: 'Alice' },
      expiresIn: '24h',
      selfDestructSeconds: null,
      personalMessage: 'safe content',
      personalMessageRaw: '**FORBIDDEN_RAW**',
      warningsBlock: '',
      sendNonce: 'nonce-contract',
    };
    let leaked = false;
    const trappedPayload = new Proxy(basePayload, {
      get(target, prop) {
        if (prop === 'personalMessageRaw') {
          leaked = true;
          throw new Error('CONTRACT VIOLATION: handleConfirmSendClick read payload.personalMessageRaw');
        }
        return target[prop];
      },
    });
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: trappedPayload, version: 1 } });
    expect(leaked).toBe(false);
  });

  test('CONTRACT (runtime, pipeline-direct): executeSendPipeline never reads personalMessageRaw from its params', async () => {
    // Complement to the handler-level Proxy test above. cr round-15
    // flagged that the handler-level test only catches a click-path
    // refactor that spreads row.payload — it doesn't catch a future
    // change that adds `personalMessageRaw` to executeSendPipeline's
    // destructure list. This test wraps the params object that
    // executeSendPipeline receives in a Proxy that throws on
    // get('personalMessageRaw'). If the destructure or any internal
    // access touches the field, the Proxy throws and the test fails.
    const u1 = '100000000000000001';
    const validParams = {
      apiKey: 'apikey-1',
      resourceType: 'file',
      attachment: VALID_ATTACHMENT,
      locationUrl: null,
      locationName: null,
      recipients: [{ id: u1, username: 'Alice', bot: false }],
      expiresIn: '24h',
      selfDestructSeconds: null,
      personalMessage: 'safe content',
      // Intentionally smuggled into params alongside personalMessage —
      // a future refactor that destructures it would trip the Proxy.
      personalMessageRaw: '**FORBIDDEN_RAW**',
      sendNonce: 'nonce-pipeline-contract',
    };
    let leaked = false;
    const trappedParams = new Proxy(validParams, {
      get(target, prop) {
        if (prop === 'personalMessageRaw') {
          leaked = true;
          throw new Error('CONTRACT VIOLATION: executeSendPipeline read params.personalMessageRaw');
        }
        return target[prop];
      },
    });
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    // executeSendPipeline does its own resolveRecipientUsers / API
    // calls; we just need to fire it and verify the Proxy didn't
    // throw on a destructure. The downstream calls will fail with
    // mocked-out dependencies, which is fine — the Proxy fires at
    // destructure-time (function entry), before any IO.
    try {
      await executeSendPipeline(int, trappedParams);
    } catch (err) {
      // Re-throw only if the Proxy was the one that threw. Other
      // errors (mocked-axios reject, etc) are expected and irrelevant.
      if (leaked) throw err;
    }
    expect(leaked).toBe(false);
  });
});
