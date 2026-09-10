'use strict';

// TODO(upstream-contract): #1553 accepts at most 10 qurl_ids in a <=4 KiB request. Current qurl-service
// IDs are much shorter; this generous bot-side ceiling keeps even a full batch
// safely below that transport limit while quarantining corrupt legacy values
// that could otherwise make every retry fail atomically with 413.
const MAX_QURL_ID_LENGTH = 128;
// TODO(upstream-contract): Current upstream IDs are exactly q_ + 11 lowercase hex chars. Keep cleanup
// tolerant of older q_-prefixed display handles, but never pass separators,
// bearer-token prefixes, or other arbitrary stored data to the endpoint.
const CLEANUP_QURL_ID_PATTERN = /^q_[A-Za-z0-9_]+$/;

function qurlIdForCleanup(value) {
  if (typeof value !== 'string') return null;
  const normalized = value.trim();
  if (
    normalized.length === 0
    || normalized.length > MAX_QURL_ID_LENGTH
    || !CLEANUP_QURL_ID_PATTERN.test(normalized)
  ) return null;
  return normalized;
}

// Persistence rejects whitespace/non-string/unsafe values without claiming the
// tolerant legacy cleanup grammar is the current upstream canonical format.
function hasPersistableQurlIdShape(value) {
  const normalized = qurlIdForCleanup(value);
  return normalized !== null && normalized === value;
}

module.exports = {
  MAX_QURL_ID_LENGTH,
  qurlIdForCleanup,
  hasPersistableQurlIdShape,
};
