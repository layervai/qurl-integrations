/**
 * Minimal HTTP listener for the gateway-only ECS task. Binds
 * `:${config.PORT}` (default 3000) and answers `/health` based on the
 * Discord.js client's connected state. The container-level wget probe
 * (re-added in a qurl-integrations-infra follow-up to #151) hits this
 * endpoint to detect WebSocket disconnect / event-loop wedge / dispatch
 * deadlock — failure modes the deployment_circuit_breaker can't see
 * because they don't terminate the node process.
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
 */
const http = require('node:http');
const config = require('./config');
const logger = require('./logger');

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
 * @param {(err: Error) => void} onFatalError - Called on a listen
 *   error (e.g. EADDRINUSE). Pass `() => gracefulShutdown(1)` from
 *   index.js so the Discord WebSocket and DB are torn down cleanly.
 * @param {number} [port=config.PORT] - Port to bind. Defaults to
 *   `config.PORT`. Tests pass an explicit port to avoid mutating
 *   shared mock state.
 * @returns {import('node:http').Server}
 */
function startGatewayHealthServer(isReady, onFatalError, port = config.PORT) {
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
    // unhealthy rather than surfacing a 500.
    let ready;
    try {
      ready = isReady();
    } catch {
      ready = false;
    }

    if (ready) {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(BODY_OK);
    } else {
      // 503 distinguishes "responding but not ready" from "not
      // responding at all" — the wget probe treats non-2xx as
      // unhealthy, so ECS replaces the task on either.
      res.writeHead(503, { 'Content-Type': 'application/json' });
      res.end(BODY_UNHEALTHY);
    }
  });

  // Localhost-only probe surface — cap idle/header timeouts to bound
  // resource usage even though nothing hostile reaches loopback.
  // Prevents a stuck connection from holding an fd open indefinitely.
  server.requestTimeout = 5_000;
  server.headersTimeout = 5_000;

  // Surface EADDRINUSE (or any listen error) as a structured log line
  // instead of an opaque uncaught-exception V8 stack trace. Route
  // through the caller's fatal-error handler so index.js can tear
  // down the Discord WebSocket + DB cleanly via gracefulShutdown.
  server.on('error', (err) => {
    logger.error('Gateway health listener failed', { error: err.message, code: err.code });
    if (onFatalError) {
      onFatalError(err);
    } else {
      // No handler — hard-exit so deployment_circuit_breaker replaces
      // the task. Callers should pass gracefulShutdown for clean teardown.
      process.exit(1);
    }
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
