// Require this before other project modules: it installs mocks and the shared
// beforeEach before loading commands.
jest.mock('../../src/config', () => ({
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

jest.mock('../../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

jest.mock('discord.js', () => {
  const { makeOptionBuilder, makeComponentChainable } = require('./discord-mock');
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
jest.mock('../../src/store', () => mockDb);

jest.mock('../../src/discord', () => ({
  sendDM: jest.fn().mockResolvedValue(true),
}));

const mockDetectWatermark = jest.fn();
jest.mock('../../src/connector', () => ({
  downloadAndUpload: jest.fn(),
  reUploadBuffer: jest.fn(),
  mintLinks: jest.fn(),
  detectWatermark: (...a) => mockDetectWatermark(...a),
  uploadJsonToConnector: jest.fn(),
  isAllowedSourceUrl: (url) => typeof url === 'string' && url.startsWith('https://cdn.discordapp.com'),
}));

jest.mock('../../src/qurl', () => ({
  createOneTimeLink: jest.fn(),
  revokeResource: jest.fn(),
  getResourceStatus: jest.fn(),
}));

const {
  mockPlacesModule,
  mockSearchPlaces,
  mockFindPlaceFromText,
  mockGetPlaceDetails,
} = require('./places-mock');
jest.mock('../../src/places', () => mockPlacesModule);

const mockCreateFlow = jest.fn();
const mockLoadFlow = jest.fn();
const mockTransitionFlow = jest.fn();
const mockDeleteFlow = jest.fn();
const mockSupersedeOrCreate = jest.fn();
jest.mock('../../src/flow-state', () => ({
  createFlow: (...a) => mockCreateFlow(...a),
  loadFlow: (...a) => mockLoadFlow(...a),
  transitionFlow: (...a) => mockTransitionFlow(...a),
  deleteFlow: (...a) => mockDeleteFlow(...a),
  supersedeOrCreate: (...a) => mockSupersedeOrCreate(...a),
}));

const commands = require('../../src/commands');
const { _test } = commands;
const logger = require('../../src/logger');
const { sendDM: mockSendDM } = require('../../src/discord');
const { mintLinks: mockMintLinks } = require('../../src/connector');
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

const { DISCORD_MEMBERS_PAGE_SIZE } = require('../../src/constants');
const isPrewarmCall = ([arg]) =>
  arg && typeof arg === 'object'
  && arg.limit === DISCORD_MEMBERS_PAGE_SIZE
  && arg.user === undefined
  && arg.query === undefined;

module.exports = {
  mockDb,
  mockDetectWatermark,
  mockPlacesModule,
  mockSearchPlaces,
  mockFindPlaceFromText,
  mockGetPlaceDetails,
  mockCreateFlow,
  mockLoadFlow,
  mockTransitionFlow,
  mockDeleteFlow,
  mockSupersedeOrCreate,
  commands,
  _test,
  logger,
  mockSendDM,
  mockMintLinks,
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
  handleConfirmUserSelect,
  handleConfirmSendClick,
  handleConfirmCancelClick,
  handleConfirmExpirySelect,
  handleConfirmSelfDestructSelect,
  handleConfirmNoteButton,
  handleConfirmNoteModal,
  handleConfirmVoiceEveryone,
  handleConfirmPickManual,
  SENDER_ID,
  makeUser,
  makeInteraction,
  VALID_ATTACHMENT,
  DISCORD_MEMBERS_PAGE_SIZE,
  isPrewarmCall,
};
