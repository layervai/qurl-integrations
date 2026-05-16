// Unit tests for src/gateway-leader.js — the Pillar 3 orchestrator
// that ties gateway-lock + peer-heartbeat + control-client + manager
// into one state machine. Pins the load-bearing contracts:
//
//   1. Tick writes heartbeat unconditionally + refreshes peer cache.
//      Heartbeat failure doesn't block lock op; peer-cache fetch
//      failure keeps the prior cache (don't blank it on transients).
//   2. Tick lock op:
//        - heldLock=false → tries acquireLock; success flips flag.
//        - heldLock=true  → tries renewLock; CCF flips flag off.
//        - throw on either → keeps flag as-is (retry next tick).
//   3. handleInboundHandoff ordering: adopt → flag → connect. Adopt
//      throw never flips the flag; connect throw flips it (watchdog
//      retries).
//   4. pushHandoff:
//        - not holding → no-op.
//        - no fresh peer → releaseLock + reason:'no_peer'.
//        - transferLock CAS fail → reason:'transfer_failed', no release
//          (peer cold-acquired; the CCF IS the release).
//        - transferLock success + push success → pushed:true.
//        - transferLock success + push timeout → pushed:true, ackReason:
//          'timeout' (active still exits; standby's watchdog recovers).
//   5. Serialization: tick + inbound-handoff + push-handoff funnel
//      through the same in-flight chain. No two lock mutators ever
//      run concurrently.
//   6. Hooks: isHoldingLock + isKnownPeer + releaseLockForExit
//      reflect internal state; releaseLockForExit clears flag +
//      calls lock.releaseLock.

const {
  createGatewayLeader,
  DEFAULT_TICK_INTERVAL_MS,
} = require('../src/gateway-leader');

function makeMocks({
  initialPeers = [],
  ...overrides
} = {}) {
  const lock = {
    acquireLock: jest.fn(async () => ({ acquired: true, version: 1 })),
    renewLock: jest.fn(async () => ({ renewed: true, version: 2 })),
    transferLock: jest.fn(async () => ({ transferred: true, version: 3 })),
    adoptLockFromHandoff: jest.fn(),
    releaseLock: jest.fn(async () => ({ released: true })),
    ...overrides.lock,
  };
  const peerHeartbeat = {
    writeHeartbeat: jest.fn(async () => {}),
    listFreshPeers: jest.fn(async () => initialPeers),
    ...overrides.peerHeartbeat,
  };
  const controlClient = {
    pushHandoff: jest.fn(async () => ({ ok: true, status: 200 })),
    ...overrides.controlClient,
  };
  const manager = {
    connect: jest.fn(async () => {}),
    isConnected: jest.fn(() => false),
    ...overrides.manager,
  };
  const logger = {
    info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(),
  };
  return { lock, peerHeartbeat, controlClient, manager, logger };
}

function makeLeader({ mocks, sleep, tickIntervalMs } = {}) {
  const m = mocks ?? makeMocks();
  const leader = createGatewayLeader({
    lock: m.lock,
    peerHeartbeat: m.peerHeartbeat,
    controlClient: m.controlClient,
    manager: m.manager,
    selfInstanceId: 'inst-A',
    selfLockHolder: 'task-arn:.../inst-A',
    shardId: '0:1',
    logger: m.logger,
    tickIntervalMs, sleep,
  });
  return { leader, ...m };
}

describe('createGatewayLeader — factory validation', () => {
  it('exposes default tick interval', () => {
    expect(DEFAULT_TICK_INTERVAL_MS).toBe(2_000);
  });

  it('throws on missing required deps', () => {
    expect(() => createGatewayLeader()).toThrow(/lock/);
    expect(() => createGatewayLeader({ lock: {} })).toThrow(/peerHeartbeat/);
    expect(() => createGatewayLeader({ lock: {}, peerHeartbeat: {} }))
      .toThrow(/controlClient/);
    expect(() => createGatewayLeader({
      lock: {}, peerHeartbeat: {}, controlClient: {},
    })).toThrow(/manager/);
    expect(() => createGatewayLeader({
      lock: {}, peerHeartbeat: {}, controlClient: {}, manager: {},
    })).toThrow(/manager/);
    expect(() => createGatewayLeader({
      lock: {}, peerHeartbeat: {}, controlClient: {}, manager: { connect() {} },
    })).toThrow(/selfInstanceId/);
    expect(() => createGatewayLeader({
      lock: {}, peerHeartbeat: {}, controlClient: {}, manager: { connect() {} },
      selfInstanceId: 'a',
    })).toThrow(/selfLockHolder/);
    expect(() => createGatewayLeader({
      lock: {}, peerHeartbeat: {}, controlClient: {}, manager: { connect() {} },
      selfInstanceId: 'a', selfLockHolder: 'h',
    })).toThrow(/shardId/);
    expect(() => createGatewayLeader({
      lock: {}, peerHeartbeat: {}, controlClient: {}, manager: { connect() {} },
      selfInstanceId: 'a', selfLockHolder: 'h', shardId: 's',
    })).toThrow(/logger/);
  });
});

describe('step (tick) — heartbeat + peer cache', () => {
  it('writes heartbeat unconditionally', async () => {
    const { leader, peerHeartbeat } = makeLeader();
    await leader._stepForTest();
    expect(peerHeartbeat.writeHeartbeat).toHaveBeenCalledTimes(1);
  });

  it('refreshes peer cache from listFreshPeers each tick', async () => {
    const mocks = makeMocks({
      initialPeers: [
        { instance_id: 'inst-B', ip: '10.0.0.2', port: 9876, updated_at: 100 },
      ],
    });
    const { leader, peerHeartbeat } = makeLeader({ mocks });
    expect(leader.isKnownPeer('inst-B')).toBe(false);
    await leader._stepForTest();
    expect(leader.isKnownPeer('inst-B')).toBe(true);
    expect(leader.isKnownPeer('inst-X')).toBe(false);
    expect(peerHeartbeat.listFreshPeers).toHaveBeenCalledTimes(1);
  });

  it('heartbeat write failure does not block the lock op', async () => {
    const mocks = makeMocks();
    mocks.peerHeartbeat.writeHeartbeat = jest.fn(async () => { throw new Error('throttled'); });
    const { leader, lock, logger } = makeLeader({ mocks });

    await leader._stepForTest();

    // Lock acquire still attempted (heldLock=false initial state).
    expect(lock.acquireLock).toHaveBeenCalledTimes(1);
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringMatching(/heartbeat write failed/),
      expect.objectContaining({ error: 'throttled' }),
    );
  });

  it('listFreshPeers failure preserves prior peer cache (no blanking on transients)', async () => {
    const mocks = makeMocks({
      initialPeers: [{ instance_id: 'inst-B', updated_at: 100 }],
    });
    const { leader, peerHeartbeat } = makeLeader({ mocks });
    await leader._stepForTest(); // populate cache
    expect(leader.isKnownPeer('inst-B')).toBe(true);

    // Next tick: listFreshPeers throws — cache must not be blanked.
    peerHeartbeat.listFreshPeers.mockRejectedValueOnce(new Error('ddb-throttled'));
    await leader._stepForTest();
    expect(leader.isKnownPeer('inst-B')).toBe(true);
  });
});

describe('step (tick) — lock state machine', () => {
  it('not-holding → tries acquireLock; success flips heldLock to true', async () => {
    const { leader, lock } = makeLeader();
    expect(leader.isHoldingLock()).toBe(false);
    await leader._stepForTest();
    expect(lock.acquireLock).toHaveBeenCalledTimes(1);
    expect(leader.isHoldingLock()).toBe(true);
  });

  it('not-holding → acquired=false keeps flag false', async () => {
    const mocks = makeMocks();
    mocks.lock.acquireLock = jest.fn(async () => ({ acquired: false }));
    const { leader } = makeLeader({ mocks });
    await leader._stepForTest();
    expect(leader.isHoldingLock()).toBe(false);
  });

  it('holding → tries renewLock; success keeps flag true', async () => {
    const { leader, lock } = makeLeader();
    await leader._stepForTest(); // acquire
    expect(leader.isHoldingLock()).toBe(true);

    await leader._stepForTest(); // renew
    expect(lock.renewLock).toHaveBeenCalledTimes(1);
    expect(leader.isHoldingLock()).toBe(true);
  });

  it('holding → renewLock CCF flips flag to false', async () => {
    const mocks = makeMocks();
    mocks.lock.renewLock = jest.fn(async () => ({ renewed: false }));
    const { leader, logger } = makeLeader({ mocks });
    // Pre-arm heldLock by stepping once with successful acquire (default).
    await leader._stepForTest();
    expect(leader.isHoldingLock()).toBe(true);

    await leader._stepForTest();
    expect(leader.isHoldingLock()).toBe(false);
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringMatching(/lost lock/),
    );
  });

  it('holding → renewLock throw keeps flag true (retry next tick)', async () => {
    const mocks = makeMocks();
    mocks.lock.renewLock = jest.fn(async () => { throw new Error('throttled'); });
    const { leader, logger } = makeLeader({ mocks });
    await leader._stepForTest(); // acquire
    expect(leader.isHoldingLock()).toBe(true);

    await leader._stepForTest(); // renew throws
    expect(leader.isHoldingLock()).toBe(true);
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/renewLock threw/),
      expect.objectContaining({ error: 'throttled' }),
    );
  });
});

describe('handleInboundHandoff', () => {
  it('order: adopt → set heldLock → connect', async () => {
    const callOrder = [];
    const mocks = makeMocks();
    mocks.lock.adoptLockFromHandoff = jest.fn(() => callOrder.push('adopt'));
    mocks.manager.connect = jest.fn(async () => callOrder.push('connect'));
    const { leader } = makeLeader({ mocks });

    // Spy the flag flip by checking inside connect()'s call.
    let flagAtConnect = null;
    mocks.manager.connect = jest.fn(async () => {
      flagAtConnect = leader.isHoldingLock();
      callOrder.push('connect');
    });

    await leader.handleInboundHandoff({
      activeInstanceId: 'inst-A', expectedVersion: 7,
    });

    expect(callOrder).toEqual(['adopt', 'connect']);
    expect(flagAtConnect).toBe(true); // flag set BEFORE connect
    expect(mocks.lock.adoptLockFromHandoff).toHaveBeenCalledWith(7);
    expect(leader.isHoldingLock()).toBe(true);
  });

  it('adopt throw → flag NOT set; rethrows so server returns 500', async () => {
    const mocks = makeMocks();
    mocks.lock.adoptLockFromHandoff = jest.fn(() => {
      throw new Error('bad-version');
    });
    const { leader, manager } = makeLeader({ mocks });

    await expect(leader.handleInboundHandoff({
      activeInstanceId: 'inst-A', expectedVersion: 0,
    })).rejects.toThrow(/bad-version/);

    expect(leader.isHoldingLock()).toBe(false);
    expect(manager.connect).not.toHaveBeenCalled();
  });

  it('connect throw → flag IS set (watchdog will retry); rethrows', async () => {
    const mocks = makeMocks();
    mocks.manager.connect = jest.fn(async () => { throw new Error('discord-down'); });
    const { leader, logger } = makeLeader({ mocks });

    await expect(leader.handleInboundHandoff({
      activeInstanceId: 'inst-A', expectedVersion: 7,
    })).rejects.toThrow(/discord-down/);

    expect(leader.isHoldingLock()).toBe(true);
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/inbound-handoff connect threw/),
      expect.objectContaining({ error: 'discord-down' }),
    );
  });
});

describe('pushHandoff', () => {
  function preHoldLock(leader) {
    // Drive a tick to flip heldLock=true via successful acquireLock.
    return leader._stepForTest();
  }

  it('returns not_holding_lock when called without the lock', async () => {
    const { leader } = makeLeader();
    const result = await leader.pushHandoff();
    expect(result).toEqual({ pushed: false, reason: 'not_holding_lock' });
  });

  it('returns no_peer + best-effort release when no fresh peer exists', async () => {
    const mocks = makeMocks({ initialPeers: [] });
    const { leader, lock } = makeLeader({ mocks });
    await preHoldLock(leader);

    const result = await leader.pushHandoff();
    expect(result).toEqual({ pushed: false, reason: 'no_peer' });
    expect(lock.releaseLock).toHaveBeenCalledTimes(1);
  });

  it('returns transfer_failed when transferLock CAS fails (does NOT release — CCF IS the release)', async () => {
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-B/inst-B', updated_at: 100,
      }],
    });
    mocks.lock.transferLock = jest.fn(async () => ({ transferred: false }));
    const { leader, lock, controlClient } = makeLeader({ mocks });
    await preHoldLock(leader);

    const result = await leader.pushHandoff();
    expect(result).toEqual({ pushed: false, reason: 'transfer_failed' });
    expect(controlClient.pushHandoff).not.toHaveBeenCalled();
    expect(lock.releaseLock).not.toHaveBeenCalled();
  });

  it('returns pushed:true, ackReason:ack on successful transfer + ACKed push', async () => {
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-B/inst-B', updated_at: 100,
      }],
    });
    const { leader, lock, controlClient } = makeLeader({ mocks });
    await preHoldLock(leader);

    const result = await leader.pushHandoff();
    expect(result).toEqual({ pushed: true, ackReason: 'ack' });
    expect(lock.transferLock).toHaveBeenCalledWith('inst-B', 'task-B/inst-B');
    expect(controlClient.pushHandoff).toHaveBeenCalledWith({
      peerIp: '10.0.0.2', peerPort: 9876, peerInstanceId: 'inst-B',
      selfInstanceId: 'inst-A', expectedVersion: 3,
    });
    expect(leader.isHoldingLock()).toBe(false);
  });

  it('returns pushed:true, ackReason:timeout when transferred but peer did not ACK', async () => {
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-B/inst-B', updated_at: 100,
      }],
    });
    mocks.controlClient.pushHandoff = jest.fn(async () => ({ ok: false, reason: 'timeout' }));
    const { leader } = makeLeader({ mocks });
    await preHoldLock(leader);

    const result = await leader.pushHandoff();
    expect(result).toEqual({ pushed: true, ackReason: 'timeout' });
  });

  it('falls back to placeholder lock_holder when peer row lacks the field', async () => {
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        // no lock_holder
        updated_at: 100,
      }],
    });
    const { leader, lock } = makeLeader({ mocks });
    await preHoldLock(leader);

    await leader.pushHandoff();
    expect(lock.transferLock).toHaveBeenCalledWith('inst-B', 'peer/inst-B');
  });

  it('stops the tick loop before the transferLock', async () => {
    // Without stop, a tick could race the transferLock with a renewLock.
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-B/inst-B', updated_at: 100,
      }],
    });
    const sleep = jest.fn(() => new Promise(() => {})); // never resolves
    const { leader } = makeLeader({ mocks, sleep, tickIntervalMs: 1 });
    leader.start();
    await preHoldLock(leader);

    await leader.pushHandoff();
    // The loop's running flag must be false after pushHandoff. We
    // can't observe `running` directly; we observe that no further
    // step is fired by checking sleep was called only once
    // (the initial start's first sleep).
    expect(sleep).toHaveBeenCalledTimes(1);
  });
});

describe('serialization — SIGTERM-during-tick (pushHandoff while a tick is in-flight)', () => {
  it('pushHandoff queues behind an in-flight tick and runs after the tick settles', async () => {
    // Real production race: SIGTERM fires mid-tick. The tick is
    // awaiting renewLock (DDB RTT ~10-30ms); pushHandoff must
    // queue behind it on `inFlight`, not race the same lock
    // mutator. The serialization chain pins this — but until now
    // only the inbound-handoff-vs-tick and parallel-handoff cases
    // had explicit tests.
    const callOrder = [];
    let resolveRenew;
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-B/inst-B', updated_at: 100,
      }],
    });
    mocks.lock.renewLock = jest.fn(() => new Promise((resolve) => {
      callOrder.push('renew-start');
      resolveRenew = resolve;
    }));
    mocks.lock.transferLock = jest.fn(async () => {
      callOrder.push('transfer');
      return { transferred: true, version: 3 };
    });
    const { leader } = makeLeader({ mocks });
    // Pre-hold the lock so the tick path enters renewLock branch.
    await leader._stepForTest(); // heldLock=true via acquire

    // Now start a SECOND tick (will go into renew); it'll block.
    const tick2 = leader._stepForTest();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(callOrder).toEqual(['renew-start']);

    // SIGTERM fires: pushHandoff. Queues behind tick2.
    const push = leader.pushHandoff();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(callOrder).toEqual(['renew-start']); // still blocked

    // Resolve the renew; tick2 settles, then push runs.
    resolveRenew({ renewed: true, version: 2 });
    await tick2;
    await push;
    expect(callOrder).toEqual(['renew-start', 'transfer']);
  });
});

describe('serialization — no two mutators interleave', () => {
  it('a tick during an in-flight pushHandoff waits for the push to settle', async () => {
    let resolvePush;
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-B/inst-B', updated_at: 100,
      }],
    });
    mocks.controlClient.pushHandoff = jest.fn(() => new Promise((resolve) => {
      resolvePush = resolve;
    }));
    const { leader, lock } = makeLeader({ mocks });
    await leader._stepForTest(); // heldLock=true

    // Reset transfer mock to count fresh calls.
    lock.transferLock.mockClear();
    const firstAcquireCalls = lock.acquireLock.mock.calls.length;

    // Kick off pushHandoff but don't await yet.
    const pushPromise = leader.pushHandoff();
    // Immediately schedule a tick — should queue behind the push.
    const tickPromise = leader._stepForTest();

    // Give microtasks a chance to run.
    await new Promise((resolve) => { setImmediate(resolve); });

    // Push is still in flight (controlClient.pushHandoff hasn't
    // resolved). Tick should NOT have run a lock op yet.
    expect(lock.transferLock).toHaveBeenCalledTimes(1); // from push
    // acquireLock not called again past the pre-step.
    expect(lock.acquireLock.mock.calls.length).toBe(firstAcquireCalls);

    // Resolve the push; tick should then run.
    resolvePush({ ok: true, status: 200 });
    await pushPromise;
    await tickPromise;

    // After push completes, heldLock=false, so the tick's branch
    // calls acquireLock again.
    expect(lock.acquireLock.mock.calls.length).toBe(firstAcquireCalls + 1);
  });

  it('two concurrent inbound handoffs serialize through adopt + connect', async () => {
    const callOrder = [];
    const mocks = makeMocks();
    mocks.lock.adoptLockFromHandoff = jest.fn((v) => {
      callOrder.push(`adopt-${v}`);
    });
    let connectResolves = [];
    mocks.manager.connect = jest.fn(() => new Promise((resolve) => {
      connectResolves.push(resolve);
      callOrder.push(`connect-start-${connectResolves.length}`);
    }));
    const { leader } = makeLeader({ mocks });

    const p1 = leader.handleInboundHandoff({
      activeInstanceId: 'inst-X', expectedVersion: 5,
    });
    const p2 = leader.handleInboundHandoff({
      activeInstanceId: 'inst-Y', expectedVersion: 9,
    });

    await new Promise((resolve) => { setImmediate(resolve); });
    // Only the FIRST handoff's adopt + connect should have started.
    expect(callOrder).toEqual(['adopt-5', 'connect-start-1']);

    connectResolves[0]();
    await p1;
    await new Promise((resolve) => { setImmediate(resolve); });
    // Now the second handoff runs.
    expect(callOrder).toEqual([
      'adopt-5', 'connect-start-1',
      'adopt-9', 'connect-start-2',
    ]);
    connectResolves[1]();
    await p2;
  });
});

describe('isConnecting — race protection between inbound-handoff and watchdog', () => {
  it('is true ONLY while handleInboundHandoff awaits manager.connect()', async () => {
    let resolveConnect;
    const mocks = makeMocks();
    mocks.manager.connect = jest.fn(() => new Promise((resolve) => {
      resolveConnect = resolve;
    }));
    const { leader } = makeLeader({ mocks });
    expect(leader.isConnecting()).toBe(false);

    const handoffPromise = leader.handleInboundHandoff({
      activeInstanceId: 'inst-X', expectedVersion: 5,
    });
    // Give the serialized fn a microtask to start.
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(leader.isConnecting()).toBe(true);

    resolveConnect();
    await handoffPromise;
    expect(leader.isConnecting()).toBe(false);
  });

  it('isConnecting clears even when manager.connect() throws', async () => {
    const mocks = makeMocks();
    mocks.manager.connect = jest.fn(async () => { throw new Error('discord-down'); });
    const { leader } = makeLeader({ mocks });
    await expect(leader.handleInboundHandoff({
      activeInstanceId: 'inst-X', expectedVersion: 5,
    })).rejects.toThrow(/discord-down/);
    // finally{} block must clear the flag so the watchdog can take
    // over the retry on its next tick.
    expect(leader.isConnecting()).toBe(false);
  });
});

describe('pushHandoff — re-entry safety', () => {
  it('a second pushHandoff call after the first transferred returns not_holding_lock', async () => {
    // SIGTERM fires twice (rare but possible: ECS retry + signal
    // bounce). Second call must observe heldLock=false (cleared by
    // the first push's transfer) and no-op cleanly rather than
    // attempt to re-transfer.
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-B/inst-B', updated_at: 100,
      }],
    });
    const { leader, lock } = makeLeader({ mocks });
    await leader._stepForTest(); // heldLock=true via acquire

    const first = await leader.pushHandoff();
    expect(first).toEqual({ pushed: true, ackReason: 'ack' });

    const second = await leader.pushHandoff();
    expect(second).toEqual({ pushed: false, reason: 'not_holding_lock' });

    // Critical: transferLock was called once, not twice.
    expect(lock.transferLock).toHaveBeenCalledTimes(1);
  });

  it('two parallel pushHandoff calls serialize — only one transfers', async () => {
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-B/inst-B', updated_at: 100,
      }],
    });
    const { leader, lock } = makeLeader({ mocks });
    await leader._stepForTest(); // heldLock=true

    const [a, b] = await Promise.all([
      leader.pushHandoff(),
      leader.pushHandoff(),
    ]);
    expect(lock.transferLock).toHaveBeenCalledTimes(1);
    // First call transfers, second sees not_holding_lock.
    const transferred = [a, b].filter((r) => r.pushed === true);
    const noOps = [a, b].filter((r) => r.pushed === false);
    expect(transferred).toHaveLength(1);
    expect(noOps).toHaveLength(1);
    expect(noOps[0]).toEqual({ pushed: false, reason: 'not_holding_lock' });
  });
});

describe('hooks for watchdog + control-channel', () => {
  it('isHoldingLock reflects internal state', async () => {
    const { leader } = makeLeader();
    expect(leader.isHoldingLock()).toBe(false);
    await leader._stepForTest(); // cold acquire
    expect(leader.isHoldingLock()).toBe(true);
  });

  it('isKnownPeer uses the cache from listFreshPeers', async () => {
    const mocks = makeMocks({
      initialPeers: [
        { instance_id: 'inst-B', updated_at: 100 },
        { instance_id: 'inst-C', updated_at: 100 },
      ],
    });
    const { leader } = makeLeader({ mocks });
    await leader._stepForTest();
    expect(leader.isKnownPeer('inst-B')).toBe(true);
    expect(leader.isKnownPeer('inst-C')).toBe(true);
    expect(leader.isKnownPeer('inst-Z')).toBe(false);
  });

  it('releaseLockForExit clears flag + calls lock.releaseLock', async () => {
    const { leader, lock } = makeLeader();
    await leader._stepForTest();
    expect(leader.isHoldingLock()).toBe(true);

    await leader.releaseLockForExit();
    expect(leader.isHoldingLock()).toBe(false);
    expect(lock.releaseLock).toHaveBeenCalledTimes(1);
  });
});

describe('start / stop lifecycle', () => {
  it('start is idempotent', async () => {
    const sleep = jest.fn(() => new Promise(() => {}));
    const { leader } = makeLeader({ sleep });
    leader.start();
    leader.start();
    leader.start();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(sleep).toHaveBeenCalledTimes(1);
  });

  it('stop halts the loop and returns a promise that resolves when the loop exits', async () => {
    const sleepResolvers = [];
    const sleep = jest.fn(() => new Promise((resolve) => { sleepResolvers.push(resolve); }));
    const { leader, peerHeartbeat } = makeLeader({ sleep });
    leader.start();

    await new Promise((resolve) => { setImmediate(resolve); });
    expect(sleep).toHaveBeenCalledTimes(1);

    const stopPromise = leader.stop();
    sleepResolvers[0](); // wake the loop so it observes running=false
    await stopPromise;

    // After stop, the loop must not have called writeHeartbeat (the
    // first tick's work — running=false check skips step).
    expect(peerHeartbeat.writeHeartbeat).not.toHaveBeenCalled();
  });

  it('start after stop without awaiting does NOT orphan a second loop', async () => {
    // Lifecycle correctness: a re-start must wait for the prior
    // loop to fully exit, otherwise the in-flight `await sleep(...)`
    // in the OLD loop resumes after a `start()` toggles running=true
    // again, and we get two concurrent ticks against the same
    // single-caller-only gateway-lock. Guard: start() is a no-op
    // while loopPromise is still pending.
    const sleepResolvers = [];
    const sleep = jest.fn(() => new Promise((resolve) => { sleepResolvers.push(resolve); }));
    const { leader } = makeLeader({ sleep });

    leader.start();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(sleep).toHaveBeenCalledTimes(1);

    // stop() does NOT immediately resolve — the old loop is still
    // inside the sleep promise. Calling start() now must NOT spawn
    // a second loop.
    leader.stop();
    leader.start();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(sleep).toHaveBeenCalledTimes(1); // still only 1, not 2

    // Wake the old loop so it can exit, then start fresh.
    sleepResolvers[0]();
    await new Promise((resolve) => { setImmediate(resolve); });
    leader.start(); // now safe — loopPromise resolved
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(sleep).toHaveBeenCalledTimes(2);
  });
});
