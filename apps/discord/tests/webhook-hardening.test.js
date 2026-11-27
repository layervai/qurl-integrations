const { createBadSigLimiter } = require('../src/utils/webhook-hardening');

test('preserving capacity rejects unseen buckets without resetting active counters, then recovers on sweep', () => {
  jest.useFakeTimers();
  const limiter = createBadSigLimiter({ max: 2, globalCap: 2, windowMs: 100, sweepMs: 200, preserveAtCapacity: true });
  try {
    limiter.recordBadSig('owner-A');
    limiter.recordBadSig('owner-A');
    limiter.recordBadSig('owner-B');
    expect(limiter.shouldThrottle('owner-A')).toBe(true);
    expect(limiter.shouldThrottle('owner-C')).toBe(true);
    expect(limiter.recordBadSig('owner-C')).toBe(2);
    expect(limiter.recordBadSig('owner-D')).toBe(2);
    expect(limiter.shouldThrottle('owner-A')).toBe(true);
    expect(limiter.shouldThrottle('owner-B')).toBe(false);
    expect(limiter.recordBadSig('owner-B')).toBe(2);
    expect(limiter.shouldThrottle('owner-B')).toBe(true);
    jest.advanceTimersByTime(201);
    expect(limiter.shouldThrottle('owner-C')).toBe(false);
    expect(limiter.recordBadSig('owner-C')).toBe(1);
  } finally {
    limiter.stopSweep();
    jest.useRealTimers();
  }
});

test('default IP limiter still admits unseen IPs at capacity', () => {
  const limiter = createBadSigLimiter({ max: 2, globalCap: 2 });
  try {
    limiter.recordBadSig('ip-A');
    limiter.recordBadSig('ip-A');
    limiter.recordBadSig('ip-B');
    expect(limiter.shouldThrottle('ip-C')).toBe(false);
    expect(limiter.recordBadSig('ip-C')).toBe(1);
    limiter.recordBadSig('ip-D');
    expect(limiter.shouldThrottle('ip-A')).toBe(false);
  } finally {
    limiter.stopSweep();
  }
});
