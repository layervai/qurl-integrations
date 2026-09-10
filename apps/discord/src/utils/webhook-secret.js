// qURL webhook-secret trust boundary.
//
// TODO(upstream-contract): keep the expected shape aligned with
// qurl-service/internal/domain/webhook.go (GenerateWebhookSecret). Shape drift
// warns but must not discard a secret whose upstream rotation already committed.
const SERVER_SECRET_PREFIX = 'whsec_';
const SERVER_SECRET_MIN_BODY_LENGTH = 16;
const SERVER_SECRET_MIN_LENGTH = SERVER_SECRET_PREFIX.length + SERVER_SECRET_MIN_BODY_LENGTH;
const SERVER_SECRET_BODY_RE = /^[A-Za-z0-9_-]+$/;
const SERVER_SECRET_EXPECTED_FORMAT = `${SERVER_SECRET_PREFIX} prefix with at least ${SERVER_SECRET_MIN_BODY_LENGTH} base64url characters after it`;

// TODO(upstream-contract): qurl-integrations-infra/qurl-bot-discord seeds the
// SSM parameter with this public bootstrap literal. It must never become an
// HMAC key, whether supplied through SSM or returned by an upstream response.
const INFRA_SEED_SENTINEL = 'PLACEHOLDER';

function isInfraSeedSentinel(value) {
  return typeof value === 'string'
    && value.trim().toUpperCase() === INFRA_SEED_SENTINEL;
}

function isServerIssuedSecret(value) {
  if (typeof value !== 'string' || !value.startsWith(SERVER_SECRET_PREFIX)) return false;
  const body = value.slice(SERVER_SECRET_PREFIX.length);
  return body.length >= SERVER_SECRET_MIN_BODY_LENGTH && SERVER_SECRET_BODY_RE.test(body);
}

function isUsableSecret(value) {
  return typeof value === 'string' && value.trim().length > 0 && !isInfraSeedSentinel(value);
}

// Preserve the exact server bytes, including whitespace, across persistence and
// restart. Usability is not proof that an operator-supplied key matches upstream.
function assertConfiguredWebhookSecret(value) {
  if (value === undefined || value === null || value === '') return false;
  if (!isUsableSecret(value)) {
    throw new Error('QURL_WEBHOOK_SECRET is unusable or the public seed; run the registrar and verify its SSM persist succeeded');
  }
  if (!isServerIssuedSecret(value)) {
    require('../logger').warn('qURL configured webhook secret has unrecognized format — preserving stored key');
  }
  return true;
}

// A committed rotation may return a new format. Reject unusable values and the
// public seed; callers warn on shape drift without changing the HMAC key bytes.
function assertUsableResponseSecret(value, operation) {
  if (value === undefined || value === null || value === '') {
    throw new Error(`${operation}: contract drift (response secret is missing; expected ${SERVER_SECRET_EXPECTED_FORMAT})`);
  }
  if (typeof value !== 'string') {
    throw new Error(`${operation}: contract drift (response secret has wrong type ${typeof value}; expected a string matching ${SERVER_SECRET_EXPECTED_FORMAT})`);
  }
  if (value.trim().length === 0) {
    throw new Error(`${operation}: contract drift (response secret is blank; expected a non-empty server-issued secret)`);
  }
  if (isInfraSeedSentinel(value)) {
    throw new Error(`${operation}: contract drift (response secret is the public infrastructure seed sentinel)`);
  }
  return value;
}

module.exports = {
  INFRA_SEED_SENTINEL,
  SERVER_SECRET_EXPECTED_FORMAT,
  SERVER_SECRET_MIN_LENGTH,
  assertConfiguredWebhookSecret,
  assertUsableResponseSecret,
  isInfraSeedSentinel,
  isServerIssuedSecret,
  isUsableSecret,
};
