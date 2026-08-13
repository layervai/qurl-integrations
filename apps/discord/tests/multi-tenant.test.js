// Tests for multi-tenant mode (activated when GUILD_ID env is unset or
// not a valid Discord snowflake). Covers the code paths in config.js,
// commands.js, and server.js that branch on it.
//
// OAUTH_STATE_SECRET is pinned globally in tests/setup-env.js — command
// dispatch reaches the shared state signer through the REAL config, and
// the signer's 32-char floor would otherwise make resolution depend on
// worker-level env leakage.

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

describe('multi-tenant mode — registerCommands registration scope', () => {
  // Explicit GUILD_ID reset before each test — it is the sole input
  // driving the mode, and a leftover value from a prior test would
  // silently flip the branch under inspection.
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

  // The post-refactor registerCommands signature is
  // `({rest, appId, guilds})`, so these tests assert the REST call
  // shape (rest.put with the right Route URL) rather than the legacy
  // `client.application.commands.set(data, guildId?)` shape.
  // Route URLs follow discord-api-types/v10's Routes module:
  //   global  → /applications/{appId}/commands
  //   guild   → /applications/{appId}/guilds/{guildId}/commands
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
    // Same surface in both modes — only the registration scope differs.
    expect(opts.body.map(c => c.name).sort()).toEqual(['qurl']);
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
  // A guild served by an older deploy keeps that deploy's guild-scoped
  // registrations in its slash-command cache — Discord's guild and
  // global namespaces don't purge each other on .set(). registerCommands
  // proactively clears them so users don't see dead commands in
  // autocomplete.
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

    // Two guilds with stale registrations, one empty guild (should not be purged).
    // Post-refactor: REST returns arrays of Command objects (was discord.js Map
    // before). The purge helper uses Array.length, so the mock returns arrays.
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

    // get called once per guild (3) — the per-guild fetch.
    expect(mockGet).toHaveBeenCalledTimes(3);
    // put called 3 times total: 2 purges (empty body, guild route) + 1 final global register.
    expect(mockPut).toHaveBeenCalledTimes(3);
    // Verify purge routes: applicationGuildCommands(appId, guildId), body=[].
    const purgeCalls = mockPut.mock.calls.filter(([, opts]) => Array.isArray(opts.body) && opts.body.length === 0);
    expect(purgeCalls).toHaveLength(2);
    const purgedRoutes = purgeCalls.map(c => c[0]).sort();
    expect(purgedRoutes).toEqual([
      '/applications/app-1/guilds/ga/commands',
      '/applications/app-1/guilds/gb/commands',
    ]);
    // Verify registration route: applicationCommands(appId), body=non-empty.
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
    // Purge (empty body) lands BEFORE the registration PUT, so the
    // fresh registration is never clobbered by its own purge.
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

    // Guild A's get failed but we still tried guild B.
    expect(mockGet).toHaveBeenCalledTimes(2);
    // Only guild B got purged (A failed before reaching put).
    const purgeCalls = mockPut.mock.calls.filter(([, opts]) => Array.isArray(opts.body) && opts.body.length === 0);
    expect(purgeCalls.map(c => c[0])).toEqual(['/applications/app-1/guilds/gb/commands']);
    // Final register still ran despite guild A's purge failure.
    const registerCalls = mockPut.mock.calls.filter(([, opts]) => Array.isArray(opts.body) && opts.body.length > 0);
    expect(registerCalls).toHaveLength(1);
  });
});

describe('handleCommand dispatch-time filter', () => {
  // Defense-in-depth: a guild served by a pre-#1026 deploy may still
  // list /link and friends in its slash-command picker, because
  // Discord's guild and global namespaces don't purge each other on
  // .set(). The filter at commands.js:handleCommand keeps those stale
  // registrations from dispatching into handlers that no longer exist,
  // and replies with a clear "no longer available" message instead of
  // letting Discord time out the interaction.
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

  it.each(['link', 'leaderboard', 'forcelink'])(
    'stale /%s interaction gets an ephemeral "no longer available" reply',
    async (commandName) => {
      process.env.GUILD_ID = '123456789012345678';

      // Mock dependencies that commands.js transitively pulls in
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
});

describe('server.js — the GitHub OAuth + webhook surfaces are gone (#1026)', () => {
  // Regression guard: /auth and /webhook were mounted behind the
  // removed mode gate. They must now 404 in EVERY mode, not just
  // multi-tenant — a re-mount would resurrect a surface whose backing
  // DDB tables no longer exist.
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

// Note: discord.js's `refreshCache()` multi-tenant early-return is
// covered directly in tests/discord.test.js, which already mocks the
// Discord client at module-import time.
