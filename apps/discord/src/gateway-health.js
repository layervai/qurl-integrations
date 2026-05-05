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

/**
 * Start the gateway-only health listener.
 *
 * @param {() => boolean} isReady - Closure returning the bot's
 *   connected state. Pass `() => client.isReady()` from index.js.
 *   Closure rather than direct client reference so this module
 *   doesn't need a discord.js dep just to type the parameter, and
 *   tests can pass a plain function.
 * @returns {import('node:http').Server}
 */
function startGatewayHealthServer(isReady) {
  const server = http.createServer((req, res) => {
    if (req.method !== 'GET' || req.url !== '/health') {
      res.writeHead(404, { 'Content-Type': 'application/json' });
      res.end('{"status":"not_found"}');
      return;
    }
    if (isReady()) {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end('{"status":"ok"}');
    } else {
      // 503 distinguishes "responding but not ready" from "not
      // responding at all" — the wget probe treats non-2xx as
      // unhealthy, so ECS replaces the task on either.
      res.writeHead(503, { 'Content-Type': 'application/json' });
      res.end('{"status":"unhealthy"}');
    }
  });

  server.listen(config.PORT, () => {
    logger.info(`Gateway health listener on port ${config.PORT}`);
  });

  return server;
}

module.exports = { startGatewayHealthServer };
