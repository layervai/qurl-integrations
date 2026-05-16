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
    logger.warn(`${name} stop failed`, { error: err.message, stack: err.stack });
  }
}

// Symmetric to tryStop, but for net.Server-shaped handles (callback-
// based close instead of Promise-based stop). Null-safe, awaits the
// close callback, surfaces any close error at warn. Resolves cleanly
// in every case so teardown doesn't stall.
async function tryClose(name, server, logger) {
  if (!server) return;
  await new Promise(resolve => {
    server.close(err => {
      if (err) logger.warn(`${name} close reported error`, { error: err.message });
      resolve();
    });
  });
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
  ceilingMs = 12_000,
  // Forced exit code when the outer ceiling fires. Defaults to 1
  // so ECS / dashboards can distinguish "clean transfer, exit 0"
  // from "timeout, standby cold-acquired, exit 1" even when the
  // SIGTERM that triggered us was code 0. Configurable for tests.
  forcedExitCode = 1,
  // Injected for tests; production uses node:timers + process.
  exit = (c) => process.exit(c),
  scheduleHardExit = setTimeout,
}) {
  logger.info('Hot-standby shutdown initiated; attempting pushHandoff');
  const hardExit = scheduleHardExit(() => {
    logger.error('PushHandoff shutdown timed out, forcing exit');
    exit(forcedExitCode);
  }, ceilingMs);
  if (hardExit && typeof hardExit.unref === 'function') {
    hardExit.unref();
  }
  // Kick the publisher drain in parallel — tryStop is null-safe,
  // catches both sync throws and async rejects, and harmonizes log
  // shape with the rest of the shutdown surface. Capturing the
  // promise (instead of awaiting here) lets pushHandoff run
  // concurrently; we await both before exit below.
  const drainPromise = tryStop('event-publisher', eventPublisher, logger);
  try {
    const result = await gatewayLeader.pushHandoff();
    logger.info('pushHandoff complete', {
      transferred: result?.transferred,
      pushAcked: result?.pushAcked,
      reason: result?.reason,
      pushReason: result?.pushReason,
    });
  } catch (err) {
    logger.error('pushHandoff threw — exiting anyway so the standby can cold-acquire', {
      error: err.message,
    });
  }
  // Wait for the in-parallel publisher drain to finish before
  // exit, so any SQS send that was almost-flushed gets its last
  // round-trip. Best-effort; the outer ceiling absorbs the worst
  // case.
  await drainPromise;
  exit(code);
}

module.exports = {
  shouldUsePushHandoffShutdown,
  selectGatewayReadinessProbe,
  tryStop,
  tryClose,
  runPushHandoffShutdown,
};
