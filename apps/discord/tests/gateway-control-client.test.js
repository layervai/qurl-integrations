// Unit tests for src/gateway-control-client.js — Pillar 3 outbound
// push-handoff. Pins the load-bearing contracts:
//
//   1. Builds a signed envelope via hmac.sign + wrapEnvelope. ts +
//      nonce are added by the client; caller passes only routing
//      + version.
//   2. POSTs to peer's /control/yours with the wire envelope as
//      body and a Content-Length header.
//   3. Returns a result OBJECT, never throws. 2xx → ok:true;
//      timeout → reason:'timeout'; non-2xx → reason:'rejected';
//      transport/error → reason:'http_error'.
//   4. Per-call timeout (default 200 ms) bounds the SIGTERM stall.
//   5. End-to-end with the real control-channel server: the
//      generated envelope is accepted by hmac.verify and routed
//      to onHandoff with the right activeInstanceId + expectedVersion.

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

// Build a fake http.request that captures the options + body and
// drives the (response, timeout, error) lifecycle via test hooks.
function makeFakeHttpRequest({ behavior }) {
  // `behavior` is invoked with the captured call context and decides
  // how to settle: respond, timeout, or error.
  const calls = [];

  function fakeRequest(options, responseHandler) {
    const req = new EventEmitter();
    const writtenChunks = [];
    req.write = (chunk) => { writtenChunks.push(chunk); };
    req.end = () => {
      const ctx = {
        options,
        body: Buffer.concat(writtenChunks),
        respond(status, body) {
          const res = new EventEmitter();
          res.statusCode = status;
          responseHandler(res);
          setImmediate(() => {
            res.emit('data', Buffer.from(body ?? '', 'utf8'));
            res.emit('end');
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
  it('rejects missing required args', async () => {
    const hmac = makeHmac();
    const client = createControlClient({ hmac, logger: makeLogger() });
    await expect(client.pushHandoff({})).rejects.toThrow(/required/);
    await expect(client.pushHandoff({ ...validArgs, peerIp: '' })).rejects.toThrow(/required/);
    await expect(client.pushHandoff({ ...validArgs, expectedVersion: 0 })).rejects.toThrow(/required/);
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
      host: '10.0.1.5',
      port: 9876,
      path: '/control/yours',
      method: 'POST',
    });
    expect(calls[0].options.headers).toMatchObject({
      'Content-Type': 'application/json',
      'Content-Length': calls[0].body.length,
    });
    // The body should unwrap cleanly into bodyBytes + signature.
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

  it('settles exactly once when timeout + error both fire', async () => {
    // Real http: timeout event fires THEN we destroy(err) THEN the
    // 'error' event fires. Result must be timeout, not http_error.
    const hmac = makeHmac();
    const { fakeRequest } = makeFakeHttpRequest({
      behavior: (ctx) => {
        ctx.timeout();
        // simulate the post-destroy error event
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
