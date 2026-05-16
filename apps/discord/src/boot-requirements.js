// Pure helpers computing which env-vars the bot refuses to boot without.
// Extracted from index.js so they can be unit-tested without importing
// the full bot (which would side-effect client.login()). The lists are
// the highest-risk branch in the module: a regression here could either
// boot in prod with missing secrets OR die on a spurious false-positive.

// Required at boot in EVERY environment. Gated on `isOpenNHPActive`,
// NOT `isMultiTenant`: the GITHUB_* vars only matter when /auth +
// /webhook routes are actually mounted. A single-guild-plain deployment
// (GUILD_ID set but ENABLE_OPENNHP_FEATURES off) never mounts those
// routes, so demanding dummy values just to pass the boot check would
// be a papercut for every customer server.
//
// Explicitly NOT on this list even in OpenNHP mode:
//   - GUILD_ID: if isOpenNHPActive === true then !isMultiTenant, which
//     means the snowflake validator in config.js already accepted a
//     17-20 digit value. Re-checking truthiness here would never catch
//     a missing GUILD_ID — the upstream check is the authority.
//   - BASE_URL: config.js supplies an unconditional "http://localhost:3000"
//     default, so `cfg.BASE_URL` is always truthy. The real enforcement
//     is the https-startswith check in index.js, which runs regardless
//     of this required-list membership.
// Listing either would be decorative — the downstream checks are the
// authority. Keeping this list to the keys whose absence is actually a
// boot blocker.
function bootRequired(isOpenNHPActive) {
  if (!isOpenNHPActive) return ['DISCORD_TOKEN'];
  return ['DISCORD_TOKEN', 'GITHUB_CLIENT_ID', 'GITHUB_CLIENT_SECRET', 'GITHUB_WEBHOOK_SECRET'];
}

// Additionally required when NODE_ENV=production. QURL_API_KEY is the
// global-fallback for /qurl send + /qurl map; single-guild-plain and
// multi-tenant deployments both rely on per-guild /qurl setup, so it's
// optional outside the OpenNHP community server.
//
// KEY_ENCRYPTION_KEY appears here AND in missingKekRequiredKeys.
// The two checks overlap on prod-with-OAuth (both fail closed there);
// the load-bearing distinct cases are: this entry catches prod
// deploys WITHOUT GITHUB_CLIENT_SECRET (KEK still protects
// guild_configs.qurl_api_key + qurl_send_configs.attachment_url),
// while missingKekRequiredKeys catches the staging/preview-with-OAuth
// case the prod block alone would not cover.
function prodRequired(isOpenNHPActive) {
  if (!isOpenNHPActive) return ['METRICS_TOKEN', 'KEY_ENCRYPTION_KEY'];
  return ['METRICS_TOKEN', 'QURL_API_KEY', 'KEY_ENCRYPTION_KEY'];
}

// Compute which required keys are missing from a given config-like
// object. Separate from bootRequired so tests can build a "config" with
// specific holes and assert the exact missing list.
function missingBootKeys(cfg, isOpenNHPActive) {
  return bootRequired(isOpenNHPActive).filter(key => !cfg[key]);
}

function missingProdKeys(env, isOpenNHPActive) {
  return prodRequired(isOpenNHPActive).filter(k => !env[k]);
}

// KEY_ENCRYPTION_KEY is required independently of NODE_ENV whenever
// GITHUB_CLIENT_SECRET is set — staging/preview environments hand out
// real GitHub OAuth tokens, and crypto.encrypt's dev plaintext fallback
// must never reach the orphan-token persistence path.
function missingKekRequiredKeys(env) {
  if (!env.GITHUB_CLIENT_SECRET) return [];
  return env.KEY_ENCRYPTION_KEY ? [] : ['KEY_ENCRYPTION_KEY'];
}

// QURL_BOT_EVENTS_QUEUE_URL is the load-bearing piece of the event-
// shipper path: producer publishes to it, consumer polls from it. When
// ENABLE_EVENT_SHIPPER=true and the queue URL isn't set, the producer
// silently drops every dispatch and the consumer sits dormant — both
// failure modes are silent enough that they wouldn't surface in
// monitoring until /qurl interactions start timing out from the user's
// end. Fail-closed at boot is preferable.
//
// API asymmetry: this takes the PARSED `cfg` while the siblings
// (missingBootKeys, missingProdKeys, missingKekRequiredKeys) take
// raw `env`. The reason is that ENABLE_EVENT_SHIPPER is parsed in
// config.js (`process.env.ENABLE_EVENT_SHIPPER === 'true'` → boolean)
// and consumers should not re-implement that parsing. Reading from
// cfg keeps the literal-'true' contract in one place.
function missingEventShipperKeys(cfg) {
  if (!cfg.ENABLE_EVENT_SHIPPER) return [];
  return cfg.QURL_BOT_EVENTS_QUEUE_URL ? [] : ['QURL_BOT_EVENTS_QUEUE_URL'];
}

// PROCESS_ROLE=combined paired with ENABLE_EVENT_SHIPPER=true is
// unsupported and rejected at boot. In combined mode both `isGateway`
// and `isHttp` evaluate true, which derives `isWorker=true`, which
// would arm both the gateway-side publish hook AND the worker-side
// consumer in one process. Every interaction would land twice: once
// via the in-process gateway WS frame, once via the SQS round-trip.
// Side effects (DM fan-out, flow-state writes) double; telemetry
// reports two dispatches per real interaction. The listener gate
// alone can't close this — discord.js's InteractionCreate action
// fires synchronously on the gateway WS frame regardless of whether
// the local listener is registered, so even gating the worker-side
// dispatcher leaves the gateway publish path firing alongside the
// consumer.
//
// The supported flag-on shape is the two-process split: a separate
// PROCESS_ROLE=gateway (singleton) publishing to SQS, and one or
// more PROCESS_ROLE=http replicas consuming. Combined mode stays
// supported for sandbox / local-dev / pre-split deployments —
// just with the flag off, running the legacy in-process path.
//
// Returns the operator-facing message on rejection or null on
// success. Kept as a string-or-null rather than throwing so the
// caller in index.js logs the message + exits via the same pattern
// as missingBootKeys (one log + process.exit) rather than handling
// a thrown error specially.
function unsupportedRoleShipperCombo(role, eventShipperEnabled) {
  if (role === 'combined' && eventShipperEnabled) {
    return (
      'PROCESS_ROLE=combined with ENABLE_EVENT_SHIPPER=true is not supported ' +
      '(would dispatch every interaction twice — once via the in-process gateway ' +
      'WS frame, once via the SQS round-trip). Run two processes: ' +
      'PROCESS_ROLE=gateway (singleton, publishes) + PROCESS_ROLE=http (consumes). ' +
      'For local dev / sandbox in one process, leave ENABLE_EVENT_SHIPPER unset.'
    );
  }
  return null;
}

// Parallel to unsupportedRoleShipperCombo for ENABLE_GATEWAY_RESUME.
// Two unsupported shapes:
//
//   1. resume=true with shipper=false — the resume shim
//      (@discordjs/ws WebSocketManager) replaces discord.js's Client
//      entirely, so the in-process interaction dispatcher has no
//      `client.on('interactionCreate')` emitter to attach to. The
//      flag-on path is only coherent when the shipper has already
//      moved dispatch to SQS.
//   2. resume=true with role=combined — combined mode runs both
//      tiers in one process, which the legacy discord.js Client
//      owns end-to-end. The resume shim would conflict with the
//      Client's WS ownership.
//
// (combined + shipper=true is independently rejected by
// unsupportedRoleShipperCombo; resume's combined-mode rejection
// catches the remaining combined+shipper-off+resume-on case.)
//
// Returns the operator-facing message on rejection or null on
// success. Same string-or-null shape as unsupportedRoleShipperCombo.
function unsupportedRoleResumeCombo(role, resumeEnabled, eventShipperEnabled) {
  if (!resumeEnabled) return null;
  if (role === 'combined') {
    return (
      'PROCESS_ROLE=combined with ENABLE_GATEWAY_RESUME=true is not supported ' +
      '(the resume shim owns the WebSocket and conflicts with the legacy ' +
      'discord.js Client that combined mode runs). Run two processes: ' +
      'PROCESS_ROLE=gateway (singleton, owns the resume shim) + ' +
      'PROCESS_ROLE=http (consumes from SQS). For local dev / sandbox in ' +
      'one process, leave ENABLE_GATEWAY_RESUME unset.'
    );
  }
  if (!eventShipperEnabled) {
    return (
      'ENABLE_GATEWAY_RESUME=true requires ENABLE_EVENT_SHIPPER=true ' +
      '(the resume shim replaces discord.js Client with @discordjs/ws ' +
      'WebSocketManager, which forwards every frame to SQS — the ' +
      'in-process dispatcher path is unreachable from the shim). ' +
      'Enable the shipper first, or leave ENABLE_GATEWAY_RESUME unset.'
    );
  }
  return null;
}

// PLACEHOLDER is treated as missing because the SSM parameter
// ships with that literal sentinel value; remediation ("seed a
// real key") is identical to the empty-key case.
//
// TODO(infra-sentinel-sync): the literal "PLACEHOLDER" is also
// the seed value for `aws_ssm_parameter.bot` in
// qurl-integrations-infra/qurl-bot-discord/terraform/main.tf
// (search that repo for `value = "PLACEHOLDER"`). If infra ever
// renames the sentinel (e.g., "REPLACE_ME"), update here in
// lockstep — otherwise the boot check silently regresses to
// "non-empty value passes" and the original incident class
// returns. `git grep TODO(infra-sentinel-sync)` finds the marker.
const GOOGLE_MAPS_API_KEY_PLACEHOLDER_SENTINEL = 'PLACEHOLDER';
function missingMapCommandKeys(cfg) {
  if (!cfg.MAP_COMMAND_ENABLED) return [];
  const key = cfg.GOOGLE_MAPS_API_KEY;
  if (!key || key === GOOGLE_MAPS_API_KEY_PLACEHOLDER_SENTINEL) {
    return ['GOOGLE_MAPS_API_KEY'];
  }
  return [];
}

// Process-role parsing for the gateway/HTTP split. Lifted out of
// index.js so the invalid-value path is testable without spawning a
// child process — same shape as missingBootKeys above. See
// .env.example's PROCESS_ROLE block for the operator-facing
// description of each role.
const VALID_PROCESS_ROLES = Object.freeze(['combined', 'gateway', 'http']);

// Normalize and validate a PROCESS_ROLE value. Returns
// `{role, isGateway, isHttp}` on success; throws an Error tagged
// with `code = 'INVALID_PROCESS_ROLE'` on an unknown role so index.js
// can surface a single boot-fail log line + exit(1) without
// re-implementing the validation.
//
// Accepts the raw env-var string (or undefined). Empty / whitespace-only
// values fall back to 'combined' — matching the documented default and
// avoiding a false-positive boot failure when an SSM-seeded param is
// templated to "" or " ".
function resolveProcessRole(rawValue) {
  const trimmed = String(rawValue ?? '').trim();
  const role = trimmed || 'combined';
  if (!VALID_PROCESS_ROLES.includes(role)) {
    const err = new Error(
      `Invalid PROCESS_ROLE: '${role}'. Valid values: ${VALID_PROCESS_ROLES.join(', ')}. ` +
      `Set PROCESS_ROLE to one of these, or leave unset to default to 'combined'.`
    );
    err.code = 'INVALID_PROCESS_ROLE';
    throw err;
  }
  return {
    role,
    isGateway: role === 'gateway' || role === 'combined',
    isHttp: role === 'http' || role === 'combined',
  };
}

// Whether the local `interactionCreate` listener should be registered
// in this process. Lifted out of index.js so the gate logic is pure +
// unit-testable across every role × flag permutation (combined + flag-on
// is unreachable here — `unsupportedRoleShipperCombo` rejects it at
// boot — but the predicate must remain coherent for any caller that
// somehow reaches it post-bypass, so it's defined for all inputs).
//
// Three intended shapes:
//   - Gateway tier + flag-off  → register (legacy in-process dispatch)
//   - Gateway tier + flag-on   → DO NOT register (publishes to SQS;
//                                local listener would dispatch the
//                                same payload a second time after the
//                                worker tier's consumer re-emits)
//   - Worker tier (HTTP + flag-on) → register (SQS consumer
//                                reconstructs the interaction and
//                                re-emits locally; the listener
//                                routes via handleCommand /
//                                handleFlowInteraction)
//   - HTTP-only + flag-off     → DO NOT register (no gateway WS,
//                                no SQS consumer, so the listener
//                                would never fire and registering it
//                                would just leak handler references)
//
// The predicate `(isGateway && !flag) || (isHttp && flag)` collapses
// these four cases. Combined mode (isGateway=true, isHttp=true) with
// flag-off reduces to the first disjunct (register); with flag-on it
// would reduce to "register" via BOTH disjuncts (the false-positive
// the boot rejection guards against — see unsupportedRoleShipperCombo).
function shouldRegisterInteractionListener({ isGateway, isHttp, eventShipperEnabled }) {
  return (isGateway && !eventShipperEnabled) || (isHttp && eventShipperEnabled);
}

module.exports = {
  bootRequired,
  prodRequired,
  missingBootKeys,
  missingProdKeys,
  missingKekRequiredKeys,
  missingEventShipperKeys,
  unsupportedRoleShipperCombo,
  unsupportedRoleResumeCombo,
  shouldRegisterInteractionListener,
  missingMapCommandKeys,
  GOOGLE_MAPS_API_KEY_PLACEHOLDER_SENTINEL,
  VALID_PROCESS_ROLES,
  resolveProcessRole,
};
