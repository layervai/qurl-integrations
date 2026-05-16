// Per-shard leader coordinator for Pillar 3. Composes:
//   - gateway-lock           (the DDB CAS lock primitive)
//   - gateway-peer-heartbeat (standby discovery)
//   - gateway-control-client (outbound push-handoff)
//   - manager                (the @discordjs/ws shim — connect()/isConnected())
//
// And exposes the four hooks the wiring layer (index.js, PR 13b.3)
// plugs into the other Pillar 3 components:
//   - `isHoldingLock()` → for gateway-connection-watchdog
//   - `releaseLockForExit()` → for gateway-connection-watchdog's
//      exhaustion path
//   - `handleInboundHandoff(payload)` → for gateway-control-channel's
//      onHandoff
//   - `isKnownPeer(instanceId)` → for gateway-control-channel's
//      isKnownPeer
//
// And the two lifecycle entry points the SIGTERM handler hits:
//   - `start()` — begin the tick loop (renew + heartbeat + acquire)
//   - `pushHandoff()` — SIGTERM path: find peer, transfer, push, return
//
// ── Tick loop ──
// Every `tickIntervalMs` (default 2_000), in order:
//   1. Write peer heartbeat (unconditional — active AND standby
//      heartbeat so each can discover the other for the handoff).
//   2. Refresh `knownPeerInstanceIds` from `listFreshPeers()`. The
//      control-channel server's `isKnownPeer` reads this cache
//      synchronously. Stale-by-at-most-tickIntervalMs is acceptable;
//      a stale peer reference would just produce a 400 unknown_peer
//      response, which is safe.
//   3. If we hold the lock → `renewLock()`. On CCF, peer took over
//      out-of-band; clear heldLock and let the watchdog observe.
//   4. If we DON'T hold the lock → `acquireLock()` (cold-fallback).
//      If acquired, set heldLock=true; the connection watchdog
//      observes lock-held + WS-not-connected and brings up the WS.
//
// ── Single-tick serialization ──
// gateway-lock's mutators (acquire / renew / transfer / adopt) are
// single-caller-only — their closure state would race under concurrency.
// The leader serializes every mutator path through `runSerialized`,
// which chains each work function onto the previous one. Tick,
// inbound-handoff, and SIGTERM-handoff all compete for this serial
// channel; whichever started first finishes first.
//
// ── Inbound-handoff ordering (adopt-then-flag-then-connect) ──
// On `handleInboundHandoff({activeInstanceId, expectedVersion})`:
//   1. `lock.adoptLockFromHandoff(expectedVersion)` — bootstrap our
//      local version cursor (the active already wrote the row in DDB).
//      If this throws, no `heldLock` flag is set; the watchdog never
//      sees lock-held when we don't actually hold it.
//   2. Set `heldLock = true`. Watchdog can now observe and act.
//   3. `manager.connect()`. If this throws, heldLock stays true; the
//      watchdog picks up next tick and retries the connect. This is
//      the design's "succeeded transferLock but failed connect"
//      recovery path.
// If we flipped heldLock=true BEFORE adopt, a transient adopt failure
// would leave the watchdog convinced we hold the lock when we don't
// even have a version cursor. Order matters.
//
// ── pushHandoff (SIGTERM path) ──
// Stops the tick loop, then:
//   1. Find a fresh peer in this shard (listFreshPeers, head of list).
//   2. If no peer → release lock (best-effort) + return {pushed: false,
//      reason: 'no_peer'}. Cold-fallback floor applies on the other
//      side.
//   3. transferLock(peer.instance_id, peer.lock_holder) — atomic
//      DDB ownership move. On CCF (active lost the lock to a cold-
//      fallback acquire by the peer) → return {pushed: false, reason:
//      'transfer_failed'}.
//   4. controlClient.pushHandoff(...) with expectedVersion = the
//      post-transfer version returned by transferLock.
//   5. Return the result. Caller (index.js SIGTERM handler) exits
//      either way.

const DEFAULT_TICK_INTERVAL_MS = 2_000;

function createGatewayLeader({
  lock,
  peerHeartbeat,
  controlClient,
  manager,
  selfInstanceId,
  selfLockHolder,
  shardId,
  logger,
  tickIntervalMs = DEFAULT_TICK_INTERVAL_MS,
  // Injected for tests. Production: setTimeout-based sleep.
  sleep = (ms) => new Promise((resolve) => { setTimeout(resolve, ms); }),
} = {}) {
  if (!lock) throw new Error('createGatewayLeader: lock is required');
  if (!peerHeartbeat) throw new Error('createGatewayLeader: peerHeartbeat is required');
  if (!controlClient) throw new Error('createGatewayLeader: controlClient is required');
  if (!manager || typeof manager.connect !== 'function') {
    throw new Error('createGatewayLeader: manager with connect() is required');
  }
  if (!selfInstanceId) throw new Error('createGatewayLeader: selfInstanceId is required');
  if (!selfLockHolder) throw new Error('createGatewayLeader: selfLockHolder is required');
  if (!shardId) throw new Error('createGatewayLeader: shardId is required');
  if (!logger) throw new Error('createGatewayLeader: logger is required');

  let heldLock = false;
  // Set true around `handleInboundHandoff`'s in-flight
  // `manager.connect()`. The watchdog observes this via
  // `isConnecting()` and skips its own `manager.connect()` call —
  // a Discord WS cold-connect routinely takes 1-3 s, longer than
  // the watchdog's 1 s tick, so without this flag the two would
  // race and call `connect()` concurrently. @discordjs/ws's
  // WebSocketManager is NOT safe under concurrent connect().
  let connecting = false;
  let running = false;
  // Terminal-after-pushHandoff sentinel. Once set, `start()` is a
  // permanent no-op. Mirrors the watchdog's `exited` flag — it's
  // there to make the "leader is dead after pushHandoff" invariant
  // explicit at the API surface rather than relying on the SIGTERM
  // handler to never call start() again.
  let closed = false;
  let loopPromise = null;
  let knownPeerInstanceIds = new Set();

  // Mutator serialization. Every call site that touches the
  // single-caller-only gateway-lock API funnels through this so
  // tick / inbound-handoff / push-handoff never overlap. Rejections
  // are caught when chained into `inFlight` so a prior failure
  // doesn't break the chain — but we log on the catch so an op
  // failure isn't observable-only-via-caller (the work itself logs
  // its own failure, this is a backstop for the chain-keeping path).
  let inFlight = Promise.resolve();
  function runSerialized(work) {
    // `.then(work, work)` — both fulfillment AND rejection handlers
    // call work(). INTENTIONAL: a prior op rejecting must NOT skip
    // the next op (that would break the serialization chain when
    // one op fails). Same `work` reference in both arms; called
    // exactly once per `runSerialized` call regardless of the
    // prior outcome.
    const next = inFlight.then(() => work(), () => work());
    inFlight = next.then(() => {}, (err) => {
      logger.debug('gateway-leader: serialized op rejected (chain continues)', {
        error: err && err.message,
      });
    });
    return next;
  }

  async function step() {
    // Heartbeat write + peer-list refresh run in parallel — both are
    // independent DDB ops, neither feeds into the other. Sequencing
    // them adds ~one DDB RTT to every tick for no correctness gain.
    // Each has independent error handling so a failure in one
    // doesn't sink the other. A heartbeat write failure is
    // recoverable (freshness window absorbs ~3 misses); a list
    // failure preserves the prior peer cache.
    await Promise.all([
      peerHeartbeat.writeHeartbeat().catch((err) => {
        logger.warn('gateway-leader: heartbeat write failed (will retry next tick)', {
          error: err.message,
        });
      }),
      peerHeartbeat.listFreshPeers().then((peers) => {
        knownPeerInstanceIds = new Set(peers.map((row) => row.instance_id));
      }).catch((err) => {
        logger.warn('gateway-leader: listFreshPeers failed (cache stays as-is)', {
          error: err.message,
        });
      }),
    ]);

    if (heldLock) {
      try {
        const renewed = await lock.renewLock();
        if (!renewed.renewed) {
          // CAS failed — peer took the lock out-of-band. Watchdog
          // will see heldLock=false and stop trying to connect; the
          // next tick will try acquire again.
          logger.warn('gateway-leader: lost lock (peer took over out-of-band)');
          heldLock = false;
        }
      } catch (err) {
        // Transient — keep heldLock as-is, retry next tick.
        logger.error('gateway-leader: renewLock threw, keeping heldLock for retry', {
          error: err.message,
        });
      }
    } else {
      try {
        const result = await lock.acquireLock();
        if (result.acquired) {
          logger.info('gateway-leader: cold-acquired lock', {
            shardId, version: result.version,
          });
          heldLock = true;
          // The connection watchdog will observe lock-held + WS-not-
          // connected on its next tick and call manager.connect().
        }
      } catch (err) {
        logger.error('gateway-leader: acquireLock threw', { error: err.message });
      }
    }
  }

  async function loop() {
    while (running) {
      await sleep(tickIntervalMs);
      if (!running) break;
      // Tick mutators (renew / acquire) MUST be serialized against
      // push-handoff and inbound-handoff. Heartbeat + peer-cache
      // refresh don't touch lock state, but bundling the whole step
      // is simpler than splitting the serialization boundary inside
      // it.
      // eslint-disable-next-line no-await-in-loop
      await runSerialized(step);
    }
  }

  function start() {
    // Guard on `loopPromise` (NOT just `running`) so a `start()`
    // after a `stop()` that hasn't yet observed the running=false
    // flag — the old loop is still inside `await sleep(...)` — does
    // not spawn a second concurrent loop. Callers that need to
    // re-start MUST `await stop()` first; the returned promise
    // resolves once the loop has actually exited.
    //
    // `closed` is permanent — once pushHandoff has run, start() is
    // a no-op. The leader is terminal after handoff (the SIGTERM
    // handler exits the process shortly after); a stray start()
    // from a wiring bug would otherwise resume the tick on a dead
    // task.
    if (loopPromise || closed) return;
    running = true;
    loopPromise = loop().finally(() => { loopPromise = null; });
  }

  // Halts the loop and returns a promise that resolves once the
  // last in-flight tick has completed. Callers that want to
  // re-start the leader MUST await this. Idempotent.
  function stop() {
    running = false;
    return loopPromise ?? Promise.resolve();
  }

  // Called by the gateway-control-channel server when a peer pushes
  // a handoff to us. Already HMAC-verified + routing-checked by the
  // server; this just does the lock-cursor + WS-connect side.
  //
  // Returns nothing on success. Throws on failure — the server
  // returns 500 to the active. Either branch is safe: the active
  // exits on any non-2xx, and the watchdog re-tries the connect
  // path if heldLock got set before the throw.
  async function handleInboundHandoff({ activeInstanceId, expectedVersion }) {
    return runSerialized(async () => {
      // Step 1: bootstrap version cursor. THROWS if expectedVersion
      // is malformed — protects against a future control-channel-server
      // bug that lets bad payloads through. Log at error level with
      // routing context so an unexpected adopt failure is visible
      // (the catch in runSerialized only logs at debug, which
      // wouldn't show in default-prod log levels).
      try {
        lock.adoptLockFromHandoff(expectedVersion);
      } catch (err) {
        logger.error('gateway-leader: adoptLockFromHandoff threw', {
          error: err.message, activeInstanceId, expectedVersion,
        });
        throw err;
      }
      // Step 2: flag held BEFORE connect. If connect throws, watchdog
      // picks up the retry. If we flipped the flag after connect,
      // the watchdog would race the connect and possibly redundantly
      // re-call it.
      heldLock = true;
      // Step 3: bring up the WS. The active is waiting for the ACK
      // (200 ms) before exiting; this connect resolving is what
      // makes the 200 "I'm live" semantic true.
      //
      // `connecting` is flipped true around the await so the
      // watchdog's parallel 1 s tick observes it via isConnecting()
      // and skips its own connect call. Without this guard a
      // Discord WS cold-connect (1-3 s) would race the watchdog
      // and produce two concurrent connect() invocations against
      // @discordjs/ws's WebSocketManager, which is NOT safe under
      // concurrency. Reset in `finally` so a connect throw still
      // clears the flag — the watchdog can then validly take over
      // the retry on the next tick.
      connecting = true;
      try {
        await manager.connect();
      } catch (err) {
        logger.error('gateway-leader: inbound-handoff connect threw (watchdog will retry)', {
          error: err.message, activeInstanceId,
        });
        throw err;
      } finally {
        connecting = false;
      }
      logger.info('gateway-leader: adopted lock + connected via inbound handoff', {
        activeInstanceId, expectedVersion,
      });
    });
  }

  // Called by the SIGTERM handler. Find peer, transfer, push, return.
  // Stops the tick loop first so the next tick can't race the
  // transferLock with a renewLock.
  //
  // TERMINAL: after this returns (any branch), the leader is dead.
  // The `closed` sentinel is latched so a subsequent `start()` is a
  // no-op — protects against a wiring bug in PR 13b.3 that might
  // accidentally re-start the leader post-SIGTERM. The SIGTERM
  // handler is still expected to `process.exit()` shortly after.
  async function pushHandoff() {
    running = false;
    closed = true;
    const cleanupHeartbeatRow = async () => {
      // Best-effort: close the discovery window immediately so the
      // freshly-promoted standby's listFreshPeers stops returning
      // this row, rather than waiting up to `freshnessWindowSeconds`
      // for the natural fade. Already-warned-and-logged inside
      // deleteOwnRow; the .catch() is belt-and-braces in case the
      // module surface changes.
      await peerHeartbeat.deleteOwnRow().catch(() => {});
    };

    return runSerialized(async () => {
      if (!heldLock) {
        logger.info('gateway-leader: pushHandoff called without holding lock (no-op)');
        await cleanupHeartbeatRow();
        return { pushed: false, reason: 'not_holding_lock' };
      }

      let peers;
      try {
        peers = await peerHeartbeat.listFreshPeers();
      } catch (err) {
        logger.error('gateway-leader: listFreshPeers failed during pushHandoff', {
          error: err.message,
        });
        // Best-effort release so the next replacement task can acquire
        // immediately rather than wait for TTL lapse.
        await lock.releaseLock().catch(() => {});
        await cleanupHeartbeatRow();
        return { pushed: false, reason: 'peer_lookup_failed' };
      }

      const peer = peers[0]; // freshest-first per listFreshPeers contract
      if (!peer) {
        logger.warn('gateway-leader: no fresh peer for handoff — falling through to cold-fallback');
        await lock.releaseLock().catch(() => {});
        await cleanupHeartbeatRow();
        return { pushed: false, reason: 'no_peer' };
      }

      // Defensive: if a heartbeat row exists without lock_holder
      // (back-compat with pre-PR-13b.2 callers or a partial-write
      // window), build a placeholder so transferLock still has a
      // valid string to write. The lock_holder field is operational
      // metadata; correctness doesn't depend on its value, only its
      // non-emptiness. TODO(post-13b.2-bake): drop this fallback
      // once all peers have been on >=13b.2 long enough that
      // missing lock_holder is impossible.
      const targetLockHolder = peer.lock_holder
        || `peer/${peer.instance_id}`;

      let transferResult;
      try {
        transferResult = await lock.transferLock(peer.instance_id, targetLockHolder);
      } catch (err) {
        logger.error('gateway-leader: transferLock threw', { error: err.message });
        await lock.releaseLock().catch(() => {});
        await cleanupHeartbeatRow();
        return { pushed: false, reason: 'transfer_threw' };
      }

      if (!transferResult.transferred) {
        // CAS failed — version moved (peer cold-acquired) or we
        // weren't actually holding. Either way the DDB row is no
        // longer ours, so flip the local flag to match.
        heldLock = false;
        logger.warn('gateway-leader: transferLock CAS failed; skipping push');
        await cleanupHeartbeatRow();
        return { pushed: false, reason: 'transfer_failed' };
      }

      heldLock = false;

      const result = await controlClient.pushHandoff({
        peerIp: peer.ip,
        peerPort: peer.port,
        peerInstanceId: peer.instance_id,
        selfInstanceId,
        expectedVersion: transferResult.version,
      });
      // Either branch is fine. The active is exiting anyway; the
      // standby has the lock either way (transferLock already moved
      // it in DDB). If the push didn't ACK, the standby's watchdog
      // sees lock-held + WS-disconnected within ~1 s and brings
      // up the gateway.
      if (result.ok) {
        logger.info('gateway-leader: pushHandoff ACKed', {
          peerInstanceId: peer.instance_id,
        });
      } else {
        logger.warn('gateway-leader: pushHandoff did not ACK (watchdog will catch)', {
          peerInstanceId: peer.instance_id, reason: result.reason,
        });
      }
      await cleanupHeartbeatRow();
      return { pushed: true, ackReason: result.ok ? 'ack' : result.reason };
    });
  }

  // For the connection-watchdog's releaseLock hook. Serialized so
  // it can't race a tick's renewLock.
  async function releaseLockForExit() {
    return runSerialized(async () => {
      heldLock = false;
      await lock.releaseLock();
    });
  }

  function isHoldingLock() {
    return heldLock;
  }

  // For the connection-watchdog: when the leader is itself mid-
  // `manager.connect()` (inbound-handoff path), the watchdog must
  // back off rather than fire its own concurrent connect.
  function isConnecting() {
    return connecting;
  }

  function isKnownPeer(instanceId) {
    return knownPeerInstanceIds.has(instanceId);
  }

  return {
    start,
    stop,
    pushHandoff,
    handleInboundHandoff,
    releaseLockForExit,
    isHoldingLock,
    isConnecting,
    isKnownPeer,
    // Inspection seams for tests.
    _stepForTest: () => runSerialized(step),
    _getKnownPeersForTest: () => new Set(knownPeerInstanceIds),
    _getLoopPromiseForTest: () => loopPromise,
  };
}

module.exports = {
  createGatewayLeader,
  DEFAULT_TICK_INTERVAL_MS,
};
