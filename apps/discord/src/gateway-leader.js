// Per-shard leader coordinator for Pillar 3. Composes:
//   - gateway-lock           (the DDB CAS lock primitive)
//   - gateway-peer-heartbeat (standby discovery)
//   - gateway-control-client (outbound push-handoff)
//   - manager                (the @discordjs/ws shim — connect()/isConnected())
//
// And exposes the four hooks the wiring layer (index.js, PR 13b.3)
// plugs into the other Pillar 3 components:
//   - `isHoldingLock()` → for gateway-connection-watchdog
//   - `releaseLockForImmediateExit()` → for gateway-connection-watchdog's
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
//   2. If no peer → release lock (best-effort) + return
//      {transferred: false, reason: 'no_peer'}. Cold-fallback floor
//      applies on the other side.
//   3. transferLock(peer.instance_id, peer.lock_holder) — atomic
//      DDB ownership move. On CCF (active lost the lock to a cold-
//      fallback acquire by the peer) → return {transferred: false,
//      reason: 'transfer_failed'}.
//   4. controlClient.pushHandoff(...) with expectedVersion = the
//      post-transfer version returned by transferLock.
//   5. Return {transferred: true, pushAcked: bool, pushReason?}. The
//      `transferred` field reflects DDB ownership (the standby owns
//      the lock); `pushAcked` reflects whether the standby's HTTP
//      ACK arrived. The watchdog covers the !pushAcked case. Caller
//      (index.js SIGTERM handler) exits either way.

const DEFAULT_TICK_INTERVAL_MS = 2_000;
// Internal ceiling for an inbound-handoff `manager.connect()` await.
// Discord WS cold-connects normally complete in 1-3s; 5s is generous
// headroom while still bounding a hung resolve. On timeout we settle
// the promise as a connect failure (heldLock stays true, watchdog
// takes over next tick). Makes the leader self-sufficient rather
// than relying on the WS shim's wiring to enforce a timeout.
const DEFAULT_INBOUND_CONNECT_TIMEOUT_MS = 5_000;

function createGatewayLeader({
  lock,
  peerHeartbeat,
  controlClient,
  manager,
  selfInstanceId,
  shardId,
  logger,
  tickIntervalMs = DEFAULT_TICK_INTERVAL_MS,
  inboundConnectTimeoutMs = DEFAULT_INBOUND_CONNECT_TIMEOUT_MS,
  // Injected for tests. Production: setTimeout-based sleep.
  sleep = (ms) => new Promise((resolve) => { setTimeout(resolve, ms); }),
} = {}) {
  if (!lock) throw new Error('createGatewayLeader: lock is required');
  if (!peerHeartbeat) throw new Error('createGatewayLeader: peerHeartbeat is required');
  if (!controlClient) throw new Error('createGatewayLeader: controlClient is required');
  if (!manager
      || typeof manager.connect !== 'function'
      || typeof manager.isConnected !== 'function') {
    throw new Error('createGatewayLeader: manager with connect() and isConnected() is required');
  }
  if (!selfInstanceId) throw new Error('createGatewayLeader: selfInstanceId is required');
  if (!shardId) throw new Error('createGatewayLeader: shardId is required');
  if (!logger) throw new Error('createGatewayLeader: logger is required');
  // tickIntervalMs=0 would saturate the renew loop; negative would
  // produce surprising setTimeout behavior. Fail loud at boot.
  if (!Number.isInteger(tickIntervalMs) || tickIntervalMs <= 0) {
    throw new Error('createGatewayLeader: tickIntervalMs must be a positive integer');
  }
  // inboundConnectTimeoutMs=0 would race the timer against the
  // connect immediately; negative would be undefined behavior.
  if (!Number.isInteger(inboundConnectTimeoutMs) || inboundConnectTimeoutMs <= 0) {
    throw new Error('createGatewayLeader: inboundConnectTimeoutMs must be a positive integer');
  }

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

  // Best-effort lock release for pushHandoff's bail-out paths. The
  // leader is exiting; we want to give up the lock so the next
  // replacement task can cold-acquire immediately instead of waiting
  // for TTL lapse. But a failure here is non-fatal — TTL absorbs it
  // — so the caller does not propagate. Swallowing silently would
  // hide a recurring DDB outage, so log at warn instead of `() => {}`.
  async function releaseLockBestEffort(reason) {
    try {
      await lock.releaseLock();
    } catch (err) {
      logger.warn('gateway-leader: best-effort releaseLock failed (TTL will reap)', {
        reason, error: err && err.message,
      });
    }
  }

  // ── Mutator serialization ──
  // Every call site that touches the single-caller-only gateway-lock
  // API funnels through `runSerialized` so tick / inbound-handoff /
  // push-handoff never overlap. The serialization chain absorbs
  // failures so one op's rejection doesn't break the next op's
  // turn — see the function-level comment for the load-bearing
  // invariant. The `opName` argument is included in the chain-catch
  // debug log so a future caller that bypasses an op-level try/catch
  // is trivially recognizable in logs (vs. an unlabeled backstop
  // firing without context).
  let inFlight = Promise.resolve();
  function runSerialized(opName, work) {
    // The load-bearing invariant: `inFlight` is ALWAYS fulfilled
    // (never rejected) because we assign it below to a `.then` whose
    // rejection arm returns undefined — turning every prior failure
    // into a fulfilled chain. That means the NEXT `runSerialized`
    // call's `inFlight.then(() => work())` reliably runs `work()`
    // regardless of whether the previous op rejected. This is what
    // keeps the serialization chain alive across failures; the
    // dual-arm `.then` we used to have on the consumer side was
    // over-defending against a state that can't happen.
    //
    // The returned `next` DOES propagate the work's rejection to the
    // caller — so callers can `.catch` their own op's failure — but
    // the side-effect `inFlight` assignment diverges and absorbs it.
    // Returned-promise vs chain-keeping behavior differ on purpose.
    const next = inFlight.then(() => work());
    inFlight = next.then(() => {}, (err) => {
      // Debug (not warn): every rethrowing op (tick / handleInboundHandoff
      // / pushHandoff) ALREADY logs at warn/error from its own
      // try/catch before throwing. Logging again at warn here
      // double-reports the same failure under a different message,
      // confusing log readers into thinking two things failed. Drop
      // to debug so the chain-keeping observability is still
      // available for whoever ever needs it (e.g., investigating a
      // backstop firing for a future caller that bypassed the
      // op-level try/catch) without polluting prod log levels.
      logger.debug('gateway-leader: serialized op rejected (chain continues)', {
        op: opName, error: err && err.message,
      });
    });
    return next;
  }

  async function step() {
    // Terminal-state guard: mirrors handleInboundHandoff's inner
    // closed re-check. The production loop already stops via
    // running=false, but `_stepForTest` enters through the same
    // serialized chain — without this guard a stray post-close test
    // call would re-write heartbeat / re-call renewLock / re-acquire,
    // diverging from the closed-sentinel invariant the rest of the
    // module enforces.
    if (closed) return;
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
      //
      // Backstop for unexpected throws inside step() that escape
      // its internal try/catch (e.g., a future row-shape regression
      // that throws inside `peers.map(...)`). Without this, a single
      // throw would resolve loopPromise as a rejection, the loop
      // would exit silently, and the leader would stop renewing
      // without anyone observing it. Mirrors the watchdog's
      // backstop. Log + continue: the next tick re-tries.
      try {
        // eslint-disable-next-line no-await-in-loop
        await runSerialized('tick', step);
      } catch (err) {
        logger.error('gateway-leader: tick threw unexpectedly (loop continues)', {
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
  // @param {object} arg
  // @param {string} arg.activeInstanceId  who's transferring to us
  // @param {number} arg.expectedVersion   the lock version to adopt
  // @returns {Promise<void>}              resolves on success
  // @throws on adopt failure, connect failure, or stray-handoff
  //
  // Server returns 500 to the active on any throw. Either branch is
  // safe: the active exits on any non-2xx, and the watchdog re-tries
  // the connect path if heldLock got set before the throw.
  async function handleInboundHandoff({ activeInstanceId, expectedVersion }) {
    // Terminal-state guard: a post-pushHandoff leader is dead.
    // A second inbound handoff arriving (e.g., racing the SIGTERM
    // path) must NOT re-adopt — the process is exiting. Throw so
    // the control-channel server returns 500 and the active gives
    // up. Symmetric to start()'s same guard.
    if (closed) {
      logger.warn('gateway-leader: inbound handoff rejected — leader closed', {
        activeInstanceId, expectedVersion,
      });
      throw new Error('leader_closed');
    }
    return runSerialized('inbound-handoff', async () => {
      // Inner closed re-check: a race window exists between the
      // outer check above and the moment this closure starts.
      // Sequence: inbound passes outer check → queued in chain
      // behind a tick → SIGTERM fires → pushHandoff sets
      // closed=true AND queues its own work → tick finishes → THIS
      // closure runs and would adopt/connect against a soon-to-be-
      // transferred lock. The re-check makes the closed sentinel
      // load-bearing inside the serial chain, not just at the API
      // entry point. Throw so the server returns 500.
      if (closed) {
        logger.warn('gateway-leader: inbound handoff aborted mid-queue — leader closed', {
          activeInstanceId, expectedVersion,
        });
        throw new Error('leader_closed');
      }
      // Stray-handoff guard: if we already hold the lock AND the WS
      // is connected, a second inbound handoff is either a duplicate
      // from a retry-ing active or a misrouted body (the server-side
      // peer_instance_id binding + isKnownPeer check make this
      // low-probability but not impossible — e.g., a cold-fallback
      // race where this replica acquired the lock just before the
      // peer's push body arrived). Re-adopting would re-anchor
      // currentVersion against a row that may have moved; re-
      // calling manager.connect() would race the existing WS state
      // (@discordjs/ws's WebSocketManager is NOT concurrent-safe).
      // Reject cleanly — the active will exit anyway.
      if (heldLock && manager.isConnected()) {
        logger.warn('gateway-leader: inbound handoff rejected — already holding lock + connected', {
          activeInstanceId, expectedVersion,
        });
        throw new Error('already_holding_lock_and_connected');
      }
      // Step 1: bootstrap version cursor. THROWS if expectedVersion
      // is malformed — protects against a future control-channel-server
      // bug that lets bad payloads through. Log at error level with
      // routing context so an unexpected adopt failure carries the
      // active_instance_id + expectedVersion in the same line as
      // the error message — the runSerialized backstop is a generic
      // warn without that routing context.
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
      //
      // Internal timeout: a hung `manager.connect()` (e.g., Discord
      // WS endpoint resolution black-holed) would otherwise pin
      // `connecting=true` indefinitely and the watchdog would
      // no-op every tick. Racing connect against a timer lets us
      // throw on timeout so the caller (control-channel server)
      // returns 500 to the active.
      //
      // Critical: `connecting=true` MUST stay true until the
      // UNDERLYING `manager.connect()` actually settles — not just
      // until `Promise.race` resolves. Otherwise: timeout fires →
      // race resolves → flag clears → next watchdog tick (1s) sees
      // !isConnecting and fires its OWN `manager.connect()` while
      // the original is STILL pending in @discordjs/ws's internal
      // state. That's exactly the concurrent-connect race the flag
      // exists to prevent.
      //
      // Pattern: start the connect, attach the flag-clear to its
      // settlement, then race the timer separately. The handler
      // returns when EITHER settles, but the flag stays held until
      // the underlying connect resolves or rejects.
      //
      // Bounded-settlement assumption: @discordjs/ws's
      // WebSocketManager.connect() is contracted to settle in
      // bounded time — the shim wraps it and the underlying
      // WebSocket has its own handshake/heartbeat timeouts. A
      // truly-never-settling connect would pin `connecting=true`
      // forever and the watchdog would no-op every tick, leaving
      // a stuck standby. We trust the upstream contract here
      // rather than racing ANOTHER timer (which would re-introduce
      // the concurrent-connect race this guard exists to prevent).
      // If a future @discordjs/ws version ever drops that contract,
      // either set a hard ceiling here that accepts the concurrent
      // risk, or wire a `manager.destroy()`-on-timeout escape in
      // the shim layer. Tracking issue #415 covers a process-level
      // health check (alert on `connecting=true` duration > 30s
      // and exit(1) so ECS replaces the task) — preferred recovery
      // over racing another timer.
      connecting = true;
      // `manager.connect()` is inside the try so a synchronous throw
      // (defensive — @discordjs/ws's WebSocketManager.connect contract
      // is async, but a future shim regression or wiring bug could
      // surface a sync throw or a non-thenable return) still clears
      // `connecting=false`. We track lifecycle attachment success
      // explicitly (rather than just `!!connectPromise`) because
      // `.then` itself can throw if `connect()` returned a non-
      // thenable — in that case `connectPromise` is set but the
      // settlement handlers never registered, so the catch is the
      // only clear.
      let connectPromise;
      let lifecycleAttached = false;
      let connectTimer;
      try {
        connectPromise = manager.connect();
        connectPromise.then(
          () => { connecting = false; },
          () => { connecting = false; },
        );
        lifecycleAttached = true;
        await Promise.race([
          connectPromise,
          new Promise((_, reject) => {
            connectTimer = setTimeout(
              () => reject(new Error(`inbound_connect_timeout_${inboundConnectTimeoutMs}ms`)),
              inboundConnectTimeoutMs,
            );
          }),
        ]);
      } catch (err) {
        // Sync-throw path: if `manager.connect()` itself throws OR
        // returns a non-thenable (in which case `.then` is undefined
        // on the return value and the call site throws synchronously
        // BEFORE `lifecycleAttached` flips true), the lifecycle
        // handlers never registered. This catch is then the only
        // clear for `connecting=false`. The async-rejection path is
        // owned by the .then() handlers, so we DON'T touch
        // `connecting` there (would race the next tick's connect;
        // see comment in finally).
        if (!lifecycleAttached) connecting = false;
        logger.error('gateway-leader: inbound-handoff connect threw (watchdog will retry)', {
          error: err.message, activeInstanceId,
        });
        throw err;
      } finally {
        clearTimeout(connectTimer);
        // Note: NO `connecting = false` here on the async path —
        // that's owned by the connectPromise settlement handlers.
        // Clearing it here on a timeout would race the still-pending
        // connect against the next watchdog tick's fresh connect().
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

    const result = await runSerialized('pushHandoff', async () => {
      if (!heldLock) {
        logger.info('gateway-leader: pushHandoff called without holding lock (no-op)');
        return { transferred: false, reason: 'not_holding_lock' };
      }

      let peers;
      try {
        peers = await peerHeartbeat.listFreshPeers();
      } catch (err) {
        logger.error('gateway-leader: listFreshPeers failed during pushHandoff', {
          error: err.message,
        });
        await releaseLockBestEffort('peer_lookup_failed');
        return { transferred: false, reason: 'peer_lookup_failed' };
      }

      const peer = peers[0]; // freshest-first per listFreshPeers contract
      if (!peer) {
        logger.warn('gateway-leader: no fresh peer for handoff — falling through to cold-fallback');
        await releaseLockBestEffort('no_peer');
        return { transferred: false, reason: 'no_peer' };
      }

      // Defensive: if a heartbeat row exists without lock_holder
      // (back-compat with pre-PR-13b.2 callers or a partial-write
      // window), build a placeholder so transferLock still has a
      // valid string to write. The lock_holder field is operational
      // metadata; correctness doesn't depend on its value, only its
      // non-emptiness. Prefix `placeholder/` so the fallback is
      // obvious in transferLock traces — vs. a real holder string
      // which is shaped like `task-arn:.../inst-X`.
      // TODO(post-13b.2-bake): drop this fallback once all peers
      // have been on >=13b.2 long enough that missing lock_holder
      // is impossible. Target removal: 2026-07-01 (~6 weeks after
      // 13b.2 lands in prod). Tracked: #416.
      // `??` (not `||`) so an empty-string or 0-valued row from a
      // misbehaving writer doesn't silently take the placeholder
      // branch. heartbeat write-time validation enforces non-empty
      // string, but defense-in-depth on the read side too.
      const targetLockHolder = peer.lock_holder
        ?? `placeholder/${peer.instance_id}`;

      let transferResult;
      try {
        transferResult = await lock.transferLock(peer.instance_id, targetLockHolder);
      } catch (err) {
        logger.error('gateway-leader: transferLock threw', { error: err.message });
        await releaseLockBestEffort('transfer_threw');
        return { transferred: false, reason: 'transfer_threw' };
      }

      if (!transferResult.transferred) {
        // CAS failed — version moved (peer cold-acquired) or we
        // weren't actually holding. Either way the DDB row is no
        // longer ours, so flip the local flag to match.
        heldLock = false;
        logger.warn('gateway-leader: transferLock CAS failed; skipping push');
        return { transferred: false, reason: 'transfer_failed' };
      }

      heldLock = false;

      // The control-client's documented contract is "never throws"
      // (returns a result object). Wrap defensively anyway: the
      // synchronous validators inside pushHandoff (peerIp/peerPort/
      // expectedVersion shape) DO throw if a row makes it past
      // listFreshPeers' filters in a future regression — and the
      // SIGTERM caller cannot handle exceptions cleanly. Mirrors the
      // transferLock try/catch posture above. transferLock has
      // already moved the lock in DDB, so the standby owns it either
      // way; the watchdog catches lock-held + WS-disconnected within
      // ~1 s and brings up the gateway from the cold-fallback path.
      let pushResult;
      try {
        pushResult = await controlClient.pushHandoff({
          peerIp: peer.ip,
          peerPort: peer.port,
          peerInstanceId: peer.instance_id,
          selfInstanceId,
          expectedVersion: transferResult.version,
        });
      } catch (err) {
        logger.error('gateway-leader: controlClient.pushHandoff threw (watchdog will catch)', {
          peerInstanceId: peer.instance_id, error: err && err.message,
        });
        return { transferred: true, pushAcked: false, pushReason: 'push_threw' };
      }
      // Either ACK branch is fine. The active is exiting anyway; the
      // standby has the lock either way (transferLock already moved
      // it in DDB). If the push didn't ACK, the standby's watchdog
      // sees lock-held + WS-disconnected within ~1 s and brings
      // up the gateway.
      if (pushResult.ok) {
        logger.info('gateway-leader: pushHandoff ACKed', {
          peerInstanceId: peer.instance_id,
        });
        return { transferred: true, pushAcked: true };
      }
      logger.warn('gateway-leader: pushHandoff did not ACK (watchdog will catch)', {
        peerInstanceId: peer.instance_id, reason: pushResult.reason,
      });
      return { transferred: true, pushAcked: false, pushReason: pushResult.reason };
    });

    // Best-effort heartbeat-row cleanup. Deliberately OUTSIDE the
    // serialized chain: deleteOwnRow does not touch gateway-lock
    // state (it's a different DDB table), so serializing it would
    // only add latency to the SIGTERM critical path. Failure is
    // swallowed — the worst case is the row fades naturally inside
    // the freshness window.
    //
    // Timing note: this runs AFTER the serialized work completes,
    // so during the window from `closed=true` (set above) through
    // the end of the serialized chain, our heartbeat row is still
    // visible. A concurrent inbound from another peer would see
    // our row in their listFreshPeers head, but the inner closed
    // re-check (in handleInboundHandoff) makes the leader reject
    // any work that got queued post-`closed`. After this call
    // returns, peer-side listFreshPeers stops returning our row.
    // `peerHeartbeat.deleteOwnRow()` is documented as never-throwing
    // (logs+swallows internally), but defense-in-depth: belt-and-
    // braces .catch so a future regression in that contract doesn't
    // poison the pushHandoff result. Debug-level — a real fault here
    // is already surfaced by the inner warn log in deleteOwnRow.
    await peerHeartbeat.deleteOwnRow().catch((err) => {
      logger.debug('gateway-leader: deleteOwnRow rejected (contract regression?)', {
        error: err && err.message,
      });
    });

    return result;
  }

  // For the connection-watchdog's releaseLock hook. Serialized so
  // it can't race a tick's renewLock.
  //
  // Name is deliberately `releaseLockForImmediateExit` (not just
  // `releaseLock`): the implementation clears `heldLock=false`
  // BEFORE awaiting the DDB release. If releaseLock throws, the
  // local flag is already false but the DDB row may still belong
  // to us until TTL lapse. This is OK ONLY because every documented
  // caller `exit(1)`s immediately afterward — the row fades
  // naturally via TTL. A future non-exiting caller would need to
  // flip the ordering (await release first, then clear the flag on
  // success); the function name surfaces that contract at the
  // call site instead of relying on a comment.
  async function releaseLockForImmediateExit() {
    // Terminal-state guard: if pushHandoff already ran, we've already
    // released (best-effort) and `heldLock=false`. A subsequent watchdog
    // call here would no-op anyway, but short-circuiting makes the
    // closed-state invariant uniform across the API surface (start()
    // and handleInboundHandoff also guard on closed).
    if (closed) {
      logger.debug('gateway-leader: releaseLockForImmediateExit called on closed leader (no-op)');
      return;
    }
    await runSerialized('releaseLockForImmediateExit', async () => {
      // Inner closed re-check: mirrors handleInboundHandoff's
      // closure-level guard. The outer check catches the post-
      // pushHandoff sequential case, but a watchdog call queued
      // BEFORE pushHandoff flipped closed could still pop here
      // after closed=true has latched. Consequence in practice is
      // only a redundant releaseLock call (the sole caller exits
      // right after), but the inner guard keeps the closed-state
      // invariant uniform across the API surface.
      if (closed) return;
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

  // True when the tick loop has been started AND has not yet
  // exited (loopPromise nulls itself via `.finally` when the loop
  // returns — start-fail, stop(), or post-pushHandoff drain all
  // clear it). Consumed by index.js's /health probe on the standby
  // path so a standby that has no WS by design isn't reported as
  // unhealthy.
  //
  // Limitation worth knowing: this is loop-exists, NOT loop-is-
  // progressing. A tick wedged inside a hanging DDB call (lock
  // renew, heartbeat write, peer-cache refresh) would still report
  // healthy here. Acceptable for now — the 2 s renew + 60 s lock
  // TTL means a hung loop loses the lock to a peer's cold-acquire
  // within ~1 min anyway. Tracked: issue #420 (lastTickAt
  // freshness check follow-up).
  function hasStartedTickLoop() {
    return loopPromise !== null;
  }

  return {
    start,
    stop,
    pushHandoff,
    handleInboundHandoff,
    releaseLockForImmediateExit,
    isHoldingLock,
    isConnecting,
    isKnownPeer,
    hasStartedTickLoop,
    // Inspection seams for tests.
    _stepForTest: () => runSerialized('tick', step),
    _getKnownPeersForTest: () => new Set(knownPeerInstanceIds),
    _getLoopPromiseForTest: () => loopPromise,
  };
}

module.exports = {
  createGatewayLeader,
  DEFAULT_TICK_INTERVAL_MS,
  DEFAULT_INBOUND_CONNECT_TIMEOUT_MS,
};
