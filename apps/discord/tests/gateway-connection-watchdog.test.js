// Unit tests for src/gateway-connection-watchdog.js — Pillar 3
// connection watchdog that closes the "lock held, WS not connected"
// gap. Pins the load-bearing contracts:
//
//   1. No-op when not holding the lock. Watchdog runs unconditionally;
//      a standby waiting for promotion ticks but does nothing.
//   2. No-op when manager.isConnected(). Steady-state: no work, no log.
//   3. Tries manager.connect() exactly once per failure tick (at-most-
//      one outstanding connect — the design depends on this).
//   4. Attempt counter resets on:
//        - successful connect()
//        - lock-not-held tick (covers give-up-and-reacquire)
//        - manager-already-connected tick
//   5. Exponential backoff sleeps after failure: 200 / 400 / 800 / 1600 ms
//      (capped at 5 s — dead code at maxAttempts=5, see source).
//   6. At attempts >= maxAttempts (default 5): releaseLock() then exit(1).
//      releaseLock failure is logged and swallowed; exit still fires.
//   7. start() is idempotent; stop() halts the loop; post-exit, start()
//      cannot re-enter.
//
// Each contract maps to a production failure mode:
//   - (3) two parallel connect() calls would race @discordjs/ws internal state.
//   - (4) without reset on lock-give-up, a previous task's failure ladder
//     would carry into a re-acquired lock and prematurely exit.
//   - (5)/(6) without bounded retry + exit, a standby that can't reach
//     Discord blocks the only failover slot indefinitely.

const {
  createConnectionWatchdog,
  DEFAULT_POLL_INTERVAL_MS,
  DEFAULT_MAX_ATTEMPTS,
  BACKOFF_BASE_MS,
  BACKOFF_CAP_MS,
} = require('../src/gateway-connection-watchdog');

function makeFakeManager({ initialConnected = false } = {}) {
  let connected = initialConnected;
  return {
    isConnected: jest.fn(() => connected),
    connect: jest.fn(async () => { connected = true; }),
    _setConnected(v) { connected = v; },
  };
}

function makeWatchdog({
  manager,
  isHoldingLock = () => true,
  isConnecting = () => false,
  releaseLock = jest.fn(async () => {}),
  deleteOwnRow,
  pollIntervalMs,
  maxAttempts,
  releaseLockCeilingMs,
  sleep = jest.fn(async () => {}),
  exit = jest.fn(),
} = {}) {
  const logger = {
    info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(),
  };
  const watchdog = createConnectionWatchdog({
    manager: manager ?? makeFakeManager(),
    isHoldingLock, isConnecting, releaseLock, deleteOwnRow, logger,
    pollIntervalMs, maxAttempts, releaseLockCeilingMs, sleep, exit,
  });
  return {
    watchdog, logger, releaseLock, deleteOwnRow, sleep, exit,
  };
}

describe('createConnectionWatchdog — factory validation', () => {
  it('exposes default constants', () => {
    expect(DEFAULT_POLL_INTERVAL_MS).toBe(1_000);
    expect(DEFAULT_MAX_ATTEMPTS).toBe(5);
    expect(BACKOFF_BASE_MS).toBe(100);
    expect(BACKOFF_CAP_MS).toBe(5_000);
  });

  it('BACKOFF_CAP_MS stays above the natural ceiling at DEFAULT_MAX_ATTEMPTS', () => {
    // Source comment names this the "dead-code branch": the highest
    // backoff that actually sleeps is at `attempts = maxAttempts - 1`
    // (the next failure tips into the exhaustion-exit path before
    // sleeping). At default maxAttempts=5 that's 2^4 * 100 = 1600 ms,
    // well under the 5000 cap. Pin the inequality so a future bump
    // to maxAttempts that pushes the natural backoff past the cap
    // surfaces here rather than silently truncating the failure
    // ladder.
    const naturalCeiling = (2 ** (DEFAULT_MAX_ATTEMPTS - 1)) * BACKOFF_BASE_MS;
    // At maxAttempts=5: ceiling = 16 * 100 = 1600. Cap 5000 > 1600.
    expect(BACKOFF_CAP_MS).toBeGreaterThan(naturalCeiling);
  });

  it('throws when manager lacks required methods', () => {
    expect(() => createConnectionWatchdog()).toThrow(/manager.*connect.*isConnected/);
    expect(() => createConnectionWatchdog({ manager: {} })).toThrow(/manager.*connect.*isConnected/);
    expect(() => createConnectionWatchdog({ manager: { connect: () => {} } }))
      .toThrow(/manager.*connect.*isConnected/);
  });

  it('throws when isHoldingLock / isConnecting / releaseLock / logger are missing', () => {
    const manager = makeFakeManager();
    expect(() => createConnectionWatchdog({ manager })).toThrow(/isHoldingLock/);
    expect(() => createConnectionWatchdog({
      manager, isHoldingLock: () => true,
    })).toThrow(/isConnecting/);
    expect(() => createConnectionWatchdog({
      manager, isHoldingLock: () => true, isConnecting: () => false,
    })).toThrow(/releaseLock/);
    expect(() => createConnectionWatchdog({
      manager, isHoldingLock: () => true, isConnecting: () => false,
      releaseLock: async () => {},
    })).toThrow(/logger is required/);
  });
});

describe('step() — no-op paths', () => {
  it('does nothing when not holding the lock', async () => {
    const manager = makeFakeManager();
    const { watchdog } = makeWatchdog({ manager, isHoldingLock: () => false });

    await watchdog._stepForTest();

    expect(manager.connect).not.toHaveBeenCalled();
    expect(manager.isConnected).not.toHaveBeenCalled();
    expect(watchdog._getAttemptsForTest()).toBe(0);
  });

  it('does nothing when manager.isConnected() returns true', async () => {
    const manager = makeFakeManager({ initialConnected: true });
    const { watchdog, sleep } = makeWatchdog({ manager });

    await watchdog._stepForTest();

    expect(manager.connect).not.toHaveBeenCalled();
    expect(sleep).not.toHaveBeenCalled();
    expect(watchdog._getAttemptsForTest()).toBe(0);
  });

  it('resets attempts when transitioning lock-held → lock-not-held mid-ladder', async () => {
    let holding = true;
    const manager = makeFakeManager();
    manager.connect.mockRejectedValue(new Error('fail'));
    const { watchdog, sleep } = makeWatchdog({
      manager, isHoldingLock: () => holding, maxAttempts: 10, sleep: jest.fn(async () => {}),
    });

    await watchdog._stepForTest();
    await watchdog._stepForTest();
    expect(watchdog._getAttemptsForTest()).toBe(2);

    // Give up the lock — next tick must reset.
    holding = false;
    await watchdog._stepForTest();
    expect(watchdog._getAttemptsForTest()).toBe(0);
    // sleep was called on each of the 2 failures, not on the no-op tick.
    expect(sleep).toHaveBeenCalledTimes(2);
  });

  it('resets attempts when manager reconnects between ticks', async () => {
    const manager = makeFakeManager();
    manager.connect.mockRejectedValue(new Error('fail'));
    const { watchdog } = makeWatchdog({ manager, maxAttempts: 10 });

    await watchdog._stepForTest();
    await watchdog._stepForTest();
    expect(watchdog._getAttemptsForTest()).toBe(2);

    // Manager reconnected on its own (e.g., a successful out-of-band
    // connect from elsewhere). Next tick must reset.
    manager._setConnected(true);
    await watchdog._stepForTest();
    expect(watchdog._getAttemptsForTest()).toBe(0);
  });
});

describe('step() — isConnecting backoff (race with leader inbound-handoff)', () => {
  it('does NOT call manager.connect() when leader reports isConnecting=true', async () => {
    // Inbound-handoff path: leader is awaiting `manager.connect()`.
    // Watchdog tick observes !isConnected() and would normally
    // fire its own connect — that would race against the same
    // WebSocketManager. The isConnecting hook gates this off.
    const manager = makeFakeManager();
    const isConnecting = jest.fn(() => true);
    const { watchdog } = makeWatchdog({ manager, isConnecting });

    await watchdog._stepForTest();

    expect(manager.connect).not.toHaveBeenCalled();
    expect(isConnecting).toHaveBeenCalled();
    expect(watchdog._getAttemptsForTest()).toBe(0);
  });

  it('resets attempts when leader transitions to isConnecting=true mid-ladder', async () => {
    // Failure ladder is at attempt 3; leader then takes over the
    // connect (inbound-handoff). The watchdog must reset attempts
    // so the leader's eventual outcome doesn't carry a stale
    // ladder into the next watchdog-driven retry.
    const manager = makeFakeManager();
    manager.connect.mockRejectedValue(new Error('fail'));
    let leaderConnecting = false;
    const { watchdog } = makeWatchdog({
      manager, isConnecting: () => leaderConnecting, maxAttempts: 10,
    });
    await watchdog._stepForTest();
    await watchdog._stepForTest();
    await watchdog._stepForTest();
    expect(watchdog._getAttemptsForTest()).toBe(3);

    leaderConnecting = true;
    await watchdog._stepForTest();
    expect(watchdog._getAttemptsForTest()).toBe(0);
    expect(manager.connect).toHaveBeenCalledTimes(3); // not 4
  });
});

describe('step() — connect retries', () => {
  it('calls manager.connect() when lock held + not connected', async () => {
    const manager = makeFakeManager();
    const { watchdog } = makeWatchdog({ manager });

    await watchdog._stepForTest();

    expect(manager.connect).toHaveBeenCalledTimes(1);
    expect(watchdog._getAttemptsForTest()).toBe(0); // reset after success
  });

  it('resets attempts on successful connect after prior failures', async () => {
    const manager = makeFakeManager();
    let nthCall = 0;
    manager.connect.mockImplementation(async () => {
      nthCall += 1;
      if (nthCall < 3) throw new Error('transient');
      // Third call succeeds.
      manager._setConnected(true);
    });
    const { watchdog } = makeWatchdog({ manager, maxAttempts: 10 });

    await watchdog._stepForTest();
    await watchdog._stepForTest();
    expect(watchdog._getAttemptsForTest()).toBe(2);
    await watchdog._stepForTest();
    expect(watchdog._getAttemptsForTest()).toBe(0);
  });

  it('backs off 200/400/800/1600 ms on attempts 1..4', async () => {
    const manager = makeFakeManager();
    manager.connect.mockRejectedValue(new Error('fail'));
    const sleep = jest.fn(async () => {});
    const { watchdog } = makeWatchdog({ manager, maxAttempts: 10, sleep });

    await watchdog._stepForTest(); // attempt 1 → 200 ms
    await watchdog._stepForTest(); // attempt 2 → 400 ms
    await watchdog._stepForTest(); // attempt 3 → 800 ms
    await watchdog._stepForTest(); // attempt 4 → 1600 ms

    expect(sleep).toHaveBeenNthCalledWith(1, 200);
    expect(sleep).toHaveBeenNthCalledWith(2, 400);
    expect(sleep).toHaveBeenNthCalledWith(3, 800);
    expect(sleep).toHaveBeenNthCalledWith(4, 1_600);
  });

  it('caps backoff at 5000 ms (dead-code branch — pins the cap for future maxAttempts bumps)', async () => {
    // With maxAttempts=10 and BACKOFF_BASE=100, attempt 7 would be
    // 2^7 * 100 = 12_800 ms → capped at 5000. Validates the cap.
    const manager = makeFakeManager();
    manager.connect.mockRejectedValue(new Error('fail'));
    const sleep = jest.fn(async () => {});
    const { watchdog } = makeWatchdog({ manager, maxAttempts: 10, sleep });

    for (let i = 0; i < 7; i += 1) {
      // eslint-disable-next-line no-await-in-loop
      await watchdog._stepForTest();
    }
    // Last call (attempt 7) should hit the cap.
    expect(sleep).toHaveBeenLastCalledWith(5_000);
  });

  it('logs each failed-connect attempt with attempts + backoffMs', async () => {
    const manager = makeFakeManager();
    manager.connect.mockRejectedValue(new Error('econnrefused'));
    const { watchdog, logger } = makeWatchdog({ manager, maxAttempts: 10 });

    await watchdog._stepForTest();
    expect(logger.warn).toHaveBeenCalledWith(
      'connection-watchdog: connect failed, backing off',
      expect.objectContaining({ attempts: 1, backoffMs: 200 }),
    );
  });
});

describe('step() — exhaustion path', () => {
  it('releases the lock and exits(1) when attempts reaches maxAttempts', async () => {
    const manager = makeFakeManager();
    manager.connect.mockRejectedValue(new Error('persistent-fail'));
    const releaseLock = jest.fn(async () => {});
    const exit = jest.fn();
    const sleep = jest.fn(async () => {});
    const { watchdog, logger } = makeWatchdog({
      manager, releaseLock, exit, sleep, maxAttempts: 5,
    });

    // 5 failed attempts.
    for (let i = 0; i < 5; i += 1) {
      // eslint-disable-next-line no-await-in-loop
      await watchdog._stepForTest();
    }

    expect(manager.connect).toHaveBeenCalledTimes(5);
    expect(releaseLock).toHaveBeenCalledTimes(1);
    expect(exit).toHaveBeenCalledWith(1);
    expect(logger.error).toHaveBeenCalledWith(
      'connection-watchdog: connect retries exhausted, releasing lock',
      expect.objectContaining({ attempts: 5 }),
    );
    // On the exhaustion tick, the backoff sleep MUST NOT fire — the
    // exit path supersedes it. So sleep only fired on attempts 1..4.
    expect(sleep).toHaveBeenCalledTimes(4);
  });

  it('still exits(1) when releaseLock itself throws', async () => {
    const manager = makeFakeManager();
    manager.connect.mockRejectedValue(new Error('fail'));
    const releaseLock = jest.fn(async () => { throw new Error('ddb-blip'); });
    const exit = jest.fn();
    const { watchdog, logger } = makeWatchdog({
      manager, releaseLock, exit, maxAttempts: 5,
    });

    for (let i = 0; i < 5; i += 1) {
      // eslint-disable-next-line no-await-in-loop
      await watchdog._stepForTest();
    }

    expect(exit).toHaveBeenCalledWith(1);
    expect(logger.error).toHaveBeenCalledWith(
      'connection-watchdog: releaseLock failed during exhaustion-exit',
      expect.objectContaining({ error: 'ddb-blip' }),
    );
  });

  it('calls deleteOwnRow on exhaustion when hook is provided (symmetric to pushHandoff)', async () => {
    const manager = makeFakeManager();
    manager.connect.mockRejectedValue(new Error('persistent-fail'));
    const releaseLock = jest.fn(async () => {});
    const deleteOwnRow = jest.fn(async () => {});
    const exit = jest.fn();
    const { watchdog } = makeWatchdog({
      manager, releaseLock, deleteOwnRow, exit, maxAttempts: 3,
    });

    for (let i = 0; i < 3; i += 1) {
      // eslint-disable-next-line no-await-in-loop
      await watchdog._stepForTest();
    }

    expect(releaseLock).toHaveBeenCalledTimes(1);
    expect(deleteOwnRow).toHaveBeenCalledTimes(1);
    expect(exit).toHaveBeenCalledWith(1);
  });

  it('still exits(1) when deleteOwnRow throws (logged and swallowed)', async () => {
    const manager = makeFakeManager();
    manager.connect.mockRejectedValue(new Error('fail'));
    const releaseLock = jest.fn(async () => {});
    const deleteOwnRow = jest.fn(async () => { throw new Error('ddb-blip'); });
    const exit = jest.fn();
    const { watchdog, logger } = makeWatchdog({
      manager, releaseLock, deleteOwnRow, exit, maxAttempts: 3,
    });

    for (let i = 0; i < 3; i += 1) {
      // eslint-disable-next-line no-await-in-loop
      await watchdog._stepForTest();
    }

    expect(exit).toHaveBeenCalledWith(1);
    expect(logger.warn).toHaveBeenCalledWith(
      'connection-watchdog: deleteOwnRow failed during exhaustion-exit',
      expect.objectContaining({ error: 'ddb-blip' }),
    );
  });

  it('omits deleteOwnRow call when hook not provided (back-compat with leader-less wiring)', async () => {
    const manager = makeFakeManager();
    manager.connect.mockRejectedValue(new Error('fail'));
    const releaseLock = jest.fn(async () => {});
    const exit = jest.fn();
    // No deleteOwnRow injected.
    const { watchdog } = makeWatchdog({
      manager, releaseLock, exit, maxAttempts: 3,
    });

    for (let i = 0; i < 3; i += 1) {
      // eslint-disable-next-line no-await-in-loop
      await watchdog._stepForTest();
    }
    expect(exit).toHaveBeenCalledWith(1);
  });

  it('exits(1) even when releaseLock hangs (Promise.race ceiling kicks in)', async () => {
    // Defense vs the inbound-handoff-hung-on-manager.connect path
    // (see #415). releaseLockForImmediateExit awaits through the
    // leader's serialization chain, which can be stuck behind a
    // hung connect. Without the race, exit(1) would never fire and
    // the failover slot stays held with no live gateway. Inject a
    // tiny ceiling (10ms) so the test runs in real time.
    const manager = makeFakeManager();
    manager.connect.mockRejectedValue(new Error('persistent-fail'));
    // releaseLock returns a never-resolving promise — simulates the
    // serialization chain being stuck behind a hung op.
    const releaseLock = jest.fn(() => new Promise(() => {}));
    const exit = jest.fn();
    const { watchdog, logger } = makeWatchdog({
      manager, releaseLock, exit, maxAttempts: 3, releaseLockCeilingMs: 10,
    });

    for (let i = 0; i < 3; i += 1) {
      // eslint-disable-next-line no-await-in-loop
      await watchdog._stepForTest();
    }

    expect(exit).toHaveBeenCalledWith(1);
    expect(logger.error).toHaveBeenCalledWith(
      'connection-watchdog: releaseLock failed during exhaustion-exit',
      expect.objectContaining({ error: expect.stringMatching(/release_lock_ceiling/) }),
    );
  });

  it('still exits(1) when releaseLock throws AND deleteOwnRow is absent (combined-permutation pin)', async () => {
    // Pin the cross-product the individual tests don't cover. Both
    // best-effort awaits are independent, but a future refactor that
    // accidentally wires them sequentially (one throw aborting the
    // other) would still need to surface exit(1).
    const manager = makeFakeManager();
    manager.connect.mockRejectedValue(new Error('fail'));
    const releaseLock = jest.fn(async () => { throw new Error('ddb-blip'); });
    const exit = jest.fn();
    const { watchdog, logger } = makeWatchdog({
      manager, releaseLock, exit, maxAttempts: 3,
      // deleteOwnRow deliberately omitted.
    });

    for (let i = 0; i < 3; i += 1) {
      // eslint-disable-next-line no-await-in-loop
      await watchdog._stepForTest();
    }
    expect(exit).toHaveBeenCalledWith(1);
    expect(logger.error).toHaveBeenCalledWith(
      'connection-watchdog: releaseLock failed during exhaustion-exit',
      expect.objectContaining({ error: 'ddb-blip' }),
    );
    // No deleteOwnRow log because hook wasn't provided.
    expect(logger.warn).not.toHaveBeenCalledWith(
      'connection-watchdog: deleteOwnRow failed during exhaustion-exit',
      expect.anything(),
    );
  });

  it('does not re-enter start() after exhaustion-exit', async () => {
    const manager = makeFakeManager();
    manager.connect.mockRejectedValue(new Error('fail'));
    const exit = jest.fn();
    const { watchdog } = makeWatchdog({
      manager, exit, maxAttempts: 5,
      // Fast poll interval so the next test setup is quick.
      pollIntervalMs: 1,
    });

    for (let i = 0; i < 5; i += 1) {
      // eslint-disable-next-line no-await-in-loop
      await watchdog._stepForTest();
    }
    expect(exit).toHaveBeenCalledWith(1);

    // Calling start() after exhaustion is a no-op (the watchdog is
    // dead; the process is supposed to be exiting).
    watchdog.start();
    expect(watchdog._getRunningForTest()).toBe(false);
  });
});

describe('loop backstop — survives unexpected throws from step()', () => {
  it('an isConnected() throw does not exit the loop (logs + retries next tick)', async () => {
    // Contractually `manager.isConnected()` is non-throwing, but a
    // future shim refactor could regress. Without a backstop, the
    // first throw would reject the loop's `await step()` and
    // silently disable the watchdog. The loop must keep running.
    const manager = makeFakeManager();
    let nthCall = 0;
    manager.isConnected = jest.fn(() => {
      nthCall += 1;
      if (nthCall === 1) throw new Error('shim-regression');
      return true;
    });
    const sleepResolvers = [];
    const sleep = jest.fn(() => new Promise((resolve) => { sleepResolvers.push(resolve); }));
    const { watchdog, logger } = makeWatchdog({
      manager, sleep, pollIntervalMs: 1,
    });

    watchdog.start();
    await flushMicrotasks();
    expect(sleep).toHaveBeenCalledTimes(1);

    // Wake the loop for tick #1 — isConnected throws.
    sleepResolvers[0]();
    await flushMicrotasks();
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/step threw unexpectedly/),
      expect.objectContaining({ error: 'shim-regression' }),
    );
    // Loop must have scheduled the next sleep.
    expect(sleep).toHaveBeenCalledTimes(2);

    // Wake tick #2 — isConnected succeeds (returns true).
    sleepResolvers[1]();
    await flushMicrotasks();
    expect(manager.isConnected).toHaveBeenCalledTimes(2);

    watchdog.stop();
    sleepResolvers[2]();
  });
});

describe('start() / stop() lifecycle', () => {
  it('start() schedules ticks via the injected sleep + stop() halts the loop', async () => {
    const manager = makeFakeManager({ initialConnected: true });
    const sleepResolvers = [];
    // Make sleep a controllable promise — resolve it when the test
    // wants the next tick to fire.
    const sleep = jest.fn(() => new Promise((resolve) => { sleepResolvers.push(resolve); }));
    const { watchdog } = makeWatchdog({ manager, sleep });

    watchdog.start();
    // First sleep is queued before the first step.
    await flushMicrotasks();
    expect(sleep).toHaveBeenCalledTimes(1);

    // Release the first poll: a step runs (no-op, isConnected=true)
    // then the next poll-sleep is queued.
    sleepResolvers[0]();
    await flushMicrotasks();
    expect(manager.isConnected).toHaveBeenCalledTimes(1);
    expect(sleep).toHaveBeenCalledTimes(2);

    watchdog.stop();
    sleepResolvers[1](); // wake the loop so it observes running=false and exits
    await flushMicrotasks();
    expect(watchdog._getRunningForTest()).toBe(false);
  });

  it('start() is idempotent', async () => {
    const manager = makeFakeManager({ initialConnected: true });
    const sleep = jest.fn(() => new Promise(() => {})); // never resolves
    const { watchdog } = makeWatchdog({ manager, sleep });

    watchdog.start();
    watchdog.start();
    watchdog.start();
    await flushMicrotasks();
    // Only the first start scheduled a sleep; later starts must no-op.
    expect(sleep).toHaveBeenCalledTimes(1);

    watchdog.stop();
  });

  it('stop() before start() is safe', () => {
    const { watchdog } = makeWatchdog();
    expect(() => watchdog.stop()).not.toThrow();
  });

  it('stop() returns a promise that resolves once the loop exits', async () => {
    const manager = makeFakeManager({ initialConnected: true });
    const sleepResolvers = [];
    const sleep = jest.fn(() => new Promise((resolve) => { sleepResolvers.push(resolve); }));
    const { watchdog } = makeWatchdog({ manager, sleep });
    watchdog.start();
    await flushMicrotasks();

    const stopPromise = watchdog.stop();
    sleepResolvers[0](); // wake the loop so it can observe running=false and exit
    await stopPromise;

    expect(watchdog._getRunningForTest()).toBe(false);
  });

  it('start() after stop() without awaiting does NOT orphan a second loop', async () => {
    // Re-start safety: caller must await stop() before start();
    // a naked start() during the wind-down window is a no-op.
    const manager = makeFakeManager({ initialConnected: true });
    const sleepResolvers = [];
    const sleep = jest.fn(() => new Promise((resolve) => { sleepResolvers.push(resolve); }));
    const { watchdog } = makeWatchdog({ manager, sleep });

    watchdog.start();
    await flushMicrotasks();
    expect(sleep).toHaveBeenCalledTimes(1);

    watchdog.stop();
    watchdog.start(); // no-op — old loop still pending
    await flushMicrotasks();
    expect(sleep).toHaveBeenCalledTimes(1);

    // Wake old loop so it exits, then a fresh start is allowed.
    sleepResolvers[0]();
    await flushMicrotasks();
    watchdog.start();
    await flushMicrotasks();
    expect(sleep).toHaveBeenCalledTimes(2);
  });
});

// Lets the queued microtasks (await sleep/connect resolutions) flush
// before the next assertion.
function flushMicrotasks() {
  return new Promise((resolve) => { setImmediate(resolve); });
}
