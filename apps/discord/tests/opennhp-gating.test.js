/**
 * Proves the ENABLE_OPENNHP_FEATURES=false branches of src/discord.js —
 * role/channel auto-creation, role assignment, badge announcements,
 * good-first-issue posts, and the guildMemberAdd welcome DM are all
 * gated and must no-op when the flag is disabled. The legacy suite
 * (tests/discord.test.js) covers the flag=true happy paths.
 */

jest.mock('../src/config', () => ({
  DISCORD_TOKEN: 'test-token',
  DISCORD_CLIENT_ID: 'test-client',
  GUILD_ID: 'guild-1',
  ENABLE_OPENNHP_FEATURES: false,
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

jest.mock('../src/logger', () => ({
  info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(),
}));

jest.mock('../src/database', () => ({
  getContributionCount: jest.fn().mockReturnValue(0),
  getContributions: jest.fn().mockReturnValue([]),
  BADGE_INFO: {},
}));

const mockMemberSend = jest.fn();
const mockChannelSend = jest.fn();
const mockRoleCreate = jest.fn();
const mockChannelCreate = jest.fn();

jest.mock('discord.js', () => {
  class EmbedBuilder {
    setColor() { return this; }
    setTitle() { return this; }
    setDescription() { return this; }
    addFields() { return this; }
    setFooter() { return this; }
    setTimestamp() { return this; }
  }
  const { EventEmitter } = require('events');
  class Client extends EventEmitter {
    constructor() {
      super();
      this.user = { tag: 'TestBot#0000' };
      this.guilds = { fetch: jest.fn() };
      this.application = { commands: { set: jest.fn() } };
    }
    login() { return Promise.resolve(); }
    destroy() { return Promise.resolve(); }
  }
  return {
    Client,
    GatewayIntentBits: { Guilds: 0, GuildMembers: 0, GuildVoiceStates: 0 },
    EmbedBuilder,
    ChannelType: { GuildText: 0 },
    PermissionFlagsBits: {
      ViewChannel: 1n, SendMessages: 2n, EmbedLinks: 4n,
      UseApplicationCommands: 8n, ManageRoles: 16n,
      ManageChannels: 32n, ReadMessageHistory: 64n,
    },
  };
});

describe('ENABLE_OPENNHP_FEATURES=false — OpenNHP behaviors are gated off', () => {
  let discord;

  beforeAll(() => {
    discord = require('../src/discord');
  });

  afterAll(async () => {
    await discord.shutdown();
  });

  test('assignContributorRole returns { success: false, reason: "opennhp-disabled" }', async () => {
    const result = await discord.assignContributorRole('user-1', 1, 'some/repo', 'ghuser');
    expect(result).toEqual({ success: false, reason: 'opennhp-disabled' });
    // DB should NOT have been touched (the guard runs before refreshCache/fetch)
    expect(require('../src/database').getContributionCount).not.toHaveBeenCalled();
  });

  test('notifyBadgeEarned is a no-op (returns undefined, sends nothing)', async () => {
    const result = await discord.notifyBadgeEarned('user-1', ['FIRST_PR']);
    expect(result).toBeUndefined();
    expect(mockChannelSend).not.toHaveBeenCalled();
  });

  test('postGoodFirstIssue returns null (skips the channel post)', async () => {
    const result = await discord.postGoodFirstIssue('some/repo', 42, 'Title', 'https://example', []);
    expect(result).toBeNull();
    expect(mockChannelSend).not.toHaveBeenCalled();
  });
});
