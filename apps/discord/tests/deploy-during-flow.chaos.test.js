// Pillar 3 chaos validation — SIGTERM mid-flow (deploy-during-flow).
//
// Composes REAL primitives at the seams the existing unit/integration
// tests don't reach:
//
//   - real createGatewayLock against a mockClient(docClient) so DDB
//     row-state transitions (instance_id flip, version bump, expires_at
//     refresh) are asserted against the actual UpdateCommand inputs,
//     not a `jest.fn().mockResolvedValue(...)` stub.
//   - real createPeerHeartbeat so listFreshPeers' Scan + filters land
//     on the real shape (the unit test for the leader bypasses this
//     via `peerHeartbeat: { listFreshPeers: () => [...] }`).
//   - real createGatewayLeader with the full runSerialized chain and
//     the pushHandoff body; the existing leader unit test exercises
//     individual ops, not the SIGTERM-through-handoff trajectory.
//   - real runPushHandoffShutdown so the eventPublisher-drain-in-
//     parallel contract is exercised end-to-end, not just the
//     gatewayLeader mock's resolve.
//
// Three regression modes this guards against:
//
//   1. A future refactor wires flow-state writes into the gateway
//      tier's SIGTERM path. The design forbids it: writes to
//      qurl_bot_flow_state run on the WORKER tier, which is a
//      separate process — gateway-shutdown-helpers.js calls this out
//      explicitly in the runPushHandoffShutdown header. The Table-
//      Name allowlist assertion below catches the regression at PR
//      time via post-hoc inspection of `ddbMock.commandCalls()` —
//      unrouted commands silently resolve, so the protection is the
//      end-of-test assertion, not a runtime throw.
//
//   2. A future refactor accidentally serializes eventPublisher.stop()
//      AFTER pushHandoff instead of in parallel. The publisher's
//      DRAIN_DEADLINE_MS (3s default) would then extend the SIGTERM
//      critical path past the 12s ceiling. Asserts both run in parallel
//      via call-order spies: eventPublisher.stop() is observed to enter
//      before pushHandoff's await landed, not via wall-clock measurement
//      (the latter is too noisy to distinguish parallel from serialized
//      at the timescales mocked-DDB pushHandoff runs at).
//
//   3. A future refactor changes pushHandoff to skip releaseLockBestEffort
//      on the no_peer path. The cold-fallback floor would then become
//      ~6s longer (waiting for TTL lapse vs immediate DDB Delete). The
//      no-peer case below asserts the DDB DeleteCommand lands on the
//      lock table within the SIGTERM window.
//
// Out of chaos scope (covered by unit tests, intentionally not duplicated):
//   - pushHandoff throwing synchronously (handoffThrew path → forcedExitCode):
//     runPushHandoffShutdown's unit tests pin the exit-code semantics;
//     test #3 below covers the symmetric hung-controlClient timeout path.
//   - exact backoff-ladder timing (200/400/800/1600 ms): the watchdog's
//     unit test pins the cadence; chaos tests stub sleep to a no-op.
//   - per-attempt log shape: also in unit tests.

const { createGatewayLock } = require('../src/gateway-lock');
const { createPeerHeartbeat } = require('../src/gateway-peer-heartbeat');
const { createGatewayLeader } = require('../src/gateway-leader');
const { runPushHandoffShutdown } = require('../src/gateway-shutdown-helpers');
const {
  setupChaosDdb, makeChaosLogger,
  LOCK_TABLE, HEARTBEAT_TABLE, SHARD_ID,
  INSTANCE_A, INSTANCE_B, HOLDER_A, HOLDER_B,
  MAX_MICROTASK_YIELDS, assertNoUnexpectedTableCalls,
} = require('./helpers/chaos-ddb');

function makeFakeManager() {
  return {
    isConnected: jest.fn(() => true),
    connect: jest.fn(async () => {}),
  };
}

function makeFakeControlClient() {
  return {
    pushHandoff: jest.fn().mockResolvedValue({ ok: true }),
  };
}

// A publisher whose stop() resolves after the test releases it. The
// caller observes WHEN stop() was entered (relative to other calls)
// via the spy's call-order, then releases the pending promise so the
// parallel drain in runPushHandoffShutdown's `await drainPromise`
// resolves before exit().
function makeControllableEventPublisher() {
  let resolveStop;
  const stopped = new Promise((resolve) => { resolveStop = resolve; });
  return {
    stop: jest.fn().mockImplementation(() => stopped),
    releaseStop: () => resolveStop(),
  };
}

function makeScheduleHardExit() {
  const timers = [];
  const schedule = jest.fn((cb, ms) => {
    const timer = { cb, ms, unref: jest.fn(), cleared: false };
    timers.push(timer);
    return timer;
  });
  const clearHardExit = jest.fn((timer) => {
    if (timer) timer.cleared = true;
  });
  return { schedule, clearHardExit, timers };
}

// Assembles a fully wired leader against the real lock + heartbeat
// primitives and the mock control-client/manager. clock is a mutable
// closure so peer rows can be seeded with `updated_at` in the
// freshness window for the SIGTERM-time clock value. controlClient is
// optional — defaults to an always-ACK fake; test #3 injects a hung
// stub to exercise the timeout path without re-implementing the
// surrounding wiring. Hard-codes the from-A perspective (instanceId:
// INSTANCE_A, lockHolder: HOLDER_A) since every SIGTERM chaos
// scenario here drives the SIGTERM-on-A leg of the handoff; a future
// chaos test that needs the from-B viewpoint will need a `selfInstance`
// knob added here.
function assembleLeader({ docClient, clock, controlClient } = {}) {
  const logger = makeChaosLogger();
  const lock = createGatewayLock({
    ddbClient: docClient,
    tableName: LOCK_TABLE,
    shardId: SHARD_ID,
    instanceId: INSTANCE_A,
    lockHolder: HOLDER_A,
    logger,
    clock,
  });
  const peerHeartbeat = createPeerHeartbeat({
    ddbClient: docClient,
    tableName: HEARTBEAT_TABLE,
    instanceId: INSTANCE_A,
    ip: '10.0.0.10',
    port: 7800,
    shardId: SHARD_ID,
    lockHolder: HOLDER_A,
    logger,
    clock,
  });
  const resolvedControlClient = controlClient ?? makeFakeControlClient();
  const manager = makeFakeManager();
  const leader = createGatewayLeader({
    lock,
    peerHeartbeat,
    controlClient: resolvedControlClient,
    manager,
    selfInstanceId: INSTANCE_A,
    shardId: SHARD_ID,
    logger,
    tickIntervalMs: 1_000,
  });
  return {
    leader, lock, peerHeartbeat, controlClient: resolvedControlClient, manager, logger,
  };
}

describe('Pillar 3 chaos — deploy-during-flow (SIGTERM mid-handoff)', () => {
  let now = 1_700_000_000_000;
  const clock = () => now;

  beforeEach(() => { now = 1_700_000_000_000; });
  // Cheap defense against an un-restored spy leaking between tests
  // (jest.spyOn(lock, 'transferLock') in test #1 below). Works today
  // because assembleLeader() builds a fresh leader+lock per test, but
  // if a future refactor pulls assembly into beforeAll, the
  // unrestored spy would leak. Note this does NOT reset the per-test
  // ddbMock — each test gets its own via setupChaosDdb(), so the
  // afterEach is a no-op for the mock client.
  afterEach(() => { jest.restoreAllMocks(); });

  it('SIGTERM with healthy standby peer → transferLock + push ACK + clean exit(0)', async () => {
    // Pre-seed: A holds the lock (version=3), B has a fresh
    // heartbeat row in the same shard inside the 6 s freshness
    // window. A's heartbeat is also present (the production wiring
    // writes one every tick from both replicas).
    const nowSeconds = Math.floor(now / 1000);
    const { docClient, ddbMock, state } = setupChaosDdb({
      initialLockRow: {
        shard_id: SHARD_ID,
        instance_id: INSTANCE_A,
        lock_holder: HOLDER_A,
        version: 3,
        expires_at: nowSeconds + 6,
      },
      initialPeerRows: [
        // A's own row.
        {
          instance_id: INSTANCE_A, ip: '10.0.0.10', port: 7800,
          shard_id: SHARD_ID, updated_at: nowSeconds, expires_at: nowSeconds + 60,
          lock_holder: HOLDER_A,
        },
        // B's row — fresh (updated_at within freshness window).
        {
          instance_id: INSTANCE_B, ip: '10.0.0.20', port: 7800,
          shard_id: SHARD_ID, updated_at: nowSeconds, expires_at: nowSeconds + 60,
          lock_holder: HOLDER_B,
        },
      ],
    });

    const { leader, lock, logger } = assembleLeader({ docClient, clock });
    // handleInboundHandoff is the production path that flips heldLock
    // true; it internally calls lock.adoptLockFromHandoff(expectedVersion)
    // (adopt-then-flag-then-connect ordering). Driving it here primes
    // the lock cursor without running the full tick loop, AND keeps
    // the test honest — if a future refactor removed the internal
    // adopt call, the test would fail (vs. silently passing if we
    // pre-primed the cursor ourselves).
    await leader.handleInboundHandoff({
      activeInstanceId: 'predecessor', expectedVersion: 3,
    });

    // Spy on `lock.transferLock` (the real primitive seam invoked by
    // pushHandoff) so we can observe call ORDER vs eventPublisher.stop.
    // Wall-clock measurement was tried first but can't distinguish
    // parallel from serialized at the timescales mocked-DDB
    // transferLock runs at (single-digit ms); call-order is the right
    // signal. Spying on transferLock (rather than the leader's
    // pushHandoff) keeps the test decoupled from the leader's surface
    // shape — if a future refactor destructures or caches pushHandoff,
    // the closure still holds the same `lock` reference and the spy
    // still fires.
    //
    // Timeline-based check (in addition to "stop was called"): record
    // both `stop()` invoke and `transferLock()` resolve as ordered
    // events, then assert stop-invoke landed BEFORE
    // transferLock-resolve. A regression that serializes the drain
    // (`await pushHandoff(); then stop()`) would resolve transferLock
    // first, then invoke stop — failing the ordering check.
    const timeline = [];
    const origTransferLock = lock.transferLock.bind(lock);
    jest.spyOn(lock, 'transferLock').mockImplementation(async (...args) => {
      const result = await origTransferLock(...args);
      timeline.push('transferLock-resolved');
      return result;
    });
    const baseEventPublisher = makeControllableEventPublisher();
    const eventPublisher = {
      stop: jest.fn(() => {
        timeline.push('stop-invoked');
        return baseEventPublisher.stop();
      }),
      releaseStop: baseEventPublisher.releaseStop,
    };
    const exit = jest.fn();
    const { schedule, clearHardExit, timers } = makeScheduleHardExit();

    const shutdownPromise = runPushHandoffShutdown({
      code: 0, gatewayLeader: leader, eventPublisher, logger,
      exit, scheduleHardExit: schedule, clearHardExit,
    });
    // Release the pending stop() so runPushHandoffShutdown's
    // `await drainPromise` can resolve once it's reached. Order doesn't
    // matter — the timeline observes both events regardless.
    eventPublisher.releaseStop();
    // Drive the full shutdown to completion (pushHandoff body runs,
    // transferLock resolves, drainPromise resolves, exit fires). With
    // mocked DDB this is a few microtask hops; no polling needed.
    await shutdownPromise;

    expect(eventPublisher.stop).toHaveBeenCalledTimes(1);
    expect(lock.transferLock).toHaveBeenCalledTimes(1);

    // Ordering check: stop-invoke must come before transferLock-resolve.
    // Tighter than "both called". A serialized refactor
    // (`await pushHandoff(); then stop()`) would resolve transferLock
    // first, then invoke stop, flipping the order.
    const stopIdx = timeline.indexOf('stop-invoked');
    const transferResolvedIdx = timeline.indexOf('transferLock-resolved');
    expect(stopIdx).toBeGreaterThanOrEqual(0);
    expect(transferResolvedIdx).toBeGreaterThanOrEqual(0);
    expect(stopIdx).toBeLessThan(transferResolvedIdx);

    expect(state.lockRow).not.toBeNull();
    expect(state.lockRow.instance_id).toBe(INSTANCE_B);
    expect(state.lockRow.lock_holder).toBe(HOLDER_B);
    expect(state.lockRow.version).toBe(4);

    const remainingPeerIds = state.peerRows.map((r) => r.instance_id);
    expect(remainingPeerIds).not.toContain(INSTANCE_A);
    expect(remainingPeerIds).toContain(INSTANCE_B);

    expect(exit).toHaveBeenCalledWith(0);
    expect(exit).toHaveBeenCalledTimes(1);
    expect(clearHardExit).toHaveBeenCalledWith(timers[0]);

    assertNoUnexpectedTableCalls(ddbMock);
  });

  it('SIGTERM with no peer (no_peer fallback) → releaseLock + clean exit(0)', async () => {
    const nowSeconds = Math.floor(now / 1000);
    // Only A's own heartbeat row exists. listFreshPeers will filter
    // it out (instance_id !== self), returning []. pushHandoff hits
    // the no_peer branch.
    const { docClient, ddbMock, state } = setupChaosDdb({
      initialLockRow: {
        shard_id: SHARD_ID,
        instance_id: INSTANCE_A,
        lock_holder: HOLDER_A,
        version: 5,
        expires_at: nowSeconds + 6,
      },
      initialPeerRows: [{
        instance_id: INSTANCE_A, ip: '10.0.0.10', port: 7800,
        shard_id: SHARD_ID, updated_at: nowSeconds, expires_at: nowSeconds + 60,
        lock_holder: HOLDER_A,
      }],
    });

    const { leader, controlClient, logger } = assembleLeader({ docClient, clock });
    await leader.handleInboundHandoff({
      activeInstanceId: 'predecessor', expectedVersion: 5,
    });

    const eventPublisher = makeControllableEventPublisher();
    eventPublisher.releaseStop(); // resolve immediately — no-peer path doesn't gate on it.
    const exit = jest.fn();
    const { schedule, clearHardExit } = makeScheduleHardExit();

    await runPushHandoffShutdown({
      code: 0, gatewayLeader: leader, eventPublisher, logger,
      exit, scheduleHardExit: schedule, clearHardExit,
    });

    expect(state.lockRow).toBeNull();
    expect(controlClient.pushHandoff).not.toHaveBeenCalled();
    expect(state.peerRows).toEqual([]);
    // exit(0): no-peer outcome is still "clean" (standby cold-acquires
    // via the TTL floor). exit(0) vs exit(1) distinguishes from the
    // timeout path for deploy SLI.
    expect(exit).toHaveBeenCalledWith(0);
    assertNoUnexpectedTableCalls(ddbMock);
    // Pin the documented no-peer log against a future branch swap
    // (e.g. always-transferLock).
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringContaining('no fresh peer for handoff'),
    );
  });

  it('SIGTERM with hung controlClient.pushHandoff → hard-exit fires with forcedExitCode=1', async () => {
    // Pins the dashboard contract: a stuck push exits with the forced
    // code (1), not the incoming code (0), so the deploy SLI
    // distinguishes "clean transfer + ACK" from "transfer happened but
    // ACK timed out" — same DDB outcome, different operator signal.
    // Lock is ALREADY transferred in DDB before pushHandoff hangs
    // (transferLock is awaited first in gateway-leader's pushHandoff
    // body); the standby's watchdog covers the no-ACK case via
    // lock-held + WS-disconnected within ~1 s.
    const nowSeconds = Math.floor(now / 1000);
    const { docClient, ddbMock, state } = setupChaosDdb({
      initialLockRow: {
        shard_id: SHARD_ID, instance_id: INSTANCE_A, lock_holder: HOLDER_A,
        version: 7, expires_at: nowSeconds + 6,
      },
      initialPeerRows: [
        {
          instance_id: INSTANCE_A, ip: '10.0.0.10', port: 7800,
          shard_id: SHARD_ID, updated_at: nowSeconds, expires_at: nowSeconds + 60,
          lock_holder: HOLDER_A,
        },
        {
          instance_id: INSTANCE_B, ip: '10.0.0.20', port: 7800,
          shard_id: SHARD_ID, updated_at: nowSeconds, expires_at: nowSeconds + 60,
          lock_holder: HOLDER_B,
        },
      ],
    });

    // Override the default ACK'ing controlClient with a hung one —
    // simulates B's process being unresponsive. All other wiring
    // (lock/heartbeat/manager/leader) is the same as the healthy-peer
    // path; reuse assembleLeader so a future ctor-arg addition can't
    // silently miss this test.
    const hungControlClient = {
      pushHandoff: jest.fn(() => new Promise(() => {})),
    };
    const { leader, logger } = assembleLeader({
      docClient, clock, controlClient: hungControlClient,
    });
    await leader.handleInboundHandoff({
      activeInstanceId: 'predecessor', expectedVersion: 7,
    });

    const eventPublisher = makeControllableEventPublisher();
    eventPublisher.releaseStop(); // drain resolves immediately; not what we're testing
    const exit = jest.fn();
    const { schedule, timers, clearHardExit } = makeScheduleHardExit();

    const shutdownPromise = runPushHandoffShutdown({
      code: 0, gatewayLeader: leader, eventPublisher, logger,
      exit, scheduleHardExit: schedule, clearHardExit,
    });
    // Park the orphan shutdown promise immediately — it stays pending
    // forever in prod (process.exit kills it), but in jest a failure
    // in any later assertion would early-return without attaching this
    // handler and the pending promise could surface as an unhandled
    // rejection that masks the real failure. Attach now so any
    // assertion failure below has clean output.
    shutdownPromise.catch(() => {});

    // Wait until transferLock has landed (state.lockRow.instance_id
    // flipped to B). This indicates the pushHandoff body has cleared
    // listFreshPeers + transferLock and is now awaiting the hung
    // controlClient.pushHandoff — the exact state we want to time
    // the hard-exit fire against. setImmediate-counter (vs wall-clock)
    // so the bound doesn't drift with CI runner load: with mocked
    // DDB, transferLock settles in ≤ 3 microtask hops; the
    // MAX_MICROTASK_YIELDS bound is generous defense without
    // depending on Date.now().
    let yieldsForTransfer = 0;
    while (state.lockRow.instance_id !== INSTANCE_B) {
      if (yieldsForTransfer++ >= MAX_MICROTASK_YIELDS) {
        throw new Error(`chaos: transferLock never landed within ${MAX_MICROTASK_YIELDS} event-loop yields`);
      }
      // eslint-disable-next-line no-await-in-loop
      await new Promise((r) => { setImmediate(r); });
    }
    expect(timers).toHaveLength(1);
    // A hard-exit timer was scheduled with a positive deadline; the
    // exact `ceilingMs` value (12 s default) is pinned by
    // gateway-shutdown-helpers' unit test. Asserting a positive bound
    // here keeps the chaos check resilient to a future ceiling tune
    // without losing the "a hard-exit was armed" signal.
    expect(timers[0].ms).toBeGreaterThan(0);
    timers[0].cb();

    // The hard-exit path calls exit(forcedExitCode) and the shutdown
    // promise itself remains pending (its inner await never resolves);
    // that's the prod shape — process.exit kills the process and the
    // pending await is moot.
    expect(exit).toHaveBeenCalledWith(1);

    expect(state.lockRow.instance_id).toBe(INSTANCE_B);
    expect(state.lockRow.version).toBe(8);
    assertNoUnexpectedTableCalls(ddbMock);
    // (shutdownPromise.catch was attached up front; see above.)
  });
});
