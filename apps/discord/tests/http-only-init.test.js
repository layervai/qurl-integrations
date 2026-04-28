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
 * login() would normally do (token + cache refresh) AND sets up
 * the periodic refresh that compensates for the missing
 * roleDelete/channelDelete events.
 */

const { initHttpOnly, REFRESH_INTERVAL_MS } = require('../src/http-only-init');

function makeClient() {
  return {
    rest: {
      setToken: jest.fn(),
    },
  };
}

function makeLogger() {
  return { info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn() };
}

describe('initHttpOnly', () => {
  it('sets the bot token on client.rest and warms the cache (single-guild)', async () => {
    const client = makeClient();
    const refreshCache = jest.fn().mockResolvedValue(undefined);
    const logger = makeLogger();
    const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '123' };

    const timer = await initHttpOnly({ client, config, refreshCache, logger });

    expect(client.rest.setToken).toHaveBeenCalledTimes(1);
    expect(client.rest.setToken).toHaveBeenCalledWith('tok-abc');
    expect(refreshCache).toHaveBeenCalledTimes(1);
    expect(timer).not.toBeNull();
    clearInterval(timer);
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

    const timer = await initHttpOnly({ client, config, refreshCache, logger });

    expect(callOrder).toEqual(['setToken', 'refreshCache']);
    clearInterval(timer);
  });

  it('skips refreshCache + timer when GUILD_ID is unset (multi-tenant mode)', async () => {
    const client = makeClient();
    const refreshCache = jest.fn().mockResolvedValue(undefined);
    const logger = makeLogger();
    const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: null };

    const timer = await initHttpOnly({ client, config, refreshCache, logger });

    expect(client.rest.setToken).toHaveBeenCalledWith('tok-abc');
    expect(refreshCache).not.toHaveBeenCalled();
    expect(timer).toBeNull();
    // No WARN — multi-tenant http-only doesn't have a single-guild
    // cache that could go stale, so the periodic-refresh disclaimer
    // would be misleading.
    expect(logger.warn).not.toHaveBeenCalled();
  });

  it('skips refreshCache + timer when GUILD_ID is empty string', async () => {
    const client = makeClient();
    const refreshCache = jest.fn().mockResolvedValue(undefined);
    const logger = makeLogger();
    const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '' };

    const timer = await initHttpOnly({ client, config, refreshCache, logger });

    expect(client.rest.setToken).toHaveBeenCalledWith('tok-abc');
    expect(refreshCache).not.toHaveBeenCalled();
    expect(timer).toBeNull();
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

  it('logs a WARN naming the cache-invalidation limitation in single-guild http-only mode', async () => {
    const client = makeClient();
    const refreshCache = jest.fn().mockResolvedValue(undefined);
    const logger = makeLogger();
    const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '123' };

    const timer = await initHttpOnly({ client, config, refreshCache, logger });

    expect(logger.warn).toHaveBeenCalledTimes(1);
    expect(logger.warn.mock.calls[0][0]).toMatch(/http-only mode/);
    expect(logger.warn.mock.calls[0][0]).toMatch(/cache invalidation/i);
    expect(logger.warn.mock.calls[0][0]).toMatch(/HTTP_ONLY_REFRESH_INTERVAL_MS/);
    clearInterval(timer);
  });

  describe('periodic refresh', () => {
    beforeEach(() => {
      jest.useFakeTimers();
    });
    afterEach(() => {
      jest.useRealTimers();
    });

    it('schedules a setInterval at REFRESH_INTERVAL_MS that calls refreshCache', async () => {
      const client = makeClient();
      const refreshCache = jest.fn().mockResolvedValue(undefined);
      const logger = makeLogger();
      const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '123' };

      const timer = await initHttpOnly({ client, config, refreshCache, logger });

      expect(refreshCache).toHaveBeenCalledTimes(1); // initial
      jest.advanceTimersByTime(REFRESH_INTERVAL_MS);
      expect(refreshCache).toHaveBeenCalledTimes(2); // periodic
      jest.advanceTimersByTime(REFRESH_INTERVAL_MS);
      expect(refreshCache).toHaveBeenCalledTimes(3);
      clearInterval(timer);
    });

    it('catches periodic-refresh rejections and logs at error (does not crash the process)', async () => {
      // A transient Discord outage during a periodic refresh must not
      // surface as an unhandledRejection that takes down the http
      // replica. The next interval retries automatically.
      const client = makeClient();
      const refreshCache = jest.fn()
        .mockResolvedValueOnce(undefined) // initial succeeds
        .mockRejectedValueOnce(Object.assign(new Error('transient 503'), { status: 503 }));
      const logger = makeLogger();
      const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '123' };

      const timer = await initHttpOnly({ client, config, refreshCache, logger });

      jest.advanceTimersByTime(REFRESH_INTERVAL_MS);
      // setInterval callbacks queue microtasks; flush them.
      await Promise.resolve();
      await Promise.resolve();

      expect(logger.error).toHaveBeenCalledTimes(1);
      expect(logger.error.mock.calls[0][0]).toMatch(/Periodic refreshCache failed/);
      expect(logger.error.mock.calls[0][1]).toEqual({ errorMessage: 'transient 503' });
      clearInterval(timer);
    });
  });

  it('REFRESH_INTERVAL_MS exposes a sane default (≥30s, ≤30min)', () => {
    expect(REFRESH_INTERVAL_MS).toBeGreaterThanOrEqual(30_000);
    expect(REFRESH_INTERVAL_MS).toBeLessThanOrEqual(30 * 60 * 1000);
  });
});

describe('HTTP_ONLY_REFRESH_INTERVAL_MS env override', () => {
  // Re-loads the module under different env values to verify the
  // module-load-time validation. Each case isolates its env mutation
  // and module cache so the canonical export above stays intact.
  const originalEnv = process.env.HTTP_ONLY_REFRESH_INTERVAL_MS;
  let warnSpy;

  beforeEach(() => {
    warnSpy = jest.spyOn(console, 'warn').mockImplementation(() => {});
  });
  afterEach(() => {
    if (originalEnv === undefined) delete process.env.HTTP_ONLY_REFRESH_INTERVAL_MS;
    else process.env.HTTP_ONLY_REFRESH_INTERVAL_MS = originalEnv;
    warnSpy.mockRestore();
    jest.resetModules();
  });

  it('accepts a valid override and uses it instead of the default', () => {
    process.env.HTTP_ONLY_REFRESH_INTERVAL_MS = '60000';
    jest.isolateModules(() => {
      const { REFRESH_INTERVAL_MS: overridden } = require('../src/http-only-init');
      expect(overridden).toBe(60_000);
    });
    expect(warnSpy).not.toHaveBeenCalled();
  });

  it('rejects sub-30s values with a console.warn naming the bad input', () => {
    process.env.HTTP_ONLY_REFRESH_INTERVAL_MS = '15000';
    jest.isolateModules(() => {
      const { REFRESH_INTERVAL_MS: overridden } = require('../src/http-only-init');
      // Falls back to default — silent fall-through would leave operators
      // wondering why their `=15000` ask isn't taking effect.
      expect(overridden).toBe(10 * 60 * 1000);
    });
    expect(warnSpy).toHaveBeenCalledTimes(1);
    expect(warnSpy.mock.calls[0][0]).toMatch(/HTTP_ONLY_REFRESH_INTERVAL_MS=/);
    expect(warnSpy.mock.calls[0][0]).toMatch(/15000/);
    expect(warnSpy.mock.calls[0][0]).toMatch(/rejected/);
  });

  it('rejects non-numeric values with a console.warn', () => {
    process.env.HTTP_ONLY_REFRESH_INTERVAL_MS = 'soon';
    jest.isolateModules(() => {
      const { REFRESH_INTERVAL_MS: overridden } = require('../src/http-only-init');
      expect(overridden).toBe(10 * 60 * 1000);
    });
    expect(warnSpy).toHaveBeenCalledTimes(1);
    expect(warnSpy.mock.calls[0][0]).toMatch(/"soon"/);
  });
});
