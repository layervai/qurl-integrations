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

// Fake WebSocketManager built on EventEmitter. Captures construction
// args so tests can interrogate the callback wiring and emit fake
// Dispatch / Error events to drive the shim's listeners.
// Factory for a manager whose connect() never resolves — drives
// the Promise.race against the deadline to fire. Function form
// (not arrow) so `new WebSocketManagerCtor(...)` works.
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
    // start() attaches Dispatch/Error listeners BEFORE racing
    // connect() against the timeout. If connect times out but the
    // underlying WS still opens before gracefulShutdown finishes,
    // dispatches arriving during the teardown window shouldn't
    // fire downstream side effects (registerCommands, eventPublisher,
    // gateway-activity ticker). start()'s catch sets stopped=true
    // before throwing, so the in-listener guard drops the frame.
    const { SlowFakeManager, instances: lateInstances } = makeSlowManagerCtor();
    const { shim } = makeShim({ WebSocketManagerCtor: SlowFakeManager });
    const handler = jest.fn();
    shim.onDispatch(handler);

    // Race the connect-timeout. start() rejects AND flips
    // `stopped=true` in its catch before rethrowing.
    await expect(shim.start({ timeoutMs: 10 })).rejects.toThrow(/timed out/);

    // Simulate the racing WS opening mid-teardown: emit a Dispatch
    // on the manager handle the shim attached its listener to.
    // The handler MUST NOT fire — stopped guard drops the frame.
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
    // Both replicas call start({ connect: false }) at boot so the
    // manager is constructed + listeners attached, but only the
    // lock-holder eventually drives connect(). Without this seam,
    // both replicas would IDENTIFY at boot and Discord would flap
    // the session identity every few seconds.
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });

    expect(managerInstances).toHaveLength(1);
    // Manager was constructed (listeners attached) but connect() was
    // NOT called by the shim — the caller drives it later.
    expect(managerInstances[0].connect).not.toHaveBeenCalled();
  });

  it('connect:false still attaches Dispatch listener (fan-out works after a later connect)', async () => {
    // Standby flow: start({connect:false}) at boot, then later the
    // leader drives manager.connect() inside handleInboundHandoff.
    // The first READY/RESUMED that arrives after that connect MUST
    // fan out to onDispatch handlers — otherwise the standby's event
    // pipeline is dead.
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
    // Critical for the hot-standby wiring path: the production
    // boot guard (isStarted()) and other test assertions depend on
    // the manager being constructed by the time start() resolves,
    // regardless of whether connect was driven.
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    expect(shim._getManagerForTest()).toBe(managerInstances[0]);
  });
});

describe('Pillar 3 manager contract — connect() + isConnected()', () => {
  // The leader (gateway-leader.js) and watchdog
  // (gateway-connection-watchdog.js) require a manager handle whose
  // typeof connect === 'function' && typeof isConnected === 'function'.
  // The raw @discordjs/ws WebSocketManager has connect() but NOT
  // isConnected() (only async fetchStatus()) — so the SHIM has to be
  // the contract-conforming handle. These tests pin the surface
  // shape so a future refactor that drops either method fails CI
  // instead of crash-looping the gateway task on next deploy.

  it('exposes connect() and isConnected() on the returned shim', () => {
    const { shim } = makeShim();
    expect(typeof shim.connect).toBe('function');
    expect(typeof shim.isConnected).toBe('function');
  });

  it('connect() throws before start() (no manager yet)', async () => {
    const { shim } = makeShim();
    await expect(shim.connect()).rejects.toThrow(/connect\(\) called before start\(\)/);
  });

  it('connect() delegates to the underlying manager once start() has run', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    await shim.connect();
    // start({connect:false}) skips the internal connect, so the
    // count reflects ONLY the shim.connect() call we just made.
    expect(managerInstances[0].connect).toHaveBeenCalledTimes(1);
  });

  it('isConnected() is false before any READY/RESUMED', async () => {
    const { shim } = makeShim();
    expect(shim.isConnected()).toBe(false);
    await shim.start({ connect: false });
    expect(shim.isConnected()).toBe(false);
  });

  it('isConnected() flips true on shard Ready event', async () => {
    // The shard-level Ready event fires BEFORE the Dispatch
    // fan-out (see @discordjs/ws WebSocketShard.onMessage: it
    // emits "ready" then "dispatch" on a READY frame). Mirroring
    // wsConnected here lets it land before manager.connect()
    // resolves on the Promise.race(once Ready) inside @discordjs/ws.
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
  });

  it('connect() rejects after stop()', async () => {
    const { shim } = makeShim();
    await shim.start({ connect: false });
    await shim.stop({ flushFinal: false });
    await expect(shim.connect()).rejects.toThrow(/after stop\(\) or a failed start\(\)/);
  });

  // Drives a start({connect:true}) into the catch arm (stopped=true,
  // manager still attached) — shared by the connect()-error and
  // listener-guard tests below.
  async function makeFailedStartShim() {
    const { SlowFakeManager, instances } = makeSlowManagerCtor();
    const { shim } = makeShim({ WebSocketManagerCtor: SlowFakeManager });
    await expect(shim.start({ timeoutMs: 5 })).rejects.toThrow(/timed out/);
    return { shim, instances };
  }

  it('connect() rejects after a failed start() with the same terminal-state error', async () => {
    // start({connect:true}) sets stopped=true on its failed-connect
    // catch, but never calls stop(). The connect() error message
    // must cover that state — without lying about which one happened.
    const { shim } = await makeFailedStartShim();
    await expect(shim.connect()).rejects.toThrow(/after stop\(\) or a failed start\(\)/);
  });

  it('concurrent shim.connect() calls both delegate to manager.connect()', async () => {
    // The shim itself doesn't dedupe concurrent connect() calls —
    // the upstream WebSocketManager isn't concurrency-safe and the
    // serialization invariant lives in the leader's `connecting`
    // latch + the watchdog's `isConnecting` observer. Pinning this
    // test surfaces a future shim refactor that accidentally adds
    // a dedup layer (which would break the contract those callers
    // expect).
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    await Promise.all([shim.connect(), shim.connect()]);
    expect(managerInstances[0].connect).toHaveBeenCalledTimes(2);
  });

  it('connect() propagates the underlying manager rejection', async () => {
    // The watchdog wraps shim.connect() in raceWithCeiling and
    // surfaces rejections through its failure ladder — they must
    // pass through verbatim, not be wrapped or swallowed.
    const { shim, managerInstances } = makeShim();
    await shim.start({ connect: false });
    managerInstances[0].connect.mockRejectedValueOnce(new Error('discord 5xx'));
    await expect(shim.connect()).rejects.toThrow('discord 5xx');
  });

  it('stop() removes every shim-installed shard listener (no leak across cycles)', async () => {
    // Regression pin for the listener-leak hazard: a future
    // `start()/stop()/start()` cycle would otherwise accumulate
    // handlers on every new manager instance, each closing over
    // the previous cycle's wsConnected/logger references.
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
    // Listeners stay attached until gracefulShutdown → shim.stop()
    // runs. A late shard event in that window must not mutate
    // wsConnected on a teardown-bound shim.
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
    // Regression guard: the prior wiring passed `shim.getManager()` —
    // the raw @discordjs/ws WebSocketManager — to createGatewayLeader,
    // which throws "manager with connect() and isConnected() is
    // required" because WebSocketManager has no isConnected(). The
    // production fix passes `gatewayShim` itself; this test asserts
    // both factories accept it without throwing.
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
      releaseLock: async () => {},
      deleteOwnRow: async () => {},
      logger: minimalDeps.logger,
    })).not.toThrow();
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
    await expect(shim.connect()).rejects.toThrow(/failed start|stop/);

    const abortReason = new Error('shard closed');
    secondController.abort(abortReason);
    await expect(blocked).rejects.toBe(abortReason);
  });

  it('does not burn an attempt when the delegate throttle rejects', async () => {
    const throttleError = new Error('delegate aborted');
    function RejectingIdentifyThrottler() {
      return { waitForIdentify: jest.fn().mockRejectedValue(throttleError) };
    }
    const { shim, managerInstances, onFatal } = makeShim({
      IdentifyThrottlerCtor: RejectingIdentifyThrottler,
    });
    await shim.start();
    const mgr = managerInstances[0];
    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);

    await expect(
      throttler.waitForIdentify(0, new AbortController().signal),
    ).rejects.toBe(throttleError);
    expect(shim._getIdentifyAttemptsForTest()).toBe(0);
    expect(onFatal).not.toHaveBeenCalled();
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

  it('keeps an over-budget grant pending if upstream omits the abort signal', async () => {
    const { shim, managerInstances, onFatal } = makeShim();
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
    // The cap=1 alone would crash-loop a long-lived task whose
    // Discord resume buffer expires (>60s outage): cold-start
    // IDENTIFY burns the budget, then the post-outage RESUME
    // rejection would throw on the very next retrieve. Reset-on-
    // READY restores the budget after every successful session
    // so reconnects-after-outage stay healthy.
    const { shim, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    const throttler = await mgr._constructorArgs.buildIdentifyThrottler(mgr);

    // Cold start: the first actual IDENTIFY grant consumes one.
    await throttler.waitForIdentify(0, new AbortController().signal);
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);

    // READY arrives — counter resets.
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
    // Defensive: Discord's READY shape always includes application.id,
    // but if a future API change moves it, we want isReady=true to
    // still flip (the WS is open) — appId stays null and registerCommands
    // can detect+report the missing piece rather than the bot looking
    // wedged.
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
    // Discord delivers RESUMED (not READY) on a successful resume,
    // which is the entire Pillar 2 win. Without this branch, the
    // shim's isReady() stays false through a successful resume, the
    // health server stays 503, and ECS would replace the task —
    // defeating the optimization.
    const { shim, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];

    expect(shim.isReady()).toBe(false);

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'RESUMED', d: {} },
      shardId: 0,
    });

    expect(shim.isReady()).toBe(true);
    // appId stays whatever it was before RESUMED — the prior
    // process's READY populated it via DDB hydration (or it's
    // null if this process never observed a READY directly,
    // which is correct: a pure-resume process never re-registers
    // commands).
    expect(shim.getAppId()).toBeNull();
  });

  it('RESUMED resets the IDENTIFY budget so a later disconnect-reconnect cycle gets a fresh allowance', async () => {
    // Symmetric with the READY-reset path: every successful session
    // (whether first-time READY or warm-start RESUMED) restores
    // the IDENTIFY counter. Otherwise a process that boots via
    // RESUME and later sees its session age out (>60s outage)
    // would have count=1 stuck since the prior process's READY
    // and would trip the cap on the very next reconnect.
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
    // Pin the payload contract — handlers receive the full {data, shardId}
    // payload identical to the underlying Dispatch event. This matches
    // what discord.js's `raw` listeners (the legacy event-publisher
    // wiring point) get, so commit 4's migration is a near-mechanical
    // re-pointing.
    expect(h1).toHaveBeenCalledWith({
      data: { t: 'INTERACTION_CREATE', d: {} },
      shardId: 0,
    });
  });

  it('a throwing handler does not break sibling handlers', async () => {
    // Defensive isolation — one bad handler (e.g., the event-publisher
    // throwing on a malformed envelope) shouldn't blackhole the
    // gateway-activity ticker.
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

    // Single most-load-bearing assertion in this file. The 60 s
    // Discord resume buffer relies on a TCP drop; manager.destroy()
    // sends a clean close that invalidates the session.
    expect(managerInstances[0].destroy).not.toHaveBeenCalled();
    // flushFinal should have run by default.
    expect(store.flushFinal).toHaveBeenCalledTimes(1);
  });

  it('stop({ flushFinal: false }) routes through store.stop()', async () => {
    // Test seam for the case where the caller wants to bail without
    // a final write (test cleanup, error paths). Still no
    // manager.destroy().
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

    // Late dispatch after stop() — handler should NOT fire.
    mgr.emit(WebSocketShardEvents.Dispatch, { data: { t: 'X' }, shardId: 0 });
    expect(h).not.toHaveBeenCalled();
  });

  it('removes only the listeners the shim installed (does not strip foreign listeners)', async () => {
    // The shim installs Dispatch + Error listeners; stop() must
    // detach those and leave any other listeners alone. An unscoped
    // manager.removeAllListeners() would also strip @discordjs/ws's
    // own internal listeners on the same emitter.
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
    // A graceful-shutdown signal arriving twice (SIGTERM then SIGINT
    // racing) shouldn't double-flush the store or otherwise re-enter
    // teardown. Single-flush also matters for cost — flushFinal
    // issues a synchronous DDB PUT; a second one is wasted.
    const { shim, store } = makeShim();
    await shim.start();

    await shim.stop();
    await shim.stop();

    expect(store.flushFinal).toHaveBeenCalledTimes(1);
  });

  it('drops dispatches that arrive during stop() teardown (between flag-flip and listener detach)', async () => {
    // Symmetric to the connect-timeout late-dispatch test: a successful
    // start() can have dispatches in flight when SIGTERM lands. stop()'s
    // first lines set stopped=true and clear dispatchHandlers — but
    // flushFinal awaits a DDB round-trip, leaving a window where the
    // manager's Dispatch listener is still attached. A frame arriving
    // mid-flush must NOT reach downstream handlers, otherwise SQS would
    // see a stray INTERACTION_CREATE after the worker has already begun
    // its own shutdown. Belt-and-suspenders coverage: both the
    // stopped-flag guard AND the cleared handlers-set must hold.
    const { shim, store, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];

    const h = jest.fn();
    shim.onDispatch(h);

    // Make flushFinal block until we say go.
    let resolveFlush;
    store.flushFinal.mockImplementation(() => new Promise((r) => { resolveFlush = r; }));

    const stopPromise = shim.stop();
    await Promise.resolve(); // let stop() run up to the await

    // Fire a dispatch during the teardown window. The handler must
    // not be called.
    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'INTERACTION_CREATE', d: {} },
      shardId: 0,
    });
    expect(h).not.toHaveBeenCalled();

    resolveFlush();
    await stopPromise;
  });

  it('stop() after a failed start still runs cleanup (listener detach + flushFinal)', async () => {
    // Regression guard for the cr-r5-caught flag-conflation bug:
    // start()'s catch sets `stopped=true` for the dispatch-race
    // guard. A naive single-flag impl would make stop()'s
    // idempotency check (`if (stopped) return`) short-circuit on
    // the failed-start path — so manager.removeAllListeners() and
    // store.flushFinal() would never run. Splitting into
    // `stopped` (drop-dispatches) and `stopCompleted` (idempotency)
    // keeps cleanup reachable.
    const { SlowFakeManager, instances: lateInstances } = makeSlowManagerCtor();
    const { shim, store } = makeShim({ WebSocketManagerCtor: SlowFakeManager });
    await expect(shim.start({ timeoutMs: 10 })).rejects.toThrow(/timed out/);
    const mgr = lateInstances[0];
    expect(mgr.listenerCount(WebSocketShardEvents.Dispatch)).toBeGreaterThan(0);

    // Caller (gracefulShutdown) now calls stop() after the throw.
    // It must reach flushFinal AND detach listeners despite
    // start()-fail having flipped `stopped` first.
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
    // No internal REST was constructed when one was injected.
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
    // Matches the legacy client.login() timeout in index.js. A drift
    // in this constant changes the boot-fail latency observably and
    // should require a deliberate test update.
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

});
