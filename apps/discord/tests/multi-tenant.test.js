
describe('multi-tenant mode — config.js GUILD_ID normalization', () => {
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
describe('multi-tenant mode — registerCommands registration scope', () => {
  let originalGuildId;
  beforeAll(() => {
    originalGuildId = process.env.GUILD_ID;
  });
  beforeEach(() => {
    delete process.env.GUILD_ID;
    jest.resetModules();
  });
  afterAll(() => {
    if (originalGuildId === undefined) {
      delete process.env.GUILD_ID;
    } else {
      process.env.GUILD_ID = originalGuildId;
    }
    jest.resetModules();
  });

  it('multi-tenant: registers /qurl on the global commands endpoint', async () => {
    const commandsModule = require('../src/commands');

    const mockPut = jest.fn().mockResolvedValue(undefined);
    const mockGet = jest.fn().mockResolvedValue([]);
    const rest = { put: mockPut, get: mockGet };
    await commandsModule.registerCommands({
      rest,
      appId: 'app-123',
      guilds: new Map(),
    });

    expect(mockPut).toHaveBeenCalledTimes(1);
    const [route, opts] = mockPut.mock.calls[0];
    expect(route).toBe('/applications/app-123/commands');
    expect(opts.body.map(c => c.name).sort()).toEqual(['qurl']);
    expect(opts.body).toHaveLength(1);
    expect(opts.body[0]).toMatchObject({
      name: 'qurl',
      integration_types: [0],
      contexts: [0],
    });
    expect(opts.body[0]).not.toHaveProperty('dm_permission');
  });

  it('single-guild: registers the same command set on the guild endpoint', async () => {
    process.env.GUILD_ID = '123456789012345678';
    jest.resetModules();
    const commandsModule = require('../src/commands');

    const mockPut = jest.fn().mockResolvedValue(undefined);
    const mockGet = jest.fn().mockResolvedValue([]);
    const rest = { put: mockPut, get: mockGet };
    await commandsModule.registerCommands({
      rest,
      appId: 'app-123',
      guilds: new Map(),
    });

    expect(mockPut).toHaveBeenCalledTimes(1);
    const [route, opts] = mockPut.mock.calls[0];
    expect(route).toBe('/applications/app-123/guilds/123456789012345678/commands');
    expect(opts.body.map(c => c.name).sort()).toEqual(['qurl']);
    expect(opts.body[0]).not.toHaveProperty('integration_types');
    expect(opts.body[0]).not.toHaveProperty('contexts');
    expect(opts.body[0]).not.toHaveProperty('dm_permission');
  });

  it('ships no GitHub account-linking or contributor commands in either mode (#1026)', async () => {
    const { commands } = require('../src/commands');
    const names = commands.map(c => c.data.name);
    expect(names).toEqual(['qurl']);
    for (const removed of [
      'link', 'unlink', 'whois', 'contributions', 'stats',
      'leaderboard', 'forcelink', 'bulklink', 'unlinked',
      'backfill-milestones',
    ]) {
      expect(names).not.toContain(removed);
    }
  });
});

describe('registerCommands stale-command purge (issue #86)', () => {
  let originalGuildId;
  beforeAll(() => {
    originalGuildId = process.env.GUILD_ID;
  });
  afterAll(() => {
    if (originalGuildId === undefined) delete process.env.GUILD_ID; else process.env.GUILD_ID = originalGuildId;
    jest.resetModules();
  });
  beforeEach(() => { jest.resetModules(); });

  it('multi-tenant mode: purges stale guild-scoped registrations from every guild the bot is in', async () => {
    delete process.env.GUILD_ID;
    const commandsModule = require('../src/commands');

    const mockGet = jest.fn()
      .mockResolvedValueOnce([{ id: 'cmd-1' }, { id: 'cmd-2' }, { id: 'cmd-3' }]) // guild A: 3 stale
      .mockResolvedValueOnce([{ id: 'cmd-4' }])                                    // guild B: 1 stale
      .mockResolvedValueOnce([]);                                                  // guild C: empty — skip
    const mockPut = jest.fn().mockResolvedValue(undefined);
    const rest = { get: mockGet, put: mockPut };

    await commandsModule.registerCommands({
      rest,
      appId: 'app-1',
      guilds: new Map([['ga', 'A'], ['gb', 'B'], ['gc', 'C']]),
    });

    expect(mockGet).toHaveBeenCalledTimes(3);
    expect(mockPut).toHaveBeenCalledTimes(3);
    const purgeCalls = mockPut.mock.calls.filter(([, opts]) => Array.isArray(opts.body) && opts.body.length === 0);
    expect(purgeCalls).toHaveLength(2);
    const purgedRoutes = purgeCalls.map(c => c[0]).sort();
    expect(purgedRoutes).toEqual([
      '/applications/app-1/guilds/ga/commands',
      '/applications/app-1/guilds/gb/commands',
    ]);
    const registerCalls = mockPut.mock.calls.filter(([, opts]) => Array.isArray(opts.body) && opts.body.length > 0);
    expect(registerCalls).toHaveLength(1);
    expect(registerCalls[0][0]).toBe('/applications/app-1/commands');
  });

  it('single-guild mode: purges first, then registers guild-scoped (strictly sequential, no race)', async () => {
    process.env.GUILD_ID = '123456789012345678';
    const commandsModule = require('../src/commands');

    const mockGet = jest.fn().mockResolvedValue([{ id: 'stale-1' }]);
    const mockPut = jest.fn().mockResolvedValue(undefined);
    const rest = { get: mockGet, put: mockPut };

    await commandsModule.registerCommands({
      rest,
      appId: 'app-1',
      guilds: new Map([['123456789012345678', 'A']]),
    });

    expect(mockGet).toHaveBeenCalledTimes(1);
    expect(mockPut).toHaveBeenCalledTimes(2);
    expect(mockPut.mock.calls[0][1].body).toEqual([]);
    expect(mockPut.mock.calls[1][0]).toBe('/applications/app-1/guilds/123456789012345678/commands');
    expect(mockPut.mock.calls[1][1].body.length).toBeGreaterThan(0);
  });

  it('purge failure in one guild does not block registration', async () => {
    delete process.env.GUILD_ID;
    const commandsModule = require('../src/commands');

    const mockGet = jest.fn()
      .mockRejectedValueOnce(new Error('Missing Access'))     // guild A: can't enumerate
      .mockResolvedValueOnce([{ id: 'cmd-1' }]);              // guild B: succeeds
    const mockPut = jest.fn().mockResolvedValue(undefined);
    const rest = { get: mockGet, put: mockPut };

    await commandsModule.registerCommands({
      rest,
      appId: 'app-1',
      guilds: new Map([['ga', 'A'], ['gb', 'B']]),
    });

    expect(mockGet).toHaveBeenCalledTimes(2);
    const purgeCalls = mockPut.mock.calls.filter(([, opts]) => Array.isArray(opts.body) && opts.body.length === 0);
    expect(purgeCalls.map(c => c[0])).toEqual(['/applications/app-1/guilds/gb/commands']);
    const registerCalls = mockPut.mock.calls.filter(([, opts]) => Array.isArray(opts.body) && opts.body.length > 0);
    expect(registerCalls).toHaveLength(1);
  });
});

describe('handleCommand dispatch-time filter', () => {
  let originalGuildId;
  beforeAll(() => {
    originalGuildId = process.env.GUILD_ID;
  });
  afterAll(() => {
    if (originalGuildId === undefined) delete process.env.GUILD_ID; else process.env.GUILD_ID = originalGuildId;
    jest.resetModules();
  });
  beforeEach(() => {
    jest.resetModules();
  });

  it.each([
    ['link', undefined],
    ['leaderboard', { 0: '123456789012345678' }],
    ['forcelink', { 0: '123456789012345678', 1: 'user-1' }],
  ])(
    'stale /%s interaction gets an ephemeral "no longer available" reply',
    async (commandName, authorizingIntegrationOwners) => {
      process.env.GUILD_ID = '123456789012345678';

      jest.doMock('../src/store', () => ({
        getStats: jest.fn(() => ({})),
        recordQURLSend: jest.fn(), getRecentSends: jest.fn(() => []),
        getSendResourceIds: jest.fn(() => []), getSendConfig: jest.fn(),
        saveSendConfig: jest.fn(),
      }));
      jest.doMock('../src/discord', () => ({
        sendDM: jest.fn(),
        client: { user: { id: 'bot' } },
      }));
      jest.doMock('../src/qurl', () => ({ mintLink: jest.fn() }));
      jest.doMock('../src/connector', () => ({ uploadAttachment: jest.fn() }));
      jest.doMock('../src/places', () => ({ autocomplete: jest.fn() }));

      const { handleCommand } = require('../src/commands');

      const reply = jest.fn().mockResolvedValue(undefined);
      const interaction = {
        isAutocomplete: () => false,
        isChatInputCommand: () => true,
        commandName,
        guildId: '123456789012345678',
        authorizingIntegrationOwners,
        user: { id: 'u1' },
        reply,
      };

      await handleCommand(interaction);

      expect(reply).toHaveBeenCalledWith(expect.objectContaining({
        content: expect.stringContaining('no longer available'),
        ephemeral: true,
      }));
    },
  );

  it.each([
    ['DM', { guildId: null, user: { id: 'user-1' } }],
    ['user install inside a guild', {
      guildId: 'guild-1',
      authorizingIntegrationOwners: { 1: 'user-1' },
      user: { id: 'user-1' },
    }],
  ])('rejects a stale %s /qurl interaction before subcommand dispatch', async (_label, context) => {
    jest.doMock('../src/store', () => ({}));
    jest.doMock('../src/discord', () => ({
      sendDM: jest.fn(),
      client: { user: { id: 'bot' } },
    }));

    const { handleCommand } = require('../src/commands');
    const reply = jest.fn().mockResolvedValue(undefined);
    const interaction = {
      isAutocomplete: () => false,
      isChatInputCommand: () => true,
      commandName: 'qurl',
      options: { getSubcommand: jest.fn(() => 'help') },
      reply,
      ...context,
    };

    await handleCommand(interaction);

    expect(reply).toHaveBeenCalledWith({
      content: 'qURL only works inside a server where it is installed, not in DMs or from a user install.',
      ephemeral: true,
    });
    expect(interaction.options.getSubcommand).not.toHaveBeenCalled();
  });
});

describe('server.js — the GitHub OAuth + webhook surfaces are gone (#1026)', () => {
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

  function mockServerDeps() {
    jest.doMock('../src/discord', () => ({ sendDM: jest.fn() }));
    jest.doMock('../src/store', () => ({
      getStats: jest.fn(() => ({ configuredGuilds: 0, totalSends: 0 })),
      healthCheck: jest.fn(() => ({ ok: true })),
    }));
  }

  it.each([
    ['multi-tenant', undefined],
    ['single-guild', '123456789012345678'],
  ])('%s: /auth/github returns 404 (route not mounted)', async (_label, guildId) => {
    if (guildId === undefined) delete process.env.GUILD_ID;
    else process.env.GUILD_ID = guildId;
    jest.resetModules();
    mockServerDeps();
    const request = require('supertest');
    const { app } = require('../src/server');
    const res = await request(app).get('/auth/github?state=whatever');
    expect(res.status).toBe(404);
  });

  it.each([
    ['multi-tenant', undefined],
    ['single-guild', '123456789012345678'],
  ])('%s: /webhook/github returns 404 (route not mounted)', async (_label, guildId) => {
    if (guildId === undefined) delete process.env.GUILD_ID;
    else process.env.GUILD_ID = guildId;
    jest.resetModules();
    mockServerDeps();
    const request = require('supertest');
    const { app } = require('../src/server');
    const res = await request(app).post('/webhook/github').send({});
    expect(res.status).toBe(404);
  });
});
