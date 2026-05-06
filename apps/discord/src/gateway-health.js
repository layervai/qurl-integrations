/**
 * Minimal HTTP listener for the gateway-only ECS task. Binds
 * `127.0.0.1:${config.PORT}` (default 3000) and answers `/health`
 * based on the Discord.js client's connected state. The container-
 * level wget probe (re-added in a qurl-integrations-infra follow-up
 * to #151) hits this endpoint to detect WebSocket disconnect /
 * event-loop wedge / dispatch deadlock — failure modes the
 * deployment_circuit_breaker can't see because they don't terminate
 * the node process.
 *
 * Only the gateway-only path uses this. The full HTTP role
 * (`isHttp=true`) starts the Express server in `server.js` which has
 * its own richer `/health` (db.healthCheck) plus OAuth callback,
 * webhooks, metrics — different surface, different probe semantics.
 *
 * Why a raw `node:http` listener vs. a tiny Express app: the gateway
 * shouldn't pull in OAuth handlers, db drivers, or rate-limit state
 * just to answer `GET /health`. Smaller surface = fewer ways to wedge
 * the very process this probe exists to detect wedging in.
 *
 * SIGTERM during the listen window — non-issue, but worth knowing:
 * `startGatewayHealthServer` returns synchronously before the
 * `listening` event fires. A SIGTERM landing in that window calls
 * `httpServer.close()` on a not-yet-listening server, which surfaces
 * its callback with `ERR_SERVER_NOT_RUNNING`. `gracefulShutdown` in
 * index.js logs that as a warn ("HTTP server close reported error")
 * and continues teardown. Tolerable; future debuggers shouldn't
 * chase that warn line as a real bug.
 *
 * IPv4 loopback dependency: this binds `127.0.0.1` and the Dockerfile
 * HEALTHCHECK probes `http://127.0.0.1:3000/health`. The Fargate
 * distroless base (`gcr.io/distroless/nodejs*` ARM64) ships v4
 * loopback enabled — if a future base-image swap ever strips IPv4
 * loopback (or flips `localhost` resolution to `::1` first), the
 * probe goes silent. Match address family on both sides if either
 * ever changes; the Dockerfile comment about `::1` vs `127.0.0.1`
 * mismatch is the same dependency from the other side.
 */
const http = require('node:http');
const config = require('./config');
const logger = require('./logger');
const { AUDIT_EVENTS } = require('./constants');

// Pre-computed — these never change, so avoid JSON.stringify per request.
const BODY_OK = JSON.stringify({ status: 'ok' });
const BODY_UNHEALTHY = JSON.stringify({ status: 'unhealthy' });
const BODY_NOT_FOUND = JSON.stringify({ status: 'not_found' });

/**
 * Start the gateway-only health listener.
 *
 * @param {() => boolean} isReady - Closure returning the bot's
 *   connected state. Pass `() => client.isReady()` from index.js.
 *   Closure rather than direct client reference so this module
 *   doesn't need a discord.js dep just to type the parameter, and
 *   tests can pass a plain function.
 * @param {(err: Error) => void} onListenError - Called on a listen
 *   error (e.g. EADDRINUSE). Required — no default, so every caller
 *   has to make an explicit choice about how a listen failure should
 *   be handled. The original default of `process.exit(1)` silently
 *   bypassed the Discord WebSocket + DB teardown that
 *   `gracefulShutdown` performs; making it required closes that gap.
 *   index.js passes `() => gracefulShutdown(1)`.
 * @param {number} [port=config.PORT] - Port to bind. Defaults to
 *   `config.PORT`. Tests pass an explicit port to avoid mutating
 *   shared mock state.
 * @returns {import('node:http').Server}
 */
function startGatewayHealthServer(isReady, onListenError, port = config.PORT) {
  // Per-listener readiness history. Set on each /health hit so we
  // can log warn/info ONLY on transitions — operators get one warn
  // at "ok → unhealthy" (the bot wedged) and one info at "unhealthy
  // → ok" (it recovered), instead of a log line every 30 s of probe
  // cadence. Initial `null` so the first observation is silent
  // (we don't know what "previous" means at boot — the start-period
  // window is allowed to flap freely without spamming on every flap).
  let prevReady = null;

  const server = http.createServer((req, res) => {
    // Strip query string before matching — some ECS/ALB probe configs
    // append a cache-busting `?ts=…`; we don't want that to 404.
    // split() instead of new URL() because URL() throws on malformed
    // input (reachable from a buggy probe or L7 scanner via ECS Exec).
    const path = req.url.split('?', 1)[0];

    // GET and HEAD only. Wget uses GET; HEAD is semantically
    // equivalent (RFC 9110 §9.3.2) and some load-balancer probes
    // default to it — accepting both avoids silent probe failure if
    // the infra follow-up specifies HEAD. Node automatically strips
    // the response body for HEAD requests.
    if ((req.method !== 'GET' && req.method !== 'HEAD') || path !== '/health') {
      res.writeHead(404, { 'Content-Type': 'application/json' });
      res.end(BODY_NOT_FOUND);
      return;
    }

    // Defensive: if the readiness closure throws (e.g. a future
    // composite signal derefs into something nullable), treat as
    // unhealthy rather than surfacing a 500. Log at debug so an
    // operator triaging an unexplained 503 rate has something to
    // grep for — info-level would be too noisy at probe cadence.
    //
    // Track WHICH unhealthy path (closure-not-ready vs closure-threw)
    // so the audit emit below can distinguish them — same alarm fires
    // on either, but the dashboard can split a real wedge from a
    // closure bug under load.
    let ready;
    let reason = 'not_ready';
    try {
      ready = isReady();
    } catch (err) {
      logger.debug('Gateway health: isReady closure threw, treating as unhealthy', { error: err.message });
      ready = false;
      reason = 'sampler_threw';
    }

    // Transition logging — fires once per state change, not per probe.
    if (prevReady === true && !ready) {
      logger.warn('Gateway health: ok → unhealthy');
    } else if (prevReady === false && ready) {
      logger.info('Gateway health: unhealthy → ok');
    }
    prevReady = ready;

    if (ready) {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(BODY_OK);
    } else {
      // 503 distinguishes "responding but not ready" from "not
      // responding at all" — the wget probe treats non-2xx as
      // unhealthy, so ECS replaces the task on either.
      //
      // Audit-event emit on EVERY 503 (not just on the transition
      // warn above) so the paired CloudWatch metric filter
      // (qurl-integrations-infra PR #419 — qurl-bot-discord/terraform
      // monitoring.tf) can count unhealthy responses at probe cadence.
      // A wedge persisting for N probes produces N count events for
      // the alarm, not one transition log.
      //
      // `reason` carries 'not_ready' (clean WebSocket disconnect) or
      // 'sampler_threw' (closure under load surfaced a bug). Both
      // routes feed the same alarm, but the dashboard can split.
      res.writeHead(503, { 'Content-Type': 'application/json' });
      res.end(BODY_UNHEALTHY);
      logger.audit(AUDIT_EVENTS.GATEWAY_HEALTH_UNHEALTHY, { reason });
    }
  });

  // Localhost-only probe surface — cap idle/header timeouts to bound
  // resource usage even though nothing hostile reaches loopback.
  // Prevents a stuck connection from holding an fd open indefinitely.
  // requestTimeout = 10 s gives the Dockerfile HEALTHCHECK --timeout=5s
  // room to be the binding constraint. headersTimeout = 5 s satisfies
  // Node's documented `headersTimeout < requestTimeout` constraint —
  // a future refactor that accepts a request body here would otherwise
  // race the two timers and surface a wedge as a header-timeout
  // instead of the more accurate request-timeout.
  server.requestTimeout = 10_000;
  server.headersTimeout = 5_000;

  // Surface EADDRINUSE (or any listen error) as a structured log line
  // instead of an opaque uncaught-exception V8 stack trace. Route
  // through the caller's listen-error handler so index.js can tear
  // down the Discord WebSocket + DB cleanly via gracefulShutdown.
  server.on('error', (err) => {
    logger.error('Gateway health listener failed', { error: err.message, code: err.code });
    onListenError(err);
  });

  // Bind to loopback only. The wget probe runs INSIDE the container,
  // so 127.0.0.1 is sufficient — and tighter than the all-interfaces
  // default. Closes the only failure mode where this readiness probe
  // could leak externally (a future ALB-target-group rewire). The
  // full Express server in `server.js` correctly binds all interfaces
  // because the ALB connects from outside the container; this
  // listener has no such requirement.
  server.listen(port, '127.0.0.1', () => {
    logger.info('Gateway health listener listening', { addr: '127.0.0.1', port: server.address().port });
  });

  return server;
}

module.exports = { startGatewayHealthServer };
