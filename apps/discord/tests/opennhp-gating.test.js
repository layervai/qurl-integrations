/**
 * Proves the ENABLE_OPENNHP_FEATURES=false branches of src/discord.js:
 *
 *   - ensureRolesAndChannels — no guild.roles.create / guild.channels.create
 *   - assignContributorRole  — returns { success:false, reason:'opennhp-disabled' }
 *                              and emits a debug log (observability)
 *   - notifyBadgeEarned      — no-op, debug log
 *   - postGoodFirstIssue     — returns null, debug log
 *   - guildMemberAdd handler — early-returns with no DB read, no DM
 *   - setupWeeklyDigest      — not scheduled (cron.schedule not called)
 *   - verifyBotPermissions   — required-perm set narrows to the 4 runtime
 *                              perms; ManageRoles/ManageChannels are NOT
 *                              demanded
 *
 * The legacy suite (tests/discord.test.js) exercises the flag=true paths.
 */

jest.mock('../src/config', () => ({
  DISCORD_TOKEN: 'test-token',
  DISCORD_CLIENT_ID: 'test-client',
  // Mode-derivation via the shared helper — single-guild-plain
  // (GUILD_ID set, flag off) → isOpenNHPActive=false.
  ...require('./helpers/buildConfigMock').buildConfigMock({
    guildId: 'guild-1',
    enableOpenNHP: false,
  }),
  CONTRIBUTOR_ROLE_NAME: 'Contributor',
  ACTIVE_CONTRIBUTOR_ROLE_NAME: 'Active Contributor',
  CORE_CONTRIBUTOR_ROLE_NAME: 'Core Contributor',
  CHAMPION_ROLE_NAME: 'Champion',
  ACTIVE_CONTRIBUTOR_THRESHOLD: 3,
  CORE_CONTRIBUTOR_THRESHOLD: 10,
  CHAMPION_THRESHOLD: 25,
  GENERAL_CHANNEL_NAME: 'general',
  NOTIFICATION_CHANNEL_NAME: 'general',
  ANNOUNCEMENTS_CHANNEL_NAME: 'announcements',
  CONTRIBUTE_CHANNEL_NAME: 'contribute',
  GITHUB_FEED_CHANNEL_NAME: 'github-feed',
  WEEKLY_DIGEST_CRON: '0 9 * * 0',
  WELCOME_DM_ENABLED: true,
}));

const mockLogger = {
  info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(),
};
jest.mock('../src/logger', () => mockLogger);

const mockDb = {
  getContributionCount: jest.fn().mockReturnValue(0),
  getContributions: jest.fn().mockReturnValue([]),
  BADGE_INFO: {},
};
jest.mock('../src/database', () => mockDb);

const mockCronSchedule = jest.fn(() => ({ stop: jest.fn() }));
jest.mock('node-cron', () => ({
  schedule: mockCronSchedule,
  validate: jest.fn(() => true),
}));

const mockRoleCreate = jest.fn();
const mockChannelCreate = jest.fn();
const mockChannelSend = jest.fn().mockResolvedValue({ id: 'msg-1' });

const allRoles = new Map();
allRoles.find = () => undefined;
const allChannels = new Map();
allChannels.find = () => undefined;

const mockMePermissionsHas = jest.fn(() => true);

const mockGuild = {
  name: 'Playground',
  id: 'guild-1',
  roles: {
    fetch: jest.fn().mockResolvedValue(allRoles),
    create: mockRoleCreate,
  },
  channels: {
    fetch: jest.fn().mockResolvedValue(allChannels),
    create: mockChannelCreate,
  },
  members: {
    fetchMe: jest.fn().mockResolvedValue({ permissions: { has: mockMePermissionsHas } }),
  },
};

const mockClient = {
  once: jest.fn(),
  on: jest.fn(),
  destroy: jest.fn(),
  user: { tag: 'TestBot#0000' },
  guilds: { fetch: jest.fn().mockResolvedValue(mockGuild) },
  application: { commands: { set: jest.fn() } },
};

jest.mock('discord.js', () => ({
  Client: jest.fn(() => mockClient),
  GatewayIntentBits: { Guilds: 1, GuildMembers: 2, GuildVoiceStates: 128 },
  EmbedBuilder: jest.fn().mockImplementation(() => ({
    setColor: jest.fn().mockReturnThis(), setTitle: jest.fn().mockReturnThis(),
    setDescription: jest.fn().mockReturnThis(), addFields: jest.fn().mockReturnThis(),
    setFooter: jest.fn().mockReturnThis(), setTimestamp: jest.fn().mockReturnThis(),
  })),
  ChannelType: { GuildText: 0 },
  PermissionFlagsBits: {
    ViewChannel: 1n, SendMessages: 2n, EmbedLinks: 4n,
    UseApplicationCommands: 8n, ManageRoles: 16n,
    ManageChannels: 32n, ReadMessageHistory: 64n,
  },
}));

const discord = require('../src/discord');

const readyHandler = mockClient.once.mock.calls.find(c => c[0] === 'ready')?.[1];
const guildMemberAddHandler = mockClient.on.mock.calls.find(c => c[0] === 'guildMemberAdd')?.[1];

beforeEach(() => {
  jest.clearAllMocks();
  mockChannelSend.mockResolvedValue({ id: 'msg-1' });
  mockGuild.members.fetchMe.mockResolvedValue({ permissions: { has: mockMePermissionsHas } });
  mockMePermissionsHas.mockReturnValue(true);
});

afterAll(async () => {
  await discord.shutdown();
});

describe('ENABLE_OPENNHP_FEATURES=false — OpenNHP behaviors are gated off', () => {
  describe('ensureRolesAndChannels (via refreshCache)', () => {
    it('does not call guild.roles.create or guild.channels.create', async () => {
      await discord.refreshCache();
      expect(mockRoleCreate).not.toHaveBeenCalled();
      expect(mockChannelCreate).not.toHaveBeenCalled();
    });
  });

  describe('assignContributorRole', () => {
    it('returns { success: false, reason: "opennhp-disabled" } without touching DB', async () => {
      const result = await discord.assignContributorRole('user-1', 1, 'some/repo', 'ghuser');
      expect(result).toEqual({ success: false, reason: 'opennhp-disabled' });
      expect(mockDb.getContributionCount).not.toHaveBeenCalled();
    });

    it('logs a debug line so prod can answer "why did this user not get the role"', async () => {
      await discord.assignContributorRole('user-1', 42, 'OpenNHP/opennhp', 'ghuser');
      expect(mockLogger.debug).toHaveBeenCalledWith(
        'assignContributorRole skipped: OpenNHP features disabled',
        expect.objectContaining({ discordId: 'user-1', prNumber: 42, repo: 'OpenNHP/opennhp' }),
      );
    });
  });

  describe('notifyBadgeEarned', () => {
    it('is a no-op, emits a debug log, and sends nothing', async () => {
      const result = await discord.notifyBadgeEarned('user-1', ['FIRST_PR']);
      expect(result).toBeUndefined();
      expect(mockChannelSend).not.toHaveBeenCalled();
      expect(mockLogger.debug).toHaveBeenCalledWith(
        'notifyBadgeEarned skipped: OpenNHP features disabled',
        expect.objectContaining({ discordId: 'user-1', badgeCount: 1 }),
      );
    });
  });

  describe('postGoodFirstIssue', () => {
    it('returns null, emits a debug log, and skips the channel post', async () => {
      const result = await discord.postGoodFirstIssue('some/repo', 42, 'Title', 'https://example', []);
      expect(result).toBeNull();
      expect(mockChannelSend).not.toHaveBeenCalled();
      expect(mockLogger.debug).toHaveBeenCalledWith(
        'postGoodFirstIssue skipped: OpenNHP features disabled',
        expect.objectContaining({ repo: 'some/repo', issueNumber: 42 }),
      );
    });
  });

  describe('guildMemberAdd handler', () => {
    it('early-returns with no DB read and no DM attempt', async () => {
      const memberSend = jest.fn();
      const fakeMember = {
        id: 'new-user',
        user: { tag: 'NewUser#0001' },
        guild: { id: 'guild-1' },
        send: memberSend,
      };
      await guildMemberAddHandler(fakeMember);
      expect(mockDb.getContributions).not.toHaveBeenCalled();
      expect(memberSend).not.toHaveBeenCalled();
    });
  });

  describe('ready handler', () => {
    it('does not schedule the weekly digest when flag is off', async () => {
      await readyHandler();
      expect(mockCronSchedule).not.toHaveBeenCalled();
    });
  });

  describe('verifyBotPermissions required-perm narrowing', () => {
    it('demands only the 4 runtime perms (no ManageRoles / ManageChannels / ReadMessageHistory)', async () => {
      const { PermissionFlagsBits } = require('discord.js');
      const askedFor = new Set();
      mockMePermissionsHas.mockImplementation((bit) => { askedFor.add(bit); return true; });
      await readyHandler();

      expect(askedFor.has(PermissionFlagsBits.ViewChannel)).toBe(true);
      expect(askedFor.has(PermissionFlagsBits.SendMessages)).toBe(true);
      expect(askedFor.has(PermissionFlagsBits.EmbedLinks)).toBe(true);
      expect(askedFor.has(PermissionFlagsBits.UseApplicationCommands)).toBe(true);

      expect(askedFor.has(PermissionFlagsBits.ManageRoles)).toBe(false);
      expect(askedFor.has(PermissionFlagsBits.ManageChannels)).toBe(false);
      expect(askedFor.has(PermissionFlagsBits.ReadMessageHistory)).toBe(false);
    });
  });
});
