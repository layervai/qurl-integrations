const {
  shouldUsePushHandoffShutdown,
  selectGatewayReadinessProbe,
  tryStop,
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

  it('logs at warn and swallows the error if stop() rejects', async () => {
    // Teardown is already on the failure path; one component's
    // stop() error shouldn't stall the rest of the drain (which
    // is why we wrap each in tryStop rather than chaining bare
    // awaits).
    const logger = makeFakeLogger();
    const handle = { stop: jest.fn().mockRejectedValue(new Error('ddb down')) };
    await expect(tryStop('leader', handle, logger)).resolves.toBeUndefined();
    expect(logger.warn).toHaveBeenCalledWith('leader stop failed', { error: 'ddb down' });
  });

  it('embeds the component name in the warn message for triage', async () => {
    const logger = makeFakeLogger();
    const handle = { stop: jest.fn().mockRejectedValue(new Error('boom')) };
    await tryStop('connection-watchdog', handle, logger);
    expect(logger.warn).toHaveBeenCalledWith('connection-watchdog stop failed', { error: 'boom' });
  });
});
