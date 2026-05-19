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
//      via thrown error from the retrieveSessionInfo wrapper. A
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
  VERIFIED_DJS_WS_MAJOR_MINOR,
} = require('../src/gateway-ws-shim');
const { WebSocketShardEvents } = require('@discordjs/ws');

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
    // REST was lazy-constructed since `rest` wasn't injected.
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
    // @discordjs/ws v1.2.x emits Resumed with `(shardId: number)` —
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
    // @discordjs/ws v1.2.x Closed payload is `{ code, shardId }` —
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
  it('passes through when mirror is non-null (RESUME path; no budget impact)', async () => {
    const { shim, store, managerInstances } = makeShim();
    store._setMirror({ sessionId: 'sess-A', resumeURL: 'wss://r/a', sequence: 1 });

    await shim.start();
    const { retrieveSessionInfo } = managerInstances[0]._constructorArgs;

    // 100 calls, all return the mirror, identifyAttempts stays 0.
    for (let i = 0; i < 100; i++) {
      expect(retrieveSessionInfo('0:1')).not.toBeNull();
    }
    expect(shim._getIdentifyAttemptsForTest()).toBe(0);
  });

  it('throws on second null-mirror call (cap = 1)', async () => {
    const { shim, managerInstances } = makeShim();
    // Default store mirror is null.

    await shim.start();
    const { retrieveSessionInfo } = managerInstances[0]._constructorArgs;

    // First call: budget=1, returns null (IDENTIFY pending).
    expect(retrieveSessionInfo('0:1')).toBeNull();
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);

    // Second call: budget exhausted, throws.
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
    // The cap=1 alone would crash-loop a long-lived task whose
    // Discord resume buffer expires (>60s outage): cold-start
    // IDENTIFY burns the budget, then the post-outage RESUME
    // rejection would throw on the very next retrieve. Reset-on-
    // READY restores the budget after every successful session
    // so reconnects-after-outage stay healthy.
    const { shim, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];
    const { retrieveSessionInfo } = mgr._constructorArgs;

    // Cold start: retrieve → null → IDENTIFY pending (count=1).
    expect(retrieveSessionInfo('0:1')).toBeNull();
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);

    // READY arrives — counter resets.
    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'READY', d: { application: { id: 'app-1' } } },
      shardId: 0,
    });
    expect(shim._getIdentifyAttemptsForTest()).toBe(0);

    // Later, Discord drops the session past its resume buffer.
    // Library calls retrieve → mirror is null → another IDENTIFY
    // is permitted (count=1, still under cap).
    expect(retrieveSessionInfo('0:1')).toBeNull();
    expect(shim._getIdentifyAttemptsForTest()).toBe(1);

    // A second consecutive null-mirror retrieve (no READY between)
    // does trip the cap — the loop-without-READY hazard the cap
    // exists to catch.
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
    const { retrieveSessionInfo } = mgr._constructorArgs;

    // Synthesize a non-zero counter as if a prior reconnect
    // attempt had landed.
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

  it('VERIFIED_DJS_WS_MAJOR_MINOR matches the installed @discordjs/ws major.minor', () => {
    // The wsConnected mirror depends on @discordjs/ws's
    // WebSocketShard.onMessage emitting Ready/Resumed BEFORE the
    // Dispatch fan-out — verified against the version captured by
    // VERIFIED_DJS_WS_MAJOR_MINOR. A node_modules bump past that
    // range fails this test, forcing whoever bumps the dep to
    // re-read the upstream dispatch handler before merging.
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
    const installedMajorMinor = djsWsVersion.split('.').slice(0, 2).join('.');
    expect(installedMajorMinor).toBe(VERIFIED_DJS_WS_MAJOR_MINOR);
  });
});
