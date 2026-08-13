/**
 * Unit tests for src/http-only-init.js — the boot wiring that
 * lets `PROCESS_ROLE=http` replicas serve OAuth + webhook traffic
 * without a Gateway login.
 *
 * The fix this guards against: pre-fix, http-only mode skipped
 * client.login() (correctly — only one Gateway connection per
 * bot token) but never seeded client.rest with the token, so the
 * very first sendDM / channel.send / member.roles.add returned
 * 401. We assert here that initHttpOnly() does both side effects
 * login() would normally do (token + cache refresh).
 *
 * This suite also pins the ABSENCE of a periodic refresh. The
 * module used to run a 10-minute REST refreshCache() to make up for
 * the Gateway roleDelete/channelDelete events http-only replicas
 * never see; #1051 deleted those handlers and the roles/channels
 * cache they invalidated, leaving nothing that reads the cached
 * guild handle on a schedule. The no-timer test below fails if a
 * future change reintroduces one without a reader to justify it.
 */

const { initHttpOnly } = require('../src/http-only-init');

// Avoid constructing a real discord.js ClientUser in tests — it
// walks the User -> Base inheritance chain and pokes the Client
// for things like options.makeCache. The mock seeds the same
// `client.user.{id,username}` shape that initHttpOnly's logger
// reads + the dispatch-reconstruction path consumes.
jest.mock('discord.js', () => ({
  ClientUser: jest.fn().mockImplementation((_client, data) => ({
    id: data.id,
    username: data.username,
    bot: true,
  })),
}));

function makeClient() {
  return {
    rest: {
      setToken: jest.fn(),
      get: jest.fn().mockResolvedValue({
        id: 'bot-id-123',
        username: 'qurl-bot-test',
        bot: true,
      }),
    },
  };
}

function makeLogger() {
  return { info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn() };
}

describe('initHttpOnly', () => {
  it('seeds client.user from REST GET /users/@me (worker-tier dispatch reconstruction depends on it)', async () => {
    // Pre-PR-#444 bug: discord.js's Action.getChannel reads
    // `client.user.id` to filter the bot from the interaction's
    // recipient list. http-only mode skips login() (gateway-token
    // singleton), so without this REST seed, client.user stays null
    // and every replayed INTERACTION_CREATE throws
    // "Cannot read properties of null (reading 'id')". This test
    // pins the seed so a future refactor that drops it fails CI
    // instead of breaking every interaction in production.
    const client = makeClient();
    const refreshCache = jest.fn().mockResolvedValue(undefined);
    const logger = makeLogger();
    const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '123' };

    await initHttpOnly({ client, config, refreshCache, logger });

    // Routes.user('@me') URI-encodes @ to %40 — assert via includes
    // so the test doesn't break if upstream Routes ever switches
    // encoding strategy.
    expect(client.rest.get).toHaveBeenCalledTimes(1);
    expect(client.rest.get.mock.calls[0][0]).toMatch(/^\/users\/(@|%40)me$/);
    expect(client.user).toEqual(expect.objectContaining({
      id: 'bot-id-123',
      username: 'qurl-bot-test',
    }));
  });

  it('sets the bot token on client.rest and warms the cache (single-guild)', async () => {
    const client = makeClient();
    const refreshCache = jest.fn().mockResolvedValue(undefined);
    const logger = makeLogger();
    const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '123' };

    await initHttpOnly({ client, config, refreshCache, logger });

    expect(client.rest.setToken).toHaveBeenCalledTimes(1);
    expect(client.rest.setToken).toHaveBeenCalledWith('tok-abc');
    expect(refreshCache).toHaveBeenCalledTimes(1);
  });

  it('seeds the token first, then refreshes (refreshCache uses REST so token must already be set)', async () => {
    const client = makeClient();
    const callOrder = [];
    client.rest.setToken.mockImplementation(() => callOrder.push('setToken'));
    const refreshCache = jest.fn().mockImplementation(async () => {
      callOrder.push('refreshCache');
    });
    const logger = makeLogger();
    const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '123' };

    await initHttpOnly({ client, config, refreshCache, logger });

    expect(callOrder).toEqual(['setToken', 'refreshCache']);
  });

  it('skips refreshCache when GUILD_ID is unset (multi-tenant mode)', async () => {
    const client = makeClient();
    const refreshCache = jest.fn().mockResolvedValue(undefined);
    const logger = makeLogger();
    const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: null };

    await initHttpOnly({ client, config, refreshCache, logger });

    expect(client.rest.setToken).toHaveBeenCalledWith('tok-abc');
    expect(refreshCache).not.toHaveBeenCalled();
    expect(logger.warn).not.toHaveBeenCalled();
  });

  it('skips refreshCache when GUILD_ID is empty string', async () => {
    const client = makeClient();
    const refreshCache = jest.fn().mockResolvedValue(undefined);
    const logger = makeLogger();
    const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '' };

    await initHttpOnly({ client, config, refreshCache, logger });

    expect(client.rest.setToken).toHaveBeenCalledWith('tok-abc');
    expect(refreshCache).not.toHaveBeenCalled();
  });

  it('propagates refreshCache rejection so start() fails loud', async () => {
    // An http-only replica that can't reach Discord must not silently
    // start serving — gracefulShutdown(1) is the expected outcome so
    // ECS reschedules the task instead of serving 5xx.
    const client = makeClient();
    const err = new Error('Discord unreachable');
    const refreshCache = jest.fn().mockRejectedValue(err);
    const logger = makeLogger();
    const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '123' };

    await expect(initHttpOnly({ client, config, refreshCache, logger })).rejects.toThrow('Discord unreachable');
    // Token is still seeded before the refresh attempt so a manual retry
    // (e.g. via the lazy refresh in route handlers) doesn't re-401.
    expect(client.rest.setToken).toHaveBeenCalledWith('tok-abc');
  });

  it('does NOT warn about cache staleness (nothing reads the guild handle on a schedule)', async () => {
    // The module used to emit a boot WARN naming the missing
    // roleDelete/channelDelete events and the periodic refresh that
    // compensated for them. Both the events and the cache they
    // invalidated are gone (#1051), so the warning would now point
    // operators at a limitation that no longer exists.
    const client = makeClient();
    const refreshCache = jest.fn().mockResolvedValue(undefined);
    const logger = makeLogger();
    const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '123' };

    await initHttpOnly({ client, config, refreshCache, logger });

    expect(logger.warn).not.toHaveBeenCalled();
  });

  describe('no periodic refresh', () => {
    beforeEach(() => {
      jest.useFakeTimers();
    });
    afterEach(() => {
      jest.useRealTimers();
    });

    it('schedules no timer — refreshCache runs once at boot and never again', async () => {
      // Regression pin for the timer removal. In http-only mode the
      // cached guild handle has no reader at all: verifyBotPermissions()
      // and the `Watching guild:` log both hang off client.once('ready'),
      // which never fires because login() is skipped, and getGuild() has
      // no production consumers. A reinstated timer would burn a REST
      // call per interval refreshing a value no request path reads.
      //
      // Asserted behaviourally (advance the clock, count the calls)
      // rather than by spying on setInterval, so it also catches a
      // setTimeout-chain or any other self-rescheduling variant.
      const client = makeClient();
      const refreshCache = jest.fn().mockResolvedValue(undefined);
      const logger = makeLogger();
      const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '123' };

      await initHttpOnly({ client, config, refreshCache, logger });

      expect(refreshCache).toHaveBeenCalledTimes(1); // initial, fatal-on-failure
      expect(jest.getTimerCount()).toBe(0);

      // Well past the 10-minute interval the deleted timer used.
      jest.advanceTimersByTime(60 * 60 * 1000);
      await Promise.resolve();
      await Promise.resolve();

      expect(refreshCache).toHaveBeenCalledTimes(1);
      expect(logger.error).not.toHaveBeenCalled();
    });
  });

  it('returns nothing — callers have no timer to clear on shutdown', async () => {
    // gracefulShutdown in src/index.js no longer tracks an
    // http-refresh timer; this pins the contract that made that
    // removal safe.
    const client = makeClient();
    const refreshCache = jest.fn().mockResolvedValue(undefined);
    const logger = makeLogger();

    const single = await initHttpOnly({
      client, refreshCache, logger,
      config: { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '123' },
    });
    const multi = await initHttpOnly({
      client: makeClient(), refreshCache, logger,
      config: { DISCORD_TOKEN: 'tok-abc', GUILD_ID: null },
    });

    expect(single).toBeUndefined();
    expect(multi).toBeUndefined();
  });
});
