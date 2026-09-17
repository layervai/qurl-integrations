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

  it('does not let an exhausted callback bucket consume the install budget', () => {
    const ip = '198.51.100.3';
    for (let i = 0; i < config.RATE_LIMIT_MAX_REQUESTS; i += 1) {
      rateLimit({ ip, path: '/oauth/qurl/callback' }, response(), jest.fn());
    }
    const callbackRes = response();
    const callbackNext = jest.fn();
    rateLimit({ ip, path: '/oauth/qurl/callback' }, callbackRes, callbackNext);
    expect(callbackNext).not.toHaveBeenCalled();
    expect(callbackRes.status).toHaveBeenCalledWith(429);

    const installRes = response();
    const installNext = jest.fn();
    installRateLimit({ ip, path: '/oauth/discord/install' }, installRes, installNext);
    expect(installNext).toHaveBeenCalledTimes(1);
    expect(installRes.status).not.toHaveBeenCalled();
  });

  it('drops an expired callback bucket on refresh so the IP becomes evictable install-only traffic', () => {
    rateLimitStore.set('stale-callback', { callback: [now - config.RATE_LIMIT_WINDOW_MS - 1] });

    installRateLimit({ ip: 'stale-callback', path: '/oauth/discord/install' }, response(), jest.fn());

    expect(rateLimitStore.get('stale-callback')).toEqual({ 'discord-install-entry': [now] });
    expect(rateLimitStore.evictInstallOnlyEntry()).toBe(true);
    expect(rateLimitStore.has('stale-callback')).toBe(false);
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

  it('preserves least-recently-active order when a sweep filters timestamps', () => {
    const staleButRetained = now - config.RATE_LIMIT_WINDOW_MS * 2 + 1;
    rateLimitStore.set('old-but-refreshed', { 'discord-install-entry': [staleButRetained] });
    rateLimitStore.set('least-recent', { 'discord-install-entry': [now - 5] });
    rateLimitStore.set('old-but-refreshed', {
      'discord-install-entry': [staleButRetained, now],
    });
    sweepRateLimitStore();
    for (let i = 0; i < MAX_RATE_LIMIT_STORE_SIZE - 2; i += 1) {
      rateLimitStore.set(`callback-${i}`, { callback: [now] });
    }

    rateLimit(
      { ip: 'new-callback', path: '/oauth/discord/callback' },
      response(),
      jest.fn(),
    );

    expect(rateLimitStore.has('least-recent')).toBe(false);
    expect(rateLimitStore.get('old-but-refreshed')).toEqual({
      'discord-install-entry': [staleButRetained, now],
    });
  });

  it('removes an install-only IP from the eviction index when it later calls back', () => {
    rateLimitStore.set('now-callback', { 'discord-install-entry': [now] });
    rateLimit(
      { ip: 'now-callback', path: '/oauth/discord/callback' },
      response(),
      jest.fn(),
    );
    for (let i = 0; i < MAX_RATE_LIMIT_STORE_SIZE - 1; i += 1) {
      rateLimitStore.set(`callback-${i}`, { callback: [now] });
    }

    const res = response();
    const next = jest.fn();
    jest.spyOn(logger, 'warn').mockImplementation(() => {});
    rateLimit({ ip: 'new-callback', path: '/oauth/discord/callback' }, res, next);

    expect(next).not.toHaveBeenCalled();
    expect(res.status).toHaveBeenCalledWith(429);
    expect(rateLimitStore.has('now-callback')).toBe(true);
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
      // Restore the inherited Map iterator instead of leaving an own property
      // that can leak into later tests through this shared singleton.
      delete rateLimitStore[Symbol.iterator];
    }

    expect(rateLimitStore[Symbol.iterator]).toBe(originalIterator);
    expect(next).not.toHaveBeenCalled();
    expect(res.status).toHaveBeenCalledWith(429);
    expect(rateLimitStore.size).toBe(MAX_RATE_LIMIT_STORE_SIZE);
  });

  it('warns once per rate-limit window at the hard cap with per-bucket shed counts', () => {
    for (let i = 0; i < MAX_RATE_LIMIT_STORE_SIZE; i += 1) {
      rateLimitStore.set(`callback-${i}`, { callback: [now] });
    }
    const warn = jest.spyOn(logger, 'warn').mockImplementation(() => {});
    const shed = (ip) => installRateLimit({ ip, path: '/oauth/discord/install' }, response(), jest.fn());
    // A full window past any warning an earlier test in this file emitted.
    Date.now.mockReturnValue(now + config.RATE_LIMIT_WINDOW_MS);

    shed('shed-1');
    shed('shed-2');
    rateLimit({ ip: 'shed-callback', path: '/oauth/qurl/callback' }, response(), jest.fn());
    expect(warn).toHaveBeenCalledTimes(1);

    Date.now.mockReturnValue(now + config.RATE_LIMIT_WINDOW_MS * 2);
    shed('shed-3');
    expect(warn).toHaveBeenCalledTimes(2);
    expect(warn).toHaveBeenLastCalledWith(
      'Rate limit store at hard cap, rejecting new IP',
      expect.objectContaining({
        shedByBucketSinceLastWarning: { 'discord-install-entry': 2, callback: 1 },
        sinceLastWarningMs: config.RATE_LIMIT_WINDOW_MS,
      }),
    );
  });

  it('starts a new saturation episode with a fresh warning after a sweep drains the store', () => {
    const W = config.RATE_LIMIT_WINDOW_MS;
    const episodeOne = now + W * 10;
    const fillStale = () => {
      for (let i = 0; i < MAX_RATE_LIMIT_STORE_SIZE; i += 1) {
        rateLimitStore.set(`callback-${i}`, { callback: [episodeOne - W * 3] });
      }
    };
    const warn = jest.spyOn(logger, 'warn').mockImplementation(() => {});
    const shed = (ip) => installRateLimit({ ip, path: '/oauth/discord/install' }, response(), jest.fn());
    Date.now.mockReturnValue(episodeOne);
    fillStale();
    shed('episode-1-warned');
    shed('episode-1-unreported');
    expect(warn).toHaveBeenCalledTimes(1);

    Date.now.mockReturnValue(episodeOne + 1);
    sweepRateLimitStore();
    expect(rateLimitStore.size).toBe(0);

    // Still inside episode one's warning window.
    fillStale();
    Date.now.mockReturnValue(episodeOne + 2);
    shed('episode-2');
    expect(warn).toHaveBeenCalledTimes(2);
    expect(warn).toHaveBeenLastCalledWith(
      'Rate limit store at hard cap, rejecting new IP',
      expect.objectContaining({
        shedByBucketSinceLastWarning: { 'discord-install-entry': 1 },
        sinceLastWarningMs: null,
      }),
    );
  });

  it('stops the sweep interval on shutdown', () => {
    const clear = jest.spyOn(global, 'clearInterval');
    // eslint-disable-next-line global-require
    require('../src/utils/oauth-rate-limit').stopIntervals();
    expect(clear).toHaveBeenCalledTimes(1);
  });
});
