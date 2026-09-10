

jest.mock('../src/config', () => ({
  QURL_API_KEY: 'test-api-key',
  QURL_ENDPOINT: 'https://api.test.local',
  CONNECTOR_URL: 'https://connector.test.local',
  GOOGLE_MAPS_API_KEY: 'test-google-key',
  QURL_SEND_COOLDOWN_MS: 30000,
  QURL_DETECT_COOLDOWN_MS: 30000,
  QURL_SEND_MAX_RECIPIENTS: 50,
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
      setDefaultValues: jest.fn().mockReturnThis(),
      addDefaultUsers: jest.fn().mockReturnThis(),
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
    MentionableSelectMenuBuilder: jest.fn().mockImplementation(() => makeChainable()),
    ModalBuilder: jest.fn().mockImplementation(() => makeChainable()),
    TextInputBuilder: jest.fn().mockImplementation(() => makeChainable()),
    TextInputStyle: { Short: 1, Paragraph: 2 },
    AttachmentBuilder: jest.fn().mockImplementation((buf, opts) => ({ buf, name: opts?.name })),
    PermissionFlagsBits: { ManageRoles: 1n, Administrator: 8n },
  };
});

const mockDb = {
  recordQURLSendBatch: jest.fn(),
  updateSendDMStatus: jest.fn(),
  markSendDMDelivered: jest.fn(),
  getRecentSends: jest.fn(() => []),
  getSendResourceIds: jest.fn(() => []),
  getSendItems: jest.fn(() => []),
  markSendRevoking: jest.fn().mockResolvedValue(true),
  markSendRevoked: jest.fn(),
  getSendConfig: jest.fn(),
  saveSendConfig: jest.fn(),
  saveSendConfirmState: jest.fn().mockResolvedValue(undefined),
  getQurlViews: jest.fn(async () => new Map()),
  recordQurlView: jest.fn(),
  tryAdvanceRenderedCount: jest.fn().mockResolvedValue(true),
  getSendRenderedCount: jest.fn().mockResolvedValue(0),
  markConfirmTerminal: jest.fn().mockResolvedValue(undefined),
};
jest.mock('../src/store', () => mockDb);

const mockSendDM = jest.fn().mockResolvedValue({ ok: true, channelId: 'dm-c', messageId: 'dm-m' });
const mockEditDM = jest.fn().mockResolvedValue({ ok: true });
const mockSendChannelMessage = jest.fn().mockResolvedValue({ ok: true, messageId: 'ch-m' });
jest.mock('../src/discord', () => ({
  assignContributorRole: jest.fn(),
  notifyPRMerge: jest.fn(),
  notifyBadgeEarned: jest.fn(),
  postGoodFirstIssue: jest.fn(),
  postReleaseAnnouncement: jest.fn(),
  postStarMilestone: jest.fn(),
  postToGitHubFeed: jest.fn(),
  sendDM: mockSendDM,
}));
jest.mock('../src/discord-rest', () => ({
  editDM: mockEditDM,
  sendChannelMessage: mockSendChannelMessage,
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

const mockDeleteLink = jest.fn();
jest.mock('../src/qurl', () => ({
  createOneTimeLink: jest.fn(),
  deleteLink: mockDeleteLink,
}));

jest.mock('../src/places', () => ({ searchPlaces: jest.fn().mockResolvedValue([]) }));

jest.mock('../src/flow-state', () => ({
  createFlow: jest.fn(),
  loadFlow: jest.fn(),
  transitionFlow: jest.fn(),
  deleteFlow: jest.fn(),
}));

jest.mock('../src/utils/time', () => {
  const actual = jest.requireActual('../src/utils/time');
  return {
    ...actual,
    expiryToMs: jest.fn(actual.expiryToMs),
  };
});
const mockTime = require('../src/utils/time');

const { _test } = require('../src/commands');
const logger = require('../src/logger');
const { resourceIdLogRef } = require('../src/utils/resource-id');
const {
  monitorLinkStatus,
  revokeAllLinks,
  renderRevokeMsg,
  safeRevokeHeader,
  renderSendConfirm,
  renderViewCounter,
  REVOKE_TRUNC_LIMIT,
  handleAddRecipients,
  buildDeliveryEmbed,
  packBulkDeliveryComponents,
  mintLinksInBatches,
  activeMonitors,
  executeSendPipeline,
  persistDispatchResult,
  clearCooldown,
  revokingSendLocks,
} = _test;

function makeInteraction(overrides = {}) {
  return {
    user: { id: 'sender-1', username: 'Sender' },
    channelId: 'ch-1',
    editReply: jest.fn().mockResolvedValue(undefined),
    member: { displayName: 'Sender' },
    ...overrides,
  };
}

const DEFAULT_ATTACHMENT = Object.freeze({ url: 'https://cdn.discordapp.com/x', name: 'x.png', contentType: 'image/png' });
function makePipelineParams(overrides = {}) {
  return {
    apiKey: 'apikey',
    resourceType: 'file',
    attachment: { ...DEFAULT_ATTACHMENT },
    locationUrl: null,
    locationName: null,
    recipients: [{ id: 'u1', username: 'u1' }],
    expiresIn: '24h',
    selfDestructSeconds: null,
    personalMessage: null,
    sendNonce: 'nonce',
    ...overrides,
  };
}

function defer() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

async function waitForMicrotaskExpectation(assertion, attempts = 10) {
  let lastError;
  for (let i = 0; i < attempts; i++) {
    try {
      assertion();
      return;
    } catch (err) {
      lastError = err;
      await Promise.resolve();
    }
  }
  throw lastError;
}

async function expectGateAccepts(params, ...gateRejectionRegexes) {
  expect.hasAssertions();
  const interaction = makeInteraction();
  try {
    await executeSendPipeline(interaction, params);
    expect(true).toBe(true);
  } catch (err) {
    for (const re of gateRejectionRegexes) {
      expect(err.message).not.toMatch(re);
    }
  }
}

const POLL_INTERVAL = 15000;

beforeEach(() => {
  jest.clearAllMocks();
  mockDb.markSendRevoking.mockReset();
  mockDb.markSendRevoking.mockResolvedValue(true);
  mockDb.markSendRevoked.mockReset();
  mockDb.markSendRevoked.mockResolvedValue(true);
  revokingSendLocks.clear();
  mockDb.getQurlViews.mockReset();
  mockDb.getQurlViews.mockResolvedValue(new Map());
  mockDb.tryAdvanceRenderedCount.mockReset();
  mockDb.tryAdvanceRenderedCount.mockResolvedValue(true);
  mockDb.getSendRenderedCount.mockReset();
  mockDb.getSendRenderedCount.mockResolvedValue(0);
  for (const m of Array.from(activeMonitors)) m.stop();
});

const TWO_LINK_SET = [
  { resourceId: 'res-1', qurlId: 'q_aaaaaaaaaa1', qurlLink: 'https://q.test/1', recipientId: 'r1' },
  { resourceId: 'res-1', qurlId: 'q_aaaaaaaaaa2', qurlLink: 'https://q.test/2', recipientId: 'r2' },
];
const ONE_LINK_SET = [
  { resourceId: 'res-1', qurlId: 'q_aaaaaaaaaa1', qurlLink: 'https://q.test/1', recipientId: 'r1' },
];

describe('monitorLinkStatus — view-counter render from qurl_views', () => {
  beforeEach(() => { jest.useFakeTimers(); });
  afterEach(() => { jest.useRealTimers(); });

  it('initial getFullMsg() shows 0 viewed / N pending before any webhook lands', () => {
    const monitor = monitorLinkStatus(
      'send-1', makeInteraction(),
      TWO_LINK_SET,
      [{ id: 'r1', username: 'Alice' }, { id: 'r2', username: 'Bob' }],
      '1m', 'Sent to 2 users', { components: [] }, 2,
    );
    expect(monitor.getFullMsg()).toBe('Sent to 2 users\n👀 0 viewed / 2 pending');
    monitor.stop();
  });

  it('webhook-fed view advances `viewed` counter on next tick', async () => {
    const interaction = makeInteraction();
    const monitor = monitorLinkStatus(
      'send-1', interaction,
      TWO_LINK_SET,
      [{ id: 'r1', username: 'Alice' }, { id: 'r2', username: 'Bob' }],
      '1m', 'Sent to 2 users', { components: [] }, 2,
    );

    mockDb.getQurlViews.mockResolvedValueOnce(new Map([
      ['q_aaaaaaaaaa1', { accessCount: 1, consumed: false }],
    ]));
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    expect(interaction.editReply).toHaveBeenCalled();
    expect(monitor.getFullMsg()).toBe('Sent to 2 users\n👀 1 viewed / 1 pending');
    monitor.stop();
  });

  it('all viewed → final-message edit clears components', async () => {
    const interaction = makeInteraction();
    const monitor = monitorLinkStatus(
      'send-1', interaction,
      ONE_LINK_SET,
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent', { components: [] }, 1,
    );

    mockDb.getQurlViews.mockResolvedValueOnce(new Map([
      ['q_aaaaaaaaaa1', { accessCount: 1, consumed: true }],
    ]));
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL); // termination tick

    const lastCall = interaction.editReply.mock.calls.at(-1);
    expect(lastCall?.[0]).toEqual(expect.objectContaining({ components: [] }));
    monitor.stop();
  });

  it('BatchGet error logs but does not crash the setInterval', async () => {
    const interaction = makeInteraction();
    mockDb.getQurlViews.mockRejectedValueOnce(new Error('DDB throttled'));

    const monitor = monitorLinkStatus(
      'send-1', interaction,
      ONE_LINK_SET,
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent', { components: [] }, 1,
    );
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    expect(logger.error).toHaveBeenCalledWith('Link monitor poll failed', expect.any(Object));
    expect(interaction.editReply).not.toHaveBeenCalled();
    monitor.stop();
  });
});

describe('monitorLinkStatus — shared monotonic floor with the webhook fast-path', () => {
  beforeEach(() => { jest.useFakeTimers(); });
  afterEach(() => { jest.useRealTimers(); });

  it('advances last_rendered_count to the DISPLAYED count after a confirmed counter render', async () => {
    const interaction = makeInteraction();
    const monitor = monitorLinkStatus(
      'send-1', interaction,
      TWO_LINK_SET,
      [{ id: 'r1', username: 'Alice' }, { id: 'r2', username: 'Bob' }],
      '1m', 'Sent to 2 users', { components: [] }, 2,
    );
    mockDb.getQurlViews.mockResolvedValueOnce(new Map([
      ['q_aaaaaaaaaa1', { accessCount: 1, consumed: false }],
    ]));
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    expect(interaction.editReply).toHaveBeenCalled();
    expect(mockDb.tryAdvanceRenderedCount).toHaveBeenCalledWith('send-1', 1);
    const editOrder = interaction.editReply.mock.invocationCallOrder[0];
    const advOrder = mockDb.tryAdvanceRenderedCount.mock.invocationCallOrder[0];
    expect(editOrder).toBeLessThan(advOrder);
    monitor.stop();
  });

  it('clamps poll render to the persisted floor so it cannot step backwards after a fast-path edit', async () => {
    const interaction = makeInteraction();
    const monitor = monitorLinkStatus(
      'send-1', interaction,
      [
        { resourceId: 'res-1', qurlId: 'q_aaaaaaaaaa1', qurlLink: 'https://q.test/1', recipientId: 'r1' },
        { resourceId: 'res-1', qurlId: 'q_aaaaaaaaaa2', qurlLink: 'https://q.test/2', recipientId: 'r2' },
        { resourceId: 'res-1', qurlId: 'q_aaaaaaaaaa3', qurlLink: 'https://q.test/3', recipientId: 'r3' },
      ],
      [
        { id: 'r1', username: 'Alice' },
        { id: 'r2', username: 'Bob' },
        { id: 'r3', username: 'Carol' },
      ],
      '1m', 'Sent to 3 users', { components: [] }, 3,
    );
    mockDb.getQurlViews.mockResolvedValueOnce(new Map([
      ['q_aaaaaaaaaa1', { accessCount: 1, consumed: false }],
    ]));
    mockDb.getSendRenderedCount.mockResolvedValueOnce(2);
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('👀 2 viewed / 1 pending'),
    }));
    expect(mockDb.tryAdvanceRenderedCount).toHaveBeenCalledWith('send-1', 2);
    expect(monitor.getFullMsg()).toBe('Sent to 3 users\n👀 1 viewed / 2 pending');
    monitor.stop();
  });

  it('falls back to the local poll count when the persisted floor read fails', async () => {
    const interaction = makeInteraction();
    const monitor = monitorLinkStatus(
      'send-1', interaction,
      TWO_LINK_SET,
      [{ id: 'r1', username: 'Alice' }, { id: 'r2', username: 'Bob' }],
      '1m', 'Sent to 2 users', { components: [] }, 2,
    );
    mockDb.getQurlViews.mockResolvedValueOnce(new Map([
      ['q_aaaaaaaaaa1', { accessCount: 1, consumed: false }],
    ]));
    mockDb.getSendRenderedCount.mockRejectedValueOnce(new Error('DDB floor read failed'));
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('👀 1 viewed / 1 pending'),
    }));
    expect(logger.debug).toHaveBeenCalledWith(
      'Monitor floor-read failed; rendering local poll count',
      { sendId: 'send-1', error: 'DDB floor read failed' },
    );
    expect(mockDb.tryAdvanceRenderedCount).toHaveBeenCalledWith('send-1', 1);
    monitor.stop();
  });

  it('COMMIT-AFTER-EDIT: a failed poll render does NOT advance the floor (mirror of the fast-path fence)', async () => {
    const interaction = makeInteraction({
      editReply: jest.fn().mockRejectedValue(new Error('Discord 500')),
    });
    const monitor = monitorLinkStatus(
      'send-1', interaction,
      ONE_LINK_SET,
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent', { components: [] }, 1,
    );
    mockDb.getQurlViews.mockResolvedValueOnce(new Map([
      ['q_aaaaaaaaaa1', { accessCount: 1, consumed: false }],
    ]));
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    expect(interaction.editReply).toHaveBeenCalled(); // attempted…
    expect(mockDb.tryAdvanceRenderedCount).not.toHaveBeenCalled(); // …but failed → no advance
    monitor.stop();
  });

  it('does NOT advance the floor on a degraded send (no counter displayed)', async () => {
    const interaction = makeInteraction();
    const DEGRADED_SET = [
      { resourceId: 'res-1', qurlId: '', qurlLink: 'https://q.test/1', recipientId: 'r1' },
    ];
    const monitor = monitorLinkStatus(
      'send-1', interaction,
      DEGRADED_SET,
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent', { components: [] }, 1,
    );
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    expect(mockDb.getQurlViews).not.toHaveBeenCalled();
    expect(mockDb.tryAdvanceRenderedCount).not.toHaveBeenCalled();
    monitor.stop();
  });
});

describe('monitorLinkStatus — early-poll latency (long-expiry sends tick within seconds)', () => {
  beforeEach(() => { jest.useFakeTimers(); });
  afterEach(() => { jest.useRealTimers(); });

  it('a view recorded seconds after a 30m-expiry send is reflected within ~5s, not ~60s', async () => {
    const interaction = makeInteraction();
    const monitor = monitorLinkStatus(
      'send-latency', interaction,
      ONE_LINK_SET,
      [{ id: 'r1', username: 'Alice' }],
      '30m', 'Sent to 1 user', { components: [] }, 1,
    );
    expect(monitor.getFullMsg()).toBe('Sent to 1 user\n👀 0 viewed / 1 pending');

    await jest.advanceTimersByTimeAsync(3000);
    expect(monitor.getFullMsg()).toBe('Sent to 1 user\n👀 0 viewed / 1 pending');

    mockDb.getQurlViews.mockResolvedValue(new Map([
      ['q_aaaaaaaaaa1', { accessCount: 1, consumed: true }],
    ]));

    await jest.advanceTimersByTimeAsync(6000);

    expect(monitor.getFullMsg()).toBe('Sent to 1 user\n👀 1 viewed / 0 pending');
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('👀 1 viewed / 0 pending'),
    }));
    monitor.stop();
  });
});

describe('monitorLinkStatus — empty-qurl_id boundary guard', () => {
  beforeEach(() => { jest.useFakeTimers(); });
  afterEach(() => { jest.useRealTimers(); });

  it('any missing qurlId degrades the whole monitor to bare base-msg', async () => {
    const mixed = [
      { resourceId: 'res-1', qurlId: 'q_aaaaaaaaaa1', qurlLink: 'https://q.test/1', recipientId: 'r1' },
      { resourceId: 'res-1', qurlId: '', qurlLink: 'https://q.test/2', recipientId: 'r2' },
    ];
    const monitor = monitorLinkStatus(
      'send-1', makeInteraction(),
      mixed,
      [{ id: 'r1', username: 'Alice' }, { id: 'r2', username: 'Bob' }],
      '1m', 'Sent to 2 users', { components: [] }, 2,
    );
    expect(monitor.getFullMsg()).toBe('Sent to 2 users');
    expect(monitor.getFullMsg()).not.toMatch(/viewed|pending|👀/);
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('Monitor view counter degraded'),
      expect.objectContaining({ sendId: 'send-1', missing: 1, total: 2 }),
    );

    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);
    expect(mockDb.getQurlViews).not.toHaveBeenCalled();
    monitor.stop();
  });
});

describe('monitorLinkStatus — first-poll cadence (BatchGet replaces upstream fanout)', () => {
  beforeEach(() => { jest.useFakeTimers(); });
  afterEach(() => { jest.useRealTimers(); });

  it('first tick fires at ~3s, not at the 15s pollInterval', async () => {
    const monitor = monitorLinkStatus(
      'send-1', makeInteraction(),
      ONE_LINK_SET,
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent', { components: [] }, 1,
    );
    await jest.advanceTimersByTimeAsync(2500);
    expect(mockDb.getQurlViews).not.toHaveBeenCalled();
    await jest.advanceTimersByTimeAsync(1000);
    expect(mockDb.getQurlViews).toHaveBeenCalled();
    monitor.stop();
  });

  it('keeps the 5s early backstop during the early window, then decays to the steady interval', async () => {
    const monitor = monitorLinkStatus(
      'send-1', makeInteraction(),
      ONE_LINK_SET,
      [{ id: 'r1', username: 'Alice' }],
      '30m', 'Sent', { components: [] }, 1,
    );

    await jest.advanceTimersByTimeAsync(3000);
    expect(mockDb.getQurlViews).toHaveBeenCalledTimes(1);

    await jest.advanceTimersByTimeAsync(20000);
    const callsAfterEarly = mockDb.getQurlViews.mock.calls.length;
    expect(callsAfterEarly).toBeGreaterThanOrEqual(4);

    await jest.advanceTimersByTimeAsync(90000); // past earlyPhaseUntil; drains remaining early ticks
    const callsAtWindowEnd = mockDb.getQurlViews.mock.calls.length;
    await jest.advanceTimersByTimeAsync(10000); // 10s ≪ 60s steady interval
    expect(mockDb.getQurlViews.mock.calls.length).toBe(callsAtWindowEnd);
    monitor.stop();
  });
});

describe('monitorLinkStatus — addRecipients() + stop() races', () => {
  beforeEach(() => { jest.useFakeTimers(); });
  afterEach(() => { jest.useRealTimers(); });

  it('addRecipients() extends trackedQurlIds and the next tick BatchGets the new IDs', async () => {
    const monitor = monitorLinkStatus(
      'send-1', makeInteraction(),
      ONE_LINK_SET,
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent', { components: [] }, 1,
    );

    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);
    mockDb.getQurlViews.mockClear();

    monitor.addRecipients(1, [{ qurlId: 'q_aaaaaaaaaa3', username: 'Charlie' }]);
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    expect(mockDb.getQurlViews).toHaveBeenCalled();
    const lastCallKeys = mockDb.getQurlViews.mock.calls.at(-1)[0];
    expect(lastCallKeys).toContain('q_aaaaaaaaaa3');
    monitor.stop();
  });

  it('addRecipients() re-arms the monitor after allDone so post-resolve adds still see views', async () => {
    const interaction = makeInteraction();
    const monitor = monitorLinkStatus(
      'send-resolve-add', interaction,
      ONE_LINK_SET,
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent to 1 user', { components: [] }, 1,
    );

    mockDb.getQurlViews.mockResolvedValueOnce(new Map([
      ['q_aaaaaaaaaa1', { accessCount: 1, consumed: false }],
    ]));
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);
    expect(monitor.getFullMsg()).toBe('Sent to 1 user\n👀 1 viewed / 0 pending');

    monitor.updateBaseMsg('Sent to 2 users');
    const callsBeforeAdd = mockDb.getQurlViews.mock.calls.length;
    monitor.addRecipients(1, [{ qurlId: 'q_aaaaaaaaaa9', username: 'Eve' }]);

    mockDb.getQurlViews.mockResolvedValueOnce(new Map([
      ['q_aaaaaaaaaa1', { accessCount: 1, consumed: false }],
      ['q_aaaaaaaaaa9', { accessCount: 1, consumed: false }],
    ]));
    await jest.advanceTimersByTimeAsync(3500);

    expect(mockDb.getQurlViews.mock.calls.length).toBeGreaterThan(callsBeforeAdd);
    expect(monitor.getFullMsg()).toBe('Sent to 2 users\n👀 2 viewed / 0 pending');
    monitor.stop();
  });

  it('addRecipients() seeds linkStatus so views on newly-added recipients flip pending → viewed', async () => {
    const interaction = makeInteraction();
    const monitor = monitorLinkStatus(
      'send-add-bug', interaction,
      ONE_LINK_SET,
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent to 1 user', { components: [] }, 1,
    );
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    monitor.addRecipients(1, [{ qurlId: 'q_aaaaaaaaaa9', username: 'Eve' }]);

    mockDb.getQurlViews.mockResolvedValueOnce(new Map([
      ['q_aaaaaaaaaa9', { accessCount: 1, consumed: false }],
    ]));
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    expect(monitor.getFullMsg()).toBe('Sent to 1 user\n👀 1 viewed / 1 pending');
    monitor.stop();
  });

  it('addRecipients() with a missing qurl_id flips viewCounterDegraded AND warns once', async () => {
    const monitor = monitorLinkStatus(
      'send-degrade-add', makeInteraction(),
      ONE_LINK_SET,
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent to 1 user', { components: [] }, 1,
    );
    expect(monitor.getFullMsg()).toBe('Sent to 1 user\n👀 0 viewed / 1 pending');

    monitor.addRecipients(1, [{ qurlId: '', username: 'Eve' }]);
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('degraded mid-life'),
      expect.objectContaining({ sendId: 'send-degrade-add' }),
    );
    expect(monitor.getFullMsg()).toBe('Sent to 1 user');
    monitor.stop();
  });

  it('addRecipients() de-dupes a qurl_id already in the tracked set', async () => {
    const monitor = monitorLinkStatus(
      'send-1', makeInteraction(),
      ONE_LINK_SET,
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent', { components: [] }, 1,
    );
    monitor.addRecipients(1, [{ qurlId: 'q_aaaaaaaaaa1', username: 'Alice' }]);
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);
    const lastCallKeys = mockDb.getQurlViews.mock.calls.at(-1)[0];
    expect(lastCallKeys.filter(k => k === 'q_aaaaaaaaaa1')).toHaveLength(1);
    monitor.stop();
  });

  it('addRecipients() called AFTER stop() is a strict no-op', async () => {
    const monitor = monitorLinkStatus(
      'send-after-stop', makeInteraction(),
      ONE_LINK_SET,
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent to 1 user', { components: [] }, 1,
    );

    const preStopMsg = monitor.getFullMsg();
    monitor.stop();

    monitor.addRecipients(1, [{ qurlId: 'q_aaaaaaaaaa9', username: 'Eve' }]);

    expect(monitor.getFullMsg()).toBe(preStopMsg);
  });

  it('stop() called concurrently with a running tick — no unhandled rejection', async () => {
    const interaction = makeInteraction();
    mockDb.getQurlViews.mockImplementation(() => new Promise(resolve =>
      setTimeout(() => resolve(new Map([['q_aaaaaaaaaa1', { accessCount: 0, consumed: false }]])), 5000),
    ));

    const monitor = monitorLinkStatus(
      'send-1', interaction,
      ONE_LINK_SET,
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent', { components: [] }, 1,
    );
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);
    monitor.stop();
    await jest.advanceTimersByTimeAsync(10000);
    expect(logger.error).not.toHaveBeenCalledWith(
      'Link monitor poll failed',
      expect.any(Object),
    );
  });

});

describe('monitorLinkStatus — edits always go through interaction.editReply (ephemeral-safe)', () => {
  beforeEach(() => { jest.useFakeTimers(); });
  afterEach(() => { jest.useRealTimers(); });

  it('never falls back to editDM — the confirm message is ephemeral and ephemeral edits are interaction-token-only', async () => {
    const interaction = makeInteraction();
    const monitor = monitorLinkStatus(
      'send-1', interaction,
      ONE_LINK_SET,
      [{ id: 'r1', username: 'Alice' }],
      '1h', 'Sent', { components: [] }, 1,
    );

    mockDb.getQurlViews.mockResolvedValueOnce(new Map([
      ['q_aaaaaaaaaa1', { accessCount: 1, consumed: false }],
    ]));
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);
    expect(interaction.editReply).toHaveBeenCalled();
    expect(mockEditDM).not.toHaveBeenCalled();
    monitor.stop();
  });
});

describe('monitorLinkStatus — duration cap + activeMonitors LRU', () => {
  beforeEach(() => { jest.useFakeTimers(); });
  afterEach(() => { jest.useRealTimers(); });

  it('stops + posts final after MAX_MONITOR_DURATION_MS (14min cap matches interaction-token TTL)', async () => {
    const monitor = monitorLinkStatus(
      'send-1', makeInteraction(),
      ONE_LINK_SET,
      [{ id: 'r1', username: 'Alice' }],
      '7d', 'Sent', { components: [] }, 1,
    );
    await jest.advanceTimersByTimeAsync(14 * 60 * 1000 + 60 * 1000);
    monitor.stop();
  });

  it('LRU bookkeeping: activeMonitors grows by N when N monitors start under the cap', () => {
    const before = activeMonitors.size;
    const monitors = [];
    for (let i = 0; i < 5; i++) {
      monitors.push(monitorLinkStatus(
        `send-${i}`, makeInteraction(),
        [{ resourceId: `res-${i}`, qurlId: `q_aaaaaaaaaa${i}`, qurlLink: `https://q.test/${i}`, recipientId: `r${i}` }],
        [{ id: `r${i}`, username: `User${i}` }],
        '1m', 'Sent', { components: [] }, 1,
      ));
    }
    expect(activeMonitors.size).toBe(before + 5);
    for (const m of monitors) m.stop();
  });

  it('exposes control surface: addRecipients, stop, updateBaseMsg, getFullMsg', () => {
    const monitor = monitorLinkStatus(
      'send-1', makeInteraction(),
      ONE_LINK_SET,
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent', { components: [] }, 1,
    );
    expect(typeof monitor.addRecipients).toBe('function');
    expect(typeof monitor.stop).toBe('function');
    expect(typeof monitor.updateBaseMsg).toBe('function');
    expect(typeof monitor.getFullMsg).toBe('function');

    monitor.updateBaseMsg('New base');
    expect(monitor.getFullMsg()).toContain('New base');
    monitor.stop();
  });
});

describe('revokeAllLinks', () => {
  const makeItems = (n) => Array.from({ length: n }, (_, i) => ({
    resource_id: `res-${i + 1}`,
    recipient_discord_id: `user-${i + 1}`,
  }));

  it('records revocation intent before DELETEs and marks the send revoked only after every DELETE succeeds', async () => {
    mockDb.getSendItems.mockResolvedValueOnce(makeItems(3));
    mockDeleteLink.mockResolvedValue(undefined);

    const result = await revokeAllLinks('send-1', 'sender-1', 'apikey');

    expect(mockDeleteLink).toHaveBeenCalledTimes(3);
    expect(mockDb.markSendRevoking).toHaveBeenCalledWith('send-1', 'sender-1');
    expect(mockDb.markSendRevoked).toHaveBeenCalledWith('send-1', 'sender-1');
    expect(mockDb.markSendRevoking.mock.invocationCallOrder[0])
      .toBeLessThan(mockDb.getSendItems.mock.invocationCallOrder[0]);
    expect(mockDb.getSendItems.mock.invocationCallOrder[0])
      .toBeLessThan(mockDeleteLink.mock.invocationCallOrder[0]);
    expect(mockDeleteLink.mock.invocationCallOrder.at(-1))
      .toBeLessThan(mockDb.markSendRevoked.mock.invocationCallOrder[0]);
    expect(result).toEqual({
      barrierEstablished: true,
      finalizationFailed: false,
      success: 3,
      total: 3,
      successUserIds: ['user-1', 'user-2', 'user-3'],
      failureUserIds: [],
    });
  });

  it.each([
    ['SDK validation', new Error('delete: only public resource IDs are accepted')],
    ['network', new Error('Network request failed')],
    ['qURL 5xx', new Error('qURL API DELETE /qurls/id failed (503)')],
  ])('keeps a partial %s failure fail-closed and available for repair/retry', async (_failureKind, failure) => {
    const sensitiveResourceId = 'at_sensitive-revoke-token';
    mockDb.getSendItems.mockResolvedValueOnce([
      makeItems(2)[0],
      { resource_id: sensitiveResourceId, recipient_discord_id: 'user-2' },
    ]);
    mockDeleteLink
      .mockResolvedValueOnce(undefined)
      .mockRejectedValueOnce(failure);

    const result = await revokeAllLinks('send-1', 'sender-1', 'apikey');

    expect(result.success).toBe(1);
    expect(result.total).toBe(2);
    expect(result.successUserIds).toEqual(['user-1']);
    expect(result.failureUserIds).toEqual(['user-2']);
    expect(logger.error).toHaveBeenCalledWith('Failed to revoke QURL', {
      resource_ref: resourceIdLogRef(sensitiveResourceId),
      error: failure.message,
    });
    expect(JSON.stringify(logger.error.mock.calls)).not.toContain(sensitiveResourceId);
    expect(logger.audit).toHaveBeenCalledWith('revoke_failed', {
      send_id: 'send-1', success: 1, total: 2, unresolvable_recipients: 0,
    });
    expect(mockDb.markSendRevoking).toHaveBeenCalledWith('send-1', 'sender-1');
    expect(mockDb.markSendRevoked).not.toHaveBeenCalled();
  });

  it('re-drives the full resource set after a partial failure and finalizes on a successful retry', async () => {
    mockDb.getSendItems.mockResolvedValue([
      {
        resource_id: 'res-1', recipient_discord_id: 'user-1', dm_status: 'sent',
        dm_channel_id: 'channel-1', dm_message_id: 'message-1',
      },
      {
        resource_id: 'res-2', recipient_discord_id: 'user-2', dm_status: 'sent',
        dm_channel_id: 'channel-2', dm_message_id: 'message-2',
      },
    ]);
    mockDeleteLink
      .mockResolvedValueOnce(undefined)
      .mockRejectedValueOnce(new Error('Network request failed'))
      .mockResolvedValueOnce(undefined)
      .mockResolvedValueOnce(undefined);

    const first = await revokeAllLinks('send-retry', 'sender-1', 'apikey');
    expect(first).toMatchObject({ success: 1, total: 2 });
    expect(mockDb.markSendRevoked).not.toHaveBeenCalled();

    const second = await revokeAllLinks('send-retry', 'sender-1', 'apikey');
    expect(second).toMatchObject({ success: 2, total: 2 });
    expect(mockDeleteLink).toHaveBeenCalledTimes(4);
    expect(mockDb.markSendRevoking).toHaveBeenCalledTimes(2);
    expect(mockDb.markSendRevoked).toHaveBeenCalledTimes(1);
    expect(mockEditDM.mock.calls.map(call => call.slice(0, 2))).toEqual([
      ['channel-1', 'message-1'],
      ['channel-1', 'message-1'],
      ['channel-2', 'message-2'],
    ]);
  });

  it('returns 0/0 when send has no items (already-revoked or unknown sendId)', async () => {
    mockDb.getSendItems.mockResolvedValueOnce([]);

    const result = await revokeAllLinks('send-1', 'sender-1', 'apikey');

    expect(result).toEqual({
      barrierEstablished: true,
      finalizationFailed: false,
      success: 0,
      total: 0,
      successUserIds: [],
      failureUserIds: [],
    });
    expect(mockDeleteLink).not.toHaveBeenCalled();
    expect(mockDb.markSendRevoking).toHaveBeenCalled();
    expect(mockDb.markSendRevoked).toHaveBeenCalled();
  });

  it('does not read or delete resources when the durable revoke barrier rejects an unknown or foreign send', async () => {
    mockDb.markSendRevoking.mockResolvedValueOnce(false);

    const result = await revokeAllLinks('foreign-send', 'sender-1', 'apikey');

    expect(result).toEqual({
      barrierEstablished: false,
      finalizationFailed: false,
      success: 0,
      total: 0,
      successUserIds: [],
      failureUserIds: [],
    });
    expect(mockDb.getSendItems).not.toHaveBeenCalled();
    expect(mockDeleteLink).not.toHaveBeenCalled();
    expect(mockDb.markSendRevoked).not.toHaveBeenCalled();
  });

  it('emits revoke_success audit event only when every link is confirmed revoked', async () => {
    mockDb.getSendItems.mockResolvedValueOnce(makeItems(2));
    mockDeleteLink.mockResolvedValue(undefined);

    await revokeAllLinks('send-42', 'sender-1', 'apikey');

    expect(logger.audit).toHaveBeenCalledWith('revoke_success', {
      send_id: 'send-42', success: 2, total: 2, unresolvable_recipients: 0,
    });
  });

  it.each([
    ['SDK validation', new Error('delete: only public resource IDs are accepted')],
    ['network', new Error('Network request failed')],
    ['qURL 5xx', new Error('qURL API DELETE /qurls/id failed (503)')],
  ])('keeps a full %s failure fail-closed and emits revoke_failed', async (_failureKind, failure) => {
    mockDb.getSendItems.mockResolvedValueOnce(makeItems(2));
    mockDeleteLink.mockRejectedValueOnce(failure).mockRejectedValueOnce(failure);

    await revokeAllLinks('send-43', 'sender-1', 'apikey');

    const events = logger.audit.mock.calls.map(c => c[0]);
    expect(events).toContain('revoke_failed');
    expect(events).not.toContain('revoke_success');
    expect(logger.audit).toHaveBeenCalledWith('revoke_failed', {
      send_id: 'send-43', success: 0, total: 2, unresolvable_recipients: 0,
    });
    expect(mockDb.markSendRevoking).toHaveBeenCalledWith('send-43', 'sender-1');
    expect(mockDb.markSendRevoked).not.toHaveBeenCalled();
  });

  it('emits no audit event when there are no resources to revoke (avoids 0/0 noise)', async () => {
    mockDb.getSendItems.mockResolvedValueOnce([]);

    await revokeAllLinks('send-44', 'sender-1', 'apikey');

    const events = logger.audit.mock.calls.map(c => c[0]);
    expect(events).not.toContain('revoke_success');
    expect(events).not.toContain('revoke_failed');
  });

  it('propagates a getSendItems failure to the caller (no DELETE attempted, no audit emitted)', async () => {
    mockDb.getSendItems.mockRejectedValueOnce(new Error('DDB throttled'));

    await expect(
      revokeAllLinks('send-throw', 'sender-1', 'apikey'),
    ).rejects.toThrow('DDB throttled');

    expect(mockDeleteLink).not.toHaveBeenCalled();
    expect(mockDb.markSendRevoked).not.toHaveBeenCalled();
    const events = logger.audit.mock.calls.map(c => c[0]);
    expect(events).not.toContain('revoke_success');
    expect(events).not.toContain('revoke_failed');
  });

  it('does not delete links when recording the revoked state fails', async () => {
    mockDb.markSendRevoking.mockRejectedValueOnce(new Error('DDB write failed'));
    mockDeleteLink.mockResolvedValue(undefined);

    await expect(
      revokeAllLinks('send-mark-fail', 'sender-1', 'apikey'),
    ).rejects.toThrow('DDB write failed');

    expect(mockDb.getSendItems).not.toHaveBeenCalled();
    expect(mockDeleteLink).not.toHaveBeenCalled();
    const events = logger.audit.mock.calls.map(c => c[0]);
    expect(events).not.toContain('revoke_success');
    expect(events).not.toContain('revoke_failed');
  });

  it('leaves revocation retryable when the final revoked_at write fails after successful DELETEs', async () => {
    mockDb.getSendItems.mockResolvedValueOnce([{
      resource_id: 'res-1',
      recipient_discord_id: 'user-1',
      dm_status: 'sent',
      dm_channel_id: 'channel-1',
      dm_message_id: 'message-1',
    }]);
    mockDb.markSendRevoking.mockResolvedValueOnce(true);
    mockDb.markSendRevoked.mockRejectedValueOnce(new Error('DDB finalize failed'));
    mockDeleteLink.mockResolvedValue(undefined);

    const result = await revokeAllLinks('send-finalize-fail', 'sender-1', 'apikey');

    expect(result).toEqual({
      barrierEstablished: true,
      finalizationFailed: true,
      success: 1,
      total: 1,
      successUserIds: ['user-1'],
      failureUserIds: [],
    });

    expect(mockDeleteLink).toHaveBeenCalledTimes(1);
    expect(mockDb.markSendRevoking).toHaveBeenCalledWith('send-finalize-fail', 'sender-1');
    expect(mockDb.markSendRevoked).toHaveBeenCalledWith('send-finalize-fail', 'sender-1');
    expect(logger.audit).toHaveBeenCalledWith('revoke_success', {
      send_id: 'send-finalize-fail', success: 1, total: 1, unresolvable_recipients: 0,
    });
    expect(logger.error).toHaveBeenCalledWith(
      'Failed to finalize revoked send state',
      { sendId: 'send-finalize-fail', error: 'DDB finalize failed' },
    );
    expect(mockEditDM).toHaveBeenCalledWith(
      'channel-1', 'message-1', expect.any(Object),
    );
  });

  it('leaves revocation retryable when the final revoked_at write is rejected without throwing', async () => {
    mockDb.getSendItems.mockResolvedValueOnce([{
      resource_id: 'res-1',
      recipient_discord_id: 'user-1',
    }]);
    mockDb.markSendRevoked.mockResolvedValueOnce(false);
    mockDeleteLink.mockResolvedValue(undefined);

    const result = await revokeAllLinks('send-finalize-rejected', 'sender-1', 'apikey');

    expect(result).toMatchObject({
      finalizationFailed: true,
      success: 1,
      total: 1,
    });
    expect(logger.error).toHaveBeenCalledWith(
      'Failed to finalize revoked send state',
      { sendId: 'send-finalize-rejected', error: 'finalization was not confirmed' },
    );
  });

  it('groups items by resource_id — shared-resource recipients all land in successUserIds on a single DELETE', async () => {
    mockDb.getSendItems.mockResolvedValueOnce([
      { resource_id: 'res-shared', recipient_discord_id: 'u-1' },
      { resource_id: 'res-shared', recipient_discord_id: 'u-2' },
      { resource_id: 'res-shared', recipient_discord_id: 'u-3' },
      { resource_id: 'res-solo',   recipient_discord_id: 'u-4' },
    ]);
    mockDeleteLink.mockResolvedValue(undefined);

    const result = await revokeAllLinks('send-shared', 'sender-1', 'apikey');

    expect(mockDeleteLink).toHaveBeenCalledTimes(2);
    expect(result.success).toBe(4);
    expect(result.total).toBe(4);
    expect(result.successUserIds.sort()).toEqual(['u-1', 'u-2', 'u-3', 'u-4']);
    expect(result.failureUserIds).toEqual([]);
  });

  it('counts one recipient once when all of their links across resources are revoked', async () => {
    mockDb.getSendItems.mockResolvedValueOnce([
      { resource_id: 'res-a', recipient_discord_id: 'u-1' },
      { resource_id: 'res-b', recipient_discord_id: 'u-1' },
    ]);
    mockDeleteLink.mockResolvedValue(undefined);

    const result = await revokeAllLinks('send-duplicate-recipient', 'sender-1', 'apikey');

    expect(mockDeleteLink).toHaveBeenCalledTimes(2);
    expect(result).toMatchObject({
      success: 1,
      total: 1,
      successUserIds: ['u-1'],
      failureUserIds: [],
    });
    expect(mockDb.markSendRevoked).toHaveBeenCalledWith(
      'send-duplicate-recipient', 'sender-1',
    );
  });

  it('groups items by resource_id — shared-resource failure fans out to all sharing recipients', async () => {
    mockDb.getSendItems.mockResolvedValueOnce([
      { resource_id: 'res-shared', recipient_discord_id: 'u-1' },
      { resource_id: 'res-shared', recipient_discord_id: 'u-2' },
      { resource_id: 'res-solo',   recipient_discord_id: 'u-3' },
    ]);
    mockDeleteLink
      .mockRejectedValueOnce(new Error('already opened'))
      .mockResolvedValueOnce(undefined);

    const result = await revokeAllLinks('send-shared-fail', 'sender-1', 'apikey');

    expect(mockDeleteLink).toHaveBeenCalledTimes(2);
    expect(result.success).toBe(1);
    expect(result.total).toBe(3);
    expect(result.successUserIds).toEqual(['u-3']);
    expect(result.failureUserIds.sort()).toEqual(['u-1', 'u-2']);
  });

  it('keeps malformed rows without resource_id retryable without calling deleteLink', async () => {
    mockDb.getSendItems.mockResolvedValueOnce([
      { resource_id: 'res-ok', recipient_discord_id: 'u-ok' },
      { resource_id: '   ', recipient_discord_id: 'u-missing' },
    ]);
    mockDeleteLink.mockResolvedValue(undefined);

    const result = await revokeAllLinks('send-malformed', 'sender-1', 'apikey');

    expect(mockDeleteLink).toHaveBeenCalledTimes(1);
    expect(mockDeleteLink).toHaveBeenCalledWith('res-ok', 'apikey');
    expect(result).toMatchObject({
      success: 1,
      total: 2,
      successUserIds: ['u-ok'],
      failureUserIds: ['u-missing'],
    });
    expect(mockDb.markSendRevoked).not.toHaveBeenCalled();
    expect(logger.audit).toHaveBeenCalledWith('revoke_failed', {
      send_id: 'send-malformed', success: 1, total: 1, unresolvable_recipients: 1,
    });
    expect(logger.error).toHaveBeenCalledWith(
      'Cannot revoke send row with missing resource identity',
      { sendId: 'send-malformed', affectedRecipients: 1 },
    );
  });

  it('audits an all-missing-resource send as a failed retryable revoke', async () => {
    mockDb.getSendItems.mockResolvedValueOnce([
      { recipient_discord_id: 'u-missing' },
      { resource_id: '   ', recipient_discord_id: 'u-missing-2' },
    ]);

    const result = await revokeAllLinks('send-all-malformed', 'sender-1', 'apikey');

    expect(mockDeleteLink).not.toHaveBeenCalled();
    expect(mockDb.markSendRevoked).not.toHaveBeenCalled();
    expect(result).toMatchObject({
      success: 0,
      total: 2,
      successUserIds: [],
      failureUserIds: ['u-missing', 'u-missing-2'],
    });
    expect(logger.audit).toHaveBeenCalledWith('revoke_failed', {
      send_id: 'send-all-malformed', success: 0, total: 0, unresolvable_recipients: 2,
    });
  });

  it('failure-wins: mixed-outcome recipient (one resource ok, another failed) → failure only', async () => {
    mockDb.getSendItems.mockResolvedValueOnce([
      { resource_id: 'res-a', recipient_discord_id: 'alice' },  // succeeds
      { resource_id: 'res-b', recipient_discord_id: 'alice' },  // fails
      { resource_id: 'res-a', recipient_discord_id: 'bob' },    // succeeds (bob clean)
    ]);
    mockDeleteLink
      .mockResolvedValueOnce(undefined)            // res-a
      .mockRejectedValueOnce(new Error('opened')); // res-b

    const result = await revokeAllLinks('send-mixed', 'sender-1', 'apikey');

    expect(result.total).toBe(2);  // 2 unique recipients
    expect(result.success).toBe(1); // only bob (alice has a failure)
    expect(result.successUserIds).toEqual(['bob']);
    expect(result.failureUserIds).toEqual(['alice']);
    expect(mockDb.markSendRevoked).not.toHaveBeenCalled();
  });

  describe('post-revoke DM edit', () => {
    beforeEach(() => {
      mockEditDM.mockClear();
      mockEditDM.mockResolvedValue({ ok: true });
    });

    it('edits the DM of every strict-success recipient with stored channel + message ids', async () => {
      mockDb.getSendItems.mockResolvedValueOnce([
        { resource_id: 'res-1', recipient_discord_id: 'u-1', dm_status: 'sent', dm_channel_id: 'c-1', dm_message_id: 'm-1' },
        { resource_id: 'res-2', recipient_discord_id: 'u-2', dm_status: 'sent', dm_channel_id: 'c-2', dm_message_id: 'm-2' },
      ]);
      mockDeleteLink.mockResolvedValue(undefined);

      await revokeAllLinks('send-edit', 'sender-1', 'apikey', 'Alice');

      expect(mockEditDM).toHaveBeenCalledTimes(2);
      const calls = mockEditDM.mock.calls.map(c => [c[0], c[1]]).sort();
      expect(calls).toEqual([['c-1', 'm-1'], ['c-2', 'm-2']]);
      const payload = mockEditDM.mock.calls[0][2];
      expect(payload.components).toEqual([]);
      expect(payload.embeds).toHaveLength(1);
    });

    it('skips recipients whose revoke failed (link was already opened)', async () => {
      mockDb.getSendItems.mockResolvedValueOnce([
        { resource_id: 'res-ok',   recipient_discord_id: 'u-ok',   dm_status: 'sent', dm_channel_id: 'c-ok',   dm_message_id: 'm-ok' },
        { resource_id: 'res-fail', recipient_discord_id: 'u-fail', dm_status: 'sent', dm_channel_id: 'c-fail', dm_message_id: 'm-fail' },
      ]);
      mockDeleteLink
        .mockResolvedValueOnce(undefined)
        .mockRejectedValueOnce(new Error('already opened'));

      await revokeAllLinks('send-partial', 'sender-1', 'apikey', 'Alice');

      expect(mockEditDM).toHaveBeenCalledTimes(1);
      expect(mockEditDM.mock.calls[0].slice(0, 2)).toEqual(['c-ok', 'm-ok']);
    });

    it('does NOT edit the DM of a mixed-outcome recipient (one of their resources failed to revoke)', async () => {
      mockDb.getSendItems.mockResolvedValueOnce([
        { resource_id: 'res-a', recipient_discord_id: 'mixed', dm_status: 'sent', dm_channel_id: 'c-mixed', dm_message_id: 'm-mixed' },
        { resource_id: 'res-b', recipient_discord_id: 'mixed', dm_status: 'sent', dm_channel_id: 'c-mixed', dm_message_id: 'm-mixed' },
        { resource_id: 'res-c', recipient_discord_id: 'clean', dm_status: 'sent', dm_channel_id: 'c-clean', dm_message_id: 'm-clean' },
      ]);
      mockDeleteLink
        .mockResolvedValueOnce(undefined)
        .mockRejectedValueOnce(new Error('already opened'))
        .mockResolvedValueOnce(undefined);

      const result = await revokeAllLinks('send-mixed-within', 'sender-1', 'apikey', 'Alice');

      expect(result.successUserIds).toEqual(['clean']);
      expect(result.failureUserIds).toEqual(['mixed']);
      expect(mockEditDM).toHaveBeenCalledTimes(1);
      expect(mockEditDM.mock.calls[0].slice(0, 2)).toEqual(['c-clean', 'm-clean']);
    });

    it('does NOT call editDM when every DELETE threw (success === 0)', async () => {
      mockDb.getSendItems.mockResolvedValueOnce([
        { resource_id: 'res-a', recipient_discord_id: 'u-a', dm_status: 'sent', dm_channel_id: 'c-a', dm_message_id: 'm-a' },
        { resource_id: 'res-b', recipient_discord_id: 'u-b', dm_status: 'sent', dm_channel_id: 'c-b', dm_message_id: 'm-b' },
      ]);
      mockDeleteLink.mockRejectedValue(new Error('qURL service down'));

      const result = await revokeAllLinks('send-all-fail', 'sender-1', 'apikey', 'Alice');

      expect(result.success).toBe(0);
      expect(mockEditDM).not.toHaveBeenCalled();
      const skipLog = logger.debug.mock.calls.find(c => c[0] === 'Revoke succeeded but no editable DM targets');
      expect(skipLog).toBeUndefined();
    });

    it('emits debug silent-skip log + no info edit log when every strict-success row is legacy', async () => {
      mockDb.getSendItems.mockResolvedValueOnce([
        { resource_id: 'res-a', recipient_discord_id: 'u-a', dm_status: 'sent' }, // legacy, no refs
        { resource_id: 'res-b', recipient_discord_id: 'u-b', dm_status: 'sent' }, // legacy, no refs
      ]);
      mockDeleteLink.mockResolvedValue(undefined);

      await revokeAllLinks('send-all-legacy', 'sender-1', 'apikey', 'Alice');

      expect(mockEditDM).not.toHaveBeenCalled();
      const editedLog = logger.info.mock.calls.find(c => c[0] === 'Edited DMs after revoke');
      expect(editedLog).toBeUndefined();
      const skipLog = logger.debug.mock.calls.find(c => c[0] === 'Revoke succeeded but no editable DM targets');
      expect(skipLog).toBeTruthy();
      expect(skipLog[1]).toMatchObject({ sendId: 'send-all-legacy', revoke_success: 2 });
    });

    it('skips rows with no stored DM refs (legacy sends predating the wire-up)', async () => {
      mockDb.getSendItems.mockResolvedValueOnce([
        { resource_id: 'res-new',    recipient_discord_id: 'u-new',    dm_status: 'sent', dm_channel_id: 'c-new', dm_message_id: 'm-new' },
        { resource_id: 'res-legacy', recipient_discord_id: 'u-legacy', dm_status: 'sent' }, // no channel / message id
      ]);
      mockDeleteLink.mockResolvedValue(undefined);

      await revokeAllLinks('send-legacy', 'sender-1', 'apikey', 'Alice');

      expect(mockEditDM).toHaveBeenCalledTimes(1);
      expect(mockEditDM.mock.calls[0].slice(0, 2)).toEqual(['c-new', 'm-new']);
    });

    it('skips rows where the DM never delivered (dm_status !== sent)', async () => {
      mockDb.getSendItems.mockResolvedValueOnce([
        { resource_id: 'res-failed', recipient_discord_id: 'u-failed', dm_status: 'failed', dm_channel_id: 'c-x', dm_message_id: 'm-x' },
      ]);
      mockDeleteLink.mockResolvedValue(undefined);

      await revokeAllLinks('send-nodm', 'sender-1', 'apikey', 'Alice');

      expect(mockEditDM).not.toHaveBeenCalled();
    });

    it.each([
      ['rejection',     () => mockEditDM.mockRejectedValueOnce(new Error('boom'))],
      ['ok:false',      () => mockEditDM.mockResolvedValueOnce({ ok: false, expected: false })],
      ['ok:false+exp',  () => mockEditDM.mockResolvedValueOnce({ ok: false, expected: true })],
    ])('does not affect revoke success/total when DM edit fails as %s', async (_shape, setupMock) => {
      mockDb.getSendItems.mockResolvedValueOnce([
        { resource_id: 'res-1', recipient_discord_id: 'u-1', dm_status: 'sent', dm_channel_id: 'c-1', dm_message_id: 'm-1' },
      ]);
      mockDeleteLink.mockResolvedValue(undefined);
      setupMock();

      const result = await revokeAllLinks('send-edit-fail', 'sender-1', 'apikey', 'Alice');

      expect(result.success).toBe(1);
      expect(result.total).toBe(1);
    });

    it('logs split attempted/edited/expectedFailures/failed counts', async () => {
      mockDb.getSendItems.mockResolvedValueOnce([
        { resource_id: 'res-1', recipient_discord_id: 'u-ok',  dm_status: 'sent', dm_channel_id: 'c-ok',  dm_message_id: 'm-ok' },
        { resource_id: 'res-2', recipient_discord_id: 'u-exp', dm_status: 'sent', dm_channel_id: 'c-exp', dm_message_id: 'm-exp' },
        { resource_id: 'res-3', recipient_discord_id: 'u-bad', dm_status: 'sent', dm_channel_id: 'c-bad', dm_message_id: 'm-bad' },
      ]);
      mockDeleteLink.mockResolvedValue(undefined);
      mockEditDM
        .mockResolvedValueOnce({ ok: true })
        .mockResolvedValueOnce({ ok: false, expected: true })
        .mockResolvedValueOnce({ ok: false, expected: false });

      await revokeAllLinks('send-split-log', 'sender-1', 'apikey', 'Alice');

      const logCall = logger.info.mock.calls.find(c => c[0] === 'Edited DMs after revoke');
      expect(logCall).toBeTruthy();
      expect(logCall[1]).toMatchObject({ attempted: 3, edited: 1, expectedFailures: 1, failed: 1 });
    });

    it('renders the fallback alias when senderAlias is omitted (forgotten-4th-arg defense)', async () => {
      mockDb.getSendItems.mockResolvedValueOnce([
        { resource_id: 'res-1', recipient_discord_id: 'u-1', dm_status: 'sent', dm_channel_id: 'c-1', dm_message_id: 'm-1' },
      ]);
      mockDeleteLink.mockResolvedValue(undefined);

      await revokeAllLinks('send-no-alias', 'sender-1', 'apikey'); // no senderAlias

      expect(mockEditDM).toHaveBeenCalledTimes(1);
      const payload = mockEditDM.mock.calls[0][2];
      expect(payload.embeds).toHaveLength(1);
      const embed = payload.embeds[0];
      const setDescCall = embed.setDescription.mock?.calls?.[0]?.[0];
      expect(setDescCall).toBeTruthy();
      expect(setDescCall).toMatch(/\*\*Someone\*\* closed the door/);
    });

    it('de-dupes per recipient when multiple rows share recipient_discord_id', async () => {
      mockDb.getSendItems.mockResolvedValueOnce([
        { resource_id: 'res-1', recipient_discord_id: 'u-1', dm_status: 'sent', dm_channel_id: 'c-1', dm_message_id: 'm-1' },
        { resource_id: 'res-2', recipient_discord_id: 'u-1', dm_status: 'sent', dm_channel_id: 'c-1', dm_message_id: 'm-1' },
      ]);
      mockDeleteLink.mockResolvedValue(undefined);

      await revokeAllLinks('send-dup', 'sender-1', 'apikey', 'Alice');

      expect(mockEditDM).toHaveBeenCalledTimes(1);
    });
  });
});

describe('persistDispatchResult — divergence guard', () => {
  beforeEach(() => {
    mockDb.markSendDMDelivered.mockClear();
    mockDb.markSendDMDelivered.mockResolvedValue(undefined);
    mockDb.updateSendDMStatus.mockClear();
    mockDb.updateSendDMStatus.mockResolvedValue(undefined);
    logger.warn.mockClear();
    logger.audit.mockClear();
    logger.error.mockClear();
  });

  it('happy path: writes markSendDMDelivered with both refs', async () => {
    await persistDispatchResult('s', 'r', { ok: true, channelId: 'c', messageId: 'm' });
    expect(mockDb.markSendDMDelivered).toHaveBeenCalledWith('s', 'r', 'c', 'm');
    expect(mockDb.updateSendDMStatus).not.toHaveBeenCalled();
    expect(logger.warn).not.toHaveBeenCalled();
    expect(logger.audit).not.toHaveBeenCalledWith('dispatch_sent_no_refs', expect.anything());
  });

  it('plain failure: writes FAILED without warning or divergence audit', async () => {
    await persistDispatchResult('s', 'r', { ok: false });
    expect(mockDb.markSendDMDelivered).not.toHaveBeenCalled();
    expect(mockDb.updateSendDMStatus).toHaveBeenCalledWith('s', 'r', 'failed');
    expect(logger.warn).not.toHaveBeenCalled();
    expect(logger.audit).not.toHaveBeenCalledWith('dispatch_sent_no_refs', expect.anything());
  });

  it.each([
    ['only messageId missing', { ok: true, channelId: 'c' },                 false, true ],
    ['only channelId missing', { ok: true, messageId: 'm' },                 true,  false],
    ['both missing',           { ok: true },                                  false, false],
  ])('records SENT + emits DISPATCH_SENT_NO_REFS on divergence (%s)', async (_name, result, hasMessageId, hasChannelId) => {
    await persistDispatchResult('s', 'r', result);
    expect(mockDb.markSendDMDelivered).not.toHaveBeenCalled();
    expect(mockDb.updateSendDMStatus).toHaveBeenCalledWith('s', 'r', 'sent');
    expect(logger.audit).toHaveBeenCalledWith('dispatch_sent_no_refs', { send_id: 's' });
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('missing channelId/messageId'),
      expect.objectContaining({ hasChannelId, hasMessageId }),
    );
  });

  it('does NOT throw when markSendDMDelivered fails — emits DISPATCH_PERSIST_FAILED + logs error', async () => {
    mockDb.markSendDMDelivered.mockRejectedValueOnce(new Error('throttled'));
    await expect(
      persistDispatchResult('s', 'r', { ok: true, channelId: 'c', messageId: 'm' }),
    ).resolves.toBeUndefined();
    expect(logger.audit).toHaveBeenCalledWith('dispatch_persist_failed', { send_id: 's' });
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('qurl_sends write failed'),
      expect.objectContaining({ sendId: 's', recipientDiscordId: 'r', delivered: true }),
    );
  });

  it('emits DISPATCH_PERSIST_FAILED when divergence-branch updateSendDMStatus fails (canary survives DDB outage)', async () => {
    mockDb.updateSendDMStatus.mockRejectedValueOnce(new Error('throttled'));
    await expect(
      persistDispatchResult('s', 'r', { ok: true }),
    ).resolves.toBeUndefined();
    expect(logger.audit).toHaveBeenCalledWith('dispatch_sent_no_refs', { send_id: 's' });
    expect(logger.audit).toHaveBeenCalledWith('dispatch_persist_failed', { send_id: 's' });
  });

  it('does NOT emit DISPATCH_PERSIST_FAILED when the FAILED-status write fails (no delivered DM)', async () => {
    mockDb.updateSendDMStatus.mockRejectedValueOnce(new Error('throttled'));
    await expect(
      persistDispatchResult('s', 'r', { ok: false }),
    ).resolves.toBeUndefined();
    expect(logger.audit).not.toHaveBeenCalledWith('dispatch_persist_failed', expect.anything());
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('qurl_sends write failed'),
      expect.objectContaining({ sendId: 's', recipientDiscordId: 'r', delivered: false }),
    );
  });
});

describe('renderViewCounter — pure "👀 N viewed / M pending" line', () => {
  it('degraded → baseMsg alone (no counter line)', () => {
    expect(renderViewCounter({
      baseMsg: 'Sent to 3 users', viewed: 1, expectedCount: 3, degraded: true,
    })).toBe('Sent to 3 users');
  });

  it('normal → "<base>\\n👀 X viewed / Y pending"', () => {
    expect(renderViewCounter({
      baseMsg: 'Sent to 3 users', viewed: 1, expectedCount: 3, degraded: false,
    })).toBe('Sent to 3 users\n👀 1 viewed / 2 pending');
  });

  it('all viewed → 0 pending', () => {
    expect(renderViewCounter({
      baseMsg: 'Sent to 2 users', viewed: 2, expectedCount: 2, degraded: false,
    })).toBe('Sent to 2 users\n👀 2 viewed / 0 pending');
  });

  it('pending floors at 0 when viewed > expectedCount (no negative pending)', () => {
    expect(renderViewCounter({
      baseMsg: 'Sent to 1 user', viewed: 3, expectedCount: 1, degraded: false,
    })).toBe('Sent to 1 user\n👀 3 viewed / 0 pending');
  });
});

describe('renderSendConfirm — post-send confirmation overflow', () => {
  const baseArgs = {
    delivered: 0, expiresIn: '1h',
    failedNamesPlain: [], successNames: [], showAll: false,
  };

  it('small list: full inline + Show Recipients toggle when >TRUNC_LIMIT', () => {
    const successNames = Array.from({ length: REVOKE_TRUNC_LIMIT + 2 }, (_, i) => `u${i}`);
    const r = renderSendConfirm({ ...baseArgs, delivered: successNames.length, successNames });
    expect(r.content).toMatch(/^Sent to \d+ users? \| /);
    expect(r.content).toContain('Recipients: u0, u1, u2, u3, u4 +2 more');
    expect(r.attachmentText).toBeNull();
    expect(r.needsExpand).toBe(true);
  });

  it('header includes "Self-destruct: <label>" when a timer is set', () => {
    const r = renderSendConfirm({
      ...baseArgs, delivered: 1, successNames: ['alice'], selfDestructSeconds: 300,
    });
    expect(r.content).toContain('| Self-destruct: 5 minutes');
    expect(r.content).not.toContain('One-time');
  });

  it('header renders "Self-destruct: off" when no timer is set', () => {
    const r = renderSendConfirm({
      ...baseArgs, delivered: 1, successNames: ['alice'],
    });
    expect(r.content).toContain('| Self-destruct: off');
  });

  it('small list, showAll=true: full names inline, no truncation marker', () => {
    const successNames = Array.from({ length: REVOKE_TRUNC_LIMIT + 2 }, (_, i) => `u${i}`);
    const r = renderSendConfirm({ ...baseArgs, delivered: successNames.length, successNames, showAll: true });
    expect(r.content).toContain('Recipients: u0, u1, u2, u3, u4, u5, u6');
    expect(r.content).not.toMatch(/\+\d+ more/);
    expect(r.attachmentText).toBeNull();
  });

  it('overflow: full list >2000 chars triggers attachment + suppresses Show Recipients', () => {
    const successNames = Array.from({ length: 200 }, (_, i) => `verylongusername${String(i).padStart(4, '0')}`);
    const r = renderSendConfirm({ ...baseArgs, delivered: successNames.length, successNames, showAll: true });
    expect(r.content.length).toBeLessThanOrEqual(2000);
    expect(r.content).toContain('(see attached)');
    expect(r.content).toContain('Recipients: verylongusername0000, verylongusername0001');
    expect(r.attachmentText).not.toBeNull();
    expect(r.attachmentText).toContain('DELIVERED (200):');
    expect(r.attachmentText.split('\n')).toContain('verylongusername0199');
    expect(r.needsExpand).toBe(false);
  });

  it('overflow: failed list also rolls into the same attachment', () => {
    const successNames = Array.from({ length: 100 }, (_, i) => `delivered_${String(i).padStart(3, '0')}_with_long_name`);
    const failedNamesPlain = Array.from({ length: 50 }, (_, i) => `failed_${String(i).padStart(3, '0')}_with_long_name`);
    const r = renderSendConfirm({
      ...baseArgs,
      delivered: successNames.length,
      successNames, failedNamesPlain, showAll: true,
    });
    expect(r.attachmentText).toContain('DELIVERED (100):');
    expect(r.attachmentText).toContain('NOT DELIVERED (50):');
    expect(r.attachmentText).toContain('failed_049_with_long_name');
    expect(r.content).toContain('could not be reached');
    expect(r.content).toContain('(see attached)');
  });

  it('plain names land verbatim in attachment (markdown not escaped)', () => {
    const successNames = Array.from({ length: 100 }, () => '*alice*_long_name_to_force_overflow');
    const r = renderSendConfirm({ ...baseArgs, delivered: successNames.length, successNames });
    expect(r.attachmentText).toContain('*alice*_long_name_to_force_overflow');
    expect(r.attachmentText).not.toContain('\\*alice\\*');
    expect(r.content).toContain('\\*alice\\*');
  });

  it('zero recipients (delivered=0): no Recipients line, no attachment', () => {
    const r = renderSendConfirm({ ...baseArgs });
    expect(r.content).not.toContain('Recipients:');
    expect(r.attachmentText).toBeNull();
    expect(r.needsExpand).toBe(false);
  });

  it('failed-only overflow: NOT DELIVERED block alone, no DELIVERED block, no leading separator', () => {
    const failedNamesPlain = Array.from({ length: 200 }, (_, i) => `failed_${String(i).padStart(3, '0')}_with_long_name_to_force_overflow`);
    const r = renderSendConfirm({
      ...baseArgs, delivered: 0, failedNamesPlain,
    });
    expect(r.attachmentText).toMatch(/^NOT DELIVERED \(200\):\n/);
    expect(r.attachmentText).not.toContain('DELIVERED (0):');
    expect(r.attachmentText).not.toMatch(/\n\n/); // no orphan separator
    expect(r.content).toContain('could not be reached');
    expect(r.content).toContain('(see attached)');
    expect(r.content).not.toContain('Recipients:');
  });

  it('overflow-vs-inline boundary at REVOKE_CONTENT_SAFE_MAX', () => {
    const make = (n) => Array.from({ length: n }, (_, i) => `aaaaaaaaaaaaa${String(i).padStart(7, '0')}`);
    const namesUnder = make(80);
    const under = renderSendConfirm({
      ...baseArgs, delivered: namesUnder.length, successNames: namesUnder, showAll: true,
    });
    expect(under.attachmentText).toBeNull();
    expect(under.content.length).toBeLessThanOrEqual(1900);

    const namesOver = make(95);
    const over = renderSendConfirm({
      ...baseArgs, delivered: namesOver.length, successNames: namesOver, showAll: true,
    });
    expect(over.attachmentText).not.toBeNull();
    expect(over.content.length).toBeLessThanOrEqual(2000);
  });

  it('(see attached) only on lines that were truncated', () => {
    const failedNamesPlain = ['fail1', 'fail2'];
    const successNames = Array.from({ length: 200 }, (_, i) => `verylongusername${String(i).padStart(4, '0')}`);
    const r = renderSendConfirm({
      ...baseArgs, delivered: successNames.length,
      successNames, failedNamesPlain, showAll: true,
    });
    expect(r.content).toContain('2 could not be reached: fail1, fail2');
    expect(r.content).not.toMatch(/could not be reached:[^\n]*\(see attached\)/);
    expect(r.content).toMatch(/Recipients:.*\(see attached\)/);
    expect(r.attachmentText).toContain('DELIVERED (200):');
    expect(r.attachmentText).toContain('NOT DELIVERED (2):');
  });
});

describe('renderRevokeMsg', () => {
  it('lists all names + no expand button when count <= TRUNC_LIMIT', () => {
    const r = renderRevokeMsg('send-1', ['alice', 'bob'], 2, false, 2);
    expect(r.content).toContain('Revoked 2/2 users');
    expect(r.content).toContain('Revoked for: alice, bob');
    expect(r.needsExpand).toBe(false);
    expect(r.row).toBeNull();
  });

  it('truncates with "+N more" + adds Show Recipients button when count > TRUNC_LIMIT', () => {
    const names = Array.from({ length: REVOKE_TRUNC_LIMIT + 3 }, (_, i) => `u${i}`);
    const r = renderRevokeMsg('send-2', names, names.length, false, names.length);
    expect(r.content).toContain(`+${3} more`);
    expect(r.content).not.toContain(names.at(-1)); // last name truncated off
    expect(r.needsExpand).toBe(true);
    expect(r.row).not.toBeNull();
    expect(r.row.components[0].setLabel).toHaveBeenCalledWith('Show Recipients');
  });

  it('shows full list + Hide Recipients button when showAll=true', () => {
    const names = Array.from({ length: REVOKE_TRUNC_LIMIT + 2 }, (_, i) => `u${i}`);
    const r = renderRevokeMsg('send-3', names, names.length, true, names.length);
    expect(r.content).toContain(names.at(-1));
    expect(r.content).not.toMatch(/\+\d+ more/);
    expect(r.needsExpand).toBe(true);
    expect(r.row.components[0].setLabel).toHaveBeenCalledWith('Hide Recipients');
  });

  it('omits the names line when no revocation succeeded', () => {
    const r = renderRevokeMsg('send-4', [], 5, false, 0);
    expect(r.content).toContain('Revoked 0/5 users');
    expect(r.content).not.toContain('Revoked for:');
    expect(r.row).toBeNull();
  });

  it('singularizes "user" when total === 1', () => {
    const r = renderRevokeMsg('send-5', ['alice'], 1, false, 1);
    expect(r.content).toContain('Revoked 1/1 user.');
    expect(r.content).not.toContain('1/1 users');
  });

  it('omits the existing-session caveat when total === 0 (nothing was attempted)', () => {
    const r = renderRevokeMsg('send-empty', [], 0, false, 0);
    expect(r.content).not.toContain('sessions already opened');
  });

  it('emits attachmentText + suppresses Show Recipients when full list would exceed Discord 2000-char cap', () => {
    const names = Array.from({ length: 200 }, (_, i) => `verylongusername${String(i).padStart(4, '0')}`);
    const r = renderRevokeMsg('send-cap', names, names.length, /* showAll */ true, names.length);
    expect(r.content.length).toBeLessThanOrEqual(2000);
    expect(r.content).toContain('(see attached)');
    expect(r.attachmentText).not.toBeNull();
    expect(r.attachmentText.split('\n')).toHaveLength(200);
    expect(r.attachmentText).toContain(names[199]);
    expect(r.needsExpand).toBe(false);
    expect(r.row).toBeNull();
  });

  it('does NOT emit attachmentText when full list fits inline', () => {
    const r = renderRevokeMsg('send-fits', ['alice', 'bob', 'carol'], 3, false, 3);
    expect(r.attachmentText).toBeNull();
  });

  it('attachmentText is plain; content escapes markdown per name', () => {
    const names = ['*alice*', 'normal', '[bob](evil)'];
    const many = Array.from({ length: 200 }, () => '*alice*');
    const r = renderRevokeMsg('send-md', many, many.length, true, many.length);
    expect(r.attachmentText).toContain('*alice*');
    expect(r.attachmentText).not.toContain('\\*alice\\*');
    expect(r.content).toContain('\\*alice\\*'); // preview line is escaped

    const inline = renderRevokeMsg('send-md2', names, names.length, false, names.length);
    expect(inline.content).toContain('\\*alice\\*');
    expect(inline.content).toContain('\\[bob\\]\\(evil\\)');
  });

  it('header uses explicit success arg, not names.length', () => {
    const r = renderRevokeMsg('send-mismatch', ['alice', 'bob', 'carol', 'dave'], 5, false, 5);
    expect(r.content).toMatch(/^Revoked 5\/5 users\./);
    expect(r.content).toContain('Revoked for: alice, bob, carol, dave');
  });

  it('returns a truthful fallback for an undefined authoritative success count', () => {
    const rendered = renderRevokeMsg('send-missing', ['alice'], 1, false);
    expect(rendered.content).toMatch(/could not display the revocation result/i);
    expect(rendered.content).toContain('If this send still appears in `/qurl revoke`, retry it there.');
    expect(rendered.row).toBeNull();
    expect(logger.error).toHaveBeenCalledWith(
      'Failed to render revoke result',
      expect.objectContaining({ sendId: 'send-missing', total: 1 }),
    );
  });

  it('keeps successful DELETEs truthful when final revoked state could not be saved', () => {
    const rendered = renderRevokeMsg('send-finalize-fail', ['alice'], 1, false, 1, true);
    expect(rendered.content).toContain('Revoked 1/1 user.');
    expect(rendered.content).toContain('could not save the final revocation state');
    expect(rendered.content).toContain('Retry with `/qurl revoke` to finish.');
  });
});

describe('buildRevokeHeader (slash-command revoke path)', () => {
  // eslint-disable-next-line global-require
  const { buildRevokeHeader } = require('../src/revoke-render');

  it('zero-attempt: "Revoked 0/0 users." (no already-opened note)', () => {
    expect(buildRevokeHeader(0, 0)).toBe('Revoked 0/0 users.');
  });

  it('singular: "Revoked 1/1 user." (singular noun + existing-session caveat)', () => {
    expect(buildRevokeHeader(1, 1)).toBe('Revoked 1/1 user. Revocation blocks new link access; sessions already opened may remain active.');
  });

  it('plural: "Revoked 3/5 users." distinguishes failures from the existing-session caveat', () => {
    expect(buildRevokeHeader(3, 5)).toBe('Revoked 3/5 users. Could not confirm revocation for 2 users. Retry with `/qurl revoke`; if this continues, run `/qurl setup` and reconnect. Revocation blocks new link access; sessions already opened may remain active.');
  });

  it('full failure reports unconfirmed revocation without claiming any existing sessions', () => {
    expect(buildRevokeHeader(0, 2)).toBe('Revoked 0/2 users. Could not confirm revocation for 2 users. Retry with `/qurl revoke`; if this continues, run `/qurl setup` and reconnect.');
  });

  it.each([
    [undefined, 1],
    [1, undefined],
    [-1, 1],
    [2, 1],
    [0.5, 1],
  ])('rejects invalid success/total pair (%s/%s)', (success, total) => {
    expect(() => buildRevokeHeader(success, total)).toThrow(/success and total must be/);
  });

  it('keeps the slash-command interaction truthful when result counts are inconsistent', () => {
    expect(safeRevokeHeader('send-bad-count', 2, 1)).toMatch(/could not display the revocation result/i);
    expect(logger.error).toHaveBeenCalledWith(
      'Failed to render revoke result',
      expect.objectContaining({ sendId: 'send-bad-count', success: 2, total: 1 }),
    );
  });
});

function makeUsersCollection(users) {
  return {
    filter: jest.fn((fn) => {
      const filtered = users.filter(fn);
      return {
        values: () => filtered,
      };
    }),
  };
}

describe('handleAddRecipients — pre-flight guards', () => {
  it('returns "Send configuration not found" when getSendConfig misses', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce(null);

    const result = await handleAddRecipients(
      'send-1', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toBe('Send configuration not found.');
    expect(result.delivered).toBe(0);
  });

  it('returns "No valid recipients" when only bots/sender are in selection', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });

    const result = await handleAddRecipients(
      'send-1',
      makeUsersCollection([
        { id: 'sender-1', username: 'Sender', bot: false },
        { id: 'bot-1', username: 'Botty', bot: true },
      ]),
      makeInteraction({ user: { id: 'sender-1', username: 'Sender' } }),
      'apikey',
    );

    expect(result.msg).toMatch(/no valid recipients/i);
    expect(result.delivered).toBe(0);
  });

  it('returns "incomplete" when send config has neither file nor location', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: null, actual_url: null, expires_in: '30m',
    });

    const result = await handleAddRecipients(
      'send-1', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toMatch(/incomplete/i);
  });

  it.each([
    ['revoked', { revoked_at: '2026-06-17T20:00:00Z' }, 'Cannot add recipients — this send has already been revoked.'],
    ['being revoked', { revoking_at: '2026-06-17T20:00:00Z' }, 'Cannot add recipients — revocation is pending; retry `/qurl revoke`.'],
  ])('refuses before minting when the send was %s while Add Recipients was pending', async (_stateName, revocationState, expectedMessage) => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
      ...revocationState,
    });

    const result = await handleAddRecipients(
      'send-revoked', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toBe(expectedMessage);
    expect(mockDownloadAndUpload).not.toHaveBeenCalled();
    expect(mockMintLinks).not.toHaveBeenCalled();
    expect(mockDb.recordQURLSendBatch).not.toHaveBeenCalled();
  });

  it('returns newRecipients with {id, username} pairs (post-Add revoke wiring)', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: null, actual_url: null, expires_in: '30m',
    });

    const result = await handleAddRecipients(
      'send-1',
      makeUsersCollection([
        { id: 'u1', username: 'Alice', bot: false },
        { id: 'u2', username: 'Bob', bot: false },
      ]),
      makeInteraction(),
      'apikey',
    );

    expect(result.newRecipients).toEqual([
      { id: 'u1', username: 'Alice' },
      { id: 'u2', username: 'Bob' },
    ]);
  });

  test.each([
    ['off-set numeric-style', '25h'],
    ['totally bogus', 'never'],
    ['empty string', ''],
    ['undefined', undefined],
    ['null', null],
    ['number (not string)', 24],
    ['NaN', NaN],
  ])('refuses when sendConfig.expires_in=%s (off allowed set) (#352)', async (_label, expiresIn) => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1',
      expires_in: expiresIn,
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });

    const result = await handleAddRecipients(
      'send-1', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toMatch(/saved expiry is invalid/i);
    expect(result.msg).toMatch(/original send's links still work/i);
    expect(mockDb.recordQURLSendBatch).not.toHaveBeenCalled();
    expect(logger.warn).toHaveBeenCalledWith(
      'addRecipients refused invalid expires_in',
      expect.objectContaining({ sendId: 'send-1', expiresIn: String(expiresIn) }),
    );
  });
});

describe('handleAddRecipients — file path failure modes', () => {
  it('refuses when a legacy stored file config is missing attachment_url', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '30m',
      attachment_url: null, attachment_name: 'x.png', attachment_content_type: 'image/png',
    });

    const result = await handleAddRecipients(
      'send-1', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toMatch(/no longer available/i);
  });

  it('refuses when stored attachment_url is not a Discord CDN URL (SSRF guard)', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '30m',
      attachment_url: 'https://evil.example.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });

    const result = await handleAddRecipients(
      'send-1', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toMatch(/no longer valid/i);
    expect(logger.error).toHaveBeenCalledWith(
      'addRecipients refused non-Discord attachment_url',
      expect.objectContaining({ sendId: 'send-1' }),
    );
  });

  it('CDN re-download failure (no err.status) emits reason=unknown + shows the expired message', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });
    mockDownloadAndUpload.mockRejectedValueOnce(new Error('403 Forbidden')); // bare Error, no status

    const result = await handleAddRecipients(
      'send-cdn', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toMatch(/expired/i);
    expect(logger.audit).toHaveBeenCalledWith('qurl_send_create_link_failure', expect.objectContaining({
      send_id: 'send-cdn', kind: 'file', reason: 'unknown',
    }));
  });

  it('a non-expiry-shaped failure shows the generic message and emits', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });
    mockDownloadAndUpload.mockRejectedValueOnce(new Error('something else broke'));

    const result = await handleAddRecipients(
      'send-generic', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toMatch(/failed to prepare links/i);
    expect(logger.audit).toHaveBeenCalledWith('qurl_send_create_link_failure', expect.objectContaining({
      send_id: 'send-generic', kind: 'file', reason: 'unknown',
    }));
  });

  it('a connector failure carrying err.status emits the classified reason (403 → upstream_4xx)', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });
    mockDownloadAndUpload.mockRejectedValueOnce(Object.assign(new Error('Connector re-upload failed (403)'), { status: 403 }));

    const result = await handleAddRecipients(
      'send-403', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(logger.audit).toHaveBeenCalledWith('qurl_send_create_link_failure', expect.objectContaining({
      send_id: 'send-403', kind: 'file', reason: 'upstream_4xx', status_code: 403,
    }));
  });

  it('reports underdelivery when mintLinks returns fewer links than recipients', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-new', fileBuffer: new ArrayBuffer(10) });
    mockMintLinks.mockResolvedValueOnce([
      { qurl_link: 'https://q.test/1' },  // only 1 minted, 2 recipients
    ]);

    const result = await handleAddRecipients(
      'send-1', makeUsersCollection([
        { id: 'u1', username: 'Alice', bot: false },
        { id: 'u2', username: 'Bob', bot: false },
      ]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toMatch(/Only 1 of 2/);
    expect(result.delivered).toBe(0);
  });

  it('surfaces "Link pool exhausted" on a 429 error from the location path (outer catch)', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: null, actual_url: 'https://maps.example.com/x',
      location_name: 'Eiffel Tower', expires_in: '30m',
    });
    mockUploadJsonToConnector.mockResolvedValueOnce({ resource_id: 'res-loc-new' });
    mockMintLinks.mockRejectedValueOnce(new Error('HTTP 429: rate limit exceeded'));

    const result = await handleAddRecipients(
      'send-1', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toMatch(/pool exhausted/i);
  });
});

describe('handleAddRecipients — QURL_SEND_CREATE_LINK_FAILURE emission (#276)', () => {
  it('inner file catch: emits with kind=file when the file re-upload + mint fails', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-new', fileBuffer: new ArrayBuffer(10) });
    mockMintLinks.mockRejectedValueOnce(new Error('Connector down'));

    const result = await handleAddRecipients(
      'send-fail', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toMatch(/Failed to prepare links/i);
    expect(logger.audit).toHaveBeenCalledWith('qurl_send_create_link_failure', expect.objectContaining({
      send_id: 'send-fail',
      kind: 'file',
      reason: 'unknown',
    }));
  });

  it('outer location catch: emits with kind=location when the location upload fails', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: null, actual_url: 'https://maps.example.com/x',
      location_name: 'Eiffel Tower', expires_in: '30m',
    });
    mockUploadJsonToConnector.mockRejectedValueOnce(Object.assign(new Error('connector 502'), { status: 502 }));

    const result = await handleAddRecipients(
      'send-loc', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toBe('Failed to create links for new recipients.');
    expect(logger.audit).toHaveBeenCalledWith('qurl_send_create_link_failure', expect.objectContaining({
      send_id: 'send-loc',
      kind: 'location',
      reason: 'upstream_5xx',
      status_code: 502,
    }));
  });

  it('quota_exceeded does NOT emit (viral upload is a normal condition, not a page)', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-new', fileBuffer: new ArrayBuffer(10) });
    mockMintLinks.mockRejectedValueOnce(Object.assign(new Error('upstream quota'), { apiCode: 'quota_exceeded' }));

    await handleAddRecipients(
      'send-quota', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    const failureCalls = logger.audit.mock.calls.filter(c => c[0] === 'qurl_send_create_link_failure');
    expect(failureCalls).toHaveLength(0);
  });

  it('mixed send config is rejected before minting duplicate-key rows', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', actual_url: 'https://maps.example.com/x',
      location_name: 'Mixed', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });

    const result = await handleAddRecipients(
      'send-mixed', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toMatch(/mixed file and location sends are not supported/);
    expect(result.newRecipients).toEqual([]);
    expect(mockDownloadAndUpload).not.toHaveBeenCalled();
    expect(mockUploadJsonToConnector).not.toHaveBeenCalled();
    expect(mockMintLinks).not.toHaveBeenCalled();
    expect(mockDb.recordQURLSendBatch).not.toHaveBeenCalled();
    expect(logger.audit).not.toHaveBeenCalledWith('qurl_send_create_link_failure', expect.anything());
  });

  it('rejects corrupt non-file configs with file payloads before minting', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      resource_type: 'maps',
      connector_resource_id: 'res-1',
      actual_url: null,
      expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png',
      attachment_content_type: 'image/png',
    });

    const result = await handleAddRecipients(
      'send-corrupt', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toMatch(/stored send configuration is unsupported/);
    expect(result.newRecipients).toEqual([]);
    expect(mockDownloadAndUpload).not.toHaveBeenCalled();
    expect(mockUploadJsonToConnector).not.toHaveBeenCalled();
    expect(mockMintLinks).not.toHaveBeenCalled();
    expect(mockDb.recordQURLSendBatch).not.toHaveBeenCalled();
  });
});

describe('executeSendPipeline — QURL_SEND_CREATE_LINK_FAILURE emission (#276, primary site)', () => {
  it('file send: mint failure emits kind=file with the classified reason + status', async () => {
    const interaction = makeInteraction();
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-new', fileBuffer: new ArrayBuffer(8) });
    mockMintLinks.mockRejectedValueOnce(Object.assign(new Error('upstream 503'), { status: 503 }));

    await executeSendPipeline(interaction, makePipelineParams());

    expect(logger.audit).toHaveBeenCalledWith('qurl_send_create_link_failure', expect.objectContaining({
      kind: 'file', // kindMap[RESOURCE_TYPES.FILE] — pins the explicit-map (null-on-unknown) behavior
      reason: 'upstream_5xx',
      status_code: 503,
    }));
  });

  it('file send: mint partial metadata reaches the primary failure log without links', async () => {
    const interaction = makeInteraction();
    const partialErr = Object.assign(new Error('Connector mint_link failed (502)'), {
      status: 502,
      partialLinkCount: 2,
      partialQurlIds: ['q_partial_one', 'q_partial_two'],
    });
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-new', fileBuffer: new ArrayBuffer(8) });
    mockMintLinks.mockRejectedValueOnce(partialErr);

    await executeSendPipeline(interaction, makePipelineParams());

    expect(logger.error).toHaveBeenCalledWith(
      'Failed to prepare QURL links',
      expect.objectContaining({
        status: 502,
        partial_link_count: 2,
        partial_qurl_ids: ['q_partial_one', 'q_partial_two'],
      }),
    );
    expect(JSON.stringify(logger.error.mock.calls)).not.toContain('qurl.link');
    expect(JSON.stringify(logger.error.mock.calls)).not.toContain('at_secret');
  });

  it('file send: quota_exceeded does NOT emit at the primary site either', async () => {
    const interaction = makeInteraction();
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-new', fileBuffer: new ArrayBuffer(8) });
    mockMintLinks.mockRejectedValueOnce(Object.assign(new Error('upstream quota'), { apiCode: 'quota_exceeded' }));

    await executeSendPipeline(interaction, makePipelineParams());

    const failureCalls = logger.audit.mock.calls.filter(c => c[0] === 'qurl_send_create_link_failure');
    expect(failureCalls).toHaveLength(0);
  });

  it('preserves empty-string apiCode as forensic dimension (?? null, not || null)', async () => {
    const interaction = makeInteraction();
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-new', fileBuffer: new ArrayBuffer(8) });
    mockMintLinks.mockRejectedValueOnce(Object.assign(new Error('upstream 422'), { status: 422, apiCode: '' }));

    await executeSendPipeline(interaction, makePipelineParams());

    expect(logger.audit).toHaveBeenCalledWith('qurl_send_create_link_failure', expect.objectContaining({
      api_code: '', // would collapse to null under `|| null`
      reason: 'upstream_4xx',
      status_code: 422,
    }));
  });
});

describe('executeSendPipeline — orphaned qURL log safety', () => {
  it('logs cleanup identifiers without the live access link when DDB persistence fails', async () => {
    const interaction = makeInteraction();
    mockDownloadAndUpload.mockResolvedValueOnce({
      resource_id: 'resource-public-id',
      fileBuffer: new ArrayBuffer(8),
    });
    mockMintLinks.mockResolvedValueOnce([
      {
        qurl_link: 'https://qurl.link/#at_secret_bearer_one',
        resource_id: 'resource-public-id',
        qurl_id: 'q_orphaned_link_one',
      },
      {
        qurl_link: 'https://qurl.site/#at_secret_bearer_two',
        resource_id: 'resource-public-id',
        qurl_id: 'q_orphaned_link_two',
      },
    ]);
    mockDb.recordQURLSendBatch.mockRejectedValueOnce(Object.assign(
      new Error('DDB rejected https://qurl.link/#at_secret_bearer_one'),
      {
        name: 'ValidationException',
        code: 'ValidationException',
        $fault: 'client',
        $metadata: { httpStatusCode: 400, requestId: 'ddb-request-123' },
      },
    ));

    await executeSendPipeline(interaction, makePipelineParams({
      recipients: [
        { id: 'u1', username: 'u1' },
        { id: 'u2', username: 'u2' },
      ],
    }));

    const failureLog = logger.error.mock.calls.find(
      ([message]) => message === 'recordQURLSendBatch failed; aborting send to keep state consistent',
    );
    expect(failureLog).toBeDefined();
    expect(failureLog[1].orphanedResources).toEqual([
      { resourceId: 'resource-public-id', qurlId: 'q_orphaned_link_one' },
      { resourceId: 'resource-public-id', qurlId: 'q_orphaned_link_two' },
    ]);
    expect(failureLog[1]).toEqual(expect.objectContaining({
      errorName: 'ValidationException',
      errorCode: 'ValidationException',
      errorFault: 'client',
      httpStatusCode: 400,
      requestId: 'ddb-request-123',
      errorMessage: 'DDB rejected [REDACTED_URL]',
    }));
    const everythingLogged = JSON.stringify([
      logger.error.mock.calls,
      logger.warn.mock.calls,
      logger.info.mock.calls,
      logger.debug.mock.calls,
      logger.audit.mock.calls,
    ]);
    expect(everythingLogged).not.toContain('https://qurl.link/#at_secret_bearer_one');
    expect(everythingLogged).not.toContain('https://qurl.site/#at_secret_bearer_two');
    expect(everythingLogged).not.toContain('at_secret_bearer_one');
    expect(everythingLogged).not.toContain('at_secret_bearer_two');
    expect(mockSendDM).not.toHaveBeenCalled();
  });

  it('keeps a scrubbed message for non-AWS persistence failures', async () => {
    const interaction = makeInteraction();
    mockDownloadAndUpload.mockResolvedValueOnce({
      resource_id: 'resource-public-id',
      fileBuffer: new ArrayBuffer(8),
    });
    mockMintLinks.mockResolvedValueOnce([{
      qurl_link: 'https://qurl.link/#at_secret_bearer_one',
      resource_id: 'resource-public-id',
      qurl_id: 'q_orphaned_link_one',
    }]);
    mockDb.recordQURLSendBatch.mockRejectedValueOnce(
      new TypeError('Persistence format_specifier failed for https://qurl.link/#at_secret_bearer_one at_secret_bearer_two https%3A%2F%2Fqurl.link%2F%23at_secret_bearer_three'),
    );

    await executeSendPipeline(interaction, makePipelineParams());

    const failureLog = logger.error.mock.calls.find(
      ([message]) => message === 'recordQURLSendBatch failed; aborting send to keep state consistent',
    );
    expect(failureLog[1]).toEqual(expect.objectContaining({
      errorName: 'TypeError',
      errorMessage: expect.stringContaining('Persistence format_specifier failed for [REDACTED_URL]'),
    }));
    expect(JSON.stringify(failureLog)).not.toContain('at_secret_bearer_one');
    expect(JSON.stringify(failureLog)).not.toContain('at_secret_bearer_two');
    expect(JSON.stringify(failureLog)).not.toContain('at_secret_bearer_three');
    expect(JSON.stringify(failureLog)).toContain('format_specifier');
  });

  it('keeps a scrubbed diagnostic when persistence rejects with a non-Error value', async () => {
    const interaction = makeInteraction();
    mockDownloadAndUpload.mockResolvedValueOnce({
      resource_id: 'resource-public-id',
      fileBuffer: new ArrayBuffer(8),
    });
    mockMintLinks.mockResolvedValueOnce([{
      qurl_link: 'https://qurl.link/#at_secret_bearer_one',
      resource_id: 'resource-public-id',
      qurl_id: 'q_orphaned_link_one',
    }]);
    mockDb.recordQURLSendBatch.mockRejectedValueOnce(
      'persistence rejected at_secret_bearer_two',
    );

    await executeSendPipeline(interaction, makePipelineParams());

    const failureLog = logger.error.mock.calls.find(
      ([message]) => message === 'recordQURLSendBatch failed; aborting send to keep state consistent',
    );
    expect(failureLog).toBeDefined();
    expect(failureLog[1]).toEqual(expect.objectContaining({
      errorMessage: 'persistence rejected at_[REDACTED]',
    }));
    expect(JSON.stringify(failureLog)).not.toContain('at_secret_bearer_one');
    expect(JSON.stringify(failureLog)).not.toContain('at_secret_bearer_two');
  });

  it('keeps a scalar diagnostic when a rejection object has a non-string message', async () => {
    const interaction = makeInteraction();
    mockDownloadAndUpload.mockResolvedValueOnce({
      resource_id: 'resource-public-id',
      fileBuffer: new ArrayBuffer(8),
    });
    mockMintLinks.mockResolvedValueOnce([{
      qurl_link: 'https://qurl.link/#at_secret_bearer_one',
      resource_id: 'resource-public-id',
      qurl_id: 'q_orphaned_link_one',
    }]);
    mockDb.recordQURLSendBatch.mockRejectedValueOnce({ name: 'WeirdFailure', message: 42 });

    await executeSendPipeline(interaction, makePipelineParams());

    const failureLog = logger.error.mock.calls.find(
      ([message]) => message === 'recordQURLSendBatch failed; aborting send to keep state consistent',
    );
    expect(failureLog[1]).toEqual(expect.objectContaining({
      errorName: 'WeirdFailure',
      errorMessage: '42',
    }));
  });
});

describe('executeSendPipeline — Revoke/Add Recipients mutual exclusion (#199)', () => {
  async function setupRevocableSend() {
    const collectHandlers = {};
    const response = {
      createMessageComponentCollector: jest.fn(() => ({
        on: jest.fn((event, handler) => {
          collectHandlers[event] = handler;
        }),
      })),
    };
    const interaction = makeInteraction({
      editReply: jest.fn().mockResolvedValue(response),
    });
    const revokeStarted = defer();
    const finishRevoke = defer();

    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-initial', fileBuffer: new ArrayBuffer(8) });
    mockMintLinks.mockResolvedValueOnce([
      { qurl_link: 'https://q.test/original', resource_id: 'res-initial', qurl_id: 'q_original' },
    ]);
    mockSendDM.mockResolvedValue({ ok: true, channelId: 'dm-c', messageId: 'dm-m' });
    mockDb.recordQURLSendBatch.mockResolvedValue(undefined);
    mockDb.saveSendConfig.mockResolvedValue(undefined);
    mockDb.getSendItems.mockReset();
    mockDb.getSendItems.mockResolvedValueOnce([
      { recipient_discord_id: 'u1', resource_id: 'res-initial', dm_channel_id: 'dm-c', dm_message_id: 'dm-m' },
    ]);
    mockDb.markSendRevoked.mockResolvedValue(true);
    mockDeleteLink.mockReset();
    mockDeleteLink.mockImplementationOnce(async () => {
      revokeStarted.resolve();
      await finishRevoke.promise;
      return undefined;
    });

    await executeSendPipeline(interaction, makePipelineParams({
      recipients: [{ id: 'u1', username: 'Alice' }],
    }));
    expect(collectHandlers.collect).toEqual(expect.any(Function));
    const sendId = mockDb.saveSendConfig.mock.calls[0][0].sendId;
    const makeClick = (action) => ({
      customId: `qurl_${action}_${sendId}`,
      deferUpdate: jest.fn().mockResolvedValue(undefined),
      editReply: jest.fn().mockResolvedValue(undefined),
      reply: jest.fn().mockResolvedValue(undefined),
    });

    return {
      collect: collectHandlers.collect,
      finishRevoke,
      interaction,
      makeClick,
      revokeStarted,
      sendId,
    };
  }

  it('rejects Add Recipients while Revoke is in flight so post-revoke mints cannot survive', async () => {
    const {
      collect, finishRevoke, interaction, makeClick, revokeStarted,
    } = await setupRevocableSend();
    const revokeClick = makeClick('revoke');
    const addClick = makeClick('add');

    const revokePromise = collect(revokeClick);
    await revokeStarted.promise;
    await collect(addClick);
    finishRevoke.resolve();
    await revokePromise;

    expect(addClick.reply).toHaveBeenCalledWith({
      content: 'Already revoking links for this send.',
      ephemeral: true,
    });
    expect(mockMintLinks).toHaveBeenCalledTimes(1);
    expect(mockDeleteLink).toHaveBeenCalledTimes(1);
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('Revoked 1/1 user.'),
    }));
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('Revoked for: Alice'),
    }));
  });

  it('rejects Add Recipients while another collector is revoking the same send', async () => {
    const { collect, makeClick, sendId } = await setupRevocableSend();
    revokingSendLocks.add(sendId);

    try {
      const addClick = makeClick('add');
      await collect(addClick);

      expect(addClick.reply).toHaveBeenCalledWith({
        content: 'Already revoking links for this send.',
        ephemeral: true,
      });
      expect(mockMintLinks).toHaveBeenCalledTimes(1);
      expect(mockDb.recordQURLSendBatch).toHaveBeenCalledTimes(1);
      expect(mockDeleteLink).not.toHaveBeenCalled();
    } finally {
      revokingSendLocks.delete(sendId);
    }
  });

  it('tells Revoke clicks when another collector is already revoking the send', async () => {
    const { collect, makeClick, sendId } = await setupRevocableSend();
    revokingSendLocks.add(sendId);

    try {
      const revokeClick = makeClick('revoke');
      await collect(revokeClick);

      expect(revokeClick.reply).toHaveBeenCalledWith({
        content: 'Already revoking links for this send.',
        ephemeral: true,
      });
      expect(revokeClick.deferUpdate).not.toHaveBeenCalled();
      expect(mockDeleteLink).not.toHaveBeenCalled();
    } finally {
      revokingSendLocks.delete(sendId);
    }
  });

  it('does not re-delete and uses generic Add wording when a stale collector sees the send already revoked', async () => {
    const { collect, interaction, makeClick } = await setupRevocableSend();
    mockDb.getSendConfig.mockResolvedValueOnce({ revoked_at: '2026-06-17T00:00:00.000Z' });
    mockDeleteLink.mockReset();

    const revokeClick = makeClick('revoke');
    await collect(revokeClick);

    expect(revokeClick.deferUpdate).toHaveBeenCalled();
    expect(mockDeleteLink).not.toHaveBeenCalled();
    expect(mockDb.markSendRevoked).not.toHaveBeenCalled();
    expect(interaction.editReply).toHaveBeenCalledWith({
      content: 'Links for this send have already been revoked.',
      components: [],
    });

    const addClick = makeClick('add');
    await collect(addClick);

    expect(addClick.reply).toHaveBeenCalledWith({
      content: 'This send is no longer revocable. Add Recipients is disabled.',
      ephemeral: true,
    });
  });

  it('does not claim success when the durable revoke barrier rejects the send', async () => {
    const { collect, interaction, makeClick } = await setupRevocableSend();
    mockDb.markSendRevoking.mockResolvedValueOnce(false);

    await collect(makeClick('revoke'));

    expect(mockDeleteLink).not.toHaveBeenCalled();
    expect(interaction.editReply).toHaveBeenCalledWith({
      content: 'Could not verify this send for revocation. It may already be revoked or unavailable; run `/qurl revoke` to refresh.',
      components: [],
    });

    const addClick = makeClick('add');
    await collect(addClick);
    expect(addClick.reply).toHaveBeenCalledWith({
      content: 'This send is no longer revocable. Add Recipients is disabled.',
      ephemeral: true,
    });
  });

  it('continues button revoke when the revoked-state pre-check fails', async () => {
    const {
      collect, finishRevoke, interaction, makeClick, revokeStarted, sendId,
    } = await setupRevocableSend();
    mockDb.getSendConfig.mockRejectedValueOnce(new Error('DDB read failed'));

    const revokePromise = collect(makeClick('revoke'));
    await revokeStarted.promise;
    finishRevoke.resolve();
    await revokePromise;

    expect(logger.warn).toHaveBeenCalledWith(
      'Could not pre-check send revoked state before button revoke',
      { sendId, error: 'DDB read failed' },
    );
    expect(mockDeleteLink).toHaveBeenCalledTimes(1);
    expect(mockDb.markSendRevoked).toHaveBeenCalledWith(sendId, 'sender-1');
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('Revoked 1/1 user.'),
    }));
  });

  it('reports successful DELETEs truthfully when the final revoked state write fails', async () => {
    const {
      collect, finishRevoke, interaction, makeClick, revokeStarted,
    } = await setupRevocableSend();
    mockDb.markSendRevoked.mockRejectedValueOnce(new Error('DDB finalize failed'));

    const revokePromise = collect(makeClick('revoke'));
    await revokeStarted.promise;
    finishRevoke.resolve();
    await revokePromise;

    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('Revoked 1/1 user.'),
    }));
    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('could not save the final revocation state'),
    }));
    expect(interaction.editReply).not.toHaveBeenCalledWith(expect.objectContaining({
      content: 'Failed to revoke links. Try `/qurl revoke` instead.',
    }));

    const addClick = makeClick('add');
    await collect(addClick);
    expect(addClick.reply).toHaveBeenCalledWith({
      content: 'Links were revoked, but qURL could not save the final state. Add Recipients is disabled; retry `/qurl revoke`.',
      ephemeral: true,
    });
  });

  it('rejects Add Recipients after Revoke succeeds so stale clicks cannot mint new links', async () => {
    const {
      collect, finishRevoke, makeClick, revokeStarted, sendId,
    } = await setupRevocableSend();
    const revokePromise = collect(makeClick('revoke'));
    await revokeStarted.promise;
    finishRevoke.resolve();
    await revokePromise;
    expect(revokingSendLocks.has(sendId)).toBe(false);

    const addClick = makeClick('add');
    await collect(addClick);

    expect(addClick.reply).toHaveBeenCalledWith({
      content: 'Links for this send have already been revoked.',
      ephemeral: true,
    });
    expect(mockMintLinks).toHaveBeenCalledTimes(1);
    expect(mockDeleteLink).toHaveBeenCalledTimes(1);
  });

  it('uses no-live-links wording when stale Add clicks follow an empty revoke', async () => {
    const { collect, makeClick } = await setupRevocableSend();
    mockDb.getSendItems.mockReset();
    mockDb.getSendItems.mockResolvedValueOnce([]);
    mockDeleteLink.mockReset();

    await collect(makeClick('revoke'));

    const addClick = makeClick('add');
    await collect(addClick);

    expect(addClick.reply).toHaveBeenCalledWith({
      content: 'No live links remain for this send.',
      ephemeral: true,
    });
    expect(mockDeleteLink).not.toHaveBeenCalled();
    expect(mockMintLinks).toHaveBeenCalledTimes(1);
  });

  it('uses partial-revoke wording when stale Add clicks follow a partial revoke', async () => {
    const { collect, makeClick } = await setupRevocableSend();
    mockDb.getSendItems.mockReset();
    mockDb.getSendItems.mockResolvedValueOnce([
      { recipient_discord_id: 'u1', resource_id: 'res-ok', dm_channel_id: 'dm-c', dm_message_id: 'dm-m', dm_status: 'sent' },
      { recipient_discord_id: 'u2', resource_id: 'res-failed', dm_channel_id: 'dm-c2', dm_message_id: 'dm-m2', dm_status: 'sent' },
    ]);
    mockDeleteLink.mockReset();
    mockDeleteLink
      .mockResolvedValueOnce(undefined)
      .mockRejectedValueOnce(new Error('delete failed'));

    await collect(makeClick('revoke'));

    const addClick = makeClick('add');
    await collect(addClick);

    expect(addClick.reply).toHaveBeenCalledWith({
      content: 'Revocation is incomplete for this send. Add Recipients is disabled; retry `/qurl revoke`.',
      ephemeral: true,
    });
    expect(mockMintLinks).toHaveBeenCalledTimes(1);
    expect(mockDeleteLink).toHaveBeenCalledTimes(2);
  });

  it('allows Add Recipients after Revoke fails because links still exist', async () => {
    const { collect, makeClick, interaction } = await setupRevocableSend();
    mockDb.getSendItems.mockReset();
    mockDb.getSendItems.mockRejectedValueOnce(new Error('ddb unavailable'));

    const revokeClick = makeClick('revoke');
    await collect(revokeClick);

    expect(interaction.editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: 'Failed to revoke links. Try `/qurl revoke` instead.',
      components: [],
    }));
    mockDeleteLink.mockReset();

    const selectInteraction = {
      users: makeUsersCollection([{ id: 'u2', username: 'Bob', bot: false }]),
      deferUpdate: jest.fn().mockResolvedValue(undefined),
      editReply: jest.fn().mockResolvedValue(undefined),
    };
    const selectReply = {
      awaitMessageComponent: jest.fn().mockResolvedValue(selectInteraction),
    };
    const addClick = makeClick('add');
    addClick.reply.mockResolvedValueOnce(selectReply);

    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-initial', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-added', fileBuffer: new ArrayBuffer(8) });
    mockMintLinks.mockResolvedValueOnce([
      {
        qurl_link: 'https://q.test/added',
        qurl_id: 'q_added',
        resource_id: 'res-added',
      },
    ]);

    try {
      await collect(addClick);

      expect(addClick.reply).toHaveBeenCalledWith(expect.objectContaining({
        content: 'Select additional recipients:',
        fetchReply: true,
      }));
      expect(selectInteraction.deferUpdate).toHaveBeenCalled();
      expect(mockMintLinks).toHaveBeenCalledTimes(2);
      expect(mockDb.recordQURLSendBatch).toHaveBeenCalledTimes(2);
      expect(selectInteraction.editReply).toHaveBeenCalledWith({
        content: 'Added 1 recipient',
        components: [],
      });
    } finally {
      clearCooldown('sender-1');
    }
  });

  it('rejects Revoke while Add Recipients is waiting on selection', async () => {
    const {
      collect, finishRevoke, makeClick, revokeStarted,
    } = await setupRevocableSend();
    const selectPending = defer();
    const selectReply = {
      awaitMessageComponent: jest.fn(() => selectPending.promise),
    };
    const addClick = makeClick('add');
    addClick.reply.mockResolvedValueOnce(selectReply);
    const addPromise = collect(addClick);
    await waitForMicrotaskExpectation(() => {
      expect(selectReply.awaitMessageComponent).toHaveBeenCalled();
    });

    const revokeClick = makeClick('revoke');
    await collect(revokeClick);

    expect(revokeClick.reply).toHaveBeenCalledWith({
      content: 'Already processing an "Add Recipients" action. Finish the current selection or try again in a moment.',
      ephemeral: true,
    });
    expect(mockDeleteLink).not.toHaveBeenCalled();

    selectPending.reject(Object.assign(new Error('time'), { code: 'InteractionCollectorError' }));
    await addPromise;

    const retryClick = makeClick('revoke');
    const retryPromise = collect(retryClick);
    await revokeStarted.promise;
    finishRevoke.resolve();
    await retryPromise;

    expect(retryClick.deferUpdate).toHaveBeenCalled();
    expect(mockDeleteLink).toHaveBeenCalledTimes(1);
  });
});

describe('handleAddRecipients — validate expires_in BEFORE recordQURLSendBatch (#352)', () => {
  afterEach(() => { mockTime.expiryToMs.mockImplementation(jest.requireActual('../src/utils/time').expiryToMs); });

  it('does not write to DB if expiryToMs throws (no orphan rows, no audit-blackhole)', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-new', fileBuffer: new ArrayBuffer(10) });
    mockMintLinks.mockResolvedValueOnce([{ qurl_id: 'q_aaaaaaaaaa1', qurl_link: 'https://q.test/1', resource_id: 'res-new' }]);
    mockTime.expiryToMs.mockImplementationOnce(() => { throw new Error('synthetic expiryToMs failure'); });

    await expect(handleAddRecipients(
      'send-1', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    )).rejects.toThrow(/synthetic expiryToMs failure/);

    expect(mockDb.recordQURLSendBatch).not.toHaveBeenCalled();
    expect(mockSendDM).not.toHaveBeenCalled();
  });
});

describe('handleAddRecipients — DB failure mid-flow', () => {
  it('aborts before DMs when recordQURLSendBatch fails (no orphan live links)', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-new', fileBuffer: new ArrayBuffer(10) });
    mockMintLinks.mockResolvedValueOnce([{ qurl_id: 'q_aaaaaaaaaa1', qurl_link: 'https://q.test/1', resource_id: 'res-new' }]);
    mockDb.recordQURLSendBatch.mockRejectedValueOnce(new Error('DB unavailable'));

    const result = await handleAddRecipients(
      'send-1', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toMatch(/Failed to save link records/);
    expect(result.delivered).toBe(0);
    expect(mockSendDM).not.toHaveBeenCalled();
    expect(mockDeleteLink).toHaveBeenCalledWith('res-new', 'apikey');
    expect(logger.info).toHaveBeenCalledWith(
      'Cleaned up freshly minted Add Recipients qURL resources',
      expect.objectContaining({
        sendId: 'send-1',
        reason: 'guarded_transaction_failed',
        total: 1,
      }),
    );
  });

  it('reports revoked when recordQURLSendBatch loses the revoked_at condition race', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-new', fileBuffer: new ArrayBuffer(10) });
    mockMintLinks.mockResolvedValueOnce([{ qurl_id: 'q_aaaaaaaaaa1', qurl_link: 'https://q.test/1', resource_id: 'res-new' }]);
    const err = new Error('revoked');
    err.code = 'SEND_CONFIG_REVOKED';
    mockDb.recordQURLSendBatch.mockRejectedValueOnce(err);

    const result = await handleAddRecipients(
      'send-1', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toBe('Cannot add recipients — this send has already been revoked.');
    expect(result.delivered).toBe(0);
    expect(result.newRecipients).toEqual([]);
    expect(mockDeleteLink).toHaveBeenCalledWith('res-new', 'apikey');
    expect(mockSendDM).not.toHaveBeenCalled();
  });

  it('logs cleanup failures but still reports revoked when the guarded write loses the race', async () => {
    const sensitiveResourceId = 'at_sensitive-cleanup-token';
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: sensitiveResourceId, fileBuffer: new ArrayBuffer(10) });
    mockMintLinks.mockResolvedValueOnce([{ qurl_id: 'q_aaaaaaaaaa1', qurl_link: 'https://q.test/1', resource_id: sensitiveResourceId }]);
    const err = new Error('revoked');
    err.code = 'SEND_CONFIG_REVOKED';
    mockDb.recordQURLSendBatch.mockRejectedValueOnce(err);
    mockDeleteLink.mockRejectedValueOnce(new Error('delete failed'));

    const result = await handleAddRecipients(
      'send-1', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toBe('Cannot add recipients — this send has already been revoked.');
    expect(result.newRecipients).toEqual([]);
    expect(logger.error).toHaveBeenCalledWith(
      'Failed to clean up freshly minted Add Recipients qURL resources',
      expect.objectContaining({
        sendId: 'send-1',
        reason: 'revoked_guard',
        failed_count: 1,
        total: 1,
        failures: [{ resource_ref: resourceIdLogRef(sensitiveResourceId), error: 'delete failed' }],
      }),
    );
    expect(JSON.stringify(logger.error.mock.calls)).not.toContain(sensitiveResourceId);
    expect(mockSendDM).not.toHaveBeenCalled();
  });

  it('refuses oversized guarded batches before DDB and cleans up freshly minted resources', async () => {
    const users = Array.from({ length: 100 }, (_, i) => ({
      id: `u${i}`,
      username: `User ${i}`,
      bot: false,
    }));
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-0', fileBuffer: new ArrayBuffer(10) });
    let reuploadIndex = 1;
    mockReUploadBuffer.mockImplementation(async () => ({ resource_id: `res-${reuploadIndex++}` }));
    for (let batch = 0; batch < 10; batch++) {
      mockMintLinks.mockResolvedValueOnce(Array.from({ length: 10 }, (_, i) => ({
        qurl_id: `q_${batch}_${i}`,
        qurl_link: `https://q.test/${batch}/${i}`,
      })));
    }
    mockDeleteLink.mockResolvedValue(undefined);

    const result = await handleAddRecipients(
      'send-1', makeUsersCollection(users), makeInteraction(), 'apikey',
    );

    expect(result.msg).toBe('Cannot add recipients — too many recipients selected. Try fewer recipients.');
    expect(result.delivered).toBe(0);
    expect(result.newRecipients).toEqual([]);
    expect(mockDb.recordQURLSendBatch).not.toHaveBeenCalled();
    expect(mockSendDM).not.toHaveBeenCalled();
    expect(mockDeleteLink).toHaveBeenCalledTimes(10);
    expect(mockDeleteLink.mock.calls.map(call => call[0]).sort()).toEqual(
      Array.from({ length: 10 }, (_, i) => `res-${i}`).sort(),
    );
  });
});

describe('handleAddRecipients — happy path (location)', () => {
  it('mints, records, DMs, returns delivered count', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      resource_type: 'maps',
      connector_resource_id: 'res-existing-location',
      actual_url: 'https://maps.example.com/x',
      location_name: 'Eiffel Tower', expires_in: '30m', personal_message: 'check this out',
    });
    mockUploadJsonToConnector.mockResolvedValueOnce({ resource_id: 'res-loc-new' });
    mockMintLinks.mockResolvedValueOnce([
      { qurl_link: 'https://q.test/1', resource_id: 'res-loc-new' },
      { qurl_link: 'https://q.test/2', resource_id: 'res-loc-new' },
    ]);
    mockSendDM.mockResolvedValue({ ok: true, channelId: 'dm-c', messageId: 'dm-m' });
    mockDb.recordQURLSendBatch.mockResolvedValue(undefined);

    const result = await handleAddRecipients(
      'send-1', makeUsersCollection([
        { id: 'u1', username: 'Alice', bot: false },
        { id: 'u2', username: 'Bob', bot: false },
      ]),
      makeInteraction(), 'apikey',
    );

    expect(result.delivered).toBe(2);
    expect(result.failed).toBe(0);
    expect(result.msg).toMatch(/Added 2 recipients/);
    expect(mockDb.markSendDMDelivered).toHaveBeenCalledTimes(2);
    const emitted = logger.audit.mock.calls.map(c => c[0]);
    expect(emitted).toEqual(expect.arrayContaining(['upload_success', 'dispatch_sent']));
    expect(logger.audit).toHaveBeenCalledWith('upload_success', expect.objectContaining({
      send_id: 'send-1', kind: 'location',
    }));
    expect(emitted.filter(e => e === 'dispatch_sent')).toHaveLength(2);
    expect(emitted).not.toContain('mint_success');
    expect(emitted).not.toContain('mint_failed');
  });

  it('packs the trust button once at the bottom (not per-link) for the bulk path', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: null, actual_url: 'https://maps.example.com/x',
      location_name: 'Eiffel Tower', expires_in: '30m',
    });
    mockUploadJsonToConnector.mockResolvedValueOnce({ resource_id: 'res-loc-pack' });
    mockMintLinks.mockResolvedValueOnce([
      { qurl_link: 'https://q.test/pack', resource_id: 'res-loc-pack' },
    ]);
    mockSendDM.mockResolvedValue({ ok: true, channelId: 'dm-c', messageId: 'dm-m' });
    mockDb.recordQURLSendBatch.mockResolvedValue(undefined);

    await handleAddRecipients(
      'send-pack', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(mockSendDM).toHaveBeenCalledTimes(1);
    const [, payload] = mockSendDM.mock.calls[0];
    expect(payload.components).toHaveLength(1);
    const buttons = payload.components[0].components;
    expect(buttons).toHaveLength(2);
  });

  it('delivers an over-512-character qv2 link without invalid Discord components', async () => {
    const qv2Link = `https://qurl.link/#qv2t1.${'A'.repeat(600)}`;
    expect(qv2Link.length).toBeGreaterThan(512);
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: null, actual_url: 'https://maps.example.com/x',
      location_name: 'Eiffel Tower', expires_in: '30m',
    });
    mockUploadJsonToConnector.mockResolvedValueOnce({ resource_id: 'res-loc-qv2' });
    mockMintLinks.mockResolvedValueOnce([
      { qurl_link: qv2Link, resource_id: 'res-loc-qv2' },
    ]);
    mockSendDM.mockResolvedValue({ ok: true, channelId: 'dm-c', messageId: 'dm-m' });
    mockDb.recordQURLSendBatch.mockResolvedValue(undefined);

    await handleAddRecipients(
      'send-qv2', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    const [, payload] = mockSendDM.mock.calls[0];
    expect(payload.components).toEqual([]);
    const description = payload.embeds[0].setDescription.mock.calls[0][0];
    expect(description).toContain(`[🚪 Step Through](${qv2Link})`);
    expect(description).toContain('[🛡️ What is qURL?](https://layerv.ai/qurl/)');
  });

  it('shares one EmbedBuilder reference across N>1 embeds in the bulk payload primitives', () => {
    const qurlLinks = ['https://q.test/share-file', 'https://q.test/share-loc'];
    const embed = buildDeliveryEmbed({
      senderAlias: 'Sender',
      guildName: 'Guild',
      guildIconUrl: null,
      expiresAt: Math.floor(Date.now() / 1000) + 3600,
      personalMessage: null,
    });
    const payload = {
      embeds: Array(qurlLinks.length).fill(embed),
      components: packBulkDeliveryComponents(qurlLinks),
    };

    expect(payload.embeds).toHaveLength(2);
    expect(payload.embeds[0]).toBe(payload.embeds[1]);
    if (typeof payload.embeds[0].toJSON === 'function') {
      expect(payload.embeds[0].toJSON()).toEqual(payload.embeds[1].toJSON());
    }
    const urls = payload.components
      .flatMap(row => row.components)
      .map(b => b.setURL.mock.calls[0]?.[0])
      .filter(Boolean);
    expect(urls).toEqual([
      'https://q.test/share-file',
      'https://q.test/share-loc',
      'https://layerv.ai/qurl/',
    ]);
  });

  it('does not emit upload_success for unsupported mixed file + location configs', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-file-orig', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
      actual_url: 'https://maps.example.com/x',
      location_name: 'Eiffel Tower',
    });

    const result = await handleAddRecipients(
      'send-mixed', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toMatch(/mixed file and location sends are not supported/);
    const uploadEvents = logger.audit.mock.calls.filter(c => c[0] === 'upload_success');
    expect(uploadEvents).toHaveLength(0);
    expect(mockDownloadAndUpload).not.toHaveBeenCalled();
    expect(mockUploadJsonToConnector).not.toHaveBeenCalled();
    expect(mockMintLinks).not.toHaveBeenCalled();
    expect(mockDb.recordQURLSendBatch).not.toHaveBeenCalled();
  });

  it('reports failed DMs as failed in the return value', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: null, actual_url: 'https://maps.example.com/x',
      location_name: 'Eiffel Tower', expires_in: '30m',
    });
    mockUploadJsonToConnector.mockResolvedValueOnce({ resource_id: 'res-loc-new' });
    mockMintLinks.mockResolvedValueOnce([
      { qurl_link: 'https://q.test/1', resource_id: 'res-loc-new' },
      { qurl_link: 'https://q.test/2', resource_id: 'res-loc-new' },
    ]);
    mockSendDM.mockResolvedValueOnce({ ok: false })
      .mockResolvedValueOnce({ ok: true, channelId: 'dm-c-2', messageId: 'dm-m-2' });
    mockDb.recordQURLSendBatch.mockResolvedValue(undefined);

    const result = await handleAddRecipients(
      'send-1', makeUsersCollection([
        { id: 'u1', username: 'Alice', bot: false },
        { id: 'u2', username: 'Bob', bot: false },
      ]),
      makeInteraction(), 'apikey',
    );

    expect(result.delivered).toBe(1);
    expect(result.failed).toBe(1);
    expect(result.msg).toMatch(/1 could not be reached/);
  });
});

describe('mintLinksInBatches', () => {
  it('mints once for recipientCount <= TOKENS_PER_RESOURCE (10)', async () => {
    mockMintLinks.mockResolvedValueOnce([
      { qurl_link: 'https://q.test/1' },
      { qurl_link: 'https://q.test/2' },
    ]);

    const result = await mintLinksInBatches({
      initialResourceId: 'res-1',
      reuploadFn: jest.fn(),
      expiresAt: new Date().toISOString(),
      recipientCount: 2,
      apiKey: 'apikey',
    });

    expect(mockMintLinks).toHaveBeenCalledTimes(1);
    expect(result).toHaveLength(2);
    expect(result[0].resourceId).toBe('res-1');
  });

  it('re-uploads + mints again when recipientCount > TOKENS_PER_RESOURCE', async () => {
    mockMintLinks
      .mockResolvedValueOnce(Array.from({ length: 10 }, (_, i) => ({ qurl_link: `https://q.test/${i}` })))
      .mockResolvedValueOnce([{ qurl_link: 'https://q.test/10' }]);
    const reuploadFn = jest.fn().mockResolvedValueOnce({ resource_id: 'res-2' });

    const result = await mintLinksInBatches({
      initialResourceId: 'res-1',
      reuploadFn,
      expiresAt: new Date().toISOString(),
      recipientCount: 11,
      apiKey: 'apikey',
    });

    expect(reuploadFn).toHaveBeenCalledTimes(1);
    expect(mockMintLinks).toHaveBeenCalledTimes(2);
    expect(result).toHaveLength(11);
    expect(result[10].resourceId).toBe('res-2');
  });

  it('returns empty array when recipientCount = 0', async () => {
    const result = await mintLinksInBatches({
      initialResourceId: 'res-1',
      reuploadFn: jest.fn(),
      expiresAt: new Date().toISOString(),
      recipientCount: 0,
      apiKey: 'apikey',
    });

    expect(result).toHaveLength(0);
    expect(mockMintLinks).not.toHaveBeenCalled();
  });

  it('forwards guildId to mintLinks on EVERY batch (#1101 attribution)', async () => {
    mockMintLinks
      .mockResolvedValueOnce(Array.from({ length: 10 }, (_, i) => ({ qurl_link: `https://q.test/${i}` })))
      .mockResolvedValueOnce([{ qurl_link: 'https://q.test/10' }]);
    const reuploadFn = jest.fn().mockResolvedValueOnce({ resource_id: 'res-2' });

    await mintLinksInBatches({
      initialResourceId: 'res-1',
      reuploadFn,
      expiresAt: new Date().toISOString(),
      recipientCount: 11,
      apiKey: 'apikey',
      guildId: 'guild-77',
    });

    expect(mockMintLinks).toHaveBeenCalledTimes(2);
    for (const call of mockMintLinks.mock.calls) {
      expect(call[1]).toEqual(expect.objectContaining({ guildId: 'guild-77' }));
    }
  });
});

describe('executeSendPipeline — guild_id threading (#1101)', () => {
  it('threads interaction.guildId into BOTH mintLinks and recordQURLSendBatch (file send)', async () => {
    const interaction = makeInteraction({ guildId: 'guild-1' });
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-new', fileBuffer: Buffer.from('x') });
    mockMintLinks.mockResolvedValueOnce([{ qurl_id: 'q_aaaaaaaaaa1', qurl_link: 'https://q.test/1', resource_id: 'res-new' }]);
    mockSendDM.mockResolvedValue({ ok: true, channelId: 'dm-c', messageId: 'dm-m' });
    mockDb.recordQURLSendBatch.mockResolvedValue(undefined);

    await executeSendPipeline(interaction, makePipelineParams());

    expect(mockMintLinks).toHaveBeenCalledWith(
      expect.any(String),
      expect.objectContaining({ guildId: 'guild-1' }),
    );
    expect(mockDb.recordQURLSendBatch).toHaveBeenCalledWith(
      expect.arrayContaining([expect.objectContaining({ guildId: 'guild-1' })]),
    );
  });
});

describe('executeSendPipeline — view-counter fast-path render-state persist', () => {
  it('persists token + appId + collapsed baseMsg + expected_count after editReply', async () => {
    const interaction = makeInteraction({
      token: 'interaction-tok-live', applicationId: 'app-123', guildId: 'guild-1',
    });
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-new', fileBuffer: Buffer.from('x') });
    mockMintLinks.mockResolvedValueOnce([{ qurl_id: 'q_aaaaaaaaaa1', qurl_link: 'https://q.test/1', resource_id: 'res-new' }]);
    mockSendDM.mockResolvedValue({ ok: true, channelId: 'dm-c', messageId: 'dm-m' });
    mockDb.recordQURLSendBatch.mockResolvedValue(undefined);

    await executeSendPipeline(interaction, makePipelineParams());

    expect(mockDb.saveSendConfirmState).toHaveBeenCalledTimes(1);
    const [sendId, fields] = mockDb.saveSendConfirmState.mock.calls[0];
    expect(typeof sendId).toBe('string');
    expect(fields.interactionToken).toBe('interaction-tok-live');
    expect(fields.interactionAppId).toBe('app-123');
    expect(fields.expectedCount).toBe(1);
    expect(fields.viewedCount).toBe(0);
    expect(typeof fields.confirmBaseMsg).toBe('string');
    expect(fields.confirmBaseMsg).toContain('Sent to 1 user');
    expect(fields.confirmBaseMsg).not.toContain('👀');
    expect(fields.confirmExpiresAt).toBeGreaterThan(Math.floor(Date.now() / 1000));
  });

  it('does NOT arm the fast-path on a view-counter-degraded send (link missing qurl_id)', async () => {
    const interaction = makeInteraction({ token: 'tok-live', applicationId: 'app-123', guildId: 'guild-1' });
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-new', fileBuffer: Buffer.from('x') });
    mockMintLinks.mockResolvedValueOnce([{ qurl_link: 'https://q.test/1', resource_id: 'res-new' }]);
    mockSendDM.mockResolvedValue({ ok: true, channelId: 'dm-c', messageId: 'dm-m' });
    mockDb.recordQURLSendBatch.mockResolvedValue(undefined);

    await executeSendPipeline(interaction, makePipelineParams());

    expect(mockDb.saveSendConfirmState).not.toHaveBeenCalled();
  });

  it('skips the persist when no interaction token is present (legacy/worker w/o token)', async () => {
    const interaction = makeInteraction({ token: undefined, applicationId: 'app-123' });
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-new', fileBuffer: Buffer.from('x') });
    mockMintLinks.mockResolvedValueOnce([{ qurl_id: 'q_aaaaaaaaaa1', qurl_link: 'https://q.test/1', resource_id: 'res-new' }]);
    mockSendDM.mockResolvedValue({ ok: true, channelId: 'dm-c', messageId: 'dm-m' });
    mockDb.recordQURLSendBatch.mockResolvedValue(undefined);

    await executeSendPipeline(interaction, makePipelineParams());

    expect(mockDb.saveSendConfirmState).not.toHaveBeenCalled();
  });

  it('a send-time persist failure is logged-swallowed, not thrown (DMs already delivered)', async () => {
    const interaction = makeInteraction({ token: 'tok', applicationId: 'app-123' });
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-new', fileBuffer: Buffer.from('x') });
    mockMintLinks.mockResolvedValueOnce([{ qurl_id: 'q_aaaaaaaaaa1', qurl_link: 'https://q.test/1', resource_id: 'res-new' }]);
    mockSendDM.mockResolvedValue({ ok: true, channelId: 'dm-c', messageId: 'dm-m' });
    mockDb.recordQURLSendBatch.mockResolvedValue(undefined);
    mockDb.saveSendConfirmState.mockRejectedValueOnce(new Error('ddb throttle'));

    await expect(executeSendPipeline(interaction, makePipelineParams())).resolves.toBeUndefined();
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringContaining('saveSendConfirmState failed'),
      expect.objectContaining({ error: 'ddb throttle' }),
    );
  });
});

describe('handleAddRecipients — guild_id threading (#1101)', () => {
  it('threads originalInteraction.guildId into BOTH mintLinks and recordQURLSendBatch', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-file-orig', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-file-new', fileBuffer: Buffer.from('x') });
    mockMintLinks.mockResolvedValueOnce([{ qurl_link: 'https://q.test/add', resource_id: 'res-file-new' }]);
    mockSendDM.mockResolvedValue({ ok: true, channelId: 'dm-c', messageId: 'dm-m' });
    mockDb.recordQURLSendBatch.mockResolvedValue(undefined);

    await handleAddRecipients(
      'send-add', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction({ guildId: 'guild-1' }), 'apikey',
    );

    expect(mockMintLinks).toHaveBeenCalledWith(
      expect.any(String),
      expect.objectContaining({ guildId: 'guild-1' }),
    );
    expect(mockDb.recordQURLSendBatch).toHaveBeenCalledWith(
      expect.arrayContaining([expect.objectContaining({ guildId: 'guild-1' })]),
      { requireSendConfigUnrevoked: true },
    );
  });
});

describe('executeSendPipeline — attachment.url SSRF re-validation gate', () => {
  test.each([
    ['null attachment', null],
    ['attachment with no url field', { name: 'x.png', contentType: 'image/png' }],
    ['attachment with non-string url', { url: 12345, name: 'x.png' }],
    ['internal localhost URL (SSRF target)', { url: 'http://localhost/internal', name: 'x.png' }],
    ['internal 127.0.0.1 URL', { url: 'http://127.0.0.1:8080/api', name: 'x.png' }],
    ['internal AWS metadata endpoint', { url: 'http://169.254.169.254/latest/meta-data/', name: 'x.png' }],
  ])('throws on %s when resourceType=file', async (_label, attachment) => {
    const interaction = makeInteraction();
    await expect(executeSendPipeline(interaction, makePipelineParams({ attachment })))
      .rejects.toThrow(/attachment\.url failed SSRF re-validation/);
    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringMatching(/Internal error — send cancelled/),
      }),
    );
  });

  test('logger.warn fires BEFORE cancelEdit on the SSRF rejection path', async () => {
    logger.warn.mockClear();
    const interaction = makeInteraction();
    await expect(executeSendPipeline(interaction, makePipelineParams({
      attachment: { ...DEFAULT_ATTACHMENT, url: 'http://localhost/internal' },
    }))).rejects.toThrow(/SSRF re-validation/);
    expect(logger.warn).toHaveBeenCalledWith(
      'executeSendPipeline: attachment.url failed isAllowedSourceUrl gate',
      expect.objectContaining({ user_id: expect.any(String) }),
    );
    expect(logger.warn).toHaveBeenCalledTimes(1);
    expect(interaction.editReply).toHaveBeenCalledTimes(1);
    const warnOrder = logger.warn.mock.invocationCallOrder[0];
    const editReplyOrder = interaction.editReply.mock.invocationCallOrder[0];
    expect(warnOrder).toBeLessThan(editReplyOrder);
  });

  test('SSRF gate is skipped when resourceType is NOT file (location sends carry no user URL)', async () => {
    const params = makePipelineParams({
      resourceType: 'location',
      attachment: { ...DEFAULT_ATTACHMENT, url: 'http://localhost/whatever' },
      locationUrl: 'https://google.com/maps/search/x',
      locationName: 'X',
    });
    await expectGateAccepts(params, /SSRF re-validation/);
  });
});

describe('executeSendPipeline — expiresIn allowed-set gate', () => {
  afterEach(() => { mockTime.expiryToMs.mockImplementation(jest.requireActual('../src/utils/time').expiryToMs); });

  test.each([
    ['off-set numeric-style', '25h'],
    ['totally bogus', 'never'],
    ['empty string', ''],
    ['undefined', undefined],
    ['number (not string)', 24],
    ['NaN', NaN],
  ])('throws on expiresIn=%s', async (_label, expiresIn) => {
    const interaction = makeInteraction();
    await expect(executeSendPipeline(interaction, makePipelineParams({ expiresIn })))
      .rejects.toThrow(/expiresIn must be one of/);
    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringMatching(/Internal error — send cancelled/),
      }),
    );
  });

  test.each(['30m', '1h', '6h', '24h', '7d'])('accepts the allowed value: %s', async (expiresIn) => {
    await expectGateAccepts(makePipelineParams({ expiresIn }), /expiresIn must be one of/);
  });

  test('hoists expiresAt above recordQURLSendBatch so a throw can\'t leave orphan rows (#352)', async () => {
    const { expiryToMs } = require('../src/utils/time');
    expiryToMs.mockImplementationOnce(() => { throw new Error('synthetic expiryToMs throw'); });
    mockDb.recordQURLSendBatch.mockClear();

    mockDownloadAndUpload.mockResolvedValueOnce({
      resource_id: 'res-new', fileBuffer: new ArrayBuffer(10),
    });
    mockMintLinks.mockResolvedValueOnce([
      { qurl_link: 'https://q.test/1', resource_id: 'res-new' },
    ]);

    const interaction = makeInteraction();
    await expect(executeSendPipeline(interaction, makePipelineParams({ expiresIn: '30m' })))
      .rejects.toThrow(/synthetic expiryToMs throw/);

    expect(mockDb.recordQURLSendBatch).not.toHaveBeenCalled();
  });
});

describe('executeSendPipeline — personalMessage shape gate', () => {
  test.each([
    ['object', { text: 'oops' }],
    ['array', ['oops']],
    ['number', 42],
    ['boolean', true],
  ])('throws on non-string non-null personalMessage (%s) — would render [object Object] in DM otherwise', async (_label, personalMessage) => {
    const interaction = makeInteraction();
    await expect(executeSendPipeline(interaction, makePipelineParams({ personalMessage })))
      .rejects.toThrow(/personalMessage must be null or string/);
  });

  test.each([
    ['null', null],
    ['empty string', ''],
    ['short note', 'See you at 5pm.'],
  ])('accepts the allowed shape: %s', async (_label, personalMessage) => {
    await expectGateAccepts(makePipelineParams({ personalMessage }), /personalMessage must be null or string/);
  });
});

describe('executeSendPipeline — recipients shape + cap gates', () => {
  const { QURL_SEND_MAX_RECIPIENTS: RECIPIENT_CAP } = require('../src/config');

  test.each([
    ['empty array', []],
    ['null', null],
    ['undefined', undefined],
    ['plain object', {}],
    ['array-like object', { 0: 'u1', length: 1 }],  // pins Array.isArray-strict (not duck-typed)
    ['string (not array)', 'u1'],
    ['number', 42],
  ])('throws TypeError on non-array-or-empty recipients (%s)', async (_label, recipients) => {
    const interaction = makeInteraction();
    await expect(executeSendPipeline(interaction, makePipelineParams({ recipients })))
      .rejects.toThrow(/recipients must be a non-empty array/);
    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringMatching(/Internal error — send cancelled/),
      }),
    );
  });

  test.each([
    ['null', null, /typeof=object, value=null/],
    ['undefined', undefined, /typeof=undefined, value=undefined/],
    ['plain object', {}, /typeof=object/],
    ['empty array', [], /typeof=object, value=<empty array>/],
  ])('rejection message distinguishes %s in the value-detail field', async (_label, recipients, detailRe) => {
    const interaction = makeInteraction();
    await expect(executeSendPipeline(interaction, makePipelineParams({ recipients })))
      .rejects.toThrow(detailRe);
  });

  test('rejection message truncates pathological values with `…` marker', async () => {
    const interaction = makeInteraction();
    const oneKB = 'x'.repeat(1024);
    await expect(executeSendPipeline(interaction, makePipelineParams({ recipients: oneKB })))
      .rejects.toThrow(/value=x{64}…/);
    const exact64 = 'y'.repeat(64);
    await expect(executeSendPipeline(interaction, makePipelineParams({ recipients: exact64 })))
      .rejects.toThrow(/value=y{64}\)/);
  });

  test.each([
    ['throws on toString', { toString() { throw new Error('nope'); } }],
    ['null-prototype object', Object.create(null)],
  ])('rejection message falls back to <unrepresentable> when String() throws (%s)', async (_label, value) => {
    const interaction = makeInteraction();
    await expect(executeSendPipeline(interaction, makePipelineParams({ recipients: value })))
      .rejects.toThrow(/value=<unrepresentable>/);
  });

  test('truncation slices on code points, not UTF-16 code units (astral-char safety)', async () => {
    const interaction = makeInteraction();
    const sixtyFourEmoji = '🚀'.repeat(64);
    await expect(executeSendPipeline(interaction, makePipelineParams({ recipients: sixtyFourEmoji })))
      .rejects.toThrow(/value=(?:🚀){64}\)/u);
    const sixtyFiveEmoji = '🚀'.repeat(65);
    await expect(executeSendPipeline(interaction, makePipelineParams({ recipients: sixtyFiveEmoji })))
      .rejects.toThrow(/value=(?:🚀){64}…/u);
  });

  test('throws RangeError when recipients.length exceeds QURL_SEND_MAX_RECIPIENTS', async () => {
    const oversized = Array.from({ length: RECIPIENT_CAP + 1 }, (_, i) => ({ id: `u${i}`, username: `u${i}` }));
    const interaction = makeInteraction();
    await expect(executeSendPipeline(interaction, makePipelineParams({ recipients: oversized })))
      .rejects.toThrow(/recipients\.length .* exceeds QURL_SEND_MAX_RECIPIENTS/);
    expect(interaction.editReply).toHaveBeenCalledWith(
      expect.objectContaining({
        content: expect.stringMatching(/Internal error — send cancelled/),
      }),
    );
  });

  test('clears cooldown on the recipients-empty path (same convention as other gates)', async () => {
    const interaction = makeInteraction();
    const { setCooldown, isOnCooldown } = _test;
    interaction.user = { id: 'recipients-empty-test-user', username: 'test' };
    setCooldown(interaction.user.id);
    expect(isOnCooldown(interaction.user.id)).toBe(true);

    await expect(executeSendPipeline(interaction, makePipelineParams({ recipients: [] })))
      .rejects.toThrow(TypeError);
    expect(isOnCooldown(interaction.user.id)).toBe(false);
  });

  test('clears cooldown on the recipients-oversized path (RangeError branch)', async () => {
    const interaction = makeInteraction();
    const { setCooldown, isOnCooldown } = _test;
    const oversized = Array.from({ length: RECIPIENT_CAP + 1 }, (_, i) => ({ id: `u${i}`, username: `u${i}` }));
    interaction.user = { id: 'recipients-oversized-test-user', username: 'test' };
    setCooldown(interaction.user.id);
    expect(isOnCooldown(interaction.user.id)).toBe(true);

    await expect(executeSendPipeline(interaction, makePipelineParams({ recipients: oversized })))
      .rejects.toThrow(RangeError);
    expect(isOnCooldown(interaction.user.id)).toBe(false);
  });

  test.each([
    ['one recipient', [{ id: 'u1', username: 'u1' }]],
    ['several recipients', Array.from({ length: 5 }, (_, i) => ({ id: `u${i}`, username: `u${i}` }))],
    ['exactly at the cap', Array.from({ length: RECIPIENT_CAP }, (_, i) => ({ id: `u${i}`, username: `u${i}` }))],
  ])('accepts the allowed shape: %s', async (_label, recipients) => {
    await expectGateAccepts(
      makePipelineParams({ recipients }),
      /recipients must be a non-empty array/,
      /exceeds QURL_SEND_MAX_RECIPIENTS/,
    );
  });
});

describe('executeSendPipeline — truncForLog applies to value-rendering gates', () => {
  test('expiresIn rejection message is bounded with `…` on oversized input', async () => {
    const interaction = makeInteraction();
    const huge = 'y'.repeat(1024);
    await expect(executeSendPipeline(interaction, makePipelineParams({ expiresIn: huge })))
      .rejects.toThrow(/expiresIn must be one of .* \(got y{64}…\)/);
  });
});

describe('executeSendPipeline — channel notification on @everyone / voice mode', () => {
  beforeEach(() => {
    mockDownloadAndUpload.mockResolvedValue({ resource_id: 'res-1', fileBuffer: new ArrayBuffer(10) });
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/1', resource_id: 'res-1' }]);
    mockSendDM.mockResolvedValue({ ok: true, channelId: 'dm-c', messageId: 'dm-m' });
  });

  test('posts non-ephemeral channel notification when recipientMode is "everyone"', async () => {
    const interaction = makeInteraction({ channelId: 'channel-everyone' });
    await executeSendPipeline(interaction, makePipelineParams({ recipientMode: 'everyone' }));
    expect(mockSendChannelMessage).toHaveBeenCalledTimes(1);
    expect(mockSendChannelMessage).toHaveBeenCalledWith(
      'channel-everyone',
      expect.objectContaining({
        content: expect.stringMatching(/shared something with everyone in this server.*qURL Bot/),
      }),
    );
  });

  test('posts notification with voice-channel copy when recipientMode is "voice"', async () => {
    const interaction = makeInteraction({ channelId: 'channel-voice' });
    await executeSendPipeline(interaction, makePipelineParams({ recipientMode: 'voice' }));
    expect(mockSendChannelMessage).toHaveBeenCalledTimes(1);
    expect(mockSendChannelMessage).toHaveBeenCalledWith(
      'channel-voice',
      expect.objectContaining({
        content: expect.stringMatching(/shared something with everyone in this voice channel.*qURL Bot/),
      }),
    );
  });

  test.each([
    ['picker', 'picker'],
    ['undefined (stale flow row, normalizeRecipientMode fallback)', undefined],
  ])('does NOT post channel notification when recipientMode is %s', async (_label, recipientMode) => {
    const interaction = makeInteraction();
    await executeSendPipeline(interaction, makePipelineParams({ recipientMode }));
    expect(mockSendChannelMessage).not.toHaveBeenCalled();
  });

  test('does NOT post channel notification when delivered === 0 (every DM failed)', async () => {
    mockSendDM.mockResolvedValue({ ok: false, error: 'all DMs blocked' });
    const interaction = makeInteraction();
    await executeSendPipeline(interaction, makePipelineParams({ recipientMode: 'everyone' }));
    expect(mockSendChannelMessage).not.toHaveBeenCalled();
  });

  test('logs warn on REST failure without failing the send (a missing Send Messages permission cannot fail a send whose DMs already delivered)', async () => {
    mockSendChannelMessage.mockResolvedValueOnce({ ok: false, error: 'Missing Permissions', status: 403 });
    logger.warn.mockClear();
    const interaction = makeInteraction({ channelId: 'channel-no-perm' });
    await expect(
      executeSendPipeline(interaction, makePipelineParams({ recipientMode: 'everyone' })),
    ).resolves.not.toThrow();
    await Promise.resolve();
    await Promise.resolve();
    expect(logger.warn).toHaveBeenCalledWith(
      'Failed to send channel notification',
      expect.objectContaining({ channelId: 'channel-no-perm', status: 403 }),
    );
  });

  test('sanitizes the sender display name before posting (bidi/RTL spoof defense)', async () => {
    const interaction = makeInteraction({
      member: { displayName: 'Alice\u202EEvil' },
      user: { id: 'sender-1', username: 'Alice' },
    });
    await executeSendPipeline(interaction, makePipelineParams({ recipientMode: 'everyone' }));
    expect(mockSendChannelMessage).toHaveBeenCalledTimes(1);
    const [, message] = mockSendChannelMessage.mock.calls[0];
    expect(message.content).not.toMatch(/\u202E/u);
  });
});
