// Tests for src/utils/oauth-state.js — the shared secret-resolution +
// HMAC signer behind both OAuth state flows (the GitHub OAuth binding
// in commands.js and the qURL OAuth setup state in
// utils/qurl-oauth-state.js). Covers the contract both flows rely on:
//   1. precedence — first truthy secretConfigKeys entry wins, then
//      GITHUB_CLIENT_SECRET, then the jest-only random fallback
//   2. MIN_STATE_SECRET_LENGTH — dedicated AND fallback secrets under
//      32 chars are rejected. This is the drift the extraction closed:
//      the GitHub flow's resolver used to accept a 4-char
//      OAUTH_STATE_SECRET silently.
//   3. sign/verify — round-trip, tamper rejection, and no-throw on
//      malformed signature input
//   4. test-harness gating — outside jest/CI the resolver throws
//      instead of silently minting with the random fallback

jest.mock('../src/logger', () => ({
  info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(), audit: jest.fn(),
}));

const crypto = require('crypto');

const SECRET_ENV_KEYS = ['OAUTH_STATE_SECRET', 'QURL_OAUTH_STATE_SECRET', 'GITHUB_CLIENT_SECRET'];

// Load a fresh copy of the module with ONLY the given secret env vars
// visible to config.js's process.env snapshot, then restore the prior
// env. Restoring immediately is safe because the signer resolves
// secrets from the config snapshot captured inside the isolate — later
// env changes are invisible to it, which mirrors the production
// semantic (env is fixed at boot). The logger is captured from the
// same isolate so warn-count assertions see the instance the module
// actually calls.
function loadSigner(envSecrets, { flowLabel = 'test OAuth state', secretConfigKeys = ['OAUTH_STATE_SECRET'] } = {}) {
  const saved = {};
  for (const key of SECRET_ENV_KEYS) {
    saved[key] = process.env[key];
    delete process.env[key];
  }
  Object.assign(process.env, envSecrets);
  let out;
  try {
    jest.isolateModules(() => {
      const logger = require('../src/logger');
      const mod = require('../src/utils/oauth-state');
      out = {
        signer: mod.createStateSigner({ flowLabel, secretConfigKeys }),
        createStateSigner: mod.createStateSigner,
        MIN_STATE_SECRET_LENGTH: mod.MIN_STATE_SECRET_LENGTH,
        logger,
      };
    });
  } finally {
    for (const key of SECRET_ENV_KEYS) {
      if (saved[key] === undefined) delete process.env[key];
      else process.env[key] = saved[key];
    }
  }
  return out;
}

function hmacHex(secret, data) {
  return crypto.createHmac('sha256', secret).update(data).digest('hex');
}

describe('oauth-state createStateSigner', () => {
  it('pins MIN_STATE_SECRET_LENGTH at 32 (round-9 #4 floor)', () => {
    const { MIN_STATE_SECRET_LENGTH } = loadSigner({});
    expect(MIN_STATE_SECRET_LENGTH).toBe(32);
  });

  describe('constructor validation', () => {
    it('rejects a missing flowLabel', () => {
      const { createStateSigner } = loadSigner({});
      expect(() => createStateSigner({ secretConfigKeys: ['OAUTH_STATE_SECRET'] })).toThrow(TypeError);
      expect(() => createStateSigner({ flowLabel: '', secretConfigKeys: ['OAUTH_STATE_SECRET'] })).toThrow(/flowLabel/);
    });

    it('rejects missing or empty secretConfigKeys', () => {
      const { createStateSigner } = loadSigner({});
      expect(() => createStateSigner({ flowLabel: 'x' })).toThrow(/secretConfigKeys/);
      expect(() => createStateSigner({ flowLabel: 'x', secretConfigKeys: [] })).toThrow(/secretConfigKeys/);
    });
  });

  describe('secret precedence', () => {
    it('uses the first config key when set', () => {
      const { signer } = loadSigner(
        { QURL_OAUTH_STATE_SECRET: 'q'.repeat(64), OAUTH_STATE_SECRET: 's'.repeat(64) },
        { secretConfigKeys: ['QURL_OAUTH_STATE_SECRET', 'OAUTH_STATE_SECRET'] },
      );
      expect(signer.sign('data')).toBe(hmacHex('q'.repeat(64), 'data'));
    });

    it('falls through to the next key when the first is unset', () => {
      const { signer } = loadSigner(
        { OAUTH_STATE_SECRET: 's'.repeat(64) },
        { secretConfigKeys: ['QURL_OAUTH_STATE_SECRET', 'OAUTH_STATE_SECRET'] },
      );
      expect(signer.sign('data')).toBe(hmacHex('s'.repeat(64), 'data'));
    });

    it('falls back to GITHUB_CLIENT_SECRET when no dedicated key is set', () => {
      const { signer } = loadSigner({ GITHUB_CLIENT_SECRET: 'g'.repeat(64) });
      expect(signer.sign('data')).toBe(hmacHex('g'.repeat(64), 'data'));
    });
  });

  describe('minimum secret length', () => {
    it('refuses a dedicated secret under 32 chars on sign AND verify', () => {
      const { signer } = loadSigner({ OAUTH_STATE_SECRET: 'shrt' });
      expect(() => signer.sign('data')).toThrow(/shorter than 32 chars \(got 4\)/);
      expect(() => signer.verify('data', 'a'.repeat(64))).toThrow(/shorter than 32 chars/);
    });

    it('accepts a dedicated secret of exactly 32 chars', () => {
      const { signer } = loadSigner({ OAUTH_STATE_SECRET: 'x'.repeat(32) });
      expect(signer.verify('data', signer.sign('data'))).toBe(true);
    });

    it('refuses a GITHUB_CLIENT_SECRET fallback under 32 chars', () => {
      const { signer } = loadSigner({ GITHUB_CLIENT_SECRET: 'test-client-secret' });
      expect(() => signer.sign('data')).toThrow(/GITHUB_CLIENT_SECRET fallback is shorter than 32 chars/);
    });

    it('names the flow (brand spelling preserved) in the refusal message', () => {
      const { signer } = loadSigner(
        { QURL_OAUTH_STATE_SECRET: 'shrt' },
        { flowLabel: 'qURL OAuth state', secretConfigKeys: ['QURL_OAUTH_STATE_SECRET', 'OAUTH_STATE_SECRET'] },
      );
      expect(() => signer.sign('data')).toThrow(/^Refusing to mint qURL OAuth state:/);
    });
  });

  describe('sign/verify', () => {
    it('round-trips', () => {
      const { signer } = loadSigner({ OAUTH_STATE_SECRET: '0'.repeat(64) });
      const sig = signer.sign('user-1:nonce-abc');
      expect(signer.verify('user-1:nonce-abc', sig)).toBe(true);
    });

    it('rejects a signature over different data', () => {
      const { signer } = loadSigner({ OAUTH_STATE_SECRET: '0'.repeat(64) });
      const sig = signer.sign('user-1:nonce-abc');
      expect(signer.verify('user-2:nonce-abc', sig)).toBe(false);
    });

    it('rejects a tampered signature', () => {
      const { signer } = loadSigner({ OAUTH_STATE_SECRET: '0'.repeat(64) });
      const sig = signer.sign('data');
      const flipped = (sig[0] === '0' ? '1' : '0') + sig.slice(1);
      expect(signer.verify('data', flipped)).toBe(false);
    });

    it('returns false (no throw) on malformed signature input', () => {
      const { signer } = loadSigner({ OAUTH_STATE_SECRET: '0'.repeat(64) });
      expect(signer.verify('data', 'z'.repeat(64))).toBe(false); // non-hex
      expect(signer.verify('data', 'abc')).toBe(false); // truncated
      expect(signer.verify('data', '')).toBe(false);
    });
  });

  describe('test-harness fallback', () => {
    it('round-trips via the per-signer random fallback when nothing is configured, warning once', () => {
      const { signer, logger } = loadSigner({});
      const sig = signer.sign('data');
      expect(signer.verify('data', sig)).toBe(true);
      expect(logger.warn).toHaveBeenCalledTimes(1);
      expect(logger.warn.mock.calls[0][0]).toContain('random test fallback');
      signer.sign('more-data');
      expect(logger.warn).toHaveBeenCalledTimes(1); // warn-once, not per call
    });

    it('gives each signer an independent fallback secret (flows cannot cross-verify)', () => {
      const { createStateSigner } = loadSigner({});
      const a = createStateSigner({ flowLabel: 'flow a', secretConfigKeys: ['OAUTH_STATE_SECRET'] });
      const b = createStateSigner({ flowLabel: 'flow b', secretConfigKeys: ['OAUTH_STATE_SECRET'] });
      const sig = a.sign('data');
      expect(a.verify('data', sig)).toBe(true);
      expect(b.verify('data', sig)).toBe(false);
    });

    it('throws outside the jest/CI harness instead of minting with the fallback', () => {
      const { signer } = loadSigner({});
      // The harness gate reads the live env at call time (unlike the
      // secrets, which come from the config snapshot) — simulate a
      // deployed process that merely has NODE_ENV=test by accident.
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
