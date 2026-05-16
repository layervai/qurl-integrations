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
    logger.warn(`${name} stop failed`, { error: err.message });
  }
}

module.exports = {
  shouldUsePushHandoffShutdown,
  selectGatewayReadinessProbe,
  tryStop,
};
