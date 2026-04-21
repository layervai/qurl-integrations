// Tests for multi-tenant mode (activated when GUILD_ID env is unset or
// not a valid Discord snowflake). Covers the code paths added to
// config.js, commands.js, discord.js, and server.js.

describe('multi-tenant mode — config.js GUILD_ID normalization', () => {
  // Each case re-requires config fresh after setting process.env, because
  // config.js snapshots process.env.GUILD_ID at module import time.
  function loadConfig(rawGuildId) {
    jest.resetModules();
    if (rawGuildId === undefined) {
      delete process.env.GUILD_ID;
    } else {
      process.env.GUILD_ID = rawGuildId;
    }
    return require('../src/config');
  }

  afterAll(() => {
    delete process.env.GUILD_ID;
    jest.resetModules();
  });

  it('accepts an 18-digit snowflake', () => {
    const cfg = loadConfig('123456789012345678');
    expect(cfg.GUILD_ID).toBe('123456789012345678');
    expect(cfg.isMultiTenant).toBe(false);
  });

  it('accepts a 17-digit snowflake (lower bound)', () => {
    const cfg = loadConfig('12345678901234567');
    expect(cfg.GUILD_ID).toBe('12345678901234567');
    expect(cfg.isMultiTenant).toBe(false);
  });

  it('accepts a 20-digit snowflake (upper bound)', () => {
    const cfg = loadConfig('12345678901234567890');
    expect(cfg.GUILD_ID).toBe('12345678901234567890');
    expect(cfg.isMultiTenant).toBe(false);
  });

  it('trims whitespace around a valid snowflake', () => {
    const cfg = loadConfig('  123456789012345678  ');
    expect(cfg.GUILD_ID).toBe('123456789012345678');
    expect(cfg.isMultiTenant).toBe(false);
  });

  it('normalizes "PLACEHOLDER" (SSM default) to null → multi-tenant', () => {
    const cfg = loadConfig('PLACEHOLDER');
    expect(cfg.GUILD_ID).toBeNull();
    expect(cfg.isMultiTenant).toBe(true);
  });

  it('normalizes whitespace-only to null → multi-tenant', () => {
    const cfg = loadConfig('   ');
    expect(cfg.GUILD_ID).toBeNull();
    expect(cfg.isMultiTenant).toBe(true);
  });

  it('normalizes 16-digit string (too short) to null → multi-tenant', () => {
    const cfg = loadConfig('1234567890123456');
    expect(cfg.GUILD_ID).toBeNull();
    expect(cfg.isMultiTenant).toBe(true);
  });

  it('normalizes 21-digit string (too long) to null → multi-tenant', () => {
    const cfg = loadConfig('123456789012345678901');
    expect(cfg.GUILD_ID).toBeNull();
    expect(cfg.isMultiTenant).toBe(true);
  });

  it('normalizes "guild-1" (non-numeric) to null → multi-tenant', () => {
    const cfg = loadConfig('guild-1');
    expect(cfg.GUILD_ID).toBeNull();
    expect(cfg.isMultiTenant).toBe(true);
  });

  it('unset GUILD_ID → null → multi-tenant', () => {
    const cfg = loadConfig(undefined);
    expect(cfg.GUILD_ID).toBeNull();
    expect(cfg.isMultiTenant).toBe(true);
  });
});

describe('multi-tenant mode — registerCommands command filtering', () => {
  // Explicit reset on BOTH the env-var pair (GUILD_ID + ENABLE_OPENNHP_FEATURES)
  // before each test — both inputs now drive the mode, and a leftover value
  // from a prior test would silently flip the branch under inspection.
  let originalGuildId;
  let originalOpenNHPFlag;
  beforeAll(() => {
    originalGuildId = process.env.GUILD_ID;
    originalOpenNHPFlag = process.env.ENABLE_OPENNHP_FEATURES;
  });
  beforeEach(() => {
    delete process.env.GUILD_ID;
    delete process.env.ENABLE_OPENNHP_FEATURES;
    jest.resetModules();
  });
  afterAll(() => {
    if (originalGuildId === undefined) {
      delete process.env.GUILD_ID;
    } else {
      process.env.GUILD_ID = originalGuildId;
    }
    if (originalOpenNHPFlag === undefined) {
      delete process.env.ENABLE_OPENNHP_FEATURES;
    } else {
      process.env.ENABLE_OPENNHP_FEATURES = originalOpenNHPFlag;
    }
    jest.resetModules();
  });

  it('multi-tenant: registers only /qurl globally (no guildId arg)', async () => {
    const commandsModule = require('../src/commands');

    const mockSet = jest.fn().mockResolvedValue(undefined);
    const mockClient = { application: { commands: { set: mockSet } } };
    await commandsModule.registerCommands(mockClient);

    expect(mockSet).toHaveBeenCalledTimes(1);
    const [data, guildArg] = mockSet.mock.calls[0];
    expect(guildArg).toBeUndefined();
    // Only /qurl is customer-safe
    expect(data.map(c => c.name).sort()).toEqual(['qurl']);
  });

  it('single-guild + ENABLE_OPENNHP_FEATURES=true: registers ALL commands scoped to the guild', async () => {
    process.env.GUILD_ID = '123456789012345678';
    process.env.ENABLE_OPENNHP_FEATURES = 'true';
    jest.resetModules();
    const commandsModule = require('../src/commands');

    const mockSet = jest.fn().mockResolvedValue(undefined);
    const mockClient = { application: { commands: { set: mockSet } } };
    await commandsModule.registerCommands(mockClient);

    expect(mockSet).toHaveBeenCalledTimes(1);
    const [data, guildArg] = mockSet.mock.calls[0];
    expect(guildArg).toBe('123456789012345678');
    expect(data.length).toBeGreaterThan(1);
    expect(data.map(c => c.name)).toContain('qurl');
    // OpenNHP commands register alongside /qurl in the OpenNHP guild
    expect(data.map(c => c.name)).toContain('link');
  });

  it('single-guild + ENABLE_OPENNHP_FEATURES unset: registers only customer-safe commands scoped to the guild', async () => {
    process.env.GUILD_ID = '123456789012345678';
    jest.resetModules();
    const commandsModule = require('../src/commands');

    const mockSet = jest.fn().mockResolvedValue(undefined);
    const mockClient = { application: { commands: { set: mockSet } } };
    await commandsModule.registerCommands(mockClient);

    expect(mockSet).toHaveBeenCalledTimes(1);
    const [data, guildArg] = mockSet.mock.calls[0];
    expect(guildArg).toBe('123456789012345678');
    // Only /qurl, even in single-guild mode, because OpenNHP features are off
    expect(data.map(c => c.name).sort()).toEqual(['qurl']);
    expect(data.map(c => c.name)).not.toContain('link');
    expect(data.map(c => c.name)).not.toContain('leaderboard');
    expect(data.map(c => c.name)).not.toContain('forcelink');
  });
});

describe('handleCommand dispatch-time filter', () => {
  // This is the defense-in-depth branch: stale guild-scoped command
  // registrations survive a mode flip (Discord's guild and global
  // command namespaces don't purge each other on .set()). The filter
  // at commands.js:handleCommand prevents those stale handlers from
  // dispatching to code paths that assume populated OpenNHP state,
  // and replies with a clear "no longer available" message instead
  // of letting Discord time out the interaction.
  let originalGuildId;
  let originalFlag;
  beforeAll(() => {
    originalGuildId = process.env.GUILD_ID;
    originalFlag = process.env.ENABLE_OPENNHP_FEATURES;
  });
  afterAll(() => {
    if (originalGuildId === undefined) delete process.env.GUILD_ID; else process.env.GUILD_ID = originalGuildId;
    if (originalFlag === undefined) delete process.env.ENABLE_OPENNHP_FEATURES; else process.env.ENABLE_OPENNHP_FEATURES = originalFlag;
    jest.resetModules();
  });
  beforeEach(() => {
    jest.resetModules();
  });

  it('non-OpenNHP mode: stale /link interaction gets an ephemeral "no longer available" reply', async () => {
    // GUILD_ID set (single-guild mode) but flag unset → isOpenNHPActive=false.
    // /link is an OpenNHP-only command, so a stale guild-scoped
    // registration from a prior deploy must not dispatch.
    process.env.GUILD_ID = '123456789012345678';
    delete process.env.ENABLE_OPENNHP_FEATURES;

    // Mock dependencies that commands.js transitively pulls in
    jest.doMock('../src/database', () => ({
      getLinkByDiscord: jest.fn(), getLinkByGithub: jest.fn(),
      createPendingLink: jest.fn(), getContributions: jest.fn(() => []),
      getBadges: jest.fn(() => []), getStats: jest.fn(() => ({})),
      getStreak: jest.fn(() => null), getTopContributors: jest.fn(() => []),
      recordQURLSend: jest.fn(), getRecentSends: jest.fn(() => []),
      getSendResourceIds: jest.fn(() => []), getSendConfig: jest.fn(),
      saveSendConfig: jest.fn(), deleteLink: jest.fn(), forceLink: jest.fn(),
    }));
    jest.doMock('../src/discord', () => ({
      sendDM: jest.fn(), assignContributorRole: jest.fn(),
      notifyBadgeEarned: jest.fn(), getVoiceChannelMembers: jest.fn(),
      getTextChannelMembers: jest.fn(), client: { user: { id: 'bot' } },
    }));
    jest.doMock('../src/qurl', () => ({ mintLink: jest.fn() }));
    jest.doMock('../src/connector', () => ({ uploadAttachment: jest.fn() }));
    jest.doMock('../src/places', () => ({ autocomplete: jest.fn() }));

    const { handleCommand } = require('../src/commands');

    const reply = jest.fn().mockResolvedValue(undefined);
    const interaction = {
      isAutocomplete: () => false,
      isChatInputCommand: () => true,
      commandName: 'link',
      user: { id: 'u1' },
      reply,
    };

    await handleCommand(interaction);

    expect(reply).toHaveBeenCalledWith(expect.objectContaining({
      content: expect.stringContaining('no longer available'),
      ephemeral: true,
    }));
  });

  it('OpenNHP mode: /link dispatches normally (no "no longer available" reply)', async () => {
    process.env.GUILD_ID = '123456789012345678';
    process.env.ENABLE_OPENNHP_FEATURES = 'true';

    jest.doMock('../src/database', () => ({
      getLinkByDiscord: jest.fn().mockReturnValue({ github_username: 'existing' }),
      createPendingLink: jest.fn(),
      getLinkByGithub: jest.fn(), getContributions: jest.fn(() => []),
      getBadges: jest.fn(() => []), getStats: jest.fn(() => ({})),
      getStreak: jest.fn(() => null), getTopContributors: jest.fn(() => []),
      recordQURLSend: jest.fn(), getRecentSends: jest.fn(() => []),
      getSendResourceIds: jest.fn(() => []), getSendConfig: jest.fn(),
      saveSendConfig: jest.fn(), deleteLink: jest.fn(), forceLink: jest.fn(),
    }));
    jest.doMock('../src/discord', () => ({
      sendDM: jest.fn(), assignContributorRole: jest.fn(),
      notifyBadgeEarned: jest.fn(), getVoiceChannelMembers: jest.fn(),
      getTextChannelMembers: jest.fn(), client: { user: { id: 'bot' } },
    }));
    jest.doMock('../src/qurl', () => ({ mintLink: jest.fn() }));
    jest.doMock('../src/connector', () => ({ uploadAttachment: jest.fn() }));
    jest.doMock('../src/places', () => ({ autocomplete: jest.fn() }));

    const { handleCommand } = require('../src/commands');

    const reply = jest.fn().mockResolvedValue(undefined);
    const interaction = {
      isAutocomplete: () => false,
      isChatInputCommand: () => true,
      commandName: 'link',
      user: { id: 'u1' },
      reply,
    };

    await handleCommand(interaction);

    // The real /link handler ran — it called reply with something
    // OTHER than the "no longer available" string. (In the already-
    // linked path it replies with a message about the existing link.)
    const replyArgs = reply.mock.calls[0]?.[0];
    expect(replyArgs).toBeDefined();
    const replyContent = typeof replyArgs === 'string' ? replyArgs : (replyArgs.content || JSON.stringify(replyArgs));
    expect(replyContent).not.toContain('no longer available');
  });
});

describe('multi-tenant mode — server.js route mounting', () => {
  let originalGuildId;
  beforeAll(() => {
    originalGuildId = process.env.GUILD_ID;
  });
  afterAll(() => {
    if (originalGuildId === undefined) {
      delete process.env.GUILD_ID;
    } else {
      process.env.GUILD_ID = originalGuildId;
    }
    jest.resetModules();
  });

  it('multi-tenant: /auth/github returns 404 (route not mounted)', async () => {
    delete process.env.GUILD_ID;
    jest.resetModules();
    // Mock discord + database to avoid side effects in server import
    jest.doMock('../src/discord', () => ({
      assignContributorRole: jest.fn(),
      notifyPRMerge: jest.fn(),
      sendDM: jest.fn(),
    }));
    jest.doMock('../src/database', () => ({
      getPendingLink: jest.fn(),
      getStats: jest.fn(() => ({ linkedUsers: 0, totalContributions: 0, uniqueContributors: 0, byRepo: [] })),
    }));
    const request = require('supertest');
    const { app } = require('../src/server');
    const res = await request(app).get('/auth/github?state=whatever');
    expect(res.status).toBe(404);
  });

  it('multi-tenant: /webhook/github returns 404 (route not mounted)', async () => {
    delete process.env.GUILD_ID;
    jest.resetModules();
    jest.doMock('../src/discord', () => ({
      notifyPRMerge: jest.fn(),
      postStarMilestone: jest.fn(),
    }));
    jest.doMock('../src/database', () => ({
      getStats: jest.fn(() => ({ linkedUsers: 0, totalContributions: 0, uniqueContributors: 0, byRepo: [] })),
    }));
    const request = require('supertest');
    const { app } = require('../src/server');
    const res = await request(app).post('/webhook/github').send({});
    expect(res.status).toBe(404);
  });
});

// Note: discord.js's `refreshCache()` early-return is covered indirectly by
// the server.js route-gating tests above — if routes are unmounted in
// multi-tenant mode, nothing webhook- or OAuth-driven can reach
// `refreshCache()` in the first place. Testing the early-return directly
// requires mocking out the Discord client at module-import time, which
// couples the test to discord.js internals; deferred to a follow-up.
