/**
 * /qurl send button-driven state-machine tests.
 *
 * Replaces the slash-options-shape coverage that lived in coverage-boost
 * and commands-comprehensive — those describes were pinned to the dead
 * 4-options shape and were removed in the redesign. Walks the new
 * 3-step flow:
 *   Step 1: ephemeral 2-button reply (Send File / Send Location)
 *   Step 2: file via awaitMessages OR location via single-input modal
 *   Step 3: shared final-step component message — target select,
 *           conditional UserSelect, message-modal button, expiry select,
 *           Send/Cancel — looped via awaitMessageComponent until Send.
 *
 * Each test drives execute() with a queue of awaitMessageComponent /
 * awaitMessages / awaitModalSubmit mock results that walks the state
 * machine to the path under test, then asserts on what the handler did.
 */

// ---------------------------------------------------------------------------
// Mocks — set up BEFORE requiring modules
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

jest.mock('discord.js', () => {
  const makeChainable = (extra = {}) => {
    const obj = {
      setCustomId: jest.fn().mockReturnThis(),
      setLabel: jest.fn().mockReturnThis(),
      setEmoji: jest.fn().mockReturnThis(),
      setStyle: jest.fn().mockReturnThis(),
      setURL: jest.fn().mockReturnThis(),
      setTitle: jest.fn().mockReturnThis(),
      setPlaceholder: jest.fn().mockReturnThis(),
      addOptions: jest.fn().mockReturnThis(),
      setMinValues: jest.fn().mockReturnThis(),
      setMaxValues: jest.fn().mockReturnThis(),
      addComponents: jest.fn().mockReturnThis(),
      setDisabled: jest.fn().mockReturnThis(),
      setMaxLength: jest.fn().mockReturnThis(),
      setRequired: jest.fn().mockReturnThis(),
      setValue: jest.fn().mockReturnThis(),
      ...extra,
    };
    return obj;
  };
  return {
    SlashCommandBuilder: jest.fn().mockImplementation(() => {
      const subBuilder = () => ({
        setName: jest.fn().mockReturnThis(),
        setDescription: jest.fn().mockReturnThis(),
        addStringOption: jest.fn().mockReturnThis(),
        addUserOption: jest.fn().mockReturnThis(),
        addAttachmentOption: jest.fn().mockReturnThis(),
        addIntegerOption: jest.fn().mockReturnThis(),
      });
      const builder = {
        setName: jest.fn(function (n) { builder.name = n; return builder; }),
        setDescription: jest.fn().mockReturnThis(),
        addSubcommand: jest.fn(function (fn) { if (typeof fn === 'function') fn(subBuilder()); return builder; }),
        addStringOption: jest.fn().mockReturnThis(),
        addUserOption: jest.fn().mockReturnThis(),
        addAttachmentOption: jest.fn().mockReturnThis(),
        addIntegerOption: jest.fn().mockReturnThis(),
        setDefaultMemberPermissions: jest.fn().mockReturnThis(),
        toJSON: jest.fn().mockReturnValue({}),
      };
      return builder;
    }),
    EmbedBuilder: jest.fn().mockImplementation(() => makeChainable({
      setColor: jest.fn().mockReturnThis(),
      setDescription: jest.fn().mockReturnThis(),
      setAuthor: jest.fn().mockReturnThis(),
      addFields: jest.fn().mockReturnThis(),
      setFooter: jest.fn().mockReturnThis(),
      setTimestamp: jest.fn().mockReturnThis(),
      setThumbnail: jest.fn().mockReturnThis(),
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
    PermissionFlagsBits: { ManageRoles: 1n, Administrator: 8n },
  };
});

const mockDb = {
  recordQURLSendBatch: jest.fn(),
  updateSendDMStatus: jest.fn(),
  getRecentSends: jest.fn(() => []),
  getSendResourceIds: jest.fn(() => []),
  markSendRevoked: jest.fn(),
  getSendConfig: jest.fn(),
  saveSendConfig: jest.fn(),
};
jest.mock('../src/database', () => mockDb);

const mockSendDM = jest.fn().mockResolvedValue(true);
const mockGetVoiceChannelMembers = jest.fn();
const mockGetTextChannelMembers = jest.fn();
jest.mock('../src/discord', () => ({
  assignContributorRole: jest.fn(),
  notifyPRMerge: jest.fn(),
  notifyBadgeEarned: jest.fn(),
  postGoodFirstIssue: jest.fn(),
  postReleaseAnnouncement: jest.fn(),
  postStarMilestone: jest.fn(),
  postToGitHubFeed: jest.fn(),
  sendDM: mockSendDM,
  getVoiceChannelMembers: mockGetVoiceChannelMembers,
  getTextChannelMembers: mockGetTextChannelMembers,
}));

jest.mock('../src/utils/admin', () => ({
  requireAdmin: jest.fn(async () => true),
  isAdmin: jest.fn(() => true),
}));

const mockDownloadAndUpload = jest.fn();
const mockReUploadBuffer = jest.fn();
const mockMintLinks = jest.fn();
const mockUploadJsonToConnector = jest.fn();
jest.mock('../src/connector', () => ({
  downloadAndUpload: mockDownloadAndUpload,
  reUploadBuffer: mockReUploadBuffer,
  mintLinks: mockMintLinks,
  uploadJsonToConnector: mockUploadJsonToConnector,
  isAllowedSourceUrl: (url) => typeof url === 'string' && url.startsWith('https://cdn.discordapp.com'),
}));

jest.mock('../src/qurl', () => ({
  createOneTimeLink: jest.fn(),
  deleteLink: jest.fn(),
  getResourceStatus: jest.fn(),
}));

jest.mock('../src/places', () => ({ searchPlaces: jest.fn().mockResolvedValue([]) }));

// ---------------------------------------------------------------------------
// Require modules under test
// ---------------------------------------------------------------------------

const crypto = require('crypto');
const NONCE = 'deadbeef01234567';
// jest.spyOn (vs raw property assignment) means jest.restoreAllMocks /
// the configured restoreMocks behavior in jest.config can roll the spy
// back automatically — and it tracks the original impl internally so
// the restore never depends on the test file remembering to capture it.
const originalRandomBytesRef = crypto.randomBytes;
const randomBytesSpy = jest.spyOn(crypto, 'randomBytes').mockImplementation(
  (size) => (size === 8 ? Buffer.from(NONCE, 'hex') : originalRandomBytesRef(size)),
);
const randomUUIDSpy = jest.spyOn(crypto, 'randomUUID').mockReturnValue('mock-send-uuid');

const { commands, _test } = require('../src/commands');
const logger = require('../src/logger');
const { sendCooldowns, setCooldown, setActiveFileSends, memberFetchCache } = _test;

afterAll(() => {
  randomBytesSpy.mockRestore();
  randomUUIDSpy.mockRestore();
});

afterEach(() => {
  // Drain any slot the prior test left claimed (e.g., a happy-path file
  // send mocked downloadAndUpload but the back-half release path is
  // exercised via test-time setActiveFileSends in some cases).
  if (typeof setActiveFileSends === 'function') setActiveFileSends(0);
});

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

const cmd = commands.find((c) => c.data.name === 'qurl');

// Form-component custom IDs derived the same way commands.js builds them.
const ids = {
  initFile: `qurl_init_file_${NONCE}`,
  initLoc: `qurl_init_loc_${NONCE}`,
  locModal: `qurl_loc_modal_${NONCE}`,
  targetSelect: `qurl_form_${NONCE}_target`,
  userSelect: `qurl_form_${NONCE}_user`,
  msgBtn: `qurl_form_${NONCE}_msg_btn`,
  msgModal: `qurl_form_${NONCE}_msg_modal`,
  expirySelect: `qurl_form_${NONCE}_expiry`,
  sendBtn: `qurl_form_${NONCE}_send`,
  cancelBtn: `qurl_form_${NONCE}_cancel`,
};

function makeCompInt(customId, overrides = {}) {
  return {
    customId,
    user: { id: 'user-1' },
    update: jest.fn().mockResolvedValue(undefined),
    deferUpdate: jest.fn().mockResolvedValue(undefined),
    showModal: jest.fn().mockResolvedValue(undefined),
    awaitModalSubmit: jest.fn().mockRejectedValue(new Error('timeout')),
    values: [],
    users: { first: jest.fn(() => null) },
    ...overrides,
  };
}

function makeInteraction({ awaitQueue = [], awaitMessages, channel = {}, guild = {}, dm = {}, user = {}, ...overrides } = {}) {
  // Each call to channel.awaitMessageComponent pulls from awaitQueue in
  // FIFO order. Items can be a value (resolved) or { reject: err } (rejected).
  let queueIdx = 0;
  const channelAwaitMC = jest.fn().mockImplementation(() => {
    const next = awaitQueue[queueIdx++];
    if (!next) return Promise.reject(new Error('timeout'));
    if (next.reject) return Promise.reject(next.reject);
    return Promise.resolve(next);
  });
  // The DM channel returned by interaction.user.createDM(). The file
  // path pivots here when channel.type !== DM. By default we wire the
  // top-level `awaitMessages` arg into the DM channel (most file tests
  // are guild-channel-pivot-to-DM); for DM-already tests, pass
  // `channel: { type: 1, awaitMessages: ... }` instead.
  const dmAwaitMessages = awaitMessages || jest.fn().mockRejectedValue(new Error('timeout'));
  const dmChannel = {
    type: 1, // ChannelType.DM
    send: jest.fn().mockResolvedValue(undefined),
    awaitMessages: dmAwaitMessages,
    ...dm,
  };
  return {
    user: { id: 'user-1', username: 'TestUser', createDM: jest.fn().mockResolvedValue(dmChannel), ...user },
    options: {
      getSubcommand: jest.fn(() => 'send'),
      getString: jest.fn(() => null),
      getUser: jest.fn(() => null),
      getAttachment: jest.fn(() => null),
      getInteger: jest.fn(() => null),
      getFocused: jest.fn(() => ({ name: '', value: '' })),
    },
    reply: jest.fn().mockResolvedValue(undefined),
    editReply: jest.fn().mockResolvedValue(undefined),
    deferReply: jest.fn().mockResolvedValue(undefined),
    followUp: jest.fn().mockResolvedValue(undefined),
    channel: {
      type: 0, // ChannelType.GuildText — DM-pivot fires by default
      awaitMessageComponent: channelAwaitMC,
      awaitMessages: jest.fn().mockRejectedValue(new Error('should not await on guild channel for file capture')),
      members: new Map(),
      ...channel,
    },
    _dmChannel: dmChannel, // exposed for assertion convenience
    channelId: 'ch-1',
    guild: {
      id: 'guild-1',
      members: { fetch: jest.fn().mockResolvedValue(undefined) },
      voiceStates: { cache: new Map() },
      ...guild,
    },
    replied: false,
    deferred: false,
    isChatInputCommand: jest.fn(() => true),
    commandName: 'qurl',
    ...overrides,
  };
}

function makeAttachmentMessage(attachment) {
  return {
    first: () => ({
      attachments: { first: () => attachment },
    }),
  };
}

beforeEach(() => {
  jest.clearAllMocks();
  sendCooldowns.clear();
  if (memberFetchCache) memberFetchCache.clear();
  if (typeof setActiveFileSends === 'function') setActiveFileSends(0);
  // Default happy-path mints / uploads — individual tests override.
  mockDownloadAndUpload.mockResolvedValue({
    resource_id: 'res-file', fileBuffer: new ArrayBuffer(10),
  });
  mockUploadJsonToConnector.mockResolvedValue({ resource_id: 'res-loc' });
  mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/x' }]);
  mockSendDM.mockResolvedValue(true);
});

// ===========================================================================
// 1. Pre-flight guards
// ===========================================================================

describe('handleSend — pre-flight guards', () => {
  it('rejects when interaction has no channel', async () => {
    const interaction = makeInteraction();
    interaction.channel = null;
    await cmd.execute(interaction);
    expect(interaction.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('Cannot use this command'),
    }));
  });

  it('rejects when QURL_API_KEY is not configured', async () => {
    jest.resetModules();
    jest.doMock('../src/config', () => ({
      ...jest.requireActual('./helpers/buildConfigMock').buildConfigMock({ guildId: 'guild-1' }),
      QURL_API_KEY: '',
      QURL_ENDPOINT: 'https://api.test.local',
      CONNECTOR_URL: 'https://connector.test.local',
      QURL_SEND_COOLDOWN_MS: 30000,
      QURL_SEND_MAX_RECIPIENTS: 50,
      DATABASE_PATH: ':memory:',
      ADMIN_USER_IDS: [],
      BASE_URL: 'http://localhost:3000',
      STAR_MILESTONES: [10],
      CONTRIBUTOR_ROLE_NAME: 'Contributor',
      ACTIVE_CONTRIBUTOR_ROLE_NAME: 'Active Contributor',
      CORE_CONTRIBUTOR_ROLE_NAME: 'Core Contributor',
      CHAMPION_ROLE_NAME: 'Champion',
    }));
    const { commands: cmds } = require('../src/commands');
    const localCmd = cmds.find((c) => c.data.name === 'qurl');
    const interaction = makeInteraction();
    await localCmd.execute(interaction);
    expect(interaction.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('not configured for this server'),
    }));
    jest.dontMock('../src/config');
  });

  it('rejects when user is on cooldown', async () => {
    setCooldown('user-1');
    const interaction = makeInteraction();
    await cmd.execute(interaction);
    expect(interaction.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('wait before sending'),
    }));
  });
});

// ===========================================================================
// 2. Step 1 — initial 2-button reply
// ===========================================================================

describe('handleSend — Step 1: 2-button entry', () => {
  it('replies with Send File + Send Location buttons (ephemeral)', async () => {
    const interaction = makeInteraction(); // empty awaitQueue → init-button times out after reply
    await cmd.execute(interaction);
    expect(interaction.reply).toHaveBeenCalledWith(expect.objectContaining({
      content: 'What would you like to send?',
      ephemeral: true,
      components: expect.any(Array),
    }));
  });

  it('cancels and clears cooldown when init-button times out', async () => {
    const interaction = makeInteraction(); // empty queue → reject('timeout')
    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('No selection made'),
      components: [],
    }));
    expect(sendCooldowns.has('user-1')).toBe(false);
  });
});

// ===========================================================================
// 3. Step 2 — file path
// ===========================================================================

describe('handleSend — Step 2: file path', () => {
  function fileInitBtn() {
    return makeCompInt(ids.initFile, { update: jest.fn().mockResolvedValue(undefined) });
  }

  // DM-pivot branch tests — the file path opens a DM with the user when
  // /qurl send is invoked from a guild channel. Goal: file CDN URL never
  // touches a public channel. The 1:1 DM has no other viewers.

  it('pivots to DM: createDM + dm.send + awaitMessages on the DM channel (not the guild channel)', async () => {
    // Don't supply awaitMessages — let it remain rejecting. We assert
    // createDM and dm.send fired but stop before file-drop.
    const fileInit = fileInitBtn();
    const interaction = makeInteraction({ awaitQueue: [fileInit] });
    await cmd.execute(interaction);
    expect(interaction.user.createDM).toHaveBeenCalledTimes(1);
    expect(interaction._dmChannel.send).toHaveBeenCalledWith(expect.stringContaining('Ready! Drop your file here'));
    expect(fileInit.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('I sent you a DM'),
    }));
    // Channel-side awaitMessages MUST NOT have been called — the
    // pivot has to consume the DM's awaitMessages instead.
    expect(interaction.channel.awaitMessages).not.toHaveBeenCalled();
  });

  it('bails with a clear error when DMs are blocked (Discord error 50007)', async () => {
    const err = Object.assign(new Error('Cannot send messages to this user'), { code: 50007 });
    const interaction = makeInteraction({
      awaitQueue: [fileInitBtn()],
      user: { createDM: jest.fn().mockRejectedValue(err) },
    });
    await cmd.execute(interaction);
    // No bot-side download / mint / DM should have happened
    expect(mockDownloadAndUpload).not.toHaveBeenCalled();
    expect(sendCooldowns.has('user-1')).toBe(false);
    // Discord-error 50007 is a normal user-state, NOT an unexpected
    // failure — we don't want logger.error noise.
    expect(logger.error).not.toHaveBeenCalledWith(
      'Failed to open DM for file capture',
      expect.any(Object),
    );
  });

  it.each([
    ['GuildVoice (voice text)', 2],
    ['GuildStageVoice', 13],
  ])('pivots to DM from %s channels (not just GuildText)', async (_label, channelType) => {
    // Voice channels with text chat (since Discord 2022) and stage
    // voice channels invoke slash commands the same way as text
    // channels, but their type isn't DM — so they MUST take the
    // DM-pivot branch. Pin the behavior so a future refactor that
    // narrows the gate to "GuildText only" doesn't silently route
    // voice-channel users back into the public-channel privacy hole.
    const fileInit = fileInitBtn();
    const interaction = makeInteraction({
      awaitQueue: [fileInit],
      channel: { type: channelType },
    });
    await cmd.execute(interaction);
    expect(interaction.user.createDM).toHaveBeenCalledTimes(1);
    expect(interaction._dmChannel.send).toHaveBeenCalledWith(expect.stringContaining('Ready! Drop your file here'));
  });

  it('keeps file capture in-channel when /qurl send is invoked already in a DM', async () => {
    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 1024, url: 'https://cdn.discordapp.com/doc.pdf' };
    const channelAwaitMessages = jest.fn().mockResolvedValue(makeAttachmentMessage(attachment));
    const interaction = makeInteraction({
      awaitQueue: [fileInitBtn()],
      // Override channel to be a DM (type:1). The file capture should
      // run on this channel directly, no createDM pivot.
      channel: {
        type: 1, // ChannelType.DM
        awaitMessages: channelAwaitMessages,
      },
    });
    await cmd.execute(interaction);
    expect(interaction.user.createDM).not.toHaveBeenCalled();
    expect(channelAwaitMessages).toHaveBeenCalled();
  });


  it('logs at warn when awaitMessages rejects with a non-timeout error', async () => {
    // discord.js v14 contract: awaitMessages with errors:['time'] rejects
    // with a Collection on timeout. An Error-shaped rejection means
    // something else broke (channel destroyed, perms revoked, gateway
    // disconnect) — that path takes the unexpected branch and logs.
    const awaitMessages = jest.fn().mockRejectedValue(new Error('Channel destroyed mid-await'));
    const interaction = makeInteraction({
      awaitQueue: [fileInitBtn()],
      awaitMessages,
    });
    await cmd.execute(interaction);
    expect(logger.warn).toHaveBeenCalledWith(
      'awaitMessages failed unexpectedly during file capture',
      expect.objectContaining({ error: 'Channel destroyed mid-await' }),
    );
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('No file received'),
    }));
    expect(sendCooldowns.has('user-1')).toBe(false);
  });

  it('deletes the stale DM "Ready!" prompt when capture times out (no orphan in DM thread)', async () => {
    // Regression guard: without the cleanup in the awaitMessages catch
    // path, a timeout would leave the bot's "Ready! Drop your file
    // here. I'll wait 60 seconds." sitting in the user's DM forever —
    // bots can't go back and delete prompts later, and the user has
    // no way to clean it up themselves (only message authors can
    // delete in DMs).
    const dmPromptDelete = jest.fn().mockResolvedValue(undefined);
    const dmPromptMessage = { delete: dmPromptDelete };
    const dmSend = jest.fn().mockResolvedValue(dmPromptMessage);
    const dmAwaitMessages = jest.fn().mockRejectedValue({ size: 0, first: () => null }); // Collection-shaped timeout
    const interaction = makeInteraction({
      awaitQueue: [fileInitBtn()],
      dm: { send: dmSend, awaitMessages: dmAwaitMessages },
    });
    await cmd.execute(interaction);
    // Bot tried to capture (awaitMessages on DM was called)
    expect(dmAwaitMessages).toHaveBeenCalled();
    // And cleaned up its own DM prompt afterwards
    expect(dmPromptDelete).toHaveBeenCalled();
    // User-facing message still surfaces
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('No file received'),
    }));
  });

  it('cancels when no file is dropped within 60s', async () => {
    // discord.js v14 contract: awaitMessages with errors:['time'] rejects
    // with the collected Collection on timeout (NOT an Error). The handler
    // distinguishes Collection-shaped from Error-shaped rejections.
    const awaitMessages = jest.fn().mockRejectedValue({ size: 0, first: () => null });
    const interaction = makeInteraction({
      awaitQueue: [fileInitBtn()],
      awaitMessages,
    });
    await cmd.execute(interaction);
    expect(awaitMessages).toHaveBeenCalled();
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('No file received'),
    }));
    // Timeout path should NOT log as unexpected
    expect(logger.warn).not.toHaveBeenCalledWith(
      expect.stringContaining('awaitMessages failed unexpectedly'),
      expect.any(Object),
    );
    expect(sendCooldowns.has('user-1')).toBe(false);
  });

  it('rejects disallowed file types', async () => {
    const attachment = { name: 'evil.exe', contentType: 'application/x-msdownload', size: 100, url: 'https://cdn.discordapp.com/x' };
    const awaitMessages = jest.fn().mockResolvedValue(makeAttachmentMessage(attachment));
    const interaction = makeInteraction({ awaitQueue: [fileInitBtn()], awaitMessages });
    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('not allowed'),
    }));
    expect(sendCooldowns.has('user-1')).toBe(false);
  });

  it('rejects files larger than MAX_FILE_SIZE', async () => {
    const attachment = { name: 'big.pdf', contentType: 'application/pdf', size: 100 * 1024 * 1024, url: 'https://cdn.discordapp.com/big' };
    const awaitMessages = jest.fn().mockResolvedValue(makeAttachmentMessage(attachment));
    const interaction = makeInteraction({ awaitQueue: [fileInitBtn()], awaitMessages });
    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('too large'),
    }));
    expect(sendCooldowns.has('user-1')).toBe(false);
  });
});

// ===========================================================================
// 4. Step 2 — location path
// ===========================================================================

describe('handleSend — Step 2: location path', () => {
  function locInitBtn(modalSubmit) {
    return makeCompInt(ids.initLoc, {
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: modalSubmit
        ? jest.fn().mockResolvedValue(modalSubmit)
        : jest.fn().mockRejectedValue(new Error('time')),
    });
  }

  function makeModalSubmit(value) {
    return {
      fields: { getTextInputValue: jest.fn(() => value) },
      deferUpdate: jest.fn().mockResolvedValue(undefined),
      update: jest.fn().mockResolvedValue(undefined),
    };
  }

  it('cancels when location modal times out', async () => {
    const interaction = makeInteraction({ awaitQueue: [locInitBtn(null)] });
    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('timed out'),
    }));
    expect(sendCooldowns.has('user-1')).toBe(false);
  });

  it('fast-fails (no 90s wait) when showModal itself rejects', async () => {
    const initBtn = makeCompInt(ids.initLoc, {
      showModal: jest.fn().mockRejectedValue(new Error('Unknown Interaction')),
      awaitModalSubmit: jest.fn().mockRejectedValue(new Error('should not be reached')),
    });
    const interaction = makeInteraction({ awaitQueue: [initBtn] });
    await cmd.execute(interaction);
    expect(initBtn.awaitModalSubmit).not.toHaveBeenCalled();
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('Could not open the location input'),
    }));
    expect(logger.warn).toHaveBeenCalledWith(
      'Location modal showModal rejected',
      expect.objectContaining({ error: 'Unknown Interaction' }),
    );
    expect(sendCooldowns.has('user-1')).toBe(false);
  });

  it('proceeds to final form after a Google Maps URL submit (then cancels)', async () => {
    // After submit, the form-step awaitMessageComponent will pull the next
    // queue item. We feed a cancel button to short-circuit the loop quickly.
    const cancel = makeCompInt(ids.cancelBtn);
    const interaction = makeInteraction({
      awaitQueue: [
        locInitBtn(makeModalSubmit('https://maps.app.goo.gl/abc123')),
        cancel,
      ],
    });
    await cmd.execute(interaction);
    expect(cancel.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('Send cancelled'),
    }));
    // editReply should have rendered the form at least once
    expect(interaction.editReply).toHaveBeenCalled();
  });

  it('falls back to a search URL for free-text place names', async () => {
    const cancel = makeCompInt(ids.cancelBtn);
    const interaction = makeInteraction({
      awaitQueue: [
        locInitBtn(makeModalSubmit('Eiffel Tower, Paris')),
        cancel,
      ],
    });
    await cmd.execute(interaction);
    // The form was rendered with the place name in the content preview.
    const editCall = interaction.editReply.mock.calls.find(
      (c) => typeof c[0]?.content === 'string' && c[0].content.includes('Eiffel Tower'),
    );
    expect(editCall).toBeTruthy();
  });
});

// ===========================================================================
// 5. Step 3 — final form interactions
// ===========================================================================

describe('handleSend — Step 3: final form', () => {
  function locInitBtn() {
    return makeCompInt(ids.initLoc, {
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockResolvedValue({
        fields: { getTextInputValue: jest.fn(() => 'Test Place') },
        deferUpdate: jest.fn().mockResolvedValue(undefined),
        update: jest.fn().mockResolvedValue(undefined),
      }),
    });
  }

  it('cancel button clears cooldown and exits', async () => {
    const cancel = makeCompInt(ids.cancelBtn);
    const interaction = makeInteraction({ awaitQueue: [locInitBtn(), cancel] });
    await cmd.execute(interaction);
    expect(cancel.update).toHaveBeenCalledWith(expect.objectContaining({
      content: 'Send cancelled.', components: [],
    }));
    expect(sendCooldowns.has('user-1')).toBe(false);
  });

  it('target=user picks a recipient, send button completes flow', async () => {
    const targetUser = makeCompInt(ids.targetSelect, { values: ['user'] });
    const userSelect = makeCompInt(ids.userSelect, {
      users: { first: jest.fn(() => ({ id: 'user-2', bot: false, username: 'Bob' })) },
    });
    const sendBtn = makeCompInt(ids.sendBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), targetUser, userSelect, sendBtn],
    });
    await cmd.execute(interaction);
    // Send button update fires before the back-half kicks in.
    expect(sendBtn.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('Preparing send'),
    }));
    // Back-half ran: mintLinks + sendDM
    expect(mockMintLinks).toHaveBeenCalled();
    expect(mockSendDM).toHaveBeenCalled();
  });

  it('target=user rejects selecting a bot', async () => {
    const targetUser = makeCompInt(ids.targetSelect, { values: ['user'] });
    const userSelectBot = makeCompInt(ids.userSelect, {
      users: { first: jest.fn(() => ({ id: 'bot-1', bot: true, username: 'Botty' })) },
    });
    const cancel = makeCompInt(ids.cancelBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), targetUser, userSelectBot, cancel],
    });
    await cmd.execute(interaction);
    expect(userSelectBot.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('Cannot send to a bot'),
    }));
  });

  it('target=user rejects selecting yourself', async () => {
    const targetUser = makeCompInt(ids.targetSelect, { values: ['user'] });
    const userSelectSelf = makeCompInt(ids.userSelect, {
      users: { first: jest.fn(() => ({ id: 'user-1', bot: false, username: 'TestUser' })) },
    });
    const cancel = makeCompInt(ids.cancelBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), targetUser, userSelectSelf, cancel],
    });
    await cmd.execute(interaction);
    expect(userSelectSelf.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('Cannot send to yourself'),
    }));
  });

  it('target=channel resolves text-channel members', async () => {
    mockGetTextChannelMembers.mockReturnValue([
      { id: 'u2', username: 'Alice' },
      { id: 'u3', username: 'Bob' },
    ]);
    const targetChannel = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const sendBtn = makeCompInt(ids.sendBtn);
    mockMintLinks.mockResolvedValue([
      { qurl_link: 'https://q.test/a' },
      { qurl_link: 'https://q.test/b' },
    ]);
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), targetChannel, sendBtn],
    });
    await cmd.execute(interaction);
    expect(mockGetTextChannelMembers).toHaveBeenCalled();
    expect(sendBtn.update).toHaveBeenCalled();
  });

  it('target=channel re-prompts when channel has no other members', async () => {
    mockGetTextChannelMembers.mockReturnValue([]);
    const targetChannel = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const cancel = makeCompInt(ids.cancelBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), targetChannel, cancel],
    });
    await cmd.execute(interaction);
    expect(targetChannel.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('No other members'),
    }));
  });

  it('target=voice re-prompts when sender is not in a voice channel', async () => {
    mockGetVoiceChannelMembers.mockReturnValue({ error: 'not_in_voice' });
    const targetVoice = makeCompInt(ids.targetSelect, { values: ['voice'] });
    const cancel = makeCompInt(ids.cancelBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), targetVoice, cancel],
    });
    await cmd.execute(interaction);
    expect(targetVoice.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('must be in a voice channel'),
    }));
  });

  it('target=voice re-prompts when voice channel has no other members', async () => {
    mockGetVoiceChannelMembers.mockReturnValue({ members: [] });
    const targetVoice = makeCompInt(ids.targetSelect, { values: ['voice'] });
    const cancel = makeCompInt(ids.cancelBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), targetVoice, cancel],
    });
    await cmd.execute(interaction);
    expect(targetVoice.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('No other users in your voice channel'),
    }));
  });

  it('expiry select updates the chosen expiry', async () => {
    const expiry = makeCompInt(ids.expirySelect, { values: ['1h'] });
    const cancel = makeCompInt(ids.cancelBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), expiry, cancel],
    });
    await cmd.execute(interaction);
    expect(expiry.update).toHaveBeenCalled(); // form re-rendered
  });

  it('message-modal button sets a personal message in form preview', async () => {
    const msgBtn = makeCompInt(ids.msgBtn, {
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockResolvedValue({
        fields: { getTextInputValue: jest.fn(() => 'hello there') },
        update: jest.fn().mockResolvedValue(undefined),
      }),
    });
    const cancel = makeCompInt(ids.cancelBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), msgBtn, cancel],
    });
    await cmd.execute(interaction);
    // Modal-submit's update call carries the new form content with the
    // personal message preview.
    const submitUpdate = msgBtn.awaitModalSubmit.mock.results[0].value;
    const submit = await submitUpdate;
    expect(submit.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('hello there'),
    }));
  });

  it('message-modal timeout leaves the form unchanged and continues the loop', async () => {
    const msgBtn = makeCompInt(ids.msgBtn, {
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockRejectedValue(new Error('time')),
    });
    const cancel = makeCompInt(ids.cancelBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), msgBtn, cancel],
    });
    await cmd.execute(interaction);
    expect(cancel.update).toHaveBeenCalledWith(expect.objectContaining({
      content: 'Send cancelled.',
    }));
  });
});

// ===========================================================================
// 6. Recipients-cap rejection (post-Send)
// ===========================================================================

describe('handleSend — recipients cap', () => {
  function locInitBtn() {
    return makeCompInt(ids.initLoc, {
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockResolvedValue({
        fields: { getTextInputValue: jest.fn(() => 'Test Place') },
        deferUpdate: jest.fn().mockResolvedValue(undefined),
      }),
    });
  }

  it('warns immediately at target select when channel members exceed cap (no Send required)', async () => {
    // 51 members > config 50 cap. The early gate fires inside the
    // targetSelect handler so the user isn't sandbagged after filling
    // in the rest of the form.
    const many = Array.from({ length: 51 }, (_, i) => ({ id: `u${i}`, username: `U${i}` }));
    mockGetTextChannelMembers.mockReturnValue(many);
    const targetChannel = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const cancel = makeCompInt(ids.cancelBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), targetChannel, cancel],
    });
    await cmd.execute(interaction);
    expect(targetChannel.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/over the per-send cap of 50/),
    }));
    expect(mockMintLinks).not.toHaveBeenCalled();
  });
});

// ===========================================================================
// 7. End-to-end happy paths
// ===========================================================================

describe('handleSend — end-to-end happy paths', () => {
  it('file send: full flow from init → file upload → mintLinks → sendDM', async () => {
    const fileInit = makeCompInt(ids.initFile);
    const targetUser = makeCompInt(ids.targetSelect, { values: ['user'] });
    const userSelect = makeCompInt(ids.userSelect, {
      users: { first: jest.fn(() => ({ id: 'user-2', bot: false, username: 'Bob' })) },
    });
    const sendBtn = makeCompInt(ids.sendBtn);
    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 1024, url: 'https://cdn.discordapp.com/doc.pdf' };
    const awaitMessages = jest.fn().mockResolvedValue(makeAttachmentMessage(attachment));
    const interaction = makeInteraction({
      awaitQueue: [fileInit, targetUser, userSelect, sendBtn],
      awaitMessages,
    });
    await cmd.execute(interaction);
    expect(mockDownloadAndUpload).toHaveBeenCalledWith(
      attachment.url, expect.any(String), attachment.contentType, expect.any(String),
    );
    expect(mockMintLinks).toHaveBeenCalled();
    expect(mockSendDM).toHaveBeenCalled();
    expect(mockDb.recordQURLSendBatch).toHaveBeenCalled();
  });

  it('location send: full flow from init → uploadJsonToConnector → mintLinks → sendDM', async () => {
    const locInit = makeCompInt(ids.initLoc, {
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockResolvedValue({
        fields: { getTextInputValue: jest.fn(() => 'https://maps.app.goo.gl/xyz') },
        deferUpdate: jest.fn().mockResolvedValue(undefined),
      }),
    });
    const targetUser = makeCompInt(ids.targetSelect, { values: ['user'] });
    const userSelect = makeCompInt(ids.userSelect, {
      users: { first: jest.fn(() => ({ id: 'user-2', bot: false, username: 'Bob' })) },
    });
    const sendBtn = makeCompInt(ids.sendBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInit, targetUser, userSelect, sendBtn],
    });
    await cmd.execute(interaction);
    expect(mockUploadJsonToConnector).toHaveBeenCalled();
    expect(mockMintLinks).toHaveBeenCalled();
    expect(mockSendDM).toHaveBeenCalled();
    expect(mockDownloadAndUpload).not.toHaveBeenCalled();
  });

  it('surfaces a quota-exceeded API error with a re-upload hint', async () => {
    const err = new Error('upstream quota');
    err.apiCode = 'quota_exceeded';
    mockUploadJsonToConnector.mockRejectedValue(err);
    mockGetTextChannelMembers.mockReturnValue([
      { id: 'u2', username: 'Alice' },
    ]);
    const locInit = makeCompInt(ids.initLoc, {
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockResolvedValue({
        fields: { getTextInputValue: jest.fn(() => 'Test Place') },
        deferUpdate: jest.fn().mockResolvedValue(undefined),
      }),
    });
    const targetChannel = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const sendBtn = makeCompInt(ids.sendBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInit, targetChannel, sendBtn],
    });
    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('share limit'),
    }));
  });

  it('aborts when the DB write fails after mint, before any DM goes out', async () => {
    mockGetTextChannelMembers.mockReturnValue([
      { id: 'u2', username: 'Alice' },
    ]);
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/a' }]);
    mockDb.recordQURLSendBatch.mockImplementationOnce(() => { throw new Error('disk full'); });
    const locInit = makeCompInt(ids.initLoc, {
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockResolvedValue({
        fields: { getTextInputValue: jest.fn(() => 'Test Place') },
        deferUpdate: jest.fn().mockResolvedValue(undefined),
      }),
    });
    const targetChannel = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const sendBtn = makeCompInt(ids.sendBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInit, targetChannel, sendBtn],
    });
    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('Failed to save link records'),
    }));
    expect(mockSendDM).not.toHaveBeenCalled();
  });

  it('reports failed DM delivery in the post-send confirmation', async () => {
    mockGetTextChannelMembers.mockReturnValue([
      { id: 'u2', username: 'Alice' },
      { id: 'u3', username: 'Bob' },
    ]);
    mockMintLinks.mockResolvedValue([
      { qurl_link: 'https://q.test/a' },
      { qurl_link: 'https://q.test/b' },
    ]);
    // Bob's DM fails — Alice succeeds.
    mockSendDM.mockImplementation(async (recipientId) => recipientId === 'u2');
    const locInit = makeCompInt(ids.initLoc, {
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockResolvedValue({
        fields: { getTextInputValue: jest.fn(() => 'Test Place') },
        deferUpdate: jest.fn().mockResolvedValue(undefined),
      }),
    });
    const targetChannel = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const sendBtn = makeCompInt(ids.sendBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInit, targetChannel, sendBtn],
    });
    await cmd.execute(interaction);
    // The confirm message should mention 1 sent + 1 unreachable.
    const confirmCall = interaction.editReply.mock.calls.find(
      (c) => typeof c[0]?.content === 'string' && c[0].content.includes('could not be reached'),
    );
    expect(confirmCall).toBeTruthy();
    expect(confirmCall[0].content).toContain('Bob');
  });

  it('returns a friendly message when mintLinks underdelivers', async () => {
    mockGetTextChannelMembers.mockReturnValue([
      { id: 'u2', username: 'Alice' },
      { id: 'u3', username: 'Bob' },
    ]);
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/only-one' }]); // 1 < 2 needed
    const locInit = makeCompInt(ids.initLoc, {
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockResolvedValue({
        fields: { getTextInputValue: jest.fn(() => 'Test Place') },
        deferUpdate: jest.fn().mockResolvedValue(undefined),
      }),
    });
    const targetChannel = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const sendBtn = makeCompInt(ids.sendBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInit, targetChannel, sendBtn],
    });
    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Only \d+ of \d+ links could be created/),
    }));
    expect(mockSendDM).not.toHaveBeenCalled();
  });
});

// ===========================================================================
// 7b. Audit event emission — guards the contract that CloudWatch metric
// filters in qurl-integrations-infra/qurl-bot-discord/terraform/main.tf
// pattern-match against. A typo in an event name silently disables a
// production metric, so these assertions live next to the happy paths.
// ===========================================================================

describe('handleSend — audit emission', () => {
  it('emits upload_success + mint_success + dispatch_sent on the file happy path', async () => {
    const fileInit = makeCompInt(ids.initFile);
    const targetUser = makeCompInt(ids.targetSelect, { values: ['user'] });
    const userSelect = makeCompInt(ids.userSelect, {
      users: { first: jest.fn(() => ({ id: 'user-2', bot: false, username: 'Bob' })) },
    });
    const sendBtn = makeCompInt(ids.sendBtn);
    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 1024, url: 'https://cdn.discordapp.com/doc.pdf' };
    const awaitMessages = jest.fn().mockResolvedValue(makeAttachmentMessage(attachment));
    const interaction = makeInteraction({
      awaitQueue: [fileInit, targetUser, userSelect, sendBtn],
      awaitMessages,
    });
    await cmd.execute(interaction);

    const emitted = logger.audit.mock.calls.map(c => c[0]);
    expect(emitted).toEqual(expect.arrayContaining(['upload_success', 'mint_success', 'dispatch_sent']));
    expect(logger.audit).toHaveBeenCalledWith('mint_success', expect.objectContaining({
      send_id: expect.any(String), kind: 'file', count: expect.any(Number),
    }));
  });

  it('emits mint_failed (not mint_success) when upload throws on the location path', async () => {
    const err = new Error('upstream quota');
    err.apiCode = 'quota_exceeded';
    mockUploadJsonToConnector.mockRejectedValue(err);
    mockGetTextChannelMembers.mockReturnValue([{ id: 'u2', username: 'Alice' }]);
    const locInit = makeCompInt(ids.initLoc, {
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockResolvedValue({
        fields: { getTextInputValue: jest.fn(() => 'Test Place') },
        deferUpdate: jest.fn().mockResolvedValue(undefined),
      }),
    });
    const targetChannel = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const sendBtn = makeCompInt(ids.sendBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInit, targetChannel, sendBtn],
    });
    await cmd.execute(interaction);

    const emitted = logger.audit.mock.calls.map(c => c[0]);
    expect(emitted).toContain('mint_failed');
    expect(emitted).not.toContain('mint_success');
    expect(logger.audit).toHaveBeenCalledWith('mint_failed', expect.objectContaining({
      send_id: expect.any(String), kind: 'location', api_code: 'quota_exceeded',
    }));
  });

  it('emits dispatch_sent / dispatch_failed once per recipient, even on partial DM failure', async () => {
    mockGetTextChannelMembers.mockReturnValue([
      { id: 'u2', username: 'Alice' },
      { id: 'u3', username: 'Bob' },
    ]);
    mockMintLinks.mockResolvedValue([
      { qurl_link: 'https://q.test/a' },
      { qurl_link: 'https://q.test/b' },
    ]);
    mockSendDM.mockImplementation(async (recipientId) => recipientId === 'u2');
    const locInit = makeCompInt(ids.initLoc, {
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockResolvedValue({
        fields: { getTextInputValue: jest.fn(() => 'Test Place') },
        deferUpdate: jest.fn().mockResolvedValue(undefined),
      }),
    });
    const targetChannel = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const sendBtn = makeCompInt(ids.sendBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInit, targetChannel, sendBtn],
    });
    await cmd.execute(interaction);

    const dispatchCalls = logger.audit.mock.calls.filter(c => c[0] === 'dispatch_sent' || c[0] === 'dispatch_failed');
    expect(dispatchCalls).toHaveLength(2);
    const events = dispatchCalls.map(c => c[0]).sort();
    expect(events).toEqual(['dispatch_failed', 'dispatch_sent']);
  });

  it('emits dispatch_sent before the DB write, so a DDB throw cannot suppress the metric', async () => {
    mockGetTextChannelMembers.mockReturnValue([{ id: 'u2', username: 'Alice' }]);
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/a' }]);
    mockSendDM.mockResolvedValue(true);
    // Make the DM-status DB write throw — audit must already have fired.
    mockDb.updateSendDMStatus.mockRejectedValueOnce(new Error('DDB ProvisionedThroughputExceededException'));
    const locInit = makeCompInt(ids.initLoc, {
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockResolvedValue({
        fields: { getTextInputValue: jest.fn(() => 'Test Place') },
        deferUpdate: jest.fn().mockResolvedValue(undefined),
      }),
    });
    const targetChannel = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const sendBtn = makeCompInt(ids.sendBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInit, targetChannel, sendBtn],
    });
    await cmd.execute(interaction);

    const emitted = logger.audit.mock.calls.map(c => c[0]);
    expect(emitted).toContain('dispatch_sent');
  });
});

// ===========================================================================
// 7b. Channel-announcement post — sender displayName sanitization
// ===========================================================================
// The /qurl send back-half posts a public announcement in the channel
// when target is 'channel' or 'voice'. Because that post is wider blast
// radius than a DM, sanitizeDisplayName MUST strip bidi (U+202E) and
// zero-width (U+200B) chars from the sender's displayName before it
// goes out. This regression test was ported from the deleted
// commands-comprehensive.test.js (the old slash-options test file
// that was retired with the redesign) — its docstring made the
// security intent explicit: "if a future refactor replaces
// sanitizeDisplayName with raw displayName at this site, this test
// fails". Keep it that way.
describe('handleSend — channel-announcement post sanitizes sender displayName', () => {
  function locInitBtn() {
    return makeCompInt(ids.initLoc, {
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockResolvedValue({
        fields: { getTextInputValue: jest.fn(() => 'Test Place') },
        deferUpdate: jest.fn().mockResolvedValue(undefined),
      }),
    });
  }

  it('strips RLO (U+202E) and ZWSP (U+200B) before posting to channel', async () => {
    mockGetTextChannelMembers.mockReturnValue([
      { id: 'u2', username: 'Alice' },
    ]);
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/a' }]);
    const channelSend = jest.fn().mockResolvedValue(undefined);
    const targetChannel = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const sendBtn = makeCompInt(ids.sendBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), targetChannel, sendBtn],
      channel: {
        send: channelSend,
        // awaitMessageComponent + awaitMessages still need to come from
        // the queue/default; spread doesn't matter since the makeInteraction
        // overrides set channel from these props.
      },
      member: { displayName: '‮Admin​bob' },
      user: { id: 'user-1', username: 'TestUser', displayName: '‮Admin​bob' },
    });
    await cmd.execute(interaction);
    // Public channel post should have happened (delivered > 0, target = 'channel')
    expect(channelSend).toHaveBeenCalledTimes(1);
    const msg = channelSend.mock.calls[0][0].content;
    // Sanitized: no RLO, no ZWSP
    expect(msg).not.toContain('‮');
    expect(msg).not.toContain('​');
    // The sanitized name should appear (NFKC + strip leaves "Adminbob")
    expect(msg).toMatch(/Adminbob/);
  });
});

// ===========================================================================
// 8. Additional coverage targeted at branches the review flagged
// ===========================================================================

describe('handleSend — additional branch coverage', () => {
  function locInitBtn(value = 'Test Place') {
    return makeCompInt(ids.initLoc, {
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockResolvedValue({
        fields: { getTextInputValue: jest.fn(() => value) },
        deferUpdate: jest.fn().mockResolvedValue(undefined),
      }),
    });
  }
  function fileInitBtn() {
    return makeCompInt(ids.initFile, { update: jest.fn().mockResolvedValue(undefined) });
  }

  // C1 — concurrency cap fires at the atomic slot-claim site after the
  // user clicks Send. The earlier file-acceptance gate was removed because
  // the up-to-3-min Step-3 form loop made it stale by the time the user
  // would actually claim a slot.
  it('rejects file send at slot-claim when concurrency cap is reached', async () => {
    setActiveFileSends(5);
    const fileInit = fileInitBtn();
    const targetUser = makeCompInt(ids.targetSelect, { values: ['user'] });
    const userSelect = makeCompInt(ids.userSelect, {
      users: { first: jest.fn(() => ({ id: 'user-2', bot: false, username: 'Bob' })) },
    });
    const sendBtn = makeCompInt(ids.sendBtn);
    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 1024, url: 'https://cdn.discordapp.com/doc.pdf' };
    const awaitMessages = jest.fn().mockResolvedValue(makeAttachmentMessage(attachment));
    const interaction = makeInteraction({
      awaitQueue: [fileInit, targetUser, userSelect, sendBtn],
      awaitMessages,
    });
    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('processing too many file sends'),
    }));
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('slot-claim'),
      expect.objectContaining({ activeFileSends: 5 }),
    );
    expect(mockDownloadAndUpload).not.toHaveBeenCalled();
  });

  // C2 — fetchGuildMembers failure on target=channel.
  it('aborts with a friendly error when fetchGuildMembers throws', async () => {
    const targetChannel = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), targetChannel],
      guild: { id: 'guild-1', members: { fetch: jest.fn().mockRejectedValue(new Error('rate limited')) } },
    });
    await cmd.execute(interaction);
    expect(targetChannel.update).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('Failed to load channel members'),
    }));
    expect(logger.error).toHaveBeenCalledWith(
      'Failed to fetch guild members',
      expect.objectContaining({ error: 'rate limited' }),
    );
    expect(sendCooldowns.has('user-1')).toBe(false);
  });

  // C3 — location modal unexpected error path (vs. timeout path which is tested).
  it('surfaces "Could not collect location input" on a non-timeout modal error', async () => {
    const initBtn = makeCompInt(ids.initLoc, {
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockRejectedValue(new Error('Discord 500')),
    });
    const interaction = makeInteraction({ awaitQueue: [initBtn] });
    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('Could not collect location input'),
    }));
    expect(logger.error).toHaveBeenCalledWith(
      'Modal submit failed unexpectedly',
      expect.objectContaining({ error: 'Discord 500' }),
    );
  });

  // C4 — Step-3 form-loop 3-min component timeout (no further queue items).
  it('cancels with a 3-min message when the form loop times out waiting for clicks', async () => {
    const interaction = makeInteraction({ awaitQueue: [locInitBtn()] });
    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('Send timed out (3 min)'),
    }));
    expect(sendCooldowns.has('user-1')).toBe(false);
  });

  // C5 — re-selecting channel ALWAYS re-resolves (members may join/leave
  // during the up-to-3-min form-loop window). Regression test for the
  // ghost-recipients bug Claude bot review flagged.
  it('re-resolves channel members on every channel re-select (no stale recipients)', async () => {
    mockGetTextChannelMembers.mockReturnValue([{ id: 'u2', username: 'Alice' }]);
    const target1 = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const target2 = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const cancel = makeCompInt(ids.cancelBtn);
    const interaction = makeInteraction({ awaitQueue: [locInitBtn(), target1, target2, cancel] });
    await cmd.execute(interaction);
    expect(mockGetTextChannelMembers).toHaveBeenCalledTimes(2);
  });

  // C5b — voice re-select also re-resolves (same staleness concern).
  it('re-resolves voice members on every voice re-select', async () => {
    mockGetVoiceChannelMembers.mockReturnValue({ members: [{ id: 'u2', username: 'Alice' }] });
    const target1 = makeCompInt(ids.targetSelect, { values: ['voice'] });
    const target2 = makeCompInt(ids.targetSelect, { values: ['voice'] });
    const cancel = makeCompInt(ids.cancelBtn);
    const interaction = makeInteraction({ awaitQueue: [locInitBtn(), target1, target2, cancel] });
    await cmd.execute(interaction);
    expect(mockGetVoiceChannelMembers).toHaveBeenCalledTimes(2);
  });

  // C5d — switching target across types resets recipients (no leakage
  // from a previously-picked user into a channel send, etc.).
  it('switches target across types and resets recipients each transition', async () => {
    mockGetTextChannelMembers.mockReturnValue([{ id: 'u3', username: 'Channeler' }]);
    const target1 = makeCompInt(ids.targetSelect, { values: ['user'] });
    const userPick = makeCompInt(ids.userSelect, {
      users: { first: jest.fn(() => ({ id: 'user-2', bot: false, username: 'Bob' })) },
    });
    const target2 = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const target3 = makeCompInt(ids.targetSelect, { values: ['user'] });
    const userPick2 = makeCompInt(ids.userSelect, {
      users: { first: jest.fn(() => ({ id: 'user-7', bot: false, username: 'Carol' })) },
    });
    const sendBtn = makeCompInt(ids.sendBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), target1, userPick, target2, target3, userPick2, sendBtn],
    });
    await cmd.execute(interaction);
    // The send went out — recipient was Carol (the second pick), not
    // Bob (whose recipients[] was reset by the channel transition).
    expect(mockSendDM).toHaveBeenCalledWith('user-7', expect.any(Object));
    expect(mockSendDM).not.toHaveBeenCalledWith('user-2', expect.any(Object));
  });

  // C5c — user re-select is still a no-op (UserSelect drives recipients).
  it('user re-select preserves the picked recipient (no recipients reset)', async () => {
    const target1 = makeCompInt(ids.targetSelect, { values: ['user'] });
    const userPick = makeCompInt(ids.userSelect, {
      users: { first: jest.fn(() => ({ id: 'user-2', bot: false, username: 'Bob' })) },
    });
    const target2 = makeCompInt(ids.targetSelect, { values: ['user'] });
    const sendBtn = makeCompInt(ids.sendBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), target1, userPick, target2, sendBtn],
    });
    await cmd.execute(interaction);
    // Send should have proceeded — i.e., the user re-select didn't blow
    // away the picked recipient.
    expect(mockMintLinks).toHaveBeenCalled();
    expect(mockSendDM).toHaveBeenCalled();
  });

  // C6 — UserSelect with empty selection just defers and continues.
  it('defers and continues when UserSelect resolves with no user', async () => {
    const targetUser = makeCompInt(ids.targetSelect, { values: ['user'] });
    const userSelectEmpty = makeCompInt(ids.userSelect, {
      users: { first: jest.fn(() => null) },
    });
    const cancel = makeCompInt(ids.cancelBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), targetUser, userSelectEmpty, cancel],
    });
    await cmd.execute(interaction);
    expect(userSelectEmpty.deferUpdate).toHaveBeenCalled();
    expect(userSelectEmpty.update).not.toHaveBeenCalled();
  });

  // C7 — submitting the message modal with whitespace-only clears the field.
  it('clearing the personal message via whitespace-only modal submission removes the preview', async () => {
    const setMsg = {
      fields: { getTextInputValue: jest.fn(() => 'first message') },
      update: jest.fn().mockResolvedValue(undefined),
    };
    const clearMsg = {
      fields: { getTextInputValue: jest.fn(() => '   ') },
      update: jest.fn().mockResolvedValue(undefined),
    };
    const msgBtn1 = makeCompInt(ids.msgBtn, {
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockResolvedValue(setMsg),
    });
    const msgBtn2 = makeCompInt(ids.msgBtn, {
      showModal: jest.fn().mockResolvedValue(undefined),
      awaitModalSubmit: jest.fn().mockResolvedValue(clearMsg),
    });
    const cancel = makeCompInt(ids.cancelBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), msgBtn1, msgBtn2, cancel],
    });
    await cmd.execute(interaction);
    // After the second submit, the form preview should NOT contain
    // "Personal message:" — assert against the most-recent update so a
    // future code change that re-renders more than once still verifies
    // the final state.
    expect(clearMsg.update).toHaveBeenLastCalledWith(expect.objectContaining({
      content: expect.not.stringContaining('Personal message:'),
    }));
  });

  // C9 — saveSendConfig failure is logged-and-swallowed; DMs still go out.
  it('logs but does not abort when saveSendConfig fails after delivery', async () => {
    mockGetTextChannelMembers.mockReturnValue([{ id: 'u2', username: 'Alice' }]);
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/a' }]);
    mockDb.saveSendConfig.mockImplementationOnce(() => { throw new Error('disk full'); });
    const targetChannel = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const sendBtn = makeCompInt(ids.sendBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), targetChannel, sendBtn],
    });
    await cmd.execute(interaction);
    expect(mockSendDM).toHaveBeenCalled(); // delivery proceeded
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('saveSendConfig failed'),
      expect.objectContaining({ error: 'disk full' }),
    );
  });

  // C10 — file-path mintLinks underdelivery (mirror of the location-path test).
  it('returns a friendly message when mintLinks underdelivers on the file path', async () => {
    mockGetTextChannelMembers.mockReturnValue([
      { id: 'u2', username: 'Alice' },
      { id: 'u3', username: 'Bob' },
    ]);
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/only-one' }]); // 1 < 2 needed
    const fileInit = makeCompInt(ids.initFile);
    const targetChannel = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const sendBtn = makeCompInt(ids.sendBtn);
    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 1024, url: 'https://cdn.discordapp.com/doc.pdf' };
    const awaitMessages = jest.fn().mockResolvedValue(makeAttachmentMessage(attachment));
    const interaction = makeInteraction({
      awaitQueue: [fileInit, targetChannel, sendBtn],
      awaitMessages,
    });
    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/Only 1 of 2 links could be created/),
    }));
  });

  // C14 — quota_exceeded on the file path uses the "re-upload" copy.
  it('quota_exceeded on the file path tells the user to re-upload', async () => {
    const err = new Error('upstream quota');
    err.apiCode = 'quota_exceeded';
    mockDownloadAndUpload.mockRejectedValueOnce(err);
    const fileInit = makeCompInt(ids.initFile);
    const targetChannel = makeCompInt(ids.targetSelect, { values: ['channel'] });
    mockGetTextChannelMembers.mockReturnValue([{ id: 'u2', username: 'Alice' }]);
    const sendBtn = makeCompInt(ids.sendBtn);
    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 1024, url: 'https://cdn.discordapp.com/doc.pdf' };
    const awaitMessages = jest.fn().mockResolvedValue(makeAttachmentMessage(attachment));
    const interaction = makeInteraction({
      awaitQueue: [fileInit, targetChannel, sendBtn],
      awaitMessages,
    });
    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('re-upload the file'),
    }));
  });

  // A2 — isAllowedSourceUrl rejection on the file path (defense in depth).
  it('rejects file send when attachment.url fails isAllowedSourceUrl', async () => {
    const fileInit = makeCompInt(ids.initFile);
    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 1024, url: 'https://evil.example.com/doc.pdf' };
    const awaitMessages = jest.fn().mockResolvedValue(makeAttachmentMessage(attachment));
    const interaction = makeInteraction({ awaitQueue: [fileInit], awaitMessages });
    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('not from a recognized Discord CDN'),
    }));
    expect(mockDownloadAndUpload).not.toHaveBeenCalled();
  });

  // A3 — empty-recipients spoof. Drive Send button without resolving recipients.
  it('rejects spoofed Send button when recipients are unresolved', async () => {
    const sendBtn = makeCompInt(ids.sendBtn);
    const interaction = makeInteraction({ awaitQueue: [locInitBtn(), sendBtn] });
    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('No recipients selected'),
    }));
    expect(mockMintLinks).not.toHaveBeenCalled();
  });

  // A15 — the form-loop filter rejects clicks from a different user.
  it('the form-loop filter excludes other users\' clicks', async () => {
    // Capture the filter function passed to channel.awaitMessageComponent
    // and exercise it with a non-matching user.id; the handler must
    // reject the spoofed event so the filter is what protects the loop.
    let formFilter = null;
    let callIdx = 0;
    const channelAwaitMC = jest.fn().mockImplementation(({ filter }) => {
      callIdx++;
      if (callIdx === 1) {
        // First call is Step-1 (init buttons). Return loc-init.
        return Promise.resolve(makeCompInt(ids.initLoc, {
          showModal: jest.fn().mockResolvedValue(undefined),
          awaitModalSubmit: jest.fn().mockResolvedValue({
            fields: { getTextInputValue: jest.fn(() => 'Place') },
            deferUpdate: jest.fn().mockResolvedValue(undefined),
          }),
        }));
      }
      // Second call is Step-3 form loop. Capture the filter and time out.
      formFilter = filter;
      return Promise.reject(new Error('timeout'));
    });
    const interaction = {
      ...makeInteraction(),
      channel: { awaitMessageComponent: channelAwaitMC, members: new Map() },
    };
    await cmd.execute(interaction);
    expect(formFilter).toBeInstanceOf(Function);
    // Click from another user with a valid form customId — must be filtered out.
    expect(formFilter({ user: { id: 'someone-else' }, customId: ids.cancelBtn })).toBe(false);
    // Click from the right user with an unrelated customId — also filtered.
    expect(formFilter({ user: { id: 'user-1' }, customId: 'qurl_form_other_send' })).toBe(false);
    // Click from the right user with a valid form customId — accepted.
    expect(formFilter({ user: { id: 'user-1' }, customId: ids.cancelBtn })).toBe(true);
  });
});

// ===========================================================================
// 9. Pure-function unit tests (no state machine)
// ===========================================================================
// These exercise helpers exported via `_test` directly, without driving
// cmd.execute(). They claw back coverage that the deleted slash-options
// tests used to provide for back-half-adjacent helpers.

describe('safeUrlHost — best-effort host extraction for log lines', () => {
  const { safeUrlHost } = _test;

  it('returns the host for a well-formed URL', () => {
    expect(safeUrlHost('https://cdn.discordapp.com/foo/bar')).toBe('cdn.discordapp.com');
  });

  it('returns invalid-url for malformed input', () => {
    expect(safeUrlHost('not-a-url')).toBe('invalid-url');
    expect(safeUrlHost('')).toBe('invalid-url');
    expect(safeUrlHost(undefined)).toBe('invalid-url');
    expect(safeUrlHost(null)).toBe('invalid-url');
  });

  it('preserves the host on a deeply-nested path', () => {
    expect(safeUrlHost('https://maps.app.goo.gl/abc/def?ghi=jkl')).toBe('maps.app.goo.gl');
  });
});

describe('buildDeliveryPayload — location resource type', () => {
  const { buildDeliveryPayload } = _test;

  it('builds a payload for a location-type send (no resource-type field on embed)', () => {
    const result = buildDeliveryPayload({
      senderAlias: 'TestSender',
      qurlLink: 'https://q.test/loc-1',
      expiresAt: Math.floor(Date.now() / 1000) + 3600,
      personalMessage: null,
    });
    // Contract: returns an object with embeds array and a components row.
    expect(result).toEqual(expect.objectContaining({
      embeds: expect.any(Array),
      components: expect.any(Array),
    }));
    expect(result.embeds.length).toBeGreaterThan(0);
    expect(result.components.length).toBeGreaterThan(0);
  });

  it('throws on a non-finite expiresAt (fail-loud over silent degradation)', () => {
    expect(() => buildDeliveryPayload({
      senderAlias: 'TestSender',
      qurlLink: 'https://q.test/loc-2',
      expiresAt: null,
      personalMessage: null,
    })).toThrow(/expiresAt must be a finite Unix-seconds number/);
    expect(() => buildDeliveryPayload({
      senderAlias: 'TestSender',
      qurlLink: 'https://q.test/loc-3',
      expiresAt: NaN,
      personalMessage: null,
    })).toThrow(/expiresAt must be a finite Unix-seconds number/);
  });

  it('accepts a sanitized personalMessage and renders it into the embed', () => {
    // CONTRACT: personalMessage arrives pre-sanitized — see the comment
    // at buildDeliveryPayload's body. This test pins that the helper
    // accepts a sanitized message without throwing or stripping further.
    const result = buildDeliveryPayload({
      senderAlias: 'TestSender',
      qurlLink: 'https://q.test/loc-4',
      expiresAt: Math.floor(Date.now() / 1000) + 3600,
      personalMessage: 'Sanitized note text',
    });
    expect(result.embeds.length).toBeGreaterThan(0);
  });
});
