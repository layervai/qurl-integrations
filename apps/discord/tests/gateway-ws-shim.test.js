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
} = require('../src/gateway-ws-shim');
const { WebSocketShardEvents } = require('@discordjs/ws');

// Fake WebSocketManager built on EventEmitter. Captures construction
// args so tests can interrogate the callback wiring and emit fake
// Dispatch / Error events to drive the shim's listeners.
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

  it('rejects on connect timeout', async () => {
    const { FakeManager } = makeFakeManagerCtor();
    // Override connect to never resolve.
    const slowFakeManager = function (args) {
      const inst = new (function () {})();
      Object.assign(inst, new EventEmitter());
      inst.connect = jest.fn(() => new Promise(() => { /* never resolves */ }));
      inst.destroy = jest.fn().mockResolvedValue(undefined);
      inst.on = EventEmitter.prototype.on.bind(inst);
      inst.emit = EventEmitter.prototype.emit.bind(inst);
      inst._constructorArgs = args;
      return inst;
    };
    const { shim } = makeShim({ WebSocketManagerCtor: slowFakeManager });

    await expect(shim.start({ timeoutMs: 10 })).rejects.toThrow(/timed out after 10ms/);
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

  it('non-READY dispatches do not flip isReady', async () => {
    const { shim, managerInstances } = makeShim();
    await shim.start();
    const mgr = managerInstances[0];

    mgr.emit(WebSocketShardEvents.Dispatch, {
      data: { t: 'GUILD_CREATE', d: { id: 'guild-1' } },
      shardId: 0,
    });

    expect(shim.isReady()).toBe(false);
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
});

describe('exposed REST instance', () => {
  it('reuses an injected REST when provided', async () => {
    const injectedRest = { setToken: jest.fn().mockReturnThis(), token: 'pre-bound' };
    const { shim, restInstances } = makeShim({ rest: injectedRest });

    await shim.start();
    expect(shim.rest).toBe(injectedRest);
    // No internal REST was constructed when one was injected.
    expect(restInstances).toHaveLength(0);
  });

  it('lazy-constructs and binds token when REST is not injected', async () => {
    const { shim, restInstances } = makeShim();
    await shim.start();

    expect(restInstances).toHaveLength(1);
    expect(shim.rest).toBe(restInstances[0]);
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
});
