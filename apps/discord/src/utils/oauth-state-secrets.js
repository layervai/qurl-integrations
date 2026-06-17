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

function shortSecretMessage(name, value) {
  const secret = normalizeSecretValue(value);
  if (!secret || secret.length >= MIN_OAUTH_STATE_SECRET_LENGTH) return null;
  return `${name} must be at least ${MIN_OAUTH_STATE_SECRET_LENGTH} chars (got ${secret.length}). `
    + 'Generate with: openssl rand -hex 32';
}

function validateProductionOAuthStateSecrets(env, { isOpenNHPActive, isQurlOAuthConfigured }) {
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

module.exports = {
  SSM_PLACEHOLDER_SECRET,
  MIN_OAUTH_STATE_SECRET_LENGTH,
  collectStateSecrets,
  normalizeSecretValue,
  readEnvSecret,
  validateProductionOAuthStateSecrets,
};
