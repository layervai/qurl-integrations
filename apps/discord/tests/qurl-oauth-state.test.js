// Tests for src/utils/qurl-oauth-state.js — the HMAC-signed state token
// the qURL OAuth flow rides through Auth0 (kept out of cookies because
// the bot's HTTP server is stateless across deploys).
//
// Cover the four invariants the callback relies on:
//   1. round-trip — sign then verify returns the original payload
//   2. tamper resistance — flipping any byte invalidates the signature
//   3. expiry — tokens older than STATE_TTL_SECONDS reject
//   4. cross-purpose — a state minted for a different `kind` rejects
//      (defense-in-depth against replaying a GitHub-OAuth state here)

// Stable shared secret across the qurl-oauth-state and qurl-oauth route
// suites. Both files set the SAME value so a Jest worker that loads the
// state module from one test and uses it from the other doesn't see a
// signature mismatch from the per-process cached fallback in the module
// (cross-test leakage flagged in PR #177 review).
process.env.OAUTH_STATE_SECRET = '0'.repeat(64);

const crypto = require('crypto');
const { signQurlOAuthState, verifyQurlOAuthState, STATE_TTL_SECONDS } = require('../src/utils/qurl-oauth-state');

describe('qurl-oauth-state', () => {
  describe('round-trip', () => {
    it('signs and verifies a valid state', () => {
      const state = signQurlOAuthState('guild-1', 'user-2');
      const result = verifyQurlOAuthState(state);
      expect(result.ok).toBe(true);
      expect(result.payload.guildId).toBe('guild-1');
      expect(result.payload.discordUserId).toBe('user-2');
      expect(result.payload.expirySec).toBeGreaterThan(Math.floor(Date.now() / 1000));
      expect(result.payload.expirySec - Math.floor(Date.now() / 1000)).toBeLessThanOrEqual(STATE_TTL_SECONDS);
    });

    it('two states for the same inputs differ (nonce uniqueness)', () => {
      const a = signQurlOAuthState('guild-1', 'user-2');
      const b = signQurlOAuthState('guild-1', 'user-2');
      expect(a).not.toBe(b);
    });
  });

  describe('input validation', () => {
    it('throws on empty guildId', () => {
      expect(() => signQurlOAuthState('', 'user-2')).toThrow(/guildId/);
    });
    it('throws on empty discordUserId', () => {
      expect(() => signQurlOAuthState('guild-1', '')).toThrow(/discordUserId/);
    });
    it('throws on non-string guildId', () => {
      expect(() => signQurlOAuthState(123, 'user-2')).toThrow(/guildId/);
    });
  });

  describe('tamper resistance', () => {
    it('rejects when the payload is mutated', () => {
      const state = signQurlOAuthState('guild-1', 'user-2');
      const [encoded, sig] = state.split('.');
      // Decode, swap guildId, re-encode without re-signing — sig should now mismatch.
      const decoded = JSON.parse(Buffer.from(encoded.replace(/-/g, '+').replace(/_/g, '/') + '==', 'base64').toString('utf8'));
      decoded.g = 'guild-evil';
      const tamperedEncoded = Buffer.from(JSON.stringify(decoded)).toString('base64')
        .replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
      const result = verifyQurlOAuthState(`${tamperedEncoded}.${sig}`);
      expect(result.ok).toBe(false);
      expect(result.reason).toBe('sig_mismatch');
    });

    it('rejects when the signature is mutated', () => {
      const state = signQurlOAuthState('guild-1', 'user-2');
      const [encoded, sig] = state.split('.');
      // Flip the last hex char of the sig (preserves length + charset).
      const lastChar = sig[sig.length - 1];
      const flippedChar = lastChar === '0' ? '1' : '0';
      const mutatedSig = sig.slice(0, -1) + flippedChar;
      const result = verifyQurlOAuthState(`${encoded}.${mutatedSig}`);
      expect(result.ok).toBe(false);
      expect(result.reason).toBe('sig_mismatch');
    });

    it('rejects malformed inputs (single segment, wrong charset)', () => {
      expect(verifyQurlOAuthState('').ok).toBe(false);
      expect(verifyQurlOAuthState('not-a-state').ok).toBe(false);
      expect(verifyQurlOAuthState('seg1.seg2.seg3').ok).toBe(false);
      expect(verifyQurlOAuthState(null).ok).toBe(false);
      expect(verifyQurlOAuthState(undefined).ok).toBe(false);
    });
  });

  describe('expiry', () => {
    it('rejects an expired token', async () => {
      const realDateNow = Date.now;
      try {
        // Mint at t=0
        Date.now = () => 0;
        const state = signQurlOAuthState('guild-1', 'user-2');
        // Verify at t=now+TTL+1 sec — should reject as expired.
        Date.now = () => (STATE_TTL_SECONDS + 1) * 1000;
        const result = verifyQurlOAuthState(state);
        expect(result.ok).toBe(false);
        expect(result.reason).toBe('expired');
      } finally {
        Date.now = realDateNow;
      }
    });

    it('accepts a state right at the TTL boundary minus 1 sec', () => {
      const realDateNow = Date.now;
      try {
        Date.now = () => 0;
        const state = signQurlOAuthState('guild-1', 'user-2');
        Date.now = () => (STATE_TTL_SECONDS - 1) * 1000;
        const result = verifyQurlOAuthState(state);
        expect(result.ok).toBe(true);
      } finally {
        Date.now = realDateNow;
      }
    });
  });

  describe('cross-purpose forgery', () => {
    it('rejects a payload with a wrong `kind` field (defense vs GitHub-OAuth state replay)', () => {
      // Construct a GitHub-OAuth-shaped payload and sign it with the same
      // secret — the verifier must reject because `k` is not 'qurl-oauth'.
      const payload = { k: 'github-oauth', g: 'guild-1', u: 'user-2', n: 'abc', e: Math.floor(Date.now() / 1000) + 60 };
      const encoded = Buffer.from(JSON.stringify(payload)).toString('base64')
        .replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
      const sig = crypto.createHmac('sha256', process.env.OAUTH_STATE_SECRET).update(encoded).digest('hex');
      const result = verifyQurlOAuthState(`${encoded}.${sig}`);
      expect(result.ok).toBe(false);
      expect(result.reason).toBe('wrong_kind');
    });
  });

  describe('secret precedence (#184)', () => {
    // Pins QURL_OAUTH_STATE_SECRET > OAUTH_STATE_SECRET migration chain.
    // Uses jest.isolateModulesAsync so the cached `_warnedFallback`
    // and `_testFallbackSecret` in the module don't leak across tests.
    it('signs with QURL_OAUTH_STATE_SECRET while accepting OAUTH_STATE_SECRET during cutover', async () => {
      const savedQurl = process.env.QURL_OAUTH_STATE_SECRET;
      const savedShared = process.env.OAUTH_STATE_SECRET;
      process.env.QURL_OAUTH_STATE_SECRET = 'q'.repeat(64);
      process.env.OAUTH_STATE_SECRET = 's'.repeat(64);
      try {
        await jest.isolateModulesAsync(async () => {
          // eslint-disable-next-line global-require
          const { signQurlOAuthState: sign, verifyQurlOAuthState: verify } = require('../src/utils/qurl-oauth-state');
          const state = sign('guild-1', 'user-2');
          // The signature MUST be HMAC over the QURL_OAUTH_STATE_SECRET,
          // not OAUTH_STATE_SECRET.
          expect(verify(state).ok).toBe(true);
          const [encoded] = state.split('.');
          const primarySig = crypto.createHmac('sha256', process.env.QURL_OAUTH_STATE_SECRET)
            .update(encoded)
            .digest('hex');
          expect(state.endsWith(`.${primarySig}`)).toBe(true);

          // Dual-read migration: old states signed with the shared secret
          // still pass while OAUTH_STATE_SECRET is configured, then stop
          // passing as soon as ops removes that legacy env var.
          const legacySig = crypto.createHmac('sha256', process.env.OAUTH_STATE_SECRET).update(encoded).digest('hex');
          const legacyState = `${encoded}.${legacySig}`;
          expect(verify(legacyState).ok).toBe(true);
          delete process.env.OAUTH_STATE_SECRET;
          expect(verify(legacyState).ok).toBe(false);
        });
      } finally {
        if (savedQurl === undefined) delete process.env.QURL_OAUTH_STATE_SECRET;
        else process.env.QURL_OAUTH_STATE_SECRET = savedQurl;
        process.env.OAUTH_STATE_SECRET = savedShared;
      }
    });

    it('checks every configured secret even when the primary secret matches', async () => {
      const savedQurl = process.env.QURL_OAUTH_STATE_SECRET;
      const savedShared = process.env.OAUTH_STATE_SECRET;
      process.env.QURL_OAUTH_STATE_SECRET = 'q'.repeat(64);
      process.env.OAUTH_STATE_SECRET = 's'.repeat(64);
      try {
        await jest.isolateModulesAsync(async () => {
          // eslint-disable-next-line global-require
          const { signQurlOAuthState: sign, verifyQurlOAuthState: verify } = require('../src/utils/qurl-oauth-state');
          const state = sign('guild-1', 'user-2');
          const createHmacSpy = jest.spyOn(crypto, 'createHmac');
          try {
            expect(verify(state).ok).toBe(true);
            expect(createHmacSpy).toHaveBeenCalledTimes(2);
          } finally {
            createHmacSpy.mockRestore();
          }
        });
      } finally {
        if (savedQurl === undefined) delete process.env.QURL_OAUTH_STATE_SECRET;
        else process.env.QURL_OAUTH_STATE_SECRET = savedQurl;
        process.env.OAUTH_STATE_SECRET = savedShared;
      }
    });

    it('falls back to OAUTH_STATE_SECRET when QURL_OAUTH_STATE_SECRET is unset', async () => {
      const savedQurl = process.env.QURL_OAUTH_STATE_SECRET;
      delete process.env.QURL_OAUTH_STATE_SECRET;
      // Keep the shared secret set (the file-level default).
      try {
        await jest.isolateModulesAsync(async () => {
          // eslint-disable-next-line global-require
          const { signQurlOAuthState: sign, verifyQurlOAuthState: verify } = require('../src/utils/qurl-oauth-state');
          const state = sign('guild-1', 'user-2');
          expect(verify(state).ok).toBe(true);
        });
      } finally {
        if (savedQurl !== undefined) process.env.QURL_OAUTH_STATE_SECRET = savedQurl;
      }
    });

    it('treats the Terraform SSM placeholder as unset', async () => {
      const savedQurl = process.env.QURL_OAUTH_STATE_SECRET;
      const savedShared = process.env.OAUTH_STATE_SECRET;
      process.env.QURL_OAUTH_STATE_SECRET = 'PLACEHOLDER';
      process.env.OAUTH_STATE_SECRET = 's'.repeat(64);
      try {
        await jest.isolateModulesAsync(async () => {
          // eslint-disable-next-line global-require
          const { signQurlOAuthState: sign, verifyQurlOAuthState: verify } = require('../src/utils/qurl-oauth-state');
          const state = sign('guild-1', 'user-2');
          const [encoded] = state.split('.');
          const sharedSig = crypto.createHmac('sha256', process.env.OAUTH_STATE_SECRET)
            .update(encoded)
            .digest('hex');

          expect(state.endsWith(`.${sharedSig}`)).toBe(true);
          expect(verify(state).ok).toBe(true);
        });
      } finally {
        if (savedQurl === undefined) delete process.env.QURL_OAUTH_STATE_SECRET;
        else process.env.QURL_OAUTH_STATE_SECRET = savedQurl;
        if (savedShared === undefined) delete process.env.OAUTH_STATE_SECRET;
        else process.env.OAUTH_STATE_SECRET = savedShared;
      }
    });

    it('ignores a short legacy secret when a dedicated qURL secret is active', async () => {
      const savedQurl = process.env.QURL_OAUTH_STATE_SECRET;
      const savedShared = process.env.OAUTH_STATE_SECRET;
      const warnSpy = jest.spyOn(console, 'warn').mockImplementation(() => {});
      process.env.QURL_OAUTH_STATE_SECRET = 'q'.repeat(64);
      process.env.OAUTH_STATE_SECRET = 'short';
      try {
        await jest.isolateModulesAsync(async () => {
          // eslint-disable-next-line global-require
          const { signQurlOAuthState: sign, verifyQurlOAuthState: verify } = require('../src/utils/qurl-oauth-state');
          const state = sign('guild-1', 'user-2');
          expect(verify(state).ok).toBe(true);

          const [encoded] = state.split('.');
          const legacySig = crypto.createHmac('sha256', process.env.OAUTH_STATE_SECRET)
            .update(encoded)
            .digest('hex');
          expect(verify(`${encoded}.${legacySig}`).ok).toBe(false);
        });
        expect(warnSpy).toHaveBeenCalledWith(expect.stringContaining('Ignoring OAUTH_STATE_SECRET'));
      } finally {
        warnSpy.mockRestore();
        if (savedQurl === undefined) delete process.env.QURL_OAUTH_STATE_SECRET;
        else process.env.QURL_OAUTH_STATE_SECRET = savedQurl;
        if (savedShared === undefined) delete process.env.OAUTH_STATE_SECRET;
        else process.env.OAUTH_STATE_SECRET = savedShared;
      }
    });

    it('returns config_error instead of throwing when configured secrets are unusable', async () => {
      const savedQurl = process.env.QURL_OAUTH_STATE_SECRET;
      const savedShared = process.env.OAUTH_STATE_SECRET;
      process.env.QURL_OAUTH_STATE_SECRET = 'short';
      delete process.env.OAUTH_STATE_SECRET;
      try {
        await jest.isolateModulesAsync(async () => {
          // eslint-disable-next-line global-require
          const { verifyQurlOAuthState: verify } = require('../src/utils/qurl-oauth-state');
          const payload = {
            k: 'qurl-oauth',
            g: 'guild-1',
            u: 'user-2',
            n: 'nonce',
            e: Math.floor(Date.now() / 1000) + 60,
          };
          const encoded = Buffer.from(JSON.stringify(payload)).toString('base64')
            .replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
          const sig = crypto.createHmac('sha256', 'short').update(encoded).digest('hex');
          expect(() => verify(`${encoded}.${sig}`)).not.toThrow();
          expect(verify(`${encoded}.${sig}`)).toEqual({ ok: false, reason: 'config_error' });
        });
      } finally {
        if (savedQurl === undefined) delete process.env.QURL_OAUTH_STATE_SECRET;
        else process.env.QURL_OAUTH_STATE_SECRET = savedQurl;
        if (savedShared === undefined) delete process.env.OAUTH_STATE_SECRET;
        else process.env.OAUTH_STATE_SECRET = savedShared;
      }
    });
  });
});
