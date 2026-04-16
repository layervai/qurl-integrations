/**
 * Tests for the TEST_MODE_EXPIRY_SCALE env var gating and behavior.
 * The scale must:
 *   - Be a no-op in production (NODE_ENV=production)
 *   - Only apply in allowed envs: development, test, staging, e2e
 *   - Only accept values in (0, 1]
 *   - Default to 1 if unset, invalid, or out of range
 */

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
}));
jest.mock('../src/config', () => ({
  config: { QURL_SEND_MAX_RECIPIENTS: 50, QURL_SEND_COOLDOWN_MS: 30000, CONNECTOR_URL: '', QURL_ENDPOINT: '', GOOGLE_MAPS_API_KEY: '' },
}));
jest.mock('../src/database', () => ({
  recordQURLSend: jest.fn(),
  saveSendConfig: jest.fn(),
  getSendConfig: jest.fn(),
  getRecentSends: jest.fn(),
  getResourceIdsForSend: jest.fn(),
}));

const { _test } = require('../src/commands');

describe('expiryScale gating', () => {
  const ORIGINAL_ENV = { ...process.env };

  afterEach(() => {
    process.env = { ...ORIGINAL_ENV };
  });

  it('returns 1 when NODE_ENV=production even if scale set', () => {
    process.env.NODE_ENV = 'production';
    process.env.TEST_MODE_EXPIRY_SCALE = '0.001';
    expect(_test.expiryScale()).toBe(1);
  });

  it('returns 1 in unknown NODE_ENV', () => {
    process.env.NODE_ENV = 'canary';
    process.env.TEST_MODE_EXPIRY_SCALE = '0.001';
    expect(_test.expiryScale()).toBe(1);
  });

  it('returns 1 when scale not set in allowed env', () => {
    process.env.NODE_ENV = 'test';
    delete process.env.TEST_MODE_EXPIRY_SCALE;
    expect(_test.expiryScale()).toBe(1);
  });

  it('returns 1 for non-numeric scale', () => {
    process.env.NODE_ENV = 'test';
    process.env.TEST_MODE_EXPIRY_SCALE = 'abc';
    expect(_test.expiryScale()).toBe(1);
  });

  it('returns 1 for negative scale', () => {
    process.env.NODE_ENV = 'test';
    process.env.TEST_MODE_EXPIRY_SCALE = '-0.5';
    expect(_test.expiryScale()).toBe(1);
  });

  it('returns 1 for scale > 1 (would extend expiry — rejected)', () => {
    process.env.NODE_ENV = 'test';
    process.env.TEST_MODE_EXPIRY_SCALE = '2';
    expect(_test.expiryScale()).toBe(1);
  });

  it('applies scale for allowed envs and valid values', () => {
    for (const env of ['development', 'test', 'staging', 'e2e']) {
      process.env.NODE_ENV = env;
      process.env.TEST_MODE_EXPIRY_SCALE = '0.001';
      expect(_test.expiryScale()).toBe(0.001);
    }
  });

  it('expiryToISO respects scale', () => {
    process.env.NODE_ENV = 'test';
    process.env.TEST_MODE_EXPIRY_SCALE = '0.001';
    const before = Date.now();
    const iso = _test.expiryToISO('1h');
    const delta = new Date(iso).getTime() - before;
    // 1h * 0.001 = 3.6s; allow 1s slack
    expect(delta).toBeGreaterThan(3000);
    expect(delta).toBeLessThan(5000);
  });

  it('expiryToISO ignores scale in production', () => {
    process.env.NODE_ENV = 'production';
    process.env.TEST_MODE_EXPIRY_SCALE = '0.001';
    const before = Date.now();
    const iso = _test.expiryToISO('1h');
    const delta = new Date(iso).getTime() - before;
    // 1h, no scaling
    expect(delta).toBeGreaterThan(3_590_000);
    expect(delta).toBeLessThan(3_610_000);
  });
});
