'use strict';

const {
  MAX_QURL_ID_LENGTH,
  qurlIdForCleanup,
  hasPersistableQurlIdShape,
} = require('../src/utils/qurl-id');

describe('qURL display/revocation identity guards', () => {
  const maxLengthId = `q_${'a'.repeat(MAX_QURL_ID_LENGTH - 2)}`;

  it('accepts a bounded q_-prefixed display identity', () => {
    expect(qurlIdForCleanup(maxLengthId)).toBe(maxLengthId);
    expect(hasPersistableQurlIdShape(maxLengthId)).toBe(true);
  });

  it('rejects an overlong identity before cleanup transport', () => {
    expect(qurlIdForCleanup(`${maxLengthId}a`)).toBeNull();
    expect(hasPersistableQurlIdShape(`${maxLengthId}a`)).toBe(false);
  });

  it.each(['q_', 'bad/id', 'at_bearer_like', '', '   ', null, undefined, 42])(
    'rejects unsafe or absent identity %p',
    (value) => {
      expect(qurlIdForCleanup(value)).toBeNull();
      expect(hasPersistableQurlIdShape(value)).toBe(false);
    },
  );

  it('normalizes safe surrounding whitespace only for best-effort cleanup', () => {
    expect(qurlIdForCleanup(' q_legacy_1 ')).toBe('q_legacy_1');
    expect(hasPersistableQurlIdShape(' q_legacy_1 ')).toBe(false);
  });
});
