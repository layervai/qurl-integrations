
const { EventEmitter } = require('node:events');
const {
  createGatewayWsShim,
  MAX_IDENTIFY_ATTEMPTS,
  DEFAULT_CONNECT_TIMEOUT_MS,
  VERIFIED_DJS_WS_MAJOR_MINOR,
} = require('../src/gateway-ws-shim');
const { WebSocketShardEvents } = require('@discordjs/ws');
const { GatewayCloseCodes } = require('discord-api-types/v10');

function makeSlowManagerCtor() {
  const instances = [];
  function SlowFakeManager(args) {
    const inst = Object.assign(new EventEmitter(), {
      _constructorArgs: args,
      connect: jest.fn(() => new Promise(() => { /* never resolves */ })),
      destroy: jest.fn().mockResolvedValue(undefined),
    });
    instances.push(inst);
    return inst;
  }
  return { SlowFakeManager, instances };
}

function makeFakeManagerCtor() {
  const instances = [];
  function FakeManager(args) {
    const inst = new EventEmitter();
    inst._constructorArgs = args;
    inst._destroyCalls = [];
    inst.connect = jest.fn().mockResolvedValue(undefined);
    inst.destroy = jest.fn().mockImplementation((opts) => {
      inst._destroyCalls.push(opts);
      return Promise.resolve();
    });
    instances.push(inst);
    return inst;
  }
  return { FakeManager, instances };
}

function makeFakeRESTCtor() {
  const instances = [];
  function FakeREST() {
    const inst = { token: null, setToken: jest.fn() };
    inst.setToken.mockImplementation((t) => {
      inst.token = t;
      return inst;
    });
    instances.push(inst);
    return inst;
  }
  return { FakeREST, instances };
}

function makeFakeStore() {
  let mirror = null;
  return {
    hydrate: jest.fn().mockResolvedValue(null),
    retrieveSessionInfo: jest.fn(() => mirror),
    updateSessionInfo: jest.fn(async (_shardId, info) => { mirror = info; }),
    flushFinal: jest.fn().mockResolvedValue(undefined),
    stop: jest.fn(),
    _setMirror: (val) => { mirror = val; },
  };
}

function makeFakeLogger() {
  return {
    info: jest.fn(),
    warn: jest.fn(),
    error: jest.fn(),
    debug: jest.fn(),
  };
}

function makeShim(overrides = {}) {
  const { FakeManager, instances: managerInstances } = makeFakeManagerCtor();
  const { FakeREST, instances: restInstances } = makeFakeRESTCtor();
  const store = makeFakeStore();
  const logger = makeFakeLogger();
  const shim = createGatewayWsShim({
    token: 'test-token',
    intents: 1,
    store,
    logger,
    WebSocketManagerCtor: FakeManager,
    RESTCtor: FakeREST,
    ...overrides,
  });
  return { shim, store, logger, managerInstances, restInstances };
}

describe('createGatewayWsShim — factory validation', () => {
  it('throws when required args are missing', () => {
    expect(() => createGatewayWsShim()).toThrow(/token is required/);
    expect(() => createGatewayWsShim({ token: 't' })).toThrow(/intents/);
    expect(() => createGatewayWsShim({ token: 't', intents: 0 })).toThrow(/store is required/);
    expect(() => createGatewayWsShim({ token: 't', intents: 0, store: {} })).toThrow(/logger is required/);
  });
});

describe('hydrate', () => {
  it('delegates to store.hydrate', async () => {
    const { shim, store } = makeShim();
    store.hydrate.mockResolvedValue({ sessionId: 'sess-A', resumeURL: 'wss://r/a', sequence: 5 });

    const result = await shim.hydrate();

    expect(result).toEqual({ sessionId: 'sess-A', resumeURL: 'wss://r/a', sequence: 5 });
    expect(store.hydrate).toHaveBeenCalledTimes(1);
  });
});

describe('start — wiring + connect', () => {
  it('constructs the manager with token, intents, rest, and callbacks', async () => {
    const { shim, managerInstances, restInstances } = makeShim();
    await shim.start();

    expect(managerInstances).toHaveLength(1);
    const args = managerInstances[0]._constructorArgs;
    expect(args.token).toBe('test-token');
    expect(args.intents).toBe(1);
    expect(restInstances).toHaveLength(1);
    expect(restInstances[0].setToken).toHaveBeenCalledWith('test-token');
    expect(args.rest).toBe(restInstances[0]);
    expect(typeof args.retrieveSessionInfo).toBe('function');
    expect(typeof args.updateSessionInfo).toBe('function');
  });

  it('rejects when start() is called twice', async () => {
    const { shim } = makeShim();
    await shim.start();
    await expect(shim.start()).rejects.toThrow(/start\(\) called twice/);
  });

  it('rejects when start() is called after stop()', async () => {
    const { shim } = makeShim();
    await shim.stop();
    await expect(shim.start()).rejects.toThrow(/start\(\) after stop\(\)/);
  });

  it('drops late dispatches that arrive after connect timeout (start-failure teardown race)', async () => {
    const { SlowFakeManager, instances: lateInstances } = makeSlowManagerCtor();
    const { shim } = makeShim({ WebSocketManagerCtor: SlowFakeManager });
    const handler = jest.fn();
    shim.onDispatch(handler);

    await expect(shim.start({ timeoutMs: 10 })).rejects.toThrow(/timed out/);

    const mgr = lateInstances[0];
    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'INTERACTION_CREATE', d: {} },
      shardId: 0,
    });
    expect(handler).not.toHaveBeenCalled();
  });

  it('rejects on connect timeout', async () => {
    const { SlowFakeManager } = makeSlowManagerCtor();
    const { shim } = makeShim({ WebSocketManagerCtor: SlowFakeManager });

    await expect(shim.start({ timeoutMs: 10 })).rejects.toThrow(/timed out after 10ms/);
  });

  it('connect:false skips manager.connect() — Pillar 3 hot-standby seam', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });

    expect(managerInstances).toHaveLength(1);
    expect(managerInstances[0].connect).not.toHaveBeenCalled();
  });

  it('connect:false still attaches Dispatch listener (fan-out works after a later connect)', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    const handler = jest.fn();
    shim.onDispatch(handler);

    managerInstances[0].emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'MESSAGE_CREATE', d: {} },
      shardId: 0,
    });
    expect(handler).toHaveBeenCalledTimes(1);
  });
});

describe('_getManagerForTest — test introspection seam', () => {
  it('returns null before start()', () => {
    const { shim } = makeShim();
    expect(shim._getManagerForTest()).toBeNull();
  });

  it('returns the WebSocketManager instance after start()', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start();
    expect(shim._getManagerForTest()).toBe(managerInstances[0]);
  });

  it('returns the manager after start({ connect: false }) too', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    expect(shim._getManagerForTest()).toBe(managerInstances[0]);
  });
});

describe('Pillar 3 manager contract — connect() + connection state', () => {

  it('exposes connect(), isConnected(), and isRecovering() on the returned shim', () => {
    const { shim } = makeShim();
    expect(typeof shim.connect).toBe('function');
    expect(typeof shim.isConnected).toBe('function');
    expect(typeof shim.isRecovering).toBe('function');
  });

  it('connect() throws before start() (no manager yet)', async () => {
    const { shim } = makeShim();
    await expect(shim.connect()).rejects.toThrow(/connect\(\) called before start\(\)/);
  });

  it('connect() delegates to the underlying manager once start() has run', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    await shim.connect();
    expect(managerInstances[0].connect).toHaveBeenCalledTimes(1);
  });

  it('connect() rejects while @discordjs/ws owns automatic recovery', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    managerInstances[0].emit(WebSocketShardEvents.Closed, { code: 4200, shardId: 0 });

    await expect(shim.connect()).rejects.toThrow(/automatic recovery is in progress/);
    expect(managerInstances[0].connect).not.toHaveBeenCalled();
  });

  it('isConnected() is false before any READY/RESUMED', async () => {
    const { shim } = makeShim();
    expect(shim.isConnected()).toBe(false);
    await shim.start({ connect: false });
    expect(shim.isConnected()).toBe(false);
  });

  it('isConnected() flips true on shard Ready event', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    managerInstances[0].emit(WebSocketShardEvents.Ready, {
      data: { application: { id: 'app-1' } },
      shardId: 0,
    });
    expect(shim.isConnected()).toBe(true);
  });

  it('isConnected() flips true on shard Resumed event (Pillar 2 happy path)', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    managerInstances[0].emit(WebSocketShardEvents.Resumed, 0);
    expect(shim.isConnected()).toBe(true);
  });

  it('isConnected() flips back to false on Closed', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    managerInstances[0].emit(WebSocketShardEvents.Ready, {
      data: { application: { id: 'app-1' } },
      shardId: 0,
    });
    expect(shim.isConnected()).toBe(true);
    managerInstances[0].emit(WebSocketShardEvents.Closed, { code: 1006, shardId: 0 });
    expect(shim.isConnected()).toBe(false);
    expect(shim.isRecovering()).toBe(true);
  });

  it('isRecovering() stays true from Closed until Ready/Resumed', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    expect(shim.isRecovering()).toBe(false);

    managerInstances[0].emit(WebSocketShardEvents.Closed, { code: 4200, shardId: 0 });
    expect(shim.isRecovering()).toBe(true);

    managerInstances[0].emit(WebSocketShardEvents.Resumed, 0);
    expect(shim.isRecovering()).toBe(false);
  });

  it.each([
    GatewayCloseCodes.AuthenticationFailed,
    GatewayCloseCodes.InvalidShard,
    GatewayCloseCodes.ShardingRequired,
    GatewayCloseCodes.InvalidAPIVersion,
    GatewayCloseCodes.InvalidIntents,
    GatewayCloseCodes.DisallowedIntents,
  ])('does not claim automatic recovery for terminal close code %i', async (code) => {
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    const rawManager = managerInstances[0];

    rawManager.emit(WebSocketShardEvents.Closed, { code, shardId: 0 });

    expect(shim.isConnected()).toBe(false);
    expect(shim.isRecovering()).toBe(false);
    await shim.connect();
    expect(rawManager.connect).toHaveBeenCalledTimes(1);
  });

  it('isConnected() is false after stop() regardless of prior Ready', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    managerInstances[0].emit(WebSocketShardEvents.Ready, {
      data: { application: { id: 'app-1' } },
      shardId: 0,
    });
    await shim.stop({ flushFinal: false });
    expect(shim.isConnected()).toBe(false);
    expect(shim.isRecovering()).toBe(false);
  });

  it('connect() rejects after stop()', async () => {
    const { shim } = makeShim();
    await shim.start({ connect: false });
    await shim.stop({ flushFinal: false });
    await expect(shim.connect()).rejects.toThrow(/after stop\(\) or a failed start\(\)/);
  });

  async function makeFailedStartShim() {
    const { SlowFakeManager, instances } = makeSlowManagerCtor();
    const { shim } = makeShim({ WebSocketManagerCtor: SlowFakeManager });
    await expect(shim.start({ timeoutMs: 5 })).rejects.toThrow(/timed out/);
    return { shim, instances };
  }

  it('connect() rejects after a failed start() with the same terminal-state error', async () => {
    const { shim } = await makeFailedStartShim();
    await expect(shim.connect()).rejects.toThrow(/after stop\(\) or a failed start\(\)/);
  });

  it('concurrent shim.connect() calls both delegate to manager.connect()', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    await Promise.all([shim.connect(), shim.connect()]);
    expect(managerInstances[0].connect).toHaveBeenCalledTimes(2);
  });

  it('connect() propagates the underlying manager rejection', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    managerInstances[0].connect.mockRejectedValueOnce(new Error('discord 5xx'));
    await expect(shim.connect()).rejects.toThrow('discord 5xx');
  });

  it('stop() removes every shim-installed shard listener (no leak across cycles)', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    expect(managerInstances[0].listenerCount(WebSocketShardEvents.Closed)).toBe(1);
    expect(managerInstances[0].listenerCount(WebSocketShardEvents.Ready)).toBe(1);
    expect(managerInstances[0].listenerCount(WebSocketShardEvents.Resumed)).toBe(1);
    await shim.stop({ flushFinal: false });
    expect(managerInstances[0].listenerCount(WebSocketShardEvents.Closed)).toBe(0);
    expect(managerInstances[0].listenerCount(WebSocketShardEvents.Ready)).toBe(0);
    expect(managerInstances[0].listenerCount(WebSocketShardEvents.Resumed)).toBe(0);
  });

  it('shard event listeners no-op after a failed start() (stopped guard)', async () => {
    const { shim, instances } = await makeFailedStartShim();
    expect(shim.isConnected()).toBe(false);
    instances[0].emit(WebSocketShardEvents.Ready, { data: {}, shardId: 0 });
    expect(shim.isConnected()).toBe(false);
    instances[0].emit(WebSocketShardEvents.Resumed, 0);
    expect(shim.isConnected()).toBe(false);
    instances[0].emit(WebSocketShardEvents.Closed, { code: 1006, shardId: 0 });
    expect(shim.isConnected()).toBe(false);
  });

  it('isStarted() reflects construction state (false → true → false)', async () => {
    const { shim } = makeShim();
    expect(shim.isStarted()).toBe(false);
    await shim.start({ connect: false });
    expect(shim.isStarted()).toBe(true);
    await shim.stop({ flushFinal: false });
    expect(shim.isStarted()).toBe(false);
  });

  it('satisfies the leader/watchdog factory contracts (no TypeError on construction)', () => {
    const { shim } = makeShim();
    const { createGatewayLeader } = require('../src/gateway-leader');
    const { createConnectionWatchdog } = require('../src/gateway-connection-watchdog');

    const minimalDeps = {
      lock: {
        acquireLock: async () => ({}),
        renewLock: async () => ({}),
        transferLock: async () => ({}),
        adoptLockFromHandoff: () => {},
        releaseLock: async () => {},
      },
      peerHeartbeat: {
        writeHeartbeat: async () => {},
        listFreshPeers: async () => [],
        deleteOwnRow: async () => {},
      },
      controlClient: { pushHandoff: async () => ({ ok: true }) },
      selfInstanceId: 'i-test',
      shardId: '0:1',
      logger: makeFakeLogger(),
    };

    expect(() => createGatewayLeader({ ...minimalDeps, manager: shim })).not.toThrow();
    expect(() => createConnectionWatchdog({
      manager: shim,
      isHoldingLock: () => false,
      isConnecting: () => false,
      readCurrentHolder: async () => null,
      selfInstanceId: 'i-test',
      releaseLock: async () => {},
      deleteOwnRow: async () => {},
      logger: minimalDeps.logger,
    })).not.toThrow();
  });

  it('lets the upstream reconnect finish without a watchdog connect race', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    const rawManager = managerInstances[0];
    rawManager.emit(WebSocketShardEvents.Ready, {
      data: { application: { id: 'app-1' } },
      shardId: 0,
    });

    const { createConnectionWatchdog } = require('../src/gateway-connection-watchdog');
    const watchdog = createConnectionWatchdog({
      manager: shim,
      isHoldingLock: () => true,
      isConnecting: () => false,
      readCurrentHolder: async () => null,
      selfInstanceId: 'i-test',
      releaseLock: async () => {},
      logger: makeFakeLogger(),
    });

    rawManager.emit(WebSocketShardEvents.Closed, { code: 4200, shardId: 0 });
    await watchdog._stepForTest();
    expect(rawManager.connect).not.toHaveBeenCalled();
    expect(watchdog._getAttemptsForTest()).toBe(0);

    rawManager.emit(WebSocketShardEvents.Resumed, 0);
    await watchdog._stepForTest();
    expect(rawManager.connect).not.toHaveBeenCalled();
    expect(watchdog._getAttemptsForTest()).toBe(0);
  });

  it('bounds a real shim recovery even while the replica is a standby', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    const rawManager = managerInstances[0];
    const releaseLock = jest.fn(async () => {});
    const deleteOwnRow = jest.fn(async () => {});
    const exit = jest.fn();
    let nowMs = 0;
    const { createConnectionWatchdog } = require('../src/gateway-connection-watchdog');
    const watchdog = createConnectionWatchdog({
      manager: shim,
      isHoldingLock: () => false,
      isConnecting: () => false,
      readCurrentHolder: async () => null,
      selfInstanceId: 'i-test',
      releaseLock,
      deleteOwnRow,
      logger: makeFakeLogger(),
      maxRecoveryMs: 1_000,
      now: () => nowMs,
      exit,
    });

    rawManager.emit(WebSocketShardEvents.Closed, { code: 4200, shardId: 0 });
    await watchdog._stepForTest();
    nowMs = 1_000;
    await watchdog._stepForTest();

    expect(releaseLock).not.toHaveBeenCalled();
    expect(deleteOwnRow).toHaveBeenCalledTimes(1);
    expect(exit).toHaveBeenCalledWith(1);
  });

  it('lets a leader adopt a handoff without racing a real recovering shim', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    const rawManager = managerInstances[0];
    rawManager.emit(WebSocketShardEvents.Closed, { code: 4200, shardId: 0 });

    const { createGatewayLeader } = require('../src/gateway-leader');
    const lock = {
      acquireLock: jest.fn(async () => ({ acquired: false })),
      renewLock: jest.fn(async () => ({ renewed: true })),
      transferLock: jest.fn(async () => ({ transferred: false })),
      adoptLockFromHandoff: jest.fn(),
      releaseLock: jest.fn(async () => ({ released: true })),
    };
    const leader = createGatewayLeader({
      lock,
      peerHeartbeat: {
        writeHeartbeat: jest.fn(async () => {}),
        listFreshPeers: jest.fn(async () => []),
      },
      controlClient: { pushHandoff: jest.fn(async () => ({ ok: true })) },
      manager: shim,
      selfInstanceId: 'inst-B',
      shardId: '0:1',
      logger: makeFakeLogger(),
    });

    await leader.handleInboundHandoff({
      activeInstanceId: 'inst-A', expectedVersion: 8,
    });

    expect(lock.adoptLockFromHandoff).toHaveBeenCalledWith(8);
    expect(leader.isHoldingLock()).toBe(true);
    expect(rawManager.connect).not.toHaveBeenCalled();
  });
});

describe('IDENTIFY budget guard', () => {
  it('passes through when mirror is non-null (RESUME path; no budget impact)', async () => {
    const { shim, store, managerInstances } = makeShim();
    store._setMirror({ sessionId: 'sess-A', resumeURL: 'wss://r/a', sequence: 1 });

    await shim.start();
    const { retrieveSessionInfo } = managerInstances[0]._constructorArgs;

    for (let i = 0; i < 100; i++) {
      expect(retrieveSessionInfo('0:1')).not.toBeNull();
    }
    expect(shim._getIdentifyAttemptsForTest()).toBe(0);
  });

  it('throws on second null-mirror call (cap = 1)', async () => {
    const { shim, managerInstances } = makeShim();

    await shim.start();
    const { retrieveSessionInfo } = managerInstances[0]._constructorArgs;

    expect(retrieveSessionInfo('0:1')).toBeNull();
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);

    let thrown;
    try {
      retrieveSessionInfo('0:1');
    } catch (err) {
      thrown = err;
    }
    expect(thrown).toBeDefined();
    expect(thrown.code).toBe('GATEWAY_IDENTIFY_BUDGET');
    expect(thrown.message).toMatch(/cap 1/);
  });

  it('exposes MAX_IDENTIFY_ATTEMPTS = 1 as a pinned constant', () => {
    expect(MAX_IDENTIFY_ATTEMPTS).toBe(1);
  });

  it('resets the counter on READY so a later resume-rejection still gets an IDENTIFY', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    const { retrieveSessionInfo } = mgr._constructorArgs;

    expect(retrieveSessionInfo('0:1')).toBeNull();
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'READY', d: { application: { id: 'app-1' } } },
      shardId: 0,
    });
    expect(shim._getIdentifyAttemptsForTest()).toBe(0);

    expect(retrieveSessionInfo('0:1')).toBeNull();
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);

    expect(() => retrieveSessionInfo('0:1')).toThrow(/IDENTIFY budget exhausted/);
  });
});

describe('READY detection', () => {
  it('flips isReady true and captures appId on READY dispatch', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];

    expect(shim.isReady()).toBe(false);
    expect(shim.getAppId()).toBeNull();

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'READY', d: { application: { id: '123456789012345678' } } },
      shardId: 0,
    });

    expect(shim.isReady()).toBe(true);
    expect(shim.getAppId()).toBe('123456789012345678');
  });

  it('handles READY without an application id (logs but stays ready)', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'READY', d: {} },
      shardId: 0,
    });

    expect(shim.isReady()).toBe(true);
    expect(shim.getAppId()).toBeNull();
  });

  it('non-READY / non-RESUMED dispatches do not flip isReady', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'GUILD_CREATE', d: { id: 'guild-1' } },
      shardId: 0,
    });

    expect(shim.isReady()).toBe(false);
  });

  it('flips isReady true on RESUMED — the cross-process resume happy path', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];

    expect(shim.isReady()).toBe(false);

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'RESUMED', d: {} },
      shardId: 0,
    });

    expect(shim.isReady()).toBe(true);
    expect(shim.getAppId()).toBeNull();
  });

  it('RESUMED resets the IDENTIFY budget so a later disconnect-reconnect cycle gets a fresh allowance', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    const { retrieveSessionInfo } = mgr._constructorArgs;

    retrieveSessionInfo('0:1'); // mirror is null → count=1
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'RESUMED', d: {} },
      shardId: 0,
    });

    expect(shim._getIdentifyAttemptsForTest()).toBe(0);
  });
});

describe('onDispatch fan-out', () => {
  it('fires every registered handler on each dispatch', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];

    const h1 = jest.fn();
    const h2 = jest.fn();
    shim.onDispatch(h1);
    shim.onDispatch(h2);

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'INTERACTION_CREATE', d: {} },
      shardId: 0,
    });

    expect(h1).toHaveBeenCalledTimes(1);
    expect(h2).toHaveBeenCalledTimes(1);
    expect(h1).toHaveBeenCalledWith({
      data: { t: 'INTERACTION_CREATE', d: {} },
      shardId: 0,
    });
  });

  it('a throwing handler does not break sibling handlers', async () => {
    const { shim, managerInstances, logger } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];

    const thrower = jest.fn(() => { throw new Error('boom'); });
    const good = jest.fn();
    shim.onDispatch(thrower);
    shim.onDispatch(good);

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'HEARTBEAT_ACK' },
      shardId: 0,
    });

    expect(thrower).toHaveBeenCalledTimes(1);
    expect(good).toHaveBeenCalledTimes(1);
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringMatching(/dispatch handler threw/i),
      expect.objectContaining({ error: 'boom' }),
    );
  });

  it('unsubscribe stops future deliveries to that handler', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];

    const h = jest.fn();
    const unsubscribe = shim.onDispatch(h);

    mgr.emit(WebSocketShardEvents.Dispatch, { data: { t: 'X' }, shardId: 0 });
    expect(h).toHaveBeenCalledTimes(1);

    unsubscribe();
    mgr.emit(WebSocketShardEvents.Dispatch, { data: { t: 'X' }, shardId: 0 });
    expect(h).toHaveBeenCalledTimes(1); // still 1
  });

  it('throws when handler is not a function', () => {
    const { shim } = makeShim();
    expect(() => shim.onDispatch(null)).toThrow(/must be a function/);
    expect(() => shim.onDispatch('hi')).toThrow(/must be a function/);
  });
});

describe('SIGTERM contract — stop() does NOT call manager.destroy()', () => {
  it('flushes store but never invokes manager.destroy()', async () => {
    const { shim, store, managerInstances } = makeShim();
    await shim.start();

    await shim.stop();

    expect(managerInstances[0].destroy).not.toHaveBeenCalled();
    expect(store.flushFinal).toHaveBeenCalledTimes(1);
  });

  it('stop({ flushFinal: false }) routes through store.stop()', async () => {
    const { shim, store, managerInstances } = makeShim();
    await shim.start();

    await shim.stop({ flushFinal: false });

    expect(store.flushFinal).not.toHaveBeenCalled();
    expect(store.stop).toHaveBeenCalledTimes(1);
    expect(managerInstances[0].destroy).not.toHaveBeenCalled();
  });

  it('clears dispatch handlers so late dispatches are dropped', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];

    const h = jest.fn();
    shim.onDispatch(h);
    await shim.stop();

    mgr.emit(WebSocketShardEvents.Dispatch, { data: { t: 'X' }, shardId: 0 });
    expect(h).not.toHaveBeenCalled();
  });

  it('removes only the listeners the shim installed (does not strip foreign listeners)', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    const foreign = jest.fn();
    mgr.on('SomeOtherEvent', foreign);
    expect(mgr.listenerCount(WebSocketShardEvents.Dispatch)).toBeGreaterThan(0);
    expect(mgr.listenerCount('SomeOtherEvent')).toBe(1);

    await shim.stop();
    expect(mgr.listenerCount(WebSocketShardEvents.Dispatch)).toBe(0);
    expect(mgr.listenerCount(WebSocketShardEvents.Error)).toBe(0);
    expect(mgr.listenerCount('SomeOtherEvent')).toBe(1); // unaffected
  });

  it('stop() is idempotent — second call is a no-op (does not double-flush)', async () => {
    const { shim, store } = makeShim();
    await shim.start();

    await shim.stop();
    await shim.stop();

    expect(store.flushFinal).toHaveBeenCalledTimes(1);
  });

  it('drops dispatches that arrive during stop() teardown (between flag-flip and listener detach)', async () => {
    const { shim, store, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];

    const h = jest.fn();
    shim.onDispatch(h);

    let resolveFlush;
    store.flushFinal.mockImplementation(() => new Promise((r) => { resolveFlush = r; }));

    const stopPromise = shim.stop();
    await Promise.resolve(); // let stop() run up to the await

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'INTERACTION_CREATE', d: {} },
      shardId: 0,
    });
    expect(h).not.toHaveBeenCalled();

    resolveFlush();
    await stopPromise;
  });

  it('stop() after a failed start still runs cleanup (listener detach + flushFinal)', async () => {
    const { SlowFakeManager, instances: lateInstances } = makeSlowManagerCtor();
    const { shim, store } = makeShim({ WebSocketManagerCtor: SlowFakeManager });
    await expect(shim.start({ timeoutMs: 10 })).rejects.toThrow(/timed out/);
    const mgr = lateInstances[0];
    expect(mgr.listenerCount(WebSocketShardEvents.Dispatch)).toBeGreaterThan(0);

    await shim.stop();

    expect(store.flushFinal).toHaveBeenCalledTimes(1);
    expect(mgr.listenerCount(WebSocketShardEvents.Dispatch)).toBe(0);
    expect(mgr.listenerCount(WebSocketShardEvents.Error)).toBe(0);
  });
});

describe('exposed REST instance', () => {
  it('reuses an injected REST when provided', async () => {
    const injectedRest = { setToken: jest.fn().mockReturnThis(), token: 'pre-bound' };
    const { shim, restInstances } = makeShim({ rest: injectedRest });

    await shim.start();
    expect(shim.getRest()).toBe(injectedRest);
    expect(restInstances).toHaveLength(0);
  });

  it('lazy-constructs and binds token when REST is not injected', async () => {
    const { shim, restInstances } = makeShim();
    await shim.start();

    expect(restInstances).toHaveLength(1);
    expect(shim.getRest()).toBe(restInstances[0]);
    expect(restInstances[0].token).toBe('test-token');
  });
});

describe('constants are pinned', () => {
  it('DEFAULT_CONNECT_TIMEOUT_MS = 30_000', () => {
    expect(DEFAULT_CONNECT_TIMEOUT_MS).toBe(30_000);
  });

  it('VERIFIED_DJS_WS_MAJOR_MINOR matches the installed @discordjs/ws major.minor', () => {
    const path = require('node:path');
    const fs = require('node:fs');
    const wsEntry = require.resolve('@discordjs/ws');
    const marker = `${path.sep}@discordjs${path.sep}ws${path.sep}`;
    const markerIdx = wsEntry.indexOf(marker);
    if (markerIdx < 0) {
      throw new Error(
        `Could not locate @discordjs/ws install from require.resolve('${wsEntry}'). ` +
        'Update the path-extraction below to match the new install layout.',
      );
    }
    const wsRoot = wsEntry.slice(0, markerIdx) + marker.slice(0, -1);
    const djsWsVersion = JSON.parse(fs.readFileSync(path.join(wsRoot, 'package.json'), 'utf8')).version;
    const installedMajorMinor = djsWsVersion.split('.').slice(0, 2).join('.');
    expect(installedMajorMinor).toBe(VERIFIED_DJS_WS_MAJOR_MINOR);
  });
});
