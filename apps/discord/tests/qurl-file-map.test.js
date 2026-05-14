/**
 * Tests for /qurl file + /qurl map slash commands (PR 7b.2).
 *
 * Covers the new handlers from src/commands.js — handleQurlFile,
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

// flow-dispatch (registerFlow + customId routing) is loaded for real —
// only flow-state's DDB-backed primitives are mocked above. Loading
// the real registerFlow lets the idempotent-failure guard fire on
// test re-loads and pins the customId → handler contract.
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
  resolveMentionableSelection,
  parseLocationInput,
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
  SEND_FLOW_TTL_SECONDS,
  SELF_DESTRUCT_NO_TIMER_CHOICE,
  isOnCooldown,
  setCooldown,
  clearCooldown,
  sendCooldowns,
  executeSendPipeline,
  getActiveFileSends,
  setActiveFileSends,
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
});

describe('resolveMentionableSelection', () => {
  // Picker-path helper (Mentionable select returns BOTH users + roles in
  // one interaction): expands the role members → flat user list, filters
  // bots, dedupes against directly picked users, caps at
  // QURL_SEND_MAX_RECIPIENTS, gates the @everyone role on canMentionEveryone.
  const GUILD_ID = 'guild-1';

  function makeRole({ id, members = [] }) {
    // role.members is a Discord.js Collection but only `.entries()`
    // (iterable of [id, member]) is read; a Map is shape-compatible.
    const memberMap = new Map(members.map((m) => [m.user.id, m]));
    return [id, { id, members: memberMap }];
  }

  function makeMentionableInteraction({
    pickedUsers = [],
    pickedRoles = [],
    guildMemberCache = new Map(),
    inDM = false,
  } = {}) {
    const guild = inDM ? null : {
      id: GUILD_ID,
      members: { cache: guildMemberCache },
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
    // expansion filling the cap.
    for (const explicitId of explicitIds) {
      expect(resultIds).toContain(explicitId);
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

  test('returns the documented shape: { users, massMentionDenied }', () => {
    const int = makeMentionableInteraction({});
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(Object.keys(r).sort()).toEqual(['massMentionDenied', 'users']);
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
  });

  test('Google Maps place URL passes through with derived name', () => {
    const r = parseLocationInput('https://www.google.com/maps/place/Eiffel+Tower/@48.85,2.29,17z');
    expect(r.locationUrl).toContain('google.com/maps/place');
    expect(r.locationName).toBeTruthy();
  });

  test('plain place name synthesizes a search URL', () => {
    const r = parseLocationInput('Eiffel Tower, Paris');
    expect(r.locationUrl).toContain('google.com/maps/search');
    expect(r.locationName).toBe('Eiffel Tower, Paris');
  });

  test('plain non-URL text falls through to synth-search', () => {
    const r = parseLocationInput('not a url just plain text input');
    expect(r.locationUrl).toContain('google.com/maps/search');
  });

  test('https URL that does NOT match MAPS_URL_PATTERNS is treated as plain text and synth-searched', () => {
    // The MAPS_URL_PATTERNS regexes are quite specific (host + path
    // shape). A non-Google https URL fails them all, so parseLocationInput
    // falls through to the plain-text branch and synth-searches with
    // the raw input as the search query. isGoogleMapsURL re-validation
    // applies inside parseLocationInput when an extracted URL pattern
    // matches — the fall-through covers the "doesn't even match the
    // regex" case directly.
    const r = parseLocationInput('https://evil.example.com/maps/place/x');
    expect(r.locationUrl).toContain('google.com/maps/search');
    // The original URL is encoded into the search query, not used as
    // the locationUrl.
    expect(r.locationUrl).not.toContain('evil.example.com/maps/place');
  });

  test('malformed %-encoding in the input does not throw', () => {
    expect(() => parseLocationInput('https://www.google.com/maps/place/%ZZ-broken')).not.toThrow();
  });

  test('spoofed host (google.com.evil.com) fails the regex AND falls through to synth-search', () => {
    // Defense-in-depth contract: MAPS_URL_PATTERNS pins the literal
    // `google.com/` token (slash forces an end-of-host boundary), so a
    // spoofed host like `google.com.evil.com/maps/place/x` cannot match
    // any pattern. parseLocationInput therefore takes the synth-search
    // fall-through with the entire raw input as the search query —
    // isGoogleMapsURL never gets a chance to look at the spoofed host.
    // The conditional `if (detectedUrl && isGoogleMapsURL(detectedUrl))`
    // remains as defense-in-depth in case a future pattern relaxes the
    // host pin; this test pins the current contract.
    const spoofed = 'https://google.com.evil.com/maps/place/Eiffel-Tower';
    const r = parseLocationInput(spoofed);
    // INTENDED UX: the recipient embed renders `locationName` as a
    // LABEL on a Maps link whose TARGET is google.com. The spoofed
    // string is visible (so the recipient sees what was searched),
    // but the click goes to google.com/maps/search/?<encoded-spoof>.
    // sanitizeContentLabel further strips bidi/control + markdown-
    // escapes the label before it lands in the embed, so the visible
    // label text can't render as a clickable masked link or flip
    // direction via U+202E.
    const parsed = new URL(r.locationUrl);
    expect(parsed.hostname).toBe('www.google.com');
    expect(parsed.pathname.startsWith('/maps/search/')).toBe(true);
    // The raw spoofed input is the search query — recipient embeds
    // render that as text on a google.com link, not as a clickable
    // link to the spoofed host.
    expect(r.locationName).toBe(spoofed);
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
  // /qurl file and /qurl map share the sendCooldowns Map. setCooldown
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
      await handleQurlFile(int);
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
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

  test('/qurl map slash entry persists recipientAliases (parity with /qurl file)', async () => {
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const lastEdit = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastEdit.content).toMatch(/Mention Everyone\b/);
    // Alice still made it into the recipient list.
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.recipientIds).toEqual([aliceId]);
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
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

  test('safety-net catch deletes orphan flow row when post-supersede throw fires', async () => {
    // When supersedeOrCreate succeeds and then a downstream call
    // (renderConfirmCardContent, renderConfirmCardRows, the final
    // editReply, etc.) throws, the DDB row we just claimed would
    // sit orphaned until TTL eviction — blocking the user's next
    // /qurl file or /qurl map under the sibling-flow guard.
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    expect(mockDeleteFlow).not.toHaveBeenCalled();
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
    // handleQurlFile.
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

  test('partial transient lookup at Send click — Send proceeds with remaining, transient drop surfaced with retry copy (/qurl file)', async () => {
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
      content: expect.stringMatching(/1 couldn't be looked up.*rerun \/qurl file/),
      ephemeral: true,
    }));
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringMatching(/partial drop at click time/),
      expect.objectContaining({ left: 0, transient: 1 }),
    );
  });

  test('partial transient lookup at Send click — /qurl map payload produces /qurl map rerun hint', async () => {
    // The same handler serves /qurl file and /qurl map. A user who
    // invoked /qurl map should NOT be told to "rerun /qurl file" in
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
    // Must NOT say /qurl file when the user invoked /qurl map.
    expect(int.followUp).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/rerun \/qurl file/),
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
    // /qurl file → Cancel → /qurl file → Cancel and rack up
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
    // re-fire /qurl file within 5s of clicking Cancel, before the
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
    // chose at /qurl file time.
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
    // The picker's setMaxValues caps at min(USER_SELECT_PER_PICK_CAP=10,
    // QURL_SEND_MAX_RECIPIENTS=25) = 10, so production users physically
    // can't pick more than 25. But a future bump to either constant
    // (or a forged interaction) could trip this branch — pin the
    // guard so a refactor that drops it produces a visible failure.
    // QURL_SEND_MAX_RECIPIENTS = 25 in the mocked config; build a
    // pick of 26 to exceed it.
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
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
    await handleQurlFile(int);
    const noteBtn = ButtonBuilder.mock.results[0].value;
    expect(noteBtn.setLabel).toHaveBeenCalledWith(expect.stringMatching(/Edit note/));
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
    ]);
    expect(ids.size).toBe(7);
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
    expect(msg).toMatch(/qurl file.*qurl map.*confirm card/i);
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
