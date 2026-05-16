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
//        - transferLock success + push success → transferred:true,
//          pushAcked:true.
//        - transferLock success + push timeout → transferred:true,
//          pushAcked:false, pushReason:'timeout' (active still exits;
//          standby's watchdog recovers).
//   5. Serialization: tick + inbound-handoff + push-handoff funnel
//      through the same in-flight chain. No two lock mutators ever
//      run concurrently.
//   6. Hooks: isHoldingLock + isKnownPeer + releaseLockForImmediateExit
//      reflect internal state; releaseLockForImmediateExit clears flag +
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
    deleteOwnRow: jest.fn(async () => {}),
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
      lock: {}, peerHeartbeat: {}, controlClient: {}, manager: { connect() {}, isConnected() {} },
    })).toThrow(/selfInstanceId/);
    expect(() => createGatewayLeader({
      lock: {}, peerHeartbeat: {}, controlClient: {}, manager: { connect() {}, isConnected() {} },
      selfInstanceId: 'a',
    })).toThrow(/shardId/);
    expect(() => createGatewayLeader({
      lock: {}, peerHeartbeat: {}, controlClient: {}, manager: { connect() {}, isConnected() {} },
      selfInstanceId: 'a', shardId: 's',
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

  it('rejects a stray inbound handoff when already holding lock + connected', async () => {
    // Defense against a duplicate or misrouted handoff body that
    // passed the server's HMAC + routing checks. Re-adopting would
    // re-anchor currentVersion against a possibly-moved row;
    // re-calling manager.connect() would race the existing WS
    // (WebSocketManager is NOT concurrent-safe).
    const mocks = makeMocks();
    mocks.manager.isConnected = jest.fn(() => true);
    const { leader } = makeLeader({ mocks });
    await leader._stepForTest(); // heldLock=true via cold acquire

    await expect(leader.handleInboundHandoff({
      activeInstanceId: 'inst-X', expectedVersion: 99,
    })).rejects.toThrow(/already_holding_lock_and_connected/);

    // Critical: must NOT have called adopt or connect on the stray.
    expect(mocks.lock.adoptLockFromHandoff).not.toHaveBeenCalled();
    expect(mocks.manager.connect).not.toHaveBeenCalled();
  });

  it('adopt throw — heldLock + connecting both stay false; runSerialized chain stays intact for next call', async () => {
    // Adopt is the first step of the handoff. If it throws, NO
    // flag mutations must happen (heldLock=false, connecting=false),
    // and the serialization chain must still accept the next op.
    const mocks = makeMocks();
    mocks.lock.adoptLockFromHandoff = jest.fn(() => {
      throw new Error('bad-version');
    });
    const { leader, manager } = makeLeader({ mocks });

    await expect(leader.handleInboundHandoff({
      activeInstanceId: 'inst-X', expectedVersion: 0,
    })).rejects.toThrow(/bad-version/);

    expect(leader.isHoldingLock()).toBe(false);
    expect(leader.isConnecting()).toBe(false);
    expect(manager.connect).not.toHaveBeenCalled();

    // Chain still works: a subsequent tick must run cleanly.
    await leader._stepForTest();
    expect(mocks.lock.acquireLock).toHaveBeenCalledTimes(1);
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

  it('synchronous manager.connect throw clears connecting flag (defensive)', async () => {
    // @discordjs/ws's WebSocketManager.connect contract is async, but
    // a future shim regression could surface a sync throw. The flag
    // must clear so the watchdog can take over the retry — otherwise
    // it would no-op forever.
    const mocks = makeMocks();
    mocks.manager.connect = jest.fn(() => { throw new Error('sync-shim-bug'); });
    const { leader } = makeLeader({ mocks });

    await expect(leader.handleInboundHandoff({
      activeInstanceId: 'inst-A', expectedVersion: 7,
    })).rejects.toThrow(/sync-shim-bug/);

    expect(leader.isConnecting()).toBe(false);
    expect(leader.isHoldingLock()).toBe(true);
  });

  it('non-thenable manager.connect return clears connecting flag (defensive)', async () => {
    // Even stronger defensive: if connect() returns a non-thenable
    // (i.e., the .then attachment itself throws synchronously after
    // connectPromise is already assigned), the lifecycle handlers
    // never register. The catch must still clear `connecting=false`.
    const mocks = makeMocks();
    mocks.manager.connect = jest.fn(() => 'not-a-promise');
    const { leader } = makeLeader({ mocks });

    await expect(leader.handleInboundHandoff({
      activeInstanceId: 'inst-A', expectedVersion: 7,
    })).rejects.toThrow();

    expect(leader.isConnecting()).toBe(false);
    expect(leader.isHoldingLock()).toBe(true);
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
    expect(result).toEqual({ transferred: false, reason: 'not_holding_lock' });
  });

  it('returns no_peer + best-effort release when no fresh peer exists', async () => {
    const mocks = makeMocks({ initialPeers: [] });
    const { leader, lock } = makeLeader({ mocks });
    await preHoldLock(leader);

    const result = await leader.pushHandoff();
    expect(result).toEqual({ transferred: false, reason: 'no_peer' });
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
    expect(result).toEqual({ transferred: false, reason: 'transfer_failed' });
    expect(controlClient.pushHandoff).not.toHaveBeenCalled();
    expect(lock.releaseLock).not.toHaveBeenCalled();
  });

  it('returns transferred:true, pushAcked:true on successful transfer + ACKed push', async () => {
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-B/inst-B', updated_at: 100,
      }],
    });
    const { leader, lock, controlClient } = makeLeader({ mocks });
    await preHoldLock(leader);

    const result = await leader.pushHandoff();
    expect(result).toEqual({ transferred: true, pushAcked: true });
    expect(lock.transferLock).toHaveBeenCalledWith('inst-B', 'task-B/inst-B');
    expect(controlClient.pushHandoff).toHaveBeenCalledWith({
      peerIp: '10.0.0.2', peerPort: 9876, peerInstanceId: 'inst-B',
      selfInstanceId: 'inst-A', expectedVersion: 3,
    });
    expect(leader.isHoldingLock()).toBe(false);
  });

  it('returns transferred:true, pushAcked:false, pushReason:timeout when transferred but peer did not ACK', async () => {
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
    expect(result).toEqual({ transferred: true, pushAcked: false, pushReason: 'timeout' });
  });

  it('returns transferred:true, pushAcked:false, pushReason:push_threw when controlClient.pushHandoff throws', async () => {
    // The control-client's documented contract is "never throws", but
    // its synchronous argument validators DO throw if a row makes it
    // past listFreshPeers' filters in a future regression. transferLock
    // has already moved the lock in DDB at this point, so the standby
    // owns it either way; the standby's watchdog catches lock-held +
    // WS-disconnected and brings up the gateway from the cold path.
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-B/inst-B', updated_at: 100,
      }],
    });
    mocks.controlClient.pushHandoff = jest.fn(async () => {
      throw new Error('pushHandoff: peerIp (IPv4 or IPv6 literal) required');
    });
    const { leader, logger } = makeLeader({ mocks });
    await preHoldLock(leader);

    const result = await leader.pushHandoff();
    expect(result).toEqual({ transferred: true, pushAcked: false, pushReason: 'push_threw' });
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/controlClient\.pushHandoff threw/),
      expect.objectContaining({ peerInstanceId: 'inst-B' }),
    );
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
    expect(lock.transferLock).toHaveBeenCalledWith('inst-B', 'placeholder/inst-B');
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

    // After push completes, `closed=true` is latched — the queued
    // tick's closed-guard now bails before touching the lock. This
    // matches handleInboundHandoff's closed-state invariant: any
    // work queued behind pushHandoff is a no-op once the leader is
    // terminal. The serialization contract is still pinned (the
    // tick waited; transferLock was called exactly once).
    expect(lock.acquireLock.mock.calls.length).toBe(firstAcquireCalls);
    expect(lock.transferLock).toHaveBeenCalledTimes(1);
  });

  it('a SIGTERM pushHandoff during an in-flight inbound-handoff queues behind it', async () => {
    // Real race: handleInboundHandoff is awaiting manager.connect()
    // when SIGTERM fires. pushHandoff must queue behind the inbound
    // handoff on the serialization chain — otherwise both would
    // touch the lock state concurrently.
    const callOrder = [];
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-X', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-X/inst-X', updated_at: 100,
      }],
    });
    let resolveConnect;
    mocks.lock.adoptLockFromHandoff = jest.fn(() => callOrder.push('adopt'));
    mocks.manager.connect = jest.fn(() => new Promise((resolve) => {
      callOrder.push('connect-start');
      resolveConnect = resolve;
    }));
    mocks.lock.transferLock = jest.fn(async () => {
      callOrder.push('transfer');
      return { transferred: true, version: 3 };
    });
    const { leader } = makeLeader({ mocks });

    // Pre-hold lock via a tick so pushHandoff has something to do.
    // (Note: this acquires; handleInboundHandoff below will overwrite
    // the version cursor via adoptLockFromHandoff. That's a slight
    // semantic stretch — in reality we wouldn't have both — but the
    // test is about serialization ordering.)
    await leader._stepForTest();
    callOrder.length = 0;

    // Kick off inbound handoff; blocks at connect-start.
    const inbound = leader.handleInboundHandoff({
      activeInstanceId: 'inst-X', expectedVersion: 5,
    });
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(callOrder).toEqual(['adopt', 'connect-start']);

    // SIGTERM fires now: pushHandoff queues.
    const push = leader.pushHandoff();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(callOrder).toEqual(['adopt', 'connect-start']); // still blocked

    // Release the inbound handoff; push then runs.
    resolveConnect();
    await inbound;
    await push;
    expect(callOrder).toEqual(['adopt', 'connect-start', 'transfer']);
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

  it('inbound handoff connect times out internally; isConnecting stays true until underlying connect settles', async () => {
    // A hung Discord WS connect would pin connecting=true forever
    // without the internal timeout. The leader races connect
    // against a timer and THROWS on timeout — but critically,
    // `isConnecting()` stays true until the UNDERLYING connect
    // promise actually settles. Otherwise the next watchdog tick
    // (1s) would fire its OWN manager.connect() while the original
    // is still pending in @discordjs/ws — exactly the concurrent-
    // connect race the flag exists to prevent.
    const mocks = makeMocks();
    let resolveUnderlying;
    mocks.manager.connect = jest.fn(() => new Promise((resolve) => {
      resolveUnderlying = resolve;
    }));
    const leader = createGatewayLeader({
      lock: mocks.lock,
      peerHeartbeat: mocks.peerHeartbeat,
      controlClient: mocks.controlClient,
      manager: mocks.manager,
      selfInstanceId: 'inst-A',
      shardId: '0:1',
      logger: mocks.logger,
      inboundConnectTimeoutMs: 50,
    });

    await expect(leader.handleInboundHandoff({
      activeInstanceId: 'inst-X', expectedVersion: 5,
    })).rejects.toThrow(/inbound_connect_timeout/);

    // Post-timeout-throw: connecting MUST still be true. The
    // underlying connect is still pending — the watchdog must
    // back off, not race a second connect.
    expect(leader.isConnecting()).toBe(true);
    // heldLock stays true so the watchdog observes lock-held but
    // is held off by isConnecting until the underlying settles.
    expect(leader.isHoldingLock()).toBe(true);

    // Resolve the underlying connect — flag clears, watchdog
    // can take over.
    resolveUnderlying();
    await new Promise((resolve) => { setImmediate(resolve); });
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

describe('pushHandoff — terminal contract (closed sentinel)', () => {
  it('step() is a no-op after pushHandoff (closed-guard mirrors handleInboundHandoff)', async () => {
    // Mirrors the closed-guard contract: handleInboundHandoff has an
    // inner closed re-check inside the serialized closure; step() now
    // has its own closed-guard so a stray post-close `_stepForTest`
    // call doesn't re-write heartbeat / re-acquire / re-renew. The
    // production loop guards via running=false, but the seam needs
    // to match the rest of the module's invariants.
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-B/inst-B', updated_at: 100,
      }],
    });
    const { leader, peerHeartbeat } = makeLeader({ mocks });
    await leader._stepForTest();
    await leader.pushHandoff();
    const writeCallsBefore = peerHeartbeat.writeHeartbeat.mock.calls.length;
    await leader._stepForTest();
    // No new heartbeat, no new peer-list refresh, no new lock work.
    expect(peerHeartbeat.writeHeartbeat).toHaveBeenCalledTimes(writeCallsBefore);
  });

  it('after pushHandoff, start() is a permanent no-op', async () => {
    const sleep = jest.fn(() => new Promise(() => {})); // never resolves
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-B/inst-B', updated_at: 100,
      }],
    });
    const { leader } = makeLeader({ mocks, sleep });
    leader.start();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(sleep).toHaveBeenCalledTimes(1);

    await leader._stepForTest(); // heldLock=true
    await leader.pushHandoff();

    // After pushHandoff, start() must NOT schedule another tick —
    // the leader is terminal (SIGTERM handler is about to exit).
    const sleepCallsBeforeRestart = sleep.mock.calls.length;
    leader.start();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(sleep.mock.calls.length).toBe(sleepCallsBeforeRestart);
  });

  it('calls peerHeartbeat.deleteOwnRow on the happy path', async () => {
    // SIGTERM cleanup: the dying replica should close its discovery
    // window immediately rather than wait the freshness window for
    // the row to fade naturally.
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-B/inst-B', updated_at: 100,
      }],
    });
    mocks.peerHeartbeat.deleteOwnRow = jest.fn(async () => {});
    const { leader, peerHeartbeat } = makeLeader({ mocks });
    await leader._stepForTest();
    await leader.pushHandoff();
    expect(peerHeartbeat.deleteOwnRow).toHaveBeenCalledTimes(1);
  });

  it('calls deleteOwnRow on the no_peer branch too', async () => {
    const mocks = makeMocks({ initialPeers: [] });
    mocks.peerHeartbeat.deleteOwnRow = jest.fn(async () => {});
    const { leader, peerHeartbeat } = makeLeader({ mocks });
    await leader._stepForTest();
    const result = await leader.pushHandoff();
    expect(result.reason).toBe('no_peer');
    expect(peerHeartbeat.deleteOwnRow).toHaveBeenCalledTimes(1);
  });

  it('a deleteOwnRow failure does NOT bubble into the pushHandoff result', async () => {
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-B/inst-B', updated_at: 100,
      }],
    });
    mocks.peerHeartbeat.deleteOwnRow = jest.fn(async () => { throw new Error('ddb-throttle'); });
    const { leader } = makeLeader({ mocks });
    await leader._stepForTest();
    const result = await leader.pushHandoff();
    // Push still succeeds; cleanup is best-effort.
    expect(result).toEqual({ transferred: true, pushAcked: true });
  });

  it('handleInboundHandoff after pushHandoff rejects with leader_closed', async () => {
    // A race where the SIGTERM path ran AND an inbound handoff hits
    // the control-channel server BEFORE the process exits. The leader
    // is dead — must not re-adopt. Uniform terminal-state invariant
    // alongside start() and releaseLockForImmediateExit.
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-B/inst-B', updated_at: 100,
      }],
    });
    const { leader, lock } = makeLeader({ mocks });
    await leader._stepForTest();
    await leader.pushHandoff();

    await expect(leader.handleInboundHandoff({
      activeInstanceId: 'inst-X', expectedVersion: 99,
    })).rejects.toThrow(/leader_closed/);
    // adoptLockFromHandoff must NOT have run.
    expect(lock.adoptLockFromHandoff).not.toHaveBeenCalled();
  });

  it('inner closed re-check: inbound-handoff queued BEFORE pushHandoff aborts when its turn comes', async () => {
    // The race: an inbound-handoff arrives, passes the outer closed
    // check, and is queued in runSerialized behind a tick. SIGTERM
    // fires DURING the queue wait: pushHandoff sets closed=true and
    // queues its own work behind the inbound. When the inbound work
    // pops off the chain and starts running, it must observe
    // closed=true and throw — otherwise it would adopt + connect
    // against a soon-to-be-transferred lock.
    //
    // Construct: gate a tick on a slow renewLock so the chain blocks,
    // kick inbound, kick pushHandoff, then release renewLock and
    // verify inbound throws leader_closed and adoptLockFromHandoff
    // never ran.
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-B/inst-B', updated_at: 100,
      }],
    });
    const { leader, lock } = makeLeader({ mocks });
    // Acquire the lock first (heldLock=true) via a tick before we
    // install the blocking renewLock mock — otherwise the lock
    // module would be the one stuck.
    await leader._stepForTest();
    expect(leader.isHoldingLock()).toBe(true);

    // Now swap renewLock to block on a held promise. The next tick
    // will queue and never settle until we release.
    let releaseRenew;
    mocks.lock.renewLock = jest.fn(() => new Promise((resolve) => {
      releaseRenew = () => resolve({ renewed: true, version: 7 });
    }));

    // Kick a tick (queued #1, will block on renewLock).
    const tickPromise = leader._stepForTest();
    // Yield microtasks so the tick reaches the blocking renewLock.
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(typeof releaseRenew).toBe('function');

    // Kick the inbound (queued #2, will wait for tick to finish).
    const inboundPromise = leader.handleInboundHandoff({
      activeInstanceId: 'inst-X', expectedVersion: 99,
    });
    // SIGTERM fires (queued #3 + sets closed=true). pushHandoff's
    // outer side-effect is `closed = true; running = false`, so by
    // the time the inbound work pops, closed has latched.
    const pushPromise = leader.pushHandoff();

    // Let the tick complete so the chain drains.
    releaseRenew();
    await tickPromise;

    // Inbound must reject with leader_closed and never call adopt.
    await expect(inboundPromise).rejects.toThrow(/leader_closed/);
    expect(lock.adoptLockFromHandoff).not.toHaveBeenCalled();

    // pushHandoff still proceeds to transfer.
    await pushPromise;
  });

  it('releaseLockForImmediateExit after pushHandoff is a no-op (closed)', async () => {
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-B/inst-B', updated_at: 100,
      }],
    });
    const { leader, lock } = makeLeader({ mocks });
    await leader._stepForTest();
    await leader.pushHandoff();
    const releaseCallsBefore = lock.releaseLock.mock.calls.length;

    await leader.releaseLockForImmediateExit();
    // releaseLock NOT called again — the closed sentinel short-circuits.
    expect(lock.releaseLock.mock.calls.length).toBe(releaseCallsBefore);
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
    expect(first).toEqual({ transferred: true, pushAcked: true });

    const second = await leader.pushHandoff();
    expect(second).toEqual({ transferred: false, reason: 'not_holding_lock' });

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
    const transferred = [a, b].filter((r) => r.transferred === true);
    const noOps = [a, b].filter((r) => r.transferred === false);
    expect(transferred).toHaveLength(1);
    expect(noOps).toHaveLength(1);
    expect(noOps[0]).toEqual({ transferred: false, reason: 'not_holding_lock' });
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

  it('releaseLockForImmediateExit clears flag + calls lock.releaseLock', async () => {
    const { leader, lock } = makeLeader();
    await leader._stepForTest();
    expect(leader.isHoldingLock()).toBe(true);

    await leader.releaseLockForImmediateExit();
    expect(leader.isHoldingLock()).toBe(false);
    expect(lock.releaseLock).toHaveBeenCalledTimes(1);
  });
});

describe('loop backstop — survives unexpected throws from step()', () => {
  it('a synchronous throw from peerHeartbeat.listFreshPeers does not kill the loop', async () => {
    // Symmetric to the watchdog's loop backstop. A synchronous throw
    // (NOT a rejecting promise) from any awaited operation inside
    // step() escapes the inner try/catch arms — `Promise.all([...])`
    // captures sync rejections, but the array-element construction
    // itself can throw synchronously and that's not caught by the
    // inner .catch() arms. Without the loop-level backstop, the
    // loop's `await runSerialized(step)` would reject and exit.
    const sleepResolvers = [];
    const sleep = jest.fn(() => new Promise((resolve) => { sleepResolvers.push(resolve); }));
    const mocks = makeMocks();
    let throwCount = 0;
    mocks.peerHeartbeat.listFreshPeers = jest.fn(() => {
      throwCount += 1;
      // Synchronous throw, NOT a rejecting promise.
      throw new Error('sync-list-throw');
    });

    const { leader, logger } = makeLeader({ mocks, sleep });
    leader.start();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(sleep).toHaveBeenCalledTimes(1);

    // Tick #1: throws synchronously → backstop catches → loop
    // schedules tick #2 sleep.
    sleepResolvers[0]();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(throwCount).toBeGreaterThanOrEqual(1);
    // Loop must NOT have exited — another sleep was scheduled.
    expect(sleep.mock.calls.length).toBeGreaterThan(1);
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/tick threw unexpectedly/),
      expect.objectContaining({ error: 'sync-list-throw' }),
    );

    leader.stop();
    while (sleepResolvers.length > 0) sleepResolvers.shift()();
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
