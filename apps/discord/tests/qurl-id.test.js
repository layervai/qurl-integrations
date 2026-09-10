'use strict';

const {
  MAX_QURL_ID_LENGTH,
  normalizeQurlId,
} = require('../src/utils/qurl-id');

describe('qURL display/revocation identity classifier', () => {
  const maxLengthId = `q_${'a'.repeat(MAX_QURL_ID_LENGTH - 2)}`;

  it('accepts a bounded q_-prefixed display identity', () => {
    expect(normalizeQurlId(maxLengthId)).toBe(maxLengthId);
  });

  it('rejects an overlong identity before cleanup transport', () => {
    expect(normalizeQurlId(`${maxLengthId}a`)).toBeNull();
  });

  it.each(['q_', 'bad/id', 'at_bearer_like', '', '   ', null, undefined, 42])(
    'rejects unsafe or absent identity %p',
    (value) => {
      expect(normalizeQurlId(value)).toBeNull();
    },
  );

  it('normalizes safe surrounding whitespace', () => {
    expect(normalizeQurlId(' q_legacy_1 ')).toBe('q_legacy_1');
  });
});
