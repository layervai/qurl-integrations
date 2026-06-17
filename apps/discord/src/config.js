const os = require('os');

// Prod safety guard: refuse to boot with DDB_TEST_ENDPOINT set under
// NODE_ENV=production. `DDB_TEST_ENDPOINT` is a local-dev / mock-test
// hook honored by `ddb-store.js`, `flow-state.js`, and
// `gateway-session-store.js`; a stale value leaking into a production
// env (CI variable copied across envs, dev `.env` accidentally
// shipped, container template propagating the local-dev block) would
// silently redirect every DDB call to whatever the endpoint resolves
// to — the same kind of silent-redirect footgun the DDB_TABLE_PREFIX
// guard in `ddb-store.js` closes. Guarding here in `config.js` (the
// first module loaded) fires before any DDB client constructor in
// the require graph.
if (process.env.NODE_ENV === 'production' && process.env.DDB_TEST_ENDPOINT) {
  throw new Error(`DDB_TEST_ENDPOINT='${process.env.DDB_TEST_ENDPOINT}' is set under NODE_ENV=production. This env var is for local-dev / aws-sdk-client-mock only — unset it in the production deployment template before booting.`);
}

// Sync derivation for INSTANCE_ID / INSTANCE_IP (hot-standby identity).
// Each helper runs once at module-load and is cached into the exported
// config object — readers of `config.INSTANCE_ID` see a frozen value,
// not a re-evaluation on access.
//
// ECS Fargate awsvpc gives each task its own hostname and ENI, but it
// does NOT substitute `${ECS_TASK_ARN}` into task-def env-var values
// (only `command = []` and `entryPoint = []` see that interpolation).
// Rather than spin up an async ECS-metadata-endpoint fetch at boot —
// which would push the missing-env guard past module load and add an
// HTTP dependency to startup — derive both values from Node's `os`
// module, which is sync, network-free, and works identically in ECS,
// local docker, and unit tests. Per-field semantics are commented at
// the call sites below.
//
// LOAD-BEARING INVARIANT for the lock primitive: two replicas in the
// same ECS service MUST see different INSTANCE_ID values. Fargate
// assigns a unique short alphanumeric hostname per task, which
// satisfies this — but if a future runtime ever reused hostnames
// across replicas in the same service, the DDB lock would short-
// circuit and both replicas would believe they hold leadership.
// The peer-heartbeat row collision (two writers on the same composite
// key) would surface post-deploy as a telemetry signal.
// Env overrides are trimmed for parity with GUILD_ID / STORE_TYPE /
// ALLOWED_GITHUB_ORGS upstream — a trailing space on INSTANCE_ID
// would otherwise silently key into the DDB lock and a replica
// mismatch would be hard to spot.
//
// Shape validation for env overrides lives in `invalidHotStandbyValues`
// (boot-requirements.js): this helper trusts env values verbatim,
// the boot-time validator catches malformed IPv4 / template-literal
// pastes via a single source of truth.
//
// `addr.family === 'IPv4'` matches Node 22's `os.networkInterfaces()`
// string-form contract. Node 18.0.0 briefly returned numeric `4`/`6`
// before that was reverted; we accept both shapes so a future Node
// major regressing back to numeric doesn't return null on every
// Fargate boot (which would surface as a misleading "no IPv4 found"
// diagnostic at 3am).
function deriveInstanceId() {
  // `|| null` normalizes the rare empty-hostname case (chroot or
  // misconfigured init namespace) for symmetry with deriveInstanceIp.
  // missingHotStandbyKeys catches both null and '' via its falsy
  // check, so behavior is unchanged — the symmetry is for callers
  // reading config.INSTANCE_ID and reasoning about its possible shapes.
  return process.env.INSTANCE_ID?.trim() || os.hostname() || null;
}

function isIPv4(addr) {
  // `!addr.internal` rejects loopback (127.0.0.0/8) but NOT link-local
  // (169.254.0.0/16) — node's `internal` flag only flips for loopback
  // interfaces. Under Fargate platform 1.4+, eth0 has 169.254.172.2
  // (the task metadata endpoint) bound alongside the real ENI IP, and
  // a naive eth0-first walk picks the link-local entry. Writing that
  // to the peer-heartbeat row breaks push-handoff — the standby
  // POSTs to a link-local address that isn't routable peer-to-peer.
  // Filter explicitly so the real awsvpc-assigned private IP wins.
  return (addr.family === 'IPv4' || addr.family === 4)
    && !addr.internal
    && !addr.address.startsWith('169.254.');
}

function deriveInstanceIp() {
  const envOverride = process.env.INSTANCE_IP?.trim();
  if (envOverride) return envOverride;
  const ifaces = os.networkInterfaces();
  // First pass: eth0 (the awsvpc ENI's stable name) gets priority —
  // under Fargate this returns the real ENI private IP once isIPv4
  // has filtered out the link-local task-metadata address.
  for (const addr of ifaces.eth0 || []) {
    if (isIPv4(addr)) return addr.address;
  }
  // Fallback for local/dev (macOS `en0`, stripped containers without
  // `eth0`, etc.). Iteration order is whatever `os.networkInterfaces()`
  // returns — best-effort only. The eth0 entries get re-walked here
  // but yield nothing (the first pass already rejected every candidate),
  // so a separate skip isn't worth the noise.
  for (const addrs of Object.values(ifaces)) {
    for (const addr of addrs || []) {
      if (isIPv4(addr)) return addr.address;
    }
  }
  return null;
}

// Safe int parser: handles NaN and falsy-zero correctly.
//
// Options:
//   minPositive: reject values <= 0 (common case for cooldowns + caps —
//     an env of "0" would otherwise silently disable a cooldown).
//   strictInteger: reject non-integer or trailing-garbage values like
//     "100abc". Use Number() + Number.isInteger() instead of parseInt's
//     lenient parse. Pair with minPositive for "must be a positive
//     integer" semantics. Required when the value's range is bounded
//     and a typo would silently truncate to a different valid value.
//   min, max: inclusive range. Out-of-range values warn + fall back
//     to the default (NOT clamp — clamping would silently mask a typo
//     past the boundary). When both are set, the warn quotes the
//     range so an operator can fix the env without diffing the source.
//
// Returns defaultVal on any rejection, with a console.warn for every
// rejected path (visible at boot regardless of LOG_LEVEL or logger
// transport state — logger isn't loaded this early in config import).
function intEnv(key, defaultVal, opts = {}) {
  const { minPositive = false, strictInteger = false, min, max } = opts;
  const raw = process.env[key];
  if (raw === undefined || raw === '') return defaultVal;
  let v;
  if (strictInteger) {
    v = Number(raw);
    if (!Number.isInteger(v)) {
      console.warn(`[config] ${key}=${JSON.stringify(raw)} rejected (must be an integer); using default ${defaultVal}`);
      return defaultVal;
    }
  } else {
    v = parseInt(raw, 10);
    if (isNaN(v)) return defaultVal;
  }
  if (minPositive && v <= 0) {
    console.warn(`[config] ${key}=${v} rejected (must be > 0); using default ${defaultVal}`);
    return defaultVal;
  }
  if ((min !== undefined && v < min) || (max !== undefined && v > max)) {
    const rangeLabel = min !== undefined && max !== undefined
      ? `[${min}, ${max}]`
      : min !== undefined ? `>= ${min}` : `<= ${max}`;
    console.warn(`[config] ${key}=${v} out of range ${rangeLabel}; using default ${defaultVal}`);
    return defaultVal;
  }
  return v;
}

// Normalize GUILD_ID: accept only a valid Discord snowflake (17–20 digits).
// Any other value — including an unset env, the literal string "PLACEHOLDER"
// that SSM-seeded params carry, or a whitespace-only value — normalizes to
// null so every downstream truthy check (`if (config.GUILD_ID)`) correctly
// treats the bot as multi-tenant. Prevents a malformed SSM value from
// silently registering commands to a nonexistent guild.
const rawGuildId = process.env.GUILD_ID;
let normalizedGuildId = null;
if (rawGuildId) {
  const trimmed = rawGuildId.trim();
  if (/^\d{17,20}$/.test(trimmed)) {
    normalizedGuildId = trimmed;
  } else {
    // logger isn't loaded this early in config import — use console directly.
    console.warn(`[config] GUILD_ID=${JSON.stringify(rawGuildId)} is not a valid Discord snowflake (17-20 digits); starting in multi-tenant mode. To run in single-guild mode, set GUILD_ID to a real guild ID.`);
  }
}

// Multi-tenant mode: derived once here, consumed everywhere else. When true,
// the bot treats itself as a public multi-server app (commands global,
// OpenNHP features dormant, /auth + /webhook routes not mounted). When
// false, the bot runs in single-guild mode targeting normalizedGuildId.
// Keeping this derived in config.js (single source of truth) means every
// downstream check is `if (config.isMultiTenant)` — semantic name at
// every callsite.
//
// Together with ENABLE_OPENNHP_FEATURES (below), this selects one of
// three supported modes:
//
//   (!isMultiTenant, ENABLE_OPENNHP_FEATURES=true)
//       Single-guild OpenNHP community server. Full command set
//       registers scoped to the guild; ensureRolesAndChannels creates
//       contributor roles + #contribute / #github-feed; /auth and
//       /webhook routes mount; weekly digest runs. Requires
//       ManageRoles + ManageChannels perms in the guild.
//
//   (!isMultiTenant, ENABLE_OPENNHP_FEATURES=false)
//       Single-guild plain qURL sharing tool. Only /qurl registers
//       (scoped to the guild for instant propagation); no role or
//       channel creation; /auth and /webhook routes dormant. Needs
//       only the 4 runtime perms (ViewChannel, SendMessages,
//       EmbedLinks, UseApplicationCommands).
//
//   (isMultiTenant, ENABLE_OPENNHP_FEATURES=false)
//       Multi-tenant plain qURL sharing tool. Commands register
//       globally (up to 1 hr Discord cache propagation); per-guild
//       config via /qurl setup; every OpenNHP code path is gated off.
//       Default for the public-bot install.
//
//   (isMultiTenant, ENABLE_OPENNHP_FEATURES=true)
//       Not a supported combination. OpenNHP behaviors need a
//       specific guild cache to target; the ready handler skips its
//       single-guild setup when isMultiTenant, so the flag has no
//       effect in multi-tenant mode.
const isMultiTenant = !normalizedGuildId;

// OpenNHP community features (role auto-creation + auto-assign, channel
// auto-creation, welcome DM, badge announcements). Default OFF so a
// vanilla install of the bot into any guild — single-tenant or
// multi-tenant — only exercises the 4 runtime permissions it was
// invited with (View Channels, Send Messages, Embed Links, Use
// Application Commands). Only the OpenNHP community server sets this
// true; everywhere else the bot is a plain qURL sharing tool with no
// elevated expectations. Must be the literal string "true" — any other
// value (including unset, empty, "TRUE", "1", "yes") keeps it disabled,
// so an env-var typo can't silently re-enable role/channel creation
// attempts in a guild that hasn't granted those permissions.
const enableOpenNHPFeatures = process.env.ENABLE_OPENNHP_FEATURES === 'true';

// Unsupported combination — catch at config load so an operator doesn't
// spend time wondering why "ENABLE_OPENNHP_FEATURES=true" had no effect
// in multi-tenant mode. logger isn't available this early (config is
// a require() dependency of logger's callers), so use console.warn.
if (!normalizedGuildId && enableOpenNHPFeatures) {
  console.warn('[config] ENABLE_OPENNHP_FEATURES=true is ignored when GUILD_ID is unset (multi-tenant mode): OpenNHP behaviors target a cached single guild that multi-tenant mode never populates. Either set GUILD_ID to the OpenNHP guild snowflake, or clear ENABLE_OPENNHP_FEATURES to silence this warning.');
}

// Single source of truth for "OpenNHP is active". Consumed by
// commands.js (command-set filter), server.js (route-mount gate),
// boot-requirements.js (which env-vars are required), and discord.js
// (every OpenNHP short-circuit). Deriving in one place means a future
// change to the predicate — e.g. adding a third flag, or broadening
// what "multi-tenant" means — only touches this file.
const isOpenNHPActive = !isMultiTenant && enableOpenNHPFeatures;

// AUTH0_DOMAIN must be a bare hostname — the codebase composes it as
// `https://${AUTH0_DOMAIN}/...` for the JWKS endpoint, /authorize, and
// /oauth/token. Round-9 #2: typo'd values like 'https://layerv.auth0.com'
// or a placeholder string would silently flip isQurlOAuthConfigured on,
// then break under load with a confusing URL parse error. Reject at
// config-load time so the bot fails to enable OAuth (legacy fallback
// path activates) instead of crashing mid-flow.
function isValidAuth0DomainShape(d) {
  if (typeof d !== 'string' || !d) return false;
  // DNS-shape cap: RFC 1035 maxes at 253 chars total. Auth0 domains in
  // practice are short (e.g., layerv.us.auth0.com), but an explicit
  // cap is clearer than relying on a regex bound.
  if (d.length > 253) return false;
  // Reject any scheme prefix or path — domain only.
  if (/[:/?#]/.test(d)) return false;
  // Bare hostname, letters/digits/dots/dashes, must contain at least
  // one dot (rejects "placeholder" / "localhost" while permitting
  // custom Auth0 domains like auth.layerv.ai). Case-insensitive on
  // input — Auth0 domains are canonically lowercase but we don't
  // reject mixed case.
  return /^[a-z0-9][a-z0-9.-]+\.[a-z]{2,}$/i.test(d);
}

// True when all four Auth0 env vars are present AND AUTH0_DOMAIN is a
// well-shaped hostname — `/qurl setup` then uses the OAuth-redirect
// flow. False = degrade to the legacy modal-paste flow (kept until
// Justin registers the Auth0 app + sets prod SSM secrets). Single
// derivation point so commands.js + routes/qurl-oauth.js + server.js
// agree on what "configured" means.
const isQurlOAuthConfigured = Boolean(
  isValidAuth0DomainShape(process.env.AUTH0_DOMAIN)
  && process.env.AUTH0_CLIENT_ID
  && process.env.AUTH0_CLIENT_SECRET
  && process.env.AUTH0_AUDIENCE,
);

// True when the Stage-2 "Add to Discord, select server" install flow can
// run end-to-end: needs the bot's Discord OAuth2 client secret (separate
// from the bot token used for normal operations) on top of qURL OAuth.
// The /oauth/discord/callback route gates on this; it returns 503 with a
// "not configured" page when false, so the install link still completes
// (bot lands in the server) but the chained Auth0 leg won't run until
// Justin sets DISCORD_CLIENT_SECRET in SSM.
const isDiscordInstallConfigured = Boolean(
  isQurlOAuthConfigured && process.env.DISCORD_CLIENT_SECRET,
);

// Shard identifier for the shard-aware composite flow_id
// (`<shard_id>#<guild_id>#<channel_id>#<user_id>`, see src/flow-id.js).
// Single-shard today, so the default is `'0:1'` (shard 0 of 1) per the
// zero-downtime design doc. When sharding lands, the runtime will set
// SHARD_ID per process to `'k:n'` (e.g. `'3:8'` for shard 3 of 8); the
// schema is already shard-aware so no migration is needed. Single
// export point so a future grep-and-replace doesn't miss a callsite.
const SHARD_ID = process.env.SHARD_ID || '0:1';

// One-shot getter for the GATEWAY_HANDOFF_HMAC secret. The raw value
// is captured into a module-private binding at require time, then
// surfaced exactly once via takeGatewayHandoffHmac() which nulls the
// binding before returning.
//
// Defense in depth against heap-dump key exposure: even if a future
// caller adds telemetry that captures the config object wholesale,
// or a debugger attaches mid-process, the secret string is no longer
// reachable through any module-level reference once startHotStandby
// has consumed it. The live HMAC instance created by createGatewayHmac
// still holds the parsed bytes (necessary for sign/verify) — that's
// the only retained reference.
//
// Why a private binding rather than a config-object property: the
// `config` object is exported and any module can capture it at
// require time. A `delete config.GATEWAY_HANDOFF_HMAC` after the
// secret is consumed does NOT remove already-captured references.
// A private binding inside this module is unreachable to anything
// except this getter.
let _gatewayHandoffHmacRaw = process.env.GATEWAY_HANDOFF_HMAC;
function takeGatewayHandoffHmac() {
  const value = _gatewayHandoffHmacRaw;
  _gatewayHandoffHmacRaw = undefined;
  return value;
}

// /qurl send + /qurl detect cooldowns. Resolved here (not inline in the
// export literal) so QURL_DETECT_COOLDOWN_MS can DEFAULT to the resolved
// send value — i.e. unset detect knob == current behavior (no decoupling
// surprise), but an operator can tune the deanonymization-oracle throttle
// independently of send cadence. See setDetectCooldown in commands.js.
const sendCooldownMs = intEnv('QURL_SEND_COOLDOWN_MS', 30000, { minPositive: true });
const detectCooldownMs = intEnv('QURL_DETECT_COOLDOWN_MS', sendCooldownMs, { minPositive: true });

// Configuration from environment variables
module.exports = {
  // Discord
  DISCORD_TOKEN: process.env.DISCORD_TOKEN,
  DISCORD_CLIENT_ID: process.env.DISCORD_CLIENT_ID,
  // Required for the Stage-2 "Add to Discord, select server" install
  // callback (src/routes/discord-install.js). Not used by normal bot
  // operations — only by the OAuth2 token exchange when an admin
  // installs the bot via the install link. Optional: omit and the
  // /oauth/discord/callback route will return 503 with a documented
  // "not configured" page until Justin sets the secret.
  DISCORD_CLIENT_SECRET: process.env.DISCORD_CLIENT_SECRET,
  GUILD_ID: normalizedGuildId,
  isMultiTenant,
  ENABLE_OPENNHP_FEATURES: enableOpenNHPFeatures,
  isOpenNHPActive,
  isQurlOAuthConfigured,
  isDiscordInstallConfigured,

  // Role names for progression
  CONTRIBUTOR_ROLE_NAME: process.env.CONTRIBUTOR_ROLE_NAME || 'Contributor',
  ACTIVE_CONTRIBUTOR_ROLE_NAME: process.env.ACTIVE_CONTRIBUTOR_ROLE_NAME || 'Active Contributor',
  CORE_CONTRIBUTOR_ROLE_NAME: process.env.CORE_CONTRIBUTOR_ROLE_NAME || 'Core Contributor',
  CHAMPION_ROLE_NAME: process.env.CHAMPION_ROLE_NAME || 'Champion',

  // Role thresholds (lowered for realistic contribution cadence)
  ACTIVE_CONTRIBUTOR_THRESHOLD: intEnv('ACTIVE_CONTRIBUTOR_THRESHOLD', 3),
  CORE_CONTRIBUTOR_THRESHOLD: intEnv('CORE_CONTRIBUTOR_THRESHOLD', 10),
  CHAMPION_THRESHOLD: intEnv('CHAMPION_THRESHOLD', 25),

  // Channel names
  GENERAL_CHANNEL_NAME: process.env.GENERAL_CHANNEL_NAME || 'general',
  NOTIFICATION_CHANNEL_NAME: process.env.NOTIFICATION_CHANNEL_NAME || 'general',
  ANNOUNCEMENTS_CHANNEL_NAME: process.env.ANNOUNCEMENTS_CHANNEL_NAME || 'announcements',
  CONTRIBUTE_CHANNEL_NAME: process.env.CONTRIBUTE_CHANNEL_NAME || 'contribute',
  GITHUB_FEED_CHANNEL_NAME: process.env.GITHUB_FEED_CHANNEL_NAME || 'github-feed',

  // GitHub OAuth
  GITHUB_CLIENT_ID: process.env.GITHUB_CLIENT_ID,
  GITHUB_CLIENT_SECRET: process.env.GITHUB_CLIENT_SECRET,
  GITHUB_WEBHOOK_SECRET: process.env.GITHUB_WEBHOOK_SECRET,

  // qURL webhook receiver HMAC. Written to SSM by the webhook-registrar
  // Lambda (apps/discord/lambda/webhook-registrar/) on each deploy
  // invocation, then injected into the bot's task env. The bot reads
  // it here and never modifies it — Lambda is the sole writer.
  QURL_WEBHOOK_SECRET: process.env.QURL_WEBHOOK_SECRET,

  // qURL OAuth (Auth0) — for /qurl setup admin consent flow.
  // When unset, /qurl setup falls back to the legacy modal-paste path so the
  // bot stays usable until Justin registers the Auth0 application + drops
  // these into prod SSM. See project_qurl_bot_onboarding_model.md memory for
  // the OAuth-app shape (Regular Web Application, callback URL = BASE_URL +
  // /oauth/qurl/callback, scopes qurl:write + qurl:read).
  AUTH0_DOMAIN: process.env.AUTH0_DOMAIN,
  AUTH0_CLIENT_ID: process.env.AUTH0_CLIENT_ID,
  AUTH0_CLIENT_SECRET: process.env.AUTH0_CLIENT_SECRET,
  AUTH0_AUDIENCE: process.env.AUTH0_AUDIENCE,

  // Allowed GitHub organizations (comma-separated)
  ALLOWED_GITHUB_ORGS: (process.env.ALLOWED_GITHUB_ORGS || 'OpenNHP').split(',').map(s => s.trim().toLowerCase()).filter(Boolean),

  // Server
  PORT: intEnv('PORT', 3000),
  BASE_URL: process.env.BASE_URL || 'http://localhost:3000',

  // Rate limiting
  RATE_LIMIT_WINDOW_MS: intEnv('RATE_LIMIT_WINDOW_MS', 60000), // 1 minute
  RATE_LIMIT_MAX_REQUESTS: intEnv('RATE_LIMIT_MAX_REQUESTS', 30),

  // OAuth link expiry (in minutes)
  // Shortened from 30 to 10 minutes: the OAuth state is not bound to the
  // initiating browser session, so a shorter expiry narrows the window for
  // a leaked/shoulder-surfed state token to be replayed by an attacker.
  PENDING_LINK_EXPIRY_MINUTES: intEnv('PENDING_LINK_EXPIRY_MINUTES', 10),

  // Admin Discord user IDs (comma-separated) — can use /forcelink, /bulklink,
  // /unlinked. Each entry is validated to look like a Discord snowflake
  // (17–20 digits) so a typo like "1234, 5678 " (stray space or non-numeric)
  // can't silently create a dead admin ID that never matches an interaction.
  ADMIN_USER_IDS: (process.env.ADMIN_USER_IDS || '')
    .split(',')
    .map(s => s.trim())
    .filter(s => {
      if (!s) return false;
      if (!/^\d{17,20}$/.test(s)) {
        // Using console.warn directly — logger isn't loaded this early in config import.
        console.warn(`[config] Dropping malformed ADMIN_USER_IDS entry (not a Discord snowflake): ${JSON.stringify(s)}`);
        return false;
      }
      return true;
    }),

  // Milestones to announce (star counts) - extended for mature repos
  STAR_MILESTONES: [10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 15000, 20000, 25000, 50000, 75000, 100000],

  // Weekly digest schedule (cron format) - default Sunday 9am UTC
  WEEKLY_DIGEST_CRON: process.env.WEEKLY_DIGEST_CRON || '0 9 * * 0',

  // Welcome message (for new member DM)
  WELCOME_DM_ENABLED: process.env.WELCOME_DM_ENABLED !== 'false',

  // qURL. In production we fall back to the real endpoints; in dev we fall
  // back to localhost so a missing .env file doesn't silently hit prod APIs.
  // index.js enforces that both env vars are set when NODE_ENV=production.
  QURL_API_KEY: process.env.QURL_API_KEY,
  QURL_ENDPOINT: process.env.QURL_ENDPOINT
    || (process.env.NODE_ENV === 'production' ? 'https://api.layerv.ai' : 'http://localhost:8080'),

  // Multi-use qURL access token (`at_...`) the bot resolves to reach the
  // watermark-detect endpoint over the qURL reverse-tunnel (PR-3, #1101).
  // connector.js's resolveDetectTarget() calls QurlClient.resolve({
  // access_token: DETECT_ACCESS_TOKEN }) — which issues an NHP knock for the
  // bot's current IP — immediately before each /api/detect POST. Secret-
  // shaped (read verbatim from env like QURL_API_KEY / QURL_WEBHOOK_SECRET);
  // no default. When unset, /qurl detect surfaces a clear configured-error
  // (resolveDetectTarget throws) rather than silently failing. SSM-seeded at
  // detect activation, the same gated step that flips DETECT_COMMAND_ENABLED.
  DETECT_ACCESS_TOKEN: process.env.DETECT_ACCESS_TOKEN,

  // qurl-s3-connector
  CONNECTOR_URL: process.env.CONNECTOR_URL
    || (process.env.NODE_ENV === 'production' ? 'https://get.qurl.link:9808' : 'http://localhost:9808'),

  // Google Maps (location autocomplete)
  GOOGLE_MAPS_API_KEY: process.env.GOOGLE_MAPS_API_KEY,

  // /qurl map feature toggle. Default OFF — the bot ships without
  // /qurl map registered as a slash subcommand. Operator opts in per
  // deploy by setting MAP_COMMAND_ENABLED=true on the task definition
  // (terraform var `map_command_enabled` plumbs this through). Must
  // be the literal string "true"; any other value (unset, empty,
  // "TRUE", "1", "yes") keeps the feature off, so an env-var typo
  // can't silently re-enable a command that requires a working
  // GOOGLE_MAPS_API_KEY in SSM. See the slash-command builder IIFE
  // in commands.js for the full set of MAP_COMMAND_ENABLED gates.
  //
  // Snapshot semantics: this value is read ONCE at module load and
  // baked into the slash registration (commands.js IIFE) +
  // SETUP_SUCCESS_MSG. Flipping MAP_COMMAND_ENABLED at runtime is
  // a no-op until the task restarts; the deploy model handles this
  // (ECS rolls fresh tasks on every task-def revision).
  MAP_COMMAND_ENABLED: process.env.MAP_COMMAND_ENABLED === 'true',

  // DETECT_COMMAND_ENABLED gates /qurl detect (#1101) exactly as
  // MAP_COMMAND_ENABLED gates /qurl map: default OFF, same snapshot/restart
  // semantics. The connector /api/detect backend 503/404s until the watermark
  // stack is ACTIVATED (a separate gated step AFTER the connector deploys), so
  // detect stays DARK until an operator flips this at activation — no
  // visible-but-failing command in the interim.
  DETECT_COMMAND_ENABLED: process.env.DETECT_COMMAND_ENABLED === 'true',

  // qURL send limits (/qurl send + /qurl map) — both must be > 0. A
  // cooldown of 0 would silently disable the rate limit; a recipients
  // cap of 0 would reject every send.
  //
  // 20,000 default chosen to accommodate the voice-everyone path against
  // stage channels (Discord's largest gathering surface). Heads-up for
  // operators reading the release notes: the same cap bounds EVERY
  // recipient-resolution path — `<@user>` list expansion, `<@&role>`
  // expansion, `<#voice>` expansion, the UserSelectMenu picker, AND
  // `@everyone` (gated on MENTION_EVERYONE). The 50 → 20,000 jump
  // therefore re-shapes the `@everyone` blast radius by 400× for any
  // member granted MENTION_EVERYONE in a guild that takes the new
  // default. Guilds that intentionally constrained the prior 50-recipient
  // ceiling for non-voice surfaces should set `QURL_SEND_MAX_RECIPIENTS`
  // to the desired bound in their env; the voice-everyone path will then
  // partial-resolve up to that bound rather than refusing.
  // Operational implications a max-size send carries:
  //   - up to ceil(20000/TOKENS_PER_RESOURCE) = 2000 re-uploads to
  //     qurl-service per send (`mintLinksInBatches`).
  //   - DM delivery is bounded by Discord's per-bot DM rate limit
  //     (~5/sec); a 20k send takes >1 hour to finish DM fan-out, and
  //     `monitorLinkStatus`'s interval-based progress tracking must
  //     survive that duration. A regression test pinning that
  //     lifetime is tracked in issue #331 — open against any change
  //     that touches `monitorLinkStatus`'s interval/cleanup logic
  //     until it lands.
  // Per-guild operators can dial this down via the env override if
  // their qurl-service plan or DM-throughput posture demands it.
  QURL_SEND_MAX_RECIPIENTS: intEnv('QURL_SEND_MAX_RECIPIENTS', 20000, { minPositive: true }),
  QURL_SEND_COOLDOWN_MS: sendCooldownMs,
  // Throttle for /qurl detect (the deanonymization oracle). Defaults to the
  // send cooldown so behavior is unchanged unless an operator sets
  // QURL_DETECT_COOLDOWN_MS explicitly — decoupled so a future send-cadence
  // change can't silently re-tune the oracle. See setDetectCooldown.
  QURL_DETECT_COOLDOWN_MS: detectCooldownMs,

  SHARD_ID,

  // Event-shipper (zero-downtime design, Pillar 1). When true, the
  // gateway tier forwards every Discord dispatch to SQS instead of
  // running handlers in-process, and the worker tier (PROCESS_ROLE=http
  // or combined) polls SQS and routes events through the same
  // handleCommand / handleFlowInteraction path. When false, the bot
  // runs the legacy in-process shape — gateway role both receives WS
  // dispatches AND runs handlers; worker role is dormant on the queue.
  //
  // Must be the literal string "true" — same shape as
  // ENABLE_OPENNHP_FEATURES so an env-var typo (TRUE/1/yes/etc.) can't
  // silently flip a production deploy into the new dispatch path.
  //
  // Rollback cliff: this flag is valid only through PR 10 (gateway
  // strip-down). The follow-up that removes the in-process fallback
  // also removes this flag; after that, rollback is a `git revert` +
  // emergency redeploy. See `apps/discord/docs/zero-downtime-design.md`
  // → "Rollback cliff: ENABLE_EVENT_SHIPPER".
  ENABLE_EVENT_SHIPPER: process.env.ENABLE_EVENT_SHIPPER === 'true',

  // Gateway RESUME (zero-downtime design, Pillar 2). When true and
  // PROCESS_ROLE=gateway, the gateway tier replaces the discord.js
  // `Client` with `@discordjs/ws` `WebSocketManager` and persists
  // session state (session_id / resume_url / sequence) to the
  // `${DDB_TABLE_PREFIX}gateway-session` DDB table. On a process
  // restart the new gateway boots, reads the persisted session, and
  // Discord's `RESUME` (op 6) replays buffered events from the last
  // sequence — eliminating the ~10 s IDENTIFY cold-start that the
  // legacy single-process deploy carried on every restart.
  //
  // Requires `ENABLE_EVENT_SHIPPER=true` because the @discordjs/ws
  // shim doesn't run the in-process dispatcher; it forwards every
  // frame to SQS (same path Pillar 1 set up). `ENABLE_GATEWAY_RESUME=true`
  // with the shipper off is rejected at boot — the shim would have
  // nowhere to send dispatches. Combined mode is also rejected
  // because the legacy Client owns the WS in that shape.
  //
  // No-op when role is http/worker (those tiers don't open a WS).
  // Default off so a deploy without the flag set behaves identically
  // to the pre-Pillar-2 codebase — matches the ENABLE_EVENT_SHIPPER
  // rollout pattern.
  //
  // Literal-'true' string check (not truthy parsing) for the same
  // reason as ENABLE_EVENT_SHIPPER: a typo (TRUE/1/yes) must not
  // silently flip the gateway path.
  ENABLE_GATEWAY_RESUME: process.env.ENABLE_GATEWAY_RESUME === 'true',

  // Gateway hot-standby (zero-downtime design, Pillar 3). When true
  // and PROCESS_ROLE=gateway, the gateway tier runs two replicas
  // (active + standby) that elect a leader via the DDB lock table,
  // exchange heartbeats via the peer-heartbeat table, and push
  // ownership of the live WebSocket via an HMAC-authenticated control
  // channel on SIGTERM. Eliminates the IDENTIFY/RESUME gap on every
  // deploy — the incoming task `manager.connect()`s synchronously
  // inside the outgoing task's pushHandoff so the next dispatch
  // lands without a cold start. See `apps/discord/docs/zero-downtime-design.md`
  // → "Pillar 3: hot-standby + push handoff".
  //
  // Requires ENABLE_GATEWAY_RESUME=true AND PROCESS_ROLE=gateway —
  // the hot-standby control plane drives the gateway-ws-shim's
  // manager handle, so neither the legacy single-process shape nor
  // the http/worker tiers have a manager to hand off. Combined +
  // hot-standby is rejected for the same reason as combined + RESUME
  // (the legacy Client owns the WS). Boot-requirements rejects every
  // unsupported combo at startup.
  //
  // Default off so a deploy without the flag set behaves identically
  // to the pre-Pillar-3 codebase — single replica, single-process
  // gateway, no control channel listening. Literal-'true' check for
  // the same typo-rejection reason as ENABLE_EVENT_SHIPPER.
  ENABLE_GATEWAY_HOT_STANDBY: process.env.ENABLE_GATEWAY_HOT_STANDBY === 'true',

  // Per-replica identity. The leader coordinator writes this into the
  // DDB lock row as the holder; the control-channel server logs it on
  // every handoff; the peer-heartbeat row keys on it. Derived from
  // `os.hostname()` (Fargate sets this to a short alphanumeric per
  // task — distinct across replicas in the same service); env override
  // wins. Populated unconditionally at module-load (even when
  // hot-standby is off) — safe because every consumer in index.js
  // lives inside `startHotStandby()`, which only runs when the flag
  // is on. A non-null `config.INSTANCE_ID` is NOT a hot-standby
  // indicator on its own. LOAD-BEARING INVARIANT (full rationale
  // above `deriveInstanceId`): two replicas in the same ECS service
  // MUST see different values, or the DDB lock short-circuits.
  INSTANCE_ID: deriveInstanceId(),

  // The IPv4 address peers reach this replica on (`http://<ip>:<port>/control/yours`).
  // ECS awsvpc mode assigns a routable VPC IP per task on the task's
  // eth0 ENI; `os.networkInterfaces()` exposes it sync at boot. Used
  // by the peer-heartbeat row's `address_v4` field so the active
  // replica's pushHandoff client knows where to connect. Env override
  // wins; if no non-internal IPv4 exists, this is null and
  // invalidHotStandbyValues rejects boot.
  INSTANCE_IP: deriveInstanceIp(),

  // Bind/listen ports for the in-VPC control channel that receives
  // pushHandoff envelopes from the outgoing leader. The bind address
  // defaults to 0.0.0.0 (awsvpc-routable) rather than 127.0.0.1
  // because the peer reaches it via the task's VPC IP. The security
  // posture is HMAC-on-every-request (gateway-hmac) plus a security-
  // group rule that restricts the listening port to peer tasks in the
  // same service — see qurl-integrations-infra `qurl-bot-discord/terraform/control-channel.tf`.
  GATEWAY_CONTROL_PORT: intEnv('GATEWAY_CONTROL_PORT', 7800, {
    strictInteger: true,
    min: 1024,
    max: 65535,
  }),
  GATEWAY_CONTROL_BIND_ADDR: process.env.GATEWAY_CONTROL_BIND_ADDR || '0.0.0.0',

  // JSON-shaped HMAC secret for the control channel. NOT a direct
  // property — read once via the `takeGatewayHandoffHmac` helper
  // exported below. The boot-presence check
  // (`missingHotStandbyKeys`) reads from `_gatewayHandoffHmacRaw` via
  // a separate `hasGatewayHandoffHmac` flag exposed below so the
  // gate can fire before takeGatewayHandoffHmac is called.
  //
  // Surfaced this way (not as a property on the exported config
  // object) so a heap dump after secret consumption can't recover
  // the raw value through `config.GATEWAY_HANDOFF_HMAC`. See the
  // takeGatewayHandoffHmac definition near the top of this file
  // for the security rationale.
  //
  // Operator note: the boot-presence check below uses
  // `process.env.GATEWAY_HANDOFF_HMAC` directly (via the flag), so
  // an unset env var still surfaces the same "required key missing"
  // error message as before — no observable behavior change.
  hasGatewayHandoffHmac: Boolean(process.env.GATEWAY_HANDOFF_HMAC),

  // Persistence backend selector. Lifted from raw env into config
  // so the boot-guard (`unsupportedRoleResumeCombo`) and the
  // gateway-shim wiring both read through the same parsed shape.
  // Unset / empty / whitespace-only falls back to 'ddb', matching
  // src/store/index.js's selection precedence.
  //
  // `src/store/index.js` is the source of truth for STORE_TYPE
  // validation — it throws on unknown values at module load (which
  // runs before any consumer reads this config field). This field
  // is retained solely so the downstream guard `unsupportedRole-
  // ResumeCombo` and other config-level checks can read a normalized
  // string instead of re-parsing the env. Don't add validation
  // logic here; surface it in store/index.js where it belongs.
  STORE_TYPE: (process.env.STORE_TYPE ?? '').trim() || 'ddb',

  // DDB table-name prefix shared by every per-table consumer
  // (ddb-store.js + gateway-session-store.js construction in
  // index.js). Trimmed here so a whitespace-padded env value
  // doesn't compute a different table name at one call site than
  // the others — ddb-store.js's `.trim()` was the original
  // normalization point.
  DDB_TABLE_PREFIX: (process.env.DDB_TABLE_PREFIX ?? '').trim(),

  // SQS Standard queue the gateway publishes to and the worker
  // consumes from (provisioned by qurl-integrations-infra PR B).
  // Required when ENABLE_EVENT_SHIPPER=true; validated at boot in
  // index.js so a misconfigured deploy fails closed instead of
  // silently no-op'ing the consumer or dropping every dispatch on
  // the producer side.
  QURL_BOT_EVENTS_QUEUE_URL: process.env.QURL_BOT_EVENTS_QUEUE_URL,

  // View-update push (feat #60, sub-second view counter). When true,
  // qurl-webhook.js publishes view events to a separate SQS queue
  // after a successful recordQurlView; the HTTP tier (same process
  // that owns the webhook receiver and the live monitorLinkStatus
  // instances) drains the queue and dispatches into the process-
  // local view-update-registry. The polling fallback in
  // commands.js stays as the correctness primitive — this flag gates
  // ONLY the latency-optimization path. Default false so a deploy
  // without the flag behaves identically to the legacy polling shape.
  // Must be the literal string "true" (same parsing posture as
  // ENABLE_EVENT_SHIPPER) so an env-var typo can't flip prod.
  ENABLE_VIEW_UPDATE_PUSH: process.env.ENABLE_VIEW_UPDATE_PUSH === 'true',

  // SQS Standard queue for view updates (separate from
  // QURL_BOT_EVENTS_QUEUE_URL, which carries Discord interactions).
  // Required when ENABLE_VIEW_UPDATE_PUSH=true.
  QURL_BOT_VIEW_UPDATES_QUEUE_URL: process.env.QURL_BOT_VIEW_UPDATES_QUEUE_URL,

  // Backpressure cap for the event consumer's in-flight handler
  // tracker (see src/event-consumer.js module header). Read here
  // instead of in event-consumer.js so the value goes through the
  // single intEnv validation path — strictInteger rejects trailing
  // garbage like "100abc" the same way as the prior IIFE.
  QURL_BOT_MAX_INFLIGHT_HANDLERS: intEnv('QURL_BOT_MAX_INFLIGHT_HANDLERS', 100, {
    minPositive: true,
    strictInteger: true,
  }),

  // Graceful-shutdown drain deadline (ms). Consumed by both
  // event-consumer.js and event-publisher.js — see each module's
  // stop() comment for the contract. Range bound is the upper limit
  // of gracefulShutdown's 10s budget minus headroom for db.close()
  // + Discord teardown. Values outside [100, 8000] fall back to the
  // default with a warn — clamping would silently mask a typo past
  // the boundary.
  QURL_BOT_DRAIN_DEADLINE_MS: intEnv('QURL_BOT_DRAIN_DEADLINE_MS', 3000, {
    strictInteger: true,
    min: 100,
    max: 8000,
  }),

  // One-shot getter for the GATEWAY_HANDOFF_HMAC raw value (see the
  // takeGatewayHandoffHmac definition near the top of this file).
  // Exposed as a function, NOT a property — second call returns
  // undefined.
  takeGatewayHandoffHmac,
};
