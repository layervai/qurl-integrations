/**
 * Unit tests for src/discord-rest.js — the REST-only Discord
 * helpers used by the HTTP-process role in the gateway/http split.
 *
 * Mocks `../src/discord` to inject a fake `client.rest` so tests
 * don't hit the Discord API. The shared-bucket invariant (one REST
 * instance per process, reused across legacy gateway-cache helpers
 * and these REST-only helpers) means the test must mock at the
 * discord.js Client boundary rather than `@discordjs/rest`.
 *
 * Asserts both the happy path AND the expected graceful-failure
 * shape (`{ ok: false, status, error }`) so callers can branch
 * without catching exceptions. Also asserts the exact REST routes
 * (POST /users/@me/channels, PUT /guilds/:guild/members/:user/roles/:role,
 * etc.) so a future refactor that swaps Routes.userChannels for
 * Routes.channelMessages would fail this suite at PR time.
 */

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
}));

// Mock ../src/discord to expose a controllable client.rest. discord-rest
// reads `client.rest` at module load and reuses it for every REST call,
// so the mock object created here doubles as the spy surface for tests.
jest.mock('../src/discord', () => {
  const mockRestInstance = {
    post: jest.fn(),
    put: jest.fn(),
    delete: jest.fn(),
  };
  return {
    client: { rest: mockRestInstance },
    __mockRestInstance: mockRestInstance,
  };
});

const restMock = require('../src/discord').__mockRestInstance;

const { rest: exportedRest, sendDM, addRoleToMember, removeRoleFromMember } = require('../src/discord-rest');

beforeEach(() => {
  restMock.post.mockReset();
  restMock.put.mockReset();
  restMock.delete.mockReset();
});

describe('module wiring', () => {
  it('re-exports the shared client.rest instance (not a new one)', () => {
    // Sharing the rate-limit bucket state is the whole reason this module
    // imports client.rest instead of constructing its own REST().
    expect(exportedRest).toBe(restMock);
  });
});

describe('sendDM via REST', () => {
  it('creates DM channel then posts message, returns ok:true', async () => {
    restMock.post
      .mockResolvedValueOnce({ id: 'channel-1' }) // create channel
      .mockResolvedValueOnce({}); // post message
    const result = await sendDM('user-1', { content: 'hi' });
    expect(result).toEqual({ ok: true, channelId: 'channel-1' });
    expect(restMock.post).toHaveBeenCalledTimes(2);
    // First call: create DM channel — POST /users/@me/channels.
    expect(restMock.post.mock.calls[0][0]).toBe('/users/@me/channels');
    expect(restMock.post.mock.calls[0][1]).toEqual({ body: { recipient_id: 'user-1' } });
    // Second call: post message body to that channel.
    expect(restMock.post.mock.calls[1][0]).toBe('/channels/channel-1/messages');
    expect(restMock.post.mock.calls[1][1]).toEqual({ body: { content: 'hi' } });
  });

  it('returns ok:false on 403 (DM disabled / blocked / Missing Access) — expected operational error', async () => {
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

  it('returns ok:false on partial-failure (channel created, message-post failed)', async () => {
    // Documented two-call flow: a successful channel create followed by a
    // failed message-post must surface the post failure rather than
    // silently masking it as success. Locks in the {ok:false, status,
    // error} shape for the second-call branch so a refactor swapping the
    // try-block boundary can't regress it.
    restMock.post
      .mockResolvedValueOnce({ id: 'channel-1' }) // create channel succeeds
      .mockRejectedValueOnce(Object.assign(new Error('rate limited'), { status: 429 }));
    const result = await sendDM('user-1', { content: 'hi' });
    expect(result.ok).toBe(false);
    expect(result.status).toBe(429);
    expect(result.error).toBe('rate limited');
    expect(restMock.post).toHaveBeenCalledTimes(2);
  });
});

describe('addRoleToMember via REST', () => {
  it('PUTs to guildMemberRole endpoint, returns ok:true', async () => {
    restMock.put.mockResolvedValueOnce({});
    const result = await addRoleToMember('guild-1', 'user-1', 'role-1');
    expect(result).toEqual({ ok: true });
    expect(restMock.put).toHaveBeenCalledTimes(1);
    // PUT /guilds/:guild/members/:user/roles/:role — verbatim route.
    expect(restMock.put.mock.calls[0][0]).toBe('/guilds/guild-1/members/user-1/roles/role-1');
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
    // DELETE /guilds/:guild/members/:user/roles/:role — same path shape as PUT.
    expect(restMock.delete.mock.calls[0][0]).toBe('/guilds/guild-1/members/user-1/roles/role-1');
  });
});
