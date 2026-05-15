const config = require('./config');
const logger = require('./logger');
const { client, refreshCache, shutdown: discordShutdown } = require('./discord');
const { registerCommands, handleCommand } = require('./commands');
const { handleFlowInteraction } = require('./flow-dispatch');
const { startServer, stopIntervals: stopServerIntervals } = require('./server');
const { startGatewayHealthServer } = require('./gateway-health');
const { startGatewayHeartbeat, startActiveGuildCount, noteGatewayActivity } = require('./gateway-metrics');
const db = require('./store');
const { startOrphanTokenSweeper } = require('./orphan-token-sweeper');
const { missingBootKeys, missingProdKeys, missingKekRequiredKeys, missingEventShipperKeys, missingMapCommandKeys, resolveProcessRole } = require('./boot-requirements');
const { initHttpOnly } = require('./http-only-init');
const eventConsumer = require('./event-consumer');

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
// explicitly in start() below — see the `if (isHttp && !isGateway)`
// branch (runs BEFORE startServer so the ALB can't route a
// request through a half-initialized replica).
//
// Known gap (acceptable for now): cache invalidation in http-only
// mode. The `client.on('roleDelete' / 'channelDelete')` handlers
// in src/discord.js only fire when the Gateway is connected, so
// deletions made on the OpenNHP guild stay cached as stale
// references until the replica restarts. The lazy refresh in
// each helper checks `if (!channels.X)` — non-null but stale
// doesn't trigger a refresh. OpenNHP guild admins rarely delete
// tracked channels; if this becomes load-bearing, a periodic
// REST-driven `refreshCache()` would close the gap without
// needing a Gateway connection.
// Resolve PROCESS_ROLE via the helper in boot-requirements.js so the
// invalid-value path is unit-testable without a child-process spawn.
let PROCESS_ROLE, isGateway, isHttp;
try {
  ({ role: PROCESS_ROLE, isGateway, isHttp } = resolveProcessRole(process.env.PROCESS_ROLE));
} catch (err) {
  logger.error(err.message);
  // Direct process.exit (not gracefulShutdown) is intentional: this
  // runs at module-top-level, before gracefulShutdown is even defined,
  // and there's no state to tear down — no DB open, no HTTP listener,
  // no WebSocket. A future "fix" routing this through gracefulShutdown
  // would either fail (undefined reference) or block on shutdown
  // teardown that has nothing to do.
  //
  // The logger.error above is guaranteed-flushed because src/logger.js
  // writes synchronously via console.error → process.stderr.write
  // (no winston/pino async transport). If logger.js ever moves to an
  // async transport, this boot-fail path needs an explicit flush or a
  // direct console.error fallback so the message survives the exit.
  process.exit(1);
}
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
  // QURL_API_KEY is the global-fallback key for /qurl file + /qurl map.
  // Only the OpenNHP community server demands it at boot; single-guild-
  // plain and multi-tenant deployments rely on per-guild /qurl setup.
  // List is in boot-requirements.js for testability.
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
}

// Any deploy that issues real GitHub OAuth tokens must encrypt persisted
// credentials at rest, in any NODE_ENV — the orphan-token path uses
// encryptStrict as a backstop, but failing closed at boot is the loud
// signal. Smoke-test the key material so a malformed value is caught here
// instead of on the first encrypt() call minutes into serving traffic.
//
// Role-agnostic by design: a `gateway`-role process doesn't mount the
// OAuth callback, but env vars are uniform across roles in a single
// deploy, so one role refusing to boot while another silently degrades
// is worse than refusing both.
const kekMissing = missingKekRequiredKeys(process.env);
if (kekMissing.length > 0) {
  logger.error(`GITHUB_CLIENT_SECRET is set but ${kekMissing.join(', ')} is missing — refusing to boot. Any deployment that issues real GitHub OAuth tokens must encrypt persisted credentials at rest.`);
  logger.error('Generate KEY_ENCRYPTION_KEY with: node -e "console.log(require(\'crypto\').randomBytes(32).toString(\'base64\'))"');
  process.exit(1);
}
if (process.env.KEY_ENCRYPTION_KEY) {
  try {
    // encryptStrict (not encrypt) so a future refactor that drops the
    // outer env-var guard fails loudly here instead of silently falling
    // through encrypt's plaintext-passthrough branch.
    const { encryptStrict, decrypt } = require('./utils/crypto');
    if (decrypt(encryptStrict('boot-smoke')) !== 'boot-smoke') {
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

// Event-shipper (zero-downtime Pillar 1) — when the flag is on, the
// queue URL is the load-bearing piece: producer publishes to it,
// consumer polls from it. Role-agnostic by design: env vars are
// uniform across roles in a single deploy, so one role refusing to
// boot is preferable to a half-wired split.
const eventShipperMissing = missingEventShipperKeys(config);
if (eventShipperMissing.length > 0) {
  logger.error(`ENABLE_EVENT_SHIPPER=true but missing required env vars: ${eventShipperMissing.join(', ')}`);
  process.exit(1);
}

// Symmetric with the eventShipper check above — fail fast on
// inconsistent flag-vs-secret state. The error message is the
// operator-facing source of truth for the remediation steps.
const mapCommandMissing = missingMapCommandKeys(config);
if (mapCommandMissing.length > 0) {
  logger.error(
    `MAP_COMMAND_ENABLED=true but ${mapCommandMissing.join(', ')} is missing or still the literal "PLACEHOLDER" sentinel. ` +
    'Seed a real Google Maps Platform API key (Places API enabled, no HTTP-referrer restriction) into the ' +
    '/qurl-bot-discord/GOOGLE_MAPS_API_KEY SSM parameter before re-flipping the toggle.'
  );
  process.exit(1);
}

// Worker role: the SQS-driven dispatch path. Distinct from `isHttp`
// because the http role today serves OAuth callbacks + webhooks
// regardless of the flag; only when ENABLE_EVENT_SHIPPER is on does
// the http role ALSO consume from SQS. Derived once here so the
// consumer-startup and event-listener registration sites stay in sync.
const isWorker = isHttp && config.ENABLE_EVENT_SHIPPER;

// Log isWorker on its own so an operator triaging a no-dispatch
// incident sees the full picture without having to mentally combine
// the role log (line 86) with the flag. Distinct line so a grep for
// `isWorker=true` lands directly.
logger.info('Worker tier configured', { isWorker, eventShipperEnabled: config.ENABLE_EVENT_SHIPPER });

// Interaction routing. Same dispatcher whether the source is the
// Gateway WS (isGateway) or an SQS message reconstructed via
// client.actions.InteractionCreate.handle (isWorker, see
// src/event-consumer.js). Registered once, gated on either flag, so
// combined-mode + flag-on doesn't double-register.
//
// **PR 10 PRE-MERGE REQUIREMENT** — the gate below MUST be updated
// to `(isGateway && !config.ENABLE_EVENT_SHIPPER) || isWorker` (or
// the gateway-side listener replaced with a publish-to-SQS shim) in
// THE SAME PR that introduces the producer. Without that change,
// combined-mode + flag-on after PR 10 ships will dispatch every
// interaction twice — once via the gateway WS frame, once via the
// SQS roundtrip — silently doubling DM fan-outs, side-effecting
// every flow-state transition twice, and corrupting telemetry.
// PR 11 alone is safe (producer doesn't exist → queue stays empty
// → consumer is a no-op); the hazard activates the instant the
// producer side starts publishing.
//
// Today (PR 11 only), the producer side doesn't exist yet, so
// combined-mode + flag-on still runs the gateway dispatch path
// in-process and the worker tier's consumer sits on an empty queue.
// TODO(PR-10): change the gate to `(isGateway && !config.ENABLE_EVENT_SHIPPER) || isWorker`
// (or replace the gateway-side path with a publish-to-SQS shim) per the
// PR 10 PRE-MERGE REQUIREMENT comment above. `git grep TODO(PR-10)` from
// the PR 10 branch surfaces every gate that needs flipping.
if (isGateway || isWorker) {
  // Handle interactions. Split-dispatch by interaction kind:
  // ChatInputCommand + Autocomplete go through the slash-command
  // path (handleCommand); MessageComponent + ModalSubmit go through
  // the DDB-backed flow dispatcher (handleFlowInteraction, which
  // replaces the legacy in-process `awaitMessageComponent` pattern
  // per the zero-downtime upgrade rollout). Keep the two paths
  // disjoint — a single listener that branched internally would
  // mix two state machines (slash dispatch vs. flow-resume) under
  // one error-handling envelope; cleaner to wire them separately.
  client.on('interactionCreate', (interaction) => {
    if (interaction.isChatInputCommand() || interaction.isAutocomplete()) {
      return handleCommand(interaction);
    }
    if (interaction.isMessageComponent() || interaction.isModalSubmit()) {
      return handleFlowInteraction(interaction);
    }
    // Future Discord interaction types we don't ship today fall
    // through here. Log at debug so an operator triaging "why isn't
    // my new component routing" sees the unrouted type rather than
    // the silent-drop black box the pre-conversion code provided.
    logger.debug('interactionCreate: unrouted interaction type', {
      type: interaction.type,
    });
    return undefined;
  });
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

  // interactionCreate listener is registered above (isGateway || isWorker
  // block) so the worker tier shares the same dispatcher.

  // Tick the gateway-activity timestamp on every WebSocket frame.
  // Feeds the `activity_age_ms` observability gauge ONLY — does not
  // gate gateway health (post-#210). The signal can't distinguish "no
  // traffic because idle" from "no traffic because wedged", so an
  // alarm on it would false-positive on quiet bots; real zombie-WS
  // detection lives elsewhere (see PR description on #210). Pass the
  // function directly (not wrapped) so v8 keeps a single shape and
  // avoids per-frame closure allocation.
  client.on('raw', noteGatewayActivity);

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
let httpRefreshTimer = null;
let gatewayHeartbeatTimer = null;
let activeGuildCountTimer = null;
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
    // SQS consumer drain. Stops new ReceiveMessage calls, then
    // awaits the current poll iteration's in-flight `processMessage`
    // promises. Has to run BEFORE db.close() — running handlers may
    // still be reading/writing flow-state DDB rows on the way to
    // ACK'ing the interaction. Idempotent + a no-op when the
    // consumer was never started, so unconditional here.
    await eventConsumer.stop();
    // Periodic REST refreshCache in http-only mode is .unref()ed so it
    // wouldn't block exit on its own, but clearing explicitly keeps
    // shutdown symmetric with the other intervals (server.js, oauth.js
    // rateLimitStore sweep, webhooks.js badSig sweep) and avoids one
    // last refresh firing mid-teardown.
    if (httpRefreshTimer) {
      clearInterval(httpRefreshTimer);
    }
    // Clear gateway-metrics timers BEFORE discordShutdown(): a stray
    // heartbeat tick during client.destroy() would race with the
    // WebSocketShard teardown and surface as a confusing "Sampler
    // threw" warn. Timers are also .unref()'d but order matters for
    // log cleanliness.
    if (gatewayHeartbeatTimer) {
      clearInterval(gatewayHeartbeatTimer);
    }
    if (activeGuildCountTimer) {
      clearInterval(activeGuildCountTimer);
    }
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

  // Pure http-only mode: seed the REST token + warm the cache BEFORE
  // opening the listener. Otherwise there's a race window where the
  // ALB can route OAuth callbacks / webhooks to a replica whose
  // `client.rest` has no token and whose channel/role cache is cold —
  // the request would 401 inside sendDM / assignContributorRole.
  // See src/http-only-init.js for the full rationale (token + cache
  // + periodic-REST-refresh that compensates for missing roleDelete /
  // channelDelete events). Failures are fatal — propagated through
  // start().catch() into gracefulShutdown(1) so a Discord-unreachable
  // replica crash-loops instead of silently serving 5xx.
  //
  // Combined mode keeps the legacy listener-then-login ordering. The
  // same cold-start race window exists every restart there
  // (listener up, client.rest token unset), but ECS health-check
  // gating typically dominates: traffic doesn't route to the task
  // until /health passes, and /health doesn't depend on Discord.
  // Not a regression here (matches pre-PR); follow-up to apply the
  // init-before-listen pattern to combined mode if the health-gate
  // assumption ever weakens.
  if (isHttp && !isGateway) {
    const timer = await initHttpOnly({ client, config, refreshCache, logger });
    // Tight race: SIGTERM during the await above runs gracefulShutdown,
    // which clears `httpRefreshTimer` (still null) and proceeds. The
    // setInterval inside initHttpOnly then registers AFTER the
    // clearInterval already happened. Guard against that here so a stray
    // refreshCache() doesn't fire mid-teardown. (.unref() means it can't
    // block exit either way; this just keeps the log noise clean.)
    if (isShuttingDown && timer) {
      clearInterval(timer);
    } else {
      httpRefreshTimer = timer;
    }
  }

  // HTTP listener.
  //   - isHttp / combined: full Express server (OAuth callback,
  //     webhooks, /health, /metrics). The ALB targets HTTP replicas
  //     only; gateway-only tasks aren't behind the ALB.
  //   - gateway-only: minimal /health responder so the container-level
  //     wget probe (re-added in qurl-integrations-infra follow-up to
  //     #151) can catch WebSocket disconnect / event-loop wedge /
  //     dispatch deadlock — failure modes the deployment_circuit_breaker
  //     misses because the node process stays alive. Without this,
  //     `desired_count=1` means one wedged gateway task = entire
  //     /qurl command surface dead until a human notices.
  if (isHttp) {
    httpServer = startServer();
  } else if (isGateway) {
    // Returns 503 until client.isReady() flips true (after READY
    // from the Discord gateway). Dockerfile --start-period=30s
    // covers this boot window so ECS doesn't replace the task early.
    httpServer = startGatewayHealthServer(() => client.isReady(), () => {
      // Null out so gracefulShutdown doesn't try to .close() a server
      // that's in an error state. Covers both the listen-window race
      // (server never finished listening — close() would log
      // "Server is not running") AND post-listen runtime errors
      // (server bound the port but emitted error later — close() is
      // still possible but unlikely to succeed cleanly during a
      // teardown that's already in flight).
      httpServer = null;
      gracefulShutdown(1);
    });
  }

  // Background retry-revoke for any OAuth tokens whose initial
  // revoke failed. Pinned to `isGateway` not because of any Discord
  // dependency (the sweeper only calls api.github.com — no Discord
  // client / REST calls anywhere in src/orphan-token-sweeper.js) but
  // to keep the sweeper a singleton: N HTTP replicas racing on the
  // same orphaned-tokens table would each claim and re-revoke every
  // row. Pinning to the single gateway process avoids that without
  // a distributed work queue. If this ever needs to scale beyond one
  // worker, replace with SQS / a Redis lock — not by spreading the
  // sweeper across HTTP replicas.
  if (isGateway) {
    startOrphanTokenSweeper();
  }

  // SQS consumer for the worker tier (zero-downtime Pillar 1).
  // Started after the HTTP listener is up so the /health endpoint
  // can accept probes during the consumer's first poll. Gated on
  // `isWorker` (which already requires ENABLE_EVENT_SHIPPER=true)
  // so the legacy in-process dispatch path stays untouched when
  // the flag is off. In combined mode + flag on, the same process
  // runs both producer (gateway role) and consumer — useful for
  // local dev and the initial flag-on soak.
  //
  // Shutdown-race guard: mirrors the timer guards above. If SIGTERM
  // landed during the awaits before this point, gracefulShutdown
  // has already run; skip starting a consumer that no one will stop.
  if (isWorker && !isShuttingDown) {
    eventConsumer.start(client);
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

    // Phase 1 monitoring — periodic gateway heartbeat + active-guild-count.
    // Started AFTER login so the first heartbeat sample sees a real ws.ping
    // and shard.lastPingTimestamp rather than -1 from the pre-login client.
    // Both .unref() inside the module so they don't pin shutdown.
    //
    // Shutdown-race guard: if SIGTERM landed during client.login() above,
    // gracefulShutdown has already cleared the (still-null) timer locals
    // and is racing to exit. Skip starting new timers in that case so we
    // don't register a setInterval that no one will ever clear. Mirrors
    // the httpRefreshTimer guard pattern at line 393.
    if (!isShuttingDown) {
      gatewayHeartbeatTimer = startGatewayHeartbeat(client);
      activeGuildCountTimer = startActiveGuildCount(client);
    }
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
