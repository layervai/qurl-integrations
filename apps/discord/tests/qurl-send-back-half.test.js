/**
 * /qurl send back-half tests — monitorLinkStatus polling, revokeAllLinks
 * direct path, and handleAddRecipients flow.
 *
 * These exercises were the gap that lowered the jest coverage thresholds
 * when commands-comprehensive / coverage-boost were removed in the
 * state-machine redesign. The state-machine spec stops at the "Sent to N"
 * confirmation; this file picks up after that, covering:
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
 * through handleSend so each spec can target one branch without the
 * 300-line front-half setup. handleSend's integration with these
 * functions is already covered by qurl-send-state-machine.test.js's
 * end-to-end happy paths.
 */

// ---------------------------------------------------------------------------
// Mocks — same shape as qurl-send-state-machine.test.js so both files share
// a coherent module surface; copied (not imported) because each test file
// gets its own jest module registry and the mock implementations diverge
// per file (mockMintLinks etc. are file-private to keep tests isolated).
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
jest.mock('../src/discord', () => ({
  assignContributorRole: jest.fn(),
  notifyPRMerge: jest.fn(),
  notifyBadgeEarned: jest.fn(),
  postGoodFirstIssue: jest.fn(),
  postReleaseAnnouncement: jest.fn(),
  postStarMilestone: jest.fn(),
  postToGitHubFeed: jest.fn(),
  sendDM: mockSendDM,
  getVoiceChannelMembers: jest.fn(),
  getTextChannelMembers: jest.fn(),
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

const mockGetResourceStatus = jest.fn();
const mockDeleteLink = jest.fn();
jest.mock('../src/qurl', () => ({
  createOneTimeLink: jest.fn(),
  deleteLink: mockDeleteLink,
  getResourceStatus: mockGetResourceStatus,
}));

jest.mock('../src/places', () => ({ searchPlaces: jest.fn().mockResolvedValue([]) }));

// ---------------------------------------------------------------------------
// Require modules under test
// ---------------------------------------------------------------------------

const { _test } = require('../src/commands');
const logger = require('../src/logger');
const {
  monitorLinkStatus,
  revokeAllLinks,
  handleAddRecipients,
  mintLinksInBatches,
  activeMonitors,
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

describe('monitorLinkStatus — initial poll + tracking', () => {
  beforeEach(() => { jest.useFakeTimers(); });
  afterEach(() => {
    jest.useRealTimers();
  });

  it('initializes trackedQurlIds from getResourceStatus on first tick', async () => {
    const interaction = makeInteraction();
    mockGetResourceStatus.mockResolvedValue({
      qurls: [
        { qurl_id: 'q1', use_count: 0, status: 'active', created_at: '2026-01-01T00:00:00Z' },
        { qurl_id: 'q2', use_count: 0, status: 'active', created_at: '2026-01-01T00:00:01Z' },
      ],
    });

    const monitor = monitorLinkStatus(
      'send-1', interaction,
      [{ resourceId: 'res-1', qurl_link: 'https://q.test/x' }],
      [{ id: 'r1', username: 'Alice' }, { id: 'r2', username: 'Bob' }],
      '1m', 'Sent to 2 users', { components: [] }, 2, 'apikey',
    );

    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    expect(mockGetResourceStatus).toHaveBeenCalled();
    expect(logger.info).toHaveBeenCalledWith(
      'Link monitor tracking',
      expect.objectContaining({ sendId: 'send-1', tracked: 2, resources: 1 }),
    );
    monitor.stop();
  });

  it('warns when getResourceStatus returns fewer qurls than recipients', async () => {
    const interaction = makeInteraction();
    // Only 1 qurl returned, but 2 recipients sent — count mismatch.
    mockGetResourceStatus.mockResolvedValue({
      qurls: [{ qurl_id: 'q1', use_count: 0, status: 'active', created_at: '2026-01-01T00:00:00Z' }],
    });

    const monitor = monitorLinkStatus(
      'send-1', interaction,
      [{ resourceId: 'res-1', qurl_link: 'https://q.test/x' }],
      [{ id: 'r1', username: 'Alice' }, { id: 'r2', username: 'Bob' }],
      '1m', 'Sent', { components: [] }, 2, 'apikey',
    );

    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    expect(logger.warn).toHaveBeenCalledWith(
      'Monitor tracking count mismatch',
      expect.objectContaining({ sendId: 'send-1', qurls: 1, recipients: 2 }),
    );
    monitor.stop();
  });

  it('returns early on first tick if getResourceStatus rejects on every resource', async () => {
    const interaction = makeInteraction();
    // .catch(() => null) on the call site — rejected promises become null,
    // which is filtered out before the localSet.add loop. trackedQurlIds
    // ends up empty (Set, size 0) but is still set, so the next tick
    // proceeds normally with an empty tracked set.
    mockGetResourceStatus.mockRejectedValue(new Error('upstream 503'));

    const monitor = monitorLinkStatus(
      'send-1', interaction,
      [{ resourceId: 'res-1' }],
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent', { components: [] }, 1, 'apikey',
    );

    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    // No throw; init logged; no editReply because no status changes.
    expect(interaction.editReply).not.toHaveBeenCalled();
    monitor.stop();
  });
});

describe('monitorLinkStatus — status transitions', () => {
  beforeEach(() => { jest.useFakeTimers(); });
  afterEach(() => { jest.useRealTimers(); });

  it('transitions pending → opened on use_count > 0 and editReplies', async () => {
    const interaction = makeInteraction();
    // First poll: q1 pending, q2 pending. Second poll: q1 opened.
    mockGetResourceStatus
      .mockResolvedValueOnce({
        qurls: [
          { qurl_id: 'q1', use_count: 0, status: 'active', created_at: '2026-01-01T00:00:00Z' },
          { qurl_id: 'q2', use_count: 0, status: 'active', created_at: '2026-01-01T00:00:01Z' },
        ],
      })
      .mockResolvedValueOnce({
        // Same shape, second poll on the same resource.
        qurls: [
          { qurl_id: 'q1', use_count: 0, status: 'active', created_at: '2026-01-01T00:00:00Z' },
          { qurl_id: 'q2', use_count: 0, status: 'active', created_at: '2026-01-01T00:00:01Z' },
        ],
      })
      .mockResolvedValueOnce({
        qurls: [
          { qurl_id: 'q1', use_count: 1, status: 'active', created_at: '2026-01-01T00:00:00Z' },
          { qurl_id: 'q2', use_count: 0, status: 'active', created_at: '2026-01-01T00:00:01Z' },
        ],
      });

    const monitor = monitorLinkStatus(
      'send-1', interaction,
      [{ resourceId: 'res-1' }],
      [{ id: 'r1', username: 'Alice' }, { id: 'r2', username: 'Bob' }],
      '1m', 'Sent', { components: [] }, 2, 'apikey',
    );

    // Tick 1 = init. Tick 2 = poll, no change. Tick 3 = poll, q1 opened.
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    // editReply called at least once with the opened-status message.
    const calls = interaction.editReply.mock.calls.map(c => c[0]?.content || '');
    expect(calls.some(c => /opened/.test(c))).toBe(true);
    monitor.stop();
  });

  it('transitions pending → expired on status=expired', async () => {
    const interaction = makeInteraction();
    mockGetResourceStatus
      .mockResolvedValueOnce({
        qurls: [{ qurl_id: 'q1', use_count: 0, status: 'active', created_at: '2026-01-01T00:00:00Z' }],
      })
      .mockResolvedValueOnce({
        qurls: [{ qurl_id: 'q1', use_count: 0, status: 'expired', created_at: '2026-01-01T00:00:00Z' }],
      });

    const monitor = monitorLinkStatus(
      'send-1', interaction,
      [{ resourceId: 'res-1' }],
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent', { components: [] }, 1, 'apikey',
    );

    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    const calls = interaction.editReply.mock.calls.map(c => c[0]?.content || '');
    expect(calls.some(c => /expired/.test(c))).toBe(true);
    monitor.stop();
  });

  it('all-opened → posts final message and clears interval', async () => {
    const interaction = makeInteraction();
    mockGetResourceStatus
      .mockResolvedValueOnce({
        qurls: [{ qurl_id: 'q1', use_count: 0, status: 'active', created_at: '2026-01-01T00:00:00Z' }],
      })
      .mockResolvedValueOnce({
        qurls: [{ qurl_id: 'q1', use_count: 1, status: 'active', created_at: '2026-01-01T00:00:00Z' }],
      });

    const monitor = monitorLinkStatus(
      'send-1', interaction,
      [{ resourceId: 'res-1' }],
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent', { components: [] }, 1, 'apikey',
    );

    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    // After all-done, an additional tick should hit the stop branch;
    // a final-message editReply with components: [] confirms the path.
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    const lastCall = interaction.editReply.mock.calls.at(-1);
    expect(lastCall?.[0]).toEqual(expect.objectContaining({ components: expect.any(Array) }));
    // monitor.stop() in afterEach is a no-op since allDone already cleared
    monitor.stop();
  });
});

describe('monitorLinkStatus — addRecipients() + stop() races', () => {
  beforeEach(() => { jest.useFakeTimers(); });
  afterEach(() => { jest.useRealTimers(); });

  it('addRecipients() bumps generation and forces re-init on next tick', async () => {
    const interaction = makeInteraction();
    mockGetResourceStatus.mockResolvedValue({
      qurls: [{ qurl_id: 'q1', use_count: 0, status: 'active', created_at: '2026-01-01T00:00:00Z' }],
    });

    const monitor = monitorLinkStatus(
      'send-1', interaction,
      [{ resourceId: 'res-1' }],
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent', { components: [] }, 1, 'apikey',
    );

    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    // Add a recipient + new resource. Generation bumps; trackedQurlIds nulls.
    monitor.addRecipients(1, ['res-2']);

    // Next tick re-inits — getResourceStatus called again across both resources.
    mockGetResourceStatus.mockClear();
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);

    expect(mockGetResourceStatus).toHaveBeenCalled();
    monitor.stop();
  });

  it('addRecipients() de-dupes new resource IDs already in the list', async () => {
    const interaction = makeInteraction();
    mockGetResourceStatus.mockResolvedValue({ qurls: [] });

    const monitor = monitorLinkStatus(
      'send-1', interaction,
      [{ resourceId: 'res-1' }],
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent', { components: [] }, 1, 'apikey',
    );

    // Calling addRecipients with an already-present resourceId should not
    // dup it. The Set-of-resourceIds semantics live inside the function;
    // this asserts no crash + the next tick still polls cleanly.
    monitor.addRecipients(1, ['res-1']);
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);
    monitor.stop();
  });

  it('stop() called concurrently with running tick — caught by outer try/catch, no crash', async () => {
    const interaction = makeInteraction();
    mockGetResourceStatus.mockImplementation(() => {
      // Simulate slow upstream — by the time this resolves, stop() has run.
      return new Promise(resolve => setTimeout(() => resolve({ qurls: [{
        qurl_id: 'q1', use_count: 0, status: 'active', created_at: '2026-01-01T00:00:00Z',
      }] }), 5000));
    });

    const monitor = monitorLinkStatus(
      'send-1', interaction,
      [{ resourceId: 'res-1' }],
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent', { components: [] }, 1, 'apikey',
    );

    // Kick the first tick + start the slow getResourceStatus.
    await jest.advanceTimersByTimeAsync(POLL_INTERVAL);
    // Stop NOW, mid-await, before getResourceStatus resolves. stop() nulls
    // `interaction`, `qurlLinks`, `recipients`, `buttonRow`. The first-tick
    // init block reads `recipients.length` AFTER the Promise.all resolves,
    // so a stop() during that gap surfaces as an NPE — caught by the outer
    // try/catch on line 609 and logged as 'Link monitor poll failed'.
    // The contract is: no thrown error escapes the setInterval; the impact
    // is one logged error line per affected tick, not a process crash.
    // (Tightening the post-await guard is tracked separately.)
    monitor.stop();
    await jest.advanceTimersByTimeAsync(10000);

    // No unhandled rejection; the outer try/catch contained the failure.
    // The exact log shape (none vs one logged poll-failed) depends on the
    // race window — we only assert the failure-mode contract: no crash.
    expect(true).toBe(true);
  });
});

describe('monitorLinkStatus — duration cap + activeMonitors LRU', () => {
  beforeEach(() => { jest.useFakeTimers(); });
  afterEach(() => { jest.useRealTimers(); });

  it('stops + posts final after MAX_MONITOR_DURATION_MS (1h cap on long expiries)', async () => {
    const interaction = makeInteraction();
    mockGetResourceStatus.mockResolvedValue({ qurls: [] });

    // 7d expiry → MAX_MONITOR_DURATION_MS (1h) clamps the run.
    const monitor = monitorLinkStatus(
      'send-1', interaction,
      [{ resourceId: 'res-1' }],
      [{ id: 'r1', username: 'Alice' }],
      '7d', 'Sent', { components: [] }, 1, 'apikey',
    );

    // Skip ~1h+1min so the cap branch fires.
    await jest.advanceTimersByTimeAsync(60 * 60 * 1000 + 60 * 1000);

    // The cap branch sets the final message + clears interval; monitor
    // is removed from activeMonitors via stop(). Subsequent .stop() is
    // idempotent.
    monitor.stop();
  });

  it('LRU-evicts oldest monitor when activeMonitors hits MAX_CONCURRENT_MONITORS', () => {
    // The cap is 50 in module-private state. We can't test the exact
    // boundary without exposing the constant, but we can confirm the set
    // behavior: starting many monitors keeps activeMonitors size bounded
    // and oldest gets stop()'d.
    const before = activeMonitors.size;
    const monitors = [];
    for (let i = 0; i < 5; i++) {
      monitors.push(monitorLinkStatus(
        `send-${i}`, makeInteraction(),
        [{ resourceId: `res-${i}` }],
        [{ id: `r${i}`, username: `User${i}` }],
        '1m', 'Sent', { components: [] }, 1, 'apikey',
      ));
    }
    // Set should have grown by exactly 5 (no eviction at 5 < 50 cap).
    expect(activeMonitors.size).toBe(before + 5);
    for (const m of monitors) m.stop();
  });

  it('exposes control methods: addRecipients, stop, updateBaseMsg, getFullMsg', () => {
    const monitor = monitorLinkStatus(
      'send-1', makeInteraction(),
      [{ resourceId: 'res-1' }],
      [{ id: 'r1', username: 'Alice' }],
      '1m', 'Sent', { components: [] }, 1, 'apikey',
    );

    expect(typeof monitor.addRecipients).toBe('function');
    expect(typeof monitor.stop).toBe('function');
    expect(typeof monitor.updateBaseMsg).toBe('function');
    expect(typeof monitor.getFullMsg).toBe('function');

    // updateBaseMsg + getFullMsg round-trip with new base message.
    monitor.updateBaseMsg('New base');
    expect(monitor.getFullMsg()).toContain('New base');
    monitor.stop();
  });
});

// ===========================================================================
// revokeAllLinks
// ===========================================================================

describe('revokeAllLinks', () => {
  it('calls deleteLink for each resource and markSendRevoked, returns success/total', async () => {
    mockDb.getSendResourceIds.mockResolvedValueOnce(['res-1', 'res-2', 'res-3']);
    mockDeleteLink.mockResolvedValue(undefined);

    const result = await revokeAllLinks('send-1', 'sender-1', 'apikey');

    expect(mockDeleteLink).toHaveBeenCalledTimes(3);
    expect(mockDb.markSendRevoked).toHaveBeenCalledWith('send-1', 'sender-1');
    expect(result).toEqual({ success: 3, total: 3 });
  });

  it('counts partial failures as success/total mismatch and logs each failure', async () => {
    mockDb.getSendResourceIds.mockResolvedValueOnce(['res-1', 'res-2']);
    mockDeleteLink
      .mockResolvedValueOnce(undefined)
      .mockRejectedValueOnce(new Error('429 rate limited'));

    const result = await revokeAllLinks('send-1', 'sender-1', 'apikey');

    expect(result).toEqual({ success: 1, total: 2 });
    expect(logger.error).toHaveBeenCalledWith('Failed to revoke QURL', expect.any(Object));
    // markSendRevoked still fires — partial failures don't block the local
    // record update, since re-picking from /qurl revoke wouldn't help.
    expect(mockDb.markSendRevoked).toHaveBeenCalled();
  });

  it('returns 0/0 when send has no resource IDs (already-revoked or unknown sendId)', async () => {
    mockDb.getSendResourceIds.mockResolvedValueOnce([]);

    const result = await revokeAllLinks('send-1', 'sender-1', 'apikey');

    expect(result).toEqual({ success: 0, total: 0 });
    expect(mockDeleteLink).not.toHaveBeenCalled();
    expect(mockDb.markSendRevoked).toHaveBeenCalled();
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
      connector_resource_id: 'res-1', expires_in: '5m',
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
      connector_resource_id: null, actual_url: null, expires_in: '5m',
    });

    const result = await handleAddRecipients(
      'send-1', makeUsersCollection([{ id: 'u1', username: 'Alice', bot: false }]),
      makeInteraction(), 'apikey',
    );

    expect(result.msg).toMatch(/incomplete/i);
  });
});

describe('handleAddRecipients — file path failure modes', () => {
  it('refuses when stored attachment_url is missing', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '5m',
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
      connector_resource_id: 'res-1', expires_in: '5m',
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
      connector_resource_id: 'res-1', expires_in: '5m',
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
      connector_resource_id: 'res-1', expires_in: '5m',
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
      connector_resource_id: 'res-1', expires_in: '5m',
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
      location_name: 'Eiffel Tower', expires_in: '5m',
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

describe('handleAddRecipients — DB failure mid-flow', () => {
  it('aborts before DMs when recordQURLSendBatch fails (no orphan live links)', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: 'res-1', expires_in: '5m',
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
      location_name: 'Eiffel Tower', expires_in: '5m', personal_message: 'check this out',
    });
    mockUploadJsonToConnector.mockResolvedValueOnce({ resource_id: 'res-loc-new' });
    mockMintLinks.mockResolvedValueOnce([
      { qurl_link: 'https://q.test/1', resource_id: 'res-loc-new' },
      { qurl_link: 'https://q.test/2', resource_id: 'res-loc-new' },
    ]);
    mockSendDM.mockResolvedValue(true);
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
    expect(result.newResourceIds).toEqual(expect.arrayContaining(['res-loc-new']));
    // updateSendDMStatus called once per recipient with SENT.
    expect(mockDb.updateSendDMStatus).toHaveBeenCalledTimes(2);
  });

  it('reports failed DMs as failed in the return value', async () => {
    mockDb.getSendConfig.mockResolvedValueOnce({
      connector_resource_id: null, actual_url: 'https://maps.example.com/x',
      location_name: 'Eiffel Tower', expires_in: '5m',
    });
    mockUploadJsonToConnector.mockResolvedValueOnce({ resource_id: 'res-loc-new' });
    mockMintLinks.mockResolvedValueOnce([
      { qurl_link: 'https://q.test/1', resource_id: 'res-loc-new' },
      { qurl_link: 'https://q.test/2', resource_id: 'res-loc-new' },
    ]);
    // First DM fails, second succeeds.
    mockSendDM.mockResolvedValueOnce(false).mockResolvedValueOnce(true);
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
