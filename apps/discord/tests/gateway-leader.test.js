
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
    isRecovering: jest.fn(() => false),
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
    })).toThrow(/isRecovering/);
    expect(() => createGatewayLeader({
      lock: {}, peerHeartbeat: {}, controlClient: {}, manager: { connect() {}, isConnected() {}, isRecovering() {} },
    })).toThrow(/selfInstanceId/);
    expect(() => createGatewayLeader({
      lock: {}, peerHeartbeat: {}, controlClient: {}, manager: { connect() {}, isConnected() {}, isRecovering() {} },
      selfInstanceId: 'a',
    })).toThrow(/shardId/);
    expect(() => createGatewayLeader({
      lock: {}, peerHeartbeat: {}, controlClient: {}, manager: { connect() {}, isConnected() {}, isRecovering() {} },
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
    const mocks = makeMocks();
    mocks.manager.isConnected = jest.fn(() => true);
    const { leader } = makeLeader({ mocks });
    await leader._stepForTest(); // heldLock=true via cold acquire

    await expect(leader.handleInboundHandoff({
      activeInstanceId: 'inst-X', expectedVersion: 99,
    })).rejects.toThrow(/already_holding_lock_and_active_ws_lifecycle/);

    expect(mocks.lock.adoptLockFromHandoff).not.toHaveBeenCalled();
    expect(mocks.manager.connect).not.toHaveBeenCalled();
  });

  it('adopts without calling connect when @discordjs/ws is already recovering', async () => {
    const mocks = makeMocks();
    mocks.manager.isRecovering = jest.fn(() => true);
    const { leader, logger } = makeLeader({ mocks });

    await leader.handleInboundHandoff({
      activeInstanceId: 'inst-A', expectedVersion: 7,
    });

    expect(mocks.lock.adoptLockFromHandoff).toHaveBeenCalledWith(7);
    expect(leader.isHoldingLock()).toBe(true);
    expect(mocks.manager.connect).not.toHaveBeenCalled();
    expect(logger.info).toHaveBeenCalledWith(
      expect.stringContaining('automatic recovery already in progress'),
      expect.objectContaining({ activeInstanceId: 'inst-A', expectedVersion: 7 }),
    );
  });

  it('rejects a duplicate inbound handoff while already holding lock + recovering', async () => {
    const mocks = makeMocks();
    const { leader } = makeLeader({ mocks });
    await leader._stepForTest();
    mocks.manager.isRecovering.mockReturnValue(true);

    await expect(leader.handleInboundHandoff({
      activeInstanceId: 'inst-X', expectedVersion: 99,
    })).rejects.toThrow(/already_holding_lock_and_active_ws_lifecycle/);

    expect(mocks.lock.adoptLockFromHandoff).not.toHaveBeenCalled();
    expect(mocks.manager.connect).not.toHaveBeenCalled();
  });

  it('inbound handoff on a never-started leader: adopts + connects without start()', async () => {
    const callOrder = [];
    const mocks = makeMocks();
    mocks.lock.adoptLockFromHandoff = jest.fn(() => callOrder.push('adopt'));
    const { leader } = makeLeader({ mocks });

    let flagAtConnect = null;
    let tickLoopStartedAtConnect = null;
    mocks.manager.connect = jest.fn(async () => {
      flagAtConnect = leader.isHoldingLock();
      tickLoopStartedAtConnect = leader.hasStartedTickLoop();
      callOrder.push('connect');
    });

    expect(leader.hasStartedTickLoop()).toBe(false);

    await leader.handleInboundHandoff({
      activeInstanceId: 'inst-A', expectedVersion: 7,
    });

    expect(callOrder).toEqual(['adopt', 'connect']);
    expect(flagAtConnect).toBe(true);
    expect(tickLoopStartedAtConnect).toBe(false); // confirms the loop never ran
    expect(leader.isHoldingLock()).toBe(true);
    expect(mocks.peerHeartbeat.writeHeartbeat).not.toHaveBeenCalled();
  });

  it('adopt throw — heldLock + connecting both stay false; runSerialized chain stays intact for next call', async () => {
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
        updated_at: 100,
      }],
    });
    const { leader, lock } = makeLeader({ mocks });
    await preHoldLock(leader);

    await leader.pushHandoff();
    expect(lock.transferLock).toHaveBeenCalledWith('inst-B', 'placeholder/inst-B');
  });

  it('stops the tick loop before the transferLock', async () => {
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
    expect(sleep).toHaveBeenCalledTimes(1);
  });
});

describe('serialization — SIGTERM-during-tick (pushHandoff while a tick is in-flight)', () => {
  it('pushHandoff queues behind an in-flight tick and runs after the tick settles', async () => {
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
    await leader._stepForTest(); // heldLock=true via acquire

    const tick2 = leader._stepForTest();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(callOrder).toEqual(['renew-start']);

    const push = leader.pushHandoff();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(callOrder).toEqual(['renew-start']); // still blocked

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

    lock.transferLock.mockClear();
    const firstAcquireCalls = lock.acquireLock.mock.calls.length;

    const pushPromise = leader.pushHandoff();
    const tickPromise = leader._stepForTest();

    await new Promise((resolve) => { setImmediate(resolve); });

    expect(lock.transferLock).toHaveBeenCalledTimes(1); // from push
    expect(lock.acquireLock.mock.calls.length).toBe(firstAcquireCalls);

    resolvePush({ ok: true, status: 200 });
    await pushPromise;
    await tickPromise;

    expect(lock.acquireLock.mock.calls.length).toBe(firstAcquireCalls);
    expect(lock.transferLock).toHaveBeenCalledTimes(1);
  });

  it('a SIGTERM pushHandoff during an in-flight inbound-handoff queues behind it', async () => {
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

    await leader._stepForTest();
    callOrder.length = 0;

    const inbound = leader.handleInboundHandoff({
      activeInstanceId: 'inst-X', expectedVersion: 5,
    });
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(callOrder).toEqual(['adopt', 'connect-start']);

    const push = leader.pushHandoff();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(callOrder).toEqual(['adopt', 'connect-start']); // still blocked

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
    expect(callOrder).toEqual(['adopt-5', 'connect-start-1']);

    connectResolves[0]();
    await p1;
    await new Promise((resolve) => { setImmediate(resolve); });
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
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(leader.isConnecting()).toBe(true);

    resolveConnect();
    await handoffPromise;
    expect(leader.isConnecting()).toBe(false);
  });

  it('inbound handoff connect times out internally; isConnecting stays true until underlying connect settles', async () => {
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

    expect(leader.isConnecting()).toBe(true);
    expect(leader.isHoldingLock()).toBe(true);

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
    expect(leader.isConnecting()).toBe(false);
  });
});

describe('pushHandoff — terminal contract (closed sentinel)', () => {
  it('step() is a no-op after pushHandoff (closed-guard mirrors handleInboundHandoff)', async () => {
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

    const sleepCallsBeforeRestart = sleep.mock.calls.length;
    leader.start();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(sleep.mock.calls.length).toBe(sleepCallsBeforeRestart);
  });

  it('calls peerHeartbeat.deleteOwnRow on the happy path', async () => {
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
    expect(result).toEqual({ transferred: true, pushAcked: true });
  });

  it('handleInboundHandoff after pushHandoff rejects with leader_closed', async () => {
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
    expect(lock.adoptLockFromHandoff).not.toHaveBeenCalled();
  });

  it('inner closed re-check: inbound-handoff queued BEFORE pushHandoff aborts when its turn comes', async () => {
    const mocks = makeMocks({
      initialPeers: [{
        instance_id: 'inst-B', ip: '10.0.0.2', port: 9876,
        lock_holder: 'task-B/inst-B', updated_at: 100,
      }],
    });
    const { leader, lock } = makeLeader({ mocks });
    await leader._stepForTest();
    expect(leader.isHoldingLock()).toBe(true);

    let releaseRenew;
    mocks.lock.renewLock = jest.fn(() => new Promise((resolve) => {
      releaseRenew = () => resolve({ renewed: true, version: 7 });
    }));

    const tickPromise = leader._stepForTest();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(typeof releaseRenew).toBe('function');

    const inboundPromise = leader.handleInboundHandoff({
      activeInstanceId: 'inst-X', expectedVersion: 99,
    });
    const pushPromise = leader.pushHandoff();

    releaseRenew();
    await tickPromise;

    await expect(inboundPromise).rejects.toThrow(/leader_closed/);
    expect(lock.adoptLockFromHandoff).not.toHaveBeenCalled();

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
    expect(lock.releaseLock.mock.calls.length).toBe(releaseCallsBefore);
  });
});

describe('pushHandoff — re-entry safety', () => {
  it('a second pushHandoff call after the first transferred returns not_holding_lock', async () => {
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
    const sleepResolvers = [];
    const sleep = jest.fn(() => new Promise((resolve) => { sleepResolvers.push(resolve); }));
    const mocks = makeMocks();
    let throwCount = 0;
    mocks.peerHeartbeat.listFreshPeers = jest.fn(() => {
      throwCount += 1;
      throw new Error('sync-list-throw');
    });

    const { leader, logger } = makeLeader({ mocks, sleep });
    leader.start();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(sleep).toHaveBeenCalledTimes(1);

    sleepResolvers[0]();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(throwCount).toBeGreaterThanOrEqual(1);
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
  it('stop() before start() is a safe no-op (returns resolved promise, no side effects)', async () => {
    const mocks = makeMocks();
    const { leader } = makeLeader({ mocks });

    await expect(leader.stop()).resolves.toBeUndefined();

    expect(mocks.lock.acquireLock).not.toHaveBeenCalled();
    expect(mocks.lock.releaseLock).not.toHaveBeenCalled();
    expect(mocks.peerHeartbeat.writeHeartbeat).not.toHaveBeenCalled();
    expect(leader.isHoldingLock()).toBe(false);
    expect(leader.hasStartedTickLoop()).toBe(false);
  });

  it('stop() is idempotent across repeated pre-start calls', async () => {
    const mocks = makeMocks();
    const { leader } = makeLeader({ mocks });

    await leader.stop();
    await leader.stop();
    await leader.stop();

    expect(mocks.lock.acquireLock).not.toHaveBeenCalled();
    expect(leader.hasStartedTickLoop()).toBe(false);
  });

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

    expect(peerHeartbeat.writeHeartbeat).not.toHaveBeenCalled();
  });

  it('start after stop without awaiting does NOT orphan a second loop', async () => {
    const sleepResolvers = [];
    const sleep = jest.fn(() => new Promise((resolve) => { sleepResolvers.push(resolve); }));
    const { leader } = makeLeader({ sleep });

    leader.start();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(sleep).toHaveBeenCalledTimes(1);

    leader.stop();
    leader.start();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(sleep).toHaveBeenCalledTimes(1); // still only 1, not 2

    sleepResolvers[0]();
    await new Promise((resolve) => { setImmediate(resolve); });
    leader.start(); // now safe — loopPromise resolved
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(sleep).toHaveBeenCalledTimes(2);
  });
});

describe('hasStartedTickLoop — /health probe seam', () => {
  it('returns false before start()', () => {
    const sleep = jest.fn(() => new Promise(() => {}));
    const { leader } = makeLeader({ sleep });
    expect(leader.hasStartedTickLoop()).toBe(false);
  });

  it('returns true after start()', async () => {
    const sleep = jest.fn(() => new Promise(() => {}));
    const { leader } = makeLeader({ sleep });
    leader.start();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(leader.hasStartedTickLoop()).toBe(true);
  });

  it('returns false again after stop() drains the loop', async () => {
    const sleepResolvers = [];
    const sleep = jest.fn(() => new Promise((resolve) => { sleepResolvers.push(resolve); }));
    const { leader } = makeLeader({ sleep });
    leader.start();
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(leader.hasStartedTickLoop()).toBe(true);

    const stopPromise = leader.stop();
    sleepResolvers[0]();
    await stopPromise;
    expect(leader.hasStartedTickLoop()).toBe(false);
  });
});
