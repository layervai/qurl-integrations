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
 */

const { initHttpOnly } = require('../src/http-only-init');

function makeClient() {
  return {
    rest: {
      setToken: jest.fn(),
    },
  };
}

describe('initHttpOnly', () => {
  it('sets the bot token on client.rest and warms the cache (single-guild)', async () => {
    const client = makeClient();
    const refreshCache = jest.fn().mockResolvedValue(undefined);
    const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '123' };

    await initHttpOnly({ client, config, refreshCache });

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
    const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '123' };

    await initHttpOnly({ client, config, refreshCache });

    expect(callOrder).toEqual(['setToken', 'refreshCache']);
  });

  it('skips refreshCache when GUILD_ID is unset (multi-tenant mode)', async () => {
    const client = makeClient();
    const refreshCache = jest.fn().mockResolvedValue(undefined);
    const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: null };

    await initHttpOnly({ client, config, refreshCache });

    expect(client.rest.setToken).toHaveBeenCalledWith('tok-abc');
    expect(refreshCache).not.toHaveBeenCalled();
  });

  it('skips refreshCache when GUILD_ID is empty string', async () => {
    const client = makeClient();
    const refreshCache = jest.fn().mockResolvedValue(undefined);
    const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '' };

    await initHttpOnly({ client, config, refreshCache });

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
    const config = { DISCORD_TOKEN: 'tok-abc', GUILD_ID: '123' };

    await expect(initHttpOnly({ client, config, refreshCache })).rejects.toThrow('Discord unreachable');
    // Token is still seeded before the refresh attempt so a manual retry
    // (e.g. via the lazy refresh in route handlers) doesn't re-401.
    expect(client.rest.setToken).toHaveBeenCalledWith('tok-abc');
  });
});
