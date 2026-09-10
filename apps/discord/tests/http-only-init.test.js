
const { initHttpOnly } = require('../src/http-only-init');

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
    const client = makeClient();
    const refreshCache = jest.fn().mockResolvedValue(undefined);
    const logger = makeLogger();
    const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '123' };

    await initHttpOnly({ client, config, refreshCache, logger });

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
    const client = makeClient();
    const err = new Error('Discord unreachable');
    const refreshCache = jest.fn().mockRejectedValue(err);
    const logger = makeLogger();
    const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '123' };

    await expect(initHttpOnly({ client, config, refreshCache, logger })).rejects.toThrow('Discord unreachable');
    expect(client.rest.setToken).toHaveBeenCalledWith('tok-abc');
  });

  it('does NOT warn about cache staleness (nothing reads the guild handle on a schedule)', async () => {
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
      const client = makeClient();
      const refreshCache = jest.fn().mockResolvedValue(undefined);
      const logger = makeLogger();
      const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '123' };

      await initHttpOnly({ client, config, refreshCache, logger });

      expect(refreshCache).toHaveBeenCalledTimes(1); // initial, fatal-on-failure
      expect(jest.getTimerCount()).toBe(0);

      jest.advanceTimersByTime(60 * 60 * 1000);
      await Promise.resolve();
      await Promise.resolve();

      expect(refreshCache).toHaveBeenCalledTimes(1);
      expect(logger.error).not.toHaveBeenCalled();
    });
  });

  it('returns nothing — callers have no timer to clear on shutdown', async () => {
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
