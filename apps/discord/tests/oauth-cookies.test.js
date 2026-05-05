// Unit tests for src/utils/oauth-cookies.js — locks the path-must-
// match invariant on clearQurlOAuthCookie so a refactor that drops
// the path arg silently bypasses cookie clearing in the browser.
//
// The happy-path /oauth/qurl/callback test in qurl-oauth.test.js
// already pins this end-to-end via header inspection; this file
// adds a unit-level regression fence so the helper itself can't
// drift.

const {
  QURL_OAUTH_SESSION_COOKIE,
  QURL_OAUTH_COOKIE_PATH,
  setQurlOAuthCookie,
  clearQurlOAuthCookie,
} = require('../src/utils/oauth-cookies');

function fakeRes() {
  return {
    cookieCalls: [],
    clearCookieCalls: [],
    cookie(name, value, opts) { this.cookieCalls.push({ name, value, opts }); },
    clearCookie(name, opts) { this.clearCookieCalls.push({ name, opts }); },
  };
}

describe('utils/oauth-cookies', () => {
  describe('setQurlOAuthCookie', () => {
    it('sets the canonical cookie shape (HttpOnly, SameSite=Lax, Secure-when-HTTPS, path=/oauth)', () => {
      const res = fakeRes();
      setQurlOAuthCookie(res, { protocol: 'https' }, 'state-token-abc');
      expect(res.cookieCalls).toHaveLength(1);
      const call = res.cookieCalls[0];
      expect(call.name).toBe(QURL_OAUTH_SESSION_COOKIE);
      expect(call.value).toBe('state-token-abc');
      expect(call.opts).toEqual({
        httpOnly: true,
        secure: true,
        sameSite: 'lax',
        maxAge: 5 * 60 * 1000,
        path: QURL_OAUTH_COOKIE_PATH,
      });
    });

    it('sets secure=false when behind plain HTTP (dev)', () => {
      const res = fakeRes();
      setQurlOAuthCookie(res, { protocol: 'http' }, 'state-token-abc');
      expect(res.cookieCalls[0].opts.secure).toBe(false);
    });
  });

  describe('clearQurlOAuthCookie', () => {
    it('always passes Path=/oauth so the browser actually forgets the cookie', () => {
      // Path-mismatch on clearCookie is silently a no-op — the browser
      // keeps the cookie alive until TTL. Pinning the path arg here
      // prevents a refactor that drops it from breaking one-shot
      // binding semantics.
      const res = fakeRes();
      clearQurlOAuthCookie(res);
      expect(res.clearCookieCalls).toHaveLength(1);
      const call = res.clearCookieCalls[0];
      expect(call.name).toBe(QURL_OAUTH_SESSION_COOKIE);
      expect(call.opts).toEqual({ path: QURL_OAUTH_COOKIE_PATH });
    });
  });
});
