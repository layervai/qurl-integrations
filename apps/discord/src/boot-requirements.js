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
// global-fallback for /qurl send; single-guild-plain and multi-tenant
// deployments both rely on per-guild /qurl setup, so it's optional
// outside the OpenNHP community server.
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

module.exports = {
  bootRequired,
  prodRequired,
  missingBootKeys,
  missingProdKeys,
  VALID_PROCESS_ROLES,
  resolveProcessRole,
};
