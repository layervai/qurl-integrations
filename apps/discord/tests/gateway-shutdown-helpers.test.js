const {
  shouldUsePushHandoffShutdown,
  selectGatewayReadinessProbe,
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
    // SIGTERM-during-startup window: the user-facing CTL+C or ECS
    // task-replacement signal can fire before startHotStandby has
    // constructed the leader. Falling back to gracefulShutdown is
    // correct — there's no lock to push.
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
    // The lock can flip between SIGTERM landings if an inbound
    // handoff already moved ownership onto our peer. A first call
    // returning true must not lock in that answer.
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
    // ENABLE_GATEWAY_RESUME=true but no shim was constructed —
    // happens on the http tier where the flag is uniform across
    // task defs but only the gateway role actually constructs the
    // shim. client.isReady() preserves legacy behavior there.
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
    // The probe is wired BEFORE startHotStandby runs; until the
    // leader is assigned the probe must report unhealthy so --start-period
    // covers the gap.
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
    // Tick-loop liveness is NOT consulted on the active path.
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
    // The standby reports tick-loop liveness, NOT WS-readiness —
    // otherwise it would 503 forever and ECS would replace it.
    expect(gatewayShim.isReady).not.toHaveBeenCalled();
    expect(leader.hasStartedTickLoop).toHaveBeenCalled();
  });

  it('hot-standby + standby with dead tick loop: probe returns false', () => {
    // The tick loop dying is the standby's failure mode — the
    // probe flipping to false here is the load-bearing signal
    // that lets ECS replace the standby task.
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
    // The active/standby flip happens between requests — the
    // outgoing leader pushHandoffs to the peer, the peer adopts
    // the lock. If the probe captured the leader handle at wire
    // time, it would keep reporting the stale role. The callback
    // indirection means every probe firing re-reads the current
    // module-level reference.
    let currentLeader = null;
    const probe = selectGatewayReadinessProbe({
      enableHotStandby: true,
      enableGatewayResume: true,
      gatewayShim: { isReady: jest.fn(() => true) },
      getGatewayLeader: () => currentLeader,
      client: { isReady: jest.fn() },
    });

    // Before startHotStandby: leader null → false.
    expect(probe()).toBe(false);

    // After startHotStandby completes: leader set, holding lock.
    currentLeader = makeFakeLeader({ holdingLock: true });
    expect(probe()).toBe(true);

    // After inbound handoff transfers ownership away: now standby.
    currentLeader = makeFakeLeader({ holdingLock: false, ticking: true });
    expect(probe()).toBe(true); // standby is still healthy via tick loop
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
    // Teardown is already on the failure path; one component's
    // stop() error shouldn't stall the rest of the drain (which
    // is why we wrap each in tryStop rather than chaining bare
    // awaits). Stack is included for triage on a stuck drain
    // where the message alone doesn't tell the operator which
    // call site threw.
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
  // Captures every scheduleHardExit call so a test can fire the
  // pending timer callback to simulate the ceiling elapsing.
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

  it('on a thrown pushHandoff, exits with forcedExitCode so deploy metrics distinguish clean transfer from throw', async () => {
    // Three observable outcomes, three exit codes:
    //   * clean transfer       → exit(code)            (typically 0)
    //   * pushHandoff threw    → exit(forcedExitCode)  (defaults to 1)
    //   * pushHandoff timed out → exit(forcedExitCode)  (via hard-exit timer)
    // Collapsing throw + clean would hide deploy-time peer-reachability
    // failures behind clean-transfer SLI metrics.
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
    // In prod process.exit kills the process so the .unref'd timer
    // is moot. But a non-terminal injected exit (tests, future
    // metric-emitting wrapper) would observe a spurious exit-code-1
    // ~12 s later without the clear.
    const deps = makeDeps();
    await runPushHandoffShutdown({ code: 0, ...deps });
    expect(deps.clearHardExit).toHaveBeenCalledTimes(1);
    expect(deps.clearHardExit).toHaveBeenCalledWith(deps.scheduleHardExit.timers[0]);
    // Sanity: exit fired exactly once (the success-path exit), NOT
    // a second time from the timer callback.
    expect(deps.exit).toHaveBeenCalledTimes(1);
    expect(deps.exit).toHaveBeenCalledWith(0);
  });

  it('clears the hard-exit timer on the throw path too (and exits with forcedExitCode)', async () => {
    const deps = makeDeps({
      gatewayLeader: { pushHandoff: jest.fn().mockRejectedValue(new Error('peer down')) },
    });
    await runPushHandoffShutdown({ code: 0, ...deps });
    expect(deps.clearHardExit).toHaveBeenCalledTimes(1);
    // Assert the exit code here too (not just call count) so a
    // regression that swaps the throw path back to `code` is caught
    // by this test independently of the dedicated throw-code test.
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
    // Load-bearing contract — dashboards/ECS need to distinguish
    // "clean transfer, exit 0" from "timeout, standby cold-acquired,
    // exit 1" so a stuck handoff doesn't masquerade as a clean
    // shutdown in the deploy metrics.
    const handoffResolvers = {};
    const handoffPromise = new Promise((resolve) => { handoffResolvers.resolve = resolve; });
    const deps = makeDeps({
      gatewayLeader: { pushHandoff: jest.fn().mockReturnValue(handoffPromise) },
    });

    const shutdown = runPushHandoffShutdown({ code: 0, ...deps });
    // Yield once so scheduleHardExit has been called.
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(deps.scheduleHardExit.timers).toHaveLength(1);

    // Fire the timer callback synchronously — represents the 12s
    // ceiling elapsing in real time.
    deps.scheduleHardExit.timers[0].cb();
    expect(deps.exit).toHaveBeenCalledWith(1);
    expect(deps.logger.error).toHaveBeenCalledWith('PushHandoff shutdown timed out, forcing exit');

    // Release the never-resolving handoff so the orphan shutdown
    // promise settles, then await it so Jest doesn't warn about
    // an open async-context.
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
    // The active received Discord dispatches that may be in-flight to
    // SQS. The standby cannot replay these — they arrived on OUR
    // WebSocket. The contract is concurrent (not sequential) so
    // publisher's DRAIN_DEADLINE_MS doesn't extend the pushHandoff
    // critical path. Prove the ordering with a pending pushHandoff:
    // publisher.stop must already have been invoked by the time the
    // test releases pushHandoff.
    const handoffResolvers = {};
    const handoffPromise = new Promise((resolve) => { handoffResolvers.resolve = resolve; });
    const eventPublisher = { stop: jest.fn().mockResolvedValue(undefined) };
    const deps = makeDeps({
      eventPublisher,
      gatewayLeader: { pushHandoff: jest.fn().mockReturnValue(handoffPromise) },
    });

    const shutdownPromise = runPushHandoffShutdown({ code: 0, ...deps });
    // Yield the microtask queue once so the helper's body runs up to
    // the `await gatewayLeader.pushHandoff()` await point. The
    // publisher drain is kicked off synchronously before that await,
    // so the stop spy must already have fired.
    await new Promise((resolve) => { setImmediate(resolve); });
    expect(eventPublisher.stop).toHaveBeenCalledTimes(1);
    expect(deps.exit).not.toHaveBeenCalled(); // pushHandoff still pending

    handoffResolvers.resolve({ transferred: true, pushAcked: true });
    await shutdownPromise;
    expect(deps.exit).toHaveBeenCalledWith(0);
  });

  it('eventPublisher omitted is fine (legacy / flag-off / test setups)', async () => {
    const deps = makeDeps();
    // Default makeDeps doesn't include eventPublisher — verify the
    // helper doesn't blow up trying to call .stop() on null.
    await runPushHandoffShutdown({ code: 0, ...deps });
    expect(deps.exit).toHaveBeenCalledWith(0);
  });

  it('eventPublisher.stop() failure is absorbed via tryStop (not propagated)', async () => {
    // The SIGTERM handler invokes pushHandoffShutdown asynchronously
    // (awaited); an unhandled-rejection bubble from the publisher
    // drain would be a runtime hazard. tryStop catches both sync
    // throws (async-function semantics) and async rejects.
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
