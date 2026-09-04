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
function makeStatusInteraction({ memberFetchBehavior }) {
  const reply = jest.fn();
  const editReply = jest.fn();
  return {
    reply: reply.mockImplementation(() => Promise.resolve()),
    deferReply: jest.fn().mockResolvedValue(undefined),
    editReply: editReply.mockImplementation(() => Promise.resolve()),
    isAutocomplete: () => false,
    isChatInputCommand: () => true,
    commandName: 'qurl',
    options: { getSubcommand: () => 'status' },
    guildId: 'guild-1',
    memberPermissions: { has: (p) => p === PermissionFlagsBits.ManageGuild },
    guild: {
      members: { fetch: jest.fn().mockImplementation(memberFetchBehavior) },
    },
    user: { id: 'admin-current' },
    _initialReply: reply,
    _reply: editReply,
  };
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
    expect(interaction._initialReply).not.toHaveBeenCalled();
    expect(interaction._reply).toHaveBeenCalledTimes(1);
    const replyContent = interaction._reply.mock.calls[0][0].content;
    expect(replyContent).toContain('qURL is configured');
    expect(replyContent).toContain('Key prefix: `lv\\_live\\_aaa`');
    expect(replyContent).toContain('Scopes: `qurl:read`, `qurl:write`');
    expect(replyContent).not.toContain(STORED_KEY);
    expect(mockGetIdentity).toHaveBeenCalledWith(STORED_KEY);
    expect(replyContent).not.toContain('has left this server');
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

    const replyContent = interaction._reply.mock.calls[0][0].content;
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

    const replyContent = interaction._reply.mock.calls[0][0].content;
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

    const replyContent = interaction._reply.mock.calls[0][0].content;
    expect(replyContent).toMatch(/check could not be completed/i);
    expect(replyContent).not.toMatch(/revoked|invalid/i);
    expect(replyContent).not.toContain(STORED_KEY);
    expect(replyContent).toContain('Configured by: <@admin-original>');
    expect(replyContent).toContain('Last updated: 2026-01-01T00:00:00Z');
    expect(logger.warn).toHaveBeenCalledWith('qURL status identity check failed', {
      guild_id: 'guild-1',
      status: expectedStatus,
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

    const replyContent = interaction._reply.mock.calls[0][0].content;
    expect(replyContent).toMatch(/check could not be completed/i);
    expect(replyContent).toContain('has left this server');
    expect(replyContent).toContain('<@admin-departed>');
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

    expect(interaction._reply.mock.calls[0][0].content).toContain('Scopes: _none_');
  });

  it('escapes service-reported prefix and scopes before rendering them', async () => {
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

    const replyContent = interaction._reply.mock.calls[0][0].content;
    expect(replyContent).toContain('Key prefix: `lv\\_live\\_\\`prefix`');
    expect(replyContent).toContain('Scopes: `qurl:read\\` @everyone`');
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

    const replyContent = interaction._reply.mock.calls[0][0].content;
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
    const replyContent = interaction._reply.mock.calls[0][0].content;
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
    const replyContent = interaction._reply.mock.calls[0][0].content;
    expect(replyContent).toContain('qURL is configured');
    // Critical: a rate-limit spike must NOT silently tell an admin
    // their colleague is gone. Only the specific 10007 fires the
    // notice.
    expect(replyContent).not.toContain('has left this server');
  });
});
