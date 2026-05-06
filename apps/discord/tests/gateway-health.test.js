/**
 * Tests for the gateway-only `/health` listener (#151). The full
 * Express server has its own /health with db.healthCheck — this is
 * the minimal `node:http` responder that runs only in `isGateway &&
 * !isHttp` containers, where the wget probe needs SOMETHING to hit
 * before ECS marks the task healthy.
 *
 * Coverage focuses on the contract the wget probe relies on:
 *   - 200 when client.isReady()
 *   - 503 when not (so a wedged WebSocket fails the probe)
 *   - 404 on any other path (no surprises for a misconfigured probe)
 *   - server.close() drains the listener (graceful-shutdown path
 *     in index.js calls this)
 */
const http = require('node:http');
const { startGatewayHealthServer } = require('../src/gateway-health');

// Disable real logger output for deterministic test output. The
// `info` log on listen() isn't load-bearing — silencing it keeps
// jest --verbose readable.
jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

// Bind to ephemeral port to avoid collisions with concurrent test
// runs or a real bot listening on 3000 during local dev. Override
// `config.PORT` via env BEFORE require — `config.js` reads at module-
// load time, and `startGatewayHealthServer` reads `config.PORT` once
// per invocation, so this works.
jest.mock('../src/config', () => {
  const actual = jest.requireActual('../src/config');
  return { ...actual, PORT: 0 }; // 0 = OS-assigned ephemeral port
});

// Default no-op listen-error handler for tests that don't exercise
// the EADDRINUSE path. `onListenError` is required (no signature
// default) so every test has to make this choice explicit. The
// EADDRINUSE test passes its own jest.fn() to assert invocation.
const noopOnListenError = () => {};

function request(server, path, method = 'GET') {
  const { port } = server.address();
  return new Promise((resolve, reject) => {
    const req = http.request(
      { hostname: '127.0.0.1', port, path, method },
      (res) => {
        let body = '';
        res.on('data', (chunk) => { body += chunk; });
        res.on('end', () => resolve({
          status: res.statusCode,
          body,
          contentType: res.headers['content-type'],
        }));
      },
    );
    // 2s cap so a wedged listener fails with a clear timeout rather
    // than hanging until jest's outer 5s default kills the test with
    // a generic message.
    req.setTimeout(2000, () => req.destroy(new Error('request timeout')));
    req.on('error', reject);
    req.end();
  });
}

function waitForListening(server) {
  return new Promise((resolve) => {
    if (server.listening) return resolve();
    server.once('listening', resolve);
  });
}

function closeServer(server) {
  return new Promise((resolve) => server.close(() => resolve()));
}

describe('gateway-health server', () => {
  beforeEach(() => jest.clearAllMocks());
  test('GET /health returns 200 ok when isReady() is true', async () => {
    const server = startGatewayHealthServer(() => true, noopOnListenError);
    await waitForListening(server);
    try {
      const { status, body, contentType } = await request(server, '/health');
      expect(status).toBe(200);
      expect(contentType).toBe('application/json');
      expect(JSON.parse(body)).toEqual({ status: 'ok' });
    } finally {
      await closeServer(server);
    }
  });

  test('GET /health returns 503 unhealthy when isReady() is false', async () => {
    const server = startGatewayHealthServer(() => false, noopOnListenError);
    await waitForListening(server);
    try {
      const { status, body, contentType } = await request(server, '/health');
      expect(status).toBe(503);
      expect(contentType).toBe('application/json');
      expect(JSON.parse(body)).toEqual({ status: 'unhealthy' });
    } finally {
      await closeServer(server);
    }
  });

  test('every 503 emits a gateway_health_unhealthy audit event (count per probe)', async () => {
    // A wedge persisting for N probes should produce N count events
    // for the alarm, not one transition log. Pin the per-probe emission
    // so a future refactor can't collapse it back into a transition-only
    // signal. Pinning `reason: 'not_ready'` keeps the dashboard split
    // intact if a future caller forgets to pass it.
    const mockLogger = require('../src/logger');
    const { AUDIT_EVENTS } = require('../src/constants');
    const server = startGatewayHealthServer(() => false, noopOnListenError);
    await waitForListening(server);
    try {
      mockLogger.audit.mockClear();
      await request(server, '/health');
      await request(server, '/health');
      await request(server, '/health');
      const unhealthyCalls = mockLogger.audit.mock.calls.filter(
        ([event]) => event === AUDIT_EVENTS.GATEWAY_HEALTH_UNHEALTHY,
      );
      expect(unhealthyCalls).toHaveLength(3);
      for (const call of unhealthyCalls) {
        expect(call[1]).toEqual({ reason: 'not_ready' });
      }
    } finally {
      await closeServer(server);
    }
  });

  test('isReady() throw path emits gateway_health_unhealthy with reason=sampler_threw', async () => {
    // Distinguishes a closure bug under load from a clean WebSocket
    // disconnect — both produce 503 + emit, but the reason field
    // splits them on the dashboard. Without this test, a future
    // refactor moving the audit call inside the non-throw branch
    // would silently drop the most operationally-interesting wedge.
    const mockLogger = require('../src/logger');
    const { AUDIT_EVENTS } = require('../src/constants');
    const server = startGatewayHealthServer(() => { throw new Error('boom'); }, noopOnListenError);
    await waitForListening(server);
    try {
      mockLogger.audit.mockClear();
      const { status } = await request(server, '/health');
      expect(status).toBe(503);
      const unhealthyCalls = mockLogger.audit.mock.calls.filter(
        ([event]) => event === AUDIT_EVENTS.GATEWAY_HEALTH_UNHEALTHY,
      );
      expect(unhealthyCalls).toHaveLength(1);
      expect(unhealthyCalls[0][1]).toEqual({ reason: 'sampler_threw' });
    } finally {
      await closeServer(server);
    }
  });

  test('HEAD /health unhealthy ALSO emits gateway_health_unhealthy', async () => {
    // Some load-balancer probes default to HEAD. The emit must fire
    // regardless of method — if a future change adds a HEAD early-
    // return for body-strip optimization, the emit would silently
    // drop and a HEAD-probing LB would never trigger the alarm.
    const mockLogger = require('../src/logger');
    const { AUDIT_EVENTS } = require('../src/constants');
    const server = startGatewayHealthServer(() => false, noopOnListenError);
    await waitForListening(server);
    try {
      mockLogger.audit.mockClear();
      const head = await request(server, '/health', 'HEAD');
      expect(head.status).toBe(503);
      const unhealthyCalls = mockLogger.audit.mock.calls.filter(
        ([event]) => event === AUDIT_EVENTS.GATEWAY_HEALTH_UNHEALTHY,
      );
      expect(unhealthyCalls).toHaveLength(1);
      expect(unhealthyCalls[0][1]).toEqual({ reason: 'not_ready' });
    } finally {
      await closeServer(server);
    }
  });

  test('200 (healthy) responses do NOT emit gateway_health_unhealthy', async () => {
    const mockLogger = require('../src/logger');
    const { AUDIT_EVENTS } = require('../src/constants');
    const server = startGatewayHealthServer(() => true, noopOnListenError);
    await waitForListening(server);
    try {
      mockLogger.audit.mockClear();
      await request(server, '/health');
      await request(server, '/health');
      const unhealthyCalls = mockLogger.audit.mock.calls.filter(
        ([event]) => event === AUDIT_EVENTS.GATEWAY_HEALTH_UNHEALTHY,
      );
      expect(unhealthyCalls).toHaveLength(0);
    } finally {
      await closeServer(server);
    }
  });

  test('isReady() flip from true → false changes response from 200 → 503', async () => {
    // Realistic shape: bot starts ready, WebSocket disconnects,
    // probe should immediately reflect the new state. The real
    // discord.js client.isReady() reads from the connection state on
    // every call, so the closure pattern guarantees no stale cache.
    const mockLogger = require('../src/logger');
    let ready = true;
    const server = startGatewayHealthServer(() => ready, noopOnListenError);
    await waitForListening(server);
    try {
      const ok = await request(server, '/health');
      expect(ok.status).toBe(200);
      ready = false;
      const fail = await request(server, '/health');
      expect(fail.status).toBe(503);
      // ok → unhealthy transition pins the warn log shape so an
      // operator can grep "Gateway health: ok → unhealthy" during
      // triage without depending on the request that was lost.
      expect(mockLogger.warn).toHaveBeenCalledWith('Gateway health: ok → unhealthy');
    } finally {
      await closeServer(server);
    }
  });

  test('unhealthy → ok transition logs info, recovery only fires once', async () => {
    // Mirror of the flip test, but for the recovery direction. Also
    // pins that subsequent ok-state probes don't re-log — the warn/
    // info should fire once per transition, not once per probe.
    const mockLogger = require('../src/logger');
    let ready = true;
    const server = startGatewayHealthServer(() => ready, noopOnListenError);
    await waitForListening(server);
    try {
      await request(server, '/health'); // sets prevReady=true (silent)
      ready = false;
      await request(server, '/health'); // ok → unhealthy (warn)
      ready = true;
      await request(server, '/health'); // unhealthy → ok (info)
      await request(server, '/health'); // still ok — no new log
      expect(mockLogger.info).toHaveBeenCalledWith('Gateway health: unhealthy → ok');
      // Exactly one info from the transition; the listen `info` log
      // is also there, so we filter to just the transition message.
      const transitionInfoCalls = mockLogger.info.mock.calls
        .filter(([msg]) => msg === 'Gateway health: unhealthy → ok');
      expect(transitionInfoCalls).toHaveLength(1);
    } finally {
      await closeServer(server);
    }
  });

  test('GET on a non-/health path returns 404', async () => {
    // Probe-misconfiguration safety: if the wget probe ever drifts
    // off /health (typo, copy-paste from another service's
    // healthcheck), we want a clean 404 not a coincidental 200.
    const server = startGatewayHealthServer(() => true, noopOnListenError);
    await waitForListening(server);
    try {
      const { status, body } = await request(server, '/');
      expect(status).toBe(404);
      expect(JSON.parse(body)).toEqual({ status: 'not_found' });
    } finally {
      await closeServer(server);
    }
  });

  test('HEAD /health returns 200 when isReady() is true', async () => {
    // Some load-balancer probes default to HEAD. HEAD is semantically
    // GET-without-body (RFC 9110 §9.3.2), so accepting it avoids
    // silent probe failure if the infra follow-up specifies HEAD.
    const server = startGatewayHealthServer(() => true, noopOnListenError);
    await waitForListening(server);
    try {
      const head = await request(server, '/health', 'HEAD');
      expect(head.status).toBe(200);
      // Node strips the body for HEAD automatically.
      expect(head.body).toBe('');
    } finally {
      await closeServer(server);
    }
  });

  test('POST /health returns 404', async () => {
    // Only GET and HEAD are accepted. Any other method is a
    // misconfiguration; treat the same as a wrong path.
    const server = startGatewayHealthServer(() => true, noopOnListenError);
    await waitForListening(server);
    try {
      const post = await request(server, '/health', 'POST');
      expect(post.status).toBe(404);
    } finally {
      await closeServer(server);
    }
  });

  test('/health?ts=123 returns 200 (query string tolerance)', async () => {
    // Some ECS/ALB probe configs append a cache-busting query string.
    // The path match strips the query before comparing so this
    // doesn't silently 404.
    const server = startGatewayHealthServer(() => true, noopOnListenError);
    await waitForListening(server);
    try {
      const { status, body } = await request(server, '/health?ts=123');
      expect(status).toBe(200);
      expect(JSON.parse(body)).toEqual({ status: 'ok' });
    } finally {
      await closeServer(server);
    }
  });

  test('isReady() throwing returns 503 instead of 500 and logs debug', async () => {
    // Future composite readiness closures might deref into something
    // nullable. The try/catch ensures a throw surfaces as 503
    // (unhealthy) rather than an unhandled 500. Pinned at debug so
    // an operator triaging an unexplained 503 rate has something to
    // grep for without spamming logs at probe cadence.
    const mockLogger = require('../src/logger');
    const server = startGatewayHealthServer(() => { throw new Error('boom'); }, noopOnListenError);
    await waitForListening(server);
    try {
      const { status, body } = await request(server, '/health');
      expect(status).toBe(503);
      expect(JSON.parse(body)).toEqual({ status: 'unhealthy' });
      expect(mockLogger.debug).toHaveBeenCalledWith(
        'Gateway health: isReady closure threw, treating as unhealthy',
        expect.objectContaining({ error: 'boom' }),
      );
    } finally {
      await closeServer(server);
    }
  });

  test('server.close() drains and exits cleanly', async () => {
    // Pinned because index.js's gracefulShutdown awaits httpServer.close()
    // — if the listener doesn't drain, SIGTERM hangs until ECS sends
    // SIGKILL. Confirms the listener honors the Node http.Server
    // close contract.
    const server = startGatewayHealthServer(() => true, noopOnListenError);
    await waitForListening(server);
    expect(server.listening).toBe(true);
    await closeServer(server);
    expect(server.listening).toBe(false);
  });

  test('EADDRINUSE surfaces as structured log and calls onListenError', async () => {
    // If the bind port is already in use (e.g. a stray combined-mode
    // process), the error handler should log a structured message
    // and invoke onListenError so index.js can route through
    // gracefulShutdown.
    const mockLogger = require('../src/logger');
    const onListenError = jest.fn();
    const first = startGatewayHealthServer(() => true, noopOnListenError);
    await waitForListening(first);

    let second;
    try {
      // Pass the occupied port directly instead of mutating the
      // shared config mock — avoids action-at-a-distance.
      const { port } = first.address();
      second = startGatewayHealthServer(() => true, onListenError, port);
      // Wait for the async listen error to fire.
      await new Promise((resolve) => { second.on('error', resolve); });

      expect(mockLogger.error).toHaveBeenCalledWith(
        'Gateway health listener failed',
        expect.objectContaining({ code: 'EADDRINUSE' }),
      );
      expect(onListenError).toHaveBeenCalledWith(
        expect.objectContaining({ code: 'EADDRINUSE' }),
      );
    } finally {
      // `second` never bound, but the Server object retains the
      // registered `error` listener — closing releases the handle so
      // `jest --detectOpenHandles` stays clean.
      if (second) await closeServer(second).catch(() => {});
      await closeServer(first);
    }
  });
});
