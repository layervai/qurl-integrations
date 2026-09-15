const { EventEmitter } = require('events');
const {
  shouldUsePushHandoffShutdown,
  selectGatewayReadinessProbe,
  awaitServerListening,
  tryStop,
  tryClose,
  runPushHandoffShutdown,
} = require('../src/gateway-shutdown-helpers');

function makeFakeLeader({ holdingLock = false, ticking = false } = {}) {
  return {
    isHoldingLock: jest.fn(() => holdingLock),
    hasStartedTickLoop: jest.fn(() => ticking),
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

describe('shouldUsePushHandoffShutdown', () => {
  it('returns false when hot-standby is off (legacy / Pillar 2 only)', () => {
    expect(shouldUsePushHandoffShutdown({
      enableHotStandby: false,
      gatewayLeader: makeFakeLeader({ holdingLock: true }),
    })).toBe(false);
  });

  it('returns false when leader is null (pre-startHotStandby boot window)', () => {
    expect(shouldUsePushHandoffShutdown({
      enableHotStandby: true,
      gatewayLeader: null,
    })).toBe(false);
  });

  it('returns false when leader is not holding the lock (standby branch)', () => {
    expect(shouldUsePushHandoffShutdown({
      enableHotStandby: true,
      gatewayLeader: makeFakeLeader({ holdingLock: false }),
    })).toBe(false);
  });

  it('returns true only when hot-standby + leader + holding-lock all hold', () => {
    expect(shouldUsePushHandoffShutdown({
      enableHotStandby: true,
      gatewayLeader: makeFakeLeader({ holdingLock: true }),
    })).toBe(true);
  });

  it('re-reads lock state per call (no caching)', () => {
    let holding = true;
    const leader = {
      isHoldingLock: jest.fn(() => holding),
    };
    expect(shouldUsePushHandoffShutdown({
      enableHotStandby: true, gatewayLeader: leader,
    })).toBe(true);
    holding = false;
    expect(shouldUsePushHandoffShutdown({
      enableHotStandby: true, gatewayLeader: leader,
    })).toBe(false);
  });
});

describe('selectGatewayReadinessProbe', () => {
  it('legacy mode (flag-off): probe reports client.isReady()', () => {
    const client = { isReady: jest.fn(() => true) };
    const probe = selectGatewayReadinessProbe({
      enableHotStandby: false,
      enableGatewayResume: false,
      gatewayShim: null,
      getGatewayLeader: () => null,
      client,
    });
    expect(probe()).toBe(true);
    expect(client.isReady).toHaveBeenCalled();
  });

  it('Pillar 2 mode: probe reports shim.isReady()', () => {
    const gatewayShim = { isReady: jest.fn(() => true) };
    const probe = selectGatewayReadinessProbe({
      enableHotStandby: false,
      enableGatewayResume: true,
      gatewayShim,
      getGatewayLeader: () => null,
      client: { isReady: jest.fn() },
    });
    expect(probe()).toBe(true);
    expect(gatewayShim.isReady).toHaveBeenCalled();
  });

  it('Pillar 2 mode without a shim (e.g. flag-on http tier): falls back to client.isReady()', () => {
    const client = { isReady: jest.fn(() => false) };
    const probe = selectGatewayReadinessProbe({
      enableHotStandby: false,
      enableGatewayResume: true,
      gatewayShim: null,
      getGatewayLeader: () => null,
      client,
    });
    expect(probe()).toBe(false);
    expect(client.isReady).toHaveBeenCalled();
  });

  it('hot-standby + pre-startHotStandby window (leader null): probe returns false', () => {
    const probe = selectGatewayReadinessProbe({
      enableHotStandby: true,
      enableGatewayResume: true,
      gatewayShim: { isReady: jest.fn() },
      getGatewayLeader: () => null,
      client: { isReady: jest.fn() },
    });
    expect(probe()).toBe(false);
  });

  it('hot-standby + active replica: probe reports shim.isReady()', () => {
    const gatewayShim = { isReady: jest.fn(() => true) };
    const leader = makeFakeLeader({ holdingLock: true, ticking: true });
    const probe = selectGatewayReadinessProbe({
      enableHotStandby: true,
      enableGatewayResume: true,
      gatewayShim,
      getGatewayLeader: () => leader,
      client: { isReady: jest.fn() },
    });
    expect(probe()).toBe(true);
    expect(gatewayShim.isReady).toHaveBeenCalled();
    expect(leader.hasStartedTickLoop).not.toHaveBeenCalled();
  });

  it('hot-standby + standby replica: probe reports hasStartedTickLoop()', () => {
    const gatewayShim = { isReady: jest.fn(() => false) }; // Standby has no WS.
    const leader = makeFakeLeader({ holdingLock: false, ticking: true });
    const probe = selectGatewayReadinessProbe({
      enableHotStandby: true,
      enableGatewayResume: true,
      gatewayShim,
      getGatewayLeader: () => leader,
      client: { isReady: jest.fn() },
    });
    expect(probe()).toBe(true);
    expect(gatewayShim.isReady).not.toHaveBeenCalled();
    expect(leader.hasStartedTickLoop).toHaveBeenCalled();
  });

  it('hot-standby + standby with dead tick loop: probe returns false', () => {
    const leader = makeFakeLeader({ holdingLock: false, ticking: false });
    const probe = selectGatewayReadinessProbe({
      enableHotStandby: true,
      enableGatewayResume: true,
      gatewayShim: { isReady: jest.fn() },
      getGatewayLeader: () => leader,
      client: { isReady: jest.fn() },
    });
    expect(probe()).toBe(false);
  });

  it('hot-standby probe re-reads gatewayLeader via the callback (lock flip mid-deploy)', () => {
    let currentLeader = null;
    const probe = selectGatewayReadinessProbe({
      enableHotStandby: true,
      enableGatewayResume: true,
      gatewayShim: { isReady: jest.fn(() => true) },
      getGatewayLeader: () => currentLeader,
      client: { isReady: jest.fn() },
    });

    expect(probe()).toBe(false);

    currentLeader = makeFakeLeader({ holdingLock: true });
    expect(probe()).toBe(true);

    currentLeader = makeFakeLeader({ holdingLock: false, ticking: true });
    expect(probe()).toBe(true); // standby is still healthy via tick loop
  });
});

describe('awaitServerListening', () => {
  function makeFakeServer({ listening = false } = {}) {
    const s = new EventEmitter();
    s.listening = listening;
    return s;
  }

  it('resolves immediately when server.listening is already true (fast path)', async () => {
    const server = makeFakeServer({ listening: true });
    await expect(awaitServerListening(server)).resolves.toBeUndefined();
  });

  it('resolves on the `listening` event when server starts not-yet-listening', async () => {
    const server = makeFakeServer();
    const promise = awaitServerListening(server);
    server.emit('listening');
    await expect(promise).resolves.toBeUndefined();
  });

  it('rejects on the `error` event with the emitted error', async () => {
    const server = makeFakeServer();
    const promise = awaitServerListening(server);
    server.emit('error', new Error('EADDRINUSE'));
    await expect(promise).rejects.toThrow(/EADDRINUSE/);
  });

  it('rejects on the `close` event — closes the SIGTERM-during-listen-await hang', async () => {
    const server = makeFakeServer();
    const promise = awaitServerListening(server);
    server.emit('close');
    await expect(promise).rejects.toThrow(/closed before listening/);
  });

  it('removes all three listeners on `listening` resolve (no late-event leakage)', async () => {
    const server = makeFakeServer();
    const promise = awaitServerListening(server);
    server.emit('listening');
    await promise;
    expect(server.listenerCount('listening')).toBe(0);
    expect(server.listenerCount('error')).toBe(0);
    expect(server.listenerCount('close')).toBe(0);
  });

  it('removes all three listeners on `error` reject', async () => {
    const server = makeFakeServer();
    const promise = awaitServerListening(server);
    server.emit('error', new Error('bind failed'));
    await expect(promise).rejects.toThrow();
    expect(server.listenerCount('listening')).toBe(0);
    expect(server.listenerCount('error')).toBe(0);
    expect(server.listenerCount('close')).toBe(0);
  });

  it('removes all three listeners on `close` reject', async () => {
    const server = makeFakeServer();
    const promise = awaitServerListening(server);
    server.emit('close');
    await expect(promise).rejects.toThrow();
    expect(server.listenerCount('listening')).toBe(0);
    expect(server.listenerCount('error')).toBe(0);
    expect(server.listenerCount('close')).toBe(0);
  });

  it('a second `error` after `listening` does not surface an unhandled rejection', async () => {
    const server = makeFakeServer();
    const promise = awaitServerListening(server);
    server.emit('listening');
    await promise;
    expect(() => server.emit('error', new Error('runtime listener-error'))).toThrow(/runtime listener-error/);
  });
});

describe('tryStop', () => {
  it('is a no-op for null handle (hot-standby off — leader never constructed)', async () => {
    const logger = makeFakeLogger();
    await tryStop('connection-watchdog', null, logger);
    expect(logger.warn).not.toHaveBeenCalled();
  });

  it('awaits the handle.stop() promise', async () => {
    const logger = makeFakeLogger();
    const handle = { stop: jest.fn().mockResolvedValue(undefined) };
    await tryStop('leader', handle, logger);
    expect(handle.stop).toHaveBeenCalledTimes(1);
    expect(logger.warn).not.toHaveBeenCalled();
  });

  it('logs at warn (with error + stack) and swallows the error if stop() rejects', async () => {
    const logger = makeFakeLogger();
    const err = new Error('ddb down');
    const handle = { stop: jest.fn().mockRejectedValue(err) };
    await expect(tryStop('leader', handle, logger)).resolves.toBeUndefined();
    expect(logger.warn).toHaveBeenCalledWith('leader stop failed', {
      error: 'ddb down',
      stack: err.stack,
    });
  });

  it('embeds the component name in the warn message for triage', async () => {
    const logger = makeFakeLogger();
    const handle = { stop: jest.fn().mockRejectedValue(new Error('boom')) };
    await tryStop('connection-watchdog', handle, logger);
    expect(logger.warn).toHaveBeenCalledWith(
      'connection-watchdog stop failed',
      expect.objectContaining({ error: 'boom' }),
    );
  });
});

describe('tryClose', () => {
  it('is a no-op for null server handle', async () => {
    const logger = makeFakeLogger();
    await tryClose('HTTP server', null, logger);
    expect(logger.warn).not.toHaveBeenCalled();
  });

  it('awaits the server.close() callback', async () => {
    const logger = makeFakeLogger();
    const server = { close: jest.fn((cb) => cb()) };
    await tryClose('HTTP server', server, logger);
    expect(server.close).toHaveBeenCalledTimes(1);
    expect(logger.warn).not.toHaveBeenCalled();
  });

  it('logs at warn (with error + stack) and resolves cleanly when close yields an error', async () => {
    const logger = makeFakeLogger();
    const err = new Error('listener already detached');
    const server = { close: jest.fn((cb) => cb(err)) };
    await expect(tryClose('control-channel server', server, logger)).resolves.toBeUndefined();
    expect(logger.warn).toHaveBeenCalledWith(
      'control-channel server close reported error',
      { error: 'listener already detached', stack: err.stack },
    );
  });

  it('embeds the component name in the warn message', async () => {
    const logger = makeFakeLogger();
    const server = { close: jest.fn((cb) => cb(new Error('boom'))) };
    await tryClose('HTTP server', server, logger);
    expect(logger.warn).toHaveBeenCalledWith(
      'HTTP server close reported error',
      expect.objectContaining({ error: 'boom' }),
    );
  });
});

describe('runPushHandoffShutdown', () => {
  function makeTimerSpy() {
    const timers = [];
    const fn = jest.fn((cb, ms) => {
      const timer = { cb, ms, unref: jest.fn() };
      timers.push(timer);
      return timer;
    });
    fn.timers = timers;
    return fn;
  }

  function makeDeps(overrides = {}) {
    return {
      logger: makeFakeLogger(),
      gatewayLeader: { pushHandoff: jest.fn().mockResolvedValue({ transferred: true, pushAcked: true }) },
      exit: jest.fn(),
      scheduleHardExit: makeTimerSpy(),
      clearHardExit: jest.fn(),
      ...overrides,
    };
  }

  it('on a successful pushHandoff, exits with the incoming code', async () => {
    const deps = makeDeps();
    await runPushHandoffShutdown({ code: 0, ...deps });
    expect(deps.gatewayLeader.pushHandoff).toHaveBeenCalledTimes(1);
    expect(deps.exit).toHaveBeenCalledWith(0);
    expect(deps.logger.info).toHaveBeenCalledWith(
      'pushHandoff complete',
      expect.objectContaining({ transferred: true, pushAcked: true }),
    );
  });

  it('stops the connection watchdog synchronously before transferring the lock', async () => {
    const order = [];
    const connectionWatchdog = {
      stop: jest.fn(() => {
        order.push('watchdog-stop');
        return Promise.resolve();
      }),
    };
    const deps = makeDeps({
      connectionWatchdog,
      gatewayLeader: {
        pushHandoff: jest.fn(async () => {
          order.push('push-handoff');
          return { transferred: true, pushAcked: true };
        }),
      },
    });

    await runPushHandoffShutdown({ code: 0, ...deps });

    expect(connectionWatchdog.stop).toHaveBeenCalledTimes(1);
    expect(order).toEqual(['watchdog-stop', 'push-handoff']);
  });

  it('logs a synchronous watchdog stop failure and still transfers the lock', async () => {
    const err = new Error('stop threw');
    const deps = makeDeps({
      connectionWatchdog: { stop: jest.fn(() => { throw err; }) },
    });

    await runPushHandoffShutdown({ code: 0, ...deps });

    expect(deps.logger.warn).toHaveBeenCalledWith(
      'connection-watchdog stop failed',
      { error: 'stop threw', stack: err.stack },
    );
    expect(deps.gatewayLeader.pushHandoff).toHaveBeenCalledTimes(1);
    expect(deps.exit).toHaveBeenCalledWith(0);
  });

  it('observes an asynchronous watchdog stop rejection without blocking handoff', async () => {
    const err = new Error('stop rejected');
    const deps = makeDeps({
      connectionWatchdog: { stop: jest.fn().mockRejectedValue(err) },
    });

    await runPushHandoffShutdown({ code: 0, ...deps });

    expect(deps.logger.warn).toHaveBeenCalledWith(
      'connection-watchdog stop failed',
      { error: 'stop rejected', stack: err.stack },
    );
    expect(deps.gatewayLeader.pushHandoff).toHaveBeenCalledTimes(1);
    expect(deps.exit).toHaveBeenCalledWith(0);
  });

  it('on a thrown pushHandoff, exits with forcedExitCode so deploy metrics distinguish clean transfer from throw', async () => {
    const deps = makeDeps({
      gatewayLeader: { pushHandoff: jest.fn().mockRejectedValue(new Error('peer unreachable')) },
    });
    await runPushHandoffShutdown({ code: 0, ...deps });
    expect(deps.exit).toHaveBeenCalledWith(1);
    expect(deps.logger.error).toHaveBeenCalledWith(
      'pushHandoff threw — exiting anyway so the standby can cold-acquire',
      expect.objectContaining({ error: 'peer unreachable' }),
    );
  });

  it('forcedExitCode is configurable for the throw path too', async () => {
    const deps = makeDeps({
      gatewayLeader: { pushHandoff: jest.fn().mockRejectedValue(new Error('hmac mismatch')) },
    });
    await runPushHandoffShutdown({ code: 0, forcedExitCode: 42, ...deps });
    expect(deps.exit).toHaveBeenCalledWith(42);
  });

  it('clears the hard-exit timer on the success path so a non-terminal injected exit does not see a spurious second exit', async () => {
    const deps = makeDeps();
    await runPushHandoffShutdown({ code: 0, ...deps });
    expect(deps.clearHardExit).toHaveBeenCalledTimes(1);
    expect(deps.clearHardExit).toHaveBeenCalledWith(deps.scheduleHardExit.timers[0]);
    expect(deps.exit).toHaveBeenCalledTimes(1);
    expect(deps.exit).toHaveBeenCalledWith(0);
  });

  it('clears the hard-exit timer on the throw path too (and exits with forcedExitCode)', async () => {
    const deps = makeDeps({
      gatewayLeader: { pushHandoff: jest.fn().mockRejectedValue(new Error('peer down')) },
    });
    await runPushHandoffShutdown({ code: 0, ...deps });
    expect(deps.clearHardExit).toHaveBeenCalledTimes(1);
    expect(deps.exit).toHaveBeenCalledTimes(1);
    expect(deps.exit).toHaveBeenCalledWith(1);
  });

  it('schedules a hard-exit timer with the configured ceiling', async () => {
    const deps = makeDeps();
    await runPushHandoffShutdown({ code: 0, ceilingMs: 9999, ...deps });
    expect(deps.scheduleHardExit).toHaveBeenCalledWith(expect.any(Function), 9999);
  });

  it('default ceiling is 12_000 ms (9s pushHandoff + 3s headroom)', async () => {
    const deps = makeDeps();
    await runPushHandoffShutdown({ code: 0, ...deps });
    expect(deps.scheduleHardExit).toHaveBeenCalledWith(expect.any(Function), 12_000);
  });

  it('unrefs the hard-exit timer so it does not pin the event loop', async () => {
    const deps = makeDeps();
    await runPushHandoffShutdown({ code: 0, ...deps });
    expect(deps.scheduleHardExit.timers[0].unref).toHaveBeenCalledTimes(1);
  });

  it('hard-exit firing uses forcedExitCode=1 even when the incoming SIGTERM was code 0', async () => {
    const handoffResolvers = {};
    const handoffPromise = new Promise((resolve) => { handoffResolvers.resolve = resolve; });
    const deps = makeDeps({
      gatewayLeader: { pushHandoff: jest.fn().mockReturnValue(handoffPromise) },
    });

    const shutdown = runPushHandoffShutdown({ code: 0, ...deps });
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(deps.scheduleHardExit.timers).toHaveLength(1);

    deps.scheduleHardExit.timers[0].cb();
    expect(deps.exit).toHaveBeenCalledWith(1);
    expect(deps.logger.error).toHaveBeenCalledWith('PushHandoff shutdown timed out, forcing exit');

    handoffResolvers.resolve({ transferred: true, pushAcked: true });
    await shutdown;
  });

  it('forcedExitCode is configurable', async () => {
    const handoffResolvers = {};
    const handoffPromise = new Promise((resolve) => { handoffResolvers.resolve = resolve; });
    const deps = makeDeps({
      gatewayLeader: { pushHandoff: jest.fn().mockReturnValue(handoffPromise) },
    });
    const shutdown = runPushHandoffShutdown({ code: 0, forcedExitCode: 42, ...deps });
    await new Promise((resolve) => { setImmediate(resolve); });
    deps.scheduleHardExit.timers[0].cb();
    expect(deps.exit).toHaveBeenCalledWith(42);
    handoffResolvers.resolve({});
    await shutdown;
  });

  it('forwards every pushHandoff result field into the log line for observability', async () => {
    const deps = makeDeps({
      gatewayLeader: { pushHandoff: jest.fn().mockResolvedValue({
        transferred: false, pushAcked: false, reason: 'no_peer', pushReason: 'push_threw',
      }) },
    });
    await runPushHandoffShutdown({ code: 0, ...deps });
    expect(deps.logger.info).toHaveBeenCalledWith('pushHandoff complete', {
      transferred: false,
      pushAcked: false,
      reason: 'no_peer',
      pushReason: 'push_threw',
    });
  });

  it('drains eventPublisher concurrently with pushHandoff (publisher.stop called before pushHandoff resolves)', async () => {
    const handoffResolvers = {};
    const handoffPromise = new Promise((resolve) => { handoffResolvers.resolve = resolve; });
    const eventPublisher = { stop: jest.fn().mockResolvedValue(undefined) };
    const deps = makeDeps({
      eventPublisher,
      gatewayLeader: { pushHandoff: jest.fn().mockReturnValue(handoffPromise) },
    });

    const shutdownPromise = runPushHandoffShutdown({ code: 0, ...deps });
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(eventPublisher.stop).toHaveBeenCalledTimes(1);
    expect(deps.exit).not.toHaveBeenCalled(); // pushHandoff still pending

    handoffResolvers.resolve({ transferred: true, pushAcked: true });
    await shutdownPromise;
    expect(deps.exit).toHaveBeenCalledWith(0);
  });

  it('eventPublisher omitted is fine (legacy / flag-off / test setups)', async () => {
    const deps = makeDeps();
    await runPushHandoffShutdown({ code: 0, ...deps });
    expect(deps.exit).toHaveBeenCalledWith(0);
  });

  it('eventPublisher explicit null is fine (SIGTERM before publisher.start() ran)', async () => {
    const deps = makeDeps();
    await runPushHandoffShutdown({ code: 0, eventPublisher: null, ...deps });
    expect(deps.exit).toHaveBeenCalledWith(0);
    expect(deps.gatewayLeader.pushHandoff).toHaveBeenCalledTimes(1);
  });

  it('eventPublisher.stop() failure is absorbed via tryStop (not propagated)', async () => {
    const eventPublisher = { stop: jest.fn().mockRejectedValue(new Error('sqs unreachable')) };
    const deps = makeDeps({ eventPublisher });
    await runPushHandoffShutdown({ code: 0, ...deps });
    expect(deps.logger.warn).toHaveBeenCalledWith(
      'event-publisher stop failed',
      expect.objectContaining({ error: 'sqs unreachable' }),
    );
    expect(deps.exit).toHaveBeenCalledWith(0);
  });
});
