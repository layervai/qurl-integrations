// DDB-backed @discordjs/ws session store. Persists
// `WebSocketShard.sessionInfo` (`{sessionId, resumeURL, sequence}`)
// so a process restart can RESUME from the last observed sequence
// instead of IDENTIFYing fresh — Discord replays buffered events
// from the last sequence within its ~60 s resume buffer window.
//
// Factory `createGatewaySessionStore` returns a store instance with
// its own mirror + throttle state, so tests run isolated and a
// future multi-shard caller can construct one per shard.
//
// ── Load-bearing contracts ──
//
// 1. In-memory mirror, not DDB-per-callback. `retrieveSessionInfo`
//    is called by @discordjs/ws every reconnect — possibly in a
//    tight IDENTIFY-reject loop. Reading DDB on each call adds
//    ~10-100 ms per iteration AND can't observe in-process state
//    changes made by an earlier `updateSessionInfo(null)`. Mirror
//    is hydrated once at boot; subsequent reads are pure-local.
//
// 2. Null-clear respected. When @discordjs/ws calls
//    `updateSessionInfo(shardId, null)` (Discord rejected the
//    RESUME), the mirror MUST be set to null AND the DDB row MUST
//    be deleted. Returning the stale session from the next
//    `retrieveSessionInfo` produces an infinite RESUME-reject
//    loop. The DDB delete is load-bearing across processes too:
//    a crash between IDENTIFY and the next READY would otherwise
//    leave the dead row visible to the next boot's hydrate().
//
// 3. Write throttling. `updateSessionInfo` fires on every gateway
//    dispatch (high rate). We write immediately on session_id
//    change (READY) or when the prior write is older than
//    WRITE_THROTTLE_MS; other updates defer to a single scheduled
//    flush. Mirror always reflects the latest dispatch; DDB lags
//    by at most WRITE_THROTTLE_MS, well inside the resume window.
//
// 4. Final flush on SIGTERM. `flushFinal()` cancels any pending
//    timer and writes the mirror's current state synchronously
//    so the next process's hydrate sees the latest sequence.

const { PutCommand, GetCommand, DeleteCommand } = require('@aws-sdk/lib-dynamodb');

// 1 Hz throttle on sequence updates. Tighter than the design doc's
// "at most once per second" guidance only insofar as the actual
// dispatch rate determines the steady-state — a low-traffic bot
// writes less often than 1/s anyway. The cap protects against the
// busy case (interaction storm during a feature launch).
const DEFAULT_WRITE_THROTTLE_MS = 1000;

function createGatewaySessionStore({
  ddbClient,
  tableName,
  shardId,
  logger,
  // Injected for deterministic throttle tests. Production uses Date.now.
  clock = () => Date.now(),
  writeThrottleMs = DEFAULT_WRITE_THROTTLE_MS,
} = {}) {
  if (!ddbClient) throw new Error('createGatewaySessionStore: ddbClient is required');
  if (!tableName) throw new Error('createGatewaySessionStore: tableName is required');
  if (!shardId) throw new Error('createGatewaySessionStore: shardId is required');
  if (!logger) throw new Error('createGatewaySessionStore: logger is required');

  // ── Mirror state ──
  //
  // `mirror` holds the latest SessionInfo (or null after a clear).
  // `lastWrittenSessionId` lets us detect a READY-fresh-session
  // transition cheaply (sessionId change → write immediately) vs a
  // sequence-only update (defer to throttle). `lastWriteAt` is the
  // throttle anchor. `pendingFlush` is the deferred-write timer
  // handle, cleared by every immediate-write path AND by stop().
  let mirror = null;
  let lastWrittenSessionId = null;
  let lastWriteAt = 0;
  let pendingFlush = null;
  let stopped = false;
  // Set of in-flight fire-and-forget DDB write promises. flushFinal
  // awaits Promise.allSettled on every entry so SIGTERM doesn't exit
  // mid-write. Each entry removes itself from the set on settle, so
  // the steady-state size is at most one (the throttle keeps writes
  // from overlapping under normal traffic). A simple "track only the
  // latest" reference would lose the earlier write's settlement
  // guarantee when a second fire-and-forget lands quickly behind it.
  const inFlightWrites = new Set();

  async function persistRow(info) {
    // Wall-clock epoch ms for `updated_at`. Matches the design
    // doc's table schema. DDB Number type takes JS Number directly;
    // no marshaling concerns up to 2^53.
    await ddbClient.send(new PutCommand({
      TableName: tableName,
      Item: {
        shard_id: shardId,
        session_id: info.sessionId,
        resume_url: info.resumeURL,
        sequence: info.sequence,
        updated_at: clock(),
      },
    }));
  }

  async function deleteRow() {
    await ddbClient.send(new DeleteCommand({
      TableName: tableName,
      Key: { shard_id: shardId },
    }));
  }

  function cancelPendingFlush() {
    if (pendingFlush) {
      clearTimeout(pendingFlush);
      pendingFlush = null;
    }
  }

  // Fire-and-forget DDB write. Cursor (lastWrittenSessionId +
  // lastWriteAt) updates synchronously so the next call's
  // sessionChanged + throttleExpired checks reflect the in-flight
  // write — without it, two same-sessionId updates in quick
  // succession would both fire immediate writes (each sees a
  // stale cursor). On failure the cursor isn't rolled back; the
  // throttle's next flush retries, and SIGTERM's flushFinal is
  // the backstop. Sustained failure logs every retry.
  function fireWrite(promise) {
    const p = promise.finally(() => inFlightWrites.delete(p));
    inFlightWrites.add(p);
  }
  function firePersist(info) {
    lastWrittenSessionId = info.sessionId;
    lastWriteAt = clock();
    fireWrite(persistRow(info).catch((err) => {
      logger.warn('gateway-session-store: write failed', { error: err.message });
    }));
  }

  // Schedule a deferred write at (lastWriteAt + writeThrottleMs).
  // Idempotent — if a timer is already pending, leave it; the
  // existing timer fires `firePersist(mirror)` with whatever's
  // current at the time. .unref() so a process exit isn't pinned
  // by a pending session-flush timer (flushFinal handles the
  // final write at SIGTERM).
  function scheduleFlush() {
    if (pendingFlush) return;
    if (stopped) return;
    const delay = Math.max(0, writeThrottleMs - (clock() - lastWriteAt));
    pendingFlush = setTimeout(() => {
      pendingFlush = null;
      // Mirror may have been null-cleared between schedule and
      // fire; if so, the null-clear path already issued a Delete
      // and there's nothing to write here.
      if (!mirror || stopped) return;
      firePersist(mirror);
    }, delay);
    pendingFlush.unref();
  }

  return {
    // ── @discordjs/ws callback surface ──
    //
    // `retrieveSessionInfo` is documented sync-or-async in
    // @discordjs/ws; we keep it sync to make the null-clear race
    // analysis trivial (no await between the callback firing and
    // the value @discordjs/ws sees). The (shardId) arg is unused
    // today — single-shard means we always serve the one row —
    // but accepted for forward-compat when sharding lands.
    retrieveSessionInfo(_shardId) {
      return mirror;
    },

    // `updateSessionInfo(shardId, info)` — info=null means Discord
    // rejected the RESUME and @discordjs/ws fell back to IDENTIFY.
    //
    // Synchronous resolution from @discordjs/ws's perspective:
    // returns before DDB I/O completes so a slow DDB write doesn't
    // back-pressure the per-frame WS dispatch loop. Writes fire-
    // and-forget; flushFinal awaits the most recent one before
    // exit so the next process's hydrate sees current state.
    updateSessionInfo(_shardId, info) {
      if (stopped) return;

      if (info === null) {
        mirror = null;
        lastWrittenSessionId = null;
        cancelPendingFlush();
        // Fire-and-forget delete. The next boot's hydrate() must
        // not observe the dead session row, hence the cross-
        // process clear — but waiting on DDB here would stall the
        // WS dispatch loop.
        fireWrite(deleteRow().catch((err) => {
          logger.warn('gateway-session-store: null-clear delete failed', { error: err.message });
        }));
        lastWriteAt = clock();
        return;
      }

      // Mirror always updated synchronously so retrieveSessionInfo
      // sees the latest sequence even if the DDB write is throttled.
      mirror = info;

      const sessionChanged = info.sessionId !== lastWrittenSessionId;
      const throttleExpired = (clock() - lastWriteAt) >= writeThrottleMs;
      if (sessionChanged || throttleExpired) {
        cancelPendingFlush();
        firePersist(info);
      } else {
        scheduleFlush();
      }
    },

    // ── Lifecycle ──
    //
    // Hydrate the mirror from DDB at boot. Called once before
    // `manager.connect()` so the first `retrieveSessionInfo` call
    // (inside @discordjs/ws's identify-or-resume decision) sees
    // the persisted session if one exists. Returns the hydrated
    // info or null — caller may log it as a "RESUME path vs cold
    // start" SLI.
    async hydrate() {
      try {
        const result = await ddbClient.send(new GetCommand({
          TableName: tableName,
          Key: { shard_id: shardId },
        }));
        if (!result.Item) {
          logger.info('gateway-session-store: hydrate found no row (cold start)');
          return null;
        }
        // Validate the shape: a malformed row (missing field, wrong
        // type) is treated as "no session" rather than throwing —
        // the bot must boot even if the DDB row is corrupted, and
        // IDENTIFY recovers the session on the next READY.
        const { session_id, resume_url, sequence } = result.Item;
        if (typeof session_id !== 'string'
            || typeof resume_url !== 'string'
            || typeof sequence !== 'number') {
          logger.warn('gateway-session-store: hydrate found malformed row, treating as cold start', {
            row: result.Item,
          });
          return null;
        }
        mirror = {
          sessionId: session_id,
          resumeURL: resume_url,
          sequence,
        };
        lastWrittenSessionId = session_id;
        // Note: NOT setting lastWriteAt here — the next dispatch
        // should hit the immediate-write path (throttleExpired=true
        // since lastWriteAt=0) so the first post-RESUME sequence
        // update lands in DDB without delay.
        logger.info('gateway-session-store: hydrated session from DDB', {
          sessionIdPrefix: session_id.slice(0, 8),
          sequence,
        });
        return mirror;
      } catch (err) {
        // Hydrate failure is non-fatal: the bot boots into IDENTIFY
        // mode and rebuilds the session from scratch. Log loudly
        // so a sustained read-fail surfaces — without it, every
        // deploy silently regresses to IDENTIFY and the operator
        // can't tell.
        logger.warn('gateway-session-store: hydrate failed, treating as cold start', {
          error: err.message,
        });
        return null;
      }
    },

    // Final flush on SIGTERM. Cancels any pending throttle timer,
    // awaits the most recently fired DDB write (write or null-
    // clear), then writes the mirror's current state synchronously
    // so the next process's hydrate sees current state.
    //
    // Failure here is the worst case for the migration: the next
    // boot's hydrate returns a stale sequence and Discord replays
    // a few seconds of events (best case) or rejects the RESUME
    // and the new process IDENTIFYs (acceptable degradation). Log
    // error-level so operators can correlate a failed flush with
    // a degraded RESUME on the next boot.
    async flushFinal() {
      stopped = true;
      cancelPendingFlush();
      // Settle every in-flight fire-and-forget write before our
      // own synchronous write — Promise.allSettled covers the
      // case where multiple writes are concurrently outstanding
      // (e.g. null-clear delete chased by a fresh-session put).
      // Inner .catch handlers in firePersist/null-clear already
      // logged each error.
      await Promise.allSettled([...inFlightWrites]);
      if (!mirror) return;
      try {
        await persistRow(mirror);
        lastWrittenSessionId = mirror.sessionId;
        lastWriteAt = clock();
      } catch (err) {
        logger.error('gateway-session-store: final flush failed', { error: err.message });
      }
    },

    // Test-only / shutdown-only: drop all timers without flushing.
    // Used by gracefulShutdown when flushFinal has already run, OR
    // by tests that want to assert "no timers leak." Distinct from
    // flushFinal so the call-sites stay readable (the spec is
    // "flush then stop", which reads as `flushFinal()` followed by
    // `stop()`; the latter is implicit because flushFinal sets
    // `stopped=true`).
    stop() {
      stopped = true;
      cancelPendingFlush();
    },

    // ── Test-only inspection ──
    //
    // Exposed for unit tests to assert mirror state without
    // round-tripping through DDB. Production code should NOT use
    // this — `retrieveSessionInfo` is the public API.
    _getMirrorForTest() {
      return mirror;
    },
    _getLastWriteAtForTest() {
      return lastWriteAt;
    },
    // Synchronizes a test on every in-flight fire-and-forget DDB
    // write. Production callers never await this (the WS dispatch
    // loop must remain non-blocking); flushFinal handles the
    // pre-exit synchronization.
    async _awaitInFlightForTest() {
      await Promise.allSettled([...inFlightWrites]);
    },
  };
}

module.exports = {
  createGatewaySessionStore,
  DEFAULT_WRITE_THROTTLE_MS,
};
