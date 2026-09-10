// Unit tests for src/gateway-ws-shim.js — the @discordjs/ws shim
// that replaces discord.js Client in the Pillar 2 gateway tier.
//
// Coverage focuses on the load-bearing contracts called out in the
// module header:
//
//   1. SIGTERM contract: stop() does NOT call manager.destroy().
//      Discord's 60 s resume buffer relies on a TCP drop, not a
//      clean close frame. A regression here breaks cross-process
//      RESUME.
//   2. IDENTIFY budget guard: MAX_IDENTIFY_ATTEMPTS = 1 enforced
//      at the actual identify-throttler boundary. A
//      future bump to 2+ would change the Discord-quota burn
//      profile — pin so it requires explicit test update.
//   3. READY detection: appId plucked from data.d.application.id,
//      isReady flips true after first READY dispatch.
//   4. Dispatch fan-out: multiple onDispatch handlers all fire;
//      a throwing handler doesn't break the others.

const { EventEmitter } = require('node:events');
const {
  createGatewayWsShim,
  MAX_IDENTIFY_ATTEMPTS,
  DEFAULT_CONNECT_TIMEOUT_MS,
  VERIFIED_DJS_WS_VERSION,
} = require('../src/gateway-ws-shim');
const {
  SimpleContextFetchingStrategy,
  SimpleIdentifyThrottler,
  WebSocketManager,
  WebSocketShard,
  WebSocketShardEvents,
} = require('@discordjs/ws');
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
    inst.fetchGatewayInformation = jest.fn().mockResolvedValue({
      session_start_limit: { max_concurrency: 1 },
    });
    inst.destroy = jest.fn().mockImplementation((opts) => {
      inst._destroyCalls.push(opts);
      return Promise.resolve();
    });
    instances.push(inst);
    return inst;
  }
  return { FakeManager, instances };
}

function makeFakeIdentifyThrottlerCtor() {
  const instances = [];
  function FakeIdentifyThrottler(maxConcurrency) {
    const inst = {
      maxConcurrency,
      waitForIdentify: jest.fn().mockResolvedValue(undefined),
    };
    instances.push(inst);
    return inst;
  }
  return { FakeIdentifyThrottler, instances };
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
  const onFatal = jest.fn();
  const {
    FakeIdentifyThrottler,
    instances: identifyThrottlerInstances,
  } = makeFakeIdentifyThrottlerCtor();
  const shim = createGatewayWsShim({
    token: 'test-token',
    intents: 1,
    store,
    logger,
    WebSocketManagerCtor: FakeManager,
    RESTCtor: FakeREST,
    IdentifyThrottlerCtor: FakeIdentifyThrottler,
    onFatal,
    ...overrides,
  });
  return {
    shim,
    store,
    logger,
    onFatal,
    managerInstances,
    restInstances,
    identifyThrottlerInstances,
  };
}

describe('createGatewayWsShim — factory validation', () => {
  it('throws when required args are missing', () => {
    expect(() => createGatewayWsShim()).toThrow(/token is required/);
    expect(() => createGatewayWsShim({ token: 't' })).toThrow(/intents/);
    expect(() => createGatewayWsShim({ token: 't', intents: 0 })).toThrow(/store is required/);
    expect(() => createGatewayWsShim({ token: 't', intents: 0, store: {} })).toThrow(/logger is required/);
    expect(() => createGatewayWsShim({
      token: 't', intents: 0, store: {}, logger: {},
    })).toThrow(/onFatal/);
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
    // The process-global IDENTIFY cap is valid only for one shard. Pin the
    // manager so a future Discord-recommended shard-count increase cannot
    // silently turn shard 1's first IDENTIFY into a fatal attempt 2.
    expect(args.shardCount).toBe(1);
    // REST was lazy-constructed since `rest` wasn't injected.
    expect(restInstances).toHaveLength(1);
    expect(restInstances[0].setToken).toHaveBeenCalledWith('test-token');
    expect(args.rest).toBe(restInstances[0]);
    expect(typeof args.retrieveSessionInfo).toBe('function');
    expect(typeof args.updateSessionInfo).toBe('function');
    expect(typeof args.buildIdentifyThrottler).toBe('function');
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
    // @discordjs/ws v1.2.3 emits Resumed with `(shardId: number)` —
    // a bare number, not an object. Match upstream shape so the
    // fixture documents the real contract.
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
    // @discordjs/ws v1.2.3 Closed payload is `{ code, shardId }` —
    // no `reason`. Listener destructures `reason` defensively
    // against a future minor adding it; the fallback logs null.
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
  it('fails health immediately when the budget trips inside an in-flight start connect', async () => {
    let overBudgetController;
    const managerInstances = [];
    function BudgetTripManager(args) {
      const inst = Object.assign(new EventEmitter(), {
        _constructorArgs: args,
        fetchGatewayInformation: jest.fn().mockResolvedValue({
          session_start_limit: { max_concurrency: 1 },
        }),
      });
      inst.connect = jest.fn(async () => {
        const throttler = await args.buildIdentifyThrottler(inst);
        await throttler.waitForIdentify(0, new AbortController().signal);
        overBudgetController = new AbortController();
        return throttler.waitForIdentify(0, overBudgetController.signal);
      });
      managerInstances.push(inst);
      return inst;
    }
    let resolveFatal;
    const fatalObserved = new Promise(resolve => { resolveFatal = resolve; });
    const onFatal = jest.fn(() => { resolveFatal(); });
    const { shim } = makeShim({ WebSocketManagerCtor: BudgetTripManager, onFatal });

    const starting = shim.start({ timeoutMs: 5_000 });
    await fatalObserved;

    expect(managerInstances[0].connect).toHaveBeenCalledTimes(1);
    expect(onFatal).toHaveBeenCalledTimes(1);
    expect(shim.isReady()).toBe(false);
    expect(shim.isConnected()).toBe(false);

    const closed = new Error('closed during fatal shutdown');
    overBudgetController.abort(closed);
    await expect(starting).rejects.toBe(closed);
  });
  it('keeps retrieveSessionInfo a pure pass-through across repeated pre-READY reads', async () => {
    const { shim, store, managerInstances } = makeShim();
    await shim.start();
    const { retrieveSessionInfo } = managerInstances[0]._constructorArgs;

    // @discordjs/ws reads session state during connect, heartbeats,
    // dispatches, and invalid-session handling. Those reads are not
    // IDENTIFY attempts and must never consume the quota guard.
    for (let i = 0; i < 100; i++) {
      expect(retrieveSessionInfo('0:1')).toBeNull();
    }
    expect(shim._getIdentifyAttemptsForTest()).toBe(0);

    store._setMirror({ sessionId: 'sess-A', resumeURL: 'wss://r/a', sequence: 1 });
    expect(retrieveSessionInfo('0:1')).toEqual({
      sessionId: 'sess-A', resumeURL: 'wss://r/a', sequence: 1,
    });
  });

  it('counts actual identify grants and blocks a second grant while shutdown starts', async () => {
    const {
      shim, logger, onFatal, managerInstances, identifyThrottlerInstances,
    } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);
    expect(identifyThrottlerInstances).toHaveLength(1);
    expect(identifyThrottlerInstances[0].maxConcurrency).toBe(1);

    await throttler.waitForIdentify(0, new AbortController().signal);
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'READY', d: { application: { id: 'app-1' } } },
      shardId: 0,
    });
    expect(shim.isReady()).toBe(true);
    await throttler.waitForIdentify(0, new AbortController().signal);

    // Throwing here is unsafe: @discordjs/ws catches throttler
    // failures, reconnects, and then proceeds to send IDENTIFY.
    // The over-budget grant therefore stays pending until shutdown
    // closes the shard and aborts its signal.
    const secondController = new AbortController();
    const blocked = throttler.waitForIdentify(0, secondController.signal);
    await Promise.resolve();
    await Promise.resolve();

    expect(onFatal).toHaveBeenCalledTimes(1);
    const fatalError = onFatal.mock.calls[0][0];
    expect(fatalError.code).toBe('GATEWAY_IDENTIFY_BUDGET');
    expect(fatalError.message).toMatch(/cap 1/);
    expect(logger.error).toHaveBeenCalledWith(
      'gateway-ws-shim: IDENTIFY budget exhausted; shutting down',
      { attempt: 2, cap: MAX_IDENTIFY_ATTEMPTS },
    );
    expect(shim._getIdentifyAttemptsForTest()).toBe(2);
    // A fatal budget trip is a terminal process state. Health must fail
    // immediately so ECS replaces the task even if graceful shutdown stalls.
    expect(shim.isReady()).toBe(false);
    expect(shim.isConnected()).toBe(false);
    await expect(shim.connect()).rejects.toThrow(/IDENTIFY-budget fatal/);

    const abortReason = new Error('shard closed');
    secondController.abort(abortReason);
    await expect(blocked).rejects.toBe(abortReason);
  });

  it('counts and bounds a non-abort delegate failure because upstream would still IDENTIFY', async () => {
    const throttleError = Object.assign(new Error('delegate failed'), { code: 'THROTTLE_DRIFT' });
    function RejectingIdentifyThrottler() {
      return { waitForIdentify: jest.fn().mockRejectedValue(throttleError) };
    }
    const {
      shim, logger, managerInstances, onFatal,
    } = makeShim({
      IdentifyThrottlerCtor: RejectingIdentifyThrottler,
    });
    await shim.start();
    const mgr = managerInstances[0];
    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);

    await expect(throttler.waitForIdentify(0, new AbortController().signal))
      .resolves.toBeUndefined();
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);
    expect(logger.warn).toHaveBeenCalledWith(
      'gateway-ws-shim: identify throttle delegate failed; counting attempted grant',
      { errorName: 'Error', errorCode: 'THROTTLE_DRIFT' },
    );

    const controller = new AbortController();
    const blocked = throttler.waitForIdentify(0, controller.signal);
    await Promise.resolve();
    await Promise.resolve();
    expect(shim._getIdentifyAttemptsForTest()).toBe(2);
    expect(onFatal).toHaveBeenCalledTimes(1);

    controller.abort(throttleError);
    await expect(blocked).rejects.toBe(throttleError);
  });

  it('does not burn an attempt when the shard aborts as the delegate releases', async () => {
    let releaseDelegate;
    function DeferredIdentifyThrottler() {
      return {
        waitForIdentify: jest.fn(() => new Promise((resolve) => {
          releaseDelegate = resolve;
        })),
      };
    }
    const { shim, managerInstances, onFatal } = makeShim({
      IdentifyThrottlerCtor: DeferredIdentifyThrottler,
    });
    await shim.start();
    const mgr = managerInstances[0];
    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);
    const controller = new AbortController();
    const aborted = throttler.waitForIdentify(0, controller.signal);
    const reason = new Error('shard closed at throttle boundary');

    controller.abort(reason);
    releaseDelegate();

    await expect(aborted).rejects.toBe(reason);
    expect(shim._getIdentifyAttemptsForTest()).toBe(0);
    expect(onFatal).not.toHaveBeenCalled();
  });

  it('installs the budget guard when gateway information cannot be fetched', async () => {
    const {
      shim, logger, managerInstances, identifyThrottlerInstances, onFatal,
    } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    mgr.fetchGatewayInformation.mockRejectedValueOnce(Object.assign(
      new Error('gateway unavailable'),
      { code: 'HTTP_503', status: 503 },
    ));

    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);

    expect(identifyThrottlerInstances.at(-1).maxConcurrency).toBe(1);
    expect(logger.warn).toHaveBeenCalledWith(
      'gateway-ws-shim: gateway info fetch failed; defaulting max_concurrency to 1',
      { errorName: 'Error', errorCode: 'HTTP_503', status: 503 },
    );
    await throttler.waitForIdentify(0, new AbortController().signal);
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);
    expect(onFatal).not.toHaveBeenCalled();
  });

  it('keeps the budget guard when the identify throttler constructor throws', async () => {
    function ThrowingIdentifyThrottler() {
      throw Object.assign(new Error('constructor changed'), { code: 'BAD_EXPORT' });
    }
    const {
      shim, logger, managerInstances, onFatal,
    } = makeShim({ IdentifyThrottlerCtor: ThrowingIdentifyThrottler });
    await shim.start();
    const mgr = managerInstances[0];

    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);

    expect(logger.error).toHaveBeenCalledWith(
      'gateway-ws-shim: identify throttler construction failed; using budget-only fallback',
      { errorName: 'Error', errorCode: 'BAD_EXPORT' },
    );
    await throttler.waitForIdentify(0, new AbortController().signal);
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);

    const controller = new AbortController();
    const blocked = throttler.waitForIdentify(0, controller.signal);
    await Promise.resolve();
    await Promise.resolve();
    expect(onFatal).toHaveBeenCalledTimes(1);
    controller.abort(new Error('closed'));
    await expect(blocked).rejects.toThrow('closed');
  });

  it.each([
    [{}, null],
    [{ session_start_limit: { max_concurrency: 0 } }, 0],
    [{ session_start_limit: { max_concurrency: '1' } }, '1'],
  ])('defaults invalid gateway max_concurrency to 1 and warns (%#)', async (info, observed) => {
    const {
      shim, logger, managerInstances, identifyThrottlerInstances,
    } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    mgr.fetchGatewayInformation.mockResolvedValueOnce(info);

    await mgr._constructorArgs.buildIdentifyThrottler(mgr);

    expect(identifyThrottlerInstances.at(-1).maxConcurrency).toBe(1);
    expect(logger.warn).toHaveBeenCalledWith(
      'gateway-ws-shim: gateway info has invalid max_concurrency; defaulting to 1',
      { observedMaxConcurrency: observed },
    );
  });

  it('reports a Discord shard recommendation above the process-global single-shard ceiling', async () => {
    const { shim, logger, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    mgr.fetchGatewayInformation.mockResolvedValue({
      shards: 2,
      session_start_limit: { max_concurrency: 1 },
    });
    logger.error.mockImplementation((message) => {
      if (message === 'gateway-ws-shim: Discord recommends more than one shard') {
        throw new Error('logger-failure');
      }
    });

    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);
    await expect(throttler.waitForIdentify(0, new AbortController().signal))
      .resolves.toBeUndefined();
    expect(logger.error).toHaveBeenCalledWith(
      'gateway-ws-shim: Discord recommends more than one shard',
      { recommendedShards: 2, configuredShards: 1 },
    );
  });

  it('retains the guard when gateway-fetch fallback logging throws', async () => {
    const { shim, logger, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    mgr.fetchGatewayInformation.mockRejectedValueOnce(new Error('gateway-down'));
    logger.warn.mockImplementation(() => { throw new Error('logger-failure'); });

    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);
    await expect(throttler.waitForIdentify(0, new AbortController().signal))
      .resolves.toBeUndefined();
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);
  });

  it('retains the guard when invalid-gateway-info logging throws', async () => {
    const { shim, logger, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    mgr.fetchGatewayInformation.mockResolvedValueOnce({
      session_start_limit: { max_concurrency: 0 },
    });
    logger.warn.mockImplementation(() => { throw new Error('logger-failure'); });

    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);
    await expect(throttler.waitForIdentify(0, new AbortController().signal))
      .resolves.toBeUndefined();
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);
  });

  it('retains the fallback guard when constructor-failure logging throws', async () => {
    function ThrowingIdentifyThrottler() {
      throw new Error('constructor-failure');
    }
    const {
      shim, logger, managerInstances,
    } = makeShim({ IdentifyThrottlerCtor: ThrowingIdentifyThrottler });
    await shim.start();
    const mgr = managerInstances[0];
    logger.error.mockImplementation(() => { throw new Error('logger-failure'); });

    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);
    await expect(throttler.waitForIdentify(0, new AbortController().signal))
      .resolves.toBeUndefined();
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);
  });

  it('grants an in-budget IDENTIFY when pending logging throws', async () => {
    const { shim, logger, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    logger.info.mockImplementation(() => { throw new Error('logger-failure'); });
    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);

    await expect(throttler.waitForIdentify(0, new AbortController().signal))
      .resolves.toBeUndefined();
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);
  });

  it('still starts fatal shutdown and blocks the grant when fatal logging throws', async () => {
    const { shim, logger, managerInstances, onFatal } = makeShim();
    logger.error.mockImplementation(() => { throw new Error('logger-failure'); });
    await shim.start();
    const mgr = managerInstances[0];
    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);
    await throttler.waitForIdentify(0, new AbortController().signal);
    const controller = new AbortController();

    const blocked = throttler.waitForIdentify(0, controller.signal);
    await Promise.resolve();
    await Promise.resolve();

    expect(onFatal).toHaveBeenCalledTimes(1);
    expect(shim.isReady()).toBe(false);
    controller.abort(new Error('closed'));
    await expect(blocked).rejects.toThrow('closed');
  });

  it.each([
    ['throws synchronously', () => { throw new Error('sync shutdown failure'); }, 'threw'],
    ['rejects asynchronously', async () => { throw new Error('async shutdown failure'); }, 'rejected'],
  ])('keeps the process unhealthy when onFatal %s', async (_label, onFatal, logSuffix) => {
    const { shim, logger, managerInstances } = makeShim({ onFatal });
    await shim.start();
    const mgr = managerInstances[0];
    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);
    await throttler.waitForIdentify(0, new AbortController().signal);
    const controller = new AbortController();
    const blocked = throttler.waitForIdentify(0, controller.signal);
    await Promise.resolve();
    await Promise.resolve();

    expect(shim.isReady()).toBe(false);
    expect(logger.error).toHaveBeenCalledWith(
      `gateway-ws-shim: fatal shutdown handler ${logSuffix}`,
      { error: expect.stringContaining('shutdown failure') },
    );
    controller.abort(new Error('closed'));
    await expect(blocked).rejects.toThrow('closed');
  });

  it('does not burn budget when a grant receives an already-aborted signal', async () => {
    const { shim, managerInstances, onFatal } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);
    await throttler.waitForIdentify(0, new AbortController().signal);
    const controller = new AbortController();
    const reason = new Error('already closed');
    controller.abort(reason);

    await expect(throttler.waitForIdentify(0, controller.signal)).rejects.toBe(reason);
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);
    expect(onFatal).not.toHaveBeenCalled();
  });

  it('uses an AbortError fallback when a malformed aborted signal omits its reason', async () => {
    const { shim, managerInstances, onFatal } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);

    await expect(throttler.waitForIdentify(0, {
      aborted: true,
      reason: undefined,
    })).rejects.toMatchObject({
      name: 'AbortError',
      message: 'gateway-ws-shim: shard aborted during identify throttle',
    });
    expect(shim._getIdentifyAttemptsForTest()).toBe(0);
    expect(onFatal).not.toHaveBeenCalled();
  });

  it('stop still flushes and detaches listeners after the budget trips', async () => {
    const {
      shim, store, logger, managerInstances, onFatal,
    } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    const handler = jest.fn();
    shim.onDispatch(handler);
    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);
    await throttler.waitForIdentify(0, new AbortController().signal);

    const controller = new AbortController();
    const blocked = throttler.waitForIdentify(0, controller.signal);
    await Promise.resolve();
    await Promise.resolve();
    expect(onFatal).toHaveBeenCalledTimes(1);

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'READY', d: { application: { id: 'late-app' } } },
      shardId: 0,
    });
    expect(handler).not.toHaveBeenCalled();
    expect(shim.isReady()).toBe(false);
    expect(shim._getIdentifyAttemptsForTest()).toBe(2);
    mgr.emit(WebSocketShardEvents.Closed, { code: 1000, reason: 'shutdown', shardId: 0 });
    expect(logger.info).toHaveBeenCalledWith(
      'gateway-ws-shim: shard closed during terminal teardown',
      { shardId: 0, code: 1000, reason: 'shutdown' },
    );

    await shim.stop();
    expect(store.flushFinal).toHaveBeenCalledTimes(1);
    expect(mgr.listenerCount(WebSocketShardEvents.Dispatch)).toBe(0);
    expect(mgr.listenerCount(WebSocketShardEvents.Error)).toBe(0);
    controller.abort(new Error('closed'));
    await expect(blocked).rejects.toThrow('closed');
  });

  it('contains logger failure when a shard closes during terminal teardown', async () => {
    const {
      shim, logger, managerInstances,
    } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);
    await throttler.waitForIdentify(0, new AbortController().signal);

    const controller = new AbortController();
    const blocked = throttler.waitForIdentify(0, controller.signal);
    await Promise.resolve();
    await Promise.resolve();
    logger.info.mockImplementation((message) => {
      if (message === 'gateway-ws-shim: shard closed during terminal teardown') {
        throw new Error('logger-failure');
      }
    });

    expect(() => mgr.emit(WebSocketShardEvents.Closed, {
      code: 1000, reason: 'shutdown', shardId: 0,
    })).not.toThrow();

    controller.abort(new Error('closed'));
    await expect(blocked).rejects.toThrow('closed');
    await shim.stop();
  });

  it('keeps an over-budget grant pending and logs once if upstream omits the abort signal', async () => {
    const { shim, logger, managerInstances, onFatal } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);
    await throttler.waitForIdentify(0, new AbortController().signal);

    const blocked = throttler.waitForIdentify(0);
    await Promise.resolve();
    await Promise.resolve();

    expect(onFatal).toHaveBeenCalledTimes(1);
    expect(await Promise.race([blocked.then(() => 'released'), Promise.resolve('pending')]))
      .toBe('pending');
    expect(logger.error).toHaveBeenCalledWith(
      'gateway-ws-shim: over-budget grant omitted shard abort signal; blocking forever',
    );

    const alsoBlocked = throttler.waitForIdentify(1);
    await Promise.resolve();
    expect(await Promise.race([alsoBlocked.then(() => 'released'), Promise.resolve('pending')]))
      .toBe('pending');
    expect(logger.error.mock.calls.filter(([message]) => (
      message === 'gateway-ws-shim: over-budget grant omitted shard abort signal; blocking forever'
    ))).toHaveLength(1);
  });

  it.each([
    ['has no listener API', { aborted: false }, 'TypeError'],
    ['throws while registering', {
      aborted: false,
      addEventListener: () => { throw new Error('listener-registration-failure'); },
    }, 'Error'],
  ])('keeps an over-budget grant pending if its abort signal %s', async (
    _label,
    unusableSignal,
    errorName,
  ) => {
    const { shim, logger, managerInstances, onFatal } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);
    await throttler.waitForIdentify(0, new AbortController().signal);

    const blocked = throttler.waitForIdentify(0, unusableSignal);
    let blockedState = 'pending';
    blocked.then(
      () => { blockedState = 'fulfilled'; },
      () => { blockedState = 'rejected'; },
    );
    await Promise.resolve();
    await Promise.resolve();

    expect(onFatal).toHaveBeenCalledTimes(1);
    expect(blockedState).toBe('pending');
    expect(logger.error).toHaveBeenCalledWith(
      'gateway-ws-shim: over-budget grant supplied unusable shard abort signal; blocking forever',
      { errorName },
    );
  });

  it('notifies the fatal handler only once across repeated over-budget grants', async () => {
    const { shim, onFatal, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);

    await throttler.waitForIdentify(0, new AbortController().signal);
    const controllers = [new AbortController(), new AbortController()];
    const blocked = controllers.map(controller => (
      throttler.waitForIdentify(0, controller.signal)
    ));
    await Promise.resolve();
    await Promise.resolve();

    expect(onFatal).toHaveBeenCalledTimes(1);
    expect(shim._getIdentifyAttemptsForTest()).toBe(2);
    controllers.forEach(controller => controller.abort(new Error('closed')));
    await Promise.allSettled(blocked);
  });

  it('exposes MAX_IDENTIFY_ATTEMPTS = 1 as a pinned constant', () => {
    expect(MAX_IDENTIFY_ATTEMPTS).toBe(1);
  });

  it('resets the counter on READY so a later resume-rejection still gets an IDENTIFY', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);

    // Cold start: the first actual IDENTIFY grant consumes one.
    await throttler.waitForIdentify(0, new AbortController().signal);
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'READY', d: { application: { id: 'app-1' } } },
      shardId: 0,
    });
    expect(shim._getIdentifyAttemptsForTest()).toBe(0);

    // Later, Discord drops the session past its resume buffer. A
    // new actual IDENTIFY grant is permitted after READY reset.
    await throttler.waitForIdentify(0, new AbortController().signal);
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);
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
    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);

    // Synthesize a non-zero counter as if a prior reconnect
    // attempt had landed.
    await throttler.waitForIdentify(0, new AbortController().signal);
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

  it('VERIFIED_DJS_WS_VERSION matches the exact installed @discordjs/ws version', () => {
    // The wsConnected mirror depends on WebSocketShard.onMessage emitting
    // Ready/Resumed before Dispatch. The identify guard also depends on the
    // v1.2.3 custom-throttler catch falling through to IDENTIFY, so any upgrade
    // must re-read both upstream paths before changing the pinned version.
    // @discordjs/ws's exports field blocks require('.../package.json'),
    // so locate the install via require.resolve (works regardless of
    // whether the dep landed in apps/discord/node_modules or got
    // hoisted to a parent node_modules).
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
    expect(djsWsVersion).toBe(VERIFIED_DJS_WS_VERSION);
    expect(require('../package.json').dependencies['@discordjs/ws']).toBe(VERIFIED_DJS_WS_VERSION);
  });

  it('pins the upstream rejection behavior that requires a blocking budget guard', async () => {
    const strategy = {
      waitForIdentify: jest.fn().mockRejectedValue(new Error('custom throttle rejected')),
      options: {
        token: 'shape-contract-only',
        identifyProperties: {},
        intents: 0,
        shardCount: 1,
        readyTimeout: 1,
      },
    };
    const shard = new WebSocketShard(strategy, 0);
    shard.destroy = jest.fn().mockResolvedValue(undefined);
    shard.send = jest.fn().mockResolvedValue(undefined);
    shard.waitForEvent = jest.fn().mockResolvedValue(undefined);

    expect(typeof shard.identify).toBe('function');
    // These mocks pin upstream control flow only: a rejected throttle reaches
    // destroy and then the op-2 send branch. They do not claim a destroyed
    // real transport could successfully transmit that frame.
    await shard.identify();

    expect(shard.destroy).toHaveBeenCalledWith(expect.objectContaining({
      reason: 'Identify throttling logic failed',
    }));
    expect(shard.send).toHaveBeenCalledWith(expect.objectContaining({ op: 2 }));
    expect(shard.destroy.mock.invocationCallOrder[0])
      .toBeLessThan(shard.send.mock.invocationCallOrder[0]);
  });

  it('pins that an aborted throttle wait suppresses the upstream IDENTIFY send', async () => {
    let shard;
    const strategy = {
      waitForIdentify: jest.fn(async (_shardId, signal) => {
        shard.emit(WebSocketShardEvents.Closed, { code: 1000, shardId: 0 });
        throw signal.reason;
      }),
      options: {
        token: 'shape-contract-only',
        identifyProperties: {},
        intents: 0,
        shardCount: 1,
        readyTimeout: 1,
      },
    };
    shard = new WebSocketShard(strategy, 0);
    shard.destroy = jest.fn().mockResolvedValue(undefined);
    shard.send = jest.fn().mockResolvedValue(undefined);
    shard.waitForEvent = jest.fn().mockResolvedValue(undefined);

    await shard.identify();

    expect(strategy.waitForIdentify).toHaveBeenCalledWith(0, expect.any(AbortSignal));
    expect(shard.destroy).not.toHaveBeenCalled();
    expect(shard.send).not.toHaveBeenCalled();
  });

  it('real @discordjs/ws exposes and honors the configured identify throttler', async () => {
    expect(typeof SimpleIdentifyThrottler).toBe('function');
    const realThrottler = new SimpleIdentifyThrottler(1);
    expect(typeof realThrottler.waitForIdentify).toBe('function');

    const delegate = { waitForIdentify: jest.fn().mockResolvedValue(undefined) };
    const buildIdentifyThrottler = jest.fn().mockResolvedValue(delegate);
    const manager = new WebSocketManager({
      token: 'shape-contract-only',
      intents: 0,
      rest: { get: jest.fn() },
      buildIdentifyThrottler,
      retrieveSessionInfo: jest.fn(),
      updateSessionInfo: jest.fn(),
    });
    expect(manager.options.buildIdentifyThrottler).toBe(buildIdentifyThrottler);

    const strategy = new SimpleContextFetchingStrategy(manager, {});
    const signal = new AbortController().signal;
    await strategy.waitForIdentify(0, signal);
    expect(buildIdentifyThrottler).toHaveBeenCalledWith(manager);
    expect(delegate.waitForIdentify).toHaveBeenCalledWith(0, signal);
  });

  it('real @discordjs/ws RESUMEs a fully hydrated cross-process session', async () => {
    class FakeWebSocket {}
    let FreshWebSocketShard;
    jest.doMock('ws', () => ({ WebSocket: FakeWebSocket }));
    try {
      jest.isolateModules(() => {
        ({ WebSocketShard: FreshWebSocketShard } = require('@discordjs/ws'));
      });
    } finally {
      jest.dontMock('ws');
    }

    const store = makeFakeStore();
    const hydrated = {
      sessionId: 'persisted-session',
      resumeURL: 'wss://resume.discord.test',
      sequence: 42,
      shardId: 0,
      shardCount: 1,
    };
    store._setMirror(hydrated);
    const strategy = {
      retrieveSessionInfo: store.retrieveSessionInfo,
      options: {
        token: 'shape-contract-only',
        version: 10,
        encoding: 'json',
        compression: null,
        shardCount: 1,
        gatewayInformation: { url: 'wss://gateway.discord.test' },
        helloTimeout: 1,
      },
    };
    const shard = new FreshWebSocketShard(strategy, 0);
    shard.waitForEvent = jest.fn().mockResolvedValue({ ok: true });
    shard.resume = jest.fn().mockResolvedValue(undefined);
    shard.identify = jest.fn().mockResolvedValue(undefined);

    await shard.internalConnect();

    expect(shard.resume).toHaveBeenCalledWith(hydrated);
    expect(shard.identify).not.toHaveBeenCalled();
  });

});
