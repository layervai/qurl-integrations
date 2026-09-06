
jest.mock('../src/config', () => ({
  QURL_API_KEY: 'test-api-key',
  QURL_ENDPOINT: 'https://api.test.local',
  CONNECTOR_URL: 'https://connector.test.local',
  GOOGLE_MAPS_API_KEY: 'test-google-key',
  MAP_COMMAND_ENABLED: true,
  DETECT_COMMAND_ENABLED: true,
  QURL_SEND_COOLDOWN_MS: 30000,
  QURL_DETECT_COOLDOWN_MS: 30000,
  QURL_SEND_MAX_RECIPIENTS: 25,
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

jest.mock('discord.js', () => {
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
  markSendRevoked: jest.fn().mockResolvedValue(true),
  getSendConfig: jest.fn(),
  saveSendConfig: jest.fn(),
  getGuildApiKey: jest.fn(),
  getGuildConfig: jest.fn(),
  findSendsByQurlId: jest.fn(async () => []),
};
jest.mock('../src/store', () => mockDb);

jest.mock('../src/discord', () => ({
  sendDM: jest.fn().mockResolvedValue(true),
}));

const mockDetectWatermark = jest.fn();
jest.mock('../src/connector', () => ({
  downloadAndUpload: jest.fn(),
  reUploadBuffer: jest.fn(),
  mintLinks: jest.fn(),
  detectWatermark: (...a) => mockDetectWatermark(...a),
  uploadJsonToConnector: jest.fn(),
  isAllowedSourceUrl: (url) => typeof url === 'string' && url.startsWith('https://cdn.discordapp.com'),
}));

jest.mock('../src/qurl', () => ({
  createOneTimeLink: jest.fn(),
  deleteLink: jest.fn(),
  getResourceStatus: jest.fn(),
}));

const {
  mockPlacesModule,
  mockSearchPlaces,
  mockFindPlaceFromText,
  mockGetPlaceDetails,
} = require('./helpers/places-mock');
jest.mock('../src/places', () => mockPlacesModule);

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

const commands = require('../src/commands');
const { _test } = commands;
const logger = require('../src/logger');
const { sendDM: mockSendDM } = require('../src/discord');
const { mintLinks: mockMintLinks } = require('../src/connector');
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
  handleQurlDetect,
  detectCooldowns,
  isOnDetectCooldown,
  sweepCooldowns,
  DETECT_NO_MATCH_MSG,
} = _test;

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
      list: jest.fn(async () => new Map()),
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
  jest.clearAllMocks();
  sendCooldowns.clear();
  detectCooldowns.clear();
  mockDetectWatermark.mockReset();
  mockDb.findSendsByQurlId.mockReset().mockResolvedValue([]);
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

describe('selfDestructOptionToSeconds', () => {
  test.each([
    ['none', null],
    [SELF_DESTRUCT_NO_TIMER_CHOICE, null],
    [null, null],
    [undefined, null],
    ['', null],
    ['0.5', 0.5],  // "1/2 second" preset — Math.floor would have downgraded this to "no timer"
    ['1', 1],
    ['5', 5],
    ['30', 30],
    ['300', 300],
    ['1800', 1800],
    ['3600', 3600],
    ['60', null],
    ['7200', null],
    ['999999999', null],
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
    const dup = makeUser('100000000000000001');
    const r = partitionRecipients([dup, dup, makeUser('100000000000000002')], SENDER_ID);
    expect(r.valid.length).toBe(3);
  });

  test('excludeSender:true drops the sender pre-validity (selfIncluded stays false)', () => {
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
    const r = partitionRecipients([makeUser(SENDER_ID)], SENDER_ID);
    expect(r.valid.map((u) => u.id)).toEqual([SENDER_ID]);
    expect(r.selfIncluded).toBe(true);
  });
});

describe('resolveMentionableSelection', () => {
  const GUILD_ID = 'guild-1';

  function makeRole({ id, members = [], mentionable = true, name }) {
    const memberMap = new Map(members.map((m) => [m.user.id, m]));
    return [id, { id, name: name ?? `role-${id}`, members: memberMap, mentionable }];
  }

  function makeMentionableInteraction({
    pickedUsers = [],
    pickedRoles = [],
    guildMemberCache = new Map(),
    inDM = false,
  } = {}) {
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
    for (let i = 0; i < explicitUsers.length; i++) {
      expect(r.users.find((u) => u.id === explicitIds[i])).toBe(explicitUsers[i]);
    }
  });

  test('DM context (no guild) → empty users, no denial flag (no roles surface at all)', () => {
    const int = makeMentionableInteraction({ inDM: true });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.users).toEqual([]);
    expect(r.massMentionDenied).toBe(false);
  });

  test('pathological all-bot role iteration bounded at exactly 4× cap (=100, pins the multiplier)', () => {
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
    expect(r.droppedFromRoles).toBe(ITER_BOUND);
  });

  test('ITER_BOUND accumulates ACROSS roles (function-scoped, not per-role)', () => {
    const config = require('../src/config');
    const ITER_BOUND = 4 * config.QURL_SEND_MAX_RECIPIENTS;
    const botsA = Array.from({ length: 100 }, (_, i) => ({
      user: makeUser(`100000000000000${String(i).padStart(3, '0')}`, { bot: true }),
    }));
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
    expect(r.users).toEqual([]);
    expect(r.droppedFromRoles).toBe(ITER_BOUND);
  });

  test('iteration cost vs count semantic: counter-before-dedupe causes overlap to consume an iter slot, blocking a later human', () => {
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
    expect(r.users).toEqual([]);
    expect(r.droppedFromRoles).toBe(99);
  });

  test('overlap dedup: same bot in two picked roles counted once in droppedFromRoles', () => {
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
    const u1Picked = makeUser('100000000000000001');
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
    expect(r.users[0]).toBe(u1Picked);
    expect(r.users[0]).not.toBe(u1FromRole);
  });

  test('directly-picked bot + same bot in role → droppedFromRoles 0 (partition reports it via droppedBots)', () => {
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
    expect(r.users.map((u) => u.id)).toEqual([bot1.id]);
    expect(r.droppedFromRoles).toBe(0);
  });

  test('bot-only role pick → droppedFromRoles counts the filtered bots', () => {
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
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ id: GUILD_ID, members: [] })],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: true });
    expect(r.users).toEqual([]);
    expect(r.everyoneCacheCold).toBe(true);
    expect(r.massMentionDenied).toBe(false);
  });

  test('everyoneCacheCold: @everyone WITH perm but EMPTY guild.members.cache → flag set (cache defined but no entries)', () => {
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ id: GUILD_ID, members: [] })],
      guildMemberCache: new Map(), // defined but size === 0
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: true });
    expect(r.users).toEqual([]);
    expect(r.everyoneCacheCold).toBe(true);
  });

  test('everyoneCacheCold stays false when @everyone is DENIED (cache state irrelevant)', () => {
    const int = makeMentionableInteraction({
      pickedRoles: [makeRole({ id: GUILD_ID, members: [] })],
    });
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(r.massMentionDenied).toBe(true);
    expect(r.everyoneCacheCold).toBe(false);
  });

  test('defense: role member with undefined .user is skipped (partial GuildMember from sparse fetch)', () => {
    const u1 = makeUser('100000000000000001');
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
    const int = makeMentionableInteraction({});
    const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
    expect(Object.keys(r).sort()).toEqual(['droppedFromRoles', 'everyoneCacheCold', 'massMentionDenied', 'roleMentionsDenied', 'users']);
  });

  describe('role-mention permission gate (#326)', () => {

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
      const int = makeMentionableInteraction({
        pickedRoles: [['orphan-id', undefined]],
      });
      const r = resolveMentionableSelection({ interaction: int, canMentionEveryone: false });
      expect(r.users).toEqual([]);
      expect(r.roleMentionsDenied).toEqual([]);
      expect(r.massMentionDenied).toBe(false);
    });

    test('@everyone-role (role.id === guild.id) NOT routed to roleMentionsDenied — uses massMentionDenied', () => {
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

describe('clearCooldown', () => {
  beforeEach(() => sendCooldowns.clear());

  test('removes the cooldown entry entirely', () => {
    sendCooldowns.set('u1', Date.now());
    clearCooldown('u1');
    expect(sendCooldowns.has('u1')).toBe(false);
    expect(isOnCooldown('u1')).toBe(false);
  });

  test('no-op when no cooldown is set', () => {
    clearCooldown('u1');
    expect(sendCooldowns.has('u1')).toBe(false);
  });
});

describe('setCooldown LRU iteration order', () => {
  beforeEach(() => sendCooldowns.clear());

  test('re-setting an existing key moves it to the end of iteration order', () => {
    setCooldown('uA');
    setCooldown('uB');
    setCooldown('uC');
    expect(Array.from(sendCooldowns.keys())).toEqual(['uA', 'uB', 'uC']);
    setCooldown('uA');
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
    const url = 'https://www.google.com/maps/search/?api=1&query=Eiffel+Tower&query_place_id=ChIJxxx';
    const r = parseLocationInput(url);
    expect(r.locationUrl).toBe(url);
    expect(r.locationName).toBe('Eiffel Tower');
  });

  test('place_id sentinel parses into a placeId branch (no URL synthesized)', () => {
    const r = parseLocationInput('qurl_place:ChIJ37FjGE63t4kRD2_jXSF1F9o');
    expect(r.placeId).toBe('ChIJ37FjGE63t4kRD2_jXSF1F9o');
    expect(r.locationUrl).toBeNull();
    expect(r.locationName).toBeNull();
  });

  test('plain place name returns text branch for server-side resolution', () => {
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
    const r = parseLocationInput('https://evil.example.com/maps/place/x');
    expect(r.locationUrl).toBeNull();
    expect(r.text).toBe('https://evil.example.com/maps/place/x');
  });

  test('malformed %-encoding in the input does not throw', () => {
    expect(() => parseLocationInput('https://www.google.com/maps/place/%ZZ-broken')).not.toThrow();
  });

  test('spoofed host (google.com.evil.com) fails the regex AND falls through to text branch', () => {
    const spoofed = 'https://google.com.evil.com/maps/place/Eiffel-Tower';
    const r = parseLocationInput(spoofed);
    expect(r.locationUrl).toBeNull();
    expect(r.text).toBe(spoofed);
  });
});

describe('resolveLocation', () => {
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
  beforeEach(() => {
    mockSearchPlaces.mockReset().mockResolvedValue([]);
    _resetAutocompleteFailureBurst();
  });

  function makeAutocompleteInteraction({
    subcommand = 'map',
    focused = { name: 'location', value: 'whitehouse' },
    guildId = 'guild-1',
    authorizingIntegrationOwners,
  } = {}) {
    const respond = jest.fn().mockResolvedValue(undefined);
    return {
      commandName: 'qurl',
      guildId,
      authorizingIntegrationOwners,
      respond,
      options: {
        getSubcommand: () => subcommand,
        getFocused: () => focused,
      },
    };
  }

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
    const int = makeAutocompleteInteraction({ guildId: null });
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
    expect(mockSearchPlaces).not.toHaveBeenCalled();
  });

  test('responds empty for user-install autocomplete invoked inside a guild', async () => {
    const int = makeAutocompleteInteraction({
      authorizingIntegrationOwners: { 1: 'user-1' },
    });
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
    const int = makeAutocompleteInteraction({ focused: { name: 'location', value: 'a' } });
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
    expect(mockSearchPlaces).not.toHaveBeenCalled();
  });

  test('skips Places call when input already looks like a URL', async () => {
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
    expect(choices[0].name).not.toBe(choices[1].name);
  });

  test('truncates a label exceeding the 100-char Discord cap (UTF-16 units)', async () => {
    const longAddress = '1234 Very Long Street Name, Somewhere Far Away, In A Large City With A Long Name, Region, Country 99999';
    mockSearchPlaces.mockResolvedValueOnce([{ placeId: fakePlaceId('longlabel'), name: 'Place', address: longAddress }]);
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    const choice = int.respond.mock.calls[0][0][0];
    expect(choice.name.length).toBeLessThanOrEqual(100);
    expect(choice.value.length).toBeLessThanOrEqual(100);
  });

  test('boundary — exactly 100 UTF-16 units ending in a lone high surrogate gets backed off', async () => {
    const malformed = 'a'.repeat(99) + '\uD83D'; // lone high surrogate at index 99 → length 100
    expect(malformed.length).toBe(100);
    mockSearchPlaces.mockResolvedValueOnce([{ placeId: fakePlaceId('boundary1'), name: malformed, address: '' }]);
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    const choice = int.respond.mock.calls[0][0][0];
    expect(choice.name.length).toBe(99);
    const loneHigh = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])/;
    expect(choice.name).not.toMatch(loneHigh);
  });

  test('truncation does not split a surrogate pair (emoji-heavy label stays valid UTF-16)', async () => {
    const emoji = '🏛️'; // 🏛 + variation selector — 3 UTF-16 units
    const name = (emoji + 'X').repeat(40); // 160 UTF-16 units of mixed surrogate + ASCII
    mockSearchPlaces.mockResolvedValueOnce([{ placeId: fakePlaceId('emojiplace'), name, address: '' }]);
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    const choice = int.respond.mock.calls[0][0][0];
    expect(choice.name.length).toBeLessThanOrEqual(100);
    const loneHigh = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])/;
    expect(choice.name).not.toMatch(loneHigh);
  });

  test('drops a choice whose value would exceed the 100-char Discord cap', async () => {
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
    const int = makeAutocompleteInteraction({ guildId: null });
    let respondCallCount = 0;
    int.respond = jest.fn(async () => {
      respondCallCount += 1;
      if (respondCallCount === 1) throw new Error('Unknown interaction');
    });
    await handleAutocomplete(int);
    expect(respondCallCount).toBe(2);
  });

  test('drops a choice whose place_id fails the documented shape check', async () => {
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
    mockSearchPlaces.mockRejectedValueOnce(new Error('Places API status: OVER_QUERY_LIMIT'));
    const int = makeAutocompleteInteraction();
    await handleAutocomplete(int);
    expect(int.respond).toHaveBeenCalledWith([]);
  });

  test('failure burst counter emits one warn per BURST failures (SRE outage signal)', async () => {
    const logger = require('../src/logger');
    logger.warn.mockClear();
    for (let i = 0; i < AUTOCOMPLETE_FAILURE_LOG_BURST - 1; i++) {
      mockSearchPlaces.mockRejectedValueOnce(new Error('Places API status: UNKNOWN_ERROR'));
      await handleAutocomplete(makeAutocompleteInteraction());
    }
    const burstWarns = () => logger.warn.mock.calls.filter(
      (call) => call[0] === 'autocomplete handler failure burst',
    ).length;
    expect(burstWarns()).toBe(0);

    mockSearchPlaces.mockRejectedValueOnce(new Error('Places API status: UNKNOWN_ERROR'));
    await handleAutocomplete(makeAutocompleteInteraction());
    expect(burstWarns()).toBe(1);
    expect(logger.warn).toHaveBeenCalledWith(
      'autocomplete handler failure burst',
      expect.objectContaining({ count: AUTOCOMPLETE_FAILURE_LOG_BURST }),
    );

    for (let i = 0; i < AUTOCOMPLETE_FAILURE_LOG_BURST - 1; i++) {
      mockSearchPlaces.mockRejectedValueOnce(new Error('Places API status: UNKNOWN_ERROR'));
      await handleAutocomplete(makeAutocompleteInteraction());
    }
    expect(burstWarns()).toBe(1);
  });

  test('failure burst counter does not increment when the early-return respond() throws', async () => {
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
  beforeEach(() => sendCooldowns.clear());

  test('setCooldown via one user blocks isOnCooldown for the same user across all entry points', () => {
    setCooldown('uA');
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
    const tokens = ['```\n[evil](https://phish.example)\n```', '`code`', 'plain'];
    const out = renderRecipientWarnings({
      invalidTokens: tokens, cappedCount: 0, unresolvedIds: [],
      droppedBots: 0,
    });
    const fenceContent = out.split('```')[1] || '';
    expect(fenceContent).not.toMatch(/`/);
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
    expect(out).not.toMatch(/yourself/);
  });

  test('transientFailureIds rendered with neutral copy (not "left the server")', () => {
    const out = renderRecipientWarnings({
      transientFailureIds: ['100000000000000001', '100000000000000002'],
    });
    expect(out).toMatch(/2 user.*couldn't be looked up.*try again/);
    expect(out).not.toMatch(/no longer in this server/);
  });

  test('renderRecipientWarnings tolerates missing fields via defaults', () => {
    expect(renderRecipientWarnings({})).toBe('');
    expect(renderRecipientWarnings()).toBe('');
  });

  test('caps each shown invalidToken at 80 codepoints with an ellipsis indicator', () => {
    const longToken = 'a'.repeat(200);
    const out = renderRecipientWarnings({ invalidTokens: [longToken] });
    const fence = out.split('```')[1] || '';
    expect(fence).toMatch(/a{80}…/);
    expect(fence).not.toMatch(/a{81}/);
  });

  test('does NOT add ellipsis when the token already fits under the cap', () => {
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
    const out = renderConfirmCardContent({ ...baseProps, selfIncluded: true });
    expect(out).toMatch(/Send includes you/);
    expect(out).not.toMatch(/Some recipients were dropped/);
  });

  test('selfIncluded omitted (default false) → no notice', () => {
    const out = renderConfirmCardContent(baseProps);
    expect(out).not.toMatch(/Send includes/);
  });

  test('voice-mode + selfIncluded=true → notice suppressed (forged/drifted payload defense)', () => {
    const u1 = { id: '100000000000000001', username: 'alice' };
    const out = renderConfirmCardContent({
      ...baseProps,
      validRecipients: [u1],
      selfIncluded: true,
      recipientMode: 'voice',
      voiceChannelId: 'voice-ch',
    });
    expect(out).not.toMatch(/Send includes you/);
    expect(out).toMatch(/<#voice-ch>/);
  });

  test('personal-message preview cap at 80 chars, rendered as blockquote', () => {
    const long = 'x'.repeat(120);
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: long });
    expect(out).toMatch(/> x{80}…/);
    expect(out).not.toMatch(/"x{80}…"/);
  });

  test('personal-message preview backs off the cut when it would land on a markdown escape', () => {
    const safePrefix = 'a'.repeat(79);  // 79 chars
    const message = safePrefix + '\\*' + 'rest'.repeat(10);  // 79+2+40 = 121 chars
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: message });
    expect(out).toContain(`> ${safePrefix}…`);
    expect(out).not.toMatch(/\\…/);
  });

  test('personal-message preview slices by codepoint, not UTF-16 code units (surrogate-pair safe)', () => {
    const message = 'a'.repeat(79) + '\u{1F600}' + 'rest'.repeat(20);
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: message });
    const lone = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/;
    expect(out).not.toMatch(lone);
  });

  test('personal-message preview back-off handles odd-count multi-backslash boundary', () => {
    const prefix = 'a'.repeat(77);  // 77 chars
    const message = prefix + '\\\\\\*' + 'rest'.repeat(20);  // 77+4+80 = 161 chars
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: message });
    expect(out).toContain(`> ${prefix}\\\\…`);
    expect(out).not.toMatch(/\\\\\\…/);
  });

  test('personal-message renders pre-sanitized input verbatim (no double-escape)', () => {
    const presanitized = '\\*\\*emphasis\\*\\*';
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: presanitized });
    expect(out).toContain(`> ${presanitized}`);
    expect(out).not.toMatch(/\\\\\*/);
  });

  test('personal-message with mixed markdown + escape sequences renders without re-escaping the escapes', () => {
    const presanitized = '\\*\\*bold\\*\\* \\\\n \\[link\\]\\(https://evil\\)';
    const out = renderConfirmCardContent({ ...baseProps, personalMessage: presanitized });
    expect(out).toContain(`> ${presanitized}`);
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
    const u = makeUser('100000000000000001', { username: 'alice' });
    const out = renderConfirmCardContent({ ...baseProps, validRecipients: [u] });
    expect(out).toMatch(/alice/);
  });

  test('personal-message blockquote collapses embedded newlines + unicode line/paragraph separators to spaces', () => {
    const out = renderConfirmCardContent({
      ...baseProps,
      personalMessage: 'first line\nsecond\r\nthird fourth fifth',
    });
    expect(out).toContain('> first line second third fourth fifth\n');
  });

  test('caps total rendered content below Discord\'s 2000-char limit + adds truncation indicator', () => {
    const bigWarnings = 'WARN ' + 'x'.repeat(3000) + '\n\n';
    const out = renderConfirmCardContent({
      ...baseProps,
      warningsBlock: bigWarnings,
    });
    expect(out.length).toBeLessThanOrEqual(2000);
    expect(out).toMatch(/…\(truncated\)$/);
  });

  test('does NOT truncate when content fits under the cap (referential-equality fast path)', () => {
    const out = renderConfirmCardContent({ ...baseProps });
    expect(out).not.toMatch(/…\(truncated\)/);
    expect(out.length).toBeLessThan(2000);
  });
});

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
    const originalActive = getActiveFileSends();
    try {
      setActiveFileSends(99);  // any value ≥ MAX_CONCURRENT_FILE_SENDS
      const int = makeInteraction({ options: { attachment: VALID_ATTACHMENT } });
      await handleQurlSend(int);
      expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
        content: expect.stringMatching(/too many file sends/i),
        ephemeral: true,
      }));
      expect(isOnCooldown(SENDER_ID)).toBe(false);
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
    expect(payload.recipientAliases).toEqual(
      expect.objectContaining({ [u1]: expect.any(String), [u2]: expect.any(String) })
    );
    expect(payload).toHaveProperty('warningsBlock');
    expect(int.editReply).toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Sending file/);
    expect(reply.content).toMatch(/x\.png/);
    expect(reply.components.length).toBeGreaterThan(0);
  });

  test('/qurl map slash entry persists recipientAliases (parity with /qurl send)', async () => {
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
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@${SENDER_ID}>` },
      guildMembers: { [SENDER_ID]: {} },
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Send includes you/);
    expect(reply.content).not.toMatch(/No valid recipients/);
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.selfIncluded).toBe(true);
    expect(payload.recipientIds).toEqual([SENDER_ID]);
  });

  test('DM context (no guild) suppresses the @everyone permission warning', async () => {
    const int = makeInteraction({
      guildId: null, // → guild = null in makeInteraction
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${SENDER_ID}>` },
    });
    await handleQurlSend(int);
    const calls = int.editReply.mock.calls;
    for (const [arg] of calls) {
      expect(arg.content || '').not.toMatch(/Mention Everyone permission/);
    }
  });

  test('guild context + no MENTION_EVERYONE → @everyone warning renders + Alice still parses', async () => {
    const aliceId = '400000000000000001';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const lastEdit = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastEdit.content).toMatch(/Mention Everyone\b/);
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.recipientIds).toEqual([aliceId]);
  });

  test('text path: <@&roleId> for non-mentionable role WITHOUT MENTION_EVERYONE → warning with role name, no expansion', async () => {
    const aliceId = '400000000000000010';
    const bobId = '400000000000000011';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@${aliceId}> <@&7000>` },
      guildMembers: { [aliceId]: {}, [bobId]: {} },
    });
    int.guild.roles.cache.set('7000', {
      id: '7000',
      name: 'admin-team',
      mentionable: false,
      members: new Map([[bobId, { user: { id: bobId, bot: false } }]]),
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.recipientIds).toEqual([aliceId]);
    const lastEdit = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastEdit.content).toMatch(/@admin-team/);
    expect(lastEdit.content).toMatch(/Mention Everyone/);
    expect(lastEdit.content).toMatch(/role\.mentionable: true/);
  });

  test('text path: every recipient denied-role-only → "no valid recipients" but NOT misleading bot-only log nor transient-retry copy', async () => {
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
    expect(reply.content).toMatch(/@private-team/);
    expect(reply.content).toMatch(/No valid recipients/);
    expect(reply.content).not.toMatch(/Could not look up recipients right now/);
    const infoLogCalls = logger.info.mock.calls.filter(
      ([msg]) => typeof msg === 'string' && msg.includes('bot-only-or-self mention list'),
    );
    expect(infoLogCalls).toEqual([]);
  });

  test('text path: <@&roleId> for non-mentionable role WITH MENTION_EVERYONE → expands normally, no warning', async () => {
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
    expect(lastEdit.content).not.toMatch(/Mention Everyone permission/);
    const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
    expect(payload.recipientIds.sort()).toEqual([aliceId, bobId].sort());
  });

  test('all mentioned recipients hit transient lookup failure → retry copy, not "no valid recipients"', async () => {
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
    expect(reply.content).toMatch(/bot/);
  });

  test('inner catch on supersedeOrCreate throw clears cooldown + surfaces specific error', async () => {
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
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/unexpected throw/),
      expect.objectContaining({ flow_id: expect.any(String) }),
    );
  });

  test('safety-net catch does NOT call deleteFlow when throw fires before supersedeOrCreate', async () => {
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

    const VOICE_CH = 'voice-ch-slash-1';
    const u1 = '100000000000000011';
    const u2 = '100000000000000012';
    const bot = '100000000000000099';

    function makeVoiceEntryInteraction({ members = [], botIds = [] } = {}) {
      const chanMembers = new Map();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT },
      });
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
      const config = require('../src/config');
      const originalCap = config.QURL_SEND_MAX_RECIPIENTS;
      config.QURL_SEND_MAX_RECIPIENTS = 1;  // force over-cap with 2 members
      try {
        const int = makeVoiceEntryInteraction({ members: [u1, u2] });
        await handleQurlSend(int);
        const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
        expect(payload.recipientMode).toBe('picker');
        expect(payload.recipientIds).toEqual([]);
        expect(payload.warningsBlock).toMatch(/Voice channel has 2 eligible recipients/);
        expect(payload.warningsBlock).toMatch(/max 1/);
      } finally {
        config.QURL_SEND_MAX_RECIPIENTS = originalCap;
      }
    });

    test('voice channel cache miss → picker-mode with "Couldn\'t read voice channel" banner', async () => {
      const int = makeInteraction({ options: { attachment: VALID_ATTACHMENT } });
      int.channel = { id: VOICE_CH, type: 2 };
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('picker');
      expect(payload.warningsBlock).toMatch(/Couldn't read voice channel members/);
    });

    test('explicit `recipients:` overrides voice-mode default (manual selection wins)', async () => {
      const int = makeVoiceEntryInteraction({ members: [u1, u2] });
      int.options.getString.mockImplementation((key) =>
        (key === 'recipients' ? `<@${u1}>` : null)
      );
      await handleQurlSend(int);
      const payload = mockSupersedeOrCreate.mock.calls[0][0].payload;
      expect(payload.recipientMode).toBe('picker');
      expect(payload.recipientIds).toEqual([u1]);
    });

    test('text `@everyone` in recipients → EVERYONE mode (picker hidden, no auto-fill)', async () => {
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
      expect(payload.selfIncluded).toBe(true);
      expect(payload.recipientIds.length).toBeGreaterThanOrEqual(2);
    });

    test('text `@everyone` with sender MISSING from members.cache post-prewarm → selfIncluded:false (documented divergence from button-click path)', async () => {
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '@everyone' },
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
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '@everyone' },
        guildMembers: { [SENDER_ID]: {} },
      });
      await handleQurlSend(int);
      const supersedeCalls = mockSupersedeOrCreate.mock.calls;
      expect(supersedeCalls.length).toBe(0);
    });
  });
});

describe('handleQurlDetect', () => {
  const VALID_IMAGE = Object.freeze({
    url: 'https://cdn.discordapp.com/attachments/1/2/shot.png',
    name: 'shot.png',
    contentType: 'image/png',
    size: 2048,
  });

  function makeDetectInteraction({
    guildId = 'guild-1',
    userId = SENDER_ID,
    image = VALID_IMAGE,
    usersFetch,
    omitImage = false,
  } = {}) {
    const base = makeInteraction({ guildId, userId, options: { image } });
    if (omitImage) {
      base.options.getAttachment = jest.fn((name, required) => {
        if (required) throw new Error(`Required option "${name}" not found`);
        return null;
      });
    }
    base.deferReply = jest.fn(async () => { base.deferred = true; });
    base.client = {
      users: {
        fetch: usersFetch || jest.fn(async (id) => ({ id, username: `recip_${id.slice(-3)}` })),
      },
    };
    return base;
  }

  let originalFetch;
  beforeEach(() => {
    originalFetch = global.fetch;
    global.fetch = jest.fn().mockResolvedValue({
      ok: true,
      status: 200,
      arrayBuffer: async () => new Uint8Array([1, 2, 3]).buffer,
    });
    mockDb.getGuildApiKey.mockResolvedValue(null); // → falls back to config.QURL_API_KEY
  });
  afterEach(() => {
    global.fetch = originalFetch;
  });

  test('rejects in DM context (no oracle outside a guild) — no cooldown, no connector call', async () => {
    const int = makeDetectInteraction({ guildId: null });
    await handleQurlDetect(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/in a server/),
      ephemeral: true,
    }));
    expect(mockDetectWatermark).not.toHaveBeenCalled();
  });

  test('rejects a non-image attachment (cooldown CLEARED — honest user error)', async () => {
    const int = makeDetectInteraction({ image: { ...VALID_IMAGE, contentType: 'application/pdf' } });
    await handleQurlDetect(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/needs an image/i),
      ephemeral: true,
    }));
    expect(mockDetectWatermark).not.toHaveBeenCalled();
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(false);
  });

  test('rejects a non-Discord-CDN attachment url (SSRF gate — cooldown PRESERVED + rejected audit)', async () => {
    const int = makeDetectInteraction({ image: { ...VALID_IMAGE, url: 'https://evil.example/x.png' } });
    await handleQurlDetect(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/source not allowed/i),
      ephemeral: true,
    }));
    expect(mockDetectWatermark).not.toHaveBeenCalled();
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(true);
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'rejected', guild_id: 'guild-1', requester_id: SENDER_ID,
    }));
    const auditMeta = logger.audit.mock.calls.find((c) => c[0] === 'qurl_detect')[1];
    expect('recipient_discord_id' in auditMeta).toBe(false);
  });

  test('per-(guild,user) cooldown blocks a second detect in the same guild', async () => {
    const int1 = makeDetectInteraction();
    mockDetectWatermark.mockResolvedValue({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
    await handleQurlDetect(int1);
    expect(int1.deferReply).toHaveBeenCalled();

    const int2 = makeDetectInteraction();
    await handleQurlDetect(int2);
    expect(int2.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/wait before running detect/i),
      ephemeral: true,
    }));
    expect(int2.deferReply).not.toHaveBeenCalled();
    expect(mockDetectWatermark).toHaveBeenCalledTimes(1);
  });

  test('cooldown is per-(guild,user): a different guild is NOT throttled', async () => {
    mockDetectWatermark.mockResolvedValue({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
    await handleQurlDetect(makeDetectInteraction({ guildId: 'guild-A' }));
    const intB = makeDetectInteraction({ guildId: 'guild-B' });
    await handleQurlDetect(intB);
    expect(intB.deferReply).toHaveBeenCalled();
    expect(mockDetectWatermark).toHaveBeenCalledTimes(2);
  });

  test('(i) detected ⇒ replies (ephemerally) with the recipient handle + % match', async () => {
    const recipientId = '100000000000000007';
    mockDetectWatermark.mockResolvedValue({
      detected: true, qurl_id: 'q_hit1', match_pct: 92, confidence: 0.98,
    });
    mockDb.findSendsByQurlId.mockResolvedValue([
      { qurl_id: 'q_hit1', recipient_discord_id: recipientId, guild_id: 'guild-1', sender_discord_id: SENDER_ID },
    ]);
    const usersFetch = jest.fn(async (id) => ({ id, username: 'AliceRecipient' }));
    const int = makeDetectInteraction({ usersFetch });

    await handleQurlDetect(int);

    expect(mockDetectWatermark).toHaveBeenCalledWith(
      expect.any(Buffer),
      expect.objectContaining({ guildId: 'guild-1', contentType: 'image/png' }),
    );
    expect(mockDb.findSendsByQurlId).toHaveBeenCalledWith('q_hit1');
    expect(usersFetch).toHaveBeenCalledWith(recipientId);
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('AliceRecipient'),
    }));
    const matchedReply = int.editReply.mock.calls.at(-1)[0];
    const replyContent = matchedReply.content;
    expect(replyContent).toContain(`<@${recipientId}>`);
    expect(replyContent).toMatch(/92% match/);
    expect(replyContent).not.toMatch(/confidence/i);
    expect(matchedReply.allowedMentions).toEqual({ parse: [] });
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'matched', qurl_id: 'q_hit1', match_pct: 92, guild_id: 'guild-1', confidence: 0.98, grant_basis: 'sender',
    }));
    const auditMeta = logger.audit.mock.calls.find((c) => c[0] === 'qurl_detect')[1];
    expect(JSON.stringify(auditMeta)).not.toContain(recipientId);
  });

  test('matched with a non-positive match_pct ⇒ bare attribution, no "0% match" (item 3)', async () => {
    const recipientId = '100000000000000011';
    mockDetectWatermark.mockResolvedValue({
      detected: true, qurl_id: 'q_zero', match_pct: 0, confidence: 0.9,
    });
    mockDb.findSendsByQurlId.mockResolvedValue([
      { qurl_id: 'q_zero', recipient_discord_id: recipientId, guild_id: 'guild-1', sender_discord_id: SENDER_ID },
    ]);
    const int = makeDetectInteraction({
      usersFetch: jest.fn(async (id) => ({ id, username: 'BobRecipient' })),
    });

    await handleQurlDetect(int);

    const replyContent = int.editReply.mock.calls.at(-1)[0].content;
    expect(replyContent).toContain('BobRecipient');
    expect(replyContent).not.toMatch(/match/i); // no "% match" suffix
    expect(replyContent).not.toContain('0%');
  });

  test('audit keeps the RAW match_pct while the reply rounds it (item 2)', async () => {
    const recipientId = '100000000000000013';
    mockDetectWatermark.mockResolvedValue({
      detected: true, qurl_id: 'q_frac', match_pct: 92.7, confidence: 0.97,
    });
    mockDb.findSendsByQurlId.mockResolvedValue([
      { qurl_id: 'q_frac', recipient_discord_id: recipientId, guild_id: 'guild-1', sender_discord_id: SENDER_ID },
    ]);
    const int = makeDetectInteraction({
      usersFetch: jest.fn(async (id) => ({ id, username: 'CarolRecipient' })),
    });

    await handleQurlDetect(int);

    const replyContent = int.editReply.mock.calls.at(-1)[0].content;
    expect(replyContent).toMatch(/93% match/);
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'matched', qurl_id: 'q_frac', match_pct: 92.7,
    }));
  });

  test('(ii) !detected ⇒ "no watermark found" ephemeral + no_match audit', async () => {
    mockDetectWatermark.mockResolvedValue({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
    const int = makeDetectInteraction();

    await handleQurlDetect(int);

    expect(mockDb.findSendsByQurlId).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: DETECT_NO_MATCH_MSG,
    }));
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'no_match', guild_id: 'guild-1',
    }));
  });

  test('(iii) cross-guild guard: a send whose guild_id ≠ interaction.guildId ⇒ defensive "no match"', async () => {
    const recipientId = '100000000000000009';
    mockDetectWatermark.mockResolvedValue({
      detected: true, qurl_id: 'q_otherguild', match_pct: 88, confidence: 0.91,
    });
    mockDb.findSendsByQurlId.mockResolvedValue([
      { qurl_id: 'q_otherguild', recipient_discord_id: recipientId, guild_id: 'guild-OTHER' },
    ]);
    const usersFetch = jest.fn();
    const int = makeDetectInteraction({ guildId: 'guild-1', usersFetch });

    await handleQurlDetect(int);

    expect(usersFetch).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: DETECT_NO_MATCH_MSG,
    }));
    expect(logger.audit).not.toHaveBeenCalledWith('qurl_detect', expect.objectContaining({ result: 'matched' }));
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'no_match', qurl_id: 'q_otherguild',
    }));
  });

  test('no-mark, cross-guild-filtered, and no-standing replies are ALL BYTE-IDENTICAL (no signal)', async () => {
    mockDetectWatermark.mockResolvedValueOnce({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
    const noMark = makeDetectInteraction();
    await handleQurlDetect(noMark);
    const noMarkText = noMark.editReply.mock.calls.at(-1)[0].content;

    detectCooldowns.clear(); // second invocation in the same guild/user
    mockDetectWatermark.mockResolvedValueOnce({ detected: true, qurl_id: 'q_x', match_pct: 80, confidence: 0.9 });
    mockDb.findSendsByQurlId.mockResolvedValueOnce([
      { qurl_id: 'q_x', recipient_discord_id: '100000000000000003', guild_id: 'guild-OTHER' },
    ]);
    const crossGuild = makeDetectInteraction({ guildId: 'guild-1' });
    await handleQurlDetect(crossGuild);
    const crossGuildText = crossGuild.editReply.mock.calls.at(-1)[0].content;

    detectCooldowns.clear();
    mockDetectWatermark.mockResolvedValueOnce({ detected: true, qurl_id: 'q_ns', match_pct: 90, confidence: 0.95 });
    mockDb.findSendsByQurlId.mockResolvedValueOnce([
      { qurl_id: 'q_ns', recipient_discord_id: '100000000000000004', guild_id: 'guild-1', sender_discord_id: '900000000000000777' },
    ]);
    const noStanding = makeDetectInteraction({ guildId: 'guild-1', userId: '900000000000000888' });
    await handleQurlDetect(noStanding);
    const noStandingText = noStanding.editReply.mock.calls.at(-1)[0].content;

    expect(noMarkText).toBe(crossGuildText);
    expect(noMarkText).toBe(noStandingText);
    expect(noMarkText).toBe(DETECT_NO_MATCH_MSG);
  });

  test('ambiguity guard: >1 same-guild row for one qurl_id ⇒ refuses to attribute', async () => {
    mockDetectWatermark.mockResolvedValue({
      detected: true, qurl_id: 'q_dup', match_pct: 90, confidence: 0.95,
    });
    mockDb.findSendsByQurlId.mockResolvedValue([
      { qurl_id: 'q_dup', recipient_discord_id: '100000000000000001', guild_id: 'guild-1' },
      { qurl_id: 'q_dup', recipient_discord_id: '100000000000000002', guild_id: 'guild-1' },
    ]);
    const usersFetch = jest.fn();
    const int = makeDetectInteraction({ guildId: 'guild-1', usersFetch });
    int.memberPermissions = { has: jest.fn(() => true) };

    await handleQurlDetect(int);

    expect(usersFetch).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/single recipient/i),
    }));
    expect(logger.audit).not.toHaveBeenCalledWith('qurl_detect', expect.objectContaining({ result: 'matched' }));
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'ambiguous', qurl_id: 'q_dup', guild_id: 'guild-1',
    }));
  });

  test('standing: a STAFF caller (not the sender) gets the reveal (grant_basis staff)', async () => {
    const recipientId = '100000000000000021';
    mockDetectWatermark.mockResolvedValue({ detected: true, qurl_id: 'q_staff', match_pct: 95, confidence: 0.97 });
    mockDb.findSendsByQurlId.mockResolvedValue([
      { qurl_id: 'q_staff', recipient_discord_id: recipientId, guild_id: 'guild-1', sender_discord_id: '900000000000000777' },
    ]);
    const usersFetch = jest.fn(async (id) => ({ id, username: 'LeakedRecipient' }));
    const int = makeDetectInteraction({ guildId: 'guild-1', userId: '900000000000000801', usersFetch });
    int.memberPermissions = { has: jest.fn(() => true) };

    await handleQurlDetect(int);

    expect(usersFetch).toHaveBeenCalledWith(recipientId);
    const reply = int.editReply.mock.calls.at(-1)[0].content;
    expect(reply).toContain('LeakedRecipient');
    expect(reply).toMatch(/95% match/);
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'matched', qurl_id: 'q_staff', grant_basis: 'staff',
    }));
  });

  test('standing: a general member (not sender, not staff) is DENIED — byte-identical no-match + audit no_standing', async () => {
    const recipientId = '100000000000000022';
    mockDetectWatermark.mockResolvedValue({ detected: true, qurl_id: 'q_deny', match_pct: 95, confidence: 0.97 });
    mockDb.findSendsByQurlId.mockResolvedValue([
      { qurl_id: 'q_deny', recipient_discord_id: recipientId, guild_id: 'guild-1', sender_discord_id: '900000000000000777' },
    ]);
    const usersFetch = jest.fn();
    const int = makeDetectInteraction({ guildId: 'guild-1', userId: '900000000000000802', usersFetch });

    await handleQurlDetect(int);

    expect(usersFetch).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({ content: DETECT_NO_MATCH_MSG }));
    expect(logger.audit).not.toHaveBeenCalledWith('qurl_detect', expect.objectContaining({ result: 'matched' }));
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'no_standing', qurl_id: 'q_deny', guild_id: 'guild-1',
    }));
    const auditMeta = logger.audit.mock.calls.find((c) => c[0] === 'qurl_detect' && c[1].result === 'no_standing')[1];
    expect(JSON.stringify(auditMeta)).not.toContain(recipientId);
  });

  test('standing+ambiguity: a no-standing caller on an AMBIGUOUS qurl_id gets byte-identical no-match, NOT "couldn\'t determine"', async () => {
    mockDetectWatermark.mockResolvedValue({ detected: true, qurl_id: 'q_dup2', match_pct: 90, confidence: 0.95 });
    mockDb.findSendsByQurlId.mockResolvedValue([
      { qurl_id: 'q_dup2', recipient_discord_id: '100000000000000001', guild_id: 'guild-1', sender_discord_id: '900000000000000777' },
      { qurl_id: 'q_dup2', recipient_discord_id: '100000000000000002', guild_id: 'guild-1', sender_discord_id: '900000000000000778' },
    ]);
    const int = makeDetectInteraction({ guildId: 'guild-1', userId: '900000000000000803' });

    await handleQurlDetect(int);

    const reply = int.editReply.mock.calls.at(-1)[0].content;
    expect(reply).toBe(DETECT_NO_MATCH_MSG);
    expect(reply).not.toMatch(/single recipient/i);
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'no_standing', qurl_id: 'q_dup2',
    }));
    expect(logger.audit).not.toHaveBeenCalledWith('qurl_detect', expect.objectContaining({ result: 'ambiguous' }));
  });

  test('connector 5xx ⇒ graceful ephemeral error, no throw, cooldown CLEARED (transient)', async () => {
    const err = new Error('Connector detect failed (503)'); err.status = 503;
    mockDetectWatermark.mockRejectedValue(err);
    const int = makeDetectInteraction();

    await expect(handleQurlDetect(int)).resolves.not.toThrow();
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/unavailable right now/i),
    }));
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(false);
    expect(logger.audit).not.toHaveBeenCalledWith('qurl_detect', expect.objectContaining({ result: 'rate_limited' }));
  });

  test('connector 429 ⇒ rate-limited ephemeral AND cooldown KEPT (back off)', async () => {
    const err = new Error('Connector detect failed (429)'); err.status = 429;
    mockDetectWatermark.mockRejectedValue(err);
    const int = makeDetectInteraction();

    await expect(handleQurlDetect(int)).resolves.not.toThrow();
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/rate-limited/i),
    }));
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(true);
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'rate_limited', guild_id: 'guild-1',
    }));
  });

  test('rejects an oversize image (cooldown CLEARED — honest user error)', async () => {
    const int = makeDetectInteraction({ image: { ...VALID_IMAGE, size: 999_999_999 } });
    await handleQurlDetect(int);
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/too large/i),
      ephemeral: true,
    }));
    expect(int.deferReply).not.toHaveBeenCalled();
    expect(mockDetectWatermark).not.toHaveBeenCalled();
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(false);
  });

  test('CDN fetch !ok ⇒ "link may have expired" ephemeral, no connector call, cooldown CLEARED', async () => {
    global.fetch = jest.fn().mockResolvedValue({ ok: false, status: 403 });
    const int = makeDetectInteraction();

    await handleQurlDetect(int);

    expect(int.deferReply).toHaveBeenCalled();
    expect(mockDetectWatermark).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/may have expired/i),
    }));
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(false);
  });

  test('CDN fetch THROWS (timeout/DNS/network) ⇒ "couldn\'t download" — NOT "detection unavailable" (item 3)', async () => {
    global.fetch = jest.fn().mockRejectedValue(new Error('fetch timed out'));
    const int = makeDetectInteraction();

    await expect(handleQurlDetect(int)).resolves.not.toThrow();

    expect(int.deferReply).toHaveBeenCalled();
    expect(mockDetectWatermark).not.toHaveBeenCalled();
    const replyContent = int.editReply.mock.calls.at(-1)[0].content;
    expect(replyContent).toMatch(/could not download|couldn't download|re-upload/i);
    expect(replyContent).not.toMatch(/detection is unavailable/i);
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(false);
  });

  test('realized buffer over cap (TOCTOU: metadata small, body huge) ⇒ "too large", no connector call (item 4)', async () => {
    const MAX = 25 * 1024 * 1024;
    global.fetch = jest.fn().mockResolvedValue({
      ok: true,
      status: 200,
      arrayBuffer: async () => new ArrayBuffer(MAX + 1),
    });
    const int = makeDetectInteraction({ image: { ...VALID_IMAGE, size: 1024 } });

    await handleQurlDetect(int);

    expect(int.deferReply).toHaveBeenCalled();
    expect(mockDetectWatermark).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/too large/i),
    }));
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(false);
  });

  test('no API key configured ⇒ "not configured" ephemeral AND cooldown CLEARED (item 2)', async () => {
    const config = require('../src/config');
    const savedKey = config.QURL_API_KEY;
    config.QURL_API_KEY = '';
    mockDb.getGuildApiKey.mockResolvedValue(null);
    const int = makeDetectInteraction();

    try {
      await handleQurlDetect(int);
    } finally {
      config.QURL_API_KEY = savedKey;
    }

    expect(int.deferReply).toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/not configured/i),
    }));
    expect(mockDetectWatermark).not.toHaveBeenCalled();
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(false);
    expect(logger.audit).toHaveBeenCalledWith('qurl_detect', expect.objectContaining({
      result: 'unconfigured', guild_id: 'guild-1', requester_id: SENDER_ID,
    }));
    const auditMeta = logger.audit.mock.calls.find((c) => c[0] === 'qurl_detect')[1];
    expect('recipient_discord_id' in auditMeta).toBe(false);
  });

  test('required image option missing ⇒ "image option is required" + cooldown CLEARED', async () => {
    const int = makeDetectInteraction({ omitImage: true });

    await handleQurlDetect(int);

    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/`image:` option is required/i),
      ephemeral: true,
    }));
    expect(int.deferReply).not.toHaveBeenCalled();
    expect(mockDetectWatermark).not.toHaveBeenCalled();
    expect(isOnDetectCooldown('guild-1', SENDER_ID)).toBe(false);
  });
});

describe('sweepCooldowns — detect window decoupled from send window (#823 round-5)', () => {
  const config = require('../src/config');
  let savedDetect;
  let savedSend;
  let nowSpy;

  beforeEach(() => {
    savedDetect = config.QURL_DETECT_COOLDOWN_MS;
    savedSend = config.QURL_SEND_COOLDOWN_MS;
    sendCooldowns.clear();
    detectCooldowns.clear();
  });
  afterEach(() => {
    config.QURL_DETECT_COOLDOWN_MS = savedDetect;
    config.QURL_SEND_COOLDOWN_MS = savedSend;
    if (nowSpy) nowSpy.mockRestore();
    sendCooldowns.clear();
    detectCooldowns.clear();
  });

  test('with DETECT(90s) > SEND(30s): a detect entry at ~31s is NOT swept (still on cooldown)', () => {
    config.QURL_SEND_COOLDOWN_MS = 30000;
    config.QURL_DETECT_COOLDOWN_MS = 90000;

    const t0 = 1_000_000_000_000;
    detectCooldowns.set('guild-1:user-1', t0);
    sendCooldowns.set('user-1', t0);

    nowSpy = jest.spyOn(Date, 'now').mockReturnValue(t0 + 31_000);

    sweepCooldowns();

    expect(detectCooldowns.has('guild-1:user-1')).toBe(true);
    expect(isOnDetectCooldown('guild-1', 'user-1')).toBe(true);
    expect(sendCooldowns.has('user-1')).toBe(false);
  });

  test('a detect entry past the DETECT window IS swept', () => {
    config.QURL_SEND_COOLDOWN_MS = 30000;
    config.QURL_DETECT_COOLDOWN_MS = 90000;
    const t0 = 1_000_000_000_000;
    detectCooldowns.set('guild-1:user-1', t0);
    nowSpy = jest.spyOn(Date, 'now').mockReturnValue(t0 + 91_000);

    sweepCooldowns();

    expect(detectCooldowns.has('guild-1:user-1')).toBe(false);
  });
});

const { DISCORD_MEMBERS_PAGE_SIZE } = require('../src/constants');
const isPrewarmCall = ([arg]) =>
  arg && typeof arg === 'object'
  && arg.limit === DISCORD_MEMBERS_PAGE_SIZE
  && arg.user === undefined
  && arg.query === undefined;

describe('handleQurlSlashSend — guild.members cache pre-warm', () => {
  test('@everyone in recipients string → members.list() pre-warm fires', async () => {
    const aliceId = '500000000000000001';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    await handleQurlSend(int);
    expect(int.guild.members.list.mock.calls.find(isPrewarmCall)).toBeTruthy();
  });

  test('<@&roleId> in recipients string → members.list() pre-warm fires', async () => {
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
    expect(int.guild.members.list.mock.calls.find(isPrewarmCall)).toBeTruthy();
  });

  test('plain <@userId> mentions only → members.list() pre-warm does NOT fire', async () => {
    const aliceId = '500000000000000003';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `<@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    await handleQurlSend(int);
    expect(int.guild.members.list.mock.calls.filter(isPrewarmCall)).toEqual([]);
  });

  test('empty recipients string → members.list() pre-warm does NOT fire', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT }, // no recipients
    });
    await handleQurlSend(int);
    expect(int.guild.members.list.mock.calls.filter(isPrewarmCall)).toEqual([]);
  });

  test('@everyone WITHOUT MENTION_EVERYONE → pre-warm does NOT fire (parser will deny anyway)', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '@everyone' },
    });
    await handleQurlSend(int);
    expect(int.guild.members.list.mock.calls.filter(isPrewarmCall)).toEqual([]);
  });

  test('@everyone + <@&roleId> WITHOUT MENTION_EVERYONE → pre-warm STILL fires (role path)', async () => {
    const aliceId = '500000000000000077';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@&7200>` },
      guildMembers: { [aliceId]: {} },
    });
    int.guild.roles.cache.set('7200', {
      id: '7200', name: 'team', mentionable: true,
      members: new Map([[aliceId, { user: { id: aliceId, bot: false } }]]),
    });
    await handleQurlSend(int);
    expect(int.guild.members.list.mock.calls.find(isPrewarmCall)).toBeTruthy();
  });

  test('@everyonefoo (no word boundary) → pre-warm does NOT fire', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '@everyonefoo' },
    });
    await handleQurlSend(int);
    expect(int.guild.members.list.mock.calls.filter(isPrewarmCall)).toEqual([]);
  });

  test('cache already at memberCount → members.list() pre-warm short-circuits', async () => {
    const aliceId = '500000000000000005';
    const bobId = '500000000000000006';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone` },
      guildMembers: { [aliceId]: {}, [bobId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    int.guild.memberCount = 2; // matches cache.size from guildMembers above
    await handleQurlSend(int);
    expect(int.guild.members.list.mock.calls.filter(isPrewarmCall)).toEqual([]);
  });

  test('http-only tier (memberCount undefined) → pre-warm always paginates, NEVER short-circuits on approximateMemberCount', async () => {
    const aliceId = '500000000000000007';
    const bobId = '500000000000000008';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone` },
      guildMembers: { [aliceId]: {}, [bobId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    int.guild.memberCount = undefined;
    int.guild.approximateMemberCount = 2;
    await handleQurlSend(int);
    expect(int.guild.members.list.mock.calls.filter(isPrewarmCall).length).toBeGreaterThan(0);
  });

  test('concurrent invocations in the same guild share one in-flight fetch', async () => {
    const aliceId = '500000000000000010';
    let release;
    const listGate = new Promise((r) => { release = r; });
    const sharedList = jest.fn(() => listGate);
    const sharedGuild = {
      id: 'shared-guild',
      members: { cache: new Map(), list: sharedList },
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
    await new Promise((r) => setImmediate(r));
    const prewarmCalls = sharedList.mock.calls.filter(isPrewarmCall);
    expect(prewarmCalls.length).toBe(1);
    release(new Map());
    await Promise.all([p1, p2]);
  });

  test('members.list() rejection is swallowed — flow continues in degraded mode', async () => {
    const aliceId = '500000000000000004';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    int.guild.members.list = jest.fn(async () => {
      const err = new Error('rate limited'); err.code = 429; throw err;
    });
    await handleQurlSend(int);
    expect(mockSupersedeOrCreate).toHaveBeenCalled();
    const logger = require('../src/logger');
    const warnCall = logger.warn.mock.calls.find(
      ([msg]) => typeof msg === 'string' && msg.includes('members pre-warm failed'),
    );
    expect(warnCall).toBeTruthy();
  });

  test('pagination: members.list() called with `after` cursor when first page returns full 1000', async () => {
    const aliceId = '500000000000000077';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    int.guild.memberCount = 1500;

    const lastIdOfPage1 = '999999999999999999';
    const page1 = { size: 1000, lastKey: () => lastIdOfPage1 };
    const page2 = new Map();  // empty → loop breaks
    int.guild.members.list = jest.fn()
      .mockResolvedValueOnce(page1)
      .mockResolvedValueOnce(page2);

    await handleQurlSend(int);

    expect(int.guild.members.list).toHaveBeenCalledTimes(2);
    expect(int.guild.members.list.mock.calls[0][0]).toEqual(expect.objectContaining({ limit: 1000 }));
    expect(int.guild.members.list.mock.calls[0][0].after).toBeUndefined();
    expect(int.guild.members.list.mock.calls[1][0]).toEqual(expect.objectContaining({ limit: 1000, after: lastIdOfPage1 }));
  });

  test('pagination: three full pages advance the cursor each time', async () => {
    const aliceId = '500000000000000078';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    int.guild.memberCount = 2500;

    const lastIdOfPage1 = '111111111111111111';
    const lastIdOfPage2 = '222222222222222222';
    const page1 = { size: 1000, lastKey: () => lastIdOfPage1 };
    const page2 = { size: 1000, lastKey: () => lastIdOfPage2 };
    const page3 = new Map(); // partial → loop breaks
    int.guild.members.list = jest.fn()
      .mockResolvedValueOnce(page1)
      .mockResolvedValueOnce(page2)
      .mockResolvedValueOnce(page3);

    await handleQurlSend(int);

    expect(int.guild.members.list).toHaveBeenCalledTimes(3);
    expect(int.guild.members.list.mock.calls[0][0].after).toBeUndefined();
    expect(int.guild.members.list.mock.calls[1][0].after).toBe(lastIdOfPage1);
    expect(int.guild.members.list.mock.calls[2][0].after).toBe(lastIdOfPage2);
  });

  test('pagination: cursor non-advancement bails with warn', async () => {
    const aliceId = '500000000000000079';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    int.guild.memberCount = 5000;

    const stuckCursor = '333333333333333333';
    const stuckPage = { size: 1000, lastKey: () => stuckCursor };
    int.guild.members.list = jest.fn().mockResolvedValue(stuckPage);

    await handleQurlSend(int);

    expect(int.guild.members.list).toHaveBeenCalledTimes(2);
    const logger = require('../src/logger');
    const warnCall = logger.warn.mock.calls.find(
      ([msg]) => typeof msg === 'string' && msg.includes('cursor did not advance'),
    );
    expect(warnCall).toBeTruthy();
    const successCall = logger.info.mock.calls.find(
      ([msg]) => typeof msg === 'string' && msg.includes('pre-warm complete'),
    );
    expect(successCall).toBeFalsy();
  });

  test('pagination: safety cap fires warn when full pages persist past PREWARM_MAX_PAGES', async () => {
    const aliceId = '500000000000000080';
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone <@${aliceId}>` },
      guildMembers: { [aliceId]: {} },
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    int.guild.memberCount = 2000000; // > 1M, defeats the hot-cache short-circuit

    let counter = 0;
    int.guild.members.list = jest.fn(async () => ({
      size: 1000,
      lastKey: () => `cursor-${counter++}`, // unique every page
    }));

    await handleQurlSend(int);

    expect(int.guild.members.list).toHaveBeenCalledTimes(1000);
    const logger = require('../src/logger');
    const warnCall = logger.warn.mock.calls.find(
      ([msg]) => typeof msg === 'string' && msg.includes('hit safety cap'),
    );
    expect(warnCall).toBeTruthy();
    expect(warnCall[1]).toEqual(expect.objectContaining({ cache_size: expect.anything() }));
    const successCall = logger.info.mock.calls.find(
      ([msg]) => typeof msg === 'string' && msg.includes('pre-warm complete'),
    );
    expect(successCall).toBeFalsy();
  });

  test('pagination: each successful list() call merges members into guild.members.cache', async () => {
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: `@everyone` },
      guildMembers: {},
    });
    int.memberPermissions = { has: jest.fn(() => true) };
    int.guild.memberCount = 2500;

    const page1Ids = ['100', '101', '102'];
    const page2Ids = ['200', '201'];
    int.guild.members.list = jest.fn(async ({ after }) => {
      if (!after) {
        for (const id of page1Ids) int.guild.members.cache.set(id, { user: { id, bot: false } });
        return { size: 1000, lastKey: () => page1Ids[page1Ids.length - 1] };
      }
      for (const id of page2Ids) int.guild.members.cache.set(id, { user: { id, bot: false } });
      return new Map(); // partial → loop breaks
    });

    await handleQurlSend(int);

    for (const id of [...page1Ids, ...page2Ids]) {
      expect(int.guild.members.cache.has(id)).toBe(true);
    }

    for (const call of int.guild.members.list.mock.calls) {
      expect(call[0].cache).not.toBe(false);
    }
  });
});

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
    const int = makeInteraction({
      options: { location: 'somewhere', recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    int.deferReply = jest.fn(async () => { const e = new Error('Unknown interaction'); e.code = 10062; throw e; });
    await handleQurlMap(int);
    expect(isOnCooldown(SENDER_ID)).toBe(false);
    expect(int.editReply).not.toHaveBeenCalled();
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
  });

  test('defers ONCE — handleQurlSlashSend skips its own defer when already deferred', async () => {
    mockFindPlaceFromText.mockResolvedValueOnce({ placeId: 'ChIJ1', name: 'Place', address: '' });
    const int = makeInteraction({
      options: { location: 'somewhere', recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlMap(int);
    expect(int.deferReply).toHaveBeenCalledTimes(1);
  });

  test('arbitrary text → resolved through Places to a place_id-pinned URL', async () => {
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
    expect(int.deferReply).toHaveBeenCalledWith(expect.objectContaining({ ephemeral: true }));
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Couldn't find/),
    }));
    expect(mockSupersedeOrCreate).not.toHaveBeenCalled();
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('stale-sentinel NOT_FOUND → place-specific message (does NOT echo the wire sentinel)', async () => {
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
    const int = makeInteraction({
      options: {
        location: 'https://www.google.com/maps/place/Cafe',
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
    const lone = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/;
    expect(payload.locationName).not.toMatch(lone);
  });

  test('forged interaction missing required `location` → actionable ephemeral, no flow row, cooldown cleared', async () => {
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

  test.each([
    [10062, 'Unknown interaction'],
    ['ECONNRESET', 'socket reset'],
  ])('failed acknowledgement (%s) stops before flow or send work', async (code, message) => {
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    int.customId = CONFIRM_SEND_CUSTOM_ID;
    const err = new Error(message);
    err.code = code;
    int.deferUpdate.mockRejectedValueOnce(err);

    await handleConfirmSendClick(int, {
      flow_id: 'fid', row: { payload: validPayload, version: 1 },
    });

    expect(mockDb.getGuildApiKey).not.toHaveBeenCalled();
    expect(mockDeleteFlow).not.toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(mockMintLinks).not.toHaveBeenCalled();
    expect(mockSendDM).not.toHaveBeenCalled();
    expect(int.editReply).not.toHaveBeenCalled();
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('stopping before state change'),
      expect.objectContaining({
        flow_id: 'fid', custom_id: CONFIRM_SEND_CUSTOM_ID, error_code: code,
      }),
    );
  });

  test('happy path → deferUpdate + deleteFlow + editReply "Preparing"', async () => {
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM, reason: 'terminal',
    }));
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Preparing send/),
    }));
    expect(int.update).not.toHaveBeenCalled();
  });

  test('deleteFlow dedup loser → version-fenced "Recipients changed" reply, no pipeline call', async () => {
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
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({ reason: 'terminal' }));
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/no longer reachable/),
    }));
  });

  test('no apiKey resolved → tells user setup is needed, cooldown cleared (admin action recovers)', async () => {
    mockDb.getGuildApiKey.mockResolvedValueOnce(null);
    const config = require('../src/config');
    jest.replaceProperty(config, 'QURL_API_KEY', null);
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    sendCooldowns.set(SENDER_ID, Date.now());
    await handleConfirmSendClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/not configured|setup/i),
    }));
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('partial-resolve at Send click — Send proceeds with remaining users, drop surfaced via followUp + info log', async () => {
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
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/1 recipient had left the server/),
      ephemeral: true,
    }));
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringMatching(/partial drop at click time/),
      expect.objectContaining({ left: 1, transient: 0 }),
    );
  });

  test('partial transient lookup at Send click — Send proceeds with remaining, transient drop surfaced with retry copy (/qurl send)', async () => {
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
    expect(int.followUp).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/rerun \/qurl send/),
    }));
  });

  test('getGuildApiKey throw at click time → ephemeral retry, NO deleteFlow (row stays alive), cooldown cleared', async () => {
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
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('resolveRecipientUsers throw at click time → ephemeral retry message, NO deleteFlow', async () => {
    const int = makeInteraction({
      guildMembers: {},
    });
    int.guild.members.fetch = jest.fn().mockRejectedValue(new Error('catastrophic'));
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
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('Send click with sender-only recipientIds → legitimate self-send, proceeds to dispatch', async () => {
    const int = makeInteraction({ guildMembers: { [SENDER_ID]: {} } });
    mockDb.getGuildApiKey.mockResolvedValueOnce('apikey-1');
    await handleConfirmSendClick(int, {
      flow_id: 'fid',
      row: { payload: { ...validPayload, recipientIds: [SENDER_ID] }, version: 1 },
    });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Preparing send/),
    }));
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      reason: 'terminal',
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Invalid recipient list/i),
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/all left the server/i),
    }));
  });

  test('forged Send click with empty recipientIds → distinct copy + deleteFlow (not the "all left" copy)', async () => {
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
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
      expectedVersion: 1,
    }));
  });

  test('empty recipientIds + concurrent picker race (deleteFlow returns deleted:false) → "card moved" followUp, no editReply wipe', async () => {
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
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/No recipients were selected/i),
    }));
  });

  test('all-invalid recipients + concurrent picker race (deleteFlow returns deleted:false) → "card moved" followUp', async () => {
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
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      expectedVersion: 5,
    }));
  });

  test('bot kicked between confirm and Send → distinct ephemeral, flow row deleted, cooldown cleared', async () => {
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
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/no longer reachable/i),
    }));
  });
});

describe('handleConfirmCancelClick', () => {
  test('failed acknowledgement stops before delete or cooldown mutation', async () => {
    const int = makeInteraction();
    int.deferUpdate.mockRejectedValueOnce(new Error('Unknown interaction'));
    const cooldownAt = Date.now();
    sendCooldowns.set(SENDER_ID, cooldownAt);

    await handleConfirmCancelClick(int, { flow_id: 'fid', row: { version: 3 } });

    expect(mockDeleteFlow).not.toHaveBeenCalled();
    expect(sendCooldowns.get(SENDER_ID)).toBe(cooldownAt);
    expect(int.editReply).not.toHaveBeenCalled();
  });

  test('happy path → version-gated deleteFlow + cooldown CLEARED + update', async () => {
    const int = makeInteraction();
    sendCooldowns.set(SENDER_ID, Date.now());
    await handleConfirmCancelClick(int, { flow_id: 'fid', row: { version: 3 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
      expectedVersion: 3,
    }));
    expect(sendCooldowns.has(SENDER_ID)).toBe(false);
    expect(isOnCooldown(SENDER_ID)).toBe(false);
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/cancelled/),
    }));
  });

  test('deleteFlow dedup loser → ephemeral message + cooldown PRESERVED (no clear)', async () => {
    mockDeleteFlow.mockResolvedValueOnce({ deleted: false });
    const cooldownAt = Date.now();
    sendCooldowns.set(SENDER_ID, cooldownAt);
    const int = makeInteraction();
    await handleConfirmCancelClick(int, { flow_id: 'fid', row: { version: 3 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/card moved/i),
      ephemeral: true,
    }));
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
    int.roles = new Map(roles);
    int.memberPermissions = {
      has: jest.fn(() => canMentionEveryone),
    };
    if (guildMemberCache && int.guild) {
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
      set_expires_at: expect.any(Number),
    }));
    const callArgs = mockTransitionFlow.mock.calls[0][2];
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
    let deferAckedBeforeTransition = false;
    const int = makeSelectInteraction();
    mockTransitionFlow.mockImplementationOnce(async () => {
      deferAckedBeforeTransition = int.deferUpdate.mock.calls.length > 0;
      return { result: 'ok', version: 2 };
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(deferAckedBeforeTransition).toBe(true);
  });

  test('duplicate whose deferUpdate is rejected cannot mutate flow state', async () => {
    const duplicate = makeSelectInteraction();
    duplicate.deferUpdate.mockRejectedValueOnce(new Error('Unknown interaction'));

    await handleConfirmUserSelect(duplicate, {
      flow_id: 'fid', row: { payload: initialPayload, version: 1 },
    });

    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(duplicate.editReply).not.toHaveBeenCalled();
    expect(duplicate.followUp).not.toHaveBeenCalled();
  });

  test('client-side already-replied error continues when this object owns the acknowledgement', async () => {
    const int = makeSelectInteraction();
    int.deferred = true;
    const err = new Error('The reply to this interaction has already been sent or deferred.');
    err.code = 'InteractionAlreadyReplied';
    int.deferUpdate.mockRejectedValueOnce(err);

    await handleConfirmUserSelect(int, {
      flow_id: 'fid', row: { payload: initialPayload, version: 1 },
    });

    expect(mockTransitionFlow).toHaveBeenCalledTimes(1);
    expect(int.editReply).toHaveBeenCalled();
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('already acknowledged by this handler'),
      expect.objectContaining({ flow_id: 'fid' }),
    );
  });

  test('client-side already-replied error stops when this object does not own the acknowledgement', async () => {
    const duplicate = makeSelectInteraction();
    const err = new Error('The reply to this interaction has already been sent or deferred.');
    err.code = 'InteractionAlreadyReplied';
    duplicate.deferUpdate.mockRejectedValueOnce(err);

    await handleConfirmUserSelect(duplicate, {
      flow_id: 'fid', row: { payload: initialPayload, version: 1 },
    });

    expect(duplicate.deferred).toBe(false);
    expect(duplicate.replied).toBe(false);
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(duplicate.editReply).not.toHaveBeenCalled();
  });

  test('two delivered copies produce one successful pick and no superseded card', async () => {
    const accepted = makeSelectInteraction();
    const duplicate = makeSelectInteraction();
    let rejectDuplicateAck;
    duplicate.deferUpdate.mockImplementationOnce(() => new Promise((_, reject) => {
      rejectDuplicateAck = reject;
    }));
    let transitionEntered;
    const transitionStarted = new Promise((resolve) => { transitionEntered = resolve; });
    let finishTransition;
    mockTransitionFlow.mockImplementationOnce(() => new Promise((resolve) => {
      finishTransition = resolve;
      transitionEntered();
    }));

    const deliveries = Promise.all([
      handleConfirmUserSelect(accepted, {
        flow_id: 'fid', row: { payload: initialPayload, version: 1 },
      }),
      handleConfirmUserSelect(duplicate, {
        flow_id: 'fid', row: { payload: initialPayload, version: 1 },
      }),
    ]);
    await transitionStarted;
    rejectDuplicateAck(new Error('Unknown interaction'));
    finishTransition({ result: 'ok', version: 2 });
    await deliveries;

    expect(mockTransitionFlow).toHaveBeenCalledTimes(1);
    expect(accepted.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Sending file/),
    }));
    const allEdits = [accepted, duplicate]
      .flatMap((int) => int.editReply.mock.calls.map(([payload]) => payload.content));
    expect(allEdits).not.toEqual(expect.arrayContaining([
      expect.stringMatching(/superseded/i),
    ]));
  });

  test('pick combining a bot AND sender → sender survives, bot is dropped, flow advances', async () => {
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
    const int = makeSelectInteraction({
      users: [makeUser(u1, { bot: true })],
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/bots/);
    expect(updated.content).toMatch(/Sending file/);
    expect(updated.content).toMatch(/Expires/);
  });

  test('defense-in-depth: cap-exceeded pick rejected even though picker setMaxValues makes it unreachable today', async () => {
    const users = Array.from({ length: 26 }, (_, i) => makeUser(`1000000000000000${String(i).padStart(2, '0')}`));
    const int = makeSelectInteraction({ users });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Pick at most/);
    expect(updated.content).toMatch(/Sending file/);
  });

  test('partial-bot pick → transitionFlow with non-bot users + warning surfaces on card', async () => {
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
    mockTransitionFlow.mockRejectedValueOnce(new Error('ddb gone'));
    const int = makeSelectInteraction();
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not save your pick/i),
      ephemeral: true,
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/superseded/i),
    }));
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/transitionFlow threw/),
      expect.objectContaining({ flow_id: 'fid' }),
    );
  });

  test('mentionable picker: role pick → role members expanded, flow advances with merged recipientIds', async () => {
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
    const int = makeSelectInteraction({
      users: [],
      roles: [],
      canMentionEveryone: false,
    });
    const everyoneId = int.guild.id;
    int.roles = new Map([[everyoneId, { id: everyoneId, members: new Map() }]]);
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/@everyone/);
    expect(updated.content).toMatch(/Mention Everyone/);
    expect(updated.content).toMatch(/Sending file/);
  });

  test('mentionable picker: @everyone role WITH MENTION_EVERYONE → expands via guild.members.cache, flow advances', async () => {
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
    expect(recipientIds).not.toContain(bot1);
  });

  test('mentionable picker: bot-only role pick → all-invalid branch with role-specific reason (no silent swallow)', async () => {
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
    expect(updated.content).toMatch(/Sending file/);
  });

  test('mentionable picker: sender via role pick → flow advances, selfIncluded flips on (#322 parity)', async () => {
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
    const u1 = makeUser('100000000000000001');
    const int = makeSelectInteraction({
      users: [u1],
      roles: [],
      canMentionEveryone: true,
    });
    int.guild.members = {};
    const everyoneId = int.guild.id;
    int.roles = new Map([[everyoneId, { id: everyoneId, members: new Map() }]]);
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    expect(mockTransitionFlow.mock.calls[0][2].payload.recipientIds).toEqual([u1.id]);
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Member cache not yet ready/);
    expect(updated.content).toMatch(/expanded to 0 members/);
  });

  test('mentionable picker: guild without members.cache (cold cache after restart) → surfaces "Member cache not yet ready" reason', async () => {
    const int = makeSelectInteraction({
      users: [],
      roles: [],
      canMentionEveryone: true,
    });
    int.guild.members = {};
    const everyoneId = int.guild.id;
    int.roles = new Map([[everyoneId, { id: everyoneId, members: new Map() }]]);
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Member cache not yet ready/);
    expect(updated.content).toMatch(/Sending file/);
  });

  test('mentionable picker: partial-valid role (humans + bots) → flow advances AND droppedFromRoles warning surfaces', async () => {
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
    const botIdx = updated.content.indexOf('Cannot send to bots');
    const roleIdx = updated.content.indexOf('Non-mentionable role');
    expect(botIdx).toBeGreaterThanOrEqual(0);
    expect(roleIdx).toBeGreaterThanOrEqual(0);
    expect(botIdx).toBeLessThan(roleIdx);
  });

  test('re-pick preserves personalMessageRaw + personalMessage through the spread', async () => {
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

  test('mentionable picker: non-mentionable role WITHOUT MENTION_EVERYONE → all-invalid banner with role-specific reason', async () => {
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
    expect(updated.content).toMatch(/Non-mentionable role requires/i);
    expect(updated.content).toMatch(/Mention Everyone/);
    expect(updated.content).toMatch(/have the role marked as mentionable/i);
    expect(updated.content).toMatch(/Sending file/);
  });

  test('mentionable picker: multiple non-mentionable roles → banner uses plural noun + verb ("roles require")', async () => {
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
    int.guild.roles.cache.set('role-admin', { id: 'role-admin', name: 'admin-team', mentionable: false });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    expect(mockTransitionFlow.mock.calls[0][2].payload.recipientIds).toEqual([u1.id]);
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/@admin-team/);
    expect(updated.content).toMatch(/Mention Everyone/);
    expect(updated.content).toMatch(/role\.mentionable: true/);
  });

  test('mentionable picker: denied role with deleted-from-cache name → fallback "unknown-role" renders', async () => {
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
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/@unknown-role/);
  });

  test('mentionable picker: non-mentionable role + canMentionEveryone → expands normally (no deny)', async () => {
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

  test('role pick → members.list() pre-warm fires', async () => {
    const role = { id: 'roleA', name: 'team', mentionable: true, members: new Map([[u1, { user: makeUser(u1) }]]) };
    const int = makeSelectInteraction({ users: [], roles: [['roleA', role]] });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.guild.members.list.mock.calls.find(isPrewarmCall)).toBeTruthy();
  });

  test('users-only pick → members.list() pre-warm does NOT fire', async () => {
    const int = makeSelectInteraction({ users: [makeUser(u1)], roles: [] });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.guild.members.list.mock.calls.filter(isPrewarmCall)).toEqual([]);
  });

  test('role pick + members.list() rejection is swallowed — flow continues', async () => {
    const role = { id: 'roleB', name: 'team', mentionable: true, members: new Map([[u1, { user: makeUser(u1) }]]) };
    const int = makeSelectInteraction({ users: [], roles: [['roleB', role]] });
    int.guild.members.list = jest.fn(async () => {
      const err = new Error('rate limited'); err.code = 429; throw err;
    });
    await handleConfirmUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const logger = require('../src/logger');
    const warnCall = logger.warn.mock.calls.find(
      ([msg]) => typeof msg === 'string' && msg.includes('members pre-warm failed'),
    );
    expect(warnCall).toBeTruthy();
  });
});

describe('handleConfirmVoiceEveryone', () => {
  const VOICE_CH = 'voice-ch-1';
  const u1 = '100000000000000001';
  const u2 = '100000000000000002';
  const bot1 = '100000000000000099';

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

  test('failed acknowledgement stops before voice resolution or flow mutation', async () => {
    const int = makeVoiceInteraction({ members: [u1, u2] });
    int.deferUpdate.mockRejectedValueOnce(new Error('Unknown interaction'));

    await handleConfirmVoiceEveryone(int, {
      flow_id: 'fid', row: { payload: basePayload, version: 1 },
    });

    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).not.toHaveBeenCalled();
  });

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
    const int = makeVoiceInteraction({ members: [u1] });
    const payloadWithoutVoice = { ...basePayload, voiceChannelId: null };
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: payloadWithoutVoice, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toBeTruthy();
    expect(Array.isArray(lastCall.components)).toBe(true);
    expect(lastCall.components.length).toBeGreaterThan(0);
  });

  test('missing voiceChannelId WITH previously-picked recipients: re-render preserves recipients (no UI/state drift)', async () => {
    const int = makeVoiceInteraction({ members: [u1] });
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
    expect(lastCall.content).toMatch(/Sending file/);
    expect(lastCall.content).toMatch(/\*\*To:\*\* 1 user/);
  });

  test('channel deleted between render and click: rejectVoice path runs (warning re-render, no transition)', async () => {
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
    const int = makeVoiceInteraction({ members: [bot1], botIds: [bot1] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/Cannot send to bots/i);
    expect(lastCall.content).not.toMatch(/No one is connected/i);
  });

  test('sender is voice-connected → excluded from recipientIds + selfIncluded:false', async () => {
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
    const int = makeVoiceInteraction({ members: [u1, u2] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientMode).toBe('voice');
  });

  test('sender-only voice channel → reject path with "you\'re the only one" copy', async () => {
    const int = makeVoiceInteraction({ members: [SENDER_ID] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/only one in this voice channel/i);
  });

  test('transitionFlow conflict → superseded message (OCC race with sibling interaction)', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    const int = makeVoiceInteraction({ members: [u1, u2] });
    await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(lastCall.content).toMatch(/superseded/);
    expect(lastCall.components).toEqual([]);
  });

  test('partial-cache row (member missing .user) emits a debug log per send (silent-shrinkage telemetry)', async () => {
    const logger = require('../src/logger');
    logger.debug.mockClear();
    const int = makeVoiceInteraction({ members: [u1] });
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
    expect(mockTransitionFlow).toHaveBeenCalled();
  });

  test('voice-everyone button succeeds when sender lacks MENTION_EVERYONE (no asymmetric gate)', async () => {
    const int = makeVoiceInteraction({ members: [u1, u2] });
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
    const config = require('../src/config');
    const origCap = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 1;
    try {
      const int = makeVoiceInteraction({ members: [u1, u2] });
      await handleConfirmVoiceEveryone(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
      expect(mockTransitionFlow).not.toHaveBeenCalled();
      const lastCall = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
      expect(lastCall.content).toMatch(/Voice channel has 2 eligible recipients \(max 1\)/i);
      expect(lastCall.content).toMatch(/picker or @mentions/i);
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = origCap;
    }
  });

  test('corrupt payload (unknown resourceType): deleteFlow + actionable re-run copy', async () => {
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
    const int = makeVoiceInteraction({ members: [u1, u2] });
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
    const logger = require('../src/logger');

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

  test('failed acknowledgement stops before manual-picker flow mutation', async () => {
    const int = makeInteraction();
    int.deferUpdate.mockRejectedValueOnce(new Error('Unknown interaction'));

    await handleConfirmPickManual(int, {
      flow_id: 'fid', row: { payload: voicePayload, version: 1 },
    });

    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).not.toHaveBeenCalled();
  });

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

  test('failed acknowledgement stops before everyone expansion or flow mutation', async () => {
    const int = makeEveryoneInteraction();
    int.deferUpdate.mockRejectedValueOnce(new Error('Unknown interaction'));

    await handleConfirmEveryone(int, {
      flow_id: 'fid', row: { payload: initialPayload, version: 1 },
    });

    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).not.toHaveBeenCalled();
  });

  test('happy path → expands to all non-bot members, transitions flow', async () => {
    const int = makeEveryoneInteraction();
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientIds.sort()).toEqual([u1, u2, SENDER_ID].sort());
    expect(payload.selfIncluded).toBe(true);
    expect(payload.recipientMode).toBe('everyone');
  });

  test('without MENTION_EVERYONE → reject with permission warning, no transition', async () => {
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
    const int = makeEveryoneInteraction({ guildMembers: {}, memberCount: 5 });
    int.guild.members.fetch = jest.fn().mockResolvedValue(new Map());
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/member cache not ready/i);
  });

  test('sender-only-in-cache (no other non-bots) → reject with "matched only you" copy', async () => {
    const int = makeEveryoneInteraction({
      guildMembers: { [SENDER_ID]: {}, [bot1]: { bot: true } },
      memberCount: 2,
    });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/matched only you/i);
  });

  test('valid.length === cap (exactly at boundary) → proceeds (cap-reject is strictly >)', async () => {
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
    const otherUser = '100000000000000077';
    const int = makeEveryoneInteraction({
      guildMembers: { [otherUser]: {} },
      memberCount: 2,  // sender + otherUser
    });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientIds.sort()).toEqual([SENDER_ID, otherUser].sort());
    expect(payload.selfIncluded).toBe(true);
  });

  test('sender missing + warm cache with degraded .user-less row + other non-bots → defensive push fires, partial-cache drops counted', async () => {
    const otherUser = '100000000000000088';
    const int = makeEveryoneInteraction({
      guildMembers: { [otherUser]: {} },
      memberCount: 3,  // sender + otherUser + the degraded row below
    });
    int.guild.members.cache.set('degraded-1', { /* no .user */ });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientIds.sort()).toEqual([SENDER_ID, otherUser].sort());
    expect(payload.selfIncluded).toBe(true);
    const logger = require('../src/logger');
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('partial-cache rows dropped'),
      expect.objectContaining({ dropped: 1 }),
    );
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
    const bot2 = '100000000000000098';
    const int = makeEveryoneInteraction({
      guildMembers: { [bot1]: { bot: true }, [bot2]: { bot: true } },
      memberCount: 3,  // sender + 2 bots
    });
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/No usable recipients|bot/i);
  });

  test('forged interaction with no guild → reject without crash + permission warning', async () => {
    const int = makeEveryoneInteraction({ guildId: null });  // → guild = null
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const reply = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(reply.content).toMatch(/Mention Everyone/);
  });

  test('success-path emits info-level audit log with counts', async () => {
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
    const int = makeEveryoneInteraction();
    int.guild.members.cache.set('degraded-1', { /* no .user */ });
    int.guild.memberCount = 5;
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    const logger = require('../src/logger');
    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringContaining('partial-cache rows dropped'),
      expect.objectContaining({ dropped: 1 }),
    );
    expect(mockTransitionFlow).toHaveBeenCalled();
  });

  test('mid-deploy forward: legacy picker-mode row with pre-filled recipientIds → click @everyone → lands in EVERYONE mode', async () => {
    const int = makeEveryoneInteraction();
    const legacyPayload = {
      ...initialPayload,
      recipientMode: 'picker',
      recipientIds: [u1],  // legacy picker-mode auto-fill subset
      recipientAliases: { [u1]: 'alice' },
    };
    await handleConfirmEveryone(int, { flow_id: 'fid', row: { payload: legacyPayload, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalled();
    const payload = mockTransitionFlow.mock.calls[0][2].payload;
    expect(payload.recipientMode).toBe('everyone');
    expect(payload.recipientIds.sort()).toEqual([u1, u2, SENDER_ID].sort());
  });
});

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

  test('failed acknowledgement stops before expiry flow mutation', async () => {
    const int = makeSelectInteraction({ value: '7d' });
    int.deferUpdate.mockRejectedValueOnce(new Error('Unknown interaction'));

    await handleConfirmExpirySelect(int, {
      flow_id: 'fid', row: { payload: basePayload, version: 1 },
    });

    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).not.toHaveBeenCalled();
  });

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
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    expect(lastEdit.content).toMatch(/7 days/);
  });

  test('selfIncluded notice survives an expiry change re-render', async () => {
    const int = makeSelectInteraction({ value: '7d' });
    const payloadWithSelf = { ...basePayload, selfIncluded: true };
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload: payloadWithSelf, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({ selfIncluded: true, expiresIn: '7d' }),
    }));
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    expect(lastEdit.content).toMatch(/Send includes you/);
  });

  test('no-op re-pick (same value as payload) → skip transitionFlow + version bump, still re-render', async () => {
    const int = makeSelectInteraction({ value: '24h' });  // same as basePayload.expiresIn
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalled();
  });

  test('forged off-set expiry value → reply warn BEFORE defer, NO transitionFlow', async () => {
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
    const logger = require('../src/logger');
    logger.warn.mockClear();
    const int = makeSelectInteraction({ value: '7d' });
    const payload = {
      ...basePayload,
      selfDestructSeconds: 60,
      personalMessage: 'hi',
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
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringMatching(/off-preset selfDestructSeconds/i),
      expect.objectContaining({ selfDestructSeconds: '60' })
    );
  });
});

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

  test('failed acknowledgement stops before self-destruct flow mutation', async () => {
    const int = makeSelectInteraction({ value: '30' });
    int.deferUpdate.mockRejectedValueOnce(new Error('Unknown interaction'));

    await handleConfirmSelfDestructSelect(int, {
      flow_id: 'fid', row: { payload: basePayload, version: 1 },
    });

    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).not.toHaveBeenCalled();
  });

  test('happy path persists new selfDestructSeconds + re-renders', async () => {
    const int = makeSelectInteraction({ value: '30' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      stage_to: SEND_STAGE_AWAITING_CONFIRM,
      payload: expect.objectContaining({ selfDestructSeconds: 30 }),
      terminal: false,
    }));
    expect(int.editReply).toHaveBeenCalled();
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    expect(lastEdit.content).toMatch(/30 seconds/);
  });

  test('no-op re-pick (same selfDestructSeconds as payload) → skip transitionFlow + version bump, still re-render', async () => {
    const int = makeSelectInteraction({ value: 'no-timer' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalled();
  });

  test('"no-timer" form-side sentinel → null (when changing FROM a previous timer)', async () => {
    const payloadWithTimer = { ...basePayload, selfDestructSeconds: 30 };
    const int = makeSelectInteraction({ value: 'no-timer' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: payloadWithTimer, version: 1 } });
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({ selfDestructSeconds: null }),
    }));
  });

  test('unknown forged value → reply warn BEFORE defer, NO transitionFlow', async () => {
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
    const logger = require('../src/logger');
    logger.warn.mockClear();
    const int = makeSelectInteraction({ value: 'no-timer' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(logger.warn).not.toHaveBeenCalled();
  });

  test('conflict → superseded copy', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'conflict' });
    const int = makeSelectInteraction({ value: '30' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/superseded/i),
    }));
  });

  test('not_found → expired copy', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    const int = makeSelectInteraction({ value: '30' });
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/expired/i),
    }));
  });
});

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
    const { TextInputBuilder } = require('discord.js');
    TextInputBuilder.mockClear();
    const int = makeButtonInteraction();
    const legacyPayload = { ...basePayload, personalMessage: '\\*\\*bold\\*\\*' };
    await handleConfirmNoteButton(int, { flow_id: 'fid', row: { payload: legacyPayload, version: 1 } });
    const builder = TextInputBuilder.mock.results[0].value;
    expect(builder.setValue).toHaveBeenCalledWith('');
  });

  test('safe when clicked after recipients fully chosen (idempotent — no transitionFlow)', async () => {
    const int = makeButtonInteraction();
    const payload = { ...basePayload, recipientIds: ['100000000000000001', '100000000000000002'] };
    await handleConfirmNoteButton(int, { flow_id: 'fid', row: { payload, version: 5 } });
    expect(int.showModal).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
  });

  test('showModal failure → ephemeral reply fallback (no silent "interaction failed" toast)', async () => {
    const int = makeButtonInteraction();
    int.showModal.mockRejectedValueOnce(new Error('Discord 500'));
    await handleConfirmNoteButton(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Could not open the note editor/i),
      ephemeral: true,
    }));
    expect(mockTransitionFlow).not.toHaveBeenCalled();
  });

  test('showModal failure → if fallback reply ALSO rejects, no unhandled rejection escapes', async () => {
    const int = makeButtonInteraction();
    int.showModal.mockRejectedValueOnce(new Error('Discord 500'));
    int.reply.mockRejectedValueOnce(new Error('Already acked'));
    let unhandled = null;
    const listener = (reason) => { unhandled = reason; };
    process.on('unhandledRejection', listener);
    try {
      await handleConfirmNoteButton(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
      await new Promise((resolve) => setImmediate(resolve));
      expect(unhandled).toBeNull();
    } finally {
      process.off('unhandledRejection', listener);
    }
    expect(int.showModal).toHaveBeenCalled();
    expect(int.reply).toHaveBeenCalled();
  });
});

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

  test('failed acknowledgement stops before note flow mutation', async () => {
    const int = makeModalInteraction({ inputValue: 'hello' });
    int.deferUpdate.mockRejectedValueOnce(new Error('Unknown interaction'));

    await handleConfirmNoteModal(int, {
      flow_id: 'fid', row: { payload: basePayload, version: 1 },
    });

    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).not.toHaveBeenCalled();
  });

  test('happy path: defers, trims/sanitizes, persists, editReply re-renders', async () => {
    const int = makeModalInteraction({ inputValue: '  **bold** message  ' });
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: basePayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).toHaveBeenCalledWith('fid', 1, expect.objectContaining({
      payload: expect.objectContaining({
        personalMessage: expect.stringMatching(/\\\*\\\*bold\\\*\\\* message/),
        personalMessageRaw: '**bold** message',
      }),
    }));
    expect(int.editReply).toHaveBeenCalled();
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    expect(lastEdit.content).toMatch(/\\\*\\\*bold\\\*\\\* message/);
  });

  test('round-trip no-op: re-submit unchanged input → skip transitionFlow + version bump, still re-render', async () => {
    const int = makeModalInteraction({ inputValue: '**bold**' });
    const existingPayload = {
      ...basePayload,
      personalMessage: '\\*\\*bold\\*\\*',
      personalMessageRaw: '**bold**',
    };
    await handleConfirmNoteModal(int, { flow_id: 'fid', row: { payload: existingPayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).toHaveBeenCalled();
  });

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
    expect(int.reply).not.toHaveBeenCalled();
    expect(int.update).not.toHaveBeenCalled();
  });

  test('sibling-mutation merge: row carries an expiry change made during typing → persisted alongside the note', async () => {
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
        expiresIn: '7d',
        selfDestructSeconds: 30,
      }),
    }));
  });
});

describe('rerenderConfirmCard cache-miss recipient fallback', () => {
  const u1 = '100000000000000001';
  const persistedAlias = 'Alice (display)';

  function makeSelectInteraction({ value = '7d', ...rest } = {}) {
    const int = makeInteraction(rest);
    int.values = [value];
    return int;
  }

  test('renders persisted alias when member-cache is empty', async () => {
    const payload = {
      resourceType: 'file',
      resourceLabel: 'x.png',
      recipientIds: [u1],
      recipientAliases: { [u1]: persistedAlias },
      expiresIn: '24h',
      selfDestructSeconds: null,
      personalMessage: null,
    };
    const int = makeSelectInteraction({ value: '7d', guildMembers: {} });
    await handleConfirmExpirySelect(int, { flow_id: 'fid', row: { payload, version: 1 } });
    expect(int.editReply).toHaveBeenCalled();
    const lastEdit = int.editReply.mock.calls.slice(-1)[0][0];
    expect(lastEdit.content).toMatch(/Alice/);
    expect(lastEdit.content).not.toMatch(new RegExp(u1));
  });

  test('warningsBlock from payload carries across menu interactions', async () => {
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
    const int = makeInteraction({ guildMembers: { '100000000000000001': {} } });
    int.values = ['30'];  // legit self-destruct preset
    await handleConfirmSelfDestructSelect(int, { flow_id: 'fid', row: { payload, version: 1 } });
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
    expect(lastEdit.content).toMatch(/0 users in <#voice-empty>/);
    expect(lastEdit.content).not.toMatch(/you not included/);
    expect(lastEdit.content).not.toMatch(/Pick recipients below/);
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

describe('renderConfirmCardRows', () => {

  test('slash-entry WITHOUT recipients → 4 rows, Send disabled, expiry/self-destruct/note interactable', async () => {
    const { ButtonBuilder } = require('discord.js');
    ButtonBuilder.mockClear();
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT },  // no `recipients` → needsPicker
    });
    await handleQurlSend(int);
    const editReplyCalls = int.editReply.mock.calls;
    const lastCall = editReplyCalls[editReplyCalls.length - 1][0];
    expect(lastCall.components).toHaveLength(4);
    const sendBuilder = ButtonBuilder.mock.results.find(
      (r) => r.value.setCustomId.mock.calls[0]?.[0] === 'qurl_confirm_send'
    );
    expect(sendBuilder).toBeDefined();
    expect(sendBuilder.value.setDisabled).toHaveBeenCalledWith(true);
  });

  test('slash-entry WITH recipients → 4 rows (picker still attached so layout is stable across menu interactions)', async () => {
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
    const { ButtonBuilder } = require('discord.js');
    ButtonBuilder.mockClear();
    const int = makeInteraction({
      options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
      guildMembers: { '100000000000000001': {} },
    });
    await handleQurlSend(int);
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
    const { ButtonBuilder } = require('discord.js');
    ButtonBuilder.mockClear();
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
    expect(label).toBe('\u{1F50A} Everyone on voice');
    expect(label.length).toBeLessThanOrEqual(80);
    expect(label).not.toContain('🎉');
    expect(label).not.toContain('**');
    expect(label).not.toContain('<@');
    expect(label).not.toContain('#');
    expect(label).not.toContain('…');
    expect(label).not.toMatch(/\(\d+\)/);
  });

  describe('voice-mode layout (recipientMode:"voice")', () => {

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
      const { MentionableSelectMenuBuilder, ButtonBuilder } = require('discord.js');
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
      expect(MentionableSelectMenuBuilder).not.toHaveBeenCalled();
      const customIds = ButtonBuilder.mock.results.map(
        (r) => r.value.setCustomId.mock.calls[0]?.[0]
      );
      expect(customIds).toContain('qurl_confirm_pick_manual');
      expect(customIds).not.toContain('qurl_confirm_voice_everyone');
      const lastCall = int.editReply.mock.calls.slice(-1)[0][0];
      expect(lastCall.content).toContain(`<#${VOICE_CH}>`);
      expect(lastCall.content).not.toMatch(/you not included/);
    });

    test('forged voice-mode without voiceChannelId still renders the escape hatch (defensive)', () => {
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
      const { ButtonBuilder } = renderEveryoneRows({ voiceChannelId: 'voice-ch-1' });
      const customIds = ButtonBuilder.mock.results.map(
        (r) => r.value.setCustomId.mock.calls[0]?.[0]
      );
      expect(customIds).not.toContain('qurl_confirm_voice_everyone');
    });

    test('bottom row has exactly 4 buttons in order: Pick-manual, Note, Send, Cancel', () => {
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
      const label = everyoneBtn.value.setLabel.mock.calls[0][0];
      expect(label).not.toMatch(/\(\d+\)/);
      expect(label).not.toMatch(/exceeds/);
      expect(label).not.toMatch(/\(\?\)/);
    });

    test('disabled when memberCount unavailable AND cache cold (displayCount null)', async () => {
      const { ButtonBuilder } = require('discord.js');
      ButtonBuilder.mockClear();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
        guildMembers: { '100000000000000001': {} },
      });
      int.memberPermissions = { has: jest.fn(() => true) };
      delete int.guild.memberCount;
      await handleQurlSend(int);
      const everyoneBtn = ButtonBuilder.mock.results.find(
        (r) => r.value.setCustomId.mock.calls[0]?.[0] === 'qurl_confirm_everyone'
      );
      expect(everyoneBtn).toBeDefined();
      expect(everyoneBtn.value.setDisabled).toHaveBeenCalledWith(true);
    });

    test('emits warn-log with branch=displayCount-null when memberCount is unavailable', async () => {
      const logger = require('../src/logger');
      logger.warn.mockClear();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
        guildMembers: { '100000000000000001': {} },
      });
      int.memberPermissions = { has: jest.fn(() => true) };
      int.guildId = 'g-diag-null';
      delete int.guild.memberCount;
      await handleQurlSend(int);
      expect(logger.warn).toHaveBeenCalledWith(
        '@everyone button rendered disabled',
        expect.objectContaining({
          branch: 'displayCount-null',
          guildId: 'g-diag-null',
          guildMemberCount: null,
          displayCount: null,
        }),
      );
    });

    test('emits warn-log with branch=over-cap when warm cache exceeds the recipient cap', async () => {
      const config = require('../src/config');
      const originalCap = config.QURL_SEND_MAX_RECIPIENTS;
      try {
        Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: 2, configurable: true, writable: true });
        const logger = require('../src/logger');
        logger.warn.mockClear();
        const int = makeInteraction({
          options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
          guildMembers: {
            '100000000000000001': {},
            '100000000000000002': {},
            '100000000000000003': {},
          },
        });
        int.memberPermissions = { has: jest.fn(() => true) };
        int.guildId = 'g-diag-overcap';
        int.guild.memberCount = 3;
        await handleQurlSend(int);
        expect(logger.warn).toHaveBeenCalledWith(
          '@everyone button rendered disabled',
          expect.objectContaining({
            branch: 'over-cap',
            guildId: 'g-diag-overcap',
            displayCount: 3,
            accurate: true,
            cap: 2,
          }),
        );
      } finally {
        Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: originalCap, configurable: true, writable: true });
      }
    });

    test('does NOT emit warn-log when @everyone button is enabled (cold-cache happy path)', async () => {
      const { ButtonBuilder } = require('discord.js');
      ButtonBuilder.mockClear();
      const logger = require('../src/logger');
      logger.warn.mockClear();
      const int = makeInteraction({
        options: { attachment: VALID_ATTACHMENT, recipients: '<@100000000000000001>' },
        guildMembers: { '100000000000000001': {} },
      });
      int.memberPermissions = { has: jest.fn(() => true) };
      int.guild.memberCount = 5;
      await handleQurlSend(int);
      const everyoneBtn = ButtonBuilder.mock.results.find(
        (r) => r.value.setCustomId.mock.calls[0]?.[0] === 'qurl_confirm_everyone'
      );
      expect(everyoneBtn).toBeDefined();
      expect(everyoneBtn.value.setDisabled).toHaveBeenCalledWith(false);
      expect(logger.warn).not.toHaveBeenCalledWith(
        '@everyone button rendered disabled',
        expect.anything(),
      );
    });

    test('does NOT render in DM context — direct renderer assertion', () => {
      const { ButtonBuilder } = require('discord.js');
      const commands = require('../src/commands');
      const { renderConfirmCardRows } = commands._test;
      ButtonBuilder.mockClear();
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
      const commands = require('../src/commands');
      const { renderConfirmCardRows, _everyoneCountMemo } = commands._test;
      const guildId = 'guild-memo-iter';
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
      expect(iterations).toBe(1);
    });

    test('memo busts on cache.size change', () => {
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
      memberCache.set('u3', { user: { id: 'u3', bot: false } });
      guild.memberCount = 3;
      renderConfirmCardRows(args);
      expect(iterations).toBe(2);
    });

    test('disabled when warm-cache non-bot count > cap (no overcap suffix in label)', async () => {
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
        expect(everyoneBtn.value.setLabel).toHaveBeenCalledWith(expect.not.stringContaining('exceeds'));
        expect(everyoneBtn.value.setDisabled).toHaveBeenCalledWith(false);
      } finally {
        Object.defineProperty(config, 'QURL_SEND_MAX_RECIPIENTS', { value: originalCap, configurable: true, writable: true });
      }
    });

    test('both @everyone AND voice-everyone buttons render together when invoked from voice + MENTION_EVERYONE', async () => {
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
      int.channel = {
        id: voiceChannelId, name: 'general', type: ChannelType.GuildVoice,
        members: new Map([['100000000000000001', { user: makeUser('100000000000000001') }]]),
      };
      int.guild.channels.cache.set(voiceChannelId, int.channel);
      await handleQurlSend(int);
      const customIds = ButtonBuilder.mock.results.map((r) => r.value.setCustomId.mock.calls[0]?.[0]);
      expect(customIds).toContain('qurl_confirm_voice_everyone');
      expect(customIds).toContain('qurl_confirm_everyone');
      expect(customIds).toContain('qurl_confirm_note_btn');
      expect(customIds).toContain('qurl_confirm_send');
      expect(customIds).toContain('qurl_confirm_cancel');
      expect(customIds.length).toBe(5);
    });

    test('does NOT render in voice-mode (recipientMode === RECIPIENT_MODE_VOICE)', () => {
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
      expect(customIds).toContain('qurl_confirm_pick_manual');
    });
  });
});

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
    const guild = {
      id: 'g-cold',
      memberCount: 50,
      members: { cache: new Map([['u1', { user: { id: 'u1', bot: false } }]]) },
    };
    _everyoneCountMemo.delete(guild);
    expect(computeEveryoneDisplayCount(guild)).toEqual({ count: 50, accurate: false });
  });

  test('memberCount undefined + warm-shape cache → cold fallback returns {count: null, accurate: false}', () => {
    const guild = {
      id: 'g-no-mc',
      members: { cache: new Map([['u1', { user: { id: 'u1', bot: false } }]]) },
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

  test('http-tier shape (memberCount undefined + approximateMemberCount set) → falls back to approximate count', () => {
    const guild = {
      id: 'g-http-tier',
      approximateMemberCount: 42,
      members: { cache: new Map([['bot', { user: { id: 'bot', bot: true } }]]) },
    };
    _everyoneCountMemo.delete(guild);
    expect(computeEveryoneDisplayCount(guild)).toEqual({ count: 42, accurate: false });
  });

  test('memberCount wins over approximateMemberCount when both are set (gateway mode preserves precedence)', () => {
    const guild = {
      id: 'g-both',
      memberCount: 10,            // live (from gateway)
      approximateMemberCount: 9,  // stale (from earlier REST fetch)
      members: { cache: new Map([['u1', { user: { id: 'u1', bot: false } }]]) },
    };
    _everyoneCountMemo.delete(guild);
    expect(computeEveryoneDisplayCount(guild)).toEqual({ count: 10, accurate: false });
  });

  test('partial-cache row (no .user) does not inflate count', () => {
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
    const orig = config.QURL_SEND_MAX_RECIPIENTS;
    config.QURL_SEND_MAX_RECIPIENTS = 1;
    try {
      expect(largeSendThreshold()).toBe(1);
    } finally {
      config.QURL_SEND_MAX_RECIPIENTS = orig;
    }
  });

  test('boundary (cap=2): floor(2/2)=1 wins WITHOUT the substitution — pins discontinuity', () => {
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

describe('constants + exports', () => {
  test('customIds match the wire-protocol values Discord routes against', () => {
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
    expect(RECIPIENT_MODE_PICKER).toBe('picker');
    expect(RECIPIENT_MODE_VOICE).toBe('voice');
    expect(RECIPIENT_MODE_EVERYONE).toBe('everyone');
  });

  test('normalizeRecipientMode: closed set {voice, everyone, picker}; everything else picker', () => {
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
    const { siblingMessageForStage } = require('../src/flow-dispatch');
    const msg = siblingMessageForStage(SEND_STAGE_AWAITING_CONFIRM);
    expect(msg).toMatch(/qurl send.*qurl map.*confirm card/i);
    expect(msg).toBeTruthy();
  });

  test('all four new confirm-card menu customIds are registered (duplicate-register throws)', () => {
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
    const fs = require('fs');
    const path = require('path');
    const src = fs.readFileSync(path.resolve(__dirname, '../src/commands.js'), 'utf8');
    const startMarker = 'async function executeSendPipeline(';
    const startIdx = src.indexOf(startMarker);
    expect(startIdx).toBeGreaterThanOrEqual(0);
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
    try {
      await executeSendPipeline(int, trappedParams);
    } catch (err) {
      if (leaked) throw err;
    }
    expect(leaked).toBe(false);
  });
});
