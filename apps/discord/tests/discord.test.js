/**
 * Tests for src/discord.js — covers refreshCache, assignContributorRole,
 * notifyPRMerge, notifyBadgeEarned, postGoodFirstIssue, postReleaseAnnouncement,
 * postStarMilestone, postToGitHubFeed, postWeeklyDigest, sendDM,
 * getVoiceChannelMembers, getTextChannelMembers, shutdown, event handlers.
 */

jest.mock('../src/config', () => ({
  DISCORD_TOKEN: 'test-token',
  DISCORD_CLIENT_ID: 'test-client',
  // This suite exercises the OpenNHP community features path (role
  // auto-creation, role assignment, welcome DM, badge/digest posts).
  // Mode-derivation (GUILD_ID, isMultiTenant, ENABLE_OPENNHP_FEATURES,
  // isOpenNHPActive) comes from the helper so a new derived field
  // added to src/config.js is picked up here automatically. A separate
  // suite (tests/opennhp-gating.test.js) covers the flag=false branches.
  ...require('./helpers/buildConfigMock').buildConfigMock({
    guildId: 'guild-1',
    enableOpenNHP: true,
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

jest.mock('../src/logger', () => ({
  info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(),
}));

const mockDb = {
  getContributions: jest.fn(() => []),
  getContributionCount: jest.fn(() => 1),
  BADGE_INFO: { first_pr: { emoji: 'e', name: 'First PR', description: 'd' } },
  getWeeklyDigestData: jest.fn(() => ({
    totalPRs: 2, uniqueContributors: 1, newContributors: [{ discord_id: 'u1' }],
    byRepo: { 'OpenNHP/opennhp': [{ pr_number: 1 }] }, prs: [],
  })),
};
jest.mock('../src/database', () => mockDb);

jest.mock('node-cron', () => ({ schedule: jest.fn(() => ({ stop: jest.fn() })) }));

// Build a rich mock guild
const mockSend = jest.fn().mockResolvedValue({ id: 'msg-1' });
const allRoles = new Map([
  ['role-c', { id: 'role-c', name: 'Contributor' }],
  ['role-ac', { id: 'role-ac', name: 'Active Contributor' }],
]);
allRoles.find = (fn) => { for (const r of allRoles.values()) { if (fn(r)) return r; } return undefined; };

const allChannels = new Map([
  ['ch-gen', { id: 'ch-gen', name: 'general', send: mockSend }],
  ['ch-ann', { id: 'ch-ann', name: 'announcements', send: mockSend }],
  ['ch-con', { id: 'ch-con', name: 'contribute', send: mockSend }],
  ['ch-ghf', { id: 'ch-ghf', name: 'github-feed', send: mockSend }],
]);
allChannels.find = (fn) => { for (const c of allChannels.values()) { if (fn(c)) return c; } return undefined; };

const mockGuild = {
  name: 'Test Guild',
  roles: { fetch: jest.fn().mockResolvedValue(allRoles), cache: allRoles, create: jest.fn() },
  channels: { fetch: jest.fn().mockResolvedValue(allChannels), create: jest.fn() },
  members: {
    fetch: jest.fn().mockImplementation((id) => {
      if (id === 'unknown-member') throw Object.assign(new Error('Unknown Member'), { code: 10007 });
      return Promise.resolve({
        id: id,
        user: { tag: `User${id}#0001` },
        roles: {
          cache: { has: jest.fn(() => false) },
          add: jest.fn().mockResolvedValue(true),
        },
        send: jest.fn().mockResolvedValue(true),
      });
    }),
  },
};

const mockClient = {
  once: jest.fn(),
  on: jest.fn(),
  destroy: jest.fn(),
  guilds: { fetch: jest.fn().mockResolvedValue(mockGuild) },
  users: { fetch: jest.fn() },
  user: { tag: 'TestBot#0001' },
  application: { commands: { set: jest.fn() } },
};

jest.mock('discord.js', () => ({
  Client: jest.fn(() => mockClient),
  GatewayIntentBits: { Guilds: 1, GuildMembers: 2, GuildVoiceStates: 128 },
  EmbedBuilder: jest.fn().mockImplementation(() => ({
    setColor: jest.fn().mockReturnThis(), setTitle: jest.fn().mockReturnThis(),
    setDescription: jest.fn().mockReturnThis(), addFields: jest.fn().mockReturnThis(),
    setFooter: jest.fn().mockReturnThis(), setTimestamp: jest.fn().mockReturnThis(),
    setURL: jest.fn().mockReturnThis(), setAuthor: jest.fn().mockReturnThis(),
  })),
  ChannelType: { GuildText: 0, GuildVoice: 2, GuildStageVoice: 13 },
  PermissionFlagsBits: { ViewChannel: 1024n },
}));

const discord = require('../src/discord');

// Save event handler references before they get cleared
const readyHandler = mockClient.once.mock.calls.find(c => c[0] === 'ready')?.[1];
const roleDeleteHandler = mockClient.on.mock.calls.find(c => c[0] === 'roleDelete')?.[1];
const channelDeleteHandler = mockClient.on.mock.calls.find(c => c[0] === 'channelDelete')?.[1];
const guildMemberAddHandler = mockClient.on.mock.calls.find(c => c[0] === 'guildMemberAdd')?.[1];

beforeEach(() => {
  jest.clearAllMocks();
  mockSend.mockResolvedValue({ id: 'msg-1' });
  mockClient.guilds.fetch.mockResolvedValue(mockGuild);
  mockGuild.roles.fetch.mockResolvedValue(allRoles);
  mockGuild.channels.fetch.mockResolvedValue(allChannels);
});

describe('discord module', () => {
  describe('refreshCache', () => {
    it('fetches guild, roles, and channels', async () => {
      await discord.refreshCache();
      expect(mockClient.guilds.fetch).toHaveBeenCalledWith('guild-1');
      expect(mockGuild.roles.fetch).toHaveBeenCalled();
      expect(mockGuild.channels.fetch).toHaveBeenCalled();
    });

    it('re-throws on fetch failure so callers know the cache is stale', async () => {
      mockClient.guilds.fetch.mockRejectedValueOnce(new Error('no guild'));
      await expect(discord.refreshCache()).rejects.toThrow('no guild');
    });
  });

  describe('assignContributorRole', () => {
    it('assigns contributor role to a member', async () => {
      await discord.refreshCache();
      mockDb.getContributionCount.mockReturnValue(1);
      const result = await discord.assignContributorRole('user-1', 1, 'OpenNHP/opennhp', 'ghuser');
      expect(result.success).toBe(true);
    });

    it('handles unknown member', async () => {
      await discord.refreshCache();
      mockGuild.members.fetch.mockRejectedValueOnce(
        Object.assign(new Error('Unknown Member'), { code: 10007 })
      );
      const result = await discord.assignContributorRole('unknown', 1, 'repo', 'user');
      expect(result.success).toBe(false);
      expect(result.reason).toBe('member_not_found');
    });

    it('handles generic error', async () => {
      await discord.refreshCache();
      mockGuild.members.fetch.mockRejectedValueOnce(new Error('generic'));
      const result = await discord.assignContributorRole('err', 1, 'repo', 'user');
      expect(result.success).toBe(false);
      expect(result.reason).toBe('error');
    });
  });

  describe('notifyPRMerge', () => {
    it('posts PR merge notification to general channel', async () => {
      await discord.refreshCache();
      const result = await discord.notifyPRMerge(42, 'OpenNHP/opennhp', 'dev', 'Fix bug', 'https://github.com/pr/42');
      expect(mockSend).toHaveBeenCalled();
      expect(result).toBeDefined();
    });
  });

  describe('notifyBadgeEarned', () => {
    it('posts badge notification', async () => {
      await discord.refreshCache();
      await discord.notifyBadgeEarned('u1', ['first_pr']);
      expect(mockSend).toHaveBeenCalled();
    });

    it('does nothing for empty badges', async () => {
      await discord.refreshCache();
      mockSend.mockClear();
      await discord.notifyBadgeEarned('u1', []);
      expect(mockSend).not.toHaveBeenCalled();
    });

    it('handles send failure', async () => {
      await discord.refreshCache();
      mockSend.mockRejectedValueOnce(new Error('send failed'));
      await discord.notifyBadgeEarned('u1', ['first_pr']);
      // Should not throw
    });
  });

  describe('postGoodFirstIssue', () => {
    it('posts to contribute channel', async () => {
      await discord.refreshCache();
      const result = await discord.postGoodFirstIssue('repo', 10, 'Easy fix', 'https://url', ['good first issue']);
      expect(mockSend).toHaveBeenCalled();
      expect(result).toBeDefined();
    });

    it('handles send failure', async () => {
      await discord.refreshCache();
      mockSend.mockRejectedValueOnce(new Error('fail'));
      const result = await discord.postGoodFirstIssue('repo', 10, 'title', 'url', []);
      expect(result).toBeNull();
    });
  });

  describe('postReleaseAnnouncement', () => {
    it('posts release to announcements channel', async () => {
      await discord.refreshCache();
      const result = await discord.postReleaseAnnouncement('repo', 'v1.0', 'Release', 'url', 'Notes here');
      expect(mockSend).toHaveBeenCalled();
      expect(result).toBeDefined();
    });

    it('handles missing body', async () => {
      await discord.refreshCache();
      const result = await discord.postReleaseAnnouncement('repo', 'v2.0', 'Release 2', 'url', null);
      expect(mockSend).toHaveBeenCalled();
    });

    it('truncates long release body', async () => {
      await discord.refreshCache();
      const longBody = 'x'.repeat(600);
      await discord.postReleaseAnnouncement('repo', 'v3', 'R', 'url', longBody);
      expect(mockSend).toHaveBeenCalled();
    });

    it('handles send failure', async () => {
      await discord.refreshCache();
      mockSend.mockRejectedValueOnce(new Error('fail'));
      const result = await discord.postReleaseAnnouncement('repo', 'v4', 'R', 'url', 'n');
      expect(result).toBeNull();
    });
  });

  describe('postStarMilestone', () => {
    it('posts star milestone', async () => {
      await discord.refreshCache();
      const result = await discord.postStarMilestone('repo', 100, 'url');
      expect(mockSend).toHaveBeenCalled();
      expect(result).toBeDefined();
    });

    it('handles send failure', async () => {
      await discord.refreshCache();
      mockSend.mockRejectedValueOnce(new Error('fail'));
      const result = await discord.postStarMilestone('repo', 50, 'url');
      expect(result).toBeNull();
    });
  });

  describe('postToGitHubFeed', () => {
    it('posts embed to github-feed channel', async () => {
      await discord.refreshCache();
      const result = await discord.postToGitHubFeed({ title: 'test' });
      expect(mockSend).toHaveBeenCalled();
    });

    it('handles send failure', async () => {
      await discord.refreshCache();
      mockSend.mockRejectedValueOnce(new Error('fail'));
      const result = await discord.postToGitHubFeed({});
      expect(result).toBeNull();
    });
  });

  describe('postWeeklyDigest', () => {
    it('posts weekly digest when there is activity', async () => {
      await discord.refreshCache();
      const result = await discord.postWeeklyDigest();
      expect(mockSend).toHaveBeenCalled();
    });

    it('skips when no activity', async () => {
      await discord.refreshCache();
      mockDb.getWeeklyDigestData.mockReturnValueOnce({
        totalPRs: 0, uniqueContributors: 0, newContributors: [], byRepo: {},
      });
      const result = await discord.postWeeklyDigest();
      expect(result).toBeNull();
    });

    it('handles send failure', async () => {
      await discord.refreshCache();
      mockSend.mockRejectedValueOnce(new Error('fail'));
      const result = await discord.postWeeklyDigest();
      expect(result).toBeNull();
    });
  });

  describe('sendDM', () => {
    it('sends DM and returns true', async () => {
      const mockUser = { send: jest.fn().mockResolvedValue(true) };
      mockClient.users.fetch.mockResolvedValue(mockUser);
      const result = await discord.sendDM('u1', 'Hello');
      expect(result).toBe(true);
    });

    it('returns false on error', async () => {
      mockClient.users.fetch.mockRejectedValue(new Error('fail'));
      const result = await discord.sendDM('u2', 'Hello');
      expect(result).toBe(false);
    });
  });

  describe('getVoiceChannelMembers', () => {
    it('returns not_in_voice when no voice state', () => {
      const guild = { voiceStates: { cache: new Map() } };
      expect(discord.getVoiceChannelMembers(guild, 's1').error).toBe('not_in_voice');
    });

    it('returns members', () => {
      const membersMap = new Map([
        ['s1', { id: 's1', user: { bot: false } }],
        ['u2', { id: 'u2', user: { bot: false, username: 'U2' } }],
      ]);
      membersMap.filter = (fn) => {
        const r = []; for (const [k, v] of membersMap) if (fn(v)) r.push(v);
        r.map = Array.prototype.map.bind(r); return r;
      };
      const guild = { voiceStates: { cache: new Map([['s1', { channel: { members: membersMap, name: 'VC' } }]]) } };
      const result = discord.getVoiceChannelMembers(guild, 's1');
      expect(result.error).toBeNull();
      expect(result.members).toHaveLength(1);
    });
  });

  describe('getTextChannelMembers', () => {
    // Helper: turn a plain Map into something with .filter() returning an
    // array that also has .map (matches discord.js Collection enough for
    // these tests without pulling in the real class).
    const asCollection = (map) => {
      map.filter = (fn) => {
        const r = []; for (const [, v] of map) if (fn(v)) r.push(v);
        r.map = Array.prototype.map.bind(r); return r;
      };
      return map;
    };

    it('filters sender and bots on a text channel', () => {
      const membersMap = asCollection(new Map([
        ['s1', { id: 's1', user: { bot: false } }],
        ['u2', { id: 'u2', user: { bot: false } }],
        ['b1', { id: 'b1', user: { bot: true } }],
      ]));
      // Omit `type` so it falls through the isVoice branch — text channel
      // semantics: channel.members is the viewer set by Discord's rules.
      const result = discord.getTextChannelMembers({ members: membersMap }, 's1');
      expect(result).toHaveLength(1);
    });

    it('on a voice channel, returns all guild members who CAN VIEW — not just voice-connected', () => {
      // Regression for "target=channel from a voice channel only sent to
      // voice-connected users." The bug was that channel.members on a
      // voice channel returns currently-connected members only; we must
      // instead compute viewers from guild.members.cache + permissionsFor.
      const connected = asCollection(new Map([
        ['u2', { id: 'u2', user: { id: 'u2', bot: false } }],
      ]));
      const allGuildMembers = asCollection(new Map([
        ['s1', { id: 's1', user: { id: 's1', bot: false } }],
        ['u2', { id: 'u2', user: { id: 'u2', bot: false } }], // connected
        ['u3', { id: 'u3', user: { id: 'u3', bot: false } }], // not connected but can view
        ['b1', { id: 'b1', user: { id: 'b1', bot: true } }],  // bot, filtered out
        ['u4', { id: 'u4', user: { id: 'u4', bot: false } }], // no perms — filtered out
      ]));

      const channel = {
        type: 2, // GuildVoice
        members: connected, // would return only u2 if we used this
        guild: { members: { cache: allGuildMembers } },
        permissionsFor: (m) => {
          // Everyone can view except u4
          if (m.id === 'u4') return { has: () => false };
          return { has: () => true };
        },
      };

      const result = discord.getTextChannelMembers(channel, 's1');
      // Pin identity, not just count — a swap of u2/u4 or inclusion of a
      // bot would also give length 2 but wrong users.
      const returnedIds = result.map(u => u.id).sort();
      expect(returnedIds).toEqual(['u2', 'u3']);
    });

    it('on a stage-voice channel, also uses the guild-viewer path', () => {
      const allGuildMembers = asCollection(new Map([
        ['s1', { id: 's1', user: { id: 's1', bot: false } }],
        ['u2', { id: 'u2', user: { id: 'u2', bot: false } }],
      ]));
      const channel = {
        type: 13, // GuildStageVoice
        members: asCollection(new Map()), // zero voice-connected
        guild: { members: { cache: allGuildMembers } },
        permissionsFor: () => ({ has: () => true }),
      };
      const result = discord.getTextChannelMembers(channel, 's1');
      expect(result.map(u => u.id)).toEqual(['u2']); // sender excluded
    });

    it('voice channel with missing .guild returns empty (does NOT fall back to broken channel.members)', () => {
      // Defense-in-depth: if a voice-typed channel somehow lacks .guild
      // (partial cache / unusual gateway state), the helper must not
      // silently fall through to channel.members — that's the
      // voice-connected-only set this function exists to avoid.
      const connected = asCollection(new Map([
        ['u2', { id: 'u2', user: { id: 'u2', bot: false } }],
      ]));
      const channel = {
        type: 2,           // GuildVoice
        members: connected, // would be returned if the fallback kicked in
        guild: undefined,
      };
      const result = discord.getTextChannelMembers(channel, 's1');
      expect(result).toEqual([]);
    });

    it('voice channel — members with null permissionsFor are excluded cleanly', () => {
      // permissionsFor can return null for partially-cached / unresolvable
      // members. The filter must not throw and must not include such users.
      const allGuildMembers = asCollection(new Map([
        ['u2', { id: 'u2', user: { id: 'u2', bot: false } }],
        ['u3', { id: 'u3', user: { id: 'u3', bot: false } }], // null perms
      ]));
      const channel = {
        type: 2,
        members: asCollection(new Map()),
        guild: { members: { cache: allGuildMembers } },
        permissionsFor: (m) => (m.id === 'u3' ? null : { has: () => true }),
      };
      const result = discord.getTextChannelMembers(channel, 's1');
      expect(result.map(u => u.id)).toEqual(['u2']);
    });
  });

  describe('shutdown', () => {
    it('destroys client', () => {
      discord.shutdown();
      expect(mockClient.destroy).toHaveBeenCalled();
    });
  });

  describe('event handlers', () => {
    it('registers ready, roleDelete, channelDelete, guildMemberAdd handlers', () => {
      expect(readyHandler).toBeDefined();
      expect(roleDeleteHandler).toBeDefined();
      expect(channelDeleteHandler).toBeDefined();
      expect(guildMemberAddHandler).toBeDefined();
    });

    it('ready handler refreshes cache', async () => {
      await readyHandler();
      expect(mockClient.guilds.fetch).toHaveBeenCalled();
    });

    it('guildMemberAdd sends welcome DM to new member', async () => {
      mockDb.getContributions.mockReturnValue([]);
      const member = {
        guild: { id: 'guild-1' },
        id: 'new-user',
        user: { tag: 'NewUser#0001' },
        send: jest.fn().mockResolvedValue(true),
      };
      await guildMemberAddHandler(member);
      expect(member.send).toHaveBeenCalled();
    });

    it('guildMemberAdd restores roles for returning contributor', async () => {
      mockDb.getContributions.mockReturnValue([{ pr_number: 1 }]);
      await discord.refreshCache();

      const member = {
        guild: { id: 'guild-1' },
        id: 'return-user',
        user: { tag: 'ReturnUser#0001' },
        roles: {
          cache: { has: jest.fn(() => false) },
          add: jest.fn().mockResolvedValue(true),
        },
        send: jest.fn().mockResolvedValue(true),
      };
      await guildMemberAddHandler(member);
      expect(mockSend).toHaveBeenCalled();
    });

    it('guildMemberAdd ignores other guilds', async () => {
      const member = {
        guild: { id: 'other-guild' },
        id: 'other-user',
        user: { tag: 'Other#0001' },
        send: jest.fn(),
      };
      await guildMemberAddHandler(member);
      expect(member.send).not.toHaveBeenCalled();
    });

    it('guildMemberAdd handles DM failure gracefully', async () => {
      mockDb.getContributions.mockReturnValue([]);
      const member = {
        guild: { id: 'guild-1' },
        id: 'dm-fail-user',
        user: { tag: 'DMFail#0001' },
        send: jest.fn().mockRejectedValue(new Error('DMs disabled')),
      };
      await guildMemberAddHandler(member);
      // Should not throw
    });
  });

  describe('refreshCache — ensureRolesAndChannels creates missing items', () => {
    it('creates missing roles and channels', async () => {
      // Override roles to be empty — should trigger creation
      const emptyRoles = new Map();
      emptyRoles.find = () => undefined;
      const emptyChannels = new Map();
      emptyChannels.find = () => undefined;

      mockGuild.roles.fetch.mockResolvedValueOnce(emptyRoles).mockResolvedValueOnce(allRoles);
      mockGuild.channels.fetch.mockResolvedValueOnce(emptyChannels).mockResolvedValueOnce(allChannels);
      mockGuild.roles.create.mockResolvedValue({ name: 'Created' });
      mockGuild.channels.create.mockResolvedValue({ name: 'created-channel' });

      await discord.refreshCache();

      // Should have attempted to create missing roles and channels
      expect(mockGuild.roles.create).toHaveBeenCalled();
      expect(mockGuild.channels.create).toHaveBeenCalled();
    });

    it('handles role creation failure', async () => {
      const emptyRoles = new Map();
      emptyRoles.find = () => undefined;

      mockGuild.roles.fetch.mockResolvedValueOnce(emptyRoles).mockResolvedValueOnce(allRoles);
      mockGuild.channels.fetch.mockResolvedValue(allChannels);
      mockGuild.roles.create.mockRejectedValue(new Error('Permission denied'));

      await discord.refreshCache();
      // Should not throw
    });

    it('handles channel creation failure', async () => {
      const emptyChannels = new Map();
      emptyChannels.find = () => undefined;

      mockGuild.roles.fetch.mockResolvedValue(allRoles);
      mockGuild.channels.fetch.mockResolvedValueOnce(emptyChannels).mockResolvedValueOnce(allChannels);
      mockGuild.channels.create.mockRejectedValue(new Error('Permission denied'));

      await discord.refreshCache();
      // Should not throw
    });
  });

  describe('roleDelete and channelDelete handlers', () => {
    it('refreshes cache when tracked role is deleted', async () => {
      await discord.refreshCache();
      mockClient.guilds.fetch.mockClear();

      // Trigger roleDelete with a tracked role
      const role = { id: 'role-c' }; // matches Contributor
      await roleDeleteHandler(role);

      expect(mockClient.guilds.fetch).toHaveBeenCalled();
    });

    it('ignores deletion of non-tracked role', async () => {
      await discord.refreshCache();
      mockClient.guilds.fetch.mockClear();

      await roleDeleteHandler({ id: 'untracked-role' });
      // Should not trigger refresh for untracked role
    });

    it('refreshes cache when tracked channel is deleted', async () => {
      await discord.refreshCache();
      mockClient.guilds.fetch.mockClear();

      const channel = { id: 'ch-gen' }; // matches general
      await channelDeleteHandler(channel);

      expect(mockClient.guilds.fetch).toHaveBeenCalled();
    });
  });

  describe('assignContributorRole — role progression', () => {
    it('announces first-time contributor', async () => {
      await discord.refreshCache();
      mockDb.getContributionCount.mockReturnValue(1);
      const result = await discord.assignContributorRole('first-contrib', 1, 'OpenNHP/opennhp', 'ghuser');
      expect(result.success).toBe(true);
    });

    it('announces role upgrade for high contribution count', async () => {
      await discord.refreshCache();
      mockDb.getContributionCount.mockReturnValue(25);
      const result = await discord.assignContributorRole('power-user', 99, 'OpenNHP/opennhp', 'powergh');
      expect(result.success).toBe(true);
    });
  });

  describe('notifyPRMerge — send error', () => {
    it('returns null when channel.send throws', async () => {
      await discord.refreshCache();
      mockSend.mockRejectedValueOnce(new Error('channel unavailable'));
      const result = await discord.notifyPRMerge(1, 'repo', 'user', 'title', 'url');
      expect(result).toBeNull();
    });
  });

  describe('postWeeklyDigest — send error', () => {
    it('returns null when channel.send throws', async () => {
      await discord.refreshCache();
      mockSend.mockRejectedValueOnce(new Error('channel unavailable'));
      const result = await discord.postWeeklyDigest();
      expect(result).toBeNull();
    });
  });

  describe('postStarMilestone — send error', () => {
    it('returns null when channel.send throws', async () => {
      await discord.refreshCache();
      mockSend.mockRejectedValueOnce(new Error('channel unavailable'));
      const result = await discord.postStarMilestone('repo', 100, 'url');
      expect(result).toBeNull();
    });
  });

  describe('postReleaseAnnouncement — send error', () => {
    it('returns null when channel.send throws', async () => {
      await discord.refreshCache();
      mockSend.mockRejectedValueOnce(new Error('channel unavailable'));
      const result = await discord.postReleaseAnnouncement('repo', 'v1', 'R', 'url', 'body');
      expect(result).toBeNull();
    });
  });

  describe('postGoodFirstIssue — send error', () => {
    it('returns null when channel.send throws', async () => {
      await discord.refreshCache();
      mockSend.mockRejectedValueOnce(new Error('channel unavailable'));
      const result = await discord.postGoodFirstIssue('repo', 1, 'title', 'url', []);
      expect(result).toBeNull();
    });
  });

  describe('exports', () => {
    it('exports all expected functions', () => {
      expect(typeof discord.sendDM).toBe('function');
      expect(typeof discord.getVoiceChannelMembers).toBe('function');
      expect(typeof discord.getTextChannelMembers).toBe('function');
      expect(typeof discord.assignContributorRole).toBe('function');
      expect(typeof discord.notifyPRMerge).toBe('function');
      expect(typeof discord.notifyBadgeEarned).toBe('function');
      expect(typeof discord.postGoodFirstIssue).toBe('function');
      expect(typeof discord.postReleaseAnnouncement).toBe('function');
      expect(typeof discord.postStarMilestone).toBe('function');
      expect(typeof discord.postToGitHubFeed).toBe('function');
      expect(typeof discord.postWeeklyDigest).toBe('function');
      expect(typeof discord.refreshCache).toBe('function');
      expect(typeof discord.shutdown).toBe('function');
      expect(typeof discord.getGuild).toBe('function');
      expect(typeof discord.getRoles).toBe('function');
      expect(typeof discord.getChannels).toBe('function');
    });
  });
});
