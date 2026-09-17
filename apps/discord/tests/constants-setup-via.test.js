const { SETUP_VIA, normalizeSetupVia, describeSetupVia } = require('../src/constants');

describe('SETUP_VIA', () => {
  test('is frozen with the wire values the audit and subscription description use', () => {
    expect(Object.isFrozen(SETUP_VIA)).toBe(true);
    expect(SETUP_VIA).toEqual({ OAUTH: 'oauth', PASTE: 'paste', UNKNOWN: 'unknown' });
  });

  test('normalizeSetupVia keeps known doors and collapses anything else to unknown', () => {
    expect(normalizeSetupVia(SETUP_VIA.OAUTH)).toBe(SETUP_VIA.OAUTH);
    expect(normalizeSetupVia(SETUP_VIA.PASTE)).toBe(SETUP_VIA.PASTE);
    // UNKNOWN is an output sentinel; passing it in is caller drift.
    expect(normalizeSetupVia(SETUP_VIA.UNKNOWN)).toBe(SETUP_VIA.UNKNOWN);
    expect(normalizeSetupVia(undefined)).toBe(SETUP_VIA.UNKNOWN);
    expect(normalizeSetupVia(null)).toBe(SETUP_VIA.UNKNOWN);
    // Keeps the persisted subscription description internally controlled.
    expect(normalizeSetupVia('oauth), configuredBy=attacker')).toBe(SETUP_VIA.UNKNOWN);
    expect(normalizeSetupVia('OAuth')).toBe(SETUP_VIA.UNKNOWN);
  });

  test('describeSetupVia echoes only short slug-shaped values', () => {
    expect(describeSetupVia('install-link')).toEqual({ via: 'install-link', via_type: 'string' });
    expect(describeSetupVia('lv_live_abcdefghijklmnopqrstuvwxyz0123456789')).toEqual({ via: '[unrecognized]', via_type: 'string' });
    expect(describeSetupVia({ a: 1 })).toEqual({ via: '[unrecognized]', via_type: 'object' });
    expect(describeSetupVia('0123456789abcdef0123456789abcdef')).toEqual({ via: '[unrecognized]', via_type: 'string' });
  });
});
