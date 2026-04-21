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

  it('multi-tenant: registers only /qurl globally (no guildId arg)', async () => {
    delete process.env.GUILD_ID;
    jest.resetModules();
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

    delete process.env.ENABLE_OPENNHP_FEATURES;
  });

  it('single-guild + ENABLE_OPENNHP_FEATURES unset: registers only customer-safe commands scoped to the guild', async () => {
    process.env.GUILD_ID = '123456789012345678';
    delete process.env.ENABLE_OPENNHP_FEATURES;
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
