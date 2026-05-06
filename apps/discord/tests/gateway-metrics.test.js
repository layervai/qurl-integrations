/**
 * Tests for the Phase 1 gateway-side periodic metric emitters.
 *
 * Coverage focuses on the readiness composite (the only non-trivial
 * logic) and the audit-event shape the terraform metric filters rely
 * on. The setInterval timers are exercised via jest fake timers so
 * each test can advance time deterministically.
 */
const logger = require('../src/logger');
const {
  startGatewayHeartbeat,
  startActiveGuildCount,
  readGatewayHealth,
} = require('../src/gateway-metrics');
const { AUDIT_EVENTS } = require('../src/constants');

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

function fakeClient({ isReady = true, ping = 42, ackedAgo = 5_000, guildCount = 3 } = {}) {
  // Mirror discord.js v14 shape: WebSocketShard.lastPingTimestamp is
  // a numeric ms epoch (-1 pre-first-ack). Tests use null to indicate
  // "no shards have an ack at all" (whole shards Map empty / each
  // shard pre-first-ack).
  const lastPingTimestamp = ackedAgo === null ? -1 : Date.now() - ackedAgo;
  const shards = new Map([[0, { lastPingTimestamp }]]);
  return {
    isReady: () => isReady,
    ws: { ping, shards },
    guilds: { cache: { size: guildCount } },
  };
}

describe('readGatewayHealth', () => {
  test('healthy when ready, ping > 0, ack age < 60s', () => {
    const snap = readGatewayHealth(fakeClient({ ackedAgo: 5_000 }));
    expect(snap.healthy).toBe(true);
    expect(snap.ping_ms).toBe(42);
    expect(snap.ack_age_ms).toBeLessThan(60_000);
    expect(snap.is_ready).toBe(true);
  });

  test('unhealthy when not ready', () => {
    const snap = readGatewayHealth(fakeClient({ isReady: false }));
    expect(snap.healthy).toBe(false);
    expect(snap.is_ready).toBe(false);
  });

  test('unhealthy when ws.ping is -1 (no ack yet)', () => {
    const snap = readGatewayHealth(fakeClient({ ping: -1 }));
    expect(snap.healthy).toBe(false);
    expect(snap.ping_ms).toBe(-1);
  });

  test('unhealthy when last ack > 60s old (zombie)', () => {
    const snap = readGatewayHealth(fakeClient({ ackedAgo: 90_000 }));
    expect(snap.healthy).toBe(false);
    expect(snap.ack_age_ms).toBeGreaterThanOrEqual(60_000);
  });

  test('unhealthy when no shards have completed a heartbeat round-trip yet (-1 sentinel)', () => {
    // discord.js v14 initializes lastPingTimestamp to -1 until the
    // first HEARTBEAT_ACK lands. The composite check must reject this
    // pre-first-ack state so the alarm doesn't go OK during a boot
    // window where the gateway hasn't actually ack'd yet.
    const client = {
      isReady: () => true,
      ws: { ping: 42, shards: new Map([[0, { lastPingTimestamp: -1 }]]) },
      guilds: { cache: { size: 1 } },
    };
    const snap = readGatewayHealth(client);
    expect(snap.healthy).toBe(false);
    expect(snap.ack_age_ms).toBe(null);
  });

  test('unhealthy when shard reports lastPingTimestamp = 0 (legacy/null shape)', () => {
    // Future-proof: if a future discord.js version flips the
    // pre-first-ack sentinel to 0, the > 0 guard still catches it.
    const client = {
      isReady: () => true,
      ws: { ping: 42, shards: new Map([[0, { lastPingTimestamp: 0 }]]) },
      guilds: { cache: { size: 1 } },
    };
    const snap = readGatewayHealth(client);
    expect(snap.healthy).toBe(false);
    expect(snap.ack_age_ms).toBe(null);
  });

  test('uses oldest ack across multiple shards (worst case)', () => {
    const now = Date.now();
    const shards = new Map([
      [0, { lastPingTimestamp: now - 5_000 }],
      [1, { lastPingTimestamp: now - 80_000 }], // stale shard wins
    ]);
    const client = {
      isReady: () => true,
      ws: { ping: 42, shards },
      guilds: { cache: { size: 1 } },
    };
    const snap = readGatewayHealth(client, () => now);
    expect(snap.healthy).toBe(false);
    expect(snap.ack_age_ms).toBeGreaterThanOrEqual(80_000);
  });

  test('safe when ws or shards are missing (early-boot client)', () => {
    const snap = readGatewayHealth({ isReady: () => false });
    expect(snap.healthy).toBe(false);
    expect(snap.ping_ms).toBe(-1);
    expect(snap.ack_age_ms).toBe(null);
  });
});

describe('startGatewayHeartbeat', () => {
  beforeEach(() => {
    jest.useFakeTimers();
    jest.clearAllMocks();
  });
  afterEach(() => jest.useRealTimers());

  test('emits gateway_heartbeat_healthy when healthy', () => {
    const client = fakeClient({ ackedAgo: 5_000 });
    startGatewayHeartbeat(client, { intervalMs: 1_000 });
    jest.advanceTimersByTime(1_000);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.GATEWAY_HEARTBEAT,
      expect.objectContaining({
        ping_ms: 42,
        ack_age_ms: expect.any(Number),
      }),
    );
  });

  test('does NOT emit when unhealthy (silence is the alarm signal)', () => {
    const client = fakeClient({ isReady: false });
    startGatewayHeartbeat(client, { intervalMs: 1_000 });
    jest.advanceTimersByTime(1_000);
    expect(logger.audit).not.toHaveBeenCalled();
  });

  test('emits on every interval tick when healthy', () => {
    const client = fakeClient({ ackedAgo: 5_000 });
    startGatewayHeartbeat(client, { intervalMs: 1_000 });
    jest.advanceTimersByTime(3_500);
    expect(logger.audit).toHaveBeenCalledTimes(3);
  });

  test('swallows sampler errors so a future API change does not wedge the bot', () => {
    const client = {
      isReady: () => { throw new Error('boom'); },
      ws: { ping: 42, shards: new Map() },
    };
    startGatewayHeartbeat(client, { intervalMs: 1_000 });
    jest.advanceTimersByTime(1_000);
    expect(logger.audit).not.toHaveBeenCalled();
    expect(logger.warn).toHaveBeenCalledWith(
      'Gateway heartbeat sampler threw',
      expect.objectContaining({ error: 'boom' }),
    );
  });

  test('returns timer handle for shutdown cleanup', () => {
    const client = fakeClient();
    const timer = startGatewayHeartbeat(client, { intervalMs: 1_000 });
    expect(timer).toBeDefined();
    expect(typeof timer.unref).toBe('function');
    clearInterval(timer);
  });
});

describe('startActiveGuildCount', () => {
  beforeEach(() => {
    jest.useFakeTimers();
    jest.clearAllMocks();
  });
  afterEach(() => jest.useRealTimers());

  test('emits active_guild_count carrying cache size', () => {
    const client = fakeClient({ guildCount: 7 });
    startActiveGuildCount(client, { intervalMs: 1_000 });
    jest.advanceTimersByTime(1_000);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.ACTIVE_GUILD_COUNT,
      { count: 7 },
    );
  });

  test('skips emission when guilds cache is missing', () => {
    const client = { ws: {} };
    startActiveGuildCount(client, { intervalMs: 1_000 });
    jest.advanceTimersByTime(1_000);
    expect(logger.audit).not.toHaveBeenCalled();
  });

  test('swallows errors and logs at warn level', () => {
    const client = {
      get guilds() { throw new Error('cache exploded'); },
    };
    startActiveGuildCount(client, { intervalMs: 1_000 });
    jest.advanceTimersByTime(1_000);
    expect(logger.audit).not.toHaveBeenCalled();
    expect(logger.warn).toHaveBeenCalled();
  });
});
