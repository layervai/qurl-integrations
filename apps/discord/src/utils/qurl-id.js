'use strict';

// TODO(upstream-contract): matches #1553's 64-byte per-ID cap, so every ID the
// bot treats as identified is one the connector accepts. Longer stored values
// are quarantined as malformed instead of failing every retry of their batch,
// and ten IDs at this cap plus a maximal resource ID stay under the 4 KiB body.
const MAX_QURL_ID_LENGTH = 64;
// TODO(upstream-contract): Current upstream IDs are exactly q_ + 11 lowercase hex chars. Keep cleanup
// tolerant of older q_-prefixed display handles, but never pass separators,
// bearer-token prefixes, or other arbitrary stored data to the endpoint.
const CLEANUP_QURL_ID_PATTERN = /^q_[A-Za-z0-9_]+$/;

// The single qurl_id classifier: mint validation, persistence, fresh-mint
// cleanup, and stored-row revoke all use it, so "identified" means the same
// thing everywhere. Returns the trimmed identity, or null when there is none.
function normalizeQurlId(value) {
  if (typeof value !== 'string') return null;
  const normalized = value.trim();
  if (
    normalized.length === 0
    || normalized.length > MAX_QURL_ID_LENGTH
    || !CLEANUP_QURL_ID_PATTERN.test(normalized)
  ) return null;
  return normalized;
}

module.exports = {
  MAX_QURL_ID_LENGTH,
  normalizeQurlId,
};
