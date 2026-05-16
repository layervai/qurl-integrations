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
//   1. If not holding the lock → reset attempts; continue. The
//      watchdog runs unconditionally, so a standby waiting to be
//      promoted enters the loop but no-ops until it acquires the
//      lock. Resetting attempts here means a previous failure ladder
//      doesn't carry across a lock-give-up + lock-reacquire cycle.
//   2. If holding the lock AND the manager reports connected → reset
//      attempts; continue. Steady-state: 1 s tick, no work, no log.
//   3. Otherwise: attempts++, try `manager.connect()`. On success,
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

const DEFAULT_POLL_INTERVAL_MS = 1_000;
const DEFAULT_MAX_ATTEMPTS = 5;
const BACKOFF_BASE_MS = 100;
const BACKOFF_CAP_MS = 5_000;

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
  releaseLock,
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
  // Injected for tests. Production: setTimeout-based.
  sleep = (ms) => new Promise((resolve) => { setTimeout(resolve, ms); }),
  // Injected for tests. Production: process.exit. Tests pass a spy.
  exit = (code) => process.exit(code),
} = {}) {
  if (!manager || typeof manager.connect !== 'function' || typeof manager.isConnected !== 'function') {
    throw new Error('createConnectionWatchdog: manager with connect() and isConnected() is required');
  }
  if (typeof isHoldingLock !== 'function') {
    throw new Error('createConnectionWatchdog: isHoldingLock function is required');
  }
  if (typeof isConnecting !== 'function') {
    throw new Error('createConnectionWatchdog: isConnecting function is required');
  }
  if (typeof releaseLock !== 'function') {
    throw new Error('createConnectionWatchdog: releaseLock function is required');
  }
  if (!logger) throw new Error('createConnectionWatchdog: logger is required');

  let running = false;
  let loopPromise = null;
  let attempts = 0;
  let closed = false;

  // Run one iteration of the watchdog. Public for tests; production
  // calls it from the `start()` loop. Returns once the iteration
  // (including any failure-path backoff sleep) settles.
  async function step() {
    if (!isHoldingLock()) {
      attempts = 0;
      return;
    }
    if (manager.isConnected()) {
      attempts = 0;
      return;
    }
    // Leader is mid-connect (inbound-handoff path). Stand down —
    // don't race a second concurrent connect against the same
    // WebSocketManager. Reset attempts so the leader's eventual
    // success doesn't leave a stale failure ladder behind, and
    // its eventual failure starts the ladder cleanly from 1 on
    // the next tick when isConnecting() returns false again.
    if (isConnecting()) {
      attempts = 0;
      return;
    }

    attempts += 1;
    try {
      await manager.connect();
      attempts = 0;
      logger.info('connection-watchdog: connect succeeded');
    } catch (err) {
      if (attempts >= maxAttempts) {
        logger.error('connection-watchdog: connect retries exhausted, releasing lock', {
          error: err.message, attempts,
        });
        try {
          await releaseLock();
        } catch (rerr) {
          // Logged-and-swallowed: we're already in the failure-exit
          // path; refusing to exit because of a DDB blip would leave
          // the lock held with no live gateway.
          logger.error('connection-watchdog: releaseLock failed during exhaustion-exit', {
            error: rerr.message,
          });
        }
        // Best-effort heartbeat-row cleanup, symmetric to gateway-
        // leader.js's pushHandoff path. Closes the discovery window
        // immediately so a freshly-promoted peer's listFreshPeers
        // stops returning this dead row. Optional hook — absence is
        // a no-op; failure is logged and swallowed (we're already
        // exiting).
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
    running = true;
    loopPromise = loop().finally(() => { loopPromise = null; });
  }

  // Halts the loop and returns a promise that resolves once the
  // last in-flight tick has completed (including any in-flight
  // `manager.connect()` — the awaiting tick still completes before
  // the loop check sees running=false). Idempotent. Callers that
  // want to re-start the watchdog MUST await this.
  function stop() {
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
    _getLoopPromiseForTest() { return loopPromise; },
  };
}

module.exports = {
  createConnectionWatchdog,
  DEFAULT_POLL_INTERVAL_MS,
  DEFAULT_MAX_ATTEMPTS,
  BACKOFF_BASE_MS,
  BACKOFF_CAP_MS,
};
