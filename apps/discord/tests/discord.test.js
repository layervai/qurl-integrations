/**
 * Tests for src/discord.js — covers refreshCache, sendDM, shutdown,
 * event handlers, and the intent boot canaries.
 */

jest.mock('../src/config', () => ({
  DISCORD_TOKEN: 'test-token',
  DISCORD_CLIENT_ID: 'test-client',
  // Mode-derivation (GUILD_ID, isMultiTenant) comes from the helper so
  // a new derived field added to src/config.js is picked up here
  // automatically.
  ...require('./helpers/buildConfigMock').buildConfigMock({
    guildId: 'guild-1',
  }),
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(), audit: jest.fn(),
}));

jest.mock('../src/store', () => ({}));

const mockGuild = {
  name: 'Test Guild',
  members: {
    fetchMe: jest.fn().mockResolvedValue({
      permissions: { has: jest.fn(() => true) },
    }),
  },
};

const mockClient = {
  once: jest.fn(),
  on: jest.fn(),
  destroy: jest.fn(),
  isReady: jest.fn(() => true),
  // Stub matches discord.js v14 WebSocketManager surface (Map of
  // shards, numeric ping). Future handlers that read client.ws via
  // gateway-metrics or anywhere else won't crash silently in tests.
  ws: { shards: new Map(), ping: 50 },
  guilds: { fetch: jest.fn().mockResolvedValue(mockGuild) },
  users: { fetch: jest.fn() },
  user: { tag: 'TestBot#0001' },
  application: { commands: { set: jest.fn() } },
};

jest.mock('discord.js', () => ({
  Client: jest.fn(() => mockClient),
  // GuildVoiceStates (=128 in production discord.js) is load-bearing
  // for the /qurl send + /qurl map voice-channel-everyone path —
  // channel.members for voice channels reads the voice-state cache
  // which is only populated when the intent is declared. The discord.js
  // module's assertIntent at boot would throw if this bit is missing
  // from the mock; the value here matches discord.js's enum so future
  // production-shape canary tests can rely on it.
  GatewayIntentBits: { Guilds: 1, GuildMembers: 2, GuildVoiceStates: 128 },
  EmbedBuilder: jest.fn().mockImplementation(() => ({
    setColor: jest.fn().mockReturnThis(), setTitle: jest.fn().mockReturnThis(),
    setDescription: jest.fn().mockReturnThis(), addFields: jest.fn().mockReturnThis(),
    setFooter: jest.fn().mockReturnThis(), setTimestamp: jest.fn().mockReturnThis(),
    setURL: jest.fn().mockReturnThis(), setAuthor: jest.fn().mockReturnThis(),
  })),
  ChannelType: { GuildText: 0, GuildVoice: 2, GuildStageVoice: 13 },
  PermissionFlagsBits: {
    ViewChannel: 1024n,
    SendMessages: 2048n,
    EmbedLinks: 16384n,
    UseApplicationCommands: 2147483648n,
  },
}));

const discord = require('../src/discord');

// Save event handler references before they get cleared
const readyHandler = mockClient.once.mock.calls.find(c => c[0] === 'ready')?.[1];
const guildCreateHandler = mockClient.on.mock.calls.find(c => c[0] === 'guildCreate')?.[1];
const guildDeleteHandler = mockClient.on.mock.calls.find(c => c[0] === 'guildDelete')?.[1];

beforeEach(() => {
  jest.clearAllMocks();
  mockClient.guilds.fetch.mockResolvedValue(mockGuild);
  mockGuild.members.fetchMe.mockResolvedValue({
    permissions: { has: jest.fn(() => true) },
  });
});

describe('discord module', () => {
  describe('refreshCache', () => {
    it('fetches and caches the watched guild', async () => {
      await discord.refreshCache();
      expect(mockClient.guilds.fetch).toHaveBeenCalledWith('guild-1');
      expect(discord.getGuild()).toBe(mockGuild);
    });

    it('re-throws on fetch failure so callers know the cache is stale', async () => {
      mockClient.guilds.fetch.mockRejectedValueOnce(new Error('no guild'));
      await expect(discord.refreshCache()).rejects.toThrow('no guild');
    });

    it('coalesces concurrent callers into a single in-flight fetch', async () => {
      await Promise.all([discord.refreshCache(), discord.refreshCache()]);
      expect(mockClient.guilds.fetch).toHaveBeenCalledTimes(1);
    });
  });

  describe('sendDM', () => {
    it('sends DM and returns ok with channel + message ids', async () => {
      const mockUser = {
        send: jest.fn().mockResolvedValue({ id: 'm-1', channelId: 'c-1' }),
      };
      mockClient.users.fetch.mockResolvedValue(mockUser);
      const result = await discord.sendDM('u1', 'Hello');
      expect(result).toEqual({ ok: true, channelId: 'c-1', messageId: 'm-1' });
    });

    it('returns { ok: false } on error', async () => {
      mockClient.users.fetch.mockRejectedValue(new Error('fail'));
      const result = await discord.sendDM('u2', 'Hello');
      expect(result).toEqual({ ok: false });
    });
  });

  describe('shutdown', () => {
    it('destroys client', () => {
      discord.shutdown();
      expect(mockClient.destroy).toHaveBeenCalled();
    });
  });

  describe('event handlers', () => {
    it('registers the ready handler', () => {
      expect(readyHandler).toBeDefined();
    });

    it('no longer subscribes to the contributor-surface events (#1026)', () => {
      // roleDelete / channelDelete existed only to invalidate the
      // role+channel caches, and guildMemberAdd only to send the
      // welcome DM. All three are gone; re-adding one without
      // a consumer would silently re-broaden what the bot reacts to.
      const subscribed = mockClient.on.mock.calls.map(c => c[0]);
      expect(subscribed).not.toContain('roleDelete');
      expect(subscribed).not.toContain('channelDelete');
      expect(subscribed).not.toContain('guildMemberAdd');
    });

    // Phase 1 monitoring — guildCreate / guildDelete emit audit events
    // for install/uninstall trend tracking. guildCreate also fires on
    // shard ready burst (Discord re-sends an event for every guild the
    // bot is already in), so the handler tags those as `replay: true`
    // when the client is not yet isReady().
    it('registers guildCreate and guildDelete handlers', () => {
      expect(guildCreateHandler).toBeDefined();
      expect(guildDeleteHandler).toBeDefined();
    });

    it('guildCreate emits guild_install audit event', () => {
      const logger = require('../src/logger');
      const { AUDIT_EVENTS } = require('../src/constants');
      logger.audit.mockClear();
      mockClient.isReady.mockReturnValueOnce(true);
      guildCreateHandler({ id: 'g-new', memberCount: 42 });
      expect(logger.audit).toHaveBeenCalledWith(
        AUDIT_EVENTS.GUILD_INSTALL,
        expect.objectContaining({ guild_id: 'g-new', member_count: 42, replay: false }),
      );
    });

    it('guildCreate tags as replay=true during shard-ready burst', () => {
      const logger = require('../src/logger');
      logger.audit.mockClear();
      mockClient.isReady.mockReturnValueOnce(false);
      guildCreateHandler({ id: 'g-replay', memberCount: 1 });
      expect(logger.audit).toHaveBeenCalledWith(
        expect.any(String),
        expect.objectContaining({ replay: true }),
      );
    });

    it('guildDelete emits guild_uninstall audit event', () => {
      const logger = require('../src/logger');
      const { AUDIT_EVENTS } = require('../src/constants');
      logger.audit.mockClear();
      guildDeleteHandler({ id: 'g-gone' });
      expect(logger.audit).toHaveBeenCalledWith(
        AUDIT_EVENTS.GUILD_UNINSTALL,
        expect.objectContaining({ guild_id: 'g-gone' }),
      );
    });

    it('guildCreate handler swallows audit errors so a logging blip cannot break installs', () => {
      const logger = require('../src/logger');
      logger.audit.mockImplementationOnce(() => { throw new Error('audit broken'); });
      expect(() => guildCreateHandler({ id: 'g-err' })).not.toThrow();
      expect(logger.error).toHaveBeenCalled();
    });

    it('ready handler refreshes cache', async () => {
      await readyHandler();
      expect(mockClient.guilds.fetch).toHaveBeenCalled();
    });

  });

  describe('exports', () => {
    it('exports all expected functions', () => {
      expect(typeof discord.sendDM).toBe('function');
      expect(typeof discord.refreshCache).toBe('function');
      expect(typeof discord.shutdown).toBe('function');
      expect(typeof discord.getGuild).toBe('function');
    });

    it('no longer exports the GitHub contributor notifiers (#1026)', () => {
      for (const removed of [
        'assignContributorRole', 'notifyPRMerge', 'notifyBadgeEarned',
        'postGoodFirstIssue', 'postReleaseAnnouncement', 'postStarMilestone',
        'postToGitHubFeed', 'postWeeklyDigest', 'getRoles', 'getChannels',
      ]) {
        expect(discord[removed]).toBeUndefined();
      }
    });
  });

  describe('assertIntent (boot canary)', () => {
    // The whole point of the per-feature canary is to fail loud at
    // startup if an intent is removed; without a positive test for the
    // throw path, a future "let's downgrade to a warn" refactor would
    // silently degrade the assertion. These tests pin the contract.
    it('throws when the required intent is not in the intents list', () => {
      const intentsList = [1 /* Guilds */, 2 /* GuildMembers */];
      expect(() => discord.assertIntent(intentsList, 128, 'test feature'))
        .toThrow(/Missing required Discord intent for test feature/);
    });

    it('throws when the required intent is undefined (partially-mocked GatewayIntentBits)', () => {
      // In a test env where GatewayIntentBits.SomeIntent === undefined,
      // the assertion should still fail — undefined is treated as
      // "not declared" rather than "silently missing."
      expect(() => discord.assertIntent([1, 2, 128], undefined, 'feature X'))
        .toThrow(/Missing required Discord intent for feature X/);
    });

    it('does not throw when the required intent is present', () => {
      expect(() => discord.assertIntent([1, 2, 128], 128, 'test feature'))
        .not.toThrow();
    });

    // Pins the actual production canary description strings so a future
    // refactor of the `assertIntent(..., '<feature>')` arg at discord.js:
    // 38–39 fails this test instead of silently shipping a stale error
    // message at boot. Tracks the live `/qurl send + /qurl map` wording
    // (the subcommand was renamed from `/qurl file` to `/qurl send`).
    it('production assertIntent invocations surface the live recipient-resolution feature label', () => {
      // Construct the same intentsList shape src/discord.js declares (the
      // numeric values are mocked at the top of this file — see the
      // GatewayIntentBits mock). Drop GuildMembers (=2) to force the
      // canary; the throw message MUST embed the production label so an
      // oncall engineer reading boot logs gets the actionable feature.
      expect(() => discord.assertIntent([1 /* Guilds */], 2 /* GuildMembers */,
        '/qurl send + /qurl map recipient resolution (members.cache for role-mention expansion + members.fetch for selected-user backfill)'))
        .toThrow(/\/qurl send \+ \/qurl map recipient resolution/);
    });

    // GuildVoiceStates is the second load-bearing intent: its boot
    // canary at discord.js pins the voice-everyone resolution path.
    // A future PR that drops the intent without removing the feature
    // (channel.members for voice channels reads the voice-state cache)
    // must trip this assertion at module load.
    it('production assertIntent for GuildVoiceStates surfaces the voice-everyone resolution feature label', () => {
      expect(() => discord.assertIntent([1 /* Guilds */, 2 /* GuildMembers */], 128 /* GuildVoiceStates */,
        '/qurl send + /qurl map voice-channel-everyone resolution (channel.members for voice-connected snapshot in the confirm card button + <#voice> mention expansion in the recipients string)'))
        .toThrow(/voice-channel-everyone resolution/);
    });
  });

  describe('assertNoIntent (negative canary)', () => {
    // Pins the negative-intent guard: if a future PR silently adds back
    // MessageContent / GuildPresences / DirectMessages to the intents
    // array, the assertNoIntent invocations at discord.js fail loud at
    // boot. This test pins both branches (re-added intent throws;
    // absent intent doesn't). GuildVoiceStates was previously listed
    // here but was re-added (with paired assertIntent above) when the
    // voice-everyone path was restored.
    it('throws when the disallowed intent IS in the intents list', () => {
      const intentsList = [1 /* Guilds */, 2 /* GuildMembers */, 4096 /* DirectMessages */];
      expect(() => discord.assertNoIntent(intentsList, 4096, 'DirectMessages'))
        .toThrow(/Intent `DirectMessages` was re-added without justification/);
    });

    it('does not throw when the disallowed intent is absent', () => {
      const intentsList = [1 /* Guilds */, 2 /* GuildMembers */];
      expect(() => discord.assertNoIntent(intentsList, 4096, 'DirectMessages'))
        .not.toThrow();
    });

    it('does not throw when the bit is undefined (partially-mocked GatewayIntentBits)', () => {
      // Unknown intent name in a future Discord.js bump shouldn't crash
      // at boot just because the bit isn't in our mock.
      expect(() => discord.assertNoIntent([1, 2], undefined, 'FutureIntent'))
        .not.toThrow();
    });

    // Pin the documented asymmetry: assertIntent fails CLOSED on
    // undefined (silent missing intent is a silent feature break);
    // assertNoIntent fails OPEN on undefined (a bit that doesn't exist
    // in GatewayIntentBits can't have been re-added). Putting both
    // halves in one spec makes the contract grep-discoverable.
    it('assertIntent and assertNoIntent disagree on undefined-bit handling (documented asymmetry)', () => {
      expect(() => discord.assertIntent([1 /* Guilds */, 2 /* GuildMembers */], undefined, 'feature requiring missing intent'))
        .toThrow(/Missing required Discord intent/);
      expect(() => discord.assertNoIntent([1 /* Guilds */, 2 /* GuildMembers */], undefined, 'FutureIntent'))
        .not.toThrow();
    });
  });
});
