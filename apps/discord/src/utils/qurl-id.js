'use strict';

const { QURL_ACCESS_TOKEN_PREFIX } = require('./resource-id');

// TODO(upstream-contract): qurl-integrations-infra#1551's POST /api/revoke_links
// rejects the whole request with 400 when any qurl_id exceeds 64 characters or
// uses anything but letters, digits, `_` and `-`. Matching that cap keeps one
// corrupt stored value from failing every retry of an otherwise valid batch.
// This revoke-transport cap intentionally also gates mint (an unrevocable
// delivered link is worse than a failed send): if upstream ids grow, raise the
// endpoint's limit and this together rather than loosening only one side.
const MAX_QURL_ID_LENGTH = 64;
// Current upstream IDs are q_ + 11 lowercase hex chars, but only the endpoint's
// charset is required so a future id prefix cannot fail every send. Access
// tokens (at_) share that charset and are bearer credentials, so they are
// rejected explicitly and never reach the wire or the logs.
const CLEANUP_QURL_ID_PATTERN = /^[A-Za-z0-9_-]+$/;

// Returns a bounded identity usable for best-effort child revoke, or null.
function qurlIdForCleanup(value) {
  if (typeof value !== 'string') return null;
  const normalized = value.trim();
  if (
    normalized.length > MAX_QURL_ID_LENGTH
    || !CLEANUP_QURL_ID_PATTERN.test(normalized)
    || normalized.startsWith(QURL_ACCESS_TOKEN_PREFIX)
  ) return null;
  return normalized;
}

// A stored identity is trustworthy only when it needed no normalization.
function hasPersistableQurlIdShape(value) {
  return typeof value === 'string' && qurlIdForCleanup(value) === value;
}

module.exports = {
  qurlIdForCleanup,
  hasPersistableQurlIdShape,
};
