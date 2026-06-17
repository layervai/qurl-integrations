const SSM_PLACEHOLDER_SECRET = 'PLACEHOLDER';
const MIN_OAUTH_STATE_SECRET_LENGTH = 32;

function normalizeSecretValue(value) {
  if (typeof value !== 'string') return undefined;
  const trimmed = value.trim();
  if (!trimmed || trimmed === SSM_PLACEHOLDER_SECRET) return undefined;
  return trimmed;
}

function readEnvSecret(name) {
  return normalizeSecretValue(process.env[name]);
}

function isUsableOAuthStateSecret(value) {
  const secret = normalizeSecretValue(value);
  return Boolean(secret && secret.length >= MIN_OAUTH_STATE_SECRET_LENGTH);
}

function normalizeProductionOAuthStateSecrets(env) {
  return {
    legacy: normalizeSecretValue(env.OAUTH_STATE_SECRET),
    github: normalizeSecretValue(env.GITHUB_OAUTH_STATE_SECRET),
    qurl: normalizeSecretValue(env.QURL_OAUTH_STATE_SECRET),
  };
}

function collectStateSecrets(candidates, { errorPrefix, warnShortOptional } = {}) {
  const secrets = [];
  for (const { value, label, optionalAfterPrimary = false } of candidates) {
    if (!value) continue;
    if (value.length < MIN_OAUTH_STATE_SECRET_LENGTH) {
      if (optionalAfterPrimary && secrets.length > 0) {
        if (warnShortOptional) warnShortOptional(label, value.length);
        continue;
      }
      throw new Error(
        `${errorPrefix}: ${label} is shorter than ${MIN_OAUTH_STATE_SECRET_LENGTH} chars `
        + `(got ${value.length}). Generate a 64-char value with: openssl rand -hex 32.`
      );
    }
    if (!secrets.includes(value)) secrets.push(value);
  }
  return secrets;
}

function collectOAuthFlowStateSecrets({ primaryEnvName, errorPrefix, warnShortOptional }) {
  return collectStateSecrets([
    { value: readEnvSecret(primaryEnvName), label: primaryEnvName },
    { value: readEnvSecret('OAUTH_STATE_SECRET'), label: 'OAUTH_STATE_SECRET', optionalAfterPrimary: true },
  ], {
    errorPrefix,
    warnShortOptional,
  });
}

function shortSecretMessage(name, value) {
  const secret = normalizeSecretValue(value);
  if (!secret || secret.length >= MIN_OAUTH_STATE_SECRET_LENGTH) return null;
  return `${name} must be at least ${MIN_OAUTH_STATE_SECRET_LENGTH} chars (got ${secret.length}). `
    + 'Generate with: openssl rand -hex 32';
}

function validateProductionOAuthStateSecrets(env, { isOpenNHPActive, isQurlOAuthConfigured }) {
  // Boot-time validation mirrors collectStateSecrets() by using the same
  // normalization helper and MIN_OAUTH_STATE_SECRET_LENGTH constant. Keep
  // runtime collection and production boot rules in lockstep.
  const secrets = normalizeProductionOAuthStateSecrets(env);
  const errors = [];
  const seenErrors = new Set();

  function pushError(message) {
    if (seenErrors.has(message)) return;
    seenErrors.add(message);
    errors.push(message);
  }

  function requireFlowSecret(primaryName, primary, flowLabel) {
    const legacy = secrets.legacy;
    const primaryShort = shortSecretMessage(primaryName, primary);
    const legacyShort = shortSecretMessage('OAUTH_STATE_SECRET', legacy);

    if (primaryShort) {
      pushError(primaryShort);
      return;
    }

    if (isUsableOAuthStateSecret(primary)) return;

    if (legacyShort) {
      pushError(`${legacyShort} It is currently the legacy OAuth state secret during migration.`);
      return;
    }

    if (!isUsableOAuthStateSecret(legacy)) {
      pushError(
        `${primaryName} must be set in production ${flowLabel} OAuth mode `
        + '(or legacy OAUTH_STATE_SECRET during migration). Generate with: openssl rand -hex 32'
      );
    }
  }

  if (isOpenNHPActive) {
    requireFlowSecret('GITHUB_OAUTH_STATE_SECRET', secrets.github, 'GitHub');
  }
  if (isQurlOAuthConfigured) {
    requireFlowSecret('QURL_OAUTH_STATE_SECRET', secrets.qurl, 'qURL/Auth0');
  }

  return { errors, secrets };
}

function productionOAuthStateSecretWarnings(secrets, { isOpenNHPActive, isQurlOAuthConfigured }) {
  const warnings = [];
  const { legacy, github, qurl } = secrets;

  if (isOpenNHPActive && legacy && !github) {
    warnings.push('GitHub OAuth state is using legacy OAUTH_STATE_SECRET; provision GITHUB_OAUTH_STATE_SECRET to close the migration window.');
  }
  if (isQurlOAuthConfigured && legacy && !qurl) {
    warnings.push('qURL OAuth state is using legacy OAUTH_STATE_SECRET; provision QURL_OAUTH_STATE_SECRET to close the migration window.');
  }
  if (legacy && legacy.length < MIN_OAUTH_STATE_SECRET_LENGTH
      && ((isOpenNHPActive && github) || (isQurlOAuthConfigured && qurl))) {
    warnings.push(
      `OAUTH_STATE_SECRET is shorter than ${MIN_OAUTH_STATE_SECRET_LENGTH} chars and will be ignored `
      + 'while dedicated OAuth state secrets are active.'
    );
  }
  if (github && qurl && github === qurl) {
    warnings.push('GITHUB_OAUTH_STATE_SECRET and QURL_OAUTH_STATE_SECRET are identical; use distinct values to preserve rotation isolation.');
  }
  if (legacy && github && legacy === github) {
    warnings.push('GITHUB_OAUTH_STATE_SECRET matches legacy OAUTH_STATE_SECRET; use distinct values before removing legacy to preserve rotation isolation.');
  }
  if (legacy && qurl && legacy === qurl) {
    warnings.push('QURL_OAUTH_STATE_SECRET matches legacy OAUTH_STATE_SECRET; use distinct values before removing legacy to preserve rotation isolation.');
  }

  return warnings;
}

function enforceProductionOAuthStateSecrets(env, flags, { logger = console, exit = process.exit } = {}) {
  const { errors, secrets } = validateProductionOAuthStateSecrets(env, flags);
  if (errors.length > 0) {
    errors.forEach(error => logger.error(error));
    exit(1);
    return { ok: false, errors, warnings: [], secrets };
  }

  const warnings = productionOAuthStateSecretWarnings(secrets, flags);
  warnings.forEach(warning => logger.warn(warning));
  return { ok: true, errors: [], warnings, secrets };
}

module.exports = {
  SSM_PLACEHOLDER_SECRET,
  MIN_OAUTH_STATE_SECRET_LENGTH,
  collectOAuthFlowStateSecrets,
  collectStateSecrets,
  enforceProductionOAuthStateSecrets,
  normalizeSecretValue,
  productionOAuthStateSecretWarnings,
  readEnvSecret,
  validateProductionOAuthStateSecrets,
};
