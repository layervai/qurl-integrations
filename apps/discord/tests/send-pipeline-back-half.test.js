/**
 * Send-pipeline back-half tests — monitorLinkStatus polling,
 * revokeAllLinks direct path, and handleAddRecipients flow. Covers the
 * code paths exercised after `/qurl send` or `/qurl map` reaches the
 * "Sent to N" confirmation:
 *   - monitorLinkStatus's setInterval body (init, status diff, pending →
 *     opened/expired transitions, addRecipients() generation bump,
 *     stop() race, allDone, max-duration cap, getResourceStatus errors,
 *     activeMonitors LRU eviction)
 *   - revokeAllLinks (deleteLink fan-out, markSendRevoked, partial failures)
 *   - handleAddRecipients (getSendConfig miss, recipient filtering,
 *     file-path re-download failure modes, location path, mint
 *     underdelivery, recordQURLSendBatch failure, pool-exhaustion 429,
 *     DM batch + status update)
 *
 * The functions are accessed via the `_test` export rather than driven
 * through executeSendPipeline so each spec can target one branch without
 * the front-half setup.
 */

// ---------------------------------------------------------------------------
// Mocks — each test file gets its own jest module registry, so mocks are
// file-private (mockMintLinks etc.) to keep specs isolated.
// ---------------------------------------------------------------------------

jest.mock('../src/config', () => ({
  QURL_API_KEY: 'test-api-key',
  QURL_ENDPOINT: 'https://api.test.local',
  CONNECTOR_URL: 'https://connector.test.local',
  GOOGLE_MAPS_API_KEY: 'test-google-key',
  QURL_SEND_COOLDOWN_MS: 30000,
  QURL_SEND_MAX_RECIPIENTS: 50,
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
  markSendRevoked: jest.fn(),
  getSendConfig: jest.fn(),
  saveSendConfig: jest.fn(),
  // qurl_views webhook-fed reader. Default is an empty Map so a test
  // that doesn't override gets the "no views yet" path. Per-test
  // mockResolvedValueOnce drives the status-transition assertions.
  getQurlViews: jest.fn(async () => new Map()),
  recordQurlView: jest.fn(),
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

const mockDeleteLink = jest.fn();
jest.mock('../src/qurl', () => ({
  createOneTimeLink: jest.fn(),
  deleteLink: mockDeleteLink,
}));

jest.mock('../src/places', () => ({ searchPlaces: jest.fn().mockResolvedValue([]) }));

// Stub flow-state so loading commands.js doesn't reach into DDB.
// These tests target the back-half (monitorLinkStatus, revokeAllLinks,
// handleAddRecipients) — none of which touch flow_state — but
// commands.js registers a flow handler at module load and requires
// the module regardless of which export the test consumes.
jest.mock('../src/flow-state', () => ({
  createFlow: jest.fn(),
  loadFlow: jest.fn(),
  transitionFlow: jest.fn(),
  deleteFlow: jest.fn(),
}));

// Partial-mock time.js so individual tests can force `expiryToMs` to
// throw without affecting the rest of the suite. Real implementation
// falls back to DEFAULT_EXPIRY_MS for malformed input (never throws),
// so the audit-blackhole / orphan-DDB-row failure mode this test pins
// against is unreachable today — the mock is the only way to exercise
// the validate-before-DB-write invariant.
jest.mock('../src/utils/time', () => {
  const actual = jest.requireActual('../src/utils/time');
  return {
    ...actual,
    expiryToMs: jest.fn(actual.expiryToMs),
  };
});
const mockTime = require('../src/utils/time');

// ---------------------------------------------------------------------------
// Require modules under test
// ---------------------------------------------------------------------------

const { _test } = require('../src/commands');
const logger = require('../src/logger');
const {
  monitorLinkStatus,
  revokeAllLinks,
  renderRevokeMsg,
  renderSendConfirm,
  REVOKE_TRUNC_LIMIT,
  handleAddRecipients,
  mintLinksInBatches,
  activeMonitors,
  executeSendPipeline,
  persistDispatchResult,
} = _test;

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function makeInteraction(overrides = {}) {
  return {
    user: { id: 'sender-1', username: 'Sender' },
    channelId: 'ch-1',
    editReply: jest.fn().mockResolvedValue(undefined),
    member: { displayName: 'Sender' },
    ...overrides,
  };
}

// Single drift-anchor for the executeSendPipeline param shape used
// across every entry-gate describe block below. Each gate test
// passes `{ [field]: value }` to vary exactly one field; everything
// else stays at the canonical baseline. Adding a new pipeline param
// is a one-line edit here instead of touching every gate's
// describe block.
//
// CAVEAT: overrides REPLACE nested objects wholesale (e.g. passing
// `{ attachment: { url: 'x' } }` drops the baseline's `contentType`
// and `name`). If a test wants to vary just one nested field, spread
// the baseline explicitly: `{ attachment: { ...DEFAULT_ATTACHMENT,
// url: 'x' } }`.
// Frozen so a test forgetting to spread (`DEFAULT_ATTACHMENT.url = ...`)
// can't corrupt the constant for later tests.
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

// Accept-path verifier: pin that the named gate did NOT reject the
// given params. Two branches both count as success:
//   - executeSendPipeline throws something downstream → assert the
//     throw message doesn't match any of the gate-specific rejection
//     regexes. A non-matching downstream throw is fine; what we're
//     pinning is "we got PAST the gate."
//   - executeSendPipeline resolves cleanly → also fine; the gate
//     didn't reject.
// `expect.hasAssertions()` forces both branches to produce at least
// one observation, so a future test-setup tightening that lets the
// await silently resolve doesn't turn this into a vacuous pass
// (was the issue #291 surfaced).
async function expectGateAccepts(params, ...gateRejectionRegexes) {
  expect.hasAssertions();
  const interaction = makeInteraction();
  try {
    await executeSendPipeline(interaction, params);
    // Resolved cleanly — gate accepted. Pin one assertion so
    // hasAssertions() is satisfied on this branch too.
    expect(true).toBe(true);
  } catch (err) {
    for (const re of gateRejectionRegexes) {
      expect(err.message).not.toMatch(re);
    }
  }
}

// monitorLinkStatus polls at max(15s, min(60s, expiryMs/10)).
// Tests use '1m' expiry → expiryMs/10 = 6s → max(15s, 6s) = 15s. So one
// POLL_INTERVAL tick fires the setInterval body once.
const POLL_INTERVAL = 15000;

beforeEach(() => {
  jest.clearAllMocks();
  // Drain any monitors a prior test left registered. activeMonitors is the
  // module-private set; clearing it prevents the LRU-cap eviction test
  // from being polluted by happy-path leftovers.
  for (const m of Array.from(activeMonitors)) m.stop();
});

// ===========================================================================
// monitorLinkStatus
// ===========================================================================

// Canonical qurlLinks shape post-PR (post qurl-service #598 + connector
// #747): every link carries a qurlId, which the monitor uses as the
// BatchGet key against the qurl_views table.
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

    // First tick: one of the two has been viewed (webhook landed via
    // the qurl-webhook receiver, which wrote to qurl_views).
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
    // No editReply because the BatchGet failed before any status diff.
    expect(interaction.editReply).not.toHaveBeenCalled();
    monitor.stop();
  });
});

describe('monitorLinkStatus — empty-qurl_id boundary guard', () => {
  beforeEach(() => { jest.useFakeTimers(); });
  afterEach(() => { jest.useRealTimers(); });

  it('any missing qurlId degrades the whole monitor to bare base-msg', async () => {
    // Mixed batch: one link has qurlId, one does not. Misattribution
    // would render `1 of 2 viewed` from a partial-attribution set —
    // worse UX than no counter. Boundary guard degrades the whole
    // monitor.
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

    // No DDB read happens during degraded mode — the BatchGet is
    // unconditionally skipped at the top of runTick when degraded.
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

  it('subsequent ticks honor standard pollInterval after the fast first tick', async () => {
    const monitor = monitorLinkStatus(
      'send-1', makeInteraction(),
      ONE_LINK_SET,
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent', { components: [] }, 1,
    );

    await jest.advanceTimersByTimeAsync(3000);
    const callsAfterFirstTick = mockDb.getQurlViews.mock.calls.length;
    expect(callsAfterFirstTick).toBeGreaterThanOrEqual(1);

    await jest.advanceTimersByTimeAsync(3000); // t=6s — no tick due
    expect(mockDb.getQurlViews.mock.calls.length).toBe(callsAfterFirstTick);

    await jest.advanceTimersByTimeAsync(12500); // cross t=18s
    expect(mockDb.getQurlViews.mock.calls.length).toBeGreaterThan(callsAfterFirstTick);
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

    // Add a recipient with a new qurl_id. The monitor's next BatchGet
    // must include the new id in its key list.
    monitor.addRecipients(1, [{ qurlId: 'q_aaaaaaaaaa3', username: 'Charlie' }]);
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    expect(mockDb.getQurlViews).toHaveBeenCalled();
    const lastCallKeys = mockDb.getQurlViews.mock.calls.at(-1)[0];
    expect(lastCallKeys).toContain('q_aaaaaaaaaa3');
    monitor.stop();
  });

  it('addRecipients() re-arms the monitor after allDone so post-resolve adds still see views', async () => {
    // cr-flagged repro: send to N → all view → setInterval clears
    // (allDone) → /qurl add M more → without the re-arm, the counter
    // stays frozen for the rest of the 1h cap.
    const interaction = makeInteraction();
    const monitor = monitorLinkStatus(
      'send-resolve-add', interaction,
      ONE_LINK_SET,
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent to 1 user', { components: [] }, 1,
    );

    // Initial recipient views — triggers allDone + clearInterval.
    mockDb.getQurlViews.mockResolvedValueOnce(new Map([
      ['q_aaaaaaaaaa1', { accessCount: 1, consumed: false }],
    ]));
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);
    expect(monitor.getFullMsg()).toBe('Sent to 1 user\n👀 1 viewed / 0 pending');

    // /qurl add a new recipient AFTER the monitor settled.
    monitor.updateBaseMsg('Sent to 2 users');
    const callsBeforeAdd = mockDb.getQurlViews.mock.calls.length;
    monitor.addRecipients(1, [{ qurlId: 'q_aaaaaaaaaa9', username: 'Eve' }]);

    // Re-arm uses the FIRST_POLL_DELAY_MS (3s) fast-tick pattern,
    // mirroring construction. Eve's view should land within a few
    // seconds of /qurl add, not pollInterval later.
    mockDb.getQurlViews.mockResolvedValueOnce(new Map([
      ['q_aaaaaaaaaa1', { accessCount: 1, consumed: false }],
      ['q_aaaaaaaaaa9', { accessCount: 1, consumed: false }],
    ]));
    await jest.advanceTimersByTimeAsync(3500);

    // The post-allDone tick must actually have fired — a missing
    // re-arm would leave callsBeforeAdd == calls.length and the
    // counter frozen at 1/0 instead of advancing to 2/0.
    expect(mockDb.getQurlViews.mock.calls.length).toBeGreaterThan(callsBeforeAdd);
    expect(monitor.getFullMsg()).toBe('Sent to 2 users\n👀 2 viewed / 0 pending');
    monitor.stop();
  });

  it('addRecipients() seeds linkStatus so views on newly-added recipients flip pending → viewed', async () => {
    // Regression guard for the cr-flagged bug: extending trackedQurlIds
    // without also seeding linkStatus left the new recipients invisible
    // to the status-flip loop. Webhook would land, BatchGet would
    // return the row, but the counter never advanced.
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
    // Regression guard for the silent-degrade bug cr round 3 surfaced:
    // construction-time degrade warns once at send-start, but a /qurl
    // add link arriving without qurl_id has to leave a breadcrumb too.
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
    // Counter degrades to bare base msg, NOT a partial-attribution render.
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
    // Same ID already tracked — should be a no-op insertion-wise.
    monitor.addRecipients(1, [{ qurlId: 'q_aaaaaaaaaa1', username: 'Alice' }]);
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);
    const lastCallKeys = mockDb.getQurlViews.mock.calls.at(-1)[0];
    // Set semantics — only one entry for q_aaaaaaaaaa1.
    expect(lastCallKeys.filter(k => k === 'q_aaaaaaaaaa1')).toHaveLength(1);
    monitor.stop();
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
    // Contract: no uncaught throw, no "poll failed" log emission from
    // the racing tick. logger.error firing here would mean stop() let
    // a thrown error escape the setInterval callback.
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
    // Pre-refactor the monitor switched to editDM (bot-token PATCH)
    // past the 14-min cutover to bypass the interaction-token TTL.
    // That fallback is broken on ephemeral messages (executeSendPipeline
    // deferReplies ephemeral, and ephemeral messages can only be edited
    // via the interaction webhook token). The monitor cap was lowered
    // to 14 min so we don't run setIntervals past the usable window.
    // This test pins the contract: no editDM call from the monitor
    // path, no matter what.
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
    // Skip ~14min+1min so the cap branch fires.
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

// ===========================================================================
// revokeAllLinks
// ===========================================================================

describe('revokeAllLinks', () => {
  // Helper: items shape mirrors `getSendItems` return — `{resource_id,
  // recipient_discord_id}` per row. The recipient_discord_id values
  // are surfaced via `successUserIds`/`failureUserIds` so callers can
  // resolve usernames against their in-scope `recipients` array.
  const makeItems = (n) => Array.from({ length: n }, (_, i) => ({
    resource_id: `res-${i + 1}`,
    recipient_discord_id: `user-${i + 1}`,
  }));

  it('calls deleteLink for each resource and markSendRevoked, returns success/total + per-recipient ids', async () => {
    mockDb.getSendItems.mockResolvedValueOnce(makeItems(3));
    mockDeleteLink.mockResolvedValue(undefined);

    const result = await revokeAllLinks('send-1', 'sender-1', 'apikey');

    expect(mockDeleteLink).toHaveBeenCalledTimes(3);
    expect(mockDb.markSendRevoked).toHaveBeenCalledWith('send-1', 'sender-1');
    expect(result).toEqual({
      success: 3,
      total: 3,
      successUserIds: ['user-1', 'user-2', 'user-3'],
      failureUserIds: [],
    });
  });

  it('counts partial failures as success/total mismatch and logs each failure', async () => {
    mockDb.getSendItems.mockResolvedValueOnce(makeItems(2));
    mockDeleteLink
      .mockResolvedValueOnce(undefined)
      .mockRejectedValueOnce(new Error('429 rate limited'));

    const result = await revokeAllLinks('send-1', 'sender-1', 'apikey');

    expect(result.success).toBe(1);
    expect(result.total).toBe(2);
    expect(result.successUserIds).toEqual(['user-1']);
    expect(result.failureUserIds).toEqual(['user-2']);
    expect(logger.error).toHaveBeenCalledWith('Failed to revoke QURL', expect.any(Object));
    // markSendRevoked still fires — partial failures don't block the local
    // record update, since re-picking from /qurl revoke wouldn't help.
    expect(mockDb.markSendRevoked).toHaveBeenCalled();
  });

  it('returns 0/0 when send has no items (already-revoked or unknown sendId)', async () => {
    mockDb.getSendItems.mockResolvedValueOnce([]);

    const result = await revokeAllLinks('send-1', 'sender-1', 'apikey');

    expect(result).toEqual({ success: 0, total: 0, successUserIds: [], failureUserIds: [] });
    expect(mockDeleteLink).not.toHaveBeenCalled();
    expect(mockDb.markSendRevoked).toHaveBeenCalled();
  });

  it('emits revoke_success audit event with success/total tally when at least one link revoked', async () => {
    mockDb.getSendItems.mockResolvedValueOnce(makeItems(2));
    mockDeleteLink.mockResolvedValue(undefined);

    await revokeAllLinks('send-42', 'sender-1', 'apikey');

    expect(logger.audit).toHaveBeenCalledWith('revoke_success', {
      send_id: 'send-42', success: 2, total: 2,
    });
  });

  it('emits revoke_failed (not revoke_success) when every per-link delete throws', async () => {
    mockDb.getSendItems.mockResolvedValueOnce(makeItems(2));
    mockDeleteLink.mockRejectedValue(new Error('429 rate limited'));

    await revokeAllLinks('send-43', 'sender-1', 'apikey');

    const events = logger.audit.mock.calls.map(c => c[0]);
    expect(events).toContain('revoke_failed');
    expect(events).not.toContain('revoke_success');
    expect(logger.audit).toHaveBeenCalledWith('revoke_failed', {
      send_id: 'send-43', success: 0, total: 2,
    });
  });

  it('emits no audit event when there are no resources to revoke (avoids 0/0 noise)', async () => {
    mockDb.getSendItems.mockResolvedValueOnce([]);

    await revokeAllLinks('send-44', 'sender-1', 'apikey');

    const events = logger.audit.mock.calls.map(c => c[0]);
    expect(events).not.toContain('revoke_success');
    expect(events).not.toContain('revoke_failed');
  });

  it('propagates a getSendItems failure to the caller (no DELETE attempted, no audit emitted)', async () => {
    // DDB outage during getSendItems means the function can't safely
    // run revokes (no items to iterate) — propagate the error so the
    // button-click handler surfaces a generic failure to the operator
    // rather than reporting 0/0 success. Pinned for symmetry with the
    // other "DDB throws" cases inside persistDispatchResult.
    mockDb.getSendItems.mockRejectedValueOnce(new Error('DDB throttled'));

    await expect(
      revokeAllLinks('send-throw', 'sender-1', 'apikey'),
    ).rejects.toThrow('DDB throttled');

    expect(mockDeleteLink).not.toHaveBeenCalled();
    const events = logger.audit.mock.calls.map(c => c[0]);
    expect(events).not.toContain('revoke_success');
    expect(events).not.toContain('revoke_failed');
  });

  // mintLinksInBatches packs up to TOKENS_PER_RESOURCE recipients onto
  // a single resource_id. Without grouping, the 2nd…Nth DELETE for a
  // shared resource throws 404 and would mis-classify N-1 recipients
  // as failures even though all their tokens were invalidated by the
  // 1st DELETE.
  it('groups items by resource_id — shared-resource recipients all land in successUserIds on a single DELETE', async () => {
    mockDb.getSendItems.mockResolvedValueOnce([
      { resource_id: 'res-shared', recipient_discord_id: 'u-1' },
      { resource_id: 'res-shared', recipient_discord_id: 'u-2' },
      { resource_id: 'res-shared', recipient_discord_id: 'u-3' },
      { resource_id: 'res-solo',   recipient_discord_id: 'u-4' },
    ]);
    mockDeleteLink.mockResolvedValue(undefined);

    const result = await revokeAllLinks('send-shared', 'sender-1', 'apikey');

    // 1 DELETE per unique resource, not per recipient.
    expect(mockDeleteLink).toHaveBeenCalledTimes(2);
    expect(result.success).toBe(4);
    expect(result.total).toBe(4);
    expect(result.successUserIds.sort()).toEqual(['u-1', 'u-2', 'u-3', 'u-4']);
    expect(result.failureUserIds).toEqual([]);
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

  // Failure-wins semantics: if a recipient has rows on multiple
  // resources and any DELETE fails, they count as failure (not
  // success) — better to tell the operator "alice is partial" than
  // misleadingly claim full success.
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
      // Edit payload MUST include components: [] so the original Step
      // Through button is cleared — Discord doesn't drop unset fields
      // on PATCH /messages, so omitting this would leave the live link
      // button in the recipient's DM after revoke. Embed copy is
      // exercised by build-delivery-embed.test.js's buildRevokedDMPayload
      // suite (where the mock captures setDescription).
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

      // Only the strict-success recipient gets the DM edit.
      expect(mockEditDM).toHaveBeenCalledTimes(1);
      expect(mockEditDM.mock.calls[0].slice(0, 2)).toEqual(['c-ok', 'm-ok']);
    });

    it('does NOT edit the DM of a mixed-outcome recipient (one of their resources failed to revoke)', async () => {
      // Within one recipient: two resources, one DELETE succeeds + one
      // DELETE fails. Pins that the mixed-outcome recipient lands in
      // failureUserIds (not successUserIds) AND that editDM is NOT
      // called for them — the door isn't fully closed, so the
      // "closed the door" copy would be misleading. Companion to the
      // bucket-level `failure-wins` test above.
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
      // Pins the `if (success > 0)` guard at the top of the post-
      // revoke edit block. When every per-resource DELETE fails (e.g.,
      // the qURL service is fully down), successUserIds is empty and
      // no recipient has had their link revoked. Editing their DM to
      // "closed the door" would be a lie.
      mockDb.getSendItems.mockResolvedValueOnce([
        { resource_id: 'res-a', recipient_discord_id: 'u-a', dm_status: 'sent', dm_channel_id: 'c-a', dm_message_id: 'm-a' },
        { resource_id: 'res-b', recipient_discord_id: 'u-b', dm_status: 'sent', dm_channel_id: 'c-b', dm_message_id: 'm-b' },
      ]);
      mockDeleteLink.mockRejectedValue(new Error('qURL service down'));

      const result = await revokeAllLinks('send-all-fail', 'sender-1', 'apikey', 'Alice');

      expect(result.success).toBe(0);
      expect(mockEditDM).not.toHaveBeenCalled();
      // Also no debug skip-log: the `if (success > 0)` guard short-
      // circuits before we even try to build editTargets.
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
      // Skip the "Edited DMs after revoke" info log entirely when
      // there's nothing to edit — keeps CloudWatch alerts from
      // interpreting attempted=0 as a noteworthy event.
      const editedLog = logger.info.mock.calls.find(c => c[0] === 'Edited DMs after revoke');
      expect(editedLog).toBeUndefined();
      // Debug-level silent-skip log surfaces the SQLite local-dev path
      // (where DM refs aren't persisted) so devs don't chase a phantom
      // bug. Doesn't fire in prod by default.
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
      // Both shapes of edit failure (thrown + soft ok:false return)
      // and both severities (expected / unexpected) must keep the
      // revoke success/total counts honest — those track the DELETE,
      // not the edit, so a recipient-side state change can't poison
      // the operator-facing tally.
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
      // Three recipients — one ok, one operational outcome (recipient
      // deleted DM), one true failure. The split log lets CloudWatch
      // alert on `failed` without false-positiving on user-side state
      // changes (`expectedFailures`).
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
      // Production callers always pass resolveSenderAlias(interaction);
      // pin the defaulted-param defense so a forgotten arg renders
      // "Someone closed the door" instead of "undefined closed the door".
      mockDb.getSendItems.mockResolvedValueOnce([
        { resource_id: 'res-1', recipient_discord_id: 'u-1', dm_status: 'sent', dm_channel_id: 'c-1', dm_message_id: 'm-1' },
      ]);
      mockDeleteLink.mockResolvedValue(undefined);

      await revokeAllLinks('send-no-alias', 'sender-1', 'apikey'); // no senderAlias

      expect(mockEditDM).toHaveBeenCalledTimes(1);
      // The Embed mock chains via setDescription returning `this`, so
      // the rendered description lives on the captured mock — verify
      // the fallback string actually lands in the embed.
      const payload = mockEditDM.mock.calls[0][2];
      expect(payload.embeds).toHaveLength(1);
      // EmbedBuilder mock in this test only chains; assert via the
      // setDescription spy on the captured embed instance.
      const embed = payload.embeds[0];
      const setDescCall = embed.setDescription.mock?.calls?.[0]?.[0];
      expect(setDescCall).toBeTruthy();
      expect(setDescCall).toMatch(/\*\*Someone\*\* closed the door/);
    });

    it('de-dupes per recipient when multiple rows share recipient_discord_id', async () => {
      // SQLite has no UNIQUE constraint on (send_id, recipient_discord_id);
      // a hypothetical multi-resource fan-out to one recipient would yield
      // multiple rows. The DM edit should fire ONCE per recipient.
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
    // Asymmetric coverage protects against a future refactor that
    // flips `&&` to `||` in the persistDispatchResult guard — only
    // BOTH refs present should land on the happy path.
    await persistDispatchResult('s', 'r', result);
    expect(mockDb.markSendDMDelivered).not.toHaveBeenCalled();
    expect(mockDb.updateSendDMStatus).toHaveBeenCalledWith('s', 'r', 'sent');
    expect(logger.audit).toHaveBeenCalledWith('dispatch_sent_no_refs', { send_id: 's' });
    // Diagnostic fields tell the operator which side of the response
    // shape drifted.
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('missing channelId/messageId'),
      expect.objectContaining({ hasChannelId, hasMessageId }),
    );
  });

  it('does NOT throw when markSendDMDelivered fails — emits DISPATCH_PERSIST_FAILED + logs error', async () => {
    // The DM was actually delivered. A bookkeeping failure here must
    // not propagate up as a thrown rejection — the dispatch lambda
    // would otherwise classify the recipient as "could not be reached"
    // even though the DM landed in their inbox. Closes the cr-flagged
    // gap where a wider markSendDMDelivered Update widens the
    // ValidationException surface.
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
    // Cycle-7 ordering moved the audit AFTER the DDB write; cycle-8
    // cr flagged that this masks the discord.js shape-drift canary
    // during a DDB outage. Audit + warn now fire BEFORE the persist
    // (canary preserved), and DISPATCH_PERSIST_FAILED fires alongside
    // when the persist also fails.
    mockDb.updateSendDMStatus.mockRejectedValueOnce(new Error('throttled'));
    await expect(
      persistDispatchResult('s', 'r', { ok: true }),
    ).resolves.toBeUndefined();
    expect(logger.audit).toHaveBeenCalledWith('dispatch_sent_no_refs', { send_id: 's' });
    expect(logger.audit).toHaveBeenCalledWith('dispatch_persist_failed', { send_id: 's' });
  });

  it('does NOT emit DISPATCH_PERSIST_FAILED when the FAILED-status write fails (no delivered DM)', async () => {
    // sendDM said failed; no DM exists. A bookkeeping miss here is a
    // dropped status update, not a real-vs-recorded divergence. Log
    // an error for grep-ability but skip the canary event so it
    // stays a high-signal indicator of "delivered but not recorded."
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

describe('renderSendConfirm — post-send confirmation overflow', () => {
  // Common args.
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

  // Pin the third header segment: self-destruct status. The 7b.3 follow-up
  // replaced the legacy "One-time links" trailer with the timer the user
  // picked (or "off" when no timer is set), so the sender sees the actual
  // viewer behavior right next to the link expiry.
  it('header includes "Self-destruct: <label>" when a timer is set', () => {
    const r = renderSendConfirm({
      ...baseArgs, delivered: 1, successNames: ['alice'], selfDestructSeconds: 300,
    });
    expect(r.content).toContain('| Self-destruct: 5 minutes');
    expect(r.content).not.toContain('One-time');
  });

  it('header renders "Self-destruct: off" when no timer is set', () => {
    // selfDestructSeconds omitted (undefined) — same as a send with no
    // timer picked. The segment is still present for header alignment.
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
    // 200 long names ~= ~6kB inline; well over Discord's 2000-char cap.
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
    // 100 names with markdown chars to force overflow + verify plain rendering.
    const successNames = Array.from({ length: 100 }, () => '*alice*_long_name_to_force_overflow');
    const r = renderSendConfirm({ ...baseArgs, delivered: successNames.length, successNames });
    expect(r.attachmentText).toContain('*alice*_long_name_to_force_overflow');
    expect(r.attachmentText).not.toContain('\\*alice\\*');
    // Inline preview escapes for message rendering.
    expect(r.content).toContain('\\*alice\\*');
  });

  it('zero recipients (delivered=0): no Recipients line, no attachment', () => {
    const r = renderSendConfirm({ ...baseArgs });
    expect(r.content).not.toContain('Recipients:');
    expect(r.attachmentText).toBeNull();
    expect(r.needsExpand).toBe(false);
  });

  // failed-only overflow: 0 success + 200 long failed names. Exercises
  // the attachment branch where the DELIVERED block is absent (no
  // leading `\n\n` separator) and the failed line is the only content
  // driver.
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

  // Pin the boundary at REVOKE_CONTENT_SAFE_MAX = 1900. Off-by-one in
  // the size calc would not be caught by 200-name (way over) or small-
  // list (way under) tests. Use plain alphanumeric names so escape
  // doesn't double underscores and skew the math.
  it('overflow-vs-inline boundary at REVOKE_CONTENT_SAFE_MAX', () => {
    // 20-char alphanumeric, no markdown chars → escaped == raw.
    const make = (n) => Array.from({ length: n }, (_, i) => `aaaaaaaaaaaaa${String(i).padStart(7, '0')}`);
    // Per-name footprint: 20 chars + ", " = 22 chars. Header ~50 +
    // prefix 13 = 63. So budget = (1900 - 63) / 22 ≈ 83 names.
    // Subtract 2 to leave headroom for the trailing comma not present:
    // 80 names = 50 + 13 + (80*22 − 2) = ~1821 — under cap.
    const namesUnder = make(80);
    const under = renderSendConfirm({
      ...baseArgs, delivered: namesUnder.length, successNames: namesUnder, showAll: true,
    });
    expect(under.attachmentText).toBeNull();
    expect(under.content.length).toBeLessThanOrEqual(1900);

    // 95 names: 50 + 13 + (95*22 − 2) = ~2151 — over cap → attachment.
    const namesOver = make(95);
    const over = renderSendConfirm({
      ...baseArgs, delivered: namesOver.length, successNames: namesOver, showAll: true,
    });
    expect(over.attachmentText).not.toBeNull();
    expect(over.content.length).toBeLessThanOrEqual(2000);
  });

  // "(see attached)" pointer must NOT appear on a line that fits
  // inline — even when overflow is driven by the OTHER list. Pinned
  // because Agent 1 flagged the unconditional-pointer bug.
  it('(see attached) only on lines that were truncated', () => {
    // 2 failed names (fits) + 200 success names (overflows).
    const failedNamesPlain = ['fail1', 'fail2'];
    const successNames = Array.from({ length: 200 }, (_, i) => `verylongusername${String(i).padStart(4, '0')}`);
    const r = renderSendConfirm({
      ...baseArgs, delivered: successNames.length,
      successNames, failedNamesPlain, showAll: true,
    });
    // Failed line: full inline, no pointer.
    expect(r.content).toContain('2 could not be reached: fail1, fail2');
    expect(r.content).not.toMatch(/could not be reached:[^\n]*\(see attached\)/);
    // Recipients line: truncated, with pointer.
    expect(r.content).toMatch(/Recipients:.*\(see attached\)/);
    // Attachment still has both lists.
    expect(r.attachmentText).toContain('DELIVERED (200):');
    expect(r.attachmentText).toContain('NOT DELIVERED (2):');
  });
});

describe('renderRevokeMsg', () => {
  it('lists all names + no expand button when count <= TRUNC_LIMIT', () => {
    const r = renderRevokeMsg('send-1', ['alice', 'bob'], 2, false);
    expect(r.content).toContain('Revoked 2/2 users');
    expect(r.content).toContain('Revoked for: alice, bob');
    expect(r.needsExpand).toBe(false);
    expect(r.row).toBeNull();
  });

  it('truncates with "+N more" + adds Show Recipients button when count > TRUNC_LIMIT', () => {
    const names = Array.from({ length: REVOKE_TRUNC_LIMIT + 3 }, (_, i) => `u${i}`);
    const r = renderRevokeMsg('send-2', names, names.length, false);
    expect(r.content).toContain(`+${3} more`);
    expect(r.content).not.toContain(names.at(-1)); // last name truncated off
    expect(r.needsExpand).toBe(true);
    expect(r.row).not.toBeNull();
    // Pin the renamed label so a regression that flips it back to the
    // ambiguous "Show All" (which users misread as a permissions
    // action next to "Revoke All") fails here.
    expect(r.row.components[0].setLabel).toHaveBeenCalledWith('Show Recipients');
  });

  it('shows full list + Hide Recipients button when showAll=true', () => {
    const names = Array.from({ length: REVOKE_TRUNC_LIMIT + 2 }, (_, i) => `u${i}`);
    const r = renderRevokeMsg('send-3', names, names.length, true);
    expect(r.content).toContain(names.at(-1));
    expect(r.content).not.toMatch(/\+\d+ more/);
    expect(r.needsExpand).toBe(true);
    expect(r.row.components[0].setLabel).toHaveBeenCalledWith('Hide Recipients');
  });

  it('omits the names line when no successful revokes (e.g. all already-opened)', () => {
    const r = renderRevokeMsg('send-4', [], 5, false);
    expect(r.content).toContain('Revoked 0/5 users');
    expect(r.content).not.toContain('Revoked for:');
    expect(r.row).toBeNull();
  });

  it('singularizes "user" when total === 1', () => {
    const r = renderRevokeMsg('send-5', ['alice'], 1, false);
    expect(r.content).toContain('Revoked 1/1 user.');
    expect(r.content).not.toContain('1/1 users');
  });

  it('omits the already-opened note when total === 0 (nothing was attempted)', () => {
    const r = renderRevokeMsg('send-empty', [], 0, false);
    expect(r.content).not.toContain('already-opened');
  });

  it('emits attachmentText + suppresses Show Recipients when full list would exceed Discord 2000-char cap', () => {
    // 200 long usernames (~30 chars each) → ~6000 chars uncapped.
    const names = Array.from({ length: 200 }, (_, i) => `verylongusername${String(i).padStart(4, '0')}`);
    const r = renderRevokeMsg('send-cap', names, names.length, /* showAll */ true);
    expect(r.content.length).toBeLessThanOrEqual(2000);
    expect(r.content).toContain('(see attached)');
    expect(r.attachmentText).not.toBeNull();
    // Newline-separated full list — every name present.
    expect(r.attachmentText.split('\n')).toHaveLength(200);
    expect(r.attachmentText).toContain(names[199]);
    // Show Recipients button suppressed — file IS the full list.
    expect(r.needsExpand).toBe(false);
    expect(r.row).toBeNull();
  });

  it('does NOT emit attachmentText when full list fits inline', () => {
    const r = renderRevokeMsg('send-fits', ['alice', 'bob', 'carol'], 3, false);
    expect(r.attachmentText).toBeNull();
  });

  // names with markdown chars must survive plain in the .txt
  // attachment but render escaped in message content.
  it('attachmentText is plain; content escapes markdown per name', () => {
    const names = ['*alice*', 'normal', '[bob](evil)'];
    // Force attachment by repeating to overflow REVOKE_CONTENT_SAFE_MAX.
    const many = Array.from({ length: 200 }, () => '*alice*');
    const r = renderRevokeMsg('send-md', many, many.length, true);
    expect(r.attachmentText).toContain('*alice*');
    expect(r.attachmentText).not.toContain('\\*alice\\*');
    expect(r.content).toContain('\\*alice\\*'); // preview line is escaped

    // Inline (small list) path also escapes.
    const inline = renderRevokeMsg('send-md2', names, names.length, false);
    expect(inline.content).toContain('\\*alice\\*');
    expect(inline.content).toContain('\\[bob\\]\\(evil\\)');
  });

  // Header takes its count from `success` (authoritative DDB count),
  // not `names.length` — guards against `recipients[]` being incomplete
  // when the caller filters by Set membership.
  it('header uses explicit success arg, not names.length', () => {
    // 5 users had links revoked but only 4 names resolvable in
    // recipients[] — header must still say "5/5".
    const r = renderRevokeMsg('send-mismatch', ['alice', 'bob', 'carol', 'dave'], 5, false, 5);
    expect(r.content).toMatch(/^Revoked 5\/5 users\./);
    expect(r.content).toContain('Revoked for: alice, bob, carol, dave');
  });
});

// Slash-command /qurl revoke uses buildRevokeHeader directly (no
// Recipients line); unit-test it so wording stays pinned.
describe('buildRevokeHeader (slash-command revoke path)', () => {
  // eslint-disable-next-line global-require
  const { buildRevokeHeader } = require('../src/revoke-render');

  it('zero-attempt: "Revoked 0/0 users." (no already-opened note)', () => {
    expect(buildRevokeHeader(0, 0)).toBe('Revoked 0/0 users.');
  });

  it('singular: "Revoked 1/1 user." (singular noun + already-opened note)', () => {
    expect(buildRevokeHeader(1, 1)).toBe('Revoked 1/1 user. Note: already-opened links cannot be revoked.');
  });

  it('plural: "Revoked 3/5 users." includes already-opened note when total > 0', () => {
    expect(buildRevokeHeader(3, 5)).toBe('Revoked 3/5 users. Note: already-opened links cannot be revoked.');
  });
});

// ===========================================================================
// handleAddRecipients
// ===========================================================================

function makeUsersCollection(users) {
  // Mirrors discord.js Collection just enough for the function under test:
  // .filter() returns a new collection with .values()
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
      // Sender + bot — both filtered out.
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

  // newRecipients carries {id, username} so callers can render added
  // users by name on a post-Add revoke. Renaming/dropping the field
  // would break that wiring silently.
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

  // #352: a stale or directly-written sendConfig row could carry an
  // off-set `expires_in` value. The pre-flight gate rejects it BEFORE
  // any mint or recordQURLSendBatch work, so we can't strand QURL
  // links upstream or write orphan DDB rows. Symmetric with the
  // failGate inside executeSendPipeline — coverage shapes mirror the
  // executeSendPipeline allowed-set gate test below.
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
    // UX: surface that the ORIGINAL send is intact — only Add
    // Recipients is blocked. Support-ticket-friendly wording.
    expect(result.msg).toMatch(/original send's links still work/i);
    expect(mockDb.recordQURLSendBatch).not.toHaveBeenCalled();
    // Audit signal: pin the structured log so a future operator-
    // facing metric can be wired on the same shape. The motivation
    // for #352 was preserving audit visibility on the orphan-row
    // failure mode; the entry gate's log is the operator-side
    // counterpart to user-visible `result.msg`. `truncForLog` coerces
    // via String(v), so non-string inputs (e.g. number 24) surface
    // as their stringified form.
    expect(logger.warn).toHaveBeenCalledWith(
      'addRecipients refused invalid expires_in',
      expect.objectContaining({ sendId: 'send-1', expiresIn: String(expiresIn) }),
    );
  });
});

describe('handleAddRecipients — file path failure modes', () => {
  it('refuses when stored attachment_url is missing', async () => {
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

  it('surfaces "URL has expired" when re-download throws a 403/expired/network error', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });
    mockDownloadAndUpload.mockRejectedValueOnce(new Error('403 Forbidden'));

    const result = await handleAddRecipients(
      'send-1', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toMatch(/expired/i);
  });

  it('surfaces generic "Failed to prepare links" when re-download throws an unknown error', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });
    mockDownloadAndUpload.mockRejectedValueOnce(new Error('something else broke'));

    const result = await handleAddRecipients(
      'send-1', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toMatch(/failed to prepare links/i);
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
    // The outer catch only fires from the LOCATION path's mintLinksInBatches —
    // the file path has its own inner try/catch that maps re-download errors
    // to "expired" / "Failed to prepare links" rather than "pool exhausted".
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

describe('handleAddRecipients — validate expires_in BEFORE recordQURLSendBatch (#352)', () => {
  // Pins the invariant that a thrown `expiryToMs` aborts the dispatch
  // BEFORE any DDB rows are written. Today's `expiryToMs` falls back
  // to DEFAULT_EXPIRY_MS for malformed input and never throws, so this
  // path is unreachable in practice — but a future regression in
  // time.js (or a future caller that swaps the helper for one that
  // does throw) would otherwise leave orphan DDB rows + audit-blackhole
  // for the batch. The mock forces the throw to exercise the ordering.
  afterEach(() => { mockTime.expiryToMs.mockImplementation(jest.requireActual('../src/utils/time').expiryToMs); });

  it('does not write to DB if expiryToMs throws (no orphan rows, no audit-blackhole)', async () => {
    // Fixture uses a VALID expires_in so the entry-level allowed-set
    // gate (added on top of the hoist) passes — we want the synthetic
    // throw to fire at the hoist site, not be short-circuited by the
    // pre-flight gate above.
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
    });
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-new', fileBuffer: new ArrayBuffer(10) });
    mockMintLinks.mockResolvedValueOnce([{ qurl_link: 'https://q.test/1', resource_id: 'res-new' }]);
    mockTime.expiryToMs.mockImplementationOnce(() => { throw new Error('synthetic expiryToMs failure'); });

    await expect(handleAddRecipients(
      'send-1', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    )).rejects.toThrow(/synthetic expiryToMs failure/);

    // The structural invariant: recordQURLSendBatch must NOT have been
    // reached, so no orphan DDB rows for the QURL links minted above.
    expect(mockDb.recordQURLSendBatch).not.toHaveBeenCalled();
    // sendDM also unreached — defense-in-depth that nothing actually
    // dispatched downstream of the bad expiry.
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
    mockMintLinks.mockResolvedValueOnce([{ qurl_link: 'https://q.test/1', resource_id: 'res-new' }]);
    mockDb.recordQURLSendBatch.mockRejectedValueOnce(new Error('DB unavailable'));

    const result = await handleAddRecipients(
      'send-1', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toMatch(/Failed to save link records/);
    expect(result.delivered).toBe(0);
    // sendDM must NOT have been called — the abort happens before DMs.
    expect(mockSendDM).not.toHaveBeenCalled();
  });
});

describe('handleAddRecipients — happy path (location)', () => {
  it('mints, records, DMs, returns delivered count', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: null, actual_url: 'https://maps.example.com/x',
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
    // Happy-path delivery coalesces status='sent' + DM refs into a
    // single markSendDMDelivered write per recipient.
    expect(mockDb.markSendDMDelivered).toHaveBeenCalledTimes(2);
    // Audit emission: upload_success + dispatch_sent (×2 recipients).
    // mint_* is intentionally not emitted from the bot — see
    // constants.js AUDIT_EVENTS comment.
    const emitted = logger.audit.mock.calls.map(c => c[0]);
    expect(emitted).toEqual(expect.arrayContaining(['upload_success', 'dispatch_sent']));
    expect(logger.audit).toHaveBeenCalledWith('upload_success', expect.objectContaining({
      send_id: 'send-1', kind: 'location',
    }));
    expect(emitted.filter(e => e === 'dispatch_sent')).toHaveLength(2);
    expect(emitted).not.toContain('mint_success');
    expect(emitted).not.toContain('mint_failed');
  });

  // Bulk-path button-packing contract: handleAddRecipients now
  // discards the trust button inside each per-link payload and
  // appends ONE trust button at the bottom of the dispatched
  // message (so multi-link recipients don't see N redundant
  // "What is qURL?" verify buttons). For the N=1 case asserted
  // here, the result must match the executeSendPipeline single-
  // path layout: one ActionRow holding [Step Through, What is qURL?].
  // A future refactor that re-introduces per-link trust buttons,
  // or that breaks the contract that components[0].components[0]
  // is the Step Through button, would surface here.
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
    // One ActionRow holding both buttons — matches the
    // executeSendPipeline layout for the typical 1-link send.
    expect(payload.components).toHaveLength(1);
    const buttons = payload.components[0].components;
    expect(buttons).toHaveLength(2);
    // No assertion on label/url shape here; build-delivery-embed.test.js
    // owns those contracts. This test only pins the packing structure
    // — that the bulk path produces a [Step Through, What is qURL?]
    // row, not a separate trust row, in the 1-link case.
  });

  // The bulk-path shared-embed optimization: at N>1, the EmbedBuilder
  // is built once and the same reference repeated via Array(N).fill.
  // discord.js' .toJSON() is a pure read of internal state, so the
  // optimization is safe — but a future discord.js bump that
  // introduces a mutating serialize hook (or a buildDeliveryEmbed
  // change that turns the embed mutation-aware) would break the
  // pattern silently. This test pins the reference-equality contract.
  it('shares one EmbedBuilder reference across N>1 embeds in the payload', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-file-shared', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
      actual_url: 'https://maps.example.com/x',
      location_name: 'Eiffel Tower',
    });
    mockDownloadAndUpload.mockResolvedValueOnce({
      resource_id: 'res-file-shared-new', fileBuffer: Buffer.from('x'),
    });
    mockMintLinks
      .mockResolvedValueOnce([{ qurl_link: 'https://q.test/share-file', resource_id: 'res-file-shared-new' }])
      .mockResolvedValueOnce([{ qurl_link: 'https://q.test/share-loc', resource_id: 'res-loc-shared-new' }]);
    mockUploadJsonToConnector.mockResolvedValueOnce({ resource_id: 'res-loc-shared-new' });
    mockSendDM.mockResolvedValue({ ok: true, channelId: 'dm-c', messageId: 'dm-m' });

    await handleAddRecipients(
      'send-share', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(mockSendDM).toHaveBeenCalledTimes(1);
    const [, payload] = mockSendDM.mock.calls[0];
    // file + location → 2 links to the one recipient.
    expect(payload.embeds).toHaveLength(2);
    // The optimization: same EmbedBuilder reference, not two copies.
    expect(payload.embeds[0]).toBe(payload.embeds[1]);
    // Belt-and-braces: even if a future discord.js bump introduced a
    // position-aware toJSON (some library serialize hooks consult
    // internal counters), the serialized output must remain identical
    // across the embeds[] entries — otherwise the recipient would see
    // diverged author/footer/description across embeds that share the
    // builder reference.
    if (typeof payload.embeds[0].toJSON === 'function') {
      expect(payload.embeds[0].toJSON()).toEqual(payload.embeds[1].toJSON());
    }
    // Link-ordering contract: the two minted qURLs (file then location,
    // per the mintLinks mock sequencing above) must map positionally
    // onto the Step Through buttons in the assembled payload. A future
    // refactor that shuffles recipientLinks[recipient.id] between
    // population and the packBulkDeliveryComponents call would
    // silently mis-route recipients to the wrong qURL otherwise.
    // discord.js mock here is the lightweight chainable variant —
    // setURL.mock.calls captures the URL each button was built with;
    // pull the URL via the first call's first arg.
    const urls = payload.components
      .flatMap(row => row.components)
      .map(b => b.setURL.mock.calls[0]?.[0])
      .filter(Boolean);
    // The two step-throughs (file then location) precede the trust
    // button; the trust button's URL is the hardcoded brand landing.
    expect(urls).toEqual([
      'https://q.test/share-file',
      'https://q.test/share-loc',
      'https://layerv.ai/qurl/',
    ]);
  });

  // Locks the single-emission contract: a sendConfig with both file
  // AND location must NOT fire upload_success twice (would double-count
  // UploadCount in CloudWatch). The kind field must be 'mixed'.
  it('emits exactly ONE upload_success with kind=mixed when both file + location prep paths run', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-file-orig', expires_in: '30m',
      attachment_url: 'https://cdn.discordapp.com/x.png',
      attachment_name: 'x.png', attachment_content_type: 'image/png',
      // Both paths active.
      actual_url: 'https://maps.example.com/x',
      location_name: 'Eiffel Tower',
    });
    // File path succeeds.
    mockDownloadAndUpload.mockResolvedValueOnce({ resource_id: 'res-file-new', fileBuffer: Buffer.from('x') });
    // mintLinks called twice (once per kind).
    mockMintLinks
      .mockResolvedValueOnce([{ qurl_link: 'https://q.test/file', resource_id: 'res-file-new' }])
      .mockResolvedValueOnce([{ qurl_link: 'https://q.test/loc', resource_id: 'res-loc-new' }]);
    // Location path succeeds.
    mockUploadJsonToConnector.mockResolvedValueOnce({ resource_id: 'res-loc-new' });
    mockSendDM.mockResolvedValue({ ok: true, channelId: 'dm-c', messageId: 'dm-m' });

    await handleAddRecipients(
      'send-mixed', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    const uploadEvents = logger.audit.mock.calls.filter(c => c[0] === 'upload_success');
    expect(uploadEvents).toHaveLength(1);
    expect(uploadEvents[0][1]).toEqual({ send_id: 'send-mixed', kind: 'mixed' });
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
    // First DM fails, second succeeds.
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

// ===========================================================================
// mintLinksInBatches
// ===========================================================================

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
    // First 10 carry res-1, 11th carries res-2.
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
    // Lock in the #292 reorder. Old order in commands.js was
    // clearCooldown → cancelEdit → logger.warn → throw. cancelEdit
    // is `interaction.editReply(...).catch(...)` — an async failure
    // is swallowed by .catch, but a SYNC throw from editReply would
    // bypass the catch and lose the SSRF breadcrumb. Reordering to
    // logger.warn → failGate(...) means the warn lands first even
    // under that hypothetical sync-throw shape. Use invocation-
    // order to pin the warn-before-editReply sequence so a future
    // refactor moving the warn back after cancelEdit fails loudly.
    logger.warn.mockClear();
    const interaction = makeInteraction();
    await expect(executeSendPipeline(interaction, makePipelineParams({
      attachment: { ...DEFAULT_ATTACHMENT, url: 'http://localhost/internal' },
    }))).rejects.toThrow(/SSRF re-validation/);
    expect(logger.warn).toHaveBeenCalledWith(
      'executeSendPipeline: attachment.url failed isAllowedSourceUrl gate',
      expect.objectContaining({ user_id: expect.any(String) }),
    );
    // Anchor `[0]` to a single observable call on each side. The
    // SSRF gate fires before the "Preparing links for…" edit, so
    // logger.warn fires exactly once (the SSRF breadcrumb) and
    // editReply fires exactly once (cancelEdit's underlying call).
    expect(logger.warn).toHaveBeenCalledTimes(1);
    expect(interaction.editReply).toHaveBeenCalledTimes(1);
    const warnOrder = logger.warn.mock.invocationCallOrder[0];
    const editReplyOrder = interaction.editReply.mock.invocationCallOrder[0];
    expect(warnOrder).toBeLessThan(editReplyOrder);
  });

  test('SSRF gate is skipped when resourceType is NOT file (location sends carry no user URL)', async () => {
    // Bogus attachment.url that WOULD fail isAllowedSourceUrl — but
    // resourceType is 'location' so the gate is bypassed. The pipeline
    // will fail later downstream (mocks aren't configured for the
    // location path), but NOT with the SSRF gate message.
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
  // Defensive: any test in this block that pollutes expiryToMs (the
  // #352 hoist test below uses mockImplementationOnce) gets reset.
  // Mirrors the parallel block at the top of handleAddRecipients tests.
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

  // #352: expiresAt is computed BEFORE recordQURLSendBatch so a future
  // `expiryToMs`-throws regression can't leave orphan DDB rows. Today
  // the entry-level failGate above protects, but the hoist makes the
  // ordering invariant explicit. Mock `expiryToMs` to throw — file-
  // prep must succeed so execution actually reaches the hoist site,
  // otherwise the test passes vacuously (mintLinks would throw first
  // with no mock implementation, hitting an upstream code path).
  test('hoists expiresAt above recordQURLSendBatch so a throw can\'t leave orphan rows (#352)', async () => {
    const { expiryToMs } = require('../src/utils/time');
    expiryToMs.mockImplementationOnce(() => { throw new Error('synthetic expiryToMs throw'); });
    mockDb.recordQURLSendBatch.mockClear();

    // File-prep succeeds so execution proceeds past mintLinksInBatches
    // to the new hoist site right above recordQURLSendBatch.
    mockDownloadAndUpload.mockResolvedValueOnce({
      resource_id: 'res-new', fileBuffer: new ArrayBuffer(10),
    });
    mockMintLinks.mockResolvedValueOnce([
      { qurl_link: 'https://q.test/1', resource_id: 'res-new' },
    ]);

    const interaction = makeInteraction();
    await expect(executeSendPipeline(interaction, makePipelineParams({ expiresIn: '30m' })))
      .rejects.toThrow(/synthetic expiryToMs throw/);

    // Load-bearing assertion: even though file-prep succeeded and
    // links were minted upstream, NO DDB rows were written.
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

// Defensive guards for the `recipients` invariants — non-empty and
// ≤ config.QURL_SEND_MAX_RECIPIENTS. The `/qurl send` + `/qurl map`
// front-half already enforces these before the pipeline call; the
// gates are defense-in-depth for a future caller (deserialized
// payload, programmatic retry) that skips those checks. Without
// them, a trip would surface deep inside mintLinksInBatches as
// "Failed to create any links" with no caller-side breadcrumb.
describe('executeSendPipeline — recipients shape + cap gates', () => {
  // Read the cap once from the same config module the gate consults
  // so the tests don't drift if the cap is bumped. Hoisting out of
  // the individual tests centralizes the drift-anchor.
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
    // Cancel-edit fires before the throw — same shape as the other
    // entry gates.
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
    // Rendering both `null` and `{}` as `typeof=object` would
    // force a prod-log reader to guess which one tripped the
    // gate. Pin that the value-detail field disambiguates the
    // realistic miscoding shapes.
    const interaction = makeInteraction();
    await expect(executeSendPipeline(interaction, makePipelineParams({ recipients })))
      .rejects.toThrow(detailRe);
  });

  test('rejection message truncates pathological values with `…` marker', async () => {
    // A future caller handing a 1MB string would otherwise dump
    // the whole blob into the rejection message AND into the
    // prod logger.error. truncForLog slices at 64 chars and
    // appends `…` so a reader can tell "the caller passed
    // exactly these 64 chars" from "we cut a longer value."
    const interaction = makeInteraction();
    const oneKB = 'x'.repeat(1024);
    await expect(executeSendPipeline(interaction, makePipelineParams({ recipients: oneKB })))
      .rejects.toThrow(/value=x{64}…/);
    // Negative pin: a 64-char value should NOT have the marker
    // (otherwise we can't distinguish exact-fit from truncated).
    const exact64 = 'y'.repeat(64);
    await expect(executeSendPipeline(interaction, makePipelineParams({ recipients: exact64 })))
      .rejects.toThrow(/value=y{64}\)/);
  });

  test.each([
    // Hostile-toString: the obvious adversarial shape. Without the
    // try/catch in truncForLog, the throw-message renderer would
    // itself throw, replacing the gate's TypeError with an opaque
    // "Cannot convert object to primitive value" — the exact
    // worse-than-original-error shape the gate exists to prevent.
    ['throws on toString', { toString() { throw new Error('nope'); } }],
    // Object.create(null) is the realistic miscoding shape — a
    // deserialized payload with prototype detached has no
    // @@toPrimitive / toString, so `String(v)` throws "Cannot
    // convert object to primitive value". Pin that the catch
    // branch handles this shape too, not just the explicitly-
    // hostile toString case.
    ['null-prototype object', Object.create(null)],
  ])('rejection message falls back to <unrepresentable> when String() throws (%s)', async (_label, value) => {
    const interaction = makeInteraction();
    await expect(executeSendPipeline(interaction, makePipelineParams({ recipients: value })))
      .rejects.toThrow(/value=<unrepresentable>/);
  });

  test('truncation slices on code points, not UTF-16 code units (astral-char safety)', async () => {
    // `slice(0, 64)` on code units would split a high-surrogate
    // at position 63 from its low-surrogate at 64, producing a
    // malformed UTF-16 pair before the `…`. Iterating via [...s]
    // (the string iterator) operates on code points, so an emoji
    // at the boundary stays intact. Build a string with 64 emoji
    // (each 2 code units) — under code-unit slicing this would
    // surface as 32 intact emoji + a lone high-surrogate; under
    // code-point slicing it surfaces as 64 intact emoji.
    const interaction = makeInteraction();
    const sixtyFourEmoji = '🚀'.repeat(64);
    await expect(executeSendPipeline(interaction, makePipelineParams({ recipients: sixtyFourEmoji })))
      .rejects.toThrow(/value=(?:🚀){64}\)/u);
    // 65 emoji → 64 in the rendering + `…` marker.
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
    // The gate's own clearCooldown call IS the cleanup; the
    // post-throw isOnCooldown assertion is what verifies it.
    expect(isOnCooldown(interaction.user.id)).toBe(false);
  });

  test('clears cooldown on the recipients-oversized path (RangeError branch)', async () => {
    // The empty-array test above pins clearCooldown on the
    // TypeError branch; the oversized-array (RangeError) branch
    // has its own clearCooldown call. Pin it separately so a
    // future refactor that drops one of the two clearCooldown
    // calls is caught by exactly one test failing.
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
    // Boundary case: pin that `length === cap` is accepted (the
    // gate uses `>`, not `>=`). A future typo flipping `>` to
    // `>=` would otherwise only show up if a real send happened
    // to hit exactly the cap.
    ['exactly at the cap', Array.from({ length: RECIPIENT_CAP }, (_, i) => ({ id: `u${i}`, username: `u${i}` }))],
  ])('accepts the allowed shape: %s', async (_label, recipients) => {
    await expectGateAccepts(
      makePipelineParams({ recipients }),
      /recipients must be a non-empty array/,
      /exceeds QURL_SEND_MAX_RECIPIENTS/,
    );
  });
});

// Pin that truncForLog applies to the value-rendering gates in the
// entry-gate family. A future caller handing a 1MB string as
// `expiresIn` (or any value-rendering gate added later) would otherwise
// dump the whole blob into the rejection message.
describe('executeSendPipeline — truncForLog applies to value-rendering gates', () => {
  test('expiresIn rejection message is bounded with `…` on oversized input', async () => {
    const interaction = makeInteraction();
    const huge = 'y'.repeat(1024);
    await expect(executeSendPipeline(interaction, makePipelineParams({ expiresIn: huge })))
      .rejects.toThrow(/expiresIn must be one of .* \(got y{64}…\)/);
  });
});

// Channel-notification on @everyone / voice mode. Each test drives
// executeSendPipeline through the full file-prep + mint + DM happy
// path so the post-send notification site is actually reached; the
// collector setup that runs after the notification throws because
// the editReply mock returns undefined, but the throw is caught
// inside the pipeline and is not load-bearing for these assertions.
//
// The channel post is fire-and-forget — the call to sendChannelMessage
// is synchronous (so `.toHaveBeenCalled()` reads true immediately after
// the pipeline resolves), but the `.then` callback that emits the
// failure warn-log runs on the microtask queue. Tests that assert on
// the warn-log flush microtasks with `await Promise.resolve()` first.
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

  // 'picker' is the gate's explicit no-notify branch; `undefined`
  // exercises the normalizeRecipientMode fallback that maps stale
  // pre-field flow rows + any future off-set drift to picker. Two
  // distinct branches, not two framings of the same one.
  test.each([
    ['picker', 'picker'],
    ['undefined (stale flow row, normalizeRecipientMode fallback)', undefined],
  ])('does NOT post channel notification when recipientMode is %s', async (_label, recipientMode) => {
    const interaction = makeInteraction();
    await executeSendPipeline(interaction, makePipelineParams({ recipientMode }));
    expect(mockSendChannelMessage).not.toHaveBeenCalled();
  });

  test('does NOT post channel notification when delivered === 0 (every DM failed)', async () => {
    // Without this gate, a public "X shared something with everyone"
    // would post even though nobody actually got the DM.
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
    // Flush the fire-and-forget `.then` so the warn-log assertion sees it.
    await Promise.resolve();
    await Promise.resolve();
    expect(logger.warn).toHaveBeenCalledWith(
      'Failed to send channel notification',
      expect.objectContaining({ channelId: 'channel-no-perm', status: 403 }),
    );
  });

  test('sanitizes the sender display name before posting (bidi/RTL spoof defense)', async () => {
    // U+202E (RIGHT-TO-LEFT OVERRIDE) in a public post would flip the
    // announcement RTL for every viewer in the channel.
    const interaction = makeInteraction({
      member: { displayName: 'Alice‮Evil' },
      user: { id: 'sender-1', username: 'Alice' },
    });
    await executeSendPipeline(interaction, makePipelineParams({ recipientMode: 'everyone' }));
    expect(mockSendChannelMessage).toHaveBeenCalledTimes(1);
    const [, message] = mockSendChannelMessage.mock.calls[0];
    expect(message.content).not.toMatch(/‮/);
  });
});

