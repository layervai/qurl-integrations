const { qurlIdForCleanup, hasPersistableQurlIdShape } = require('../src/utils/qurl-id');

describe('qURL child identity guards', () => {
  it('accepts a canonical id and the endpoint charset', () => {
    expect(qurlIdForCleanup('q_0123456789a')).toBe('q_0123456789a');
    expect(qurlIdForCleanup('q_a-b')).toBe('q_a-b');
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

  it('accepts a future id prefix within the endpoint charset', () => {
    expect(qurlIdForCleanup('t_0123456789a')).toBe('t_0123456789a');
  });

  it.each([undefined, null, '', '   ', 42, {}, 'bad/id', 'q_a.b', 'at_bearer', ' at_bearer '])('rejects %p', (value) => {
    expect(qurlIdForCleanup(value)).toBeNull();
    expect(hasPersistableQurlIdShape(value)).toBe(false);
  });
});
