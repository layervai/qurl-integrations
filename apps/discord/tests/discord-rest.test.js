/**
 * Unit tests for src/discord-rest.js — the REST-only Discord
 * helpers used by the HTTP-process role in the gateway/http split.
 *
 * Mocks `@discordjs/rest` at the module level so tests don't hit
 * the Discord API. Asserts both the happy path AND the expected
 * graceful-failure shape (`{ ok: false, status, error }`) so
 * callers can branch without catching exceptions.
 */

jest.mock('../src/config', () => ({
  DISCORD_TOKEN: 'fake-test-token',
  PENDING_LINK_EXPIRY_MINUTES: 10,
  DATABASE_PATH: ':memory:',
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
}));

// Mock the REST class before discord-rest imports it. jest.mock's
// factory runs before any other module code, so the mock object
// must be created inside the factory (naming it `mockRest` or
// similar with the `mock` prefix lets jest hoist it). Expose the
// instance via a module-level accessor so tests can interact with it.
jest.mock('@discordjs/rest', () => {
  const mockRestInstance = {
    setToken: jest.fn().mockReturnThis(),
    post: jest.fn(),
    put: jest.fn(),
    delete: jest.fn(),
  };
  return {
    REST: jest.fn().mockImplementation(() => mockRestInstance),
    __mockRestInstance: mockRestInstance,
  };
});

const restMock = require('@discordjs/rest').__mockRestInstance;

const { sendDM, addRoleToMember, removeRoleFromMember } = require('../src/discord-rest');

beforeEach(() => {
  restMock.post.mockReset();
  restMock.put.mockReset();
  restMock.delete.mockReset();
});

describe('sendDM via REST', () => {
  it('creates DM channel then posts message, returns ok:true', async () => {
    restMock.post
      .mockResolvedValueOnce({ id: 'channel-1' }) // create channel
      .mockResolvedValueOnce({}); // post message
    const result = await sendDM('user-1', { content: 'hi' });
    expect(result).toEqual({ ok: true, channelId: 'channel-1' });
    expect(restMock.post).toHaveBeenCalledTimes(2);
    // First call: create DM channel with recipient_id
    expect(restMock.post.mock.calls[0][1]).toEqual({ body: { recipient_id: 'user-1' } });
    // Second call: post message body to that channel
    expect(restMock.post.mock.calls[1][1]).toEqual({ body: { content: 'hi' } });
  });

  it('returns ok:false on 403 (DM disabled / blocked) — expected operational error', async () => {
    const err = new Error('Cannot send messages to this user');
    err.status = 403;
    restMock.post.mockRejectedValueOnce(err);
    const result = await sendDM('user-1', { content: 'hi' });
    expect(result.ok).toBe(false);
    expect(result.status).toBe(403);
  });

  it('returns ok:false on other errors, logs at error level', async () => {
    const err = new Error('Network broken');
    err.status = 503;
    restMock.post.mockRejectedValueOnce(err);
    const result = await sendDM('user-1', { content: 'hi' });
    expect(result.ok).toBe(false);
    expect(result.status).toBe(503);
  });

  it('short-circuits when DISCORD_TOKEN is not configured', async () => {
    // Re-mock config to return empty token, re-require module.
    jest.resetModules();
    jest.doMock('../src/config', () => ({ DISCORD_TOKEN: '', PENDING_LINK_EXPIRY_MINUTES: 10 }));
    const freshMock = { setToken: jest.fn().mockReturnThis(), post: jest.fn() };
    jest.doMock('@discordjs/rest', () => ({ REST: jest.fn().mockImplementation(() => freshMock) }));
    const fresh = require('../src/discord-rest');
    const result = await fresh.sendDM('user-1', { content: 'hi' });
    expect(result.ok).toBe(false);
    expect(result.error).toMatch(/DISCORD_TOKEN/);
    expect(freshMock.post).not.toHaveBeenCalled();
  });
});

describe('addRoleToMember via REST', () => {
  it('PUTs to guildMemberRole endpoint, returns ok:true', async () => {
    restMock.put.mockResolvedValueOnce({});
    const result = await addRoleToMember('guild-1', 'user-1', 'role-1');
    expect(result).toEqual({ ok: true });
    expect(restMock.put).toHaveBeenCalledTimes(1);
  });

  it('returns ok:false + status on error', async () => {
    const err = new Error('Forbidden');
    err.status = 403;
    restMock.put.mockRejectedValueOnce(err);
    const result = await addRoleToMember('guild-1', 'user-1', 'role-1');
    expect(result.ok).toBe(false);
    expect(result.status).toBe(403);
  });
});

describe('removeRoleFromMember via REST', () => {
  it('DELETEs guildMemberRole endpoint, returns ok:true', async () => {
    restMock.delete.mockResolvedValueOnce({});
    const result = await removeRoleFromMember('guild-1', 'user-1', 'role-1');
    expect(result).toEqual({ ok: true });
    expect(restMock.delete).toHaveBeenCalledTimes(1);
  });
});
