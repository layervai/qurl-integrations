const { EventEmitter } = require('events');
const fs = require('fs');
const path = require('path');
const {
  shouldUsePushHandoffShutdown,
  selectGatewayReadinessProbe,
  awaitServerListening,
  tryStop,
  tryClose,
  stopGatewayHotStandby,
  runGracefulShutdown,
  runGatewayFatalShutdown,
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

describe('index gateway-fatal composition contract', () => {
  const indexSource = fs.readFileSync(path.join(__dirname, '../src/index.js'), 'utf8');

  it('wires the IDENTIFY fatal callback into the gateway shim', () => {
    expect(indexSource).toMatch(
      /createGatewayWsShim\(\{[^}]*\bonFatal:\s*gatewayFatalShutdown\b[^}]*\}\)/s,
    );
  });

  it('threads every fatal non-await option through graceful teardown', () => {
    expect(indexSource).toMatch(
      /stopGatewayHotStandby\(\{[^}]*\bawaitControlChannelServer\b[^}]*\bawaitConnectionWatchdog\b[^}]*\bawaitGatewayLeader\b[^}]*\}\)/s,
    );
    expect(indexSource).toMatch(
      /teardown:\s*\(\)\s*=>\s*gracefulShutdownTeardown\(\{[^}]*\bawaitControlChannelServer\b[^}]*\bawaitConnectionWatchdog\b[^}]*\bawaitGatewayLeader\b[^}]*\}\)/s,
    );
  });
});

describe('runGracefulShutdown', () => {
  it('claims shutdown and arms the hard-exit backstop before teardown starts', async () => {
    const order = [];
    let finishTeardown;
    const hardExit = { unref: jest.fn() };
    const scheduleHardExit = jest.fn(() => {
      order.push('timer');
      return hardExit;
    });
    const clearHardExit = jest.fn();
    const exit = jest.fn();
    const shutdown = runGracefulShutdown({
      code: 1,
      claimShutdown: () => { order.push('claim'); return true; },
      teardown: async () => {
        order.push('teardown');
        await new Promise(resolve => { finishTeardown = resolve; });
      },
      logger: makeFakeLogger(),
      scheduleHardExit,
      clearHardExit,
      exit,
    });

    expect(order).toEqual(['claim', 'timer', 'teardown']);
    expect(hardExit.unref).toHaveBeenCalledTimes(1);
    expect(exit).not.toHaveBeenCalled();

    finishTeardown();
    await shutdown;
    expect(clearHardExit).toHaveBeenCalledWith(hardExit);
    expect(exit).toHaveBeenCalledTimes(1);
    expect(exit).toHaveBeenCalledWith(1);
  });

  it('exits only once when the hard timeout wins the teardown race', async () => {
    let forceExit;
    let finishTeardown;
    const exit = jest.fn();
    const shutdown = runGracefulShutdown({
      claimShutdown: () => true,
      teardown: () => new Promise(resolve => { finishTeardown = resolve; }),
      logger: makeFakeLogger(),
      scheduleHardExit: (callback) => { forceExit = callback; return { unref() {} }; },
      clearHardExit: jest.fn(),
      exit,
    });

    forceExit();
    expect(exit).toHaveBeenCalledTimes(1);
    expect(exit).toHaveBeenCalledWith(1);
    finishTeardown();
    await shutdown;
    expect(exit).toHaveBeenCalledTimes(1);
  });

  it('still forces exit when timeout logging throws', async () => {
    let forceExit;
    let finishTeardown;
    const logger = makeFakeLogger();
    logger.error.mockImplementation(() => { throw new Error('logger-failure'); });
    const exit = jest.fn();
    const shutdown = runGracefulShutdown({
      claimShutdown: () => true,
      teardown: () => new Promise(resolve => { finishTeardown = resolve; }),
      logger,
      scheduleHardExit: (callback) => { forceExit = callback; return { unref() {} }; },
      clearHardExit: jest.fn(),
      exit,
    });

    expect(() => forceExit()).toThrow('logger-failure');
    expect(exit).toHaveBeenCalledWith(1);
    finishTeardown();
    await shutdown;
    expect(exit).toHaveBeenCalledTimes(1);
  });

  it('does nothing when another shutdown path already owns the gate', async () => {
    const teardown = jest.fn();
    const scheduleHardExit = jest.fn();
    const exit = jest.fn();

    await runGracefulShutdown({
      claimShutdown: () => false,
      teardown,
      logger: makeFakeLogger(),
      scheduleHardExit,
      exit,
    });

    expect(scheduleHardExit).not.toHaveBeenCalled();
    expect(teardown).not.toHaveBeenCalled();
    expect(exit).not.toHaveBeenCalled();
  });

  it('contains a non-Error teardown rejection and exits with the requested code', async () => {
    const logger = makeFakeLogger();
    const exit = jest.fn();
    const clearHardExit = jest.fn();
    const hardExit = { unref: jest.fn() };

    await runGracefulShutdown({
      code: 7,
      claimShutdown: () => true,
      teardown: () => Promise.reject('teardown-failure'),
      logger,
      scheduleHardExit: () => hardExit,
      clearHardExit,
      exit,
    });

    expect(logger.error).toHaveBeenCalledWith(
      'Error during shutdown',
      { error: 'teardown-failure' },
    );
    expect(clearHardExit).toHaveBeenCalledWith(hardExit);
    expect(exit).toHaveBeenCalledWith(7);
  });

  it('runs teardown and exits with the requested code when initiation logging throws', async () => {
    const logger = makeFakeLogger();
    logger.info.mockImplementation(() => { throw new Error('logger-failure'); });
    const teardown = jest.fn().mockResolvedValue(undefined);
    const exit = jest.fn();
    const clearHardExit = jest.fn();
    const hardExit = { unref: jest.fn() };

    await expect(runGracefulShutdown({
      code: 7,
      claimShutdown: () => true,
      teardown,
      logger,
      scheduleHardExit: () => hardExit,
      clearHardExit,
      exit,
    })).resolves.toBeUndefined();

    expect(teardown).toHaveBeenCalledTimes(1);
    expect(clearHardExit).toHaveBeenCalledWith(hardExit);
    expect(exit).toHaveBeenCalledWith(7);
  });

  it('exits with the requested code when teardown and failure logging both throw', async () => {
    const logger = makeFakeLogger();
    logger.error.mockImplementation(() => { throw new Error('logger-failure'); });
    const exit = jest.fn();
    const clearHardExit = jest.fn();
    const hardExit = { unref: jest.fn() };

    await expect(runGracefulShutdown({
      code: 7,
      claimShutdown: () => true,
      teardown: () => Promise.reject(new Error('teardown-failure')),
      logger,
      scheduleHardExit: () => hardExit,
      clearHardExit,
      exit,
    })).resolves.toBeUndefined();

    expect(clearHardExit).toHaveBeenCalledWith(hardExit);
    expect(exit).toHaveBeenCalledWith(7);
  });
});

describe('stopGatewayHotStandby', () => {
  it('stops control listener, watchdog, and leader in order on normal shutdown', async () => {
    const order = [];
    const controlChannelServer = {
      close: (callback) => { order.push('control'); callback(); },
    };
    const connectionWatchdog = {
      stop: async () => { order.push('watchdog'); },
    };
    const gatewayLeader = {
      stop: async () => { order.push('leader'); },
    };

    await stopGatewayHotStandby({
      controlChannelServer,
      connectionWatchdog,
      gatewayLeader,
      logger: makeFakeLogger(),
    });

    expect(order).toEqual(['control', 'watchdog', 'leader']);
  });

  it('stops but does not await watchdog or leader on IDENTIFY-fatal teardown', async () => {
    const controlChannelServer = { close: jest.fn() };
    const connectionWatchdog = { stop: jest.fn(() => new Promise(() => {})) };
    const gatewayLeader = { stop: jest.fn(() => new Promise(() => {})) };

    await expect(stopGatewayHotStandby({
      controlChannelServer,
      connectionWatchdog,
      gatewayLeader,
      awaitControlChannelServer: false,
      awaitConnectionWatchdog: false,
      awaitGatewayLeader: false,
      logger: makeFakeLogger(),
    })).resolves.toBeUndefined();

    expect(controlChannelServer.close).toHaveBeenCalledTimes(1);
    expect(connectionWatchdog.stop).toHaveBeenCalledTimes(1);
    expect(gatewayLeader.stop).toHaveBeenCalledTimes(1);
  });
});

describe('runGatewayFatalShutdown', () => {
  it('starts graceful shutdown before stopping an in-flight watchdog', async () => {
    const order = [];
    let forceExit;
    const shutdownResult = Promise.resolve('shutdown');
    const gracefulShutdown = jest.fn(() => {
      order.push('shutdown');
      return shutdownResult;
    });
    const watchdog = {
      stop: jest.fn(() => {
        order.push('watchdog-stop');
        return Promise.resolve();
      }),
    };

    const result = runGatewayFatalShutdown({
      gracefulShutdown,
      getConnectionWatchdog: () => watchdog,
      logger: makeFakeLogger(),
      scheduleHardExit: (callback) => {
        order.push('fatal-timer');
        forceExit = callback;
        return { unref: jest.fn() };
      },
      exit: jest.fn(),
    });

    expect(result).toBe(shutdownResult);
    expect(order).toEqual(['fatal-timer', 'shutdown', 'watchdog-stop']);
    expect(gracefulShutdown).toHaveBeenCalledWith(1, {
      awaitControlChannelServer: false,
      awaitConnectionWatchdog: false,
      awaitGatewayLeader: false,
    });
    expect(forceExit).toEqual(expect.any(Function));
    await result;
  });

  it('forces exit if the delegated shutdown does not terminate the process', async () => {
    let forceExit;
    const logger = makeFakeLogger();
    const exit = jest.fn();
    const timer = { unref: jest.fn() };
    const shutdown = runGatewayFatalShutdown({
      // Models gracefulShutdown returning immediately because another path
      // already owns its internal shutdown gate.
      gracefulShutdown: jest.fn().mockResolvedValue(undefined),
      getConnectionWatchdog: () => null,
      logger,
      scheduleHardExit: (callback) => { forceExit = callback; return timer; },
      exit,
    });

    expect(timer.unref).not.toHaveBeenCalled();
    await shutdown;
    expect(exit).not.toHaveBeenCalled();

    forceExit();
    expect(logger.error).toHaveBeenCalledWith(
      'Gateway fatal shutdown timed out, forcing exit',
    );
    expect(exit).toHaveBeenCalledWith(1);
  });

  it('still forces exit when fatal-timeout logging throws', async () => {
    let forceExit;
    const logger = makeFakeLogger();
    logger.error.mockImplementation(() => { throw new Error('logger-failure'); });
    const exit = jest.fn();

    await runGatewayFatalShutdown({
      gracefulShutdown: jest.fn().mockResolvedValue(undefined),
      getConnectionWatchdog: () => null,
      logger,
      scheduleHardExit: (callback) => { forceExit = callback; return { unref() {} }; },
      exit,
    });

    expect(() => forceExit()).toThrow('logger-failure');
    expect(exit).toHaveBeenCalledWith(1);
  });

  it('tolerates a fatal before the connection watchdog is constructed', async () => {
    const gracefulShutdown = jest.fn().mockResolvedValue(undefined);

    await expect(runGatewayFatalShutdown({
      gracefulShutdown,
      getConnectionWatchdog: () => null,
      logger: makeFakeLogger(),
      scheduleHardExit: () => ({ unref() {} }),
      exit: jest.fn(),
    })).resolves.toBeUndefined();

    expect(gracefulShutdown).toHaveBeenCalledWith(1, {
      awaitControlChannelServer: false,
      awaitConnectionWatchdog: false,
      awaitGatewayLeader: false,
    });
  });

  it.each([
    ['throws synchronously', { stop: () => { throw new Error('sync-stop-failure'); } }],
    ['rejects with a non-Error value', { stop: () => Promise.reject('async-stop-failure') }],
  ])('contains a watchdog stop that %s', async (_label, watchdog) => {
    const logger = makeFakeLogger();

    await expect(runGatewayFatalShutdown({
      gracefulShutdown: jest.fn().mockResolvedValue(undefined),
      getConnectionWatchdog: () => watchdog,
      logger,
      scheduleHardExit: () => ({ unref() {} }),
      exit: jest.fn(),
    })).resolves.toBeUndefined();
    await Promise.resolve();

    expect(logger.warn).toHaveBeenCalledWith(
      'connection-watchdog stop failed during gateway fatal shutdown',
      { error: expect.stringContaining('stop-failure') },
    );
  });

  it('contains watchdog stop and warning failures during fatal shutdown', async () => {
    const logger = makeFakeLogger();
    logger.warn.mockImplementation(() => { throw new Error('logger-failure'); });

    const shutdown = runGatewayFatalShutdown({
      gracefulShutdown: jest.fn().mockResolvedValue(undefined),
      getConnectionWatchdog: () => ({
        stop: () => { throw new Error('stop-failure'); },
      }),
      logger,
      scheduleHardExit: () => ({ unref() {} }),
      exit: jest.fn(),
    });

    await expect(shutdown).resolves.toBeUndefined();
    expect(logger.warn).toHaveBeenCalledWith(
      'connection-watchdog stop failed during gateway fatal shutdown',
      { error: 'stop-failure' },
    );
  });
});

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

  it.each([null, undefined, 'stop-failure'])(
    'contains a non-Error stop rejection: %p',
    async (rejection) => {
      const logger = makeFakeLogger();
      const handle = { stop: jest.fn().mockRejectedValue(rejection) };

      await expect(tryStop('leader', handle, logger)).resolves.toBeUndefined();
      expect(logger.warn).toHaveBeenCalledWith('leader stop failed', {
        error: String(rejection),
        stack: undefined,
      });
    },
  );

  it('contains logger failure while reporting a stop rejection', async () => {
    const logger = makeFakeLogger();
    logger.warn.mockImplementation(() => { throw new Error('logger-failure'); });
    const handle = { stop: jest.fn().mockRejectedValue('stop-failure') };

    await expect(tryStop('leader', handle, logger)).resolves.toBeUndefined();
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

  it('contains logger failure while reporting a close error', async () => {
    const logger = makeFakeLogger();
    logger.warn.mockImplementation(() => { throw new Error('logger-failure'); });
    const server = { close: jest.fn((cb) => cb(new Error('close-failure'))) };

    await expect(tryClose('HTTP server', server, logger)).resolves.toBeUndefined();
  });

  it('contains a synchronous server.close throw', async () => {
    const logger = makeFakeLogger();
    const server = { close: jest.fn(() => { throw new Error('close-failure'); }) };

    await expect(tryClose('HTTP server', server, logger)).resolves.toBeUndefined();
    expect(logger.warn).toHaveBeenCalledWith('HTTP server close reported error', {
      error: 'close-failure',
      stack: expect.any(String),
    });
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
    expect(deps.exit).toHaveBeenCalledTimes(1);
  });

  it('still forces exit when push-handoff timeout logging throws', async () => {
    const handoffResolvers = {};
    const handoffPromise = new Promise((resolve) => { handoffResolvers.resolve = resolve; });
    const logger = makeFakeLogger();
    logger.error.mockImplementation(() => { throw new Error('logger-failure'); });
    const deps = makeDeps({
      logger,
      gatewayLeader: { pushHandoff: jest.fn().mockReturnValue(handoffPromise) },
    });

    const shutdown = runPushHandoffShutdown({ code: 0, ...deps });
    await new Promise((resolve) => { setImmediate(resolve); });

    expect(() => deps.scheduleHardExit.timers[0].cb()).toThrow('logger-failure');
    expect(deps.exit).toHaveBeenCalledWith(1);

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
