// Unit tests for src/gateway-control-channel.js — Pillar 3 push-
// handoff receiver. Pins the load-bearing contracts:
//
//   1. Only POST /control/yours is accepted; everything else → 404.
//   2. Body cap enforced before parse; oversize → 413 with no parse,
//      no onHandoff call, no nonce burn.
//   3. Invalid envelope (non-JSON / missing body+signature) → 400
//      without invoking hmac.verify (so an attacker can't even
//      probe whether a key matches without a well-formed envelope).
//   4. HMAC verify runs BEFORE payload-shape / routing checks.
//      Bad signature → 401; stale/replay → 401 with the verifier's
//      reason field surfaced.
//   5. Routing checks (peer_instance_id binding + isKnownPeer)
//      enforced AFTER verify; 400 for mismatch.
//   6. onHandoff is awaited; thrown errors → 500. ACK 200 only
//      after onHandoff resolves (the "I'm live" semantic from the
//      design doc).
//   7. expected_version is validated as a positive integer; non-
//      integer / negative / zero → 400 invalid_payload.
//
// Test architecture: most tests drive the exported _handleRequestForTest
// with stub req/res to keep them fast and deterministic. A small set
// of end-to-end tests bind a real ephemeral port + send via
// http.request to pin the listen path, body-cap streaming, and
// header timeouts.

const http = require('node:http');
const crypto = require('node:crypto');
const { Readable } = require('node:stream');

const {
  startControlChannelServer,
  _handleRequestForTest,
  DEFAULT_BODY_BYTE_CAP,
} = require('../src/gateway-control-channel');

const { createGatewayHmac, wrapEnvelope } = require('../src/gateway-hmac');

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

// Build a stub IncomingMessage from an in-memory Buffer body.
function makeReq({ method = 'POST', url = '/control/yours', body = Buffer.alloc(0) } = {}) {
  const req = Readable.from([body]);
  req.method = method;
  req.url = url;
  req.headers = {};
  // The handler may call req.destroy() on body-too-large; the
  // Readable.from stream implements destroy as a no-op for our needs.
  return req;
}

function makeRes() {
  const chunks = [];
  const res = {
    headersSent: false,
    writableEnded: false,
    statusCode: null,
    headers: null,
    body: null,
    writeHead(status, headers) {
      this.statusCode = status;
      this.headers = headers;
      this.headersSent = true;
    },
    end(buf) {
      if (buf) chunks.push(buf);
      this.body = Buffer.concat(chunks).toString('utf8');
      this.writableEnded = true;
    },
  };
  return res;
}

function makeCtx({
  hmac, selfInstanceId = 'inst-B',
  isKnownPeer = () => true,
  onHandoff = jest.fn(async () => {}),
  bodyByteCap = DEFAULT_BODY_BYTE_CAP,
} = {}) {
  return {
    hmac: hmac ?? makeHmac(),
    selfInstanceId,
    isKnownPeer,
    onHandoff,
    logger: makeLogger(),
    bodyByteCap,
  };
}

function makeSignedEnvelope({ payload }) {
  // Recompute the signature directly with the shared SECRET rather
  // than calling into a `makeHmac()` instance — that would couple
  // the fixture to the hmac module's nonce LRU state and require
  // careful clock injection on every call site.
  const bodyBytes = Buffer.from(JSON.stringify(payload), 'utf8');
  const signature = crypto.createHmac('sha256', SECRET).update(bodyBytes).digest('hex');
  return wrapEnvelope({ bodyBytes, signature }).toString('utf8');
}

function makeFreshPayload({
  now = 1_700_000_000_000,
  activeInstanceId = 'inst-A',
  peerInstanceId = 'inst-B',
  expectedVersion = 7,
  nonceChar = 'n',
} = {}) {
  return {
    ts: now,
    nonce: nonceChar.repeat(32),
    active_instance_id: activeInstanceId,
    peer_instance_id: peerInstanceId,
    expected_version: expectedVersion,
  };
}

describe('startControlChannelServer — factory validation', () => {
  it('throws on missing required deps', () => {
    expect(() => startControlChannelServer()).toThrow(/hmac/);
    expect(() => startControlChannelServer({ hmac: { verify() {} } }))
      .toThrow(/selfInstanceId/);
    expect(() => startControlChannelServer({ hmac: { verify() {} }, selfInstanceId: 'a' }))
      .toThrow(/isKnownPeer/);
    expect(() => startControlChannelServer({
      hmac: { verify() {} }, selfInstanceId: 'a', isKnownPeer: () => true,
    })).toThrow(/onHandoff/);
    expect(() => startControlChannelServer({
      hmac: { verify() {} }, selfInstanceId: 'a', isKnownPeer: () => true, onHandoff: () => {},
    })).toThrow(/logger/);
    expect(() => startControlChannelServer({
      hmac: { verify() {} }, selfInstanceId: 'a', isKnownPeer: () => true, onHandoff: () => {},
      logger: makeLogger(),
    })).toThrow(/onListenError/);
    expect(() => startControlChannelServer({
      hmac: { verify() {} }, selfInstanceId: 'a', isKnownPeer: () => true, onHandoff: () => {},
      logger: makeLogger(), onListenError: () => {},
    })).toThrow(/port/);
  });
});

describe('handleRequest — method + path routing', () => {
  it('404s any path other than /control/yours', async () => {
    const ctx = makeCtx();
    const req = makeReq({ method: 'POST', url: '/control/other' });
    const res = makeRes();
    await _handleRequestForTest(req, res, ctx);
    expect(res.statusCode).toBe(404);
    expect(JSON.parse(res.body)).toEqual({ error: 'not_found' });
    expect(ctx.onHandoff).not.toHaveBeenCalled();
  });

  it('405s GET /control/yours with an Allow: POST header (only POST is allowed)', async () => {
    const ctx = makeCtx();
    const req = makeReq({ method: 'GET' });
    const res = makeRes();
    await _handleRequestForTest(req, res, ctx);
    expect(res.statusCode).toBe(405);
    expect(JSON.parse(res.body)).toEqual({ error: 'method_not_allowed' });
    expect(res.headers).toMatchObject({ Allow: 'POST' });
  });

  it('strips query string before matching path', async () => {
    const now = 1_700_000_000_000;
    const hmac = makeHmac({ clock: () => now });
    const ctx = makeCtx({ hmac });
    const payload = makeFreshPayload({ now });
    const envelope = makeSignedEnvelope({ payload });

    const req = makeReq({ url: '/control/yours?cachebust=1', body: Buffer.from(envelope) });
    const res = makeRes();
    await _handleRequestForTest(req, res, ctx);
    expect(res.statusCode).toBe(200);
  });

  it.each([
    ['application/json-patch+json'],
    ['application/jsonp'],
    ['application/jsonish'],
    ['text/plain'],
    ['application/octet-stream'],
  ])('415s POST /control/yours with content-type %s', async (contentType) => {
    const ctx = makeCtx();
    const req = makeReq();
    req.headers['content-type'] = contentType;
    const res = makeRes();
    await _handleRequestForTest(req, res, ctx);
    expect(res.statusCode).toBe(415);
    expect(JSON.parse(res.body)).toEqual({ error: 'unsupported_media_type' });
    expect(ctx.onHandoff).not.toHaveBeenCalled();
  });

  it.each([
    ['application/json'],
    ['application/json; charset=utf-8'],
    ['Application/JSON'],
    ['application/json;charset=UTF-8'],
  ])('accepts content-type %s (matched by prefix)', async (contentType) => {
    const now = 1_700_000_000_000;
    const hmac = makeHmac({ clock: () => now });
    const ctx = makeCtx({ hmac });
    const payload = makeFreshPayload({ now });
    const envelope = makeSignedEnvelope({ payload });

    const req = makeReq({ body: Buffer.from(envelope) });
    req.headers['content-type'] = contentType;
    const res = makeRes();
    await _handleRequestForTest(req, res, ctx);
    expect(res.statusCode).toBe(200);
  });
});

describe('handleRequest — body cap', () => {
  it('413s when body exceeds bodyByteCap (and never calls onHandoff)', async () => {
    const ctx = makeCtx({ bodyByteCap: 100 });
    const big = Buffer.alloc(200, 0x61); // 200 bytes
    const req = makeReq({ body: big });
    const res = makeRes();
    await _handleRequestForTest(req, res, ctx);
    expect(res.statusCode).toBe(413);
    expect(JSON.parse(res.body)).toEqual({ error: 'body_too_large' });
    expect(ctx.onHandoff).not.toHaveBeenCalled();
  });

  it('accepts a body exactly at bodyByteCap (boundary: cap = "max allowed", not "first rejected")', async () => {
    // Off-by-one pin: the cap is the largest accepted size. A body
    // of exactly `cap` bytes must NOT 413. (`> cap` rejects, `>=
    // cap` would reject the boundary — wrong shape.)
    const ctx = makeCtx({ bodyByteCap: 100 });
    // Body must be a valid signed envelope to reach the 200 path,
    // so we build a tiny payload and verify the envelope fits.
    const now = 1_700_000_000_000;
    const hmac = makeHmac({ clock: () => now });
    ctx.hmac = hmac;
    const payload = makeFreshPayload({ now });
    const envelope = makeSignedEnvelope({ payload });
    // Pin the precondition: the envelope must be longer than cap
    // for the body-to-cap to mean anything; if not, this is a
    // useful test of "small envelope passes," which is also fine.
    const padded = Buffer.alloc(100, 0x20); // 100 bytes of spaces
    padded.write(envelope.slice(0, Math.min(envelope.length, 100)), 0);
    // Either case (passes verify or fails verify) reaches a status
    // code that isn't 413. The contract under test is "at-cap is
    // not body_too_large."
    const req = makeReq({ body: padded });
    const res = makeRes();
    await _handleRequestForTest(req, res, ctx);
    expect(res.statusCode).not.toBe(413);
  });

  it('413s when cumulative chunks exceed bodyByteCap (streaming path)', async () => {
    // Real HTTP streams chunk delivery (~16 KB at a time). A bug in
    // the cumulative-counter that only checked the LAST chunk would
    // miss a body where each individual chunk is under cap but the
    // sum is over. Drive that path with a multi-chunk stream.
    const ctx = makeCtx({ bodyByteCap: 100 });
    const chunk1 = Buffer.alloc(60, 0x61); // 60 bytes — under cap on its own
    const chunk2 = Buffer.alloc(60, 0x62); // 60 bytes — but 120 cumulative
    const req = Readable.from([chunk1, chunk2]);
    req.method = 'POST';
    req.url = '/control/yours';
    req.headers = {};
    const res = makeRes();
    await _handleRequestForTest(req, res, ctx);
    expect(res.statusCode).toBe(413);
    expect(ctx.onHandoff).not.toHaveBeenCalled();
  });
});

describe('handleRequest — envelope shape', () => {
  it('400s non-JSON body', async () => {
    const ctx = makeCtx();
    const req = makeReq({ body: Buffer.from('not-json') });
    const res = makeRes();
    await _handleRequestForTest(req, res, ctx);
    expect(res.statusCode).toBe(400);
    expect(JSON.parse(res.body)).toEqual({ error: 'invalid_envelope' });
    expect(ctx.onHandoff).not.toHaveBeenCalled();
  });

  it('400s envelope missing body or signature', async () => {
    const ctx = makeCtx();
    for (const env of [{}, { body: 'x' }, { signature: 'y' }, { body: 42, signature: 'y' }]) {
      const req = makeReq({ body: Buffer.from(JSON.stringify(env)) });
      const res = makeRes();
      // eslint-disable-next-line no-await-in-loop
      await _handleRequestForTest(req, res, ctx);
      expect(res.statusCode).toBe(400);
    }
  });
});

describe('handleRequest — HMAC verify', () => {
  it('200s a well-formed signed body with valid routing', async () => {
    const now = 1_700_000_000_000;
    const hmac = makeHmac({ clock: () => now });
    const ctx = makeCtx({ hmac });
    const payload = makeFreshPayload({ now });
    const envelope = makeSignedEnvelope({ payload });

    const req = makeReq({ body: Buffer.from(envelope) });
    const res = makeRes();
    await _handleRequestForTest(req, res, ctx);

    expect(res.statusCode).toBe(200);
    expect(JSON.parse(res.body)).toEqual({ status: 'ok' });
    expect(ctx.onHandoff).toHaveBeenCalledWith({
      activeInstanceId: 'inst-A',
      expectedVersion: 7,
    });
  });

  it('401s a tampered signature with the verifier reason', async () => {
    const now = 1_700_000_000_000;
    const hmac = makeHmac({ clock: () => now });
    const ctx = makeCtx({ hmac });
    const payload = makeFreshPayload({ now });
    const bodyStr = JSON.stringify(payload);
    const envelope = JSON.stringify({
      body: bodyStr,
      signature: 'f'.repeat(64),
    });
    const req = makeReq({ body: Buffer.from(envelope) });
    const res = makeRes();
    await _handleRequestForTest(req, res, ctx);
    expect(res.statusCode).toBe(401);
    expect(JSON.parse(res.body)).toEqual({ error: 'unauthorized', reason: 'bad_signature' });
    expect(ctx.onHandoff).not.toHaveBeenCalled();
  });

  it('401s a stale body with reason=stale', async () => {
    let now = 1_700_000_000_000;
    const hmac = makeHmac({ clock: () => now });
    const ctx = makeCtx({ hmac });
    const payload = makeFreshPayload({ now });
    const envelope = makeSignedEnvelope({ payload });
    now += 10_000; // 10s past freshness window
    const req = makeReq({ body: Buffer.from(envelope) });
    const res = makeRes();
    await _handleRequestForTest(req, res, ctx);
    expect(res.statusCode).toBe(401);
    expect(JSON.parse(res.body)).toEqual({ error: 'unauthorized', reason: 'stale' });
  });

  it('401s a replay with reason=replay', async () => {
    const now = 1_700_000_000_000;
    const hmac = makeHmac({ clock: () => now });
    const ctx = makeCtx({ hmac });
    const payload = makeFreshPayload({ now });
    const envelope = makeSignedEnvelope({ payload });

    // First call succeeds.
    const req1 = makeReq({ body: Buffer.from(envelope) });
    const res1 = makeRes();
    await _handleRequestForTest(req1, res1, ctx);
    expect(res1.statusCode).toBe(200);

    // Second call (same envelope, same nonce) is rejected as replay.
    const req2 = makeReq({ body: Buffer.from(envelope) });
    const res2 = makeRes();
    await _handleRequestForTest(req2, res2, ctx);
    expect(res2.statusCode).toBe(401);
    expect(JSON.parse(res2.body)).toEqual({ error: 'unauthorized', reason: 'replay' });
  });
});

describe('handleRequest — routing checks (after HMAC verify)', () => {
  it('400s when peer_instance_id does not match self', async () => {
    const now = 1_700_000_000_000;
    const hmac = makeHmac({ clock: () => now });
    const ctx = makeCtx({ hmac, selfInstanceId: 'inst-B' });
    const payload = makeFreshPayload({ now, peerInstanceId: 'inst-C' });
    const envelope = makeSignedEnvelope({ payload });

    const req = makeReq({ body: Buffer.from(envelope) });
    const res = makeRes();
    await _handleRequestForTest(req, res, ctx);
    expect(res.statusCode).toBe(400);
    expect(JSON.parse(res.body)).toEqual({ error: 'wrong_peer' });
    expect(ctx.onHandoff).not.toHaveBeenCalled();
  });

  it('400s when active_instance_id is not a known peer', async () => {
    const now = 1_700_000_000_000;
    const hmac = makeHmac({ clock: () => now });
    const isKnownPeer = jest.fn(() => false);
    const ctx = makeCtx({ hmac, isKnownPeer });
    const payload = makeFreshPayload({ now });
    const envelope = makeSignedEnvelope({ payload });

    const req = makeReq({ body: Buffer.from(envelope) });
    const res = makeRes();
    await _handleRequestForTest(req, res, ctx);
    expect(res.statusCode).toBe(400);
    expect(JSON.parse(res.body)).toEqual({ error: 'unknown_peer' });
    expect(isKnownPeer).toHaveBeenCalledWith('inst-A');
    expect(ctx.onHandoff).not.toHaveBeenCalled();
  });

  it('400s when expected_version is non-positive-integer or wrong type', async () => {
    const now = 1_700_000_000_000;
    const hmac = makeHmac({ clock: () => now });
    let i = 0;
    for (const bad of [0, -1, 1.5, '7']) {
      const payload = makeFreshPayload({ now, expectedVersion: bad });
      // Distinct nonce per case so the LRU doesn't false-fail later
      // iterations as replay.
      i += 1;
      payload.nonce = `bad${i}`.padEnd(32, 'x');
      const envelope = makeSignedEnvelope({ payload });
      const ctx = makeCtx({ hmac });
      const req = makeReq({ body: Buffer.from(envelope) });
      const res = makeRes();
      // eslint-disable-next-line no-await-in-loop
      await _handleRequestForTest(req, res, ctx);
      expect(res.statusCode).toBe(400);
      expect(JSON.parse(res.body)).toEqual({ error: 'invalid_payload' });
    }
  });

  it('400s when expected_version is missing entirely', async () => {
    const now = 1_700_000_000_000;
    const hmac = makeHmac({ clock: () => now });
    // Build the payload directly so we can omit expected_version
    // without going through makeFreshPayload's default.
    const payload = {
      ts: now,
      nonce: 'miss'.padEnd(32, 'y'),
      active_instance_id: 'inst-A',
      peer_instance_id: 'inst-B',
    };
    const envelope = makeSignedEnvelope({ payload });
    const ctx = makeCtx({ hmac });
    const req = makeReq({ body: Buffer.from(envelope) });
    const res = makeRes();
    await _handleRequestForTest(req, res, ctx);
    expect(res.statusCode).toBe(400);
    expect(JSON.parse(res.body)).toEqual({ error: 'invalid_payload' });
  });
});

describe('handleRequest — onHandoff', () => {
  it('awaits onHandoff before ACKing (200 means "standby is live")', async () => {
    const now = 1_700_000_000_000;
    const hmac = makeHmac({ clock: () => now });
    let resolvedAt = null;
    let respondedAt = null;
    const onHandoff = jest.fn(async () => {
      await new Promise((resolve) => { setImmediate(resolve); });
      resolvedAt = Date.now();
    });
    const ctx = makeCtx({ hmac, onHandoff });
    const payload = makeFreshPayload({ now });
    const envelope = makeSignedEnvelope({ payload });

    const req = makeReq({ body: Buffer.from(envelope) });
    const res = makeRes();
    const originalEnd = res.end.bind(res);
    res.end = (buf) => {
      respondedAt = Date.now();
      return originalEnd(buf);
    };

    await _handleRequestForTest(req, res, ctx);
    expect(res.statusCode).toBe(200);
    expect(resolvedAt).not.toBeNull();
    expect(respondedAt).toBeGreaterThanOrEqual(resolvedAt);
  });

  it('500s when onHandoff throws', async () => {
    const now = 1_700_000_000_000;
    const hmac = makeHmac({ clock: () => now });
    const onHandoff = jest.fn(async () => { throw new Error('connect-rejected'); });
    const ctx = makeCtx({ hmac, onHandoff });
    const payload = makeFreshPayload({ now });
    const envelope = makeSignedEnvelope({ payload });

    const req = makeReq({ body: Buffer.from(envelope) });
    const res = makeRes();
    await _handleRequestForTest(req, res, ctx);
    expect(res.statusCode).toBe(500);
    expect(JSON.parse(res.body)).toEqual({ error: 'handoff_failed' });
    expect(ctx.logger.error).toHaveBeenCalledWith(
      'control-channel: onHandoff threw',
      expect.objectContaining({ error: 'connect-rejected' }),
    );
  });
});

describe('handleRequest — body cap response race (real socket)', () => {
  let server;
  let port;

  beforeEach(() => new Promise((resolve) => {
    const hmac = makeHmac();
    server = startControlChannelServer({
      hmac,
      selfInstanceId: 'inst-B',
      isKnownPeer: () => true,
      onHandoff: jest.fn(async () => {}),
      logger: makeLogger(),
      port: 0,
      bindAddr: '127.0.0.1',
      bodyByteCap: 100,
      onListenError: () => {},
    });
    server.on('listening', () => {
      port = server.address().port;
      resolve();
    });
  }));

  afterEach(() => new Promise((resolve) => {
    server.close(() => resolve());
  }));

  it('returns the 413 response to the client even after the body exceeds the cap (no socket-destroy race)', async () => {
    // Earlier shape used req.destroy() on over-cap which tore down
    // the socket BEFORE the catch handler could write the 413 —
    // legitimate over-cap clients never saw the response. The fix
    // uses req.pause(). Drive a real HTTP request that streams
    // bytes past the cap and assert we get a 413 status code +
    // JSON body, not a connection-reset error.
    const result = await new Promise((resolve, reject) => {
      const big = Buffer.alloc(500, 0x61); // 5× the cap
      const req = http.request({
        host: '127.0.0.1', port, path: '/control/yours', method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'Content-Length': big.length,
        },
      }, (res) => {
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () => resolve({
          status: res.statusCode,
          body: Buffer.concat(chunks).toString('utf8'),
        }));
      });
      req.on('error', reject);
      req.write(big);
      req.end();
    });
    expect(result.status).toBe(413);
    expect(JSON.parse(result.body)).toEqual({ error: 'body_too_large' });
  });
});

describe('end-to-end — real HTTP server on ephemeral port', () => {
  let server;
  let port;

  function start({ hmac, selfInstanceId = 'inst-B', isKnownPeer = () => true, onHandoff }) {
    const logger = makeLogger();
    server = startControlChannelServer({
      hmac, selfInstanceId, isKnownPeer, onHandoff,
      logger, port: 0, bindAddr: '127.0.0.1',
      onListenError: () => {},
    });
    return new Promise((resolve) => {
      server.on('listening', () => {
        port = server.address().port;
        resolve();
      });
    });
  }

  afterEach(() => new Promise((resolve) => {
    if (!server) { resolve(); return; }
    server.close(() => resolve());
    server = null;
  }));

  function post(path, body) {
    return new Promise((resolve, reject) => {
      const req = http.request({
        host: '127.0.0.1', port, path, method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) },
      }, (res) => {
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () => resolve({
          status: res.statusCode,
          body: Buffer.concat(chunks).toString('utf8'),
        }));
      });
      req.on('error', reject);
      req.write(body);
      req.end();
    });
  }

  it('round-trips a valid signed handoff body to onHandoff', async () => {
    const now = 1_700_000_000_000;
    const hmac = makeHmac({ clock: () => now });
    const onHandoff = jest.fn(async () => {});
    await start({ hmac, onHandoff });

    const payload = makeFreshPayload({ now });
    const envelope = makeSignedEnvelope({ payload });
    const result = await post('/control/yours', envelope);
    expect(result.status).toBe(200);
    expect(onHandoff).toHaveBeenCalledWith({
      activeInstanceId: 'inst-A', expectedVersion: 7,
    });
  });

  it('404s an unknown path', async () => {
    const hmac = makeHmac();
    await start({ hmac, onHandoff: jest.fn() });
    const result = await post('/health', '{}');
    expect(result.status).toBe(404);
  });
});
