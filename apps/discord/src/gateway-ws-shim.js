// @discordjs/ws-backed gateway shim for the gateway tier (zero-
// downtime design, Pillar 2). Replaces the discord.js `Client` +
// `client.login()` shape with a direct `WebSocketManager` whose
// session callbacks point at the DDB-backed store
// (src/gateway-session-store.js).
//
// Why drop discord.js: under the event-shipper split (Pillar 1),
// the gateway tier doesn't run handlers in-process — it forwards
// every dispatch to SQS, and the worker tier reconstructs the
// interaction via `client.actions.InteractionCreate.handle`. The
// gateway's only job becomes "open a WebSocket, persist session,
// forward frames." discord.js's `Client` adds significant surface
// (REST cache, voice, presence, role/channel events, sharding
// helpers) that the gateway tier never uses. Dropping it shrinks
// the gateway's process to its essential role and removes the
// in-memory `WebSocketShard.sessionInfo` storage that prevents
// cross-process RESUME today.
//
// What stays in discord.js: the worker tier (PROCESS_ROLE=http +
// ENABLE_EVENT_SHIPPER=true) still constructs a Client for the
// reconstruction-and-dispatch path. So this shim is gateway-tier-
// only — flag-off keeps the legacy Client-on-gateway shape.
//
// Module shape: factory `createGatewayWsShim(...)` so tests inject
// dependencies (store mock, manager constructor mock) cleanly and
// multi-shard generalization in PR 14+ is a parameter change.
//
// ── Surface (called by index.js in the flag-on path) ──
//
//   - hydrate()          — read persisted session from DDB into the
//                          store's mirror; returns the hydrated info
//                          or null. Called once before start().
//   - start({ timeoutMs }) — wire callbacks, register dispatch
//                          listener, call manager.connect(). Resolves
//                          after the WS is open; rejects on timeout.
//   - stop({ flushFinal }) — cancel the budget guard, flush the
//                          session store, do NOT call
//                          manager.destroy() (see SIGTERM contract
//                          below).
//   - isReady()          — true after the first READY dispatch.
//   - onDispatch(handler) — register a (payload) => void listener
//                          for every gateway dispatch. Returns an
//                          unsubscribe function. Multiple listeners
//                          supported (the event-publisher in
//                          commit 4 attaches one; the gateway-
//                          activity ticker attaches another).
//   - getAppId()         — application id from READY, or null
//                          before READY fires. Used by
//                          registerCommands.
//   - getRest()          — REST instance (constructed with the
//                          token); exposed for registerCommands.
//
// ── SIGTERM contract: do NOT call manager.destroy() ──
//
// @discordjs/ws's `destroy()` calls `updateSessionInfo(shardId, null)`
// and sends a close-1000 frame unless `recover: Resume` is passed.
// Both signals invalidate the session on Discord's side. Cross-
// process RESUME requires Discord to PRESERVE the session in its
// ~60 s resume buffer past our exit, so the next process can
// RESUME (op 6) on it. The only way to achieve that is to drop the
// TCP connection without a clean close: persist final state, then
// `process.exit()` from the caller. Discord sees a network-level
// disconnect (not a clean close), holds the session, and the new
// process's IDENTIFY-or-RESUME decision goes through
// retrieveSessionInfo → returns the persisted row → @discordjs/ws
// issues RESUME.
//
// stop() therefore flushes the store's mirror to DDB and clears
// internal timers, but does not touch manager state. Caller is
// responsible for the `process.exit()` that drops the TCP socket.
//
// ── IDENTIFY budget guard ──
//
// Discord enforces a per-bot identify quota of 1000 per 24 h. An
// unexpected churn loop (e.g., another process contending for the
// same token, a malformed RESUME bouncing fresh sessions) can
// burn the entire budget in minutes. MAX_IDENTIFY_ATTEMPTS bounds
// the count of CONSECUTIVE IDENTIFYs without an intervening READY
// — when a successful READY lands the counter resets to zero.
//
// Reset-on-READY is what makes cap=1 safe for long-lived processes.
// Without it, the only IDENTIFY a task could ever do is its cold-
// start one — a network blip >60s (resume buffer expires on
// Discord's side) would burn the budget and ECS would crash-loop
// the task while Discord refused to accept fresh IDENTIFYs from
// the replacement. With reset-on-READY, every successful session
// gets a fresh budget for the next reconnect.
//
// What the cap still catches: IDENTIFY-without-READY loops. A
// token-contention scenario (two processes claiming the same
// identity) produces fast IDENTIFY-reject churn with no READY
// arriving between attempts — the counter never resets, the cap
// trips on the second attempt, and the task exits cleanly
// instead of burning Discord's per-bot quota.

const { WebSocketManager, WebSocketShardEvents } = require('@discordjs/ws');
const { REST } = require('@discordjs/rest');

// Tightly bounded per the budget rationale above.
const MAX_IDENTIFY_ATTEMPTS = 1;

// Default connect-timeout matches the legacy client.login() timeout
// (30 s in index.js). Discord's IDENTIFY → READY round-trip is
// typically under 5 s; 30 s is a generous ceiling that surfaces
// "Discord API unreachable" as a fast-fail rather than a hang.
const DEFAULT_CONNECT_TIMEOUT_MS = 30_000;

function createGatewayWsShim({
  token,
  intents,
  store,
  logger,
  // Test seam: injectable WebSocketManager constructor + REST class.
  // Production passes neither and the @discordjs/ws/@discordjs/rest
  // imports above are used. Tests inject mocks to assert
  // construction args + emit fake dispatch events.
  WebSocketManagerCtor = WebSocketManager,
  RESTCtor = REST,
  rest, // pre-built REST instance (test seam); production constructs internally
} = {}) {
  if (!token) throw new Error('createGatewayWsShim: token is required');
  if (typeof intents !== 'number') throw new Error('createGatewayWsShim: intents (number) is required');
  if (!store) throw new Error('createGatewayWsShim: store is required');
  if (!logger) throw new Error('createGatewayWsShim: logger is required');

  // ── Internal state ──
  let manager = null;
  let restInstance = rest;
  let isReady = false;
  let appId = null;
  let identifyAttempts = 0;
  // Two distinct flags:
  //
  //   `stopped`       — "drop late dispatches." Set by start()'s
  //                     catch on connect failure AND by stop(). The
  //                     Dispatch listener guards on this to ignore
  //                     frames arriving in the boot-teardown race
  //                     or after stop().
  //   `stopCompleted` — "stop() has already run cleanup once." stop()
  //                     idempotency gate; without splitting these,
  //                     start()-fail → stopped=true → gracefulShutdown
  //                     calls shim.stop() → stop() short-circuits on
  //                     stopped=true → flushFinal + listener detach
  //                     never run. The split keeps the dispatch guard
  //                     wide AND lets stop() actually clean up on
  //                     the failed-start path.
  let stopped = false;
  let stopCompleted = false;
  // Set rather than array for O(1) unsubscribe. The fan-out path
  // (Dispatch listener) iterates this once per gateway frame; Set
  // iteration is fine at expected handler counts (~2-3 today:
  // event-publisher + noteGatewayActivity).
  const dispatchHandlers = new Set();

  function buildRetrieveCallback() {
    // Wraps store.retrieveSessionInfo with the IDENTIFY budget
    // guard. When the store mirror is non-null, the wrapper is a
    // pass-through (no budget impact — we're RESUMing). When the
    // mirror is null, every call is a pending IDENTIFY; throw past
    // MAX_IDENTIFY_ATTEMPTS so a churn loop fails fast.
    return (shardId) => {
      const info = store.retrieveSessionInfo(shardId);
      if (info !== null) {
        return info;
      }
      identifyAttempts += 1;
      if (identifyAttempts > MAX_IDENTIFY_ATTEMPTS) {
        // The thrown error propagates through @discordjs/ws's
        // identify path and rejects the in-flight connect()
        // promise; start() surfaces it to the caller, which
        // routes through gracefulShutdown(1). After ECS task
        // replacement, the new process's budget counter resets
        // — a fresh shot at the resume window.
        const err = new Error(`gateway-ws-shim: IDENTIFY budget exhausted (${identifyAttempts} attempts; cap ${MAX_IDENTIFY_ATTEMPTS})`);
        err.code = 'GATEWAY_IDENTIFY_BUDGET';
        throw err;
      }
      logger.info('gateway-ws-shim: IDENTIFY pending', {
        attempt: identifyAttempts,
        cap: MAX_IDENTIFY_ATTEMPTS,
      });
      return null;
    };
  }

  function buildUpdateCallback() {
    // Pass-through to the store. Throttle / write / delete behavior
    // is the store's concern; the shim doesn't second-guess.
    return (shardId, info) => store.updateSessionInfo(shardId, info);
  }

  return {
    // Pre-flight hydration. Called once by the caller before
    // start() so the manager's first retrieveSessionInfo invocation
    // sees the mirror, not null. Returns whatever the store
    // returned (info or null) so the caller can log the RESUME-
    // path-vs-cold-start SLI.
    async hydrate() {
      return store.hydrate();
    },

    async start({ timeoutMs = DEFAULT_CONNECT_TIMEOUT_MS } = {}) {
      if (manager) {
        throw new Error('gateway-ws-shim: start() called twice (manager already constructed)');
      }
      if (stopped) {
        throw new Error('gateway-ws-shim: start() after stop() is unsupported');
      }

      // REST is lazy-constructed if the caller didn't inject one.
      // The single token-bound instance is reused by registerCommands
      // (which accepts REST + appId rather than a discord.js Client).
      if (!restInstance) {
        restInstance = new RESTCtor().setToken(token);
      }

      manager = new WebSocketManagerCtor({
        token,
        intents,
        rest: restInstance,
        retrieveSessionInfo: buildRetrieveCallback(),
        updateSessionInfo: buildUpdateCallback(),
      });

      // Dispatch listener: pluck the appId from READY, mirror the
      // legacy `client.once('ready')` semantics, and fan out to
      // user-registered handlers (the event-publisher, the
      // gateway-activity ticker).
      manager.on(WebSocketShardEvents.Dispatch, ({ data, shardId }) => {
        // Guard against the connect-timeout-then-late-WS-open race:
        // start() attaches listeners before Promise.race against the
        // connect timeout. If the race rejects but manager.connect()
        // continues opening the WS, dispatches arriving during the
        // gracefulShutdown(1) teardown window would otherwise trigger
        // downstream side effects (publish-to-SQS, registerCommands).
        if (stopped) return;
        const eventType = data?.t;
        if (eventType === 'READY') {
          // application.id is the OAuth2 application snowflake —
          // identical to client.application.id in the legacy path.
          // RegisterCommands needs this to address the global-
          // commands or guild-commands endpoint.
          appId = data?.d?.application?.id ?? null;
          isReady = true;
          // Reset the IDENTIFY budget: every successful READY
          // restores a fresh allowance for the next reconnect.
          // See module header.
          identifyAttempts = 0;
          logger.info('gateway-ws-shim: READY received', {
            shardId,
            appIdPrefix: appId ? appId.slice(0, 6) : null,
          });
        } else if (eventType === 'RESUMED') {
          // RESUMED is Discord's ACK that a `RESUME` op succeeded —
          // the production happy path for Pillar 2 (cross-process
          // resume from a persisted DDB row). Without flipping
          // isReady on this path, /health would stay 503, ECS would
          // replace the task after --start-period elapses, and the
          // resume win would be defeated.
          //
          // No appId update on resume — the application id doesn't
          // change between processes for the same bot identity, so
          // the value hydrated from the previous READY (or null if
          // we never observed one in this process) is fine. Reset
          // the IDENTIFY budget so a subsequent disconnect-then-
          // reconnect path gets a fresh allowance the same way
          // READY would.
          isReady = true;
          identifyAttempts = 0;
          logger.info('gateway-ws-shim: RESUMED received', { shardId });
        }
        // Fan-out. Each handler runs synchronously; any thrown error
        // is caught and logged so one bad handler doesn't break the
        // others (matches discord.js's EventEmitter forgiveness shape).
        for (const handler of dispatchHandlers) {
          try {
            handler({ data, shardId });
          } catch (err) {
            logger.warn('gateway-ws-shim: dispatch handler threw', {
              error: err.message,
              eventType: data?.t,
            });
          }
        }
      });

      // Shard errors aren't fatal — @discordjs/ws reconnects
      // automatically on most failure shapes. Logging at warn
      // matches the legacy client.on('error') level.
      manager.on(WebSocketShardEvents.Error, ({ error, shardId }) => {
        logger.warn('gateway-ws-shim: shard error', {
          shardId,
          error: error?.message ?? String(error),
        });
      });

      // Race connect() against a deadline. Without this, an
      // unreachable Discord API would hang the boot indefinitely
      // (same hazard the legacy client.login() timeout closed).
      // .unref() prevents the dangling timer from pinning the
      // event loop after a successful connect.
      //
      // On timeout, the losing side of Promise.race leaves
      // manager.connect()'s in-flight WS attempt pending. We don't
      // call manager.destroy() here (see SIGTERM contract in
      // module header); manager lifecycle is owned by stop().
      // The caller's gracefulShutdown(1) → process.exit() unwinds
      // the dangling promise as the process tears down — acceptable
      // because the boot is failing anyway.
      let timeoutHandle;
      try {
        await Promise.race([
          manager.connect(),
          new Promise((_, reject) => {
            timeoutHandle = setTimeout(
              () => reject(new Error(`gateway-ws-shim: connect timed out after ${timeoutMs}ms`)),
              timeoutMs,
            );
            timeoutHandle.unref();
          }),
        ]);
      } catch (err) {
        // Connect failed (timeout, identify budget exhaustion, or
        // a Discord-side rejection). Set `stopped=true` so the
        // Dispatch listener's guard drops any frames that arrive
        // in the race window between this throw and the caller's
        // gracefulShutdown → shim.stop() call (the legacy
        // gracefulShutdown awaits db.close() etc. before reaching
        // stop()). Without the early flip, a WS that finishes
        // opening mid-teardown would still side-effect through
        // registerCommands / eventPublisher / gateway-activity.
        // Leave the manager handle intact so stop() can clean
        // it up.
        stopped = true;
        if (timeoutHandle) clearTimeout(timeoutHandle);
        throw err;
      }
      if (timeoutHandle) clearTimeout(timeoutHandle);
    },

    async stop({ flushFinal: shouldFlush = true } = {}) {
      // Idempotent: a second SIGTERM/SIGINT racing the first shouldn't
      // re-flush the store or re-strip listeners.
      if (stopCompleted) return;
      stopCompleted = true;
      // Also flip `stopped` so the Dispatch listener drops any
      // frames that may still arrive on the way out. (start() may
      // have already set this on a failed-connect path; the flip
      // here is idempotent.)
      stopped = true;
      // Drop dispatch handlers so any late dispatch arriving on
      // the way out doesn't trigger a downstream side effect.
      dispatchHandlers.clear();
      if (shouldFlush) {
        // Persists the latest sequence so the next process's
        // RESUME picks up cleanly. After this, the store transitions
        // to stopped=true and rejects further updates.
        await store.flushFinal();
      } else {
        store.stop();
      }
      // NO manager.destroy() — see SIGTERM contract in module header.
      // The caller's process.exit() drops the TCP socket; Discord
      // holds the session in its 60 s buffer for the next process's
      // RESUME. Detach only the listeners we installed; an
      // unscoped removeAllListeners() could strip @discordjs/ws's
      // own internal listeners on the same emitter.
      if (manager) {
        manager.removeAllListeners(WebSocketShardEvents.Dispatch);
        manager.removeAllListeners(WebSocketShardEvents.Error);
      }
      manager = null;
    },

    isReady() {
      return isReady;
    },

    onDispatch(handler) {
      if (typeof handler !== 'function') {
        throw new Error('gateway-ws-shim: onDispatch handler must be a function');
      }
      dispatchHandlers.add(handler);
      return () => dispatchHandlers.delete(handler);
    },

    // Null until the first READY in this process. Pure-RESUMED boots
    // (no IDENTIFY → no READY) leave this null for the process
    // lifetime — appId isn't persisted alongside the session row.
    // Callers MUST null-check before templating into a REST endpoint.
    getAppId() {
      return appId;
    },

    // Null until start() resolves.
    getRest() {
      return restInstance;
    },

    // ── Test-only inspection ──
    _getManagerForTest() {
      return manager;
    },
    _getIdentifyAttemptsForTest() {
      return identifyAttempts;
    },
  };
}

module.exports = {
  createGatewayWsShim,
  MAX_IDENTIFY_ATTEMPTS,
  DEFAULT_CONNECT_TIMEOUT_MS,
};
