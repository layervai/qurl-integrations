
const {
  DISCORD_INSTALL_SESSION_COOKIE,
  DISCORD_INSTALL_COOKIE_PATH,
  DISCORD_INSTALL_COOKIE_TTL_SECONDS,
  setCookie,
  setQurlOAuthCookie,
  setQurlOAuthPkceCookie,
  setDiscordInstallSessionCookie,
  clearQurlOAuthCookie,
  clearQurlOAuthPkceCookie,
  clearDiscordInstallSessionCookie,
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
  describe('setCookie validation', () => {
    it.each([undefined, '', 'oauth/qurl'])(
      'rejects an unsafe cookie path (%p)',
      path => {
        expect(() => setCookie(fakeRes(), { protocol: 'https' }, 'name', 'value', {
          path,
          ttlSeconds: 300,
        })).toThrow('absolute path');
      },
    );

    it.each([undefined, 0, -1, 1.5, Number.NaN])(
      'rejects an unsafe cookie TTL (%p)',
      ttlSeconds => {
        expect(() => setCookie(fakeRes(), { protocol: 'https' }, 'name', 'value', {
          path: '/',
          ttlSeconds,
        })).toThrow('positive integer');
      },
    );

    it('requires an explicit Secure decision when no request is available', () => {
      expect(() => setCookie(fakeRes(), null, 'name', 'value', {
        path: '/',
        ttlSeconds: 300,
      })).toThrow('secure flag must be explicit');
    });
  });

  describe('setQurlOAuthCookie', () => {
    it('sets the canonical cookie shape (HttpOnly, SameSite=Lax, Secure, Path=/)', () => {
      const res = fakeRes();
      setQurlOAuthCookie(res, { protocol: 'https' }, 'state-token-abc');
      expect(res.cookieCalls).toHaveLength(1);
      const call = res.cookieCalls[0];
      expect(call.name).toBe('__Host-qurl_setup_session');
      expect(call.value).toBe('state-token-abc');
      expect(call.opts).toEqual({
        httpOnly: true,
        secure: true,
        sameSite: 'lax',
        maxAge: 15 * 60 * 1000,
        path: '/',
      });
    });

    it('sets the PKCE verifier cookie with the same browser/session scope', () => {
      const res = fakeRes();
      setQurlOAuthPkceCookie(res, { protocol: 'https' }, 'verifier-abc');
      expect(res.cookieCalls).toHaveLength(1);
      const call = res.cookieCalls[0];
      expect(call.name).toBe('__Host-qurl_setup_pkce');
      expect(call.value).toBe('verifier-abc');
      expect(call.opts).toEqual({
        httpOnly: true,
        secure: true,
        sameSite: 'lax',
        maxAge: 15 * 60 * 1000,
        path: '/',
      });
    });

    it('keeps host cookies Secure behind plain HTTP and TLS-terminating proxies', () => {
      const res = fakeRes();
      setQurlOAuthCookie(res, { protocol: 'http' }, 'state-token-abc');
      expect(res.cookieCalls[0].opts.secure).toBe(true);
    });
  });

  describe('clearQurlOAuthCookie', () => {
    it('always passes Secure and Path=/ so the browser actually forgets the cookie', () => {
      const res = fakeRes();
      clearQurlOAuthCookie(res);
      expect(res.clearCookieCalls).toHaveLength(1);
      const call = res.clearCookieCalls[0];
      expect(call.name).toBe('__Host-qurl_setup_session');
      expect(call.opts).toEqual({ path: '/', secure: true });
    });

    it('clears the PKCE verifier cookie with the same path', () => {
      const res = fakeRes();
      clearQurlOAuthPkceCookie(res);
      expect(res.clearCookieCalls).toHaveLength(1);
      const call = res.clearCookieCalls[0];
      expect(call.name).toBe('__Host-qurl_setup_pkce');
      expect(call.opts).toEqual({ path: '/', secure: true });
    });
  });

  describe('Discord install session cookie', () => {
    it('uses the install-session TTL on a __Host- cookie with Secure and Path=/', () => {
      const res = fakeRes();

      setDiscordInstallSessionCookie(res, 'install-state');

      expect(res.cookieCalls).toEqual([{
        name: DISCORD_INSTALL_SESSION_COOKIE,
        value: 'install-state',
        opts: {
          httpOnly: true,
          secure: true,
          sameSite: 'lax',
          maxAge: DISCORD_INSTALL_COOKIE_TTL_SECONDS * 1000,
          path: DISCORD_INSTALL_COOKIE_PATH,
        },
      }]);
    });

    it('clears with the same host-prefix attributes', () => {
      const res = fakeRes();

      clearDiscordInstallSessionCookie(res);

      expect(res.clearCookieCalls).toEqual([{
        name: DISCORD_INSTALL_SESSION_COOKIE,
        opts: { path: DISCORD_INSTALL_COOKIE_PATH, secure: true },
      }]);
    });
  });
});
