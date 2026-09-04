// Tests for /qurl status service verification and the admin-offboarding nudge
// (#185).
//
// The status reply for a configured guild surfaces a passive notice
// when the admin who originally ran setup has left the server —
// remaining ManageGuild admins need to know that qURL usage is still
// billing to the absent admin's layerv.ai account, and that running
// /qurl setup again will re-bind the key to themselves.
//
// Best-effort detection: a Discord API blip during the
// `members.fetch` call is treated as "skip the nudge", not "fail
// the status read." The notice only fires on the specific
// "Unknown Member" error code (10007) so transient errors don't
// mis-flag a present admin as gone.

// OAUTH_STATE_SECRET is pinned globally in tests/setup-env.js.
process.env.KEY_ENCRYPTION_KEY = '1'.repeat(64);
process.env.GUILD_ID = '123456789012345678';

jest.mock('../src/discord', () => ({
  sendDM: jest.fn().mockResolvedValue(true),
  assignContributorRole: jest.fn(),
  notifyPRMerge: jest.fn(),
  notifyBadgeEarned: jest.fn(),
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

jest.mock('../src/store', () => ({
  setGuildApiKey: jest.fn().mockResolvedValue(undefined),
  getGuildApiKey: jest.fn(),
  getGuildConfig: jest.fn(),
  getPendingLink: jest.fn(),
  consumePendingLink: jest.fn(),
}));

const mockGetIdentity = jest.fn();
jest.mock('../src/qurl', () => ({
  deleteLink: jest.fn(),
  getIdentity: mockGetIdentity,
  isPrivateHost: jest.fn(),
}));

const db = require('../src/store');
const logger = require('../src/logger');
const { handleCommand } = require('../src/commands');
const { PermissionFlagsBits } = require('discord.js');

const STORED_KEY = 'lv_live_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';

// Minimal interaction stub for the /qurl status path. The real
// discord.js Interaction has dozens of fields we don't need; only
// the surface the status handler actually reads is mocked.
function makeStatusInteraction({ memberFetchBehavior, isAdmin = true }) {
  const reply = jest.fn();
  const editReply = jest.fn();
  const followUp = jest.fn();
  const interaction = {
    reply: reply.mockImplementation(() => Promise.resolve()),
    editReply: editReply.mockImplementation(() => Promise.resolve()),
    followUp: followUp.mockImplementation(() => Promise.resolve()),
    deferred: false,
    isAutocomplete: () => false,
    isChatInputCommand: () => true,
    commandName: 'qurl',
    options: { getSubcommand: () => 'status' },
    guildId: 'guild-1',
    memberPermissions: { has: (p) => isAdmin && p === PermissionFlagsBits.ManageGuild },
    guild: {
      members: { fetch: jest.fn().mockImplementation(memberFetchBehavior) },
    },
    user: { id: 'admin-current' },
    _reply: reply,
    _editReply: editReply,
    _followUp: followUp,
  };
  interaction.deferReply = jest.fn().mockImplementation(async () => {
    interaction.deferred = true;
  });
  return interaction;
}

beforeEach(() => {
  jest.clearAllMocks();
  db.getGuildApiKey.mockResolvedValue(STORED_KEY);
  mockGetIdentity.mockResolvedValue({
    api_key: {
      key_id: 'key-123',
      key_prefix: 'lv_live_aaa',
      scopes: ['qurl:read', 'qurl:write'],
    },
  });
});

describe('/qurl status — admin-offboarding nudge (#185)', () => {
  it('rejects non-admins before deferring or checking the stored key', async () => {
    const interaction = makeStatusInteraction({
      memberFetchBehavior: async () => ({ id: 'admin-original' }),
      isAdmin: false,
    });

    await handleCommand(interaction);

    expect(interaction._reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/only server administrators/i),
      ephemeral: true,
    }));
    expect(interaction.deferReply).not.toHaveBeenCalled();
    expect(db.getGuildConfig).not.toHaveBeenCalled();
    expect(mockGetIdentity).not.toHaveBeenCalled();
  });

  it('resolves the deferred response when the configuration read fails', async () => {
    db.getGuildConfig.mockRejectedValueOnce(new Error('DDB unavailable'));
    const interaction = makeStatusInteraction({
      memberFetchBehavior: async () => ({ id: 'admin-original' }),
    });

    await handleCommand(interaction);

    expect(interaction.deferReply).toHaveBeenCalledWith({ ephemeral: true });
    expect(interaction._reply).not.toHaveBeenCalled();
    expect(interaction._editReply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringMatching(/status check could not be completed/i),
    }));
    expect(interaction._followUp).not.toHaveBeenCalled();
    expect(mockGetIdentity).not.toHaveBeenCalled();
  });

  it('does NOT show the nudge when the original admin is still in the guild', async () => {
    db.getGuildConfig.mockResolvedValueOnce({
      guild_id: 'guild-1',
      configured_by: 'admin-original',
      updated_at: '2026-01-01T00:00:00Z',
    });
    const interaction = makeStatusInteraction({
      memberFetchBehavior: async () => ({ id: 'admin-original' }), // present
    });
    await handleCommand(interaction);
    expect(interaction.deferReply).toHaveBeenCalledWith({ ephemeral: true });
    expect(interaction.deferred).toBe(true);
    expect(interaction._reply).not.toHaveBeenCalled();
    expect(interaction._editReply).toHaveBeenCalledTimes(1);
    const replyContent = interaction._editReply.mock.calls[0][0].content;
    expect(replyContent).toContain('qURL is configured');
    expect(replyContent).toContain('Key prefix: `lv_live_aaa`');
    expect(replyContent).toContain('Scopes: `qurl:read`, `qurl:write`');
    expect(replyContent).not.toContain(STORED_KEY);
    expect(mockGetIdentity).toHaveBeenCalledTimes(1);
    expect(mockGetIdentity).toHaveBeenCalledWith(STORED_KEY, 'guild-1');
    expect(interaction._editReply).toHaveBeenCalledWith(expect.objectContaining({
      allowedMentions: { parse: [] },
    }));
    expect(replyContent).not.toContain('has left this server');
  });

  it('uses the deferred error path when the status edit fails', async () => {
    db.getGuildConfig.mockResolvedValueOnce({
      guild_id: 'guild-1',
      configured_by: 'admin-original',
      updated_at: '2026-01-01T00:00:00Z',
    });
    const interaction = makeStatusInteraction({
      memberFetchBehavior: async () => ({ id: 'admin-original' }),
    });
    interaction._editReply.mockRejectedValueOnce(new Error('Unknown Webhook'));

    await handleCommand(interaction);

    expect(interaction._followUp).toHaveBeenCalledWith(expect.objectContaining({
      content: 'There was an error executing this command.',
      ephemeral: true,
    }));
  });

  it.each([401, 403])('reports a %i response as a revoked or invalid key', async (status) => {
    db.getGuildConfig.mockResolvedValueOnce({
      guild_id: 'guild-1',
      configured_by: 'admin-original',
      updated_at: '2026-01-01T00:00:00Z',
    });
    mockGetIdentity.mockRejectedValueOnce(Object.assign(new Error('redacted'), { status }));
    const interaction = makeStatusInteraction({
      memberFetchBehavior: async () => ({ id: 'admin-original' }),
    });

    await handleCommand(interaction);

    const replyContent = interaction._editReply.mock.calls[0][0].content;
    expect(replyContent).toMatch(/revoked or invalid/i);
    expect(replyContent).toContain('`/qurl setup`');
    expect(replyContent).not.toContain(STORED_KEY);
    expect(replyContent).toContain('Configured by: <@admin-original>');
    expect(replyContent).toContain('Last updated: 2026-01-01T00:00:00Z');
  });

  it('keeps the admin-left notice when the stored key is revoked', async () => {
    db.getGuildConfig.mockResolvedValueOnce({
      guild_id: 'guild-1',
      configured_by: 'admin-departed',
      updated_at: '2026-01-01T00:00:00Z',
    });
    mockGetIdentity.mockRejectedValueOnce(Object.assign(new Error('redacted'), { status: 401 }));
    const interaction = makeStatusInteraction({
      memberFetchBehavior: async () => {
        throw Object.assign(new Error('Unknown Member'), { code: 10007 });
      },
    });

    await handleCommand(interaction);

    const replyContent = interaction._editReply.mock.calls[0][0].content;
    expect(replyContent).toMatch(/revoked or invalid/i);
    expect(replyContent).toContain('has left this server');
    expect(replyContent).toContain('<@admin-departed>');
  });

  it.each([
    ['transport error', new Error('connect ECONNREFUSED 10.0.0.5:8080'), null],
    ['timeout', new DOMException('operation timed out', 'TimeoutError'), null],
    ['service error', Object.assign(new Error('redacted'), { status: 503 }), 503],
  ])('does not report a %s as revoked', async (_case, error, expectedStatus) => {
    db.getGuildConfig.mockResolvedValueOnce({
      guild_id: 'guild-1',
      configured_by: 'admin-original',
      updated_at: '2026-01-01T00:00:00Z',
    });
    mockGetIdentity.mockRejectedValueOnce(error);
    const interaction = makeStatusInteraction({
      memberFetchBehavior: async () => ({ id: 'admin-original' }),
    });

    await handleCommand(interaction);

    const replyContent = interaction._editReply.mock.calls[0][0].content;
    expect(replyContent).toMatch(/check could not be completed/i);
    expect(replyContent).toMatch(/stored qURL configuration found/i);
    expect(replyContent).not.toMatch(/revoked|invalid/i);
    expect(replyContent).not.toContain(STORED_KEY);
    expect(replyContent).toContain('again later.\n\nConfigured by:');
    expect(replyContent).toContain('Configured by: <@admin-original>');
    expect(replyContent).toContain('Last updated: 2026-01-01T00:00:00Z');
    expect(logger.warn).toHaveBeenCalledWith('qURL status identity check failed', {
      guild_id: 'guild-1',
      status: expectedStatus,
      failure_stage: 'qurl_service',
    });
    expect(JSON.stringify(logger.warn.mock.calls)).not.toContain(STORED_KEY);
  });

  it('keeps the admin-left notice when the identity check cannot be completed', async () => {
    db.getGuildConfig.mockResolvedValueOnce({
      guild_id: 'guild-1',
      configured_by: 'admin-departed',
      updated_at: '2026-01-01T00:00:00Z',
    });
    mockGetIdentity.mockRejectedValueOnce(new Error('network unavailable'));
    const interaction = makeStatusInteraction({
      memberFetchBehavior: async () => {
        throw Object.assign(new Error('Unknown Member'), { code: 10007 });
      },
    });

    await handleCommand(interaction);

    const replyContent = interaction._editReply.mock.calls[0][0].content;
    expect(replyContent).toMatch(/check could not be completed/i);
    expect(replyContent).toContain('has left this server');
    expect(replyContent).toContain('<@admin-departed>');
  });

  it('distinguishes a stored-key read failure without logging the error text', async () => {
    db.getGuildConfig.mockResolvedValueOnce({
      guild_id: 'guild-1',
      configured_by: 'admin-original',
      updated_at: '2026-01-01T00:00:00Z',
    });
    db.getGuildApiKey.mockRejectedValueOnce(new Error(`KMS failure for ${STORED_KEY}`));
    const interaction = makeStatusInteraction({
      memberFetchBehavior: async () => ({ id: 'admin-original' }),
    });

    await handleCommand(interaction);

    expect(interaction._editReply.mock.calls[0][0].content)
      .toMatch(/check could not be completed/i);
    expect(logger.warn).toHaveBeenCalledWith('qURL status identity check failed', {
      guild_id: 'guild-1',
      status: null,
      failure_stage: 'key_store',
    });
    expect(JSON.stringify(logger.warn.mock.calls)).not.toContain(STORED_KEY);
  });

  it('renders an empty service-reported scope list explicitly', async () => {
    db.getGuildConfig.mockResolvedValueOnce({
      guild_id: 'guild-1',
      configured_by: 'admin-original',
      updated_at: '2026-01-01T00:00:00Z',
    });
    mockGetIdentity.mockResolvedValueOnce({
      api_key: { key_id: 'key-123', key_prefix: 'lv_live_aaa', scopes: [] },
    });
    const interaction = makeStatusInteraction({
      memberFetchBehavior: async () => ({ id: 'admin-original' }),
    });

    await handleCommand(interaction);

    expect(interaction._editReply.mock.calls[0][0].content).toContain('Scopes: _none_');
  });

  it('renders normalized empty identity fields with explicit fallbacks', async () => {
    db.getGuildConfig.mockResolvedValueOnce({
      guild_id: 'guild-1',
      configured_by: null,
      updated_at: '2026-01-01T00:00:00Z',
    });
    mockGetIdentity.mockResolvedValueOnce({
      api_key: { key_id: 'key-123', key_prefix: '', scopes: [''] },
    });
    const interaction = makeStatusInteraction({
      memberFetchBehavior: async () => ({ id: 'admin-original' }),
    });

    await handleCommand(interaction);

    const replyContent = interaction._editReply.mock.calls[0][0].content;
    expect(replyContent).toContain('Key prefix: `unknown`');
    expect(replyContent).toContain('Scopes: `unnamed`');
    expect(replyContent).toContain('Configured by: unknown');
    expect(replyContent).not.toContain('<@unknown>');
  });

  it('summarizes service-reported scopes beyond the display limit', async () => {
    db.getGuildConfig.mockResolvedValueOnce({
      guild_id: 'guild-1',
      configured_by: 'admin-original',
      updated_at: '2026-01-01T00:00:00Z',
    });
    mockGetIdentity.mockResolvedValueOnce({
      api_key: {
        key_id: 'key-123',
        key_prefix: 'lv_live_aaa',
        scopes: Array.from({ length: 12 }, (_, i) => `qurl:scope-${i}`),
      },
    });
    const interaction = makeStatusInteraction({
      memberFetchBehavior: async () => ({ id: 'admin-original' }),
    });

    await handleCommand(interaction);

    const replyContent = interaction._editReply.mock.calls[0][0].content;
    expect(replyContent).toContain('_+2 more_');
    expect(replyContent).not.toContain('`qurl:scope-10`');
  });

  it('sanitizes service-reported prefix and scopes before rendering them', async () => {
    db.getGuildConfig.mockResolvedValueOnce({
      guild_id: 'guild-1',
      configured_by: 'admin-original',
      updated_at: '2026-01-01T00:00:00Z',
    });
    mockGetIdentity.mockResolvedValueOnce({
      api_key: {
        key_id: 'key-123',
        key_prefix: 'lv_live_`prefix',
        scopes: ['qurl:read` @everyone'],
      },
    });
    const interaction = makeStatusInteraction({
      memberFetchBehavior: async () => ({ id: 'admin-original' }),
    });

    await handleCommand(interaction);

    const replyContent = interaction._editReply.mock.calls[0][0].content;
    expect(replyContent).toContain('Key prefix: `lv_live_prefix`');
    expect(replyContent).toContain('Scopes: `qurl:read @everyone`');
  });

  // Worst case for the 2,000-char content limit: surrogate-pair scopes (two
  // UTF-16 units per codepoint, which is what discord.js length-checks), an
  // oversized updated_at, and the admin-left notice all in one reply.
  it('bounds service-reported identity fields to Discord reply limits', async () => {
    db.getGuildConfig.mockResolvedValueOnce({
      guild_id: 'guild-1',
      configured_by: 'admin-departed',
      updated_at: '2026-01-01T00:00:00Z'.repeat(100),
    });
    mockGetIdentity.mockResolvedValueOnce({
      api_key: {
        key_id: 'key-123',
        key_prefix: '🧪'.repeat(3000),
        scopes: Array.from({ length: 100 }, (_, i) => `qurl:scope-${i}-${'🧪'.repeat(100)}`),
      },
    });
    const interaction = makeStatusInteraction({
      memberFetchBehavior: async () => {
        throw Object.assign(new Error('Unknown Member'), { code: 10007 });
      },
    });

    await handleCommand(interaction);

    const replyContent = interaction._editReply.mock.calls[0][0].content;
    expect(replyContent.length).toBeLessThanOrEqual(2000);
    expect(replyContent).toContain('qURL is configured');
    expect(replyContent).toContain('Configured by: <@admin-departed>');
    expect(replyContent).toContain('Last updated:');
    expect(replyContent).toContain('again to take over billing.');
  });

  it('replies to the deferred interaction when the guild is not configured', async () => {
    db.getGuildConfig.mockResolvedValueOnce(null);
    const interaction = makeStatusInteraction({
      memberFetchBehavior: async () => ({ id: 'admin-original' }),
    });

    await handleCommand(interaction);

    expect(interaction.deferReply).toHaveBeenCalledWith({ ephemeral: true });
    expect(interaction._reply).not.toHaveBeenCalled();
    expect(interaction._editReply).toHaveBeenCalledTimes(1);
    expect(interaction._editReply.mock.calls[0][0].content).toContain('not configured for this server');
    expect(mockGetIdentity).not.toHaveBeenCalled();
  });

  it('starts identity verification while the admin-presence check is pending', async () => {
    db.getGuildConfig.mockResolvedValueOnce({
      guild_id: 'guild-1',
      configured_by: 'admin-original',
      updated_at: '2026-01-01T00:00:00Z',
    });
    let releaseKeyRead;
    let signalKeyReadStarted;
    const keyReadStarted = new Promise((resolve) => { signalKeyReadStarted = resolve; });
    const keyReadFinished = new Promise((resolve) => { releaseKeyRead = resolve; });
    db.getGuildApiKey.mockImplementationOnce(() => {
      signalKeyReadStarted();
      return keyReadFinished;
    });
    let releaseMemberFetch;
    let signalMemberFetchStarted;
    const memberFetchStarted = new Promise((resolve) => { signalMemberFetchStarted = resolve; });
    const memberFetchFinished = new Promise((resolve) => { releaseMemberFetch = resolve; });
    const interaction = makeStatusInteraction({
      memberFetchBehavior: () => {
        signalMemberFetchStarted();
        return memberFetchFinished;
      },
    });

    const command = handleCommand(interaction);
    await Promise.all([keyReadStarted, memberFetchStarted]);
    releaseKeyRead(STORED_KEY);
    await Promise.resolve();
    const identityStartedBeforeMemberFinished = mockGetIdentity.mock.calls.length;
    releaseMemberFetch({ id: 'admin-original' });
    await command;

    expect(identityStartedBeforeMemberFinished).toBe(1);
  });

  it('asks the admin to rerun setup when the config row has no stored key', async () => {
    db.getGuildConfig.mockResolvedValueOnce({
      guild_id: 'guild-1',
      configured_by: 'admin-original',
      updated_at: '2026-01-01T00:00:00Z',
    });
    db.getGuildApiKey.mockResolvedValueOnce(null);
    const interaction = makeStatusInteraction({
      memberFetchBehavior: async () => ({ id: 'admin-original' }),
    });

    await handleCommand(interaction);

    const replyContent = interaction._editReply.mock.calls[0][0].content;
    expect(replyContent).toContain('`/qurl setup`');
    expect(replyContent).not.toContain('try `/qurl status` again later');
    expect(mockGetIdentity).not.toHaveBeenCalled();
    expect(logger.warn).toHaveBeenCalledWith('qURL status key unavailable', {
      guild_id: 'guild-1',
    });
  });

  it('shows the passive nudge when members.fetch throws DiscordAPIError 10007 (Unknown Member)', async () => {
    db.getGuildConfig.mockResolvedValueOnce({
      guild_id: 'guild-1',
      configured_by: 'admin-departed',
      updated_at: '2026-01-01T00:00:00Z',
    });
    const interaction = makeStatusInteraction({
      memberFetchBehavior: async () => {
        const err = new Error('Unknown Member');
        err.code = 10007;
        throw err;
      },
    });
    await handleCommand(interaction);
    const replyContent = interaction._editReply.mock.calls[0][0].content;
    expect(replyContent).toContain('qURL is configured');
    expect(replyContent).toContain('has left this server');
    expect(replyContent).toContain('<@admin-departed>');
    // Confirms the remediation guidance is on the wire — the whole
    // point of the nudge is to tell remaining admins what to do next.
    expect(replyContent).toMatch(/run.*\/qurl setup/i);
  });

  it('does NOT show the nudge on a transient Discord API error (avoids mis-flagging a present admin)', async () => {
    db.getGuildConfig.mockResolvedValueOnce({
      guild_id: 'guild-1',
      configured_by: 'admin-original',
      updated_at: '2026-01-01T00:00:00Z',
    });
    const interaction = makeStatusInteraction({
      memberFetchBehavior: async () => {
        const err = new Error('429 Too Many Requests');
        err.code = 429;
        throw err;
      },
    });
    await handleCommand(interaction);
    const replyContent = interaction._editReply.mock.calls[0][0].content;
    expect(replyContent).toContain('qURL is configured');
    // Critical: a rate-limit spike must NOT silently tell an admin
    // their colleague is gone. Only the specific 10007 fires the
    // notice.
    expect(replyContent).not.toContain('has left this server');
  });
});
