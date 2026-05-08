/**
 * Tests for the Phase 1 gateway-side periodic metric emitters.
 *
 * Coverage focuses on the readiness composite (the only non-trivial
 * logic) and the audit-event shape the terraform metric filters rely
 * on. The setInterval timers are exercised via jest fake timers so
 * each test can advance time deterministically.
 */
const { WebSocketShardDestroyRecovery } = require('discord.js');
const logger = require('../src/logger');
const {
  startGatewayHeartbeat,
  startActiveGuildCount,
  readGatewayHealth,
  noteGatewayActivity,
  maybeAutoRecoverZombieWS,
  RECOVERY_ACTIVITY_THRESHOLD_MS,
  RECOVERY_COOLDOWN_MS,
  _test: gatewayMetricsTest,
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

// Most readGatewayHealth tests want a "recent activity" baseline so
// the activity-gate (Justin #193 §2) doesn't make every test report
// unhealthy. Reset + tick at the start of each test that exercises
// the composite. Tests for the pre-first-frame ("no activity yet")
// path explicitly skip this.
beforeEach(() => {
  if (gatewayMetricsTest && typeof gatewayMetricsTest._resetGatewayActivity === 'function') {
    gatewayMetricsTest._resetGatewayActivity();
  }
  noteGatewayActivity();
});

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

  test('unhealthy before the first gateway frame (lastGatewayActivityAt = 0 sentinel)', () => {
    // Justin #193 §2: dispatch wedge / event-loop saturation case.
    // At fresh-boot, before client.on('raw') has fired even once,
    // lastGatewayActivityAt = 0 so activity_age_ms is null and
    // composite must report unhealthy regardless of other signals.
    gatewayMetricsTest._resetGatewayActivity();
    const snap = readGatewayHealth(fakeClient({ ackedAgo: 5_000 }));
    expect(snap.healthy).toBe(false);
    expect(snap.activity_age_ms).toBe(null);
    expect(snap.is_ready).toBe(true); // ready, but never tick'd → still unhealthy
  });

  test('unhealthy when activity is stale (>60s — event-loop saturation)', () => {
    gatewayMetricsTest._resetGatewayActivity();
    // Simulate a tick 90 s ago by calling noteGatewayActivity() with a
    // back-dated `now` injection.
    noteGatewayActivity(() => Date.now() - 90_000);
    const snap = readGatewayHealth(fakeClient({ ackedAgo: 5_000 }));
    expect(snap.healthy).toBe(false);
    expect(snap.activity_age_ms).toBeGreaterThanOrEqual(60_000);
  });

  test('healthy when activity is recent and other signals OK', () => {
    gatewayMetricsTest._resetGatewayActivity();
    noteGatewayActivity();
    const snap = readGatewayHealth(fakeClient({ ackedAgo: 5_000 }));
    expect(snap.healthy).toBe(true);
    expect(snap.activity_age_ms).toBeLessThan(60_000);
  });

  test('NTP step-backward clamps activity_age_ms to 0', () => {
    gatewayMetricsTest._resetGatewayActivity();
    // Simulate a tick "in the future" (clock just stepped backward).
    noteGatewayActivity(() => Date.now() + 10_000);
    const snap = readGatewayHealth(fakeClient({ ackedAgo: 5_000 }));
    expect(snap.activity_age_ms).toBe(0);
    expect(snap.healthy).toBe(true);
  });

  test('noteGatewayActivity treats non-function arg as Date.now (production caller shape)', () => {
    // discord.js's `raw` event passes a packet object as the first
    // arg, not a clock function. The defensive `typeof === 'function'`
    // fallback must keep the timestamp updating in that case. Without
    // this test, a future refactor that drops the typeof check would
    // pass tests that only exercise the clock-fn injection form.
    gatewayMetricsTest._resetGatewayActivity();
    const fakePacket = { op: 11, t: null, s: null, d: null }; // discord.js raw shape
    const before = Date.now();
    noteGatewayActivity(fakePacket);
    const snap = readGatewayHealth(fakeClient({ ackedAgo: 5_000 }));
    expect(snap.activity_age_ms).toBeGreaterThanOrEqual(0);
    expect(snap.activity_age_ms).toBeLessThan(Date.now() - before + 100);
    expect(snap.healthy).toBe(true);
  });
});

describe('startGatewayHeartbeat', () => {
  beforeEach(() => {
    jest.useFakeTimers();
    jest.clearAllMocks();
    // Reset + tick activity AFTER useFakeTimers so the timestamp uses
    // the same fake-clock source as the readGatewayHealth comparisons.
    // Without this, the outer beforeEach() ticks with real Date.now(),
    // then advanceTimersByTime drifts the fake clock — Math.max(0,…)
    // papers over the negative diff so tests pass, but the activity
    // gate isn't actually being exercised in these tests.
    //
    // Reset the recovery cooldown too: a previous test in this block
    // may have stamped lastRecoveryAttemptAt, which would silently
    // make a downstream test that expects a triggered recovery
    // pass-as-cooldown'd instead.
    if (gatewayMetricsTest && typeof gatewayMetricsTest._resetGatewayActivity === 'function') {
      gatewayMetricsTest._resetGatewayActivity();
      gatewayMetricsTest._resetRecoveryClock();
    }
    noteGatewayActivity();
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

  test('does NOT emit gateway_heartbeat_healthy when unhealthy (silence is the existing alarm signal)', () => {
    const client = fakeClient({ isReady: false });
    startGatewayHeartbeat(client, { intervalMs: 1_000 });
    jest.advanceTimersByTime(1_000);
    expect(logger.audit).not.toHaveBeenCalledWith(
      AUDIT_EVENTS.GATEWAY_HEARTBEAT,
      expect.any(Object),
    );
  });

  test('emits gateway_heartbeat_unhealthy carrying activity_age_ms when unhealthy (Max-on-activity_age_ms alarm source)', () => {
    // Pin the contract that the unhealthy companion event always
    // carries activity_age_ms — terraform's metric filter extracts
    // that field directly. A future refactor that drops it from the
    // payload would silently break the alarm.
    const client = fakeClient({ isReady: false });
    startGatewayHeartbeat(client, { intervalMs: 1_000 });
    jest.advanceTimersByTime(1_000);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.GATEWAY_HEARTBEAT_UNHEALTHY,
      expect.objectContaining({
        activity_age_ms: expect.any(Number),
        is_ready: false,
      }),
    );
  });

  test('emits on every interval tick when healthy', () => {
    const client = fakeClient({ ackedAgo: 5_000 });
    startGatewayHeartbeat(client, { intervalMs: 1_000 });
    // 1 immediate runOnce + 3 interval ticks at t=1000/2000/3000 = 4
    jest.advanceTimersByTime(3_500);
    expect(logger.audit).toHaveBeenCalledTimes(4);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.GATEWAY_HEARTBEAT,
      expect.any(Object),
    );
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

  test('logs healthy → unhealthy transition exactly once (edge-triggered, not per-tick)', () => {
    // Start healthy, run two healthy ticks, then flip the client to
    // unhealthy and run two more ticks. Expect exactly ONE warn at
    // the edge — NOT one per unhealthy tick. Pin the broken contract
    // ("warn fires every interval while wedged") that would spam logs.
    let isReady = true;
    const client = {
      isReady: () => isReady,
      ws: { ping: 42, shards: new Map([[0, { lastPingTimestamp: Date.now() - 5_000 }]]) },
      guilds: { cache: { size: 1 } },
    };
    startGatewayHeartbeat(client, { intervalMs: 1_000 });
    // Initial runOnce + 1 tick = 2 healthy samples
    jest.advanceTimersByTime(1_000);
    expect(logger.warn).not.toHaveBeenCalled();

    isReady = false;
    jest.advanceTimersByTime(1_000); // first unhealthy → warn
    jest.advanceTimersByTime(1_000); // still unhealthy, no second warn
    expect(logger.warn).toHaveBeenCalledTimes(1);
    expect(logger.warn).toHaveBeenCalledWith(
      'Gateway heartbeat: healthy → unhealthy',
      expect.objectContaining({ is_ready: false }),
    );

    // Recovery → exactly one info, not one per healthy tick
    isReady = true;
    logger.info.mockClear();
    jest.advanceTimersByTime(1_000); // recovery → info
    jest.advanceTimersByTime(1_000); // still healthy, no second info
    expect(logger.info).toHaveBeenCalledTimes(1);
    expect(logger.info).toHaveBeenCalledWith(
      'Gateway heartbeat: unhealthy → healthy',
      expect.any(Object),
    );
  });

  test('tick() invokes auto-recovery when activity is stale past threshold (integration: pin the wiring)', () => {
    // Without this test, deleting the maybeAutoRecoverZombieWS call
    // from tick() — or flipping its guard from `!healthy` to
    // `healthy` — would pass every other test in this file. Pin the
    // wire so a regression has to break this assertion to land.
    if (gatewayMetricsTest && typeof gatewayMetricsTest._resetGatewayActivity === 'function') {
      gatewayMetricsTest._resetGatewayActivity();
      gatewayMetricsTest._resetRecoveryClock();
    }
    // Back-date activity by 200s so the snapshot will be unhealthy
    // AND past RECOVERY_ACTIVITY_THRESHOLD_MS (120s).
    noteGatewayActivity(() => Date.now() - 200_000);
    const shard = { destroy: jest.fn(() => Promise.resolve()) };
    const client = {
      isReady: () => true,
      ws: { ping: 42, shards: new Map([[0, shard]]) },
      guilds: { cache: { size: 1 } },
    };
    startGatewayHeartbeat(client, { intervalMs: 60_000 });
    // runOnce already fired — no advanceTimers needed.
    expect(shard.destroy).toHaveBeenCalledWith(
      expect.objectContaining({
        code: 4000,
        recover: WebSocketShardDestroyRecovery.Reconnect,
      }),
    );
    expect(logger.warn).toHaveBeenCalledWith(
      'Gateway auto-recovery: forcing reconnect (zombie WS)',
      expect.objectContaining({ shards_terminated: 1 }),
    );
  });

  test('runs once immediately so the first datapoint lands inside the boot alarm window', () => {
    const client = fakeClient({ ackedAgo: 5_000 });
    startGatewayHeartbeat(client, { intervalMs: 60_000 });
    // No timer advance — just invocation. The runOnce should already
    // have emitted a heartbeat without waiting for the first interval.
    expect(logger.audit).toHaveBeenCalledTimes(1);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.GATEWAY_HEARTBEAT,
      expect.any(Object),
    );
  });

  test('Math.max(0, …) guards against negative ack_age_ms from NTP step backward', () => {
    const future = Date.now() + 10_000; // simulate clock that jumped backward
    const shards = new Map([[0, { lastPingTimestamp: future }]]);
    const client = {
      isReady: () => true,
      ws: { ping: 42, shards },
      guilds: { cache: { size: 1 } },
    };
    const snap = readGatewayHealth(client);
    expect(snap.ack_age_ms).toBe(0);
    // healthy still requires ack_age_ms < 60_000 — clamp keeps the
    // signal correct: a backward clock step now produces "very recent
    // ack" which is the right semantic, not "stale by Number.MIN".
    expect(snap.healthy).toBe(true);
  });
});

describe('startActiveGuildCount', () => {
  beforeEach(() => {
    jest.useFakeTimers();
    jest.clearAllMocks();
    // See startGatewayHeartbeat beforeEach for why activity reset
    // happens AFTER useFakeTimers (clock-source consistency).
    if (gatewayMetricsTest && typeof gatewayMetricsTest._resetGatewayActivity === 'function') {
      gatewayMetricsTest._resetGatewayActivity();
    }
    noteGatewayActivity();
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

  test('runs once immediately so the first gauge sample lands without waiting for the interval', () => {
    // Symmetric with the startGatewayHeartbeat runOnce test. Pins the
    // round-3 addition: a future refactor that drops the immediate
    // tick() would otherwise pass CI silently because the existing
    // tests advance fake timers before asserting.
    const client = fakeClient({ guildCount: 5 });
    startActiveGuildCount(client, { intervalMs: 60_000 });
    // No timer advance — runOnce should already have emitted.
    expect(logger.audit).toHaveBeenCalledTimes(1);
    expect(logger.audit).toHaveBeenCalledWith(
      AUDIT_EVENTS.ACTIVE_GUILD_COUNT,
      { count: 5 },
    );
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

describe('maybeAutoRecoverZombieWS', () => {
  // Default destroy returns a resolved promise — discord.js's destroy
  // is async, and we test that the fire-and-forget caller handles
  // both success and rejection without breaking the tick loop.
  function shardClient({ destroyImpl } = {}) {
    const shard = { destroy: destroyImpl ?? jest.fn(() => Promise.resolve()) };
    return {
      client: { ws: { shards: new Map([[0, shard]]) } },
      shard,
    };
  }

  beforeEach(() => {
    if (gatewayMetricsTest && typeof gatewayMetricsTest._resetRecoveryClock === 'function') {
      gatewayMetricsTest._resetRecoveryClock();
    }
    jest.clearAllMocks();
  });

  test('skips when client is not ready (boot/reconnect already running)', () => {
    const { client, shard } = shardClient();
    const result = maybeAutoRecoverZombieWS(
      client,
      { is_ready: false, activity_age_ms: 999_999 },
    );
    expect(result.triggered).toBe(false);
    expect(result.reason).toBe('not_ready');
    expect(shard.destroy).not.toHaveBeenCalled();
  });

  test('skips when activity baseline is null (pre-first-frame)', () => {
    const { client, shard } = shardClient();
    const result = maybeAutoRecoverZombieWS(
      client,
      { is_ready: true, activity_age_ms: null },
    );
    expect(result.triggered).toBe(false);
    expect(result.reason).toBe('no_activity_baseline');
    expect(shard.destroy).not.toHaveBeenCalled();
  });

  test('skips when activity is below threshold', () => {
    const { client, shard } = shardClient();
    const result = maybeAutoRecoverZombieWS(
      client,
      { is_ready: true, activity_age_ms: RECOVERY_ACTIVITY_THRESHOLD_MS - 1 },
    );
    expect(result.triggered).toBe(false);
    expect(result.reason).toBe('within_threshold');
    expect(shard.destroy).not.toHaveBeenCalled();
  });

  test('triggers shard.destroy when activity is past threshold', () => {
    const { client, shard } = shardClient();
    const t = 1_000_000;
    const result = maybeAutoRecoverZombieWS(
      client,
      { is_ready: true, activity_age_ms: RECOVERY_ACTIVITY_THRESHOLD_MS + 1 },
      () => t,
    );
    expect(result.triggered).toBe(true);
    expect(result.reason).toBe('zombie_ws');
    expect(result.shardsTerminated).toBe(1);
    // `code` (NOT closeCode) — different field; closeCode would be
    // silently ignored. `recover` MUST be set or the shard goes Idle
    // and never reconnects (defeats the whole point of this code path).
    expect(shard.destroy).toHaveBeenCalledWith(
      expect.objectContaining({
        code: 4000,
        recover: WebSocketShardDestroyRecovery.Reconnect,
      }),
    );
  });

  test('debounces a second attempt inside the cooldown window', () => {
    const { client, shard } = shardClient();
    const t0 = 1_000_000;
    const first = maybeAutoRecoverZombieWS(
      client,
      { is_ready: true, activity_age_ms: RECOVERY_ACTIVITY_THRESHOLD_MS + 1 },
      () => t0,
    );
    expect(first.triggered).toBe(true);
    shard.destroy.mockClear();

    const second = maybeAutoRecoverZombieWS(
      client,
      { is_ready: true, activity_age_ms: RECOVERY_ACTIVITY_THRESHOLD_MS + 5_000 },
      () => t0 + RECOVERY_COOLDOWN_MS - 1,
    );
    expect(second.triggered).toBe(false);
    expect(second.reason).toBe('cooldown');
    expect(shard.destroy).not.toHaveBeenCalled();
  });

  test('allows a second attempt after the cooldown elapses', () => {
    const { client, shard } = shardClient();
    const t0 = 1_000_000;
    maybeAutoRecoverZombieWS(
      client,
      { is_ready: true, activity_age_ms: RECOVERY_ACTIVITY_THRESHOLD_MS + 1 },
      () => t0,
    );
    shard.destroy.mockClear();

    const second = maybeAutoRecoverZombieWS(
      client,
      { is_ready: true, activity_age_ms: RECOVERY_ACTIVITY_THRESHOLD_MS + 1 },
      () => t0 + RECOVERY_COOLDOWN_MS + 1,
    );
    expect(second.triggered).toBe(true);
    expect(shard.destroy).toHaveBeenCalledTimes(1);
  });

  test('catches sync throw from shard.destroy and keeps going', () => {
    const { client, shard } = shardClient({
      destroyImpl: jest.fn(() => { throw new Error('bad-args'); }),
    });
    const t = 1_000_000;
    const result = maybeAutoRecoverZombieWS(
      client,
      { is_ready: true, activity_age_ms: RECOVERY_ACTIVITY_THRESHOLD_MS + 1 },
      () => t,
    );
    // Sync throw => shardsTerminated stays 0 => returns no_shards
    // and DOES NOT consume the cooldown. The next tick retries.
    expect(result.triggered).toBe(false);
    expect(result.reason).toBe('no_shards');
    expect(logger.warn).toHaveBeenCalledWith(
      'Shard destroy threw synchronously during auto-recovery',
      expect.objectContaining({ error: 'bad-args' }),
    );
    expect(shard.destroy).toHaveBeenCalled();

    // Confirm cooldown was NOT stamped — second call same tick can fire.
    const second = maybeAutoRecoverZombieWS(
      client,
      { is_ready: true, activity_age_ms: RECOVERY_ACTIVITY_THRESHOLD_MS + 1 },
      () => t + 1_000,
    );
    expect(second.reason).toBe('no_shards'); // still failing the same way
  });

  test('handles destroy() promise rejection without crashing the process', async () => {
    const destroyImpl = jest.fn(() => Promise.reject(new Error('async-destroy-failed')));
    const { client, shard } = shardClient({ destroyImpl });
    const t = 1_000_000;
    const result = maybeAutoRecoverZombieWS(
      client,
      { is_ready: true, activity_age_ms: RECOVERY_ACTIVITY_THRESHOLD_MS + 1 },
      () => t,
    );
    // Sync result still triggered — we count the destroy as fired.
    expect(result.triggered).toBe(true);
    expect(result.shardsTerminated).toBe(1);
    // Wait one microtask tick for the rejection handler.
    await Promise.resolve();
    await Promise.resolve();
    expect(logger.warn).toHaveBeenCalledWith(
      'Shard destroy promise rejected during auto-recovery',
      expect.objectContaining({ error: 'async-destroy-failed' }),
    );
    expect(shard.destroy).toHaveBeenCalledWith(
      expect.objectContaining({
        code: 4000,
        recover: WebSocketShardDestroyRecovery.Reconnect,
      }),
    );
  });

  test('returns no_shards (does NOT consume cooldown) when client.ws.shards is missing', () => {
    const t = 1_000_000;
    const result = maybeAutoRecoverZombieWS(
      { ws: {} },
      { is_ready: true, activity_age_ms: RECOVERY_ACTIVITY_THRESHOLD_MS + 1 },
      () => t,
    );
    // No shards to terminate => not triggered, no cooldown stamp,
    // next tick retries. (If is_ready=true while shards is missing,
    // the bot is in a pathological state — locking out for 10min
    // serves no one.)
    expect(result.triggered).toBe(false);
    expect(result.reason).toBe('no_shards');

    // Same-tick second call confirms the cooldown was NOT stamped.
    const second = maybeAutoRecoverZombieWS(
      { ws: {} },
      { is_ready: true, activity_age_ms: RECOVERY_ACTIVITY_THRESHOLD_MS + 1 },
      () => t + 100,
    );
    expect(second.reason).toBe('no_shards');
  });

  test('returns no_shards when client.ws itself is undefined', () => {
    const t = 1_000_000;
    const result = maybeAutoRecoverZombieWS(
      {},
      { is_ready: true, activity_age_ms: RECOVERY_ACTIVITY_THRESHOLD_MS + 1 },
      () => t,
    );
    expect(result.triggered).toBe(false);
    expect(result.reason).toBe('no_shards');
  });

  test('triggers across multiple shards (happy path) — pin shardsTerminated count', () => {
    const shardA = { destroy: jest.fn(() => Promise.resolve()) };
    const shardB = { destroy: jest.fn(() => Promise.resolve()) };
    const client = { ws: { shards: new Map([[0, shardA], [1, shardB]]) } };
    const result = maybeAutoRecoverZombieWS(
      client,
      { is_ready: true, activity_age_ms: RECOVERY_ACTIVITY_THRESHOLD_MS + 1 },
      () => 1_000_000,
    );
    expect(result.triggered).toBe(true);
    expect(result.shardsTerminated).toBe(2);
    expect(shardA.destroy).toHaveBeenCalledTimes(1);
    expect(shardB.destroy).toHaveBeenCalledTimes(1);
  });

  test('partial-failure: one shard sync-throws, other succeeds — cooldown still stamped', () => {
    const shardA = { destroy: jest.fn(() => { throw new Error('borked'); }) };
    const shardB = { destroy: jest.fn(() => Promise.resolve()) };
    const client = { ws: { shards: new Map([[0, shardA], [1, shardB]]) } };
    const t = 1_000_000;
    const result = maybeAutoRecoverZombieWS(
      client,
      { is_ready: true, activity_age_ms: RECOVERY_ACTIVITY_THRESHOLD_MS + 1 },
      () => t,
    );
    expect(result.triggered).toBe(true);
    expect(result.shardsTerminated).toBe(1); // B succeeded; A threw
    // Cooldown WAS stamped (>=1 success), so an immediate retry debounces.
    const second = maybeAutoRecoverZombieWS(
      client,
      { is_ready: true, activity_age_ms: RECOVERY_ACTIVITY_THRESHOLD_MS + 1 },
      () => t + 1_000,
    );
    expect(second.reason).toBe('cooldown');
  });

  test('boundary: activity_age_ms === RECOVERY_ACTIVITY_THRESHOLD_MS triggers (>= semantic)', () => {
    // Code uses `< THRESHOLD` to gate, so equality falls THROUGH to
    // the recovery path. Pin the boundary so a future refactor to
    // `<=` (which would silently delay recovery by one tick) breaks
    // this test instead of getting silently merged.
    const { client, shard } = shardClient();
    const result = maybeAutoRecoverZombieWS(
      client,
      { is_ready: true, activity_age_ms: RECOVERY_ACTIVITY_THRESHOLD_MS },
      () => 1_000_000,
    );
    expect(result.triggered).toBe(true);
    expect(shard.destroy).toHaveBeenCalledTimes(1);
  });
});
