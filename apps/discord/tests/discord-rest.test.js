
jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

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

const { sendDM, editDM, sendChannelMessage, addRoleToMember, removeRoleFromMember, editInteractionReply } = require('../src/discord-rest');
const logger = require('../src/logger');

beforeEach(() => {
  restMock.post.mockReset();
  restMock.put.mockReset();
  restMock.delete.mockReset();
  restMock.patch.mockReset();
  logger.info.mockClear();
  logger.warn.mockClear();
  logger.error.mockClear();
  logger.debug.mockClear();
});

describe('sendDM via REST', () => {
  it('creates DM channel then posts message, returns ok:true with channel + message ids', async () => {
    restMock.post
      .mockResolvedValueOnce({ id: 'channel-1' })     // create channel
      .mockResolvedValueOnce({ id: 'message-1' });    // post message
    const result = await sendDM('user-1', { content: 'hi' });
    expect(result).toEqual({ ok: true, channelId: 'channel-1', messageId: 'message-1' });
    expect(restMock.post).toHaveBeenCalledTimes(2);
    expect(restMock.post.mock.calls[0][0]).toBe('/users/@me/channels');
    expect(restMock.post.mock.calls[0][1]).toEqual({ body: { recipient_id: 'user-1' } });
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

  it('marks 10008 (Unknown Message — recipient deleted the DM) as expected and exposes code+reason', async () => {
    restMock.patch.mockRejectedValueOnce(
      Object.assign(new Error('Unknown Message'), { status: 404, code: 10008 }),
    );
    const result = await editDM('c', 'm', { embeds: [], components: [] });
    expect(result).toEqual({
      ok: false,
      expected: true,
      code: 10008,
      reason: expect.stringContaining('Unknown Message'),
    });
  });

  it.each([
    ['10003', 10003, 404, 'Unknown Channel'],
    ['50001', 50001, 403, 'Missing Access'],
    ['50007', 50007, 403, 'Cannot send messages to this user'],
  ])('marks %s as expected and surfaces code+reason on the return', async (_name, code, status, descriptionFragment) => {
    restMock.patch.mockRejectedValueOnce(
      Object.assign(new Error('expected'), { status, code }),
    );
    const result = await editDM('c', 'm', { embeds: [], components: [] });
    expect(result).toEqual({
      ok: false,
      expected: true,
      code,
      reason: expect.stringContaining(descriptionFragment),
    });
  });

  it('marks unrecognized errors as unexpected (logged at warn) — code passes through, reason is undefined', async () => {
    restMock.patch.mockRejectedValueOnce(
      Object.assign(new Error('boom'), { status: 500, code: 0 }),
    );
    const result = await editDM('c', 'm', { embeds: [], components: [] });
    expect(result).toEqual({ ok: false, expected: false, code: 0, reason: undefined });
  });

  it('marks bare 403 / 404 without a known API code as UNEXPECTED', async () => {
    restMock.patch.mockRejectedValueOnce(
      Object.assign(new Error('Forbidden'), { status: 403, code: undefined }),
    );
    const r403 = await editDM('c', 'm', { embeds: [], components: [] });
    expect(r403).toEqual({ ok: false, expected: false, code: undefined, reason: undefined });

    restMock.patch.mockRejectedValueOnce(
      Object.assign(new Error('Not Found'), { status: 404, code: undefined }),
    );
    const r404 = await editDM('c', 'm', { embeds: [], components: [] });
    expect(r404).toEqual({ ok: false, expected: false, code: undefined, reason: undefined });
  });

  it('tags the expected log line with expectedReason for greppability', async () => {
    const logger = require('../src/logger');
    logger.info.mockClear();
    logger.warn.mockClear();
    restMock.patch.mockRejectedValueOnce(
      Object.assign(new Error('Cannot send messages to this user'), { status: 403, code: 50007 }),
    );
    await editDM('c', 'm', { embeds: [], components: [] });
    expect(logger.info).toHaveBeenCalledWith(
      'Failed to edit DM',
      expect.objectContaining({
        code: 50007,
        expectedReason: expect.stringContaining('Cannot send messages to this user'),
      }),
    );
    logger.info.mockClear();
    logger.warn.mockClear();
    restMock.patch.mockRejectedValueOnce(
      Object.assign(new Error('Internal Server Error'), { status: 500, code: 0 }),
    );
    await editDM('c', 'm', { embeds: [], components: [] });
    expect(logger.warn).toHaveBeenCalledWith(
      'Failed to edit DM',
      expect.not.objectContaining({ expectedReason: expect.anything() }),
    );
  });
});

describe('addRoleToMember via REST', () => {
  it('PUTs to guildMemberRole endpoint, returns ok:true', async () => {
    restMock.put.mockResolvedValueOnce({});
    const result = await addRoleToMember('guild-1', 'user-1', 'role-1');
    expect(result).toEqual({ ok: true });
    expect(restMock.put).toHaveBeenCalledTimes(1);
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
    expect(restMock.delete.mock.calls[0][0]).toBe('/guilds/guild-1/members/user-1/roles/role-1');
  });
});

describe('sendChannelMessage via REST', () => {
  it('POSTs to /channels/:cid/messages with the message body, returns ok:true with messageId', async () => {
    restMock.post.mockResolvedValueOnce({ id: 'message-7' });
    const result = await sendChannelMessage('channel-1', { content: 'hello room' });
    expect(result).toEqual({ ok: true, messageId: 'message-7' });
    expect(restMock.post).toHaveBeenCalledTimes(1);
    expect(restMock.post.mock.calls[0][0]).toBe('/channels/channel-1/messages');
    expect(restMock.post.mock.calls[0][1]).toEqual({ body: { content: 'hello room' } });
  });

  it('returns ok:false on 403 (Missing Permissions) — caller decides whether to surface', async () => {
    const err = new Error('Missing Permissions');
    err.status = 403;
    restMock.post.mockRejectedValueOnce(err);
    const result = await sendChannelMessage('channel-1', { content: 'x' });
    expect(result.ok).toBe(false);
    expect(result.status).toBe(403);
  });

  it('returns ok:false on transient 5xx', async () => {
    const err = new Error('Bad Gateway');
    err.status = 502;
    restMock.post.mockRejectedValueOnce(err);
    const result = await sendChannelMessage('channel-1', { content: 'x' });
    expect(result.ok).toBe(false);
    expect(result.status).toBe(502);
  });
});

describe('editInteractionReply via webhook token (cross-replica view-counter primitive)', () => {
  const APP_ID = 'app-xyz';
  const TOKEN = 'tok-LIVE-bearer-cred-abc123';

  it('PATCHes /webhooks/{app}/{token}/messages/@original with the payload, returns ok:true', async () => {
    restMock.patch.mockResolvedValueOnce({});
    const result = await editInteractionReply(APP_ID, TOKEN, { content: '👀 1 viewed' });
    expect(result).toEqual({ ok: true });
    expect(restMock.patch).toHaveBeenCalledTimes(1);
    const [route, opts] = restMock.patch.mock.calls[0];
    expect(route).toBe(`/webhooks/${APP_ID}/${TOKEN}/messages/%40original`);
    expect(opts).toEqual({ body: { content: '👀 1 viewed' }, auth: false });
  });

  it('returns ok:false {status,code} on an expired token (logs at info, not warn)', async () => {
    const err = new Error('Invalid Webhook Token');
    err.code = 50027;
    err.status = 401;
    restMock.patch.mockRejectedValueOnce(err);
    const result = await editInteractionReply(APP_ID, TOKEN, { content: 'x' });
    expect(result).toEqual({ ok: false, status: 401, code: 50027 });
    expect(logger.info).toHaveBeenCalledWith(
      'editInteractionReply via webhook token failed',
      expect.objectContaining({ expired: true, status: 401, code: 50027 }),
    );
    expect(logger.warn).not.toHaveBeenCalled();
  });

  it('SECURITY: scrubs the token from a network-error message before logging', async () => {
    const leaky = new Error(`request to https://discord.com/api/v10/webhooks/${APP_ID}/${TOKEN}/messages/@original failed, reason: ECONNRESET`);
    restMock.patch.mockRejectedValueOnce(leaky);
    const result = await editInteractionReply(APP_ID, TOKEN, { content: 'x' });
    expect(result).toEqual({ ok: false, status: undefined, code: undefined });

    expect(logger.warn).toHaveBeenCalledTimes(1);
    const [, fields] = logger.warn.mock.calls[0];
    expect(fields.errorMessage).not.toContain(TOKEN);
    expect(fields.errorMessage).toContain('[redacted-token]');
    expect(fields.errorMessage).toContain('ECONNRESET');
    expect(JSON.stringify(fields)).not.toContain(TOKEN);
  });

  it('SECURITY: also scrubs a percent-encoded token form from the message', async () => {
    const encToken = 'tok needs/encoding';            // space + slash → %20, %2F
    const encoded = encodeURIComponent(encToken);     // 'tok%20needs%2Fencoding'
    const leaky = new Error(`fetch to https://discord.com/api/v10/webhooks/${APP_ID}/${encoded}/messages/@original failed`);
    restMock.patch.mockRejectedValueOnce(leaky);
    const result = await editInteractionReply(APP_ID, encToken, { content: 'x' });
    expect(result.ok).toBe(false);
    const [, fields] = logger.warn.mock.calls[0];
    expect(fields.errorMessage).not.toContain(encoded);
    expect(fields.errorMessage).toContain('[redacted-token]');
    expect(JSON.stringify(fields)).not.toContain(encoded);
  });

  it('never logs the token even when the error message is non-string', async () => {
    const weird = { status: 500, code: undefined, message: undefined };
    restMock.patch.mockRejectedValueOnce(weird);
    const result = await editInteractionReply(APP_ID, TOKEN, { content: 'x' });
    expect(result).toEqual({ ok: false, status: 500, code: undefined });
    const [, fields] = logger.warn.mock.calls[0];
    expect(fields.errorMessage).toBeUndefined();
    expect(JSON.stringify(fields)).not.toContain(TOKEN);
  });
});
