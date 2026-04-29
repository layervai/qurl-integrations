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
}));

jest.mock('discord.js', () => {
  const makeChainable = (extra = {}) => {
    const obj = {
      setCustomId: jest.fn().mockReturnThis(),
      setLabel: jest.fn().mockReturnThis(),
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
    ChannelType: { GuildText: 0, GuildVoice: 2, GuildStageVoice: 13 },
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
const originalRandomBytes = crypto.randomBytes;
const NONCE = 'deadbeef01234567';
crypto.randomBytes = jest.fn((size) => (size === 8 ? Buffer.from(NONCE, 'hex') : originalRandomBytes(size)));
crypto.randomUUID = jest.fn(() => 'mock-send-uuid');

const { commands, _test } = require('../src/commands');
const logger = require('../src/logger');
const { sendCooldowns, setCooldown, setActiveFileSends, memberFetchCache } = _test;

afterAll(() => {
  // Restore the singleton crypto.randomBytes so subsequent test files in
  // the same Jest worker (or future shared-runner mode) see the real impl.
  crypto.randomBytes = originalRandomBytes;
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

function makeInteraction({ awaitQueue = [], awaitMessages, channel = {}, guild = {}, ...overrides } = {}) {
  // Each call to channel.awaitMessageComponent pulls from awaitQueue in
  // FIFO order. Items can be a value (resolved) or { reject: err } (rejected).
  let queueIdx = 0;
  const channelAwaitMC = jest.fn().mockImplementation(() => {
    const next = awaitQueue[queueIdx++];
    if (!next) return Promise.reject(new Error('timeout'));
    if (next.reject) return Promise.reject(next.reject);
    return Promise.resolve(next);
  });
  return {
    user: { id: 'user-1', username: 'TestUser' },
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
      awaitMessageComponent: channelAwaitMC,
      awaitMessages: awaitMessages || jest.fn().mockRejectedValue(new Error('timeout')),
      members: new Map(),
      ...channel,
    },
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

  it('cancels when no file is dropped within 60s', async () => {
    const awaitMessages = jest.fn().mockRejectedValue(new Error('time'));
    const interaction = makeInteraction({
      awaitQueue: [fileInitBtn()],
      awaitMessages,
    });
    await cmd.execute(interaction);
    expect(awaitMessages).toHaveBeenCalled();
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('No file received'),
    }));
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

  // C1 — concurrency cap fires both at file-acceptance and at slot-claim.
  it('rejects file send up-front when concurrency cap is already reached', async () => {
    setActiveFileSends(5);
    const attachment = { name: 'doc.pdf', contentType: 'application/pdf', size: 1024, url: 'https://cdn.discordapp.com/doc.pdf' };
    const awaitMessages = jest.fn().mockResolvedValue(makeAttachmentMessage(attachment));
    const interaction = makeInteraction({ awaitQueue: [fileInitBtn()], awaitMessages });
    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('processing too many file sends'),
    }));
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('concurrency cap reached'),
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

  // C4 — Step-3 form-loop 5-min component timeout (no further queue items).
  it('cancels with a 5-min message when the form loop times out waiting for clicks', async () => {
    const interaction = makeInteraction({ awaitQueue: [locInitBtn()] });
    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('Send timed out (5 min)'),
    }));
    expect(sendCooldowns.has('user-1')).toBe(false);
  });

  // C5 — re-selecting the same target is a no-op (no second member fetch).
  it('does not re-fetch members when target is re-selected to the same value', async () => {
    mockGetTextChannelMembers.mockReturnValue([{ id: 'u2', username: 'Alice' }]);
    const target1 = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const target2 = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const cancel = makeCompInt(ids.cancelBtn);
    const interaction = makeInteraction({ awaitQueue: [locInitBtn(), target1, target2, cancel] });
    await cmd.execute(interaction);
    expect(mockGetTextChannelMembers).toHaveBeenCalledTimes(1);
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
    // "Personal message:" — assert against the LATEST update.
    const lastUpdateContent = clearMsg.update.mock.calls[0][0].content;
    expect(lastUpdateContent).not.toContain('Personal message:');
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
