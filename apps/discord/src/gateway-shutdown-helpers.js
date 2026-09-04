// Pure helpers for the Pillar 3 hot-standby gateway tier's shutdown
// branching + /health probe selection. Extracted from index.js so the
// contracts are unit-testable without the full bot bootstrap.

// True when this replica's SIGTERM should invoke pushHandoff-then-exit
// (active branch). Reads `gatewayLeader.isHoldingLock()` at call time
// so an inbound handoff that already moved the lock to our peer
// correctly routes us into gracefulShutdown instead.
function shouldUsePushHandoffShutdown({ enableHotStandby, gatewayLeader }) {
  if (!enableHotStandby) return false;
  if (!gatewayLeader) return false;
  return gatewayLeader.isHoldingLock();
}

// Returns the /health readiness probe closure for the gateway tier:
// hot-standby = active reports shim.isReady, standby reports tick-loop
// liveness; Pillar 2 = shim.isReady; legacy = client.isReady.
//
// `getGatewayLeader` is a callback (NOT the handle) because the probe
// is wired before startHotStandby runs, when the leader is still null;
// the callback closes over the caller's mutable binding so each probe
// firing reads the current value.
function selectGatewayReadinessProbe({
  enableHotStandby,
  enableGatewayResume,
  gatewayShim,
  getGatewayLeader,
  client,
}) {
  if (enableHotStandby) {
    // gatewayShim is guaranteed non-null on this branch — the
    // boot-requirements gate rejects hot-standby without RESUME
    // (which requires the shim's construction). No null-check needed.
    return () => {
      const gatewayLeader = getGatewayLeader();
      if (!gatewayLeader) return false;
      if (gatewayLeader.isHoldingLock()) {
        return gatewayShim.isReady();
      }
      return gatewayLeader.hasStartedTickLoop();
    };
  }
  if (enableGatewayResume && gatewayShim) {
    return () => gatewayShim.isReady();
  }
  return () => client.isReady();
}

// Best-effort stop() invoker for shutdown teardown. Null-safe so
// callers can pass a handle that was never constructed (e.g.
// hot-standby off). Failures log at warn and resolve — teardown is
// already on the failure path; a stop() error shouldn't stall the
// rest of the drain.
async function tryStop(name, handle, logger) {
  if (!handle) return;
  try {
    await handle.stop();
  } catch (err) {
    // Include the stack so a stuck SIGTERM drain log has both the
    // symptom and the call site.
    try {
      logger.warn(`${name} stop failed`, {
        error: err?.message ?? String(err),
        stack: err?.stack,
      });
    } catch {
      // Logging is best-effort too: this helper must never re-reject and
      // truncate the remaining teardown when its returned promise is floated.
    }
  }
}

// Waits for an http.Server-shaped handle to fire its `listening`
// event, with three-way unwind: resolve on `listening`, reject on
// `error`, reject on `close`. The `close` clause closes the
// SIGTERM-during-listen-await hang: if gracefulShutdown calls
// `server.close()` while we're still waiting for the listener to
// come up, Node fires `close` (not `error` or `listening`) and the
// Promise would otherwise hang until gracefulShutdown's force-exit
// timer fires.
//
// Mutual listener removal — leaving idle `error → reject` /
// `close → reject` listeners attached after a `listening` resolve
// would surface unhandled rejections on every runtime listener-
// error; the caller's `onListenError` hook routes those to
// gracefulShutdown(1) and we don't need a duplicate path.
function awaitServerListening(server) {
  return new Promise((resolve, reject) => {
    if (server.listening) {
      resolve();
      return;
    }
    const cleanup = () => {
      server.off('listening', onListening);
      server.off('error', onError);
      server.off('close', onClose);
    };
    const onListening = () => { cleanup(); resolve(); };
    const onError = (err) => { cleanup(); reject(err); };
    const onClose = () => { cleanup(); reject(new Error('control-channel server closed before listening')); };
    server.once('listening', onListening);
    server.once('error', onError);
    server.once('close', onClose);
  });
}

// Symmetric to tryStop, but for net.Server-shaped handles (callback-
// based close instead of Promise-based stop). Null-safe, awaits the
// close callback, surfaces any close error at warn. Resolves cleanly
// in every case so teardown doesn't stall.
async function tryClose(name, server, logger) {
  if (!server) return;
  await new Promise(resolve => {
    server.close(err => {
      // Stack alongside message — symmetric with tryStop. Most
      // net.Server.close errors are low-information ("listener
      // already detached"), but the next failure mode introduced
      // here will benefit from the call-site trace.
      if (err) logger.warn(`${name} close reported error`, { error: err.message, stack: err.stack });
      resolve();
    });
  });
}

// Stops the hot-standby control plane in dependency order. The IDENTIFY-fatal
// path calls both stop methods to set their synchronous guards, but must skip
// awaiting tasks that may be parked behind manager.connect() so the
// session-store final flush retains the shutdown budget.
async function stopGatewayHotStandby({
  controlChannelServer,
  connectionWatchdog,
  gatewayLeader,
  awaitConnectionWatchdog = true,
  awaitGatewayLeader = true,
  logger,
}) {
  await tryClose('control-channel server', controlChannelServer, logger);
  const watchdogStopped = tryStop('connection-watchdog', connectionWatchdog, logger);
  if (awaitConnectionWatchdog) await watchdogStopped;
  const leaderStopped = tryStop('gateway-leader', gatewayLeader, logger);
  if (awaitGatewayLeader) await leaderStopped;
}

// Plain graceful-shutdown envelope shared by every non-push exit path. The
// ownership gate and hard-exit timer run synchronously before teardown is
// invoked, so a stalled first cleanup await cannot leave the process healthy
// indefinitely. Dependencies are injected to make that ordering executable in
// tests rather than relying on source-shape assertions in index.js.
async function runGracefulShutdown({
  code = 0,
  claimShutdown,
  teardown,
  logger,
  ceilingMs = 10_000,
  exit = (c) => process.exit(c),
  scheduleHardExit = setTimeout,
  clearHardExit = clearTimeout,
}) {
  if (!claimShutdown()) return;

  let exited = false;
  const exitOnce = (exitCode) => {
    if (exited) return;
    exited = true;
    exit(exitCode);
  };
  const hardExit = scheduleHardExit(() => {
    try {
      logger.error('Shutdown timed out, forcing exit');
    } finally {
      exitOnce(1);
    }
  }, ceilingMs);
  if (hardExit && typeof hardExit.unref === 'function') hardExit.unref();

  logger.info('Graceful shutdown initiated...');
  try {
    await teardown();
  } catch (error) {
    logger.error('Error during shutdown', { error: error?.message ?? String(error) });
  }
  clearHardExit(hardExit);
  exitOnce(code);
}

// Gateway-fatal adapter: arm graceful shutdown's hard-exit backstop before
// asking the connection watchdog to stop. A fatal can arrive from inside
// watchdog.manager.connect(), so its stop is deliberately best-effort and
// non-blocking. This helper also owns the fatal-path teardown option: graceful
// teardown re-invokes the same idempotent stop, but does not await the blocked
// connect before flushing the resumable session.
function runGatewayFatalShutdown({
  gracefulShutdown,
  getConnectionWatchdog,
  logger,
  fatalCeilingMs = 15_000,
  scheduleHardExit = setTimeout,
  exit = (code) => process.exit(code),
}) {
  // Independent of gracefulShutdown's ownership gate: every path that owns
  // shutdown today has its own 10/12-second backstop, but this fatal boundary
  // must remain terminal if a future owner forgets one. Fifteen seconds stays
  // inside ECS's 30-second SIGTERM window and trails both existing ceilings.
  const hardExit = scheduleHardExit(() => {
    try {
      logger.error('Gateway fatal shutdown timed out, forcing exit');
    } finally {
      exit(1);
    }
  }, fatalCeilingMs);
  if (hardExit && typeof hardExit.unref === 'function') hardExit.unref();

  const shutdown = gracefulShutdown(1, {
    awaitConnectionWatchdog: false,
    awaitGatewayLeader: false,
  });
  const connectionWatchdog = getConnectionWatchdog();
  if (!connectionWatchdog) return shutdown;

  const warnStopFailed = (error) => {
    logger.warn('connection-watchdog stop failed during gateway fatal shutdown', {
      error: error?.message ?? String(error),
    });
  };
  try {
    Promise.resolve(connectionWatchdog.stop()).catch(warnStopFailed);
  } catch (error) {
    warnStopFailed(error);
  }
  return shutdown;
}

// Push-handoff SIGTERM body. Distinct from gracefulShutdown because
// the active replica's job is to transfer ownership ASAP — not run
// the full drain (close DB, flush its own session row, etc.) — the
// standby is already doing the steady-state work. Three load-bearing
// skips:
//
//   * gatewayShim.stop() — flushFinal=true would clobber the (newer)
//     session row the standby has advanced past our snapshot.
//   * controlChannelServer close + leader/watchdog stop — leader's
//     `closed=true` sentinel (set by pushHandoff itself) short-
//     circuits any late inbound-handoff envelopes the still-listening
//     server delivers in the ~ms window before process.exit fires.
//     Calling tryClose/tryStop symmetrically would add latency to
//     the active SIGTERM critical path with no correctness gain.
//   * db.close() — the 12 s ceiling + process.exit() forces socket
//     cleanup at the OS level. The active replica is not the system
//     of record for any uncommitted DB state during the SIGTERM
//     window: writes to qurl_bot_flow_state run on the worker tier
//     (separate process), and the gateway's only DB interactions
//     are lock / heartbeat reads that are idempotent and re-driven
//     by the standby. Holding up SIGTERM for a Knex pool drain
//     would extend the handoff critical path with no correctness
//     gain.
//   * eventConsumer — never runs on the gateway tier (isWorker is
//     derived as isHttp && ENABLE_EVENT_SHIPPER, so hot-standby
//     gateways have no consumer to drain). Skipped implicitly by
//     never being constructed; called out here so a future role-
//     shape refactor doesn't quietly re-introduce a consumer on
//     the gateway path without re-thinking this list.
//
// One NON-skip: `eventPublisher.stop()` runs in parallel with
// pushHandoff. The publisher's in-flight SQS sends are the outgoing
// process's responsibility — those frames arrived on OUR WebSocket,
// the standby cannot replay them. Draining in parallel (not before)
// means the publisher's DRAIN_DEADLINE_MS doesn't extend the
// pushHandoff critical path.
//
// The caller manages the `isShuttingDown` re-entry gate; this
// function is purely "what runs inside the gate". Deps are injected
// so the timeout / exit-code / handoff-result / drain contracts are
// unit-testable without process.exit side effects.
async function runPushHandoffShutdown({
  code = 0,
  gatewayLeader,
  // Optional. When provided, `.stop()` runs in parallel with
  // pushHandoff so in-flight SQS sends drain inside the 12 s ceiling
  // instead of being truncated at process.exit. Production wires
  // src/event-publisher.js; tests can omit or pass a spy.
  eventPublisher = null,
  logger,
  // 12 s = 9 s pushHandoff internal ceiling (enforced in
  // gateway-leader.js's DEFAULT_INBOUND_CONNECT_TIMEOUT_MS /
  // pushHandoff race) + ~3 s headroom for the post-handoff log,
  // publisher drain, and process.exit unwind. Well under ECS's 30 s
  // SIGTERM deadline.
  ceilingMs = 12_000,
  // Forced exit code when the outer ceiling fires. Defaults to 1
  // so ECS / dashboards can distinguish "clean transfer, exit 0"
  // from "timeout, standby cold-acquired, exit 1" even when the
  // SIGTERM that triggered us was code 0. Configurable for tests.
  forcedExitCode = 1,
  // Injected for tests; production uses node:timers + process.
  exit = (c) => process.exit(c),
  scheduleHardExit = setTimeout,
  clearHardExit = clearTimeout,
}) {
  logger.info('Hot-standby shutdown initiated; attempting pushHandoff');
  const hardExit = scheduleHardExit(() => {
    try {
      logger.error('PushHandoff shutdown timed out, forcing exit');
    } finally {
      exit(forcedExitCode);
    }
  }, ceilingMs);
  if (hardExit && typeof hardExit.unref === 'function') {
    // `.unref()` so the timer can't pin shutdown past the explicit
    // `exit()` below. Safe against "loop idles out early before
    // the ceiling fires" because pushHandoff itself holds DDB +
    // HTTP socket handles open during its await, and we exit
    // synchronously after `await drainPromise` resolves — there's
    // no window where the unref'd timer is the only loop handle
    // before we call exit() explicitly.
    hardExit.unref();
  }
  // Kick the publisher drain in parallel — tryStop is null-safe,
  // catches both sync throws and async rejects, and harmonizes log
  // shape with the rest of the shutdown surface. Capturing the
  // promise (instead of awaiting here) lets pushHandoff run
  // concurrently; we await both before exit below.
  const drainPromise = tryStop('event-publisher', eventPublisher, logger);
  // Track whether pushHandoff threw — we still exit (so the standby
  // cold-acquires after lock TTL) but with `forcedExitCode` instead
  // of the incoming `code` so deploy dashboards can distinguish three
  // outcomes by exit code: clean transfer (code), pushHandoff threw
  // (forcedExitCode), pushHandoff timed out (forcedExitCode, via the
  // hard-exit timer above).
  let handoffThrew = false;
  try {
    const result = await gatewayLeader.pushHandoff();
    logger.info('pushHandoff complete', {
      transferred: result?.transferred,
      pushAcked: result?.pushAcked,
      reason: result?.reason,
      pushReason: result?.pushReason,
    });
  } catch (err) {
    handoffThrew = true;
    logger.error('pushHandoff threw — exiting anyway so the standby can cold-acquire', {
      error: err.message,
    });
  }
  // Wait for the in-parallel publisher drain to finish before
  // exit, so any SQS send that was almost-flushed gets its last
  // round-trip. Best-effort; the outer ceiling absorbs the worst
  // case.
  await drainPromise;
  // Clear the hard-exit timer on the success path. In prod
  // process.exit kills the process so the pending timer is moot
  // (and .unref'd so it can't pin shutdown anyway), but a
  // non-terminal injected `exit` (tests, future metric-emitting
  // wrappers) would observe a spurious second exit-code-1 ~12 s
  // later without this clear.
  clearHardExit(hardExit);
  exit(handoffThrew ? forcedExitCode : code);
}

module.exports = {
  shouldUsePushHandoffShutdown,
  selectGatewayReadinessProbe,
  awaitServerListening,
  tryStop,
  tryClose,
  stopGatewayHotStandby,
  runGracefulShutdown,
  runGatewayFatalShutdown,
  runPushHandoffShutdown,
};
