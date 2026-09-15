
const {
  QURL_OAUTH_SESSION_COOKIE,
  QURL_OAUTH_PKCE_COOKIE,
  QURL_OAUTH_COOKIE_PATH,
  setQurlOAuthCookie,
  setQurlOAuthPkceCookie,
  clearQurlOAuthCookie,
  clearQurlOAuthPkceCookie,
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
    it('sets the canonical cookie shape (HttpOnly, SameSite=Lax, Secure-when-HTTPS, path=/oauth/qurl)', () => {
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

    it('sets the PKCE verifier cookie with the same browser/session scope', () => {
      const res = fakeRes();
      setQurlOAuthPkceCookie(res, { protocol: 'https' }, 'verifier-abc');
      expect(res.cookieCalls).toHaveLength(1);
      const call = res.cookieCalls[0];
      expect(call.name).toBe(QURL_OAUTH_PKCE_COOKIE);
      expect(call.value).toBe('verifier-abc');
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
    it('always passes Path=/oauth/qurl so the browser actually forgets the cookie', () => {
      const res = fakeRes();
      clearQurlOAuthCookie(res);
      expect(res.clearCookieCalls).toHaveLength(1);
      const call = res.clearCookieCalls[0];
      expect(call.name).toBe(QURL_OAUTH_SESSION_COOKIE);
      expect(call.opts).toEqual({ path: QURL_OAUTH_COOKIE_PATH });
    });

    it('clears the PKCE verifier cookie with the same path', () => {
      const res = fakeRes();
      clearQurlOAuthPkceCookie(res);
      expect(res.clearCookieCalls).toHaveLength(1);
      const call = res.clearCookieCalls[0];
      expect(call.name).toBe(QURL_OAUTH_PKCE_COOKIE);
      expect(call.opts).toEqual({ path: QURL_OAUTH_COOKIE_PATH });
    });
  });
});
