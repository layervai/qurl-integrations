
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

describe('bounded inactive command-scope cleanup (#84/#94)', () => {
  const current = '123456789012345678';
  const previous = '223456789012345678';
  const globalRoute = '/applications/app-1/commands';
  const guildRoute = id => `/applications/app-1/guilds/${id}/commands`;
  let originalGuildId;
  let originalPrevious;
  beforeAll(() => {
    originalGuildId = process.env.GUILD_ID;
    originalPrevious = process.env.PREVIOUS_COMMAND_SCOPE;
  });
  afterAll(() => {
    if (originalGuildId === undefined) delete process.env.GUILD_ID; else process.env.GUILD_ID = originalGuildId;
    if (originalPrevious === undefined) delete process.env.PREVIOUS_COMMAND_SCOPE; else process.env.PREVIOUS_COMMAND_SCOPE = originalPrevious;
    jest.resetModules();
  });
  function load(active, old) {
    if (active === undefined) delete process.env.GUILD_ID; else process.env.GUILD_ID = active;
    if (old === undefined) delete process.env.PREVIOUS_COMMAND_SCOPE; else process.env.PREVIOUS_COMMAND_SCOPE = old;
    jest.resetModules();
    return require('../src/commands');
  }
  function api() {
    const state = new Map([
      [globalRoute, [{ name: 'old-global' }]],
      [guildRoute(previous), [{ name: 'old-guild' }]],
    ]);
    const rest = {
      get: jest.fn(async route => state.get(route) || []),
      put: jest.fn(async (route, { body }) => { state.set(route, body); }),
    };
    return { state, rest };
  }

  it.each([0, 10_000])('steady multi-tenant boot ignores a %i-guild cache and makes no cleanup calls', async size => {
    const commands = load(undefined, undefined);
    const { rest } = api();
    // Deliberately pass the removed cache argument to guard against reintroducing a scan.
    const guilds = new Map(Array.from({ length: size }, (_, i) => [`guild-${i}`, 'name']));
    await commands.registerCommands({ rest, appId: 'app-1', guilds });
    expect(rest.get).not.toHaveBeenCalled();
    expect(rest.put).toHaveBeenCalledTimes(1);
    expect(rest.put.mock.calls[0][0]).toBe(globalRoute);
  });

  it('single to multi: registers globally before clearing only the previous guild, without a cache', async () => {
    const commands = load(undefined, previous);
    const { rest, state } = api();
    await commands.registerCommands({ rest, appId: 'app-1' });
    expect(rest.get.mock.calls).toEqual([[guildRoute(previous)]]);
    expect(rest.put.mock.calls.map(([route]) => route)).toEqual([globalRoute, guildRoute(previous)]);
    expect(state.get(globalRoute).map(command => command.name)).toEqual(['qurl']);
    expect(state.get(guildRoute(previous))).toEqual([]);
  });

  it('multi to single: explicitly clears the old global scope after target registration', async () => {
    const commands = load(current, 'global');
    const { rest, state } = api();
    await commands.registerCommands({ rest, appId: 'app-1' });
    expect(rest.put.mock.calls.map(([route]) => route)).toEqual([guildRoute(current), globalRoute]);
    expect(state.get(guildRoute(current)).map(command => command.name)).toEqual(['qurl']);
    expect(state.get(globalRoute)).toEqual([]);
    expect(state.get(guildRoute(previous))).toHaveLength(1);
  });

  it('single to another: clears only the explicit old guild and preserves global commands', async () => {
    const commands = load(current, previous);
    const { rest, state } = api();
    await commands.registerCommands({ rest, appId: 'app-1' });
    expect(rest.get.mock.calls).toEqual([[guildRoute(previous)]]);
    expect(state.get(globalRoute)[0].name).toBe('old-global');
    expect(state.get(guildRoute(previous))).toEqual([]);
    expect(state.get(guildRoute(current)).map(command => command.name)).toEqual(['qurl']);
  });

  it('never purges the active guild when the previous value equals it', async () => {
    const commands = load(current, current);
    const { rest, state } = api();
    await commands.registerCommands({ rest, appId: 'app-1' });
    expect(rest.get).not.toHaveBeenCalled();
    expect(state.get(guildRoute(current)).map(command => command.name)).toEqual(['qurl']);
  });

  it('keeps old namespaces when active registration fails', async () => {
    const commands = load(current, previous);
    const { rest, state } = api();
    rest.put.mockRejectedValueOnce(new Error('registration unavailable'));
    await commands.registerCommands({ rest, appId: 'app-1' });
    expect(rest.get).not.toHaveBeenCalled();
    expect(state.get(globalRoute)[0].name).toBe('old-global');
    expect(state.get(guildRoute(previous))[0].name).toBe('old-guild');
  });

  it('steady single-guild boot never touches the global namespace', async () => {
    const commands = load(current, undefined);
    const { rest, state } = api();
    await commands.registerCommands({ rest, appId: 'app-1' });
    expect(rest.get).not.toHaveBeenCalled();
    expect(state.get(globalRoute)[0].name).toBe('old-global');
    expect(rest.put).toHaveBeenCalledTimes(1);
  });

  it('never clears the active global namespace when previous scope is global', async () => {
    const commands = load(undefined, 'global');
    const { rest, state } = api();
    await commands.registerCommands({ rest, appId: 'app-1' });
    expect(rest.get).not.toHaveBeenCalled();
    expect(state.get(globalRoute).map(command => command.name)).toEqual(['qurl']);
  });

  it('does not treat a malformed namespace response as empty', async () => {
    const commands = load(current, 'global');
    const { rest, state } = api();
    rest.get.mockResolvedValueOnce({ unexpected: true });
    const logger = require('../src/logger');
    const warning = jest.spyOn(logger, 'warn').mockImplementation(() => {});
    try {
      await commands.registerCommands({ rest, appId: 'app-1' });
      expect(warning).toHaveBeenCalledWith('Could not purge stale command namespace', {
        scope: 'global', error: 'Command namespace response must be an array',
      });
      expect(state.get(globalRoute)[0].name).toBe('old-global');
    } finally { warning.mockRestore(); }
  });

  it('trims a valid previous guild ID', () => {
    load(undefined, ` ${previous} `);
    expect(require('../src/config').PREVIOUS_COMMAND_SCOPE).toBe(previous);
  });

  it.each(['PLACEHOLDER', 'guild-1', '123', '123456789012345678901'])('rejects invalid previous guild %p before registration', value => {
    expect(() => load(undefined, value)).toThrow('PREVIOUS_COMMAND_SCOPE must be global or a Discord snowflake');
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
