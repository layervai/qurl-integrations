/**
 * /qurl send button-driven state-machine tests.
 *
 * Replaces the slash-options-shape coverage that landed in coverage-boost
 * and commands-comprehensive (now describe.skip'd). Walks the new
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
const { sendCooldowns, setCooldown } = _test;

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

  it('rejects when resolved recipients exceed QURL_SEND_MAX_RECIPIENTS', async () => {
    // 51 members > config 50 cap.
    const many = Array.from({ length: 51 }, (_, i) => ({ id: `u${i}`, username: `U${i}` }));
    mockGetTextChannelMembers.mockReturnValue(many);
    const targetChannel = makeCompInt(ids.targetSelect, { values: ['channel'] });
    const sendBtn = makeCompInt(ids.sendBtn);
    const interaction = makeInteraction({
      awaitQueue: [locInitBtn(), targetChannel, sendBtn],
    });
    await cmd.execute(interaction);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('per-send cap'),
    }));
    // mintLinks should NOT have been called — guard fires before back-half
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
