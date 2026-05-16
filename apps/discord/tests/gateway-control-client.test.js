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
    req.end = (chunk) => {
      if (chunk) writtenChunks.push(chunk);
      const ctx = {
        options,
        body: Buffer.concat(writtenChunks),
        respond(status, body) {
          const res = new EventEmitter();
          res.statusCode = status;
          res.destroyed = false;
          // Real http.IncomingMessage exposes .destroy() — the client
          // calls it on body-cap-exceeded to stop the stream.
          res.destroy = () => { res.destroyed = true; };
          responseHandler(res);
          setImmediate(() => {
            res.emit('data', Buffer.from(body ?? '', 'utf8'));
            res.emit('end');
          });
        },
        respondThenAbort(status) {
          // Headers + partial body arrive, then peer crashes. Node 17+
          // (after `aborted` was deprecated) signals this as `close`
          // on the response with `destroyed=true` and a statusCode
          // already set from headers — that's what the client listens
          // for now. See the comment in gateway-control-client.js's
          // res.on('close') handler for why.
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
    // The "never throws" contract documented in the module header is
    // load-bearing for the SIGTERM caller. Validators must surface as
    // result objects, not rejected promises.
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
    // The heartbeat-side validator (gateway-peer-heartbeat.js) uses
    // net.isIP() to reject hostnames + the literal "undefined" from
    // env-stringification. The client mirrors that check so a
    // corrupted row (or pre-13b.2 callers that bypassed the write-
    // time validator) doesn't get a free DNS resolution + POST to
    // an arbitrary host. Returned as result object (see "never
    // throws" contract in the module header).
    const hmac = makeHmac();
    const client = createControlClient({ hmac, logger: makeLogger() });
    for (const bad of ['discord.com', 'localhost', 'undefined', 'not-an-ip', '10.0.0', '10.0.0.0.0']) {
      // eslint-disable-next-line no-await-in-loop
      await expect(client.pushHandoff({ ...validArgs, peerIp: bad }))
        .resolves.toMatchObject({ ok: false, reason: 'invalid_arg', arg: 'peerIp' });
    }
    // IPv4 + IPv6 literals pass validation. Use a fake httpRequest
    // so the assertion runs without actually opening a socket.
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

  it('caps response body and returns reason:http_error on cap exceeded', async () => {
    // Defense vs OOM during SIGTERM: a misrouted POST hitting an
    // HTML error page (or hostile in-VPC actor) could otherwise
    // return multi-MB. The client must settle as http_error before
    // buffering grows unbounded.
    const hmac = makeHmac();
    const { fakeRequest } = makeFakeHttpRequest({
      behavior: (ctx) => {
        // Simulate a streaming response of chunks that together
        // exceed the configured cap. We send via the 'respond'
        // helper which fires data + end, but we want to drive
        // multiple data chunks. Reach inside to do it manually.
        const res = new (require('events').EventEmitter)();
        res.statusCode = 200;
        ctx.options; // no-op — keep ctx referenced
        // The responseHandler lives in the closure of httpRequest;
        // simplest path is to use the 'respond' helper with a giant
        // body so the single data emit > cap.
        ctx.respond(200, 'X'.repeat(100));
      },
    });
    const logger = makeLogger();
    // Set a tiny cap so a 100-byte response trips it.
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

  it('returns reason:http_error when the response is aborted mid-stream', async () => {
    // Peer sends headers then crashes — `aborted` event fires but
    // `end` never will. Without an aborted handler we'd block on
    // the 200 ms timeout; with it we settle immediately.
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

describe('end-to-end — IPv6 loopback (::1)', () => {
  // peer-heartbeat rows can carry IPv6 literals; pushHandoff's
  // validator accepts ::1, and node:http handles bare-IPv6 hostname
  // correctly. This test closes the loop end-to-end on v6 so a
  // future deploy onto an IPv6-only ENI doesn't surface an
  // untested wire path.
  //
  // Some CI environments disable IPv6 entirely. Probe ::1 bindability
  // in beforeAll and skip the suite cleanly if unavailable.
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
