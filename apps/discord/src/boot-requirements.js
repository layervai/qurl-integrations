// Pure helpers computing which env-vars the bot refuses to boot without.
// Extracted from index.js so they can be unit-tested without importing
// the full bot (which would side-effect client.login()). The lists are
// the highest-risk branch in the module: a regression here could either
// boot in prod with missing secrets OR die on a spurious false-positive.

// Required at boot in EVERY environment. Split by mode: multi-tenant
// doesn't need GITHUB_* / BASE_URL / GUILD_ID because /auth + /webhook
// routes stay unmounted and every OpenNHP code path is gated off.
function bootRequired(isMultiTenant) {
  if (isMultiTenant) return ['DISCORD_TOKEN'];
  return ['DISCORD_TOKEN', 'GITHUB_CLIENT_ID', 'GITHUB_CLIENT_SECRET', 'GITHUB_WEBHOOK_SECRET', 'GUILD_ID', 'BASE_URL'];
}

// Additionally required when NODE_ENV=production. QURL_API_KEY is the
// global-fallback for /qurl send; in multi-tenant mode each guild brings
// their own via /qurl setup, so it's optional there.
function prodRequired(isMultiTenant) {
  if (isMultiTenant) return ['METRICS_TOKEN', 'KEY_ENCRYPTION_KEY'];
  return ['METRICS_TOKEN', 'QURL_API_KEY', 'KEY_ENCRYPTION_KEY'];
}

// Compute which required keys are missing from a given config-like
// object. Separate from bootRequired so tests can build a "config" with
// specific holes and assert the exact missing list.
function missingBootKeys(cfg, isMultiTenant) {
  return bootRequired(isMultiTenant).filter(key => !cfg[key]);
}

function missingProdKeys(env, isMultiTenant) {
  return prodRequired(isMultiTenant).filter(k => !env[k]);
}

module.exports = { bootRequired, prodRequired, missingBootKeys, missingProdKeys };
