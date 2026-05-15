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
  audit: jest.fn(),
}));

// Mock ../src/discord to expose a controllable client.rest. discord-rest
// reads `client.rest` at module load and reuses it for every REST call,
// so the mock object created here doubles as the spy surface for tests.
jest.mock('../src/discord', () => {
  const mockRestInstance = {
    post: jest.fn(),
    put: jest.fn(),
    delete: jest.fn(),
    patch: jest.fn(),
  };
  return {
    client: { rest: mockRestInstance },
    __mockRestInstance: mockRestInstance,
  };
});

const restMock = require('../src/discord').__mockRestInstance;

const { sendDM, editDM, addRoleToMember, removeRoleFromMember } = require('../src/discord-rest');

beforeEach(() => {
  restMock.post.mockReset();
  restMock.put.mockReset();
  restMock.delete.mockReset();
  restMock.patch.mockReset();
});

// Each helper-level test implicitly pins the shared-rate-limit-bucket
// invariant: every helper calls `client.rest.X` (verified via
// `restMock.post/put/delete/patch.mock.calls`), so they share the
// same rate-limit bucket as the gateway-cache helpers in discord.js.

describe('sendDM via REST', () => {
  it('creates DM channel then posts message, returns ok:true with channel + message ids', async () => {
    restMock.post
      .mockResolvedValueOnce({ id: 'channel-1' })     // create channel
      .mockResolvedValueOnce({ id: 'message-1' });    // post message
    const result = await sendDM('user-1', { content: 'hi' });
    expect(result).toEqual({ ok: true, channelId: 'channel-1', messageId: 'message-1' });
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

describe('editDM via REST', () => {
  it('PATCHes the target message in one REST call and returns ok', async () => {
    restMock.patch.mockResolvedValueOnce(undefined);
    const payload = { embeds: [{ description: 'closed' }], components: [] };
    const result = await editDM('channel-1', 'message-1', payload);
    expect(result).toEqual({ ok: true });
    expect(restMock.patch).toHaveBeenCalledTimes(1);
    expect(restMock.patch.mock.calls[0][0]).toBe('/channels/channel-1/messages/message-1');
    expect(restMock.patch.mock.calls[0][1]).toEqual({ body: payload });
  });

  it('marks 10008 (Unknown Message — recipient deleted the DM) as expected', async () => {
    restMock.patch.mockRejectedValueOnce(
      Object.assign(new Error('Unknown Message'), { status: 404, code: 10008 }),
    );
    const result = await editDM('c', 'm', { embeds: [], components: [] });
    expect(result).toEqual({ ok: false, expected: true });
  });

  it.each([
    ['10003', 10003, 404],  // Unknown Channel
    ['50001', 50001, 403],  // Missing Access
    ['50007', 50007, 403],  // Cannot send messages to this user
  ])('marks %s as expected', async (_name, code, status) => {
    restMock.patch.mockRejectedValueOnce(
      Object.assign(new Error('expected'), { status, code }),
    );
    const result = await editDM('c', 'm', { embeds: [], components: [] });
    expect(result).toEqual({ ok: false, expected: true });
  });

  it('marks unrecognized errors as unexpected (logged at warn)', async () => {
    restMock.patch.mockRejectedValueOnce(
      Object.assign(new Error('boom'), { status: 500, code: 0 }),
    );
    const result = await editDM('c', 'm', { embeds: [], components: [] });
    expect(result).toEqual({ ok: false, expected: false });
  });

  it('marks bare 403 / 404 without a known API code as UNEXPECTED', async () => {
    // Defense against the cr-flagged scenario: Discord-side bugs,
    // proxy 404s, and revoked-token 403s shouldn't get the silent
    // info-level treatment reserved for known operational outcomes.
    // The API code is the gate; status alone is not.
    restMock.patch.mockRejectedValueOnce(
      Object.assign(new Error('Forbidden'), { status: 403, code: undefined }),
    );
    const r403 = await editDM('c', 'm', { embeds: [], components: [] });
    expect(r403).toEqual({ ok: false, expected: false });

    restMock.patch.mockRejectedValueOnce(
      Object.assign(new Error('Not Found'), { status: 404, code: undefined }),
    );
    const r404 = await editDM('c', 'm', { embeds: [], components: [] });
    expect(r404).toEqual({ ok: false, expected: false });
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
