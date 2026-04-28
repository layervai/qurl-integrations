const config = require('./config');
const logger = require('./logger');
const { client, refreshCache, shutdown: discordShutdown } = require('./discord');
const { registerCommands, handleCommand } = require('./commands');
const { startServer, stopIntervals: stopServerIntervals } = require('./server');
const db = require('./store');
const { startOrphanTokenSweeper } = require('./orphan-token-sweeper');
const { missingBootKeys, missingProdKeys } = require('./boot-requirements');
const { initHttpOnly } = require('./http-only-init');

// Process role — selects which subset of the bot runs in this
// container. Three modes:
//
//   combined (default) — legacy single-process shape. Gateway
//                        client logs in; Express HTTP server
//                        listens; cron jobs run. Backward-compatible
//                        for any env that hasn't opted into the
//                        split (sandbox pre-migration, local dev).
//
//   gateway — Discord Gateway WebSocket + interaction handlers +
//             cron jobs. No Express listener. Deployed as a
//             singleton (desired_count=1) because a Discord bot
//             token admits only one active Gateway connection —
//             two concurrent logins cause a session-identity flap
//             every few seconds as each replaces the other's
//             WebSocket.
//
//   http — Express listener only (OAuth callback + GitHub
//          webhooks + /health + /metrics). No Gateway login.
//          Can scale horizontally behind an ALB. Outbound Discord
//          API calls go through `discord-rest.js` (REST-only, no
//          persistent connection).
//
// Invariant: `discord.js`'s Client object is still required at
// module load (it exposes `sendDM` et al. that the current HTTP
// routes import), but `client.login()` is gated on `isGateway`.
// Creating a Client without login() does NOT open a WebSocket —
// the WS only opens on login. HTTP-only replicas therefore never
// collide with the gateway singleton.
//
// http-only mode (`PROCESS_ROLE=http`) needs two things login()
// would otherwise do for free: (1) a token on `client.rest` so
// REST helpers (sendDM, channels.X.send, member.roles.add) can
// authenticate, and (2) an initial `refreshCache()` so the
// route handlers find a populated guild/roles/channels cache on
// the first OAuth callback or webhook. Both are seeded
// explicitly in start() below — see the `else if (isHttp)`
// branch. Migration to `discord-rest.js` (lighter, no Client
// object needed at all) remains a follow-up; today's helpers
// already work in either role once the token + cache are in
// place.
const PROCESS_ROLE = (process.env.PROCESS_ROLE || 'combined').trim();
const VALID_ROLES = ['combined', 'gateway', 'http'];
if (!VALID_ROLES.includes(PROCESS_ROLE)) {
  logger.error(`Invalid PROCESS_ROLE: '${PROCESS_ROLE}'. Valid values: ${VALID_ROLES.join(', ')}. Set PROCESS_ROLE to one of these, or leave unset to default to 'combined'.`);
  process.exit(1);
}
const isGateway = PROCESS_ROLE === 'gateway' || PROCESS_ROLE === 'combined';
const isHttp = PROCESS_ROLE === 'http' || PROCESS_ROLE === 'combined';
logger.info('Process role configured', { role: PROCESS_ROLE, isGateway, isHttp });

// Multi-tenant mode: when GUILD_ID is unset (or not a valid snowflake), the
// bot treats itself as a public multi-server app. Commands register globally,
// per-guild qURL API keys come from /qurl setup (stored encrypted in
// guild_configs), and OpenNHP-specific features (contributor roles, welcome
// DMs, GitHub OAuth linking, PR webhook notifications) are dormant because
// no single guild is being tracked.
//
// When GUILD_ID is set to a valid Discord snowflake, the original
// single-guild OpenNHP deployment behavior is preserved: commands register
// to that guild only, and all OpenNHP features are active.
const { isMultiTenant } = config;

// Validate required config. Fail fast at boot so misconfigurations are caught
// during deploy, not when the first request arrives. Lists live in
// boot-requirements.js so they can be unit-tested without side-effecting
// a bot boot. Gated on isOpenNHPActive (see config.js) — single-guild-plain
// and multi-tenant both use the short required list.
const missing = missingBootKeys(config, config.isOpenNHPActive);

if (missing.length > 0) {
  logger.error('Missing required environment variables:');
  missing.forEach(key => logger.error(`  - ${key}`));
  logger.error('See .env.example for required variables.');
  process.exit(1);
}

// Boot-log the effective mode so prod triage can grep it. The three
// lines correspond exactly to the supported modes in config.js.
if (isMultiTenant) {
  logger.info('Multi-tenant mode (GUILD_ID unset): commands will register globally; OpenNHP features are dormant.');
} else if (config.isOpenNHPActive) {
  logger.info(`Single-guild OpenNHP mode: targeting GUILD_ID=${config.GUILD_ID}. OpenNHP community features active.`);
} else {
  logger.info(`Single-guild plain mode: targeting GUILD_ID=${config.GUILD_ID}. OpenNHP features dormant; only /qurl registered.`);
}

// Production-only required secrets. In dev these are optional so localhost
// workflows stay convenient. Keep this list in sync with the production
// comments in .env.example.
if (process.env.NODE_ENV === 'production') {
  // QURL_API_KEY is the global-fallback key for /qurl send. Only the
  // OpenNHP community server demands it at boot; single-guild-plain and
  // multi-tenant deployments rely on per-guild /qurl setup. List is in
  // boot-requirements.js for testability.
  const prodMissing = missingProdKeys(process.env, config.isOpenNHPActive);
  if (prodMissing.length > 0) {
    logger.error(`NODE_ENV=production but missing required env vars: ${prodMissing.join(', ')}`);
    logger.error('For KEY_ENCRYPTION_KEY, generate with: node -e "console.log(require(\'crypto\').randomBytes(32).toString(\'base64\'))"');
    process.exit(1);
  }

  // BASE_URL https check: unconditional in OpenNHP mode. An OpenNHP
  // prod deploy that forgets to set BASE_URL in its task-def would
  // otherwise fall through to the "http://localhost:3000" default in
  // config.js — which would boot successfully but fail at the first
  // OAuth callback, exactly the deferred-error mode this fail-fast
  // exists to prevent. In non-OpenNHP modes BASE_URL is unused
  // (no /auth or /webhook routes mounted), so we only enforce https
  // there if the operator explicitly set it — lets single-guild-plain
  // and multi-tenant deployments ignore BASE_URL without a false-
  // positive failure, while still catching a stale http:// SSM value
  // if a future code path re-enables BASE_URL use.
  if (config.isOpenNHPActive && !config.BASE_URL.startsWith('https://')) {
    logger.error(`BASE_URL must use https:// in production (OpenNHP mode). Got: ${config.BASE_URL}`);
    process.exit(1);
  }
  // Treat "" and whitespace-only as unset (matches GUILD_ID's normalization
  // robustness). An operator who parameterized the SSM value but seeded it
  // with "" or " " should not silently escape the https check — but they
  // also shouldn't get a false-positive boot failure from an accidentally-
  // empty param, since config.BASE_URL falls through to the localhost
  // default in that case and the downstream "http://localhost:3000" is
  // caught by the OpenNHP https check above anyway.
  const baseUrlExplicitlySet = Boolean(process.env.BASE_URL?.trim());
  if (!config.isOpenNHPActive && baseUrlExplicitlySet && !config.BASE_URL.startsWith('https://')) {
    logger.error(`BASE_URL must use https:// in production (got ${config.BASE_URL})`);
    process.exit(1);
  }

  // OAUTH_STATE_SECRET guards GitHub OAuth state, which is dormant
  // unless OpenNHP mode is active (the only mode that mounts /auth +
  // /webhook routes). Require it only when that surface is live.
  if (config.isOpenNHPActive && !process.env.OAUTH_STATE_SECRET) {
    // Falling back to GITHUB_CLIENT_SECRET couples the two secrets —
    // rotating GitHub's client secret would invalidate all in-flight
    // OAuth states and vice versa. A prod deploy must set this explicitly.
    logger.error('OAUTH_STATE_SECRET must be set in production. Generate with: openssl rand -hex 32');
    process.exit(1);
  }

  // Crypto smoke test: catch a misconfigured KEY_ENCRYPTION_KEY at boot
  // instead of on the first encrypt() call (which could be an OAuth token
  // persist minutes into serving traffic). This validates the key material
  // can both encrypt AND decrypt, not just decode as base64.
  try {
    const { encrypt, decrypt } = require('./utils/crypto');
    const probe = `boot-smoke-${Date.now()}`;
    if (decrypt(encrypt(probe)) !== probe) {
      throw new Error('round-trip mismatch');
    }
  } catch (err) {
    logger.error('KEY_ENCRYPTION_KEY smoke test failed at boot — refusing to start', { error: err.message });
    process.exit(1);
  }
}

// Validate numeric config values
if (isNaN(config.PENDING_LINK_EXPIRY_MINUTES) || config.PENDING_LINK_EXPIRY_MINUTES <= 0) {
  logger.error('PENDING_LINK_EXPIRY_MINUTES must be a positive integer');
  process.exit(1);
}
if (!Number.isFinite(config.RATE_LIMIT_WINDOW_MS) || config.RATE_LIMIT_WINDOW_MS <= 0) {
  logger.error('RATE_LIMIT_WINDOW_MS must be a positive integer (set to 0 would disable rate limiting)');
  process.exit(1);
}
if (!Number.isFinite(config.RATE_LIMIT_MAX_REQUESTS) || config.RATE_LIMIT_MAX_REQUESTS <= 0) {
  logger.error('RATE_LIMIT_MAX_REQUESTS must be a positive integer');
  process.exit(1);
}
// Each org name is interpolated into GitHub search queries
// (`type:pr author:X org:<org> is:merged`). Reject anything that doesn't
// match GitHub's org-name rules so an injected space can't smuggle extra
// search qualifiers.
for (const org of config.ALLOWED_GITHUB_ORGS) {
  if (!/^[a-z0-9](?:[a-z0-9]|-(?=[a-z0-9])){0,38}$/.test(org)) {
    logger.error(`ALLOWED_GITHUB_ORGS contains invalid org name: "${org}"`);
    process.exit(1);
  }
}

if (config.QURL_ENDPOINT === 'https://api.layerv.ai') {
  logger.warn('QURL_ENDPOINT is using production default — set via env var for non-prod');
}
if (config.CONNECTOR_URL === 'https://get.qurl.link:9808') {
  logger.warn('CONNECTOR_URL is using production default — set via env var for non-prod');
}
// In production, require explicit endpoint config rather than relying on
// fallbacks. Sending traffic to the wrong endpoint (e.g. stale fallback
// after an infra rename) would silently route tenant keys + files to an
// unintended host.
if (process.env.NODE_ENV === 'production') {
  if (!process.env.QURL_ENDPOINT) {
    logger.error('QURL_ENDPOINT must be explicitly set in production');
    process.exit(1);
  }
  if (!process.env.CONNECTOR_URL) {
    logger.error('CONNECTOR_URL must be explicitly set in production');
    process.exit(1);
  }
}

// Gateway-only event wiring. HTTP-only replicas skip these because
// they never login() and so the client never fires these events —
// but the `.on()` registrations would still leak handler references
// and register no-op listeners, so gate them for clarity.
if (isGateway) {
  // Register commands when ready
  client.once('ready', async () => {
    await registerCommands(client);
  });

  // Handle interactions
  client.on('interactionCreate', handleCommand);

  // Error handling
  client.on('error', error => {
    logger.error('Discord client error', { error: error.message });
  });
}

// Log and continue on unhandled rejections. The old behavior killed the
// entire process on any stray rejection (transient Discord timeouts, network
// blips) which made the bot fragile. Truly fatal errors surface via
// uncaughtException below.
process.on('unhandledRejection', (error, _promise) => {
  logger.error('Unhandled promise rejection (logged, not fatal)', {
    error: error?.message || error,
    stack: error?.stack,
  });
});

// Uncaught exceptions indicate corrupted process state — no safe recovery.
process.on('uncaughtException', error => {
  logger.error('Uncaught exception', { error: error.message, stack: error.stack });
  gracefulShutdown(1);
});

// Graceful shutdown
let httpServer = null;
let isShuttingDown = false;

async function gracefulShutdown(code = 0) {
  if (isShuttingDown) return;
  isShuttingDown = true;

  // Force exit after 10s if shutdown hangs
  setTimeout(() => { logger.error('Shutdown timed out, forcing exit'); process.exit(1); }, 10000).unref();

  logger.info('Graceful shutdown initiated...');

  try {
    // Wait for in-flight HTTP requests to drain — server.close() is async,
    // process.exit() called immediately after would truncate OAuth callbacks
    // mid-flight and leave users with a consumed pending_link but no GitHub
    // link created.
    if (httpServer) {
      await new Promise(resolve => {
        httpServer.close(err => {
          if (err) logger.warn('HTTP server close reported error', { error: err.message });
          resolve();
        });
      });
    }
    stopServerIntervals();
    // Discord client shutdown only meaningful when we're the gateway
    // role. HTTP-only replicas never called login(), so there's no
    // WebSocket to close — discordShutdown() on an un-logged-in
    // client just releases the event emitter handles.
    if (isGateway) {
      await discordShutdown();
    }
    await db.close();
    logger.info('Shutdown complete');
  } catch (error) {
    logger.error('Error during shutdown', { error: error.message });
  }

  process.exit(code);
}

// Handle shutdown signals
process.on('SIGTERM', () => {
  logger.info('Received SIGTERM');
  gracefulShutdown(0);
});

process.on('SIGINT', () => {
  logger.info('Received SIGINT');
  gracefulShutdown(0);
});

// Start everything
async function start() {
  logger.info('Starting qURL Discord Bot...');
  logger.info(`Version: ${require('../package.json').version}`);
  logger.info(`Environment: ${process.env.NODE_ENV || 'development'}`);

  // HTTP listener — OAuth callback, webhooks, health, metrics.
  // Skipped in `gateway`-only mode; the ALB targets HTTP replicas
  // only. `combined` keeps the legacy single-process behavior.
  if (isHttp) {
    httpServer = startServer();
  }

  // Background retry-revoke for any OAuth tokens whose initial
  // revoke failed. Only on the gateway side — the sweeper calls
  // into Discord via the gateway client (`user.send()` etc.) and
  // would need to be refactored to `discord-rest.js` before
  // running in `http`-only mode. Tracked as a follow-up.
  if (isGateway) {
    startOrphanTokenSweeper();
  }

  // Login to Discord with a 30s deadline. client.login() doesn't
  // expose a native timeout; if the Discord API is unreachable
  // the call can hang indefinitely and block boot without any
  // log line pointing at the cause. Gated on `isGateway` — the
  // HTTP-only replica must NOT open a second Gateway connection
  // on the same bot token; Discord would flap session identity
  // between the two WebSockets every few seconds.
  if (isGateway) {
    await Promise.race([
      client.login(config.DISCORD_TOKEN),
      new Promise((_, reject) => {
        const t = setTimeout(() => reject(new Error('Discord login timed out after 30s')), 30_000);
        t.unref();
      }),
    ]);
  } else if (isHttp) {
    // Pure http-only mode: seed the REST token + warm the cache without
    // a Gateway login. See src/http-only-init.js for the rationale.
    // Failures are fatal — propagated through start().catch() into
    // gracefulShutdown(1) so a Discord-unreachable replica crash-loops
    // instead of silently serving 5xx.
    await initHttpOnly({ client, config, refreshCache });
  }
}

start().catch(error => {
  logger.error('Failed to start', { error: error.message });
  // Route through gracefulShutdown so any partial state (httpServer
  // listening, Discord partially connected, DB open, sweeper timer armed)
  // is torn down cleanly before exit. Previously this went straight to
  // process.exit(1) and risked leaking WAL checkpoints + WebSocket sessions
  // under ECS rolling deploys.
  gracefulShutdown(1);
});
