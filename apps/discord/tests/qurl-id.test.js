const { qurlIdForCleanup, hasPersistableQurlIdShape } = require('../src/utils/qurl-id');

describe('qURL child identity guards', () => {
  it('accepts a canonical id', () => {
    expect(qurlIdForCleanup('q_0123456789a')).toBe('q_0123456789a');
    expect(hasPersistableQurlIdShape('q_0123456789a')).toBe(true);
  });

  it('trims for best-effort cleanup but never treats the stored value as canonical', () => {
    expect(qurlIdForCleanup(' q_one ')).toBe('q_one');
    expect(hasPersistableQurlIdShape(' q_one ')).toBe(false);
  });

  it('caps length at the connector transport limit', () => {
    const atCap = `q_${'a'.repeat(62)}`;
    expect(qurlIdForCleanup(atCap)).toBe(atCap);
    expect(qurlIdForCleanup(`${atCap}a`)).toBeNull();
  });

  it.each([undefined, null, '', '   ', 42, {}, 'bad/id', 'at_bearer', 'q_', 'q_a-b'])('rejects %p', (value) => {
    expect(qurlIdForCleanup(value)).toBeNull();
    expect(hasPersistableQurlIdShape(value)).toBe(false);
  });
});
