'use strict';

// TODO(upstream-contract): qurl-integrations-infra#1551's POST /api/revoke_links
// rejects the whole request with 400 when any qurl_id exceeds 64 characters or
// uses anything but letters, digits, `_` and `-`. Matching that cap keeps one
// corrupt stored value from failing every retry of an otherwise valid batch.
const MAX_QURL_ID_LENGTH = 64;
// Current upstream IDs are q_ + 11 lowercase hex chars. Stay tolerant of older
// q_-prefixed display handles, but never send separators, bearer-token
// prefixes, or other arbitrary stored data to the endpoint.
const CLEANUP_QURL_ID_PATTERN = /^q_[A-Za-z0-9_]+$/;

// Returns a bounded identity usable for best-effort child revoke, or null.
function qurlIdForCleanup(value) {
  if (typeof value !== 'string') return null;
  const normalized = value.trim();
  if (normalized.length > MAX_QURL_ID_LENGTH || !CLEANUP_QURL_ID_PATTERN.test(normalized)) return null;
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
