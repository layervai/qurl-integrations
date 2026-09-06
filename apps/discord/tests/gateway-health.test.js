const http = require('node:http');
const { startGatewayHealthServer } = require('../src/gateway-health');

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

jest.mock('../src/config', () => {
  const actual = jest.requireActual('../src/config');
  return { ...actual, PORT: 0 }; // 0 = OS-assigned ephemeral port
});

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
      expect(mockLogger.warn).toHaveBeenCalledWith('Gateway health: ok → unhealthy');
    } finally {
      await closeServer(server);
    }
  });

  test('unhealthy → ok transition logs info, recovery only fires once', async () => {
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
      const transitionInfoCalls = mockLogger.info.mock.calls
        .filter(([msg]) => msg === 'Gateway health: unhealthy → ok');
      expect(transitionInfoCalls).toHaveLength(1);
    } finally {
      await closeServer(server);
    }
  });

  test('GET on a non-/health path returns 404', async () => {
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

  test('strict /health match — trailing slash and sub-paths return 404', async () => {
    const server = startGatewayHealthServer(() => true, noopOnListenError);
    await waitForListening(server);
    try {
      const trailing = await request(server, '/health/');
      expect(trailing.status).toBe(404);
      const subpath = await request(server, '/health/ready');
      expect(subpath.status).toBe(404);
    } finally {
      await closeServer(server);
    }
  });

  test('HEAD /health returns 200 when isReady() is true', async () => {
    const server = startGatewayHealthServer(() => true, noopOnListenError);
    await waitForListening(server);
    try {
      const head = await request(server, '/health', 'HEAD');
      expect(head.status).toBe(200);
      expect(head.body).toBe('');
    } finally {
      await closeServer(server);
    }
  });

  test('POST /health returns 404', async () => {
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
    const server = startGatewayHealthServer(() => true, noopOnListenError);
    await waitForListening(server);
    expect(server.listening).toBe(true);
    await closeServer(server);
    expect(server.listening).toBe(false);
  });

  test('EADDRINUSE surfaces as structured log and calls onListenError', async () => {
    const mockLogger = require('../src/logger');
    const onListenError = jest.fn();
    const first = startGatewayHealthServer(() => true, noopOnListenError);
    await waitForListening(first);

    let second;
    try {
      const { port } = first.address();
      second = startGatewayHealthServer(() => true, onListenError, port);
      await new Promise((resolve) => { second.on('error', resolve); });

      expect(mockLogger.error).toHaveBeenCalledWith(
        'Gateway health listener failed',
        expect.objectContaining({ code: 'EADDRINUSE' }),
      );
      expect(onListenError).toHaveBeenCalledWith(
        expect.objectContaining({ code: 'EADDRINUSE' }),
      );
    } finally {
      if (second) await closeServer(second);
      await closeServer(first);
    }
  });
});
