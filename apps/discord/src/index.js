const config = require('./config');
const logger = require('./logger');
const { isPositiveFinite } = require('./utils/time');
const { client, GATEWAY_INTENTS_BITFIELD, refreshCache, shutdown: discordShutdown } = require('./discord');
const { registerCommands, handleCommand } = require('./commands');
const { createGatewayWsShim } = require('./gateway-ws-shim');
const { createGatewaySessionStore } = require('./gateway-session-store');
const { createGatewayLock } = require('./gateway-lock');
const { createPeerHeartbeat } = require('./gateway-peer-heartbeat');
const { createGatewayHmac } = require('./gateway-hmac');
const { createGatewayLeader } = require('./gateway-leader');
const { createControlClient } = require('./gateway-control-client');
const { createConnectionWatchdog } = require('./gateway-connection-watchdog');
const { startControlChannelServer } = require('./gateway-control-channel');
const { loadGatewayHmacSecret } = require('./gateway-hmac-secret-loader');
const {
  shouldUsePushHandoffShutdown,
  selectGatewayReadinessProbe,
  awaitServerListening,
  tryStop,
  tryClose,
  runPushHandoffShutdown,
} = require('./gateway-shutdown-helpers');
const { DynamoDBClient } = require('@aws-sdk/client-dynamodb');
const { DynamoDBDocumentClient } = require('@aws-sdk/lib-dynamodb');
const { handleFlowInteraction } = require('./flow-dispatch');
const { startServer, stopIntervals: stopServerIntervals } = require('./server');
const { startGatewayHealthServer } = require('./gateway-health');
const { startGatewayHeartbeat, startActiveGuildCount, noteGatewayActivity } = require('./gateway-metrics');
const db = require('./store');
const { startOrphanTokenSweeper } = require('./orphan-token-sweeper');
const {
  missingBootKeys,
  missingProdKeys,
  missingKekRequiredKeys,
  baseUrlHttpsProblem,
  missingEventShipperKeys,
  missingViewUpdatePushKeys,
  missingMapCommandKeys,
  unsupportedRoleShipperCombo,
  unsupportedRoleResumeCombo,
  unsupportedRoleHotStandbyCombo,
  missingHotStandbyKeys,
  invalidHotStandbyValues,
  shouldRegisterInteractionListener,
  resolveProcessRole,
} = require('./boot-requirements');
const { initHttpOnly } = require('./http-only-init');
const eventConsumer = require('./event-consumer');
const eventPublisher = require('./event-publisher');
const viewUpdateConsumer = require('./view-update-consumer');
const viewUpdatePublisher = require('./view-update-publisher');
const webhookSubscriptions = require('./webhook-subscriptions');
const { LOG_KINDS } = require('./constants');

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
  // QURL_API_KEY is the global-fallback key for /qurl send + /qurl map.
  // Only the OpenNHP community server demands it at boot; single-guild-
  // plain and multi-tenant deployments rely on per-guild /qurl setup.
  // List is in boot-requirements.js for testability.
  const prodMissing = missingProdKeys(process.env, config.isOpenNHPActive);
  if (prodMissing.length > 0) {
    logger.error(`NODE_ENV=production but missing required env vars: ${prodMissing.join(', ')}`);
    logger.error('For KEY_ENCRYPTION_KEY, generate with: node -e "console.log(require(\'crypto\').randomBytes(32).toString(\'base64\'))"');
    process.exit(1);
  }

  // BASE_URL https check — fail fast at boot when the qURL guided setup flow
  // is configured (isQurlOAuthConfigured) but BASE_URL isn't a usable https
  // origin, so /qurl setup can't dead-end at the OAuth redirect later (#619).
  // The qURL OAuth router (server.js) mounts unconditionally, so this applies
  // to plain single-guild and multi-tenant deploys, not just one mode. See
  // baseUrlHttpsProblem for the consumer inventory + the operator-facing
  // message. baseUrlExplicitlySet treats "" / whitespace-only as unset
  // (matches GUILD_ID normalization) so an accidentally-empty SSM param
  // neither escapes the check nor false-positives a non-consuming deploy.
  const baseUrlExplicitlySet = Boolean(process.env.BASE_URL?.trim());
  const baseUrlProblem = baseUrlHttpsProblem(config, baseUrlExplicitlySet);
  if (baseUrlProblem) {
    logger.error(baseUrlProblem);
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
if (!isPositiveFinite(config.RATE_LIMIT_WINDOW_MS)) {
  logger.error('RATE_LIMIT_WINDOW_MS must be a positive integer (set to 0 would disable rate limiting)');
  process.exit(1);
}
if (!isPositiveFinite(config.RATE_LIMIT_MAX_REQUESTS)) {
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

// View-update push (feat #60). Same boot-time refusal pattern: if
// the flag is on but the queue URL is missing, fail closed at boot
// rather than silently dropping every view event at runtime.
const viewUpdatePushMissing = missingViewUpdatePushKeys(config);
if (viewUpdatePushMissing.length > 0) {
  logger.error(`ENABLE_VIEW_UPDATE_PUSH=true but missing required env vars: ${viewUpdatePushMissing.join(', ')}`);
  process.exit(1);
}

// Reject combined + flag-on. In combined mode the gateway-side
// publish hook AND the worker-side consumer would both arm in one
// process, double-dispatching every interaction (gateway WS frame
// + SQS round-trip). See unsupportedRoleShipperCombo for the full
// rationale and operator-facing remediation.
const roleShipperConflict = unsupportedRoleShipperCombo(PROCESS_ROLE, config.ENABLE_EVENT_SHIPPER);
if (roleShipperConflict) {
  logger.error(roleShipperConflict);
  process.exit(1);
}

// Gateway-RESUME (Pillar 2) precondition check. Rejects two shapes:
//   1. ENABLE_GATEWAY_RESUME=true with ENABLE_EVENT_SHIPPER=false
//      (the resume shim replaces discord.js Client and only has a
//      forward-to-SQS path; the in-process dispatcher would be
//      unreachable).
//   2. ENABLE_GATEWAY_RESUME=true with PROCESS_ROLE=combined (the
//      legacy Client owns the WS in combined mode; the shim would
//      conflict).
// Sequenced AFTER unsupportedRoleShipperCombo so the operator sees
// the shipper-first remediation when both are misconfigured, rather
// than chasing a downstream resume error.
const roleResumeConflict = unsupportedRoleResumeCombo(
  PROCESS_ROLE,
  config.ENABLE_GATEWAY_RESUME,
  config.ENABLE_EVENT_SHIPPER,
  config.STORE_TYPE,
);
if (roleResumeConflict) {
  logger.error(roleResumeConflict);
  process.exit(1);
}

// Pillar 3 hot-standby — sequenced AFTER unsupportedRoleResumeCombo
// so an operator who turned both flags on but forgot the prerequisites
// sees the RESUME-side fix first (the hot-standby gate then becomes a
// derivative of "RESUME is on"). Same boot-fail shape as the others
// above: log + exit(1), no partial-state teardown.
const roleHotStandbyConflict = unsupportedRoleHotStandbyCombo(
  PROCESS_ROLE,
  config.ENABLE_GATEWAY_HOT_STANDBY,
  config.ENABLE_GATEWAY_RESUME,
);
if (roleHotStandbyConflict) {
  logger.error(roleHotStandbyConflict);
  process.exit(1);
}

const hotStandbyMissing = missingHotStandbyKeys(config);
if (hotStandbyMissing.length > 0) {
  logger.error(
    `ENABLE_GATEWAY_HOT_STANDBY=true but required env vars missing: ${hotStandbyMissing.join(', ')}. ` +
    'INSTANCE_ID + INSTANCE_IP are derived in-process from `os.hostname()` and `os.networkInterfaces()` ' +
    '(env overrides accepted) — null or empty means the container has no hostname or no non-internal IPv4. ' +
    'GATEWAY_HANDOFF_HMAC is the SSM-decrypted JSON `{current, previous?}` secret. Verify the ' +
    'qurl-integrations-infra/qurl-bot-discord/terraform/main.tf wiring and re-deploy.'
  );
  process.exit(1);
}

const hotStandbyInvalid = invalidHotStandbyValues(config);
if (hotStandbyInvalid.length > 0) {
  for (const problem of hotStandbyInvalid) {
    logger.error(problem);
  }
  process.exit(1);
}

// Parse + validate the HMAC secret AT BOOT, not inside startHotStandby.
// All hot-standby misconfigs (env-var presence AND secret-format validity)
// surface upfront before any I/O — same posture as missingHotStandbyKeys.
// Stashed in a module-level so startHotStandby can read the parsed value
// without re-parsing (and without drifting if the parse logic changes).
//
// `config.takeGatewayHandoffHmac()` reads the raw value from the
// module-private binding in config.js and nulls that binding — see
// the takeGatewayHandoffHmac definition for the heap-dump rationale.
// Called once at boot here; calling again would return undefined.
let gatewayHmacSecrets = null;
if (config.ENABLE_GATEWAY_HOT_STANDBY) {
  try {
    gatewayHmacSecrets = loadGatewayHmacSecret(config.takeGatewayHandoffHmac());
  } catch (err) {
    logger.error(err.message);
    process.exit(1);
  }
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

// Pillar 2 gateway-resume shim. Constructed once at module load when
// PROCESS_ROLE=gateway AND ENABLE_GATEWAY_RESUME=true. Owns its own
// @discordjs/ws WebSocketManager (replacing the legacy
// `client.login()` path), persists session state to DDB, and
// forwards every dispatch to local subscribers via shim.onDispatch.
// Stays null in every other configuration (flag-off, http tier,
// worker tier) so the legacy code paths run unchanged.
//
// Constructed at module load rather than inside start() because the
// gracefulShutdown path needs a reference to it for the SIGTERM
// store-flush; lazy construction would race the shutdown handler.
//
// The DDB session-table name is derived from `DDB_TABLE_PREFIX` (the
// same env-specific prefix the rest of the bot uses for its DDB
// tables) so a future env-name rename touches one var, not many.
// Both this module and the qurl-integrations-infra qurl-bot-ddb
// module pin the `gateway-session` suffix.
// Pillar 3 hot-standby plumbing. Constructed inside startHotStandby
// (the leader factory needs the shim's WebSocketManager handle, which
// only exists after `gatewayShim.start({ connect: false })` resolves).
// Hoisted to module scope so gracefulShutdown + signal handlers can
// see them; tryClose/tryStop are null-guarded so a SIGTERM mid-
// construction is safe, and the `isShuttingDown` re-check in
// startHotStandby closes the inverse race.
let gatewayLeader = null;
let controlChannelServer = null;
let connectionWatchdog = null;

// Shared DDB client. Constructed at module load when ENABLE_GATEWAY_RESUME=true
// so the Pillar 2 gateway-session store AND the Pillar 3 lock + peer-
// heartbeat tables can share a single connection pool. Building two
// clients would double the SDK's HTTPS keep-alive sockets to the
// same DDB endpoint with no upside.
let sharedGatewayDdbClient = null;

let gatewayShim = null;
if (isGateway && config.ENABLE_GATEWAY_RESUME) {
  // DDB_TABLE_PREFIX and AWS_REGION are validated upstream: the
  // STORE_TYPE=ddb requirement comes from unsupportedRoleResumeCombo
  // (above), and ddb-store.js throws on either env-var missing when
  // it loads via `require('./store')` at the top of this file —
  // before this branch runs. Use config.DDB_TABLE_PREFIX (already
  // trimmed) so the table-name computation matches every other
  // DDB call site.
  const ddbTablePrefix = config.DDB_TABLE_PREFIX;
  const awsRegion = process.env.AWS_REGION;
  const rawDdbClient = new DynamoDBClient({
    region: awsRegion,
    ...(process.env.DDB_TEST_ENDPOINT ? { endpoint: process.env.DDB_TEST_ENDPOINT } : {}),
  });
  sharedGatewayDdbClient = DynamoDBDocumentClient.from(rawDdbClient, {
    marshallOptions: { removeUndefinedValues: true },
  });
  const sessionStore = createGatewaySessionStore({
    ddbClient: sharedGatewayDdbClient,
    tableName: `${ddbTablePrefix}gateway-session`,
    shardId: config.SHARD_ID,
    logger,
  });
  gatewayShim = createGatewayWsShim({
    token: config.DISCORD_TOKEN,
    intents: GATEWAY_INTENTS_BITFIELD,
    store: sessionStore,
    logger,
  });
  logger.info('gateway-resume shim constructed', {
    tableName: `${ddbTablePrefix}gateway-session`,
    shardId: config.SHARD_ID,
  });
}

// Log isWorker on its own so an operator triaging a no-dispatch
// incident sees the full picture without having to mentally combine
// the role log (line 86) with the flag. Distinct line so a grep for
// `isWorker=true` lands directly.
logger.info('Worker tier configured', { isWorker, eventShipperEnabled: config.ENABLE_EVENT_SHIPPER });

// Interaction routing. Three disjoint shapes:
//
//   * Gateway tier + flag-on: discord.js emits `interactionCreate`
//     on the gateway WS frame, but we do NOT subscribe — the raw
//     publish hook (registered in the isGateway block below) forwards
//     the payload to SQS instead. The worker tier picks it up.
//   * Worker tier + flag-on: the SQS consumer reconstructs the
//     interaction via `client.actions.InteractionCreate.handle(data)`,
//     which emits `interactionCreate` locally. We DO subscribe so
//     handleCommand / handleFlowInteraction run.
//   * Combined or split + flag-off: legacy in-process path. The
//     gateway WS emit lands on the local listener directly.
//
// The shouldRegisterInteractionListener predicate (in
// boot-requirements.js) collapses every role × flag permutation
// into a single boolean. Combined + flag-on is rejected at boot by
// unsupportedRoleShipperCombo before reaching here — see that
// helper's comment for the double-dispatch hazard.
if (shouldRegisterInteractionListener({
  isGateway,
  isHttp,
  eventShipperEnabled: config.ENABLE_EVENT_SHIPPER,
})) {
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
    let result;
    if (interaction.isChatInputCommand() || interaction.isAutocomplete()) {
      result = handleCommand(interaction);
    } else if (interaction.isMessageComponent() || interaction.isModalSubmit()) {
      result = handleFlowInteraction(interaction);
    } else {
      // Future Discord interaction types we don't ship today fall
      // through here. Log at debug so an operator triaging "why isn't
      // my new component routing" sees the unrouted type rather than
      // the silent-drop black box the pre-conversion code provided.
      // No trackDispatch call: there's no handler promise to register,
      // and the consumer's at-cap accounting should not count the
      // unrouted no-op against its in-flight budget.
      logger.debug('interactionCreate: unrouted interaction type', {
        type: interaction.type,
      });
      return undefined;
    }
    // Register the dispatch promise with the consumer's backpressure
    // tracker. No-op for gateway-WS-driven dispatches (the consumer's
    // isWorkerDispatch flag is only true during its synchronous emit
    // call); during a consumer-driven emit, the increment runs here
    // and the decrement fires when the handler settles.
    //
    // No trailing `return result;` — EventEmitter listeners discard
    // their return value, and a pre-conversion vestigial return would
    // imply otherwise to a future reader. The unrouted branch's
    // `return undefined` IS load-bearing (it short-circuits before
    // trackDispatch), so the asymmetry is intentional.
    eventConsumer.trackDispatch(result);
  });
}

// Gateway-only client event wiring. Skipped on HTTP-only replicas
// (the client never logs in there) and on the shim path (the shim
// owns the WS — listeners attach via shim.onDispatch instead).
if (isGateway && !config.ENABLE_GATEWAY_RESUME) {
  // Register commands when ready
  client.once('ready', async () => {
    // registerCommands takes REST + appId + a guilds map so the
    // shim path can call the same function without a Client.
    await registerCommands({
      rest: client.rest,
      appId: client.application.id,
      guilds: new Map([...client.guilds.cache.values()].map((g) => [g.id, g.name])),
    });
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

  // Publish-to-SQS hook for the worker tier (zero-downtime Pillar 1).
  // Fires on every gateway dispatch; `publish` filters internally to
  // `INTERACTION_CREATE` (single string-eq) before any allocation,
  // so non-interaction dispatches pay only the comparison cost.
  // Listener registered here at module-top because discord.js's raw
  // event fires inside the WebSocketShard's onPacket — by the time
  // login() resolves, frames are already arriving. publisher.start()
  // (called in start() below before client.login()) constructs the
  // SQS client; publish() drops at debug if a frame somehow arrives
  // pre-start.
  //
  // Gated on `config.ENABLE_EVENT_SHIPPER` — when the flag is off,
  // the legacy in-process listener (registered above) handles
  // dispatch and no SQS round-trip is needed. Function-reference
  // (not closure) to keep v8's hidden-class shape stable across
  // every WS frame, matching the noteGatewayActivity pattern.
  if (config.ENABLE_EVENT_SHIPPER) {
    client.on('raw', eventPublisher.publish);
  }

  // Error handling
  client.on('error', error => {
    logger.error('Discord client error', { error: error.message });
  });
}

// Shim-path dispatch listener. One handler, three branches —
// keeping a single closure on the per-dispatch hot path instead of
// fanning out to three. The shim fires only for op=0 dispatches
// (@discordjs/ws absorbs the control frames internally), so the
// publish op-filter is trivially satisfied; the t-filter inside
// eventPublisher.publish remains the load-bearing INTERACTION_CREATE
// gate.
//
// registerCommands fires only on the FIRST READY in this process.
// Discord delivers RESUMED (not READY) on a successful resume,
// but READY *can* land twice: a >60s outage expires the resume
// buffer, RESUME is rejected, fresh IDENTIFY yields a fresh READY.
// The once-flag mirrors the legacy path's `client.once('ready', …)`.
if (isGateway && config.ENABLE_GATEWAY_RESUME && gatewayShim) {
  let commandsRegistered = false;
  gatewayShim.onDispatch(({ data }) => {
    if (data?.t === 'READY' && !commandsRegistered) {
      const appId = gatewayShim.getAppId();
      if (!appId) {
        // READY without application.id is a degenerate Discord shape
        // (the spec requires it). Skip registration rather than
        // route a request through `/applications/null/commands`,
        // and don't latch commandsRegistered so a subsequent
        // well-formed READY can recover.
        logger.warn('registerCommands (shim path) skipped: appId not populated by READY');
      } else {
        commandsRegistered = true;
        // Gateway-tier shim doesn't maintain a guild cache (that's
        // the worker tier's job); empty map skips purge. The
        // handleCommand dispatch-time filter remains the correctness
        // guarantee for stale registrations.
        registerCommands({
          rest: gatewayShim.getRest(),
          appId,
          guilds: new Map(),
        }).catch((err) => {
          logger.error('registerCommands (shim path) failed', { error: err.message });
        });
      }
    }
    noteGatewayActivity();
    eventPublisher.publish(data);
  });
}

// Log and continue on unhandled rejections. The old behavior killed the
// entire process on any stray rejection (transient Discord timeouts, network
// blips) which made the bot fragile. Truly fatal errors surface via
// uncaughtException below.
// `kind: 'unhandledRejection'` matches the tag emitted by
// trackDispatch's .catch in event-consumer.js. A single CloudWatch
// query filtering on the structured field finds both this gateway-WS-
// driven path AND the worker-tier path that absorbs rejections in the
// SQS consumer. Without the parity, dashboards either grep the
// message text or miss one of the tiers.
process.on('unhandledRejection', (error, _promise) => {
  logger.error('Unhandled promise rejection (logged, not fatal)', {
    kind: LOG_KINDS.UNHANDLED_REJECTION,
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
    await tryClose('HTTP server', httpServer, logger);
    stopServerIntervals();
    // SQS consumer drain. Stops new ReceiveMessage calls, then
    // awaits the current poll iteration's in-flight `processMessage`
    // promises. Has to run BEFORE db.close() — running handlers may
    // still be reading/writing flow-state DDB rows on the way to
    // ACK'ing the interaction. Idempotent + a no-op when the
    // consumer was never started, so unconditional here.
    await eventConsumer.stop();
    // SQS publisher drain. Same shape as the consumer above:
    // idempotent + no-op when never started, so unconditional.
    // Runs AFTER eventConsumer.stop() but both are bounded by their
    // own DRAIN_DEADLINE_MS; in the split shape only one of them is
    // actually running per process (combined + flag-on is rejected
    // at boot), so the sequencing matters only as documentation.
    await eventPublisher.stop();
    // View-update plumbing drain (feat #60). Same idempotent shape;
    // unconditional. Consumer + publisher are stopped in parallel
    // via Promise.all so the combined drain stays within the
    // gracefulShutdown 10s budget — sequencing each module's
    // DRAIN_DEADLINE_MS (3s each) plus the event-shipper drains
    // above would push worst-case past 10s. Order-independence is
    // safe: publisher.stop() snapshots inFlightSends; consumer.stop()
    // aborts the long-poll; neither depends on the other's state.
    await Promise.all([
      viewUpdateConsumer.stop(),
      viewUpdatePublisher.stop(),
    ]);
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
    // Pillar 3 standby-path teardown. Active replicas take the
    // pushHandoffShutdown branch instead; this code only runs when
    // hot-standby is off, OR hot-standby is on but THIS replica is
    // the standby (no lock to push).
    //
    // Order matters: close the control-channel server FIRST so no
    // late inbound handoff envelope can land on a half-stopped
    // leader (handleInboundHandoff against a leader whose tick loop
    // has already exited is technically a no-op, but the explicit
    // ordering makes the no-inbound-during-teardown invariant load-
    // bearing rather than incidental). Watchdog stops next so its
    // tick can't fire a manager.connect() during teardown. Leader
    // last so any in-flight tick observes running=false and exits.
    //
    // Gated on ENABLE_GATEWAY_HOT_STANDBY for symmetry with the
    // Pillar 2 shim block below: all three handles are null when
    // hot-standby is off, so the tryClose/tryStop calls are no-ops,
    // but skipping the gate would still pay 3 microtask hops per
    // teardown across every HTTP-only / combined replica that never
    // built the hot-standby surface.
    //
    // No per-call timeout on tryStop/tryClose: a wedged
    // gatewayLeader.stop() (e.g., DDB hanging in the final renew)
    // is bounded by the 10 s `force-exit` setTimeout at the top of
    // this function — which itself sits inside ECS's 30 s SIGTERM
    // deadline. That layered ceiling is the deliberate outermost
    // belt; introducing a third per-call timeout here would just
    // multiply the moving parts without changing the worst-case.
    if (config.ENABLE_GATEWAY_HOT_STANDBY) {
      await tryClose('control-channel server', controlChannelServer, logger);
      await tryStop('connection-watchdog', connectionWatchdog, logger);
      await tryStop('gateway-leader', gatewayLeader, logger);
    }

    // Discord client shutdown only meaningful when we're the gateway
    // role. HTTP-only replicas never called login(), so there's no
    // WebSocket to close — discordShutdown() on an un-logged-in
    // client just releases the event emitter handles.
    //
    // Pillar 2 shim path is structurally different: the shim owns
    // the WebSocket (the legacy Client never logged in). stop()
    // flushes the session store synchronously then drops manager
    // state WITHOUT calling manager.destroy() — that's the
    // load-bearing SIGTERM contract that keeps Discord's resume
    // buffer alive for the next process. The TCP socket drops when
    // process.exit() fires below, which Discord treats as a network
    // disconnect rather than a clean close.
    if (isGateway && config.ENABLE_GATEWAY_RESUME && gatewayShim) {
      try {
        await gatewayShim.stop();
      } catch (err) {
        logger.error('gateway shim stop failed', { error: err.message });
      }
    } else if (isGateway && !config.ENABLE_GATEWAY_RESUME) {
      // Explicit !ENABLE_GATEWAY_RESUME (vs bare `else if (isGateway)`)
      // so a future refactor that wraps shim construction in try/catch
      // (leaving `gatewayShim` null when the flag is on) can't silently
      // fall through to discordShutdown() — the legacy path would
      // call .destroy() on an un-logged-in Client, which is harmless
      // today but masks the underlying construction failure.
      await discordShutdown();
    }
    await db.close();
    logger.info('Shutdown complete');
  } catch (error) {
    logger.error('Error during shutdown', { error: error.message });
  }

  process.exit(code);
}

// Hot-standby push-handoff SIGTERM path. The body lives in
// `runPushHandoffShutdown` (gateway-shutdown-helpers.js) so the
// timeout / exit-code / handoff-result / publisher-drain contracts
// are unit-testable; this wrapper owns the `isShuttingDown` gate.
//
// eventPublisher is passed in so its DRAIN_DEADLINE_MS bounded
// `.stop()` runs in parallel with pushHandoff — the outgoing
// process's in-flight SQS sends carry dispatches the standby
// can't replay (they arrived on OUR WebSocket).
//
// Defaults left implicit (forcedExitCode=1, ceilingMs=12_000): the
// 12 s ceiling = 9 s pushHandoff race + 3 s headroom, comfortably
// inside ECS's 30 s SIGTERM-to-SIGKILL window. If that deadline
// ever changes, runPushHandoffShutdown's DEFAULT_CEILING_MS is
// the load-bearing knob to revisit.
async function pushHandoffShutdown(code = 0) {
  if (isShuttingDown) return;
  isShuttingDown = true;
  await runPushHandoffShutdown({ code, gatewayLeader, eventPublisher, logger });
}

// SIGTERM during boot (gatewayLeader still null) falls through to
// gracefulShutdown via shouldUsePushHandoffShutdown's null-leader
// check — pinned in gateway-shutdown-helpers.test.js.
async function signalShutdown() {
  if (shouldUsePushHandoffShutdown({
    enableHotStandby: config.ENABLE_GATEWAY_HOT_STANDBY,
    gatewayLeader,
  })) {
    await pushHandoffShutdown(0);
  } else {
    await gracefulShutdown(0);
  }
}

// SIGTERM vs SIGINT split: only SIGTERM routes through signalShutdown
// → pushHandoffShutdown. SIGINT (dev ctrl-c, manual `kill -2` from an
// operator) goes straight to plain gracefulShutdown. Triggering a
// real DDB CAS + cross-AZ HTTP round-trip on a developer's ctrl-c
// is overkill, and ECS sends SIGTERM for production replacement —
// so push-handoff lives on the production-deploy signal exclusively.
const onShutdownReject = (label) => (err) => {
  logger.error(`${label} rejected unexpectedly`, { error: err.message, stack: err.stack });
  process.exit(1);
};
process.on('SIGTERM', () => {
  logger.info('Received SIGTERM');
  signalShutdown().catch(onShutdownReject('signalShutdown'));
});
process.on('SIGINT', () => {
  logger.info('Received SIGINT');
  gracefulShutdown(0).catch(onShutdownReject('gracefulShutdown'));
});

// Pillar 3 hot-standby wiring. Called from start() AFTER the shim is
// constructed and started in connect-deferred mode. Sequencing matters:
//
//   1. Construct lock + peer-heartbeat (DDB-backed, no manager dep).
//   2. Load + validate the HMAC secret (JSON shape + hex format).
//   3. Construct hmac, controlClient, leader (leader is wired against
//      `gatewayShim` itself — the shim provides the connect() +
//      isConnected() contract that @discordjs/ws's WebSocketManager
//      lacks isConnected() for).
//   4. Start the control-channel HTTP server. AWAIT `listening` event
//      before continuing — if we start the leader first, the peer could
//      acquire the lock and pushHandoff to us before our listener is
//      up, dropping the connection.
//   5. Start the leader tick loop. The watchdog wakes inside the tick
//      flow on the active path; standby just heartbeats and waits.
//
// Errors propagate to start().catch() → gracefulShutdown(1). Constructing
// inside a single function (vs. spreading across start()) keeps the
// chain of dependencies legible and the rollback ordering implicit.
async function startHotStandby() {
  const ddbTablePrefix = config.DDB_TABLE_PREFIX;
  const lockTableName = `${ddbTablePrefix}gateway-lock`;
  const peerHeartbeatTableName = `${ddbTablePrefix}gateway-peer-heartbeat`;
  // `lockHolder` identifies the ROLE-owning entity (one shard, one
  // gateway role), NOT the replica. Both active + standby replicas
  // of the same shard share the same lockHolder string — replica
  // disambiguation lives on `instanceId` (passed separately to the
  // lock primitive, used in the heartbeat row + DDB CAS attribution).
  const lockHolder = `${config.SHARD_ID}#gateway`;

  const lock = createGatewayLock({
    ddbClient: sharedGatewayDdbClient,
    tableName: lockTableName,
    shardId: config.SHARD_ID,
    instanceId: config.INSTANCE_ID,
    lockHolder,
    logger,
  });

  const peerHeartbeat = createPeerHeartbeat({
    ddbClient: sharedGatewayDdbClient,
    tableName: peerHeartbeatTableName,
    instanceId: config.INSTANCE_ID,
    ip: config.INSTANCE_IP,
    port: config.GATEWAY_CONTROL_PORT,
    shardId: config.SHARD_ID,
    lockHolder,
    logger,
  });

  // Secrets were parsed + validated at module load (see boot chain
  // adjacent to missingHotStandbyKeys). Read the stash here, then
  // null it once createGatewayHmac has captured the material
  // internally. The raw env value is already unreachable — the
  // one-shot config.takeGatewayHandoffHmac() at boot nulled the
  // private binding in config.js — so nulling gatewayHmacSecrets
  // here closes the remaining module-scope reference. The live
  // HMAC instance still holds the strings for sign/verify (the
  // only retained reference, unavoidable).
  const hmac = createGatewayHmac({ secrets: gatewayHmacSecrets, logger });
  gatewayHmacSecrets = null;

  const controlClient = createControlClient({ hmac, logger });

  // Belt-and-suspenders: shim.start({ connect: false }) constructs
  // the manager synchronously inside its await, so by the time we
  // get here isStarted() should always be true. If a future refactor
  // moves construction later (e.g. lazy on first connect), this
  // guard surfaces the wiring regression at boot rather than as a
  // delayed leader/watchdog runtime error.
  //
  // Not redundant with the leader/watchdog factory typeof checks:
  // those pass when called before start() too, because the shim
  // exposes connect()/isConnected() unconditionally. This catches
  // the wiring-order regression those checks would miss.
  if (!gatewayShim.isStarted()) {
    throw new Error('startHotStandby: gatewayShim.isStarted() is false — shim.start() ordering regression');
  }

  // The shim itself satisfies the leader/watchdog `manager` contract
  // (connect() + isConnected()). Passing the raw @discordjs/ws
  // WebSocketManager would fail the factory's typeof check because
  // upstream exposes only fetchStatus() (async) — see gateway-ws-shim
  // module header "Pillar 3 manager contract".
  gatewayLeader = createGatewayLeader({
    lock,
    peerHeartbeat,
    controlClient,
    manager: gatewayShim,
    selfInstanceId: config.INSTANCE_ID,
    shardId: config.SHARD_ID,
    logger,
  });

  // Wait for `listening` before starting the leader. server.listen()
  // is async; if we don't await it, the leader can win the lock and
  // its peer can pushHandoff to us before we accept TCP connections.
  controlChannelServer = startControlChannelServer({
    hmac,
    selfInstanceId: config.INSTANCE_ID,
    isKnownPeer: gatewayLeader.isKnownPeer,
    onHandoff: gatewayLeader.handleInboundHandoff,
    logger,
    port: config.GATEWAY_CONTROL_PORT,
    bindAddr: config.GATEWAY_CONTROL_BIND_ADDR,
    onListenError: (err) => {
      // Listener failure is fatal. The peer can't reach us, so no
      // pushHandoff will succeed — we'd block the next deploy
      // indefinitely. Route through gracefulShutdown(1) to clean up
      // the leader / lock release / etc.
      //
      // Null out controlChannelServer so gracefulShutdown's
      // `tryClose` skips a server that's in an error state.
      // Mirrors the same defensive null-out in
      // startGatewayHealthServer's error callback below.
      //
      // `.catch` mirrors the SIGTERM/SIGINT handlers' defense-in-depth:
      // gracefulShutdown handles its own errors internally, but a
      // future refactor that introduces an uncaught reject path would
      // otherwise surface as an unhandledRejection on the process.
      logger.error('control-channel listener fatal error; shutting down', { error: err.message });
      controlChannelServer = null;
      gracefulShutdown(1).catch(shutdownErr => {
        logger.error('gracefulShutdown rejected unexpectedly from onListenError', {
          error: shutdownErr.message, stack: shutdownErr.stack,
        });
        process.exit(1);
      });
    },
  });
  await awaitServerListening(controlChannelServer);

  // SIGTERM-during-boot race guard. If a signal landed while we were
  // awaiting the `listening` event, gracefulShutdown has already
  // started tearing down the partial state (it sees a non-null
  // gatewayLeader + controlChannelServer because they're assigned
  // synchronously above). Continuing past this point would call
  // .start() on the leader / watchdog after their gracefulShutdown
  // .stop() already ran, leaving an unsupervised tick loop that
  // process.exit() doesn't reap until SIGKILL.
  if (isShuttingDown) {
    return;
  }

  // INVARIANT: no `await` below this guard. Everything from here to
  // the function's return runs synchronously, so a SIGTERM can't
  // preempt — the OS-level signal is queued until the JS micro-
  // tasks settle and signalShutdown observes `isShuttingDown=true`
  // via the `gracefulShutdown` re-entry gate. Adding an async hop
  // here silently reopens the SIGTERM-mid-startup race.

  // Watchdog drives the active path's manager.connect() on lock
  // acquisition + monitors the connection during the active lifetime.
  // `releaseLock` is wired to leader.releaseLockForImmediateExit
  // (not lock.releaseLock directly) so the watchdog's failure-replace
  // path goes through the same serialization chain that pushHandoff /
  // inbound-handoff use. Without this, the watchdog could release the
  // lock concurrently with an in-flight transferLock and the DDB
  // row would land in a state neither replica expects.
  connectionWatchdog = createConnectionWatchdog({
    manager: gatewayShim,
    isHoldingLock: gatewayLeader.isHoldingLock,
    isConnecting: gatewayLeader.isConnecting,
    releaseLock: gatewayLeader.releaseLockForImmediateExit,
    deleteOwnRow: peerHeartbeat.deleteOwnRow,
    logger,
  });

  // gatewayLeader.start() and connectionWatchdog.start() are
  // sync-by-design: each schedules its own tick loop via
  // setTimeout, captures the loop promise in a closure (so it
  // can be awaited by stop()), and returns. Awaiting here would
  // hang — the loop only resolves on stop(). If a future refactor
  // adds an async pre-flight (e.g., warming a connection), that
  // pre-flight should resolve BEFORE `.start()` returns so any
  // thrown error still routes through `start().catch()`.
  gatewayLeader.start();
  connectionWatchdog.start();

  logger.info('Pillar 3 hot-standby wired', {
    instanceId: config.INSTANCE_ID,
    instanceIp: config.INSTANCE_IP,
    controlPort: config.GATEWAY_CONTROL_PORT,
    lockTable: lockTableName,
    peerHeartbeatTable: peerHeartbeatTableName,
  });
}

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
    // Webhook subscription registration is OUT-OF-PROCESS for the
    // bot's default key, via the webhook-registrar Lambda — see
    //   apps/discord/lambda/webhook-registrar/index.js  (handler)
    //   apps/discord/lambda/webhook-registrar/README.md  (bundling)
    //   apps/discord/docs/qurl-webhook-rollout.md        (operator flow)
    // The Lambda is invoked once per deploy via Terraform and writes
    // the default-key secret to SSM; the bot reads QURL_WEBHOOK_SECRET
    // from env at boot.
    //
    // PER-GUILD subscriptions (BYOK view counter) are in-process:
    // every guild that links its own API key (via /qurl setup or the
    // OAuth callback) gets its own subscription. The registry below
    // owns the in-memory map<owner_id, secret> the multi-secret
    // receiver consults on every inbound webhook, primes from
    // guild_configs at boot, and refreshes every 30s — including
    // re-discovering the default-key owner_id by listing the
    // Lambda's subscription. See src/webhook-subscriptions.js.
    //
    // Fire-and-forget: the receiver returns 503 until cachePrimed is
    // true, so a slow first-scan doesn't drop events — qurl-service
    // retries 503. Awaiting here would delay the HTTP listener
    // unnecessarily.
    webhookSubscriptions.start().catch((err) => {
      logger.error('webhook-subscriptions registry initial scan crash', { error: err?.message });
    });
  } else if (isGateway) {
    // Returns 503 until isReady() flips true (after READY from the
    // Discord gateway). Dockerfile --start-period=30s covers this
    // boot window so ECS doesn't replace the task early.
    //
    // Pillar 2 shim path: poll shim.isReady() instead of
    // client.isReady() — the legacy Client never logs in under the
    // shim, so client.isReady() always returns false there. The
    // shim's flag flips to true on the first READY dispatch
    // (cold-start path) or on the first RESUMED dispatch (cross-
    // process resume path); both are equivalent for "health check
    // should pass."
    const isReadyFn = selectGatewayReadinessProbe({
      enableHotStandby: config.ENABLE_GATEWAY_HOT_STANDBY,
      enableGatewayResume: config.ENABLE_GATEWAY_RESUME,
      gatewayShim,
      getGatewayLeader: () => gatewayLeader,
      client,
    });
    httpServer = startGatewayHealthServer(isReadyFn, () => {
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
  // the flag is off. Combined + flag-on is rejected at boot
  // (unsupportedRoleShipperCombo), so this and the publisher.start()
  // below are necessarily disjoint per-process.
  //
  // Shutdown-race guard: mirrors the timer guards above. If SIGTERM
  // landed during the awaits before this point, gracefulShutdown
  // has already run; skip starting a consumer that no one will stop.
  if (isWorker && !isShuttingDown) {
    // onFatal routes pollLoop's permanent-AWS-error path through
    // gracefulShutdown(1) so in-flight handler drain, db.close() WAL
    // checkpoint, and the discord WebSocket close all run before
    // exit. Without this, a direct process.exit(1) from event-consumer
    // would leak the same partial state the `start().catch()` comment
    // below explicitly calls out.
    eventConsumer.start(client, { onFatal: () => gracefulShutdown(1) });
  }

  // SQS publisher for the gateway tier (zero-downtime Pillar 1).
  // Started BEFORE client.login() so the SQS client exists by the
  // time the first gateway frame arrives — the `client.on('raw',
  // eventPublisher.publish)` registration at module-top fires
  // synchronously inside discord.js's WebSocketShard.onPacket as
  // soon as login resolves, and a frame that arrived pre-start()
  // would otherwise drop at debug (defense-in-depth in publish()).
  // Same shutdown-race guard as the consumer above.
  if (isGateway && config.ENABLE_EVENT_SHIPPER && !isShuttingDown) {
    eventPublisher.start();
  }

  // View-update SQS plumbing (feat #60, sub-second view counter).
  // Independent of the event-shipper gates above; gated only on
  // `config.ENABLE_VIEW_UPDATE_PUSH` AND `isHttp` (the tier that
  // owns BOTH the webhook receiver — publisher — and the
  // monitorLinkStatus instances — consumer). Same shutdown-race
  // guard as the event-shipper sibling above.
  //
  // Combined mode is intentionally NOT rejected here (unlike
  // ENABLE_EVENT_SHIPPER): there's no in-process direct-dispatch
  // path competing with the SQS round-trip — both paths converge
  // in consumer → registry → monitor. Combined mode just means
  // the publisher and consumer live in the same process; messages
  // still round-trip through SQS, and registry dispatch + status
  // === 'opened' guards handle any race or redelivery.
  if (config.ENABLE_VIEW_UPDATE_PUSH && isHttp && !isShuttingDown) {
    viewUpdatePublisher.start();
    viewUpdateConsumer.start({ onFatal: () => gracefulShutdown(1) });
  }

  // Open the Discord gateway WebSocket. Two disjoint paths:
  //
  //   - Pillar 2 (ENABLE_GATEWAY_RESUME=true): hydrate the persisted
  //     session from DDB, then start the @discordjs/ws shim. On the
  //     RESUME path the shim's `retrieveSessionInfo` returns the
  //     hydrated row and Discord replays buffered events since the
  //     last sequence (no IDENTIFY). On the cold-start path
  //     (sandbox fresh boot / resume window expired) the mirror is
  //     null and @discordjs/ws falls back to IDENTIFY.
  //   - Legacy (flag-off): client.login() with the same 30s timeout
  //     that the pre-Pillar-2 code carried.
  //
  // Both are gated on `isGateway`. HTTP-only replicas never open a
  // second Gateway connection on the bot token (Discord would flap
  // session identity between the two WebSockets).
  if (isGateway && config.ENABLE_GATEWAY_RESUME && gatewayShim) {
    const hydrated = await gatewayShim.hydrate();
    logger.info('gateway-resume hydrate complete', {
      // Log "resume" vs "cold start" as an SLI — operators can
      // correlate restart frequency with successful-resume rate.
      // Under hot-standby, the standby's hydrated mirror is largely
      // wasted (any inbound push-handoff carries a fresh snapshot
      // that replaces it). The boot sequence stays symmetric across
      // active/standby so the hydrate path remains a single code path
      // worth one log line for SLI parity.
      mode: hydrated ? 'resume' : 'cold-start',
    });
    // Under hot-standby, both replicas construct the manager + attach
    // listeners but only the lock-holder calls manager.connect().
    // gateway-leader (active path) and the inbound-handoff handler
    // (standby-becoming-active path) drive connect() themselves —
    // see gateway-ws-shim.js's `start({ connect })` comment for the
    // session-flap hazard the seam prevents.
    await gatewayShim.start({ connect: !config.ENABLE_GATEWAY_HOT_STANDBY });

    if (config.ENABLE_GATEWAY_HOT_STANDBY && !isShuttingDown) {
      await startHotStandby();
    }
    // No equivalent of startGatewayHeartbeat / startActiveGuildCount
    // under the shim today — those probe discord.js Client's
    // WebSocketManager shape (client.ws.shards[*].lastPingTimestamp)
    // which doesn't exist when the shim owns the WS. Tracked as a
    // follow-up: port the heartbeat / active-guild observability to
    // a shim-aware snapshot. The gateway-health server's /health
    // endpoint (isReady() probe) remains the load-bearing
    // ECS-replacement signal in the interim.
  } else if (isGateway) {
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
