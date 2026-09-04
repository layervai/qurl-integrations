// qURL webhook-secret trust boundary.
//
// TODO(upstream-contract): keep the strict reusable-secret shape aligned with
// qurl-service/internal/domain/webhook.go (GenerateWebhookSecret). A stored
// default secret is trusted across process restarts, so only the documented
// server-issued shape may cross that startup boundary.
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

// Unset is intentional pure-BYOK mode. Any configured non-empty value becomes
// the receiver's default HMAC key, so fail startup unless it is positively
// identified as qurl-service-issued key material.
function assertConfiguredWebhookSecret(value) {
  if (value === undefined || value === null || value === '') return false;
  if (!isServerIssuedSecret(value)) {
    throw new Error(`QURL_WEBHOOK_SECRET must be unset for pure-BYOK mode or contain a server-issued ${SERVER_SECRET_EXPECTED_FORMAT}`);
  }
  return true;
}

// Create/rotate is different from startup reuse: qurl-service may have already
// committed a rotation before this response reaches us. Rejecting unfamiliar
// but usable key material here would discard the only live secret and cause an
// outage. Reject only values that cannot be HMAC keys or the known public seed;
// callers separately warn on shape drift before persisting the response.
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
};
