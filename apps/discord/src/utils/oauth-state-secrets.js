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

function isSeededSecret(value) {
  return normalizeSecretValue(value) !== undefined;
}

function isUsableOAuthStateSecret(value) {
  const secret = normalizeSecretValue(value);
  return Boolean(secret && secret.length >= MIN_OAUTH_STATE_SECRET_LENGTH);
}

function shortSecretMessage(name, value) {
  const secret = normalizeSecretValue(value);
  if (!secret || secret.length >= MIN_OAUTH_STATE_SECRET_LENGTH) return null;
  return `${name} must be at least ${MIN_OAUTH_STATE_SECRET_LENGTH} chars (got ${secret.length}). `
    + 'Generate with: openssl rand -hex 32';
}

function validateProductionOAuthStateSecrets(env, { isOpenNHPActive, isQurlOAuthConfigured }) {
  const errors = [];

  function requireFlowSecret(primaryName, flowLabel) {
    const primary = normalizeSecretValue(env[primaryName]);
    const legacy = normalizeSecretValue(env.OAUTH_STATE_SECRET);
    const primaryShort = shortSecretMessage(primaryName, primary);
    const legacyShort = shortSecretMessage('OAUTH_STATE_SECRET', legacy);

    if (primaryShort) {
      errors.push(primaryShort);
      return;
    }

    if (isUsableOAuthStateSecret(primary)) return;

    if (legacyShort) {
      errors.push(`${legacyShort} It is currently the legacy ${flowLabel} OAuth state secret.`);
      return;
    }

    if (!isUsableOAuthStateSecret(legacy)) {
      errors.push(
        `${primaryName} must be set in production ${flowLabel} OAuth mode `
        + '(or legacy OAUTH_STATE_SECRET during migration). Generate with: openssl rand -hex 32'
      );
    }
  }

  if (isOpenNHPActive) {
    requireFlowSecret('GITHUB_OAUTH_STATE_SECRET', 'GitHub');
  }
  if (isQurlOAuthConfigured) {
    requireFlowSecret('QURL_OAUTH_STATE_SECRET', 'qURL/Auth0');
  }

  return errors;
}

module.exports = {
  SSM_PLACEHOLDER_SECRET,
  MIN_OAUTH_STATE_SECRET_LENGTH,
  normalizeSecretValue,
  readEnvSecret,
  isSeededSecret,
  isUsableOAuthStateSecret,
  validateProductionOAuthStateSecrets,
};
