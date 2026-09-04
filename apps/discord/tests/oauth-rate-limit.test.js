const config = require('../src/config');
const logger = require('../src/logger');
const {
  MAX_RATE_LIMIT_STORE_SIZE,
  installRateLimit,
  rateLimit,
  rateLimitStore,
  sweepRateLimitStore,
} = require('../src/utils/oauth-rate-limit');

function response() {
  return {
    renderPage: jest.fn().mockReturnValue('rate-limited'),
    send: jest.fn().mockReturnThis(),
    status: jest.fn().mockReturnThis(),
  };
}

describe('OAuth rate-limit store', () => {
  const now = 2_000_000;

  beforeEach(() => {
    jest.clearAllMocks();
    rateLimitStore.clear();
    jest.spyOn(Date, 'now').mockReturnValue(now);
  });

  afterEach(() => {
    jest.restoreAllMocks();
  });

  it('sweeps expired buckets independently and preserves a live sibling bucket', () => {
    const expired = now - config.RATE_LIMIT_WINDOW_MS * 2 - 1;
    const recent = now - config.RATE_LIMIT_WINDOW_MS;
    rateLimitStore.set('198.51.100.1', {
      callback: [expired],
      'discord-install-entry': [expired, recent],
    });

    sweepRateLimitStore();

    expect(rateLimitStore.get('198.51.100.1')).toEqual({
      'discord-install-entry': [recent],
    });
  });

  it('removes an IP once every bucket is expired', () => {
    const expired = now - config.RATE_LIMIT_WINDOW_MS * 2 - 1;
    rateLimitStore.set('198.51.100.2', {
      callback: [expired],
      'discord-install-entry': [expired],
    });

    sweepRateLimitStore();

    expect(rateLimitStore.has('198.51.100.2')).toBe(false);
  });

  it('evicts install-only traffic so the shared hard cap cannot starve a callback', () => {
    for (let i = 0; i < MAX_RATE_LIMIT_STORE_SIZE; i += 1) {
      rateLimitStore.set(`install-${i}`, { 'discord-install-entry': [now] });
    }
    const res = response();
    const next = jest.fn();

    rateLimit({ ip: 'legitimate-callback', path: '/oauth/discord/callback' }, res, next);

    expect(next).toHaveBeenCalledTimes(1);
    expect(res.status).not.toHaveBeenCalled();
    expect(rateLimitStore.size).toBeLessThanOrEqual(MAX_RATE_LIMIT_STORE_SIZE);
    expect(rateLimitStore.has('install-0')).toBe(false);
    expect(rateLimitStore.get('legitimate-callback')).toEqual({ callback: [now] });
  });

  it('evicts the least-recently-active install-only entry without resetting active counters', () => {
    rateLimitStore.set('old-but-refreshed', { 'discord-install-entry': [now - 10] });
    rateLimitStore.set('least-recent', { 'discord-install-entry': [now - 5] });
    // Refreshing an existing entry must move its index position without
    // discarding the timestamp that still counts against its per-IP budget.
    rateLimitStore.set('old-but-refreshed', {
      'discord-install-entry': [now - 10, now],
    });
    for (let i = 0; i < MAX_RATE_LIMIT_STORE_SIZE - 2; i += 1) {
      rateLimitStore.set(`callback-${i}`, { callback: [now] });
    }

    const res = response();
    const next = jest.fn();
    rateLimit({ ip: 'new-callback', path: '/oauth/discord/callback' }, res, next);

    expect(next).toHaveBeenCalledTimes(1);
    expect(rateLimitStore.has('least-recent')).toBe(false);
    expect(rateLimitStore.get('old-but-refreshed')).toEqual({
      'discord-install-entry': [now - 10, now],
    });
  });

  it('re-indexes an install-only IP when sweep expires its callback bucket', () => {
    const expired = now - config.RATE_LIMIT_WINDOW_MS * 2 - 1;
    rateLimitStore.set('becomes-install-only', {
      callback: [expired],
      'discord-install-entry': [now],
    });
    sweepRateLimitStore();
    for (let i = 0; i < MAX_RATE_LIMIT_STORE_SIZE - 1; i += 1) {
      rateLimitStore.set(`callback-${i}`, { callback: [now] });
    }

    const res = response();
    const next = jest.fn();
    rateLimit({ ip: 'new-callback', path: '/oauth/discord/callback' }, res, next);

    expect(next).toHaveBeenCalledTimes(1);
    expect(rateLimitStore.has('becomes-install-only')).toBe(false);
    expect(rateLimitStore.get('new-callback')).toEqual({ callback: [now] });
  });

  it('can reach and hold the hard cap before shedding the next new IP', () => {
    for (let i = 0; i < MAX_RATE_LIMIT_STORE_SIZE - 1; i += 1) {
      rateLimitStore.set(`callback-${i}`, { callback: [now] });
    }
    const accepted = response();
    const acceptedNext = jest.fn();

    rateLimit({ ip: 'last-capacity', path: '/oauth/qurl/callback' }, accepted, acceptedNext);

    expect(acceptedNext).toHaveBeenCalledTimes(1);
    expect(rateLimitStore.size).toBe(MAX_RATE_LIMIT_STORE_SIZE);

    const shed = response();
    const shedNext = jest.fn();
    jest.spyOn(logger, 'warn').mockImplementation(() => {});
    installRateLimit({ ip: 'over-capacity', path: '/oauth/discord/install' }, shed, shedNext);

    expect(shedNext).not.toHaveBeenCalled();
    expect(shed.status).toHaveBeenCalledWith(429);
    expect(rateLimitStore.size).toBe(MAX_RATE_LIMIT_STORE_SIZE);
  });

  it('still sheds new install traffic at the shared hard cap', () => {
    for (let i = 0; i < MAX_RATE_LIMIT_STORE_SIZE; i += 1) {
      rateLimitStore.set(`callback-${i}`, { callback: [now] });
    }
    const res = response();
    const next = jest.fn();
    jest.spyOn(logger, 'warn').mockImplementation(() => {});

    installRateLimit({ ip: 'new-install', path: '/oauth/discord/install' }, res, next);

    expect(next).not.toHaveBeenCalled();
    expect(res.status).toHaveBeenCalledWith(429);
    expect(rateLimitStore.has('new-install')).toBe(false);
    expect(rateLimitStore.size).toBe(MAX_RATE_LIMIT_STORE_SIZE);
  });

  it('sheds a callback in O(1) when the full store has no install-only entry', () => {
    for (let i = 0; i < MAX_RATE_LIMIT_STORE_SIZE; i += 1) {
      rateLimitStore.set(`callback-${i}`, { callback: [now] });
    }
    const originalIterator = rateLimitStore[Symbol.iterator];
    rateLimitStore[Symbol.iterator] = jest.fn(() => {
      throw new Error('at-cap callback path must not scan the store');
    });
    const res = response();
    const next = jest.fn();
    jest.spyOn(logger, 'warn').mockImplementation(() => {});

    try {
      rateLimit({ ip: 'new-callback', path: '/oauth/qurl/callback' }, res, next);
    } finally {
      rateLimitStore[Symbol.iterator] = originalIterator;
    }

    expect(next).not.toHaveBeenCalled();
    expect(res.status).toHaveBeenCalledWith(429);
    expect(rateLimitStore.size).toBe(MAX_RATE_LIMIT_STORE_SIZE);
  });
});
