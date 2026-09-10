
const { EventEmitter } = require('node:events');

const { createGatewayHmac, unwrapEnvelope } = require('../src/gateway-hmac');
const { createControlClient, DEFAULT_TIMEOUT_MS } = require('../src/gateway-control-client');
const { startControlChannelServer } = require('../src/gateway-control-channel');

const SECRET = 'a'.repeat(64);

function makeHmac({ clock } = {}) {
  return createGatewayHmac({
    secrets: { current: SECRET },
    logger: { info() {}, warn() {}, error() {}, debug() {} },
    clock,
  });
}

function makeLogger() {
  return {
    info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(),
  };
}

function makeFakeHttpRequest({ behavior }) {
  const calls = [];

  function fakeRequest(options, responseHandler) {
    const req = new EventEmitter();
    const writtenChunks = [];
    req.write = (chunk) => { writtenChunks.push(chunk); };
    req.end = (chunk) => {
      if (chunk) writtenChunks.push(chunk);
      const ctx = {
        options,
        body: Buffer.concat(writtenChunks),
        respond(status, body) {
          const res = new EventEmitter();
          res.statusCode = status;
          res.destroyed = false;
          res.destroy = () => { res.destroyed = true; };
          responseHandler(res);
          setImmediate(() => {
            res.emit('data', Buffer.from(body ?? '', 'utf8'));
            res.emit('end');
          });
        },
        respondThenAbort(status) {
          const res = new EventEmitter();
          res.statusCode = status;
          res.destroyed = false;
          responseHandler(res);
          setImmediate(() => {
            res.destroyed = true;
            res.emit('close');
          });
        },
        timeout() { req.emit('timeout'); },
        error(err) { req.emit('error', err); },
      };
      calls.push(ctx);
      behavior(ctx);
    };
    req.destroy = (err) => { if (err) req.emit('error', err); };
    return req;
  }

  return { fakeRequest, calls };
}

const validArgs = {
  peerIp: '10.0.1.5',
  peerPort: 9876,
  peerInstanceId: 'inst-B',
  selfInstanceId: 'inst-A',
  expectedVersion: 7,
};

describe('createControlClient — factory validation', () => {
  it('exposes default timeout', () => {
    expect(DEFAULT_TIMEOUT_MS).toBe(200);
  });

  it('throws on missing hmac or logger', () => {
    expect(() => createControlClient()).toThrow(/hmac/);
    expect(() => createControlClient({ hmac: {} })).toThrow(/hmac/);
    expect(() => createControlClient({ hmac: { sign() {}, generateNonce() {} } }))
      .toThrow(/logger/);
  });
});

describe('pushHandoff — argument validation', () => {
  it('returns ok:false reason:invalid_arg on missing required args (never throws)', async () => {
    const hmac = makeHmac();
    const client = createControlClient({ hmac, logger: makeLogger() });
    await expect(client.pushHandoff({}))
      .resolves.toMatchObject({ ok: false, reason: 'invalid_arg', arg: 'peerIp' });
    await expect(client.pushHandoff({ ...validArgs, peerIp: '' }))
      .resolves.toMatchObject({ ok: false, reason: 'invalid_arg', arg: 'peerIp' });
    await expect(client.pushHandoff({ ...validArgs, peerPort: 0 }))
      .resolves.toMatchObject({ ok: false, reason: 'invalid_arg', arg: 'peerPort' });
    await expect(client.pushHandoff({ ...validArgs, peerInstanceId: '' }))
      .resolves.toMatchObject({ ok: false, reason: 'invalid_arg', arg: 'peerInstanceId' });
    await expect(client.pushHandoff({ ...validArgs, selfInstanceId: '' }))
      .resolves.toMatchObject({ ok: false, reason: 'invalid_arg', arg: 'selfInstanceId' });
    await expect(client.pushHandoff({ ...validArgs, expectedVersion: 0 }))
      .resolves.toMatchObject({ ok: false, reason: 'invalid_arg', arg: 'expectedVersion' });
  });

  it('rejects peerIp that is not an IPv4/IPv6 literal (defense-in-depth vs corrupted heartbeat row)', async () => {
    const hmac = makeHmac();
    const client = createControlClient({ hmac, logger: makeLogger() });
    for (const bad of ['discord.com', 'localhost', 'undefined', 'not-an-ip', '10.0.0', '10.0.0.0.0']) {
      // eslint-disable-next-line no-await-in-loop
      await expect(client.pushHandoff({ ...validArgs, peerIp: bad }))
        .resolves.toMatchObject({ ok: false, reason: 'invalid_arg', arg: 'peerIp' });
    }
    const { fakeRequest } = makeFakeHttpRequest({
      behavior: (ctx) => ctx.respond(200, '{}'),
    });
    const okClient = createControlClient({
      hmac, logger: makeLogger(), httpRequest: fakeRequest,
    });
    await expect(okClient.pushHandoff({ ...validArgs, peerIp: '10.0.0.1' }))
      .resolves.toEqual({ ok: true, status: 200 });
    await expect(okClient.pushHandoff({ ...validArgs, peerIp: '::1' }))
      .resolves.toEqual({ ok: true, status: 200 });
  });
});

describe('pushHandoff — request shape', () => {
  it('POSTs /control/yours with a signed envelope to the peer IP+port', async () => {
    const hmac = makeHmac();
    const { fakeRequest, calls } = makeFakeHttpRequest({
      behavior: (ctx) => ctx.respond(200, '{"status":"ok"}'),
    });
    const client = createControlClient({
      hmac, logger: makeLogger(), httpRequest: fakeRequest,
    });

    const result = await client.pushHandoff(validArgs);
    expect(result).toEqual({ ok: true, status: 200 });
    expect(calls).toHaveLength(1);
    expect(calls[0].options).toMatchObject({
      hostname: '10.0.1.5',
      port: 9876,
      path: '/control/yours',
      method: 'POST',
    });
    expect(calls[0].options.headers).toMatchObject({
      'Content-Type': 'application/json',
      'Content-Length': calls[0].body.length,
    });
    const unwrapped = unwrapEnvelope(calls[0].body);
    expect(unwrapped).not.toBeNull();
    expect(typeof unwrapped.signature).toBe('string');
    expect(unwrapped.signature).toHaveLength(64);
  });

  it('payload includes selfInstanceId, peerInstanceId, expectedVersion, ts, nonce', async () => {
    const hmac = makeHmac();
    const { fakeRequest, calls } = makeFakeHttpRequest({
      behavior: (ctx) => ctx.respond(200, '{}'),
    });
    const client = createControlClient({ hmac, logger: makeLogger(), httpRequest: fakeRequest });
    await client.pushHandoff(validArgs);

    const unwrapped = unwrapEnvelope(calls[0].body);
    const payload = JSON.parse(unwrapped.bodyBytes.toString('utf8'));
    expect(payload).toMatchObject({
      active_instance_id: 'inst-A',
      peer_instance_id: 'inst-B',
      expected_version: 7,
    });
    expect(typeof payload.ts).toBe('number');
    expect(typeof payload.nonce).toBe('string');
    expect(payload.nonce).toMatch(/^[0-9a-f]{32}$/);
  });

  it('passes the per-call timeout (default 200 ms) to httpRequest', async () => {
    const hmac = makeHmac();
    const { fakeRequest, calls } = makeFakeHttpRequest({
      behavior: (ctx) => ctx.respond(200, '{}'),
    });
    const client = createControlClient({ hmac, logger: makeLogger(), httpRequest: fakeRequest });
    await client.pushHandoff(validArgs);
    expect(calls[0].options.timeout).toBe(200);

    const fastClient = createControlClient({
      hmac, logger: makeLogger(), httpRequest: fakeRequest, timeoutMs: 50,
    });
    await fastClient.pushHandoff(validArgs);
    expect(calls[1].options.timeout).toBe(50);
  });
});

describe('pushHandoff — result mapping', () => {
  it('returns ok:true on 2xx', async () => {
    const hmac = makeHmac();
    const { fakeRequest } = makeFakeHttpRequest({
      behavior: (ctx) => ctx.respond(200, '{}'),
    });
    const client = createControlClient({ hmac, logger: makeLogger(), httpRequest: fakeRequest });
    const result = await client.pushHandoff(validArgs);
    expect(result).toEqual({ ok: true, status: 200 });
  });

  it('returns reason:rejected on non-2xx (with body)', async () => {
    const hmac = makeHmac();
    const { fakeRequest } = makeFakeHttpRequest({
      behavior: (ctx) => ctx.respond(401, '{"error":"unauthorized"}'),
    });
    const client = createControlClient({ hmac, logger: makeLogger(), httpRequest: fakeRequest });
    const result = await client.pushHandoff(validArgs);
    expect(result).toEqual({
      ok: false, reason: 'rejected', status: 401, body: '{"error":"unauthorized"}',
    });
  });

  it('returns reason:timeout when peer is unresponsive', async () => {
    const hmac = makeHmac();
    const { fakeRequest } = makeFakeHttpRequest({
      behavior: (ctx) => ctx.timeout(),
    });
    const logger = makeLogger();
    const client = createControlClient({ hmac, logger, httpRequest: fakeRequest });
    const result = await client.pushHandoff(validArgs);
    expect(result).toEqual({ ok: false, reason: 'timeout' });
    expect(logger.warn).toHaveBeenCalledWith(
      'control-client: handoff timed out',
      expect.objectContaining({ peerInstanceId: 'inst-B' }),
    );
  });

  it('returns reason:http_error on transport error', async () => {
    const hmac = makeHmac();
    const { fakeRequest } = makeFakeHttpRequest({
      behavior: (ctx) => ctx.error(new Error('ECONNREFUSED')),
    });
    const client = createControlClient({ hmac, logger: makeLogger(), httpRequest: fakeRequest });
    const result = await client.pushHandoff(validArgs);
    expect(result).toEqual({ ok: false, reason: 'http_error', error: 'ECONNREFUSED' });
  });

  it('caps response body and returns reason:http_error on cap exceeded', async () => {
    const hmac = makeHmac();
    const { fakeRequest } = makeFakeHttpRequest({
      behavior: (ctx) => {
        const res = new (require('events').EventEmitter)();
        res.statusCode = 200;
        ctx.options; // no-op — keep ctx referenced
        ctx.respond(200, 'X'.repeat(100));
      },
    });
    const logger = makeLogger();
    const client = createControlClient({
      hmac, logger, httpRequest: fakeRequest, responseByteCap: 50,
    });
    const result = await client.pushHandoff(validArgs);
    expect(result).toEqual({
      ok: false, reason: 'http_error', error: 'response_body_too_large',
    });
    expect(logger.warn).toHaveBeenCalledWith(
      'control-client: response body exceeded cap',
      expect.objectContaining({ peerInstanceId: 'inst-B', cap: 50 }),
    );
  });

  it('drops in-flight chunks AFTER bodyCapExceeded fires (synchronous-destroy + flag-mark)', async () => {
    const hmac = makeHmac();
    let capturedRes;
    const fakeRequest = (options, responseHandler) => {
      const req = new EventEmitter();
      req.write = () => {};
      req.end = () => {
        const res = new EventEmitter();
        res.statusCode = 200;
        res.destroyed = false;
        res.destroy = () => { res.destroyed = true; };
        capturedRes = res;
        responseHandler(res);
        setImmediate(() => {
          res.emit('data', Buffer.from('A'.repeat(30), 'utf8'));
          res.emit('data', Buffer.from('B'.repeat(30), 'utf8')); // 60 > 50 cap
          res.emit('data', Buffer.from('C'.repeat(30), 'utf8')); // post-cap drop
          res.emit('end');
        });
      };
      req.destroy = () => {};
      return req;
    };
    const logger = makeLogger();
    const client = createControlClient({
      hmac, logger, httpRequest: fakeRequest, responseByteCap: 50,
    });
    const result = await client.pushHandoff(validArgs);
    expect(result).toEqual({
      ok: false, reason: 'http_error', error: 'response_body_too_large',
    });
    expect(capturedRes.destroyed).toBe(true);
    const capLogs = logger.warn.mock.calls.filter(
      ([msg]) => msg === 'control-client: response body exceeded cap',
    );
    expect(capLogs).toHaveLength(1);
  });

  it('returns reason:http_error when the response is aborted mid-stream', async () => {
    const hmac = makeHmac();
    const { fakeRequest } = makeFakeHttpRequest({
      behavior: (ctx) => ctx.respondThenAbort(200),
    });
    const logger = makeLogger();
    const client = createControlClient({ hmac, logger, httpRequest: fakeRequest });
    const result = await client.pushHandoff(validArgs);
    expect(result).toEqual({
      ok: false, reason: 'http_error', error: 'response_aborted',
    });
    expect(logger.warn).toHaveBeenCalledWith(
      'control-client: response aborted',
      expect.objectContaining({ peerInstanceId: 'inst-B' }),
    );
  });

  it('returns reason:http_error when the connection aborts BEFORE headers (req error path)', async () => {
    const hmac = makeHmac();
    const { fakeRequest } = makeFakeHttpRequest({
      behavior: (ctx) => ctx.error(new Error('ECONNRESET')),
    });
    const client = createControlClient({ hmac, logger: makeLogger(), httpRequest: fakeRequest });
    const result = await client.pushHandoff(validArgs);
    expect(result).toEqual({ ok: false, reason: 'http_error', error: 'ECONNRESET' });
  });

  it('settles exactly once when timeout + error both fire', async () => {
    const hmac = makeHmac();
    const { fakeRequest } = makeFakeHttpRequest({
      behavior: (ctx) => {
        ctx.timeout();
        setImmediate(() => ctx.error(new Error('handoff_timeout')));
      },
    });
    const client = createControlClient({ hmac, logger: makeLogger(), httpRequest: fakeRequest });
    const result = await client.pushHandoff(validArgs);
    expect(result.reason).toBe('timeout');
  });
});

describe('end-to-end — client → server', () => {
  let server;
  let port;
  const onHandoff = jest.fn(async () => {});

  beforeEach(() => new Promise((resolve) => {
    const hmac = makeHmac();
    server = startControlChannelServer({
      hmac,
      selfInstanceId: 'inst-B',
      isKnownPeer: (id) => id === 'inst-A',
      onHandoff,
      logger: makeLogger(),
      port: 0,
      bindAddr: '127.0.0.1',
      onListenError: () => {},
    });
    server.on('listening', () => {
      port = server.address().port;
      resolve();
    });
  }));

  afterEach(() => new Promise((resolve) => {
    onHandoff.mockClear();
    server.close(() => resolve());
  }));

  it('round-trips a real handoff through the real HTTP path', async () => {
    const hmac = makeHmac();
    const client = createControlClient({ hmac, logger: makeLogger() });
    const result = await client.pushHandoff({
      peerIp: '127.0.0.1',
      peerPort: port,
      peerInstanceId: 'inst-B',
      selfInstanceId: 'inst-A',
      expectedVersion: 7,
    });
    expect(result).toEqual({ ok: true, status: 200 });
    expect(onHandoff).toHaveBeenCalledWith({
      activeInstanceId: 'inst-A', expectedVersion: 7,
    });
  });
});

describe('end-to-end — IPv6 loopback (::1)', () => {
  let server;
  let port;
  let ipv6Available = false;
  const onHandoff = jest.fn(async () => {});

  beforeAll(async () => {
    ipv6Available = await new Promise((resolve) => {
      const probe = require('node:http').createServer();
      probe.once('error', () => resolve(false));
      probe.listen(0, '::1', () => {
        probe.close(() => resolve(true));
      });
    });
  });

  beforeEach(() => {
    if (!ipv6Available) return undefined;
    return new Promise((resolve) => {
      const hmac = makeHmac();
      server = startControlChannelServer({
        hmac,
        selfInstanceId: 'inst-B',
        isKnownPeer: (id) => id === 'inst-A',
        onHandoff,
        logger: makeLogger(),
        port: 0,
        bindAddr: '::1',
        onListenError: () => {},
      });
      server.on('listening', () => {
        port = server.address().port;
        resolve();
      });
    });
  });

  afterEach(() => new Promise((resolve) => {
    onHandoff.mockClear();
    if (!server) { resolve(); return; }
    server.close(() => resolve());
    server = null;
  }));

  it('round-trips a handoff over ::1', async () => {
    if (!ipv6Available) {
      // eslint-disable-next-line no-console
      console.warn('IPv6 loopback unavailable; skipping ::1 e2e test');
      return;
    }
    const hmac = makeHmac();
    const client = createControlClient({ hmac, logger: makeLogger() });
    const result = await client.pushHandoff({
      peerIp: '::1',
      peerPort: port,
      peerInstanceId: 'inst-B',
      selfInstanceId: 'inst-A',
      expectedVersion: 7,
    });
    expect(result).toEqual({ ok: true, status: 200 });
    expect(onHandoff).toHaveBeenCalledWith({
      activeInstanceId: 'inst-A', expectedVersion: 7,
    });
  });
});
