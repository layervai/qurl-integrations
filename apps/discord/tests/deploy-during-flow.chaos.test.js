
const { createGatewayLock } = require('../src/gateway-lock');
const { createPeerHeartbeat } = require('../src/gateway-peer-heartbeat');
const { createGatewayLeader } = require('../src/gateway-leader');
const { runPushHandoffShutdown } = require('../src/gateway-shutdown-helpers');
const {
  setupChaosDdb, makeChaosLogger,
  LOCK_TABLE, HEARTBEAT_TABLE, SHARD_ID,
  INSTANCE_A, INSTANCE_B, HOLDER_A, HOLDER_B,
  MAX_MICROTASK_YIELDS, assertNoUnexpectedTableCalls,
} = require('./helpers/chaos-ddb');

function makeFakeManager() {
  return {
    isConnected: jest.fn(() => true),
    isRecovering: jest.fn(() => false),
    connect: jest.fn(async () => {}),
  };
}

function makeFakeControlClient() {
  return {
    pushHandoff: jest.fn().mockResolvedValue({ ok: true }),
  };
}

function makeControllableEventPublisher() {
  let resolveStop;
  const stopped = new Promise((resolve) => { resolveStop = resolve; });
  return {
    stop: jest.fn().mockImplementation(() => stopped),
    releaseStop: () => resolveStop(),
  };
}

function makeScheduleHardExit() {
  const timers = [];
  const schedule = jest.fn((cb, ms) => {
    if (typeof cb !== 'function' || cb.length !== 0) {
      throw new Error(`chaos: scheduleHardExit callback must be zero-arity, got arity=${cb?.length}`);
    }
    const timer = { cb, ms, unref: jest.fn(), cleared: false };
    timers.push(timer);
    return timer;
  });
  const clearHardExit = jest.fn((timer) => {
    if (timer) timer.cleared = true;
  });
  return { schedule, clearHardExit, timers };
}

function assembleLeader({ docClient, clock, controlClient } = {}) {
  const logger = makeChaosLogger();
  const lock = createGatewayLock({
    ddbClient: docClient,
    tableName: LOCK_TABLE,
    shardId: SHARD_ID,
    instanceId: INSTANCE_A,
    lockHolder: HOLDER_A,
    logger,
    clock,
  });
  const peerHeartbeat = createPeerHeartbeat({
    ddbClient: docClient,
    tableName: HEARTBEAT_TABLE,
    instanceId: INSTANCE_A,
    ip: '10.0.0.10',
    port: 7800,
    shardId: SHARD_ID,
    lockHolder: HOLDER_A,
    logger,
    clock,
  });
  const resolvedControlClient = controlClient ?? makeFakeControlClient();
  const manager = makeFakeManager();
  const leader = createGatewayLeader({
    lock,
    peerHeartbeat,
    controlClient: resolvedControlClient,
    manager,
    selfInstanceId: INSTANCE_A,
    shardId: SHARD_ID,
    logger,
    tickIntervalMs: 1_000,
  });
  return {
    leader, lock, peerHeartbeat, controlClient: resolvedControlClient, manager, logger,
  };
}

describe('Pillar 3 chaos — deploy-during-flow (SIGTERM mid-handoff)', () => {
  let now = 1_700_000_000_000;
  const clock = () => now;

  beforeEach(() => { now = 1_700_000_000_000; });
  afterEach(() => { jest.restoreAllMocks(); });

  it('SIGTERM with healthy standby peer → transferLock + push ACK + clean exit(0)', async () => {
    const nowSeconds = Math.floor(now / 1000);
    const { docClient, ddbMock, state } = setupChaosDdb({
      initialLockRow: {
        shard_id: SHARD_ID,
        instance_id: INSTANCE_A,
        lock_holder: HOLDER_A,
        version: 3,
        expires_at: nowSeconds + 6,
      },
      initialPeerRows: [
        {
          instance_id: INSTANCE_A, ip: '10.0.0.10', port: 7800,
          shard_id: SHARD_ID, updated_at: nowSeconds, expires_at: nowSeconds + 60,
          lock_holder: HOLDER_A,
        },
        {
          instance_id: INSTANCE_B, ip: '10.0.0.20', port: 7800,
          shard_id: SHARD_ID, updated_at: nowSeconds, expires_at: nowSeconds + 60,
          lock_holder: HOLDER_B,
        },
      ],
    });

    const { leader, lock, logger } = assembleLeader({ docClient, clock });
    await leader.handleInboundHandoff({
      activeInstanceId: 'predecessor', expectedVersion: 3,
    });

    const timeline = [];
    const origTransferLock = lock.transferLock.bind(lock);
    jest.spyOn(lock, 'transferLock').mockImplementation(async (...args) => {
      const result = await origTransferLock(...args);
      timeline.push('transferLock-resolved');
      return result;
    });
    const baseEventPublisher = makeControllableEventPublisher();
    const eventPublisher = {
      stop: jest.fn(() => {
        timeline.push('stop-invoked');
        return baseEventPublisher.stop();
      }),
      releaseStop: baseEventPublisher.releaseStop,
    };
    const exit = jest.fn();
    const { schedule, clearHardExit, timers } = makeScheduleHardExit();

    const shutdownPromise = runPushHandoffShutdown({
      code: 0, gatewayLeader: leader, eventPublisher, logger,
      exit, scheduleHardExit: schedule, clearHardExit,
    });
    expect(eventPublisher.stop).toHaveBeenCalledTimes(1);
    eventPublisher.releaseStop();
    await shutdownPromise;

    expect(lock.transferLock).toHaveBeenCalledTimes(1);

    const stopIdx = timeline.indexOf('stop-invoked');
    const transferResolvedIdx = timeline.indexOf('transferLock-resolved');
    expect(stopIdx).toBeGreaterThanOrEqual(0);
    expect(transferResolvedIdx).toBeGreaterThanOrEqual(0);
    expect(stopIdx).toBeLessThan(transferResolvedIdx);

    expect(state.lockRow).not.toBeNull();
    expect(state.lockRow.instance_id).toBe(INSTANCE_B);
    expect(state.lockRow.lock_holder).toBe(HOLDER_B);
    expect(state.lockRow.version).toBe(4);

    const remainingPeerIds = state.peerRows.map((r) => r.instance_id);
    expect(remainingPeerIds).not.toContain(INSTANCE_A);
    expect(remainingPeerIds).toContain(INSTANCE_B);

    expect(exit).toHaveBeenCalledWith(0);
    expect(exit).toHaveBeenCalledTimes(1);
    expect(clearHardExit).toHaveBeenCalledWith(timers[0]);

    assertNoUnexpectedTableCalls(ddbMock);
  });

  it('SIGTERM with no peer (no_peer fallback) → releaseLock + clean exit(0)', async () => {
    const nowSeconds = Math.floor(now / 1000);
    const { docClient, ddbMock, state } = setupChaosDdb({
      initialLockRow: {
        shard_id: SHARD_ID,
        instance_id: INSTANCE_A,
        lock_holder: HOLDER_A,
        version: 5,
        expires_at: nowSeconds + 6,
      },
      initialPeerRows: [{
        instance_id: INSTANCE_A, ip: '10.0.0.10', port: 7800,
        shard_id: SHARD_ID, updated_at: nowSeconds, expires_at: nowSeconds + 60,
        lock_holder: HOLDER_A,
      }],
    });

    const { leader, controlClient, logger } = assembleLeader({ docClient, clock });
    await leader.handleInboundHandoff({
      activeInstanceId: 'predecessor', expectedVersion: 5,
    });

    const eventPublisher = makeControllableEventPublisher();
    eventPublisher.releaseStop(); // resolve immediately — no-peer path doesn't gate on it.
    const exit = jest.fn();
    const { schedule, clearHardExit } = makeScheduleHardExit();

    await runPushHandoffShutdown({
      code: 0, gatewayLeader: leader, eventPublisher, logger,
      exit, scheduleHardExit: schedule, clearHardExit,
    });

    expect(state.lockRow).toBeNull();
    expect(controlClient.pushHandoff).not.toHaveBeenCalled();
    expect(state.peerRows).toEqual([]);
    expect(exit).toHaveBeenCalledWith(0);
    assertNoUnexpectedTableCalls(ddbMock);
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('no fresh peer for handoff'),
    );
  });

  it('SIGTERM with hung controlClient.pushHandoff → hard-exit fires with forcedExitCode=1', async () => {
    const nowSeconds = Math.floor(now / 1000);
    const { docClient, ddbMock, state } = setupChaosDdb({
      initialLockRow: {
        shard_id: SHARD_ID, instance_id: INSTANCE_A, lock_holder: HOLDER_A,
        version: 7, expires_at: nowSeconds + 6,
      },
      initialPeerRows: [
        {
          instance_id: INSTANCE_A, ip: '10.0.0.10', port: 7800,
          shard_id: SHARD_ID, updated_at: nowSeconds, expires_at: nowSeconds + 60,
          lock_holder: HOLDER_A,
        },
        {
          instance_id: INSTANCE_B, ip: '10.0.0.20', port: 7800,
          shard_id: SHARD_ID, updated_at: nowSeconds, expires_at: nowSeconds + 60,
          lock_holder: HOLDER_B,
        },
      ],
    });

    const hungControlClient = {
      pushHandoff: jest.fn(() => new Promise(() => {})),
    };
    const { leader, logger } = assembleLeader({
      docClient, clock, controlClient: hungControlClient,
    });
    await leader.handleInboundHandoff({
      activeInstanceId: 'predecessor', expectedVersion: 7,
    });

    const eventPublisher = makeControllableEventPublisher();
    eventPublisher.releaseStop(); // drain resolves immediately; not what we're testing
    const exit = jest.fn();
    const { schedule, timers, clearHardExit } = makeScheduleHardExit();

    const shutdownPromise = runPushHandoffShutdown({
      code: 0, gatewayLeader: leader, eventPublisher, logger,
      exit, scheduleHardExit: schedule, clearHardExit,
    });
    shutdownPromise.catch(() => {});

    let yieldsForTransfer = 0;
    while (state.lockRow.instance_id !== INSTANCE_B) {
      if (yieldsForTransfer++ >= MAX_MICROTASK_YIELDS) {
        throw new Error(`chaos: transferLock never landed within ${MAX_MICROTASK_YIELDS} event-loop yields`);
      }
      // eslint-disable-next-line no-await-in-loop
      await new Promise((r) => { setImmediate(r); });
    }
    expect(timers).toHaveLength(1);
    expect(timers[0].ms).toBeGreaterThan(0);
    timers[0].cb();

    expect(exit).toHaveBeenCalledWith(1);

    expect(state.lockRow.instance_id).toBe(INSTANCE_B);
    expect(state.lockRow.version).toBe(8);
    assertNoUnexpectedTableCalls(ddbMock);
  });
});
