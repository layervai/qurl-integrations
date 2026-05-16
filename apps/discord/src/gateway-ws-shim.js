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
//   - rest               — the REST instance (constructed with the
//                          token); exposed for registerCommands.
//
// ── SIGTERM contract: do NOT call manager.destroy() ──
//
// @discordjs/ws@1.2.3's `destroy()` (dist/index.js around line 733)
// calls `updateSessionInfo(shardId, null)` and sends a close-1000
// frame unless `recover: Resume` is passed. Both signals invalidate
// the session on Discord's side. For Pillar 2 cross-process RESUME,
// we need Discord to PRESERVE the session in its 60 s resume buffer
// past our exit, so the next process can RESUME (op 6) on it. The
// only way to achieve that is to drop the TCP connection without a
// clean close: persist final state, then `process.exit()` from the
// caller. Discord sees a network-level disconnect (not a clean
// close), holds the session for ~60 s, and the new process's
// IDENTIFY-or-RESUME decision goes through retrieveSessionInfo →
// returns the persisted row → @discordjs/ws issues RESUME.
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
// burn the entire budget in minutes. MAX_IDENTIFY_ATTEMPTS=1 caps
// in-process IDENTIFYs at one — after a single failed attempt,
// retrieveSessionInfo throws and the manager's connect()
// rejects. The caller (start()) propagates the rejection up to
// gracefulShutdown(1), which crash-loops the task via ECS.
// ECS replaces the task and the budget guard resets in the new
// process — bounding burn to ~1 IDENTIFY per task lifetime
// rather than 1000 per pathological loop.
//
// The cap = 1 is intentional: a real cold start always IDENTIFYs
// successfully on the first try (Discord's IDENTIFY → READY
// handshake is reliable absent infra issues). A second attempt
// would only happen if the first IDENTIFY succeeded but the
// resulting session was immediately invalidated — which signals
// either token revocation or another process colliding on the
// same identity, both of which are operator-action items, not
// "retry quietly" items.

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
  let stopped = false;
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
    // Pass-through to the store. Whether the store throttles,
    // writes immediately, or deletes is its own concern; the shim
    // doesn't second-guess.
    //
    // Bound to preserve `this` semantics if the store ever moves
    // to a class shape. Factory closure today, but the bind is
    // belt-and-suspenders.
    return store.updateSessionInfo.bind(store);
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
      // The single token-bound instance is reused for registerCommands
      // (commit 6 will refactor registerCommands to accept REST + appId).
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
        if (data?.t === 'READY') {
          // application.id is the OAuth2 application snowflake —
          // identical to client.application.id in the legacy path.
          // RegisterCommands needs this to address the global-
          // commands or guild-commands endpoint.
          appId = data?.d?.application?.id ?? null;
          isReady = true;
          logger.info('gateway-ws-shim: READY received', {
            shardId,
            appIdPrefix: appId ? appId.slice(0, 6) : null,
          });
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
        // a Discord-side rejection). Leave the manager intact so
        // the caller's exception handler can interrogate state if
        // needed; stop() is the cleanup path.
        if (timeoutHandle) clearTimeout(timeoutHandle);
        throw err;
      }
      if (timeoutHandle) clearTimeout(timeoutHandle);
    },

    async stop({ flushFinal: shouldFlush = true } = {}) {
      stopped = true;
      // Drop dispatch handlers so any late dispatch arriving on
      // the way out doesn't trigger a downstream side effect
      // (the event-publisher's drain handles its own in-flight
      // sends).
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
      // RESUME.
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

    getAppId() {
      return appId;
    },

    get rest() {
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
