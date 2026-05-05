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

function fetchJson(server, path) {
  // `server.address().port` is only valid AFTER `listen` resolves.
  const { port } = server.address();
  return new Promise((resolve, reject) => {
    http.get(`http://127.0.0.1:${port}${path}`, (res) => {
      let body = '';
      res.on('data', (chunk) => { body += chunk; });
      res.on('end', () => resolve({ status: res.statusCode, body }));
    }).on('error', reject);
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
      const { status, body } = await fetchJson(server, '/health');
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
      const { status, body } = await fetchJson(server, '/health');
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
      const ok = await fetchJson(server, '/health');
      expect(ok.status).toBe(200);
      ready = false;
      const fail = await fetchJson(server, '/health');
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
      const { status, body } = await fetchJson(server, '/');
      expect(status).toBe(404);
      expect(JSON.parse(body)).toEqual({ status: 'not_found' });
    } finally {
      await closeServer(server);
    }
  });

  test('non-GET method on /health returns 404', async () => {
    // The wget probe uses GET. Any other method on /health is also a
    // misconfiguration; treat the same as a wrong path so the failure
    // mode is uniform.
    const server = startGatewayHealthServer(() => true);
    await waitForListening(server);
    try {
      const { port } = server.address();
      const post = await new Promise((resolve, reject) => {
        const req = http.request({
          hostname: '127.0.0.1', port, path: '/health', method: 'POST',
        }, (res) => {
          let body = '';
          res.on('data', (c) => { body += c; });
          res.on('end', () => resolve({ status: res.statusCode, body }));
        });
        req.on('error', reject);
        req.end();
      });
      expect(post.status).toBe(404);
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
});
