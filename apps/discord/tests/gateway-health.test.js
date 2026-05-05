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

function request(server, path, method = 'GET') {
  const { port } = server.address();
  return new Promise((resolve, reject) => {
    const req = http.request(
      { hostname: '127.0.0.1', port, path, method },
      (res) => {
        let body = '';
        res.on('data', (chunk) => { body += chunk; });
        res.on('end', () => resolve({ status: res.statusCode, body }));
      },
    );
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
  test('GET /health returns 200 ok when isReady() is true', async () => {
    const server = startGatewayHealthServer(() => true);
    await waitForListening(server);
    try {
      const { status, body } = await request(server, '/health');
      expect(status).toBe(200);
      expect(JSON.parse(body)).toEqual({ status: 'ok' });
    } finally {
      await closeServer(server);
    }
  });

  test('GET /health returns 503 unhealthy when isReady() is false', async () => {
    const server = startGatewayHealthServer(() => false);
    await waitForListening(server);
    try {
      const { status, body } = await request(server, '/health');
      expect(status).toBe(503);
      expect(JSON.parse(body)).toEqual({ status: 'unhealthy' });
    } finally {
      await closeServer(server);
    }
  });

  test('isReady() flip from true → false changes response from 200 → 503', async () => {
    // Realistic shape: bot starts ready, WebSocket disconnects,
    // probe should immediately reflect the new state. The real
    // discord.js client.isReady() reads from the connection state on
    // every call, so the closure pattern guarantees no stale cache.
    let ready = true;
    const server = startGatewayHealthServer(() => ready);
    await waitForListening(server);
    try {
      const ok = await request(server, '/health');
      expect(ok.status).toBe(200);
      ready = false;
      const fail = await request(server, '/health');
      expect(fail.status).toBe(503);
    } finally {
      await closeServer(server);
    }
  });

  test('GET on a non-/health path returns 404', async () => {
    // Probe-misconfiguration safety: if the wget probe ever drifts
    // off /health (typo, copy-paste from another service's
    // healthcheck), we want a clean 404 not a coincidental 200.
    const server = startGatewayHealthServer(() => true);
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
    const server = startGatewayHealthServer(() => true);
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
    const server = startGatewayHealthServer(() => true);
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
    const server = startGatewayHealthServer(() => true);
    await waitForListening(server);
    try {
      const { status, body } = await request(server, '/health?ts=123');
      expect(status).toBe(200);
      expect(JSON.parse(body)).toEqual({ status: 'ok' });
    } finally {
      await closeServer(server);
    }
  });

  test('isReady() throwing returns 503 instead of 500', async () => {
    // Future composite readiness closures might deref into something
    // nullable. The try/catch ensures a throw surfaces as 503
    // (unhealthy) rather than an unhandled 500.
    const server = startGatewayHealthServer(() => { throw new Error('boom'); });
    await waitForListening(server);
    try {
      const { status, body } = await request(server, '/health');
      expect(status).toBe(503);
      expect(JSON.parse(body)).toEqual({ status: 'unhealthy' });
    } finally {
      await closeServer(server);
    }
  });

  test('server.close() drains and exits cleanly', async () => {
    // Pinned because index.js's gracefulShutdown awaits httpServer.close()
    // — if the listener doesn't drain, SIGTERM hangs until ECS sends
    // SIGKILL. Confirms the listener honors the Node http.Server
    // close contract.
    const server = startGatewayHealthServer(() => true);
    await waitForListening(server);
    expect(server.listening).toBe(true);
    await closeServer(server);
    expect(server.listening).toBe(false);
  });

  test('EADDRINUSE surfaces as structured log and calls onFatalError', async () => {
    // If config.PORT is already in use (e.g. a stray combined-mode
    // process), the error handler should log a structured message
    // and invoke onFatalError so index.js can route through
    // gracefulShutdown.
    const mockLogger = require('../src/logger');
    const onFatalError = jest.fn();
    const first = startGatewayHealthServer(() => true);
    await waitForListening(first);

    try {
      // Pass the occupied port directly instead of mutating the
      // shared config mock — avoids action-at-a-distance.
      const { port } = first.address();
      const second = startGatewayHealthServer(() => true, onFatalError, port);
      // Wait for the async listen error to fire.
      await new Promise((resolve) => { second.on('error', resolve); });

      expect(mockLogger.error).toHaveBeenCalledWith(
        'Gateway health listener failed',
        expect.objectContaining({ code: 'EADDRINUSE' }),
      );
      expect(onFatalError).toHaveBeenCalledWith(
        expect.objectContaining({ code: 'EADDRINUSE' }),
      );
    } finally {
      await closeServer(first);
    }
  });
});
