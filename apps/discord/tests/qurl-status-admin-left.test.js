// Tests for the /qurl status admin-offboarding nudge (#185).
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

jest.mock('../src/store', () => ({
  setGuildApiKey: jest.fn().mockResolvedValue(undefined),
  getGuildApiKey: jest.fn(),
  getGuildConfig: jest.fn(),
  getPendingLink: jest.fn(),
  consumePendingLink: jest.fn(),
}));

const db = require('../src/store');
const { handleCommand } = require('../src/commands');
const { PermissionFlagsBits } = require('discord.js');

// Minimal interaction stub for the /qurl status path. The real
// discord.js Interaction has dozens of fields we don't need; only
// the surface the status handler actually reads is mocked.
function makeStatusInteraction({ memberFetchBehavior }) {
  const reply = jest.fn();
  return {
    reply: reply.mockImplementation(() => Promise.resolve()),
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
    _reply: reply,
  };
}

beforeEach(() => {
  jest.clearAllMocks();
  db.getGuildApiKey.mockResolvedValue('lv_live_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa');
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
    expect(interaction._reply).toHaveBeenCalledTimes(1);
    const replyContent = interaction._reply.mock.calls[0][0].content;
    expect(replyContent).toContain('qURL is configured');
    expect(replyContent).not.toContain('has left this server');
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
