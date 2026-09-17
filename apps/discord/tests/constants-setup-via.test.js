const { SETUP_VIA, normalizeSetupVia } = require('../src/constants');

describe('SETUP_VIA', () => {
  test('is frozen with the wire values the audit and subscription description use', () => {
    expect(Object.isFrozen(SETUP_VIA)).toBe(true);
    expect(SETUP_VIA).toEqual({ OAUTH: 'oauth', PASTE: 'paste', UNKNOWN: 'unknown' });
  });

  test('normalizeSetupVia keeps known doors and collapses anything else to unknown', () => {
    for (const via of Object.values(SETUP_VIA)) expect(normalizeSetupVia(via)).toBe(via);
    expect(normalizeSetupVia(undefined)).toBe(SETUP_VIA.UNKNOWN);
    expect(normalizeSetupVia(null)).toBe(SETUP_VIA.UNKNOWN);
    expect(normalizeSetupVia('OAuth')).toBe(SETUP_VIA.UNKNOWN);
  });
});
