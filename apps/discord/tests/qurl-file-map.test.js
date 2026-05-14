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
  parseLocationInput,
  safeDecodeURIComponent,
  softenCooldown,
  SEND_STAGE_AWAITING_CONFIRM,
  SEND_USER_SELECT_CUSTOM_ID,
  SEND_CONFIRM_SEND_CUSTOM_ID,
  SEND_CONFIRM_CANCEL_CUSTOM_ID,
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
  // /qurl send, /qurl file, /qurl map share the sendCooldowns Map.
  // setCooldown from any one MUST block the others — without this
  // contract, a user could bypass the per-user throttle by alternating
  // entry points.
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
    await handleSendConfirmClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
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
    await handleSendConfirmClick(int, { flow_id: 'fid', row: { payload: payloadWithFlaky, version: 1 } });
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
    await handleSendConfirmClick(int, { flow_id: 'fid', row: { payload: mapPayload, version: 1 } });
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
    // throw on read — that lands *after* handleSendConfirmClick's
    // bot-kicked guard (which only checks `interaction.guild`).
    Object.defineProperty(int.guild, 'members', {
      get() { throw new Error('cache exploded'); },
    });
    sendCooldowns.set(SENDER_ID, Date.now());
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
    // Zero side effects → cooldown cleared for immediate retry.
    expect(isOnCooldown(SENDER_ID)).toBe(false);
  });

  test('forged Send click with sender-only recipientIds → "Invalid recipient list" copy, NOT "all left"', async () => {
    // Forged payload edge: recipientIds=[SENDER_ID] passes the empty-
    // array guard, resolves fine (sender is in guild cache), then
    // partitionRecipients drops the sender (droppedSelf=1) and
    // valid.length === 0. Pre-fix this hit the all-unresolved branch
    // and surfaced "Recipients are no longer reachable (all left the
    // server)" — misleading. Distinguish by droppedSelf/droppedBots > 0.
    const int = makeInteraction({ guildMembers: { [SENDER_ID]: {} } });
    await handleSendConfirmClick(int, {
      flow_id: 'fid',
      row: { payload: { ...validPayload, recipientIds: [SENDER_ID] }, version: 1 },
    });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Invalid recipient list/i),
      components: [],
    }));
    expect(int.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/all left the server/i),
    }));
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      stage: SEND_STAGE_AWAITING_CONFIRM,
      reason: 'terminal',
    }));
  });

  test('forged Send click with empty recipientIds → distinct copy + deleteFlow (not the "all left" copy)', async () => {
    // A legitimate Send click only lands when the card has at least
    // one recipient (Send is disabled in the empty state). A click
    // with payload.recipientIds === [] therefore implies a fabricated
    // interaction. The all-unresolved branch's "they left the server"
    // copy is wrong here — nobody left, nobody was ever selected.
    const int = makeInteraction({ guildMembers: { [u1]: {} } });
    await handleSendConfirmClick(int, {
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
    await handleSendConfirmClick(int, {
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
    // loaded a row whose recipientIds resolved to all-bots or all-
    // sender. Version-gated delete catches the concurrent picker
    // advance and surfaces the recovery.
    mockDeleteFlow.mockResolvedValueOnce({ deleted: false });
    const int = makeInteraction({ guildMembers: { [SENDER_ID]: {} } });
    await handleSendConfirmClick(int, {
      flow_id: 'fid',
      row: { payload: { ...validPayload, recipientIds: [SENDER_ID] }, version: 5 },
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
    await handleSendConfirmClick(int, { flow_id: 'fid', row: { payload: validPayload, version: 1 } });
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
// handleSendCancelClick
// ──────────────────────────────────────────────────────────────

describe('handleSendCancelClick', () => {
  test('happy path → version-gated deleteFlow + cooldown softened to ~5s residual + update', async () => {
    // softenCooldown leaves 5s of throttle so a user can't spam
    // /qurl file → Cancel → /qurl file → Cancel and rack up
    // supersedeOrCreate DDB writes with zero cost. A legitimate
    // "I changed my mind" still has the cooldown softened from full
    // QURL_SEND_COOLDOWN_MS down to 5s.
    const int = makeInteraction();
    const cooldownStart = Date.now();
    sendCooldowns.set(SENDER_ID, cooldownStart);
    await handleSendCancelClick(int, { flow_id: 'fid', row: { version: 3 } });
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
    await handleSendCancelClick(int, { flow_id: 'fid', row: { version: 3 } });
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
    await handleSendCancelClick(int, { flow_id: 'fid', row: { version: 11 } });
    expect(mockDeleteFlow).toHaveBeenCalledWith('fid', expect.objectContaining({
      expectedVersion: 11,
    }));
  });

  test('deleteFlow throw → ephemeral retry, cooldown preserved (Send may still be in flight)', async () => {
    // Targeted catch around the Cancel deleteFlow
    // for symmetry with handleSendConfirmClick. A DDB blip during a
    // Cancel click now surfaces an actionable ephemeral instead of
    // the dispatcher's generic safety net. Cooldown stays set on the
    // throw path — Send may still be in flight, the user's cooldown
    // should honor the original Send invocation.
    mockDeleteFlow.mockRejectedValueOnce(new Error('ddb gone'));
    sendCooldowns.set(SENDER_ID, Date.now());
    const int = makeInteraction();
    await handleSendCancelClick(int, { flow_id: 'fid', row: { version: 3 } });
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
    expect(int.editReply).toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/Sending file/);
  });

  test('empty pick → deferUpdate, no transition, no editReply', async () => {
    const int = makeSelectInteraction({ users: [] });
    await handleSendUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.deferUpdate).toHaveBeenCalled();
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    expect(int.editReply).not.toHaveBeenCalled();
  });

  test('deferUpdate fires before transitionFlow await — protects Discord 3s ack budget on slow DDB', async () => {
    // Without this guard the transitionFlow DDB OCC call can blow
    // Discord's hard ack deadline on tail-latency, surfacing as an
    // "interaction failed" toast. Mirror handleSendConfirmClick /
    // handleSendCancelClick: ack first, then do the work.
    let deferAckedBeforeTransition = false;
    const int = makeSelectInteraction();
    mockTransitionFlow.mockImplementationOnce(async () => {
      deferAckedBeforeTransition = int.deferUpdate.mock.calls.length > 0;
      return { result: 'ok', version: 2 };
    });
    await handleSendUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(deferAckedBeforeTransition).toBe(true);
  });

  test('all-invalid pick combining bots AND self → message lists BOTH reasons (not just bot-only)', async () => {
    // Previous wording collapsed to bot-only via a ternary. A pick of
    // [bot, sender] is BOTH "cannot send to bots" AND "cannot send to
    // yourself"; the user deserves to see both reasons so they know
    // why removing only the bot wouldn't be enough.
    const bot1 = '100000000000000099';
    const int = makeSelectInteraction({
      users: [makeUser(bot1, { bot: true }), makeUser(SENDER_ID)],
    });
    await handleSendUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(mockTransitionFlow).not.toHaveBeenCalled();
    const updated = int.editReply.mock.calls[int.editReply.mock.calls.length - 1][0];
    expect(updated.content).toMatch(/bots/);
    expect(updated.content).toMatch(/yourself/i);
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
    await handleSendUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
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
    await handleSendUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
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
    await handleSendUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
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
    await handleSendUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/superseded/),
    }));
  });

  test('transitionFlow not_found → expired message', async () => {
    mockTransitionFlow.mockResolvedValueOnce({ result: 'not_found' });
    const int = makeSelectInteraction();
    await handleSendUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
    expect(int.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/expired/),
    }));
  });

  test('transitionFlow throw → targeted ephemeral retry, NOT generic "superseded" copy', async () => {
    // Without the targeted catch, a DDB blip during transitionFlow
    // bubbles to the dispatcher's outer catch which surfaces a
    // generic "superseded" message — wrong, since nothing was actually
    // superseded. Symmetric with handleSendConfirmClick /
    // handleSendCancelClick's DDB-call guards.
    mockTransitionFlow.mockRejectedValueOnce(new Error('ddb gone'));
    const int = makeSelectInteraction();
    await handleSendUserSelect(int, { flow_id: 'fid', row: { payload: initialPayload, version: 1 } });
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
    expect(SEND_USER_SELECT_CUSTOM_ID).toBe('qurl_send_user_select');
    expect(SEND_CONFIRM_SEND_CUSTOM_ID).toBe('qurl_send_confirm_send');
    expect(SEND_CONFIRM_CANCEL_CUSTOM_ID).toBe('qurl_send_confirm_cancel');
  });

  test('all customIds unique', () => {
    const ids = new Set([SEND_USER_SELECT_CUSTOM_ID, SEND_CONFIRM_SEND_CUSTOM_ID, SEND_CONFIRM_CANCEL_CUSTOM_ID]);
    expect(ids.size).toBe(3);
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
