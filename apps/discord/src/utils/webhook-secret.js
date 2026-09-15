// qURL webhook-secret trust boundary.
//
// Two deliberately different policies share the helpers below:
//   - assertUsableResponseSecret (create/rotate response): reject unusable
//     values and the public seed; anything else is persisted even on shape
//     drift because upstream may already have committed the rotation.
//   - assertConfiguredWebhookSecret (startup, SSM-fed): same reject set, but
//     shape drift only warns — a strict startup check would crashloop every
//     bot task after a legitimate upstream format change. Do not "unify".
//
const logger = require('../logger');

// TODO(upstream-contract): keep the expected shape aligned with
// qurl-service/internal/domain/webhook.go (GenerateWebhookSecret). Shape drift
// warns but must not discard a secret whose upstream rotation already committed.
const SERVER_SECRET_PREFIX = 'whsec_';
// Upstream emits 43 chars (32 random bytes, base64url). 16 is a deliberate
// tolerance band so a plausible future format does not warn; do not tighten.
const SERVER_SECRET_MIN_BODY_LENGTH = 16;
const SERVER_SECRET_MIN_LENGTH = SERVER_SECRET_PREFIX.length + SERVER_SECRET_MIN_BODY_LENGTH;
const SERVER_SECRET_BODY_RE = /^[A-Za-z0-9_-]+$/;
const SERVER_SECRET_EXPECTED_FORMAT = `${SERVER_SECRET_PREFIX} prefix with at least ${SERVER_SECRET_MIN_BODY_LENGTH} base64url characters after it`;

// Single definition of the infra seed literal; boot-requirements.js consumes
// it for the Maps key check. It must never become an HMAC key, whether
// supplied through SSM or returned by an upstream response.
//
// TODO(infra-sentinel-sync): the literal "PLACEHOLDER" is also the seed
// value for the `aws_ssm_parameter` resources in
// qurl-integrations-infra/qurl-bot-discord/terraform (search that repo for
// `value = "PLACEHOLDER"`). If infra ever renames the sentinel (e.g.,
// "REPLACE_ME"), update here in lockstep — otherwise both checks silently
// regress to "non-empty value passes" and the original incident class
// returns. `git grep TODO(infra-sentinel-sync)` finds the marker.
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

// config.js trims outer whitespace on read; beyond that, preserve the exact
// server bytes across persistence and restart. Usability is not proof that an
// operator-supplied key matches upstream.
function assertConfiguredWebhookSecret(value) {
  if (value === undefined || value === null || value === '') return false;
  if (!isUsableSecret(value)) {
    throw new Error('QURL_WEBHOOK_SECRET is unusable or the public seed; run the registrar and verify its SSM persist succeeded');
  }
  if (!isServerIssuedSecret(value)) {
    logger.warn('qURL configured webhook secret has unrecognized format — preserving stored key');
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
    throw new Error(`${operation}: contract drift (response secret is blank; expected ${SERVER_SECRET_EXPECTED_FORMAT})`);
  }
  if (isInfraSeedSentinel(value)) {
    throw new Error(`${operation}: contract drift (response secret is the public infrastructure seed sentinel; expected ${SERVER_SECRET_EXPECTED_FORMAT})`);
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
