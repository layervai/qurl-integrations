// Tests for src/utils/oauth-state.js — the shared secret-resolution +
// HMAC signer behind both OAuth state flows (the GitHub OAuth binding
// in commands.js and the qURL OAuth setup state in
// utils/qurl-oauth-state.js). Covers the contract both flows rely on:
//   1. precedence — first truthy secretConfigKeys entry wins, then
//      GITHUB_CLIENT_SECRET, then the jest-only random fallback
//   2. MIN_STATE_SECRET_LENGTH — whichever key wins the resolution is
//      rejected under 32 chars. This is the drift the extraction
//      closed: the GitHub flow's resolver used to accept a 4-char
//      OAUTH_STATE_SECRET silently.
//   3. sign/verify — round-trip, tamper rejection, and no-throw on
//      malformed signature input
//   4. test-harness gating — outside jest/CI the resolver throws
//      instead of silently minting with the random fallback
//
// The signer reads secrets from the config snapshot lazily on every
// sign/verify (a documented affordance for exactly this mock shape),
// so the suite mutates a plain config mock per test — no
// process.env fiddling or module isolation needed except in the
// harness-gate test, which exercises the one live-env read.

jest.mock('../src/config', () => ({}));
jest.mock('../src/logger', () => ({
  info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(), audit: jest.fn(),
}));

const crypto = require('crypto');
const config = require('../src/config');
const logger = require('../src/logger');
const { createStateSigner, MIN_STATE_SECRET_LENGTH } = require('../src/utils/oauth-state');

// Fresh signer per test: warn-once state and the random test-fallback
// secret are per-signer, so nothing leaks across tests.
function makeSigner(overrides = {}) {
  return createStateSigner({
    flowLabel: 'test OAuth state',
    secretConfigKeys: ['OAUTH_STATE_SECRET'],
    ...overrides,
  });
}

function hmacHex(secret, data) {
  return crypto.createHmac('sha256', secret).update(data).digest('hex');
}

beforeEach(() => {
  for (const key of Object.keys(config)) delete config[key];
  jest.clearAllMocks();
});

describe('oauth-state createStateSigner', () => {
  it('pins MIN_STATE_SECRET_LENGTH at 32 (round-9 #4 floor)', () => {
    expect(MIN_STATE_SECRET_LENGTH).toBe(32);
  });

  it('rejects missing or empty secretConfigKeys at construction', () => {
    // An empty list would silently resolve straight to
    // GITHUB_CLIENT_SECRET — a precedence change, not a cosmetic bug.
    expect(() => createStateSigner({ flowLabel: 'x' })).toThrow(/secretConfigKeys/);
    expect(() => createStateSigner({ flowLabel: 'x', secretConfigKeys: [] })).toThrow(/secretConfigKeys/);
  });

  describe('secret precedence', () => {
    it('uses the first config key when set', () => {
      config.QURL_OAUTH_STATE_SECRET = 'q'.repeat(64);
      config.OAUTH_STATE_SECRET = 's'.repeat(64);
      const signer = makeSigner({ secretConfigKeys: ['QURL_OAUTH_STATE_SECRET', 'OAUTH_STATE_SECRET'] });
      expect(signer.sign('data')).toBe(hmacHex('q'.repeat(64), 'data'));
    });

    it('falls through to the next key when the first is unset', () => {
      config.OAUTH_STATE_SECRET = 's'.repeat(64);
      const signer = makeSigner({ secretConfigKeys: ['QURL_OAUTH_STATE_SECRET', 'OAUTH_STATE_SECRET'] });
      expect(signer.sign('data')).toBe(hmacHex('s'.repeat(64), 'data'));
    });

    it('falls back to GITHUB_CLIENT_SECRET when no dedicated key is set', () => {
      config.GITHUB_CLIENT_SECRET = 'g'.repeat(64);
      expect(makeSigner().sign('data')).toBe(hmacHex('g'.repeat(64), 'data'));
    });

    it('resolves lazily per call — a rotated secret invalidates states signed with the old one', () => {
      config.OAUTH_STATE_SECRET = 'a'.repeat(64);
      const signer = makeSigner();
      const sig = signer.sign('data');
      config.OAUTH_STATE_SECRET = 'b'.repeat(64);
      expect(signer.verify('data', sig)).toBe(false);
      expect(signer.verify('data', signer.sign('data'))).toBe(true);
    });
  });

  describe('minimum secret length', () => {
    it('refuses a dedicated secret under 32 chars on sign AND verify, naming the key', () => {
      config.OAUTH_STATE_SECRET = 'shrt';
      const signer = makeSigner();
      expect(() => signer.sign('data')).toThrow(/OAUTH_STATE_SECRET is shorter than 32 chars \(got 4\)/);
      expect(() => signer.verify('data', 'a'.repeat(64))).toThrow(/shorter than 32 chars/);
    });

    it('accepts a dedicated secret of exactly 32 chars', () => {
      config.OAUTH_STATE_SECRET = 'x'.repeat(32);
      const signer = makeSigner();
      expect(signer.verify('data', signer.sign('data'))).toBe(true);
    });

    it('refuses a GITHUB_CLIENT_SECRET fallback under 32 chars, naming the key', () => {
      config.GITHUB_CLIENT_SECRET = 'test-client-secret';
      expect(() => makeSigner().sign('data')).toThrow(/GITHUB_CLIENT_SECRET is shorter than 32 chars/);
    });

    it('names the flow (brand spelling preserved) in the refusal message', () => {
      config.QURL_OAUTH_STATE_SECRET = 'shrt';
      const signer = makeSigner({
        flowLabel: 'qURL OAuth state',
        secretConfigKeys: ['QURL_OAUTH_STATE_SECRET', 'OAUTH_STATE_SECRET'],
      });
      expect(() => signer.sign('data')).toThrow(/^Refusing to mint qURL OAuth state:/);
    });
  });

  describe('sign/verify', () => {
    beforeEach(() => {
      config.OAUTH_STATE_SECRET = '0'.repeat(64);
    });

    it('round-trips', () => {
      const signer = makeSigner();
      const sig = signer.sign('user-1:nonce-abc');
      expect(signer.verify('user-1:nonce-abc', sig)).toBe(true);
    });

    it('rejects a signature over different data', () => {
      const signer = makeSigner();
      const sig = signer.sign('user-1:nonce-abc');
      expect(signer.verify('user-2:nonce-abc', sig)).toBe(false);
    });

    it('rejects a tampered signature', () => {
      const signer = makeSigner();
      const sig = signer.sign('data');
      const flipped = (sig[0] === '0' ? '1' : '0') + sig.slice(1);
      expect(signer.verify('data', flipped)).toBe(false);
    });

    it('returns false (no throw) on malformed signature input', () => {
      const signer = makeSigner();
      expect(signer.verify('data', 'z'.repeat(64))).toBe(false); // non-hex
      expect(signer.verify('data', 'abc')).toBe(false); // truncated
      expect(signer.verify('data', '')).toBe(false);
    });
  });

  describe('test-harness fallback', () => {
    it('round-trips via the per-signer random fallback when nothing is configured, warning once', () => {
      const signer = makeSigner();
      const sig = signer.sign('data');
      expect(signer.verify('data', sig)).toBe(true);
      expect(logger.warn).toHaveBeenCalledTimes(1);
      expect(logger.warn.mock.calls[0][0]).toContain('random test fallback');
      signer.sign('more-data');
      expect(logger.warn).toHaveBeenCalledTimes(1); // warn-once, not per call
    });

    it('gives each signer an independent fallback secret (flows cannot cross-verify)', () => {
      const a = makeSigner({ flowLabel: 'flow a' });
      const b = makeSigner({ flowLabel: 'flow b' });
      const sig = a.sign('data');
      expect(a.verify('data', sig)).toBe(true);
      expect(b.verify('data', sig)).toBe(false);
    });

    it('throws outside the jest/CI harness instead of minting with the fallback', () => {
      const signer = makeSigner();
      // The harness gate reads the live env at call time (unlike the
      // secrets, which come through config) — simulate a deployed
      // process that merely has NODE_ENV=test by accident.
      const savedWorker = process.env.JEST_WORKER_ID;
      const savedCI = process.env.CI;
      delete process.env.JEST_WORKER_ID;
      delete process.env.CI;
      try {
        expect(() => signer.sign('data')).toThrow(
          /Refusing to mint test OAuth state: OAUTH_STATE_SECRET or GITHUB_CLIENT_SECRET must be set\./,
        );
      } finally {
        if (savedWorker !== undefined) process.env.JEST_WORKER_ID = savedWorker;
        if (savedCI !== undefined) process.env.CI = savedCI;
      }
    });
  });
});
