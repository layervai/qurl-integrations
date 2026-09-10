// Connection watchdog for Pillar 3 — closes the gap where the
// standby has been handed the lock (or acquired it via cold
// fallback) but the gateway WebSocket isn't connected to Discord.
// Without this loop, the active times out on ACK after 200 ms and
// exits, while the standby sits lock-held-but-no-WS indefinitely.
//
// Spec reference: docs/zero-downtime-design.md §Pillar 3
// "connection-watchdog" (lines ~583-612).
//
// ── Loop ──
// Every `pollIntervalMs` (default 1000):
//   1. If connected → reset attempts/recovery; continue. When local lock
//      ownership is false, strongly read the holder first and exit only if a
//      peer owner confirms split brain.
//   2. If @discordjs/ws is automatically recovering from Closed,
//      wait for Ready/Resumed. Never race it with manager.connect().
//      A recovery that stays stuck for maxRecoveryMs releases the lock,
//      when held, and exits so ECS can replace the process. This limit
//      also runs on a non-lock-holder, so a stale standby latch cannot
//      poison a later handoff. Promotion does not reset the process-wide
//      timer, so lock flapping cannot extend the bound.
//   3. If not holding the lock → reset attempts; continue. The standby
//      loop no-ops until promotion, but recovery in step 2 stays bounded.
//   4. If the leader is already connecting → reset attempts; continue.
//      This prevents concurrent connect() calls.
//   5. Otherwise: attempts++, try `manager.connect()`. On success,
//      reset attempts. On failure, log + exponential backoff sleep
//      (200 ms, 400 ms, 800 ms, 1.6 s — capped at 5 s, see below).
//      At `attempts >= maxAttempts` (default 5), release the lock
//      and `exit(1)` so ECS replaces the task.
//
// ── Why bounded retry → exit, not infinite retry ──
// A standby that can't reach Discord but holds the lock blocks the
// only failover slot. ECS will restart us with fresh DNS / fresh
// gateway endpoint pinning / fresh networking state; that's the
// cheapest recovery path. Holding the lock and looping forever
// merely delays handoff to a new task.
//
// ── Backoff cap is dead code ──
// `Math.min(2^attempts * 100, 5000)` caps at 5 s. At maxAttempts=5,
// the last backoff that ever sleeps is at attempts=4 → 1600 ms,
// before the attempts=5 check triggers exit. The 5 s ceiling is
// unreachable at this cap. Kept literally for future-proofing: if
// someone bumps maxAttempts to e.g. 8, the cap kicks in (12800 ms
// → 5000 ms) and they don't have to revisit the formula.
//
// ── Concurrency ──
// The watchdog awaits `manager.connect()` inline. The next 1 s tick
// does NOT fire until that call settles. Two concurrent connect()
// invocations would race the @discordjs/ws internal state; the
// design depends on at-most-one outstanding connect() per watchdog
// instance. Tests pin this by counting connect() calls under
// fake-timer advancement.
//
// ── Process exit is injected ──
// Real prod calls `process.exit(1)`. Tests inject a no-op so the
// retries-exhausted branch is observable without killing jest.

const { performance } = require('node:perf_hooks');

const DEFAULT_POLL_INTERVAL_MS = 1_000;
const DEFAULT_MAX_ATTEMPTS = 5;
// Give @discordjs/ws 20 seconds to finish its own reconnect loops. This
// covers the library's 500 ms reconnect delay, Invalid Session backoff,
// and an IDENTIFY bucket wait, while staying well inside Discord's
// resumable-session window. Terminal close codes bypass this timer in the
// shim and enter the explicit-connect failure ladder immediately.
// TODO(upstream-contract): Re-check this limit when the pinned @discordjs/ws
// reconnect delay or Discord resumable-session behavior changes.
const DEFAULT_MAX_RECOVERY_MS = 20_000;
const BACKOFF_BASE_MS = 100;
const BACKOFF_CAP_MS = 5_000;
// Ceilings below bound the WATCHDOG's own failure-replace path —
// they're NOT on the SIGTERM critical path (which has its own
// ~200 ms client-side cap in gateway-leader.js's pushHandoff). A
// stuck standby holding the failover slot is what these protect
// against; worst-case watchdog tick at exhaustion is CONNECT +
// RELEASE_LOCK ≈ 11 s, well inside the 30 s ECS graceful-stop.
//
// Hard ceiling on the releaseLock-during-exit call. The leader's
// releaseLockForImmediateExit awaits through `runSerialized`, so a
// hung inbound-handoff (parked inside manager.connect() with
// `connecting=true` latched per gateway-leader.js's bounded-
// settlement comment, tracked in #415) would otherwise block the
// chain forever and exit(1) would never fire. 3 s is generous
// vs. the 1 s expected DDB RTT; on miss we log and exit anyway.
const RELEASE_LOCK_CEILING_MS = 3_000;
// Hard ceiling on the watchdog's `manager.connect()` await. Mirrors
// the leader's `inboundConnectTimeoutMs` (5 s default) — a hung
// connect would otherwise pin the tick forever, attempts wouldn't
// advance, and the `exit(1)` recovery would never fire. 8 s is
// generous vs. Discord WS cold-connect's 1-3 s typical, slightly
// wider than the leader's 5 s to leave headroom for the watchdog's
// fail-loud-then-replace posture (the leader caps tighter because
// it's gated on the SIGTERM ECS deadline). On timeout we throw
// like a connect rejection; the attempts ladder advances normally.
const CONNECT_CEILING_MS = 8_000;

// `manager` is conventionally the gateway-ws-shim instance (passed
// from startHotStandby), but any object satisfying connect() +
// sync isConnected() + isRecovering() is accepted. The raw @discordjs/ws
// WebSocketManager does NOT satisfy this contract — it exposes
// only an async fetchStatus() — which is why the shim wraps it.
function createConnectionWatchdog({
  manager,
  isHoldingLock,
  // REQUIRED. Returns true when the leader is itself mid-
  // `manager.connect()` (inbound-handoff path); the watchdog backs
  // off rather than race a second concurrent connect. Required
  // because the race is otherwise silent: an inbound-handoff connect
  // taking >1 s would produce overlapping connect() calls against
  // the non-concurrency-safe WebSocketManager. A wiring oversight
  // failing loud at boot is preferable to a rare production race.
  // Tests that don't exercise the inbound-handoff path can pass
  // `() => false`.
  isConnecting,
  // REQUIRED. Strongly reads the DDB holder row when a connected shard no
  // longer has local ownership. A missing row can be reacquired by the
  // leader; a row owned by another instance confirms split brain and makes
  // this process exit without touching the peer's lock.
  readCurrentHolder,
  selfInstanceId,
  releaseLock,
  // Ceilings — see RELEASE_LOCK_CEILING_MS / CONNECT_CEILING_MS above
  // for rationale. Injected for tests so they can be tuned tiny.
  releaseLockCeilingMs = RELEASE_LOCK_CEILING_MS,
  connectCeilingMs = CONNECT_CEILING_MS,
  // Optional. If provided, called best-effort on the exhaustion-exit
  // path alongside releaseLock. Symmetric to gateway-leader.js's
  // SIGTERM pushHandoff path which also deletes the row. Without
  // this, the heartbeat row lingers until DDB TTL (default 60s) and
  // surviving peers keep `isKnownPeer`-ing this dead replica + the
  // next handoff source may pick this row from listFreshPeers head
  // and time out the 200 ms ACK. Not a correctness issue (the cold-
  // fallback floor applies), but a small latency tax during the TTL
  // window. Optional so existing wiring without peerHeartbeat
  // visibility doesn't break — the absence is a no-op, not a throw.
  deleteOwnRow,
  logger,
  pollIntervalMs = DEFAULT_POLL_INTERVAL_MS,
  maxAttempts = DEFAULT_MAX_ATTEMPTS,
  maxRecoveryMs = DEFAULT_MAX_RECOVERY_MS,
  // Injected for tests. Production: setTimeout-based.
  sleep = (ms) => new Promise((resolve) => { setTimeout(resolve, ms); }),
  // Injected for deterministic tests. performance.now() is monotonic, so an
  // NTP wall-clock correction cannot shorten or extend the recovery bound.
  now = () => performance.now(),
  // Wall time is used only to compare the DDB lease's epoch-seconds expiry.
  wallClock = () => Date.now(),
  // Injected for tests. Production: process.exit. Tests pass a spy.
  exit = (code) => process.exit(code),
} = {}) {
  if (!manager
      || typeof manager.connect !== 'function'
      || typeof manager.isConnected !== 'function'
      || typeof manager.isRecovering !== 'function') {
    throw new Error('createConnectionWatchdog: manager with connect(), isConnected(), and isRecovering() is required');
  }
  if (typeof isHoldingLock !== 'function') {
    throw new Error('createConnectionWatchdog: isHoldingLock function is required');
  }
  if (typeof isConnecting !== 'function') {
    throw new Error('createConnectionWatchdog: isConnecting function is required');
  }
  if (typeof readCurrentHolder !== 'function') {
    throw new Error('createConnectionWatchdog: readCurrentHolder function is required');
  }
  if (!selfInstanceId) {
    throw new Error('createConnectionWatchdog: selfInstanceId is required');
  }
  if (typeof releaseLock !== 'function') {
    throw new Error('createConnectionWatchdog: releaseLock function is required');
  }
  if (!logger) throw new Error('createConnectionWatchdog: logger is required');
  // maxAttempts=0 would exit(1) on the first tick; pollIntervalMs=0
  // would saturate the loop. Fail loud at boot.
  if (!Number.isInteger(maxAttempts) || maxAttempts <= 0) {
    throw new Error('createConnectionWatchdog: maxAttempts must be a positive integer');
  }
  if (!Number.isInteger(maxRecoveryMs) || maxRecoveryMs <= 0) {
    throw new Error('createConnectionWatchdog: maxRecoveryMs must be a positive integer');
  }
  if (!Number.isInteger(pollIntervalMs) || pollIntervalMs <= 0) {
    throw new Error('createConnectionWatchdog: pollIntervalMs must be a positive integer');
  }
  if (!Number.isInteger(releaseLockCeilingMs) || releaseLockCeilingMs <= 0) {
    throw new Error('createConnectionWatchdog: releaseLockCeilingMs must be a positive integer');
  }
  if (!Number.isInteger(connectCeilingMs) || connectCeilingMs <= 0) {
    throw new Error('createConnectionWatchdog: connectCeilingMs must be a positive integer');
  }
  if (typeof now !== 'function') {
    throw new Error('createConnectionWatchdog: now must be a function');
  }
  if (typeof wallClock !== 'function') {
    throw new Error('createConnectionWatchdog: wallClock must be a function');
  }

  // Races `promise` against a `ceilingMs` timer that rejects with a
  // labeled Error. Used to bound `manager.connect()` and
  // `releaseLock()` — either could hang and pin the failure-exit
  // recovery path. Mirrors `gateway-leader.js`'s
  // `inboundConnectTimeoutMs` pattern.
  async function raceWithCeiling(promise, ceilingMs, label) {
    let timer;
    try {
      return await Promise.race([
        promise,
        new Promise((_, reject) => {
          timer = setTimeout(
            () => reject(new Error(`${label}_${ceilingMs}ms`)),
            ceilingMs,
          );
        }),
      ]);
    } finally {
      clearTimeout(timer);
    }
  }

  let running = false;
  let loopPromise = null;
  let attempts = 0;
  let recoveryStartedAt = null;
  let closed = false;
  let stopping = false;

  function resetRecovery() {
    recoveryStartedAt = null;
  }

  async function exitAfterExhaustion({
    reason,
    mode,
    outcome,
    details = { attempts },
    releaseOwnedLock = true,
  }) {
    const action = releaseOwnedLock ? 'releasing lock' : 'exiting';
    logger.error(`connection-watchdog: ${mode} ${outcome}, ${action}`, {
      error: reason, ...details,
    });
    if (releaseOwnedLock) {
      try {
        // Bounded by RELEASE_LOCK_CEILING_MS. If release hangs, exit
        // anyway and let the short DDB lease expire.
        await raceWithCeiling(releaseLock(), releaseLockCeilingMs, 'release_lock_ceiling');
      } catch (rerr) {
        logger.error('connection-watchdog: releaseLock failed during exhaustion-exit', {
          error: rerr.message,
        });
      }
    }
    // Remove this process from peer discovery before ECS replaces it.
    if (typeof deleteOwnRow === 'function') {
      try {
        await deleteOwnRow();
      } catch (derr) {
        logger.warn('connection-watchdog: deleteOwnRow failed during exhaustion-exit', {
          error: derr.message,
        });
      }
    }
    closed = true;
    running = false;
    exit(1);
  }

  // Run one iteration of the watchdog. Public for tests; production
  // calls it from the `start()` loop. Returns once the iteration
  // (including any failure-path backoff sleep) settles.
  async function step() {
    // Terminal-state guard. After the exhaustion-exit branch sets
    // closed=true, the production loop stops. But _stepForTest can
    // still be called manually post-exit; without this guard a
    // re-entry would re-fire releaseLock + deleteOwnRow. Test-tool
    // robustness only; production never re-enters.
    if (closed || stopping) return;
    const holdingLock = isHoldingLock();
    if (manager.isConnected()) {
      attempts = 0;
      resetRecovery();
      // Local ownership can go false after a renew CAS miss because either a
      // peer owns the row OR the row disappeared. Only the first case proves
      // split brain. Confirm it with a strong DDB read before exiting; an
      // absent/self-owned row is safe for the leader's next acquire tick.
      // The SIGTERM handoff path stops the watchdog before it transfers the
      // lock, so its expected connected-then-transfer window cannot reach
      // this guard.
      if (!holdingLock) {
        let currentHolder;
        try {
          currentHolder = await readCurrentHolder();
        } catch (err) {
          if (stopping) return;
          logger.error('connection-watchdog: could not confirm lock ownership; will retry', {
            error: err.message,
          });
          return;
        }
        if (stopping) return;
        const expiresAt = Number(currentHolder?.expires_at);
        const currentEpochSeconds = Math.floor(wallClock() / 1000);
        const peerOwnsLiveLease = currentHolder?.instance_id
          && currentHolder.instance_id !== selfInstanceId
          && Number.isFinite(expiresAt)
          && expiresAt >= currentEpochSeconds;
        // Exit only for a live peer lease. An expired peer row is acquirable
        // under gateway-lock's CAS contract. The leader will replace it on its
        // next tick; if the old peer still has a live shard, its watchdog then
        // sees this process's live lease and exits that losing replica.
        if (peerOwnsLiveLease) {
          await exitAfterExhaustion({
            reason: 'gateway shard is connected while a peer owns the leader lock',
            mode: 'lock ownership',
            outcome: 'violation',
            details: { peer_instance_id: currentHolder.instance_id },
            releaseOwnedLock: false,
          });
        } else {
          logger.warn('connection-watchdog: connected without local lock; no peer owner confirmed', {
            current_instance_id: currentHolder?.instance_id ?? null,
            current_expires_at: Number.isFinite(expiresAt) ? expiresAt : null,
          });
        }
      }
      return;
    }

    // A Closed event makes @discordjs/ws reconnect internally. Its
    // implementation has a 500 ms Idle gap before internalConnect(),
    // so manager.connect() here can create a second live shard. Wait
    // for the library's reconnect. Track this before the lock and
    // leader-connect guards: recovery belongs to the process, and must
    // remain bounded even while this replica is a standby or a leader
    // connect promise is still settling.
    if (manager.isRecovering()) {
      attempts = 0;
      const currentTime = now();
      if (recoveryStartedAt === null) {
        recoveryStartedAt = currentTime;
        logger.warn('connection-watchdog: automatic reconnect in progress; standing down', {
          max_recovery_ms: maxRecoveryMs,
        });
      }
      const recoveryElapsedMs = Math.max(0, currentTime - recoveryStartedAt);
      if (recoveryElapsedMs >= maxRecoveryMs) {
        await exitAfterExhaustion({
          reason: 'automatic reconnect did not reach Ready/Resumed',
          mode: 'automatic reconnect',
          outcome: 'timed out',
          details: { recovery_elapsed_ms: recoveryElapsedMs },
          releaseOwnedLock: holdingLock,
        });
      }
      return;
    }

    resetRecovery();
    if (!holdingLock) {
      attempts = 0;
      return;
    }
    // Leader is mid-connect (inbound-handoff path). Stand down —
    // don't race a second concurrent connect against the same
    // WebSocketManager. Reset attempts so the leader's eventual
    // success doesn't leave a stale failure ladder behind. A connect
    // that never settles without emitting Closed remains the existing
    // issue #415 case; the automatic-recovery timer above cannot see it.
    if (isConnecting()) {
      attempts = 0;
      return;
    }

    attempts += 1;
    try {
      // Bounded by CONNECT_CEILING_MS — a hung connect would otherwise
      // pin this tick and the failure-exit recovery would never fire.
      await raceWithCeiling(manager.connect(), connectCeilingMs, 'watchdog_connect_ceiling');
      if (stopping) return;
      attempts = 0;
      logger.info('connection-watchdog: connect succeeded');
    } catch (err) {
      if (stopping) return;
      if (attempts >= maxAttempts) {
        await exitAfterExhaustion({
          reason: err.message,
          mode: 'connect',
          outcome: 'retries exhausted',
        });
        return;
      }
      const backoffMs = Math.min((2 ** attempts) * BACKOFF_BASE_MS, BACKOFF_CAP_MS);
      logger.warn('connection-watchdog: connect failed, backing off', {
        error: err.message, attempts, backoffMs,
      });
      await sleep(backoffMs);
    }
  }

  async function loop() {
    while (running) {
      // Sleep first so an immediate stop() right after start()
      // doesn't run a single tick — useful for tests + clean
      // shutdown semantics.
      await sleep(pollIntervalMs);
      if (!running) break;
      // Backstop for unexpected throws from `manager.isConnected()`
      // (contractually non-throwing, but a future shim refactor
      // could regress) or any other unhandled path. Without this,
      // a single throw inside step() would exit the loop and
      // silently disable the watchdog. Log + continue: the next
      // tick re-tries.
      try {
        await step();
      } catch (err) {
        logger.error('connection-watchdog: step threw unexpectedly (loop continues)', {
          error: err && err.message,
        });
      }
    }
  }

  function start() {
    // Guard on `loopPromise` (NOT just `running`) so a `start()`
    // after a `stop()` that hasn't yet observed the running=false
    // flag — the old loop is still inside `await sleep(...)` — does
    // not spawn a second concurrent loop. Callers that need to
    // re-start MUST await `stop()` first.
    if (loopPromise || closed) return;
    stopping = false;
    running = true;
    loopPromise = loop().finally(() => { loopPromise = null; });
  }

  // Halts the loop and returns a promise that resolves once the
  // last in-flight tick has completed (including any in-flight
  // `manager.connect()` — the awaiting tick still completes before
  // the loop check sees running=false). Idempotent. Callers that
  // want to re-start the watchdog MUST await this.
  function stop() {
    stopping = true;
    running = false;
    return loopPromise ?? Promise.resolve();
  }

  return {
    start,
    stop,
    // Inspection + driver seams for tests.
    _stepForTest: step,
    _getAttemptsForTest() { return attempts; },
    _getRunningForTest() { return running; },
    _getStoppingForTest() { return stopping; },
    _getLoopPromiseForTest() { return loopPromise; },
  };
}

module.exports = {
  createConnectionWatchdog,
  DEFAULT_POLL_INTERVAL_MS,
  DEFAULT_MAX_ATTEMPTS,
  DEFAULT_MAX_RECOVERY_MS,
  BACKOFF_BASE_MS,
  BACKOFF_CAP_MS,
};
