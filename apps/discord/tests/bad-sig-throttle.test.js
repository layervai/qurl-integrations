// Tests for the per-IP bad-signature throttle factory in
// utils/bad-sig-throttle.js. The factory is consumed by the canary
// route today; the migration of routes/webhooks.js's inline copy is
// tracked as a follow-up. Pinning the contract here so a future
// regression doesn't cascade across both consumers.

const { createBadSigThrottle } = require('../src/utils/bad-sig-throttle');

describe('createBadSigThrottle — defaults', () => {
  let throttle;
  beforeEach(() => {
    throttle = createBadSigThrottle();
  });

  it('check() returns false on a fresh IP', () => {
    expect(throttle.check('1.2.3.4')).toBe(false);
  });

  it('record() returns the post-record count for log context', () => {
    expect(throttle.record('1.2.3.4')).toBe(1);
    expect(throttle.record('1.2.3.4')).toBe(2);
    expect(throttle.record('1.2.3.4')).toBe(3);
  });

  it('check() flips to true at the 30th attempt with default config', () => {
    for (let i = 0; i < 29; i++) throttle.record('1.2.3.4');
    expect(throttle.check('1.2.3.4')).toBe(false);
    throttle.record('1.2.3.4');
    expect(throttle.check('1.2.3.4')).toBe(true);
  });

  it('check() is per-IP — one IP being over-budget does not affect another', () => {
    for (let i = 0; i < 30; i++) throttle.record('1.2.3.4');
    expect(throttle.check('1.2.3.4')).toBe(true);
    expect(throttle.check('5.6.7.8')).toBe(false);
  });

  it('reset() clears the entire Map', () => {
    for (let i = 0; i < 30; i++) throttle.record('1.2.3.4');
    expect(throttle.check('1.2.3.4')).toBe(true);
    throttle.reset();
    expect(throttle.check('1.2.3.4')).toBe(false);
  });

  it('respects the rolling window — old attempts drop out', () => {
    // Exercise the windowMs filter. Use a custom factory with a
    // 50ms window so the test doesn't have to fake-time.
    const t = createBadSigThrottle({ windowMs: 50, maxPerWindow: 5 });
    for (let i = 0; i < 5; i++) t.record('1.2.3.4');
    expect(t.check('1.2.3.4')).toBe(true);
    return new Promise(resolve => setTimeout(() => {
      // After the window passes, all old timestamps drop out and
      // the IP is fresh again.
      expect(t.check('1.2.3.4')).toBe(false);
      resolve();
    }, 80));
  });

  it('honors a custom maxPerWindow', () => {
    const t = createBadSigThrottle({ maxPerWindow: 3 });
    expect(t.check('1.2.3.4')).toBe(false);
    t.record('1.2.3.4');
    t.record('1.2.3.4');
    expect(t.check('1.2.3.4')).toBe(false);
    t.record('1.2.3.4');
    expect(t.check('1.2.3.4')).toBe(true);
  });

  it('caps the per-IP timestamp array at perIpCap = 4× maxPerWindow', () => {
    // 30 max → 120 cap. Push 200 → array sliced to last 120.
    // Exposing perIpCap via record() return value is the only way
    // to observe — after 200 records, return is min(200, 120).
    for (let i = 0; i < 200; i++) throttle.record('1.2.3.4');
    expect(throttle.record('1.2.3.4')).toBe(120);
  });
});
