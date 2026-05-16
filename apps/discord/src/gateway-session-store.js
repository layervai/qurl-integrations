// DDB-backed @discordjs/ws session store for the gateway tier
// (zero-downtime design, Pillar 2 — cross-process RESUME).
//
// Persists `WebSocketShard.sessionInfo` (`{sessionId, resumeURL,
// sequence}`) to DDB so a process restart can RESUME from the last
// observed sequence instead of IDENTIFYing fresh. Without this
// persistence, every restart costs the cold-start window
// (~5-10 s of WebSocket handshake + READY + GUILD_CREATE bursts);
// with it, Discord replays buffered events from the last sequence
// within the ~60 s resume buffer window and the bot misses no
// interactions across the deploy boundary.
//
// Module shape: factory `createGatewaySessionStore({ ddbClient,
// tableName, shardId, logger, clock })` returns a store instance
// with its own mirror + throttle state. The factory shape (vs
// module-level singleton like event-publisher.js) lets tests run
// in parallel without resetting shared state and generalizes when
// PR 14+ scales beyond single-shard.
//
// ── Load-bearing contracts ──
//
// 1. In-memory mirror, not DDB-per-callback. `retrieveSessionInfo`
//    is called by @discordjs/ws every reconnect — possibly inside
//    a tight IDENTIFY-reject loop. Reading DDB on each call would
//    introduce ~10-100 ms of latency per loop iteration AND
//    couldn't observe in-process state changes made by an earlier
//    `updateSessionInfo(null)` (DDB writes are visible only after
//    SDK round-trip; mirror updates are visible immediately).
//    The mirror is hydrated from DDB once at boot via `hydrate()`;
//    subsequent reads from `retrieveSessionInfo` are pure-local.
//
// 2. Null-clear respected. When @discordjs/ws calls
//    `updateSessionInfo(shardId, null)` (Discord rejected the
//    RESUME), the mirror MUST be set to null AND the DDB row MUST
//    be deleted. Returning the stale session from the next
//    `retrieveSessionInfo` produces an infinite RESUME-reject loop
//    that the spike's first sandbox run reproduced. The DDB delete
//    is load-bearing for the cross-process case: a process that
//    crashes between IDENTIFY and the next READY would otherwise
//    leave the dead session row visible to the next boot's
//    hydrate(), and the next process would try to RESUME on a
//    session Discord has already invalidated.
//
// 3. Write throttling. `updateSessionInfo` fires on every gateway
//    dispatch (HEARTBEAT_ACK + PRESENCE_UPDATE + READY + every
//    INTERACTION_CREATE — high rate). DDB-write per dispatch would
//    burn ~$50/month at typical interaction volume AND introduce
//    write-rate-limit risk. We write immediately only when:
//      - the session_id changes (READY just fired), OR
//      - the prior write is more than WRITE_THROTTLE_MS old.
//    Otherwise the update is deferred via a single scheduled flush
//    that fires at most every WRITE_THROTTLE_MS. Mirror always
//    reflects the latest dispatch (so retrieveSessionInfo is
//    fresh); DDB lags by at most WRITE_THROTTLE_MS, which the
//    Discord 60 s resume window absorbs trivially.
//
// 4. Final flush on SIGTERM. The throttle defers writes; on
//    shutdown we must ensure the latest sequence reaches DDB
//    before exit. `flushFinal()` cancels any pending timer and
//    issues a synchronous write of the mirror's current state.
//    Skipping this drops the last (up to WRITE_THROTTLE_MS) worth
//    of sequence on the floor; the next boot's RESUME starts from
//    a too-old sequence and Discord replays those events (or
//    rejects the resume if it's older than the buffer).

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

  // Schedule a deferred write at (lastWriteAt + writeThrottleMs).
  // Idempotent — if a timer is already pending, leave it; the
  // existing timer will pick up whatever's in the mirror at fire
  // time, which is the latest dispatch. .unref() so a process
  // exit doesn't get pinned by a pending session-flush timer
  // (gracefulShutdown's flushFinal handles the final write).
  function scheduleFlush() {
    if (pendingFlush) return;
    if (stopped) return;
    const delay = Math.max(0, writeThrottleMs - (clock() - lastWriteAt));
    pendingFlush = setTimeout(async () => {
      pendingFlush = null;
      // Re-check mirror at fire time: an interleaved null-clear
      // would have set mirror=null and cancelled this timer. If
      // the clear fired AFTER this callback was queued by the
      // event loop but BEFORE it ran, mirror is now null and we
      // shouldn't write the stale info. The null-clear path
      // already deleted the row directly, so there's nothing left
      // to do here.
      if (!mirror || stopped) return;
      try {
        await persistRow(mirror);
        lastWrittenSessionId = mirror.sessionId;
        lastWriteAt = clock();
      } catch (err) {
        // Deferred write failure is non-fatal: the mirror still
        // holds the latest info, and the next dispatch will either
        // re-throttle (and retry on the next firing) or trigger an
        // immediate write (sessionId change). Final flush on
        // SIGTERM is the backstop for "throttle never recovers."
        logger.warn('gateway-session-store: deferred write failed', { error: err.message });
      }
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
    // rejected the RESUME (session expired / version skew / etc.)
    // and @discordjs/ws fell back to IDENTIFY. Async so the
    // immediate-write paths can await the DDB call; @discordjs/ws
    // accepts async callbacks.
    async updateSessionInfo(_shardId, info) {
      if (stopped) {
        // Race: SIGTERM landed mid-dispatch. Don't issue new DDB
        // writes; flushFinal already wrote (or about to write) the
        // mirror state captured before stop().
        return;
      }

      if (info === null) {
        // Null-clear path. Mirror cleared first so any in-flight
        // retrieveSessionInfo on a tight loop never observes the
        // dead session. Pending throttle is cancelled (it would
        // have re-written the dead row). DDB delete completes the
        // cross-process clear so the next boot's hydrate() returns
        // null and the new process IDENTIFYs cleanly.
        mirror = null;
        lastWrittenSessionId = null;
        cancelPendingFlush();
        try {
          await deleteRow();
          lastWriteAt = clock();
        } catch (err) {
          // Delete failure is recoverable: the in-process mirror
          // is already cleared (correctness for this process is
          // preserved), and the next dispatch's putItem (after
          // READY) will overwrite the stale row. Log loudly so
          // a sustained delete-fail surfaces in metrics.
          logger.warn('gateway-session-store: null-clear delete failed', { error: err.message });
        }
        return;
      }

      // Non-null update. Mirror always updated first so
      // retrieveSessionInfo sees the latest sequence even if the
      // DDB write is throttled.
      mirror = info;

      const sessionChanged = info.sessionId !== lastWrittenSessionId;
      const throttleExpired = (clock() - lastWriteAt) >= writeThrottleMs;
      if (sessionChanged || throttleExpired) {
        // Immediate-write path. Cancel any pending flush since we're
        // about to persist a newer value.
        cancelPendingFlush();
        try {
          await persistRow(info);
          lastWrittenSessionId = info.sessionId;
          lastWriteAt = clock();
        } catch (err) {
          // Immediate-write failure is the same recoverability
          // story as the deferred path: mirror holds the truth,
          // next dispatch retries. Log loudly so a sustained
          // write-fail surfaces — without it, the gateway runs
          // happily until restart and then RESUMEs on a too-old
          // sequence (or hits the resume-rejection IDENTIFY
          // fallback, which is at least observable).
          logger.warn('gateway-session-store: write failed', { error: err.message });
        }
      } else {
        // Throttle still cooling. Schedule a deferred flush so the
        // latest sequence reaches DDB within writeThrottleMs.
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

    // Final flush on SIGTERM. Cancels any pending throttle timer
    // and synchronously writes the mirror's current state. If
    // mirror is null (session was cleared mid-flight), nothing to
    // write — the null-clear path already issued the DDB delete.
    // Idempotent: safe to call multiple times (stop() guards
    // re-entry).
    async flushFinal() {
      stopped = true;
      cancelPendingFlush();
      if (!mirror) return;
      try {
        await persistRow(mirror);
        lastWrittenSessionId = mirror.sessionId;
        lastWriteAt = clock();
      } catch (err) {
        // Final-flush failure is the worst case for Pillar 2: the
        // next boot's hydrate() returns a stale sequence, and
        // Discord either replays a few seconds of events (best
        // case) or rejects the RESUME and the next process
        // IDENTIFYs (acceptable degradation). Log loudly so the
        // operator can correlate a failed flush with a degraded-
        // RESUME observation on the next boot.
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
  };
}

module.exports = {
  createGatewaySessionStore,
  DEFAULT_WRITE_THROTTLE_MS,
};
