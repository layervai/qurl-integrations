// Shared cookie constants for the Discord-install and qURL OAuth flows.
// Extracted from routes/qurl-oauth.js so the Stage-2 chain (routes/discord-install.js →
// /oauth/qurl/callback) can't drift on the cookie name or path — both
// MUST match exactly or the qurl-oauth callback's cookie/state CSRF
// check 400s. PR #177 follow-up C.1.
//
// Host prefixes prevent sibling subdomains from planting setup state or PKCE
// cookies. Secure and Path=/ are required for setting and clearing them.
// Old unprefixed cookies are not accepted; in-flight setups restart once.
const QURL_OAUTH_SESSION_COOKIE = '__Host-qurl_setup_session';
const QURL_OAUTH_PKCE_COOKIE = '__Host-qurl_setup_pkce';
const QURL_OAUTH_COOKIE_PATH = '/';
// Matches STATE_TTL_SECONDS in qurl-oauth-state.js.
const QURL_OAUTH_COOKIE_TTL_SECONDS = 15 * 60;
// The install flow accepts a Discord guild binding, so prevent sibling
// subdomains from shadowing this cookie. __Host- requires Secure, Path=/,
// and no Domain attribute.
const DISCORD_INSTALL_SESSION_COOKIE = '__Host-qurl_discord_install_session';
const DISCORD_INSTALL_COOKIE_PATH = '/';
// Discord's authorization-code callback is interactive and can include a
// server picker (or server creation and app switches on mobile), so its
// browser binding gets thirty minutes rather than the fifteen-minute qURL setup
// window. The callback also enforces this TTL server-side from the expiry
// embedded in the state (routes/discord-install.js).
const DISCORD_INSTALL_COOKIE_TTL_SECONDS = 30 * 60;

// Shared setter; every setup/install caller explicitly requires Secure.
function setCookie(res, req, name, value, options = {}) {
  const { path, ttlSeconds } = options;
  const hasExplicitSecureFlag = Object.hasOwn(options, 'secure');
  if (req == null && !hasExplicitSecureFlag) {
    throw new TypeError('OAuth cookie secure flag must be explicit when no request is supplied');
  }
  const secure = hasExplicitSecureFlag ? options.secure : req.protocol === 'https';
  if (typeof secure !== 'boolean') {
    throw new TypeError('OAuth cookie secure flag must be a boolean');
  }
  if (typeof path !== 'string' || !path.startsWith('/')) {
    throw new TypeError('OAuth cookie path must be an absolute path');
  }
  if (!Number.isSafeInteger(ttlSeconds) || ttlSeconds <= 0) {
    throw new TypeError('OAuth cookie TTL must be a positive integer');
  }
  res.cookie(name, value, {
    httpOnly: true,
    secure,
    sameSite: 'lax',
    maxAge: ttlSeconds * 1000,
    path,
  });
}

function setQurlOAuthCookie(res, req, value) {
  setCookie(res, req, QURL_OAUTH_SESSION_COOKIE, value, {
    path: QURL_OAUTH_COOKIE_PATH,
    ttlSeconds: QURL_OAUTH_COOKIE_TTL_SECONDS,
    secure: true,
  });
}

// PKCE verifier cookie. Kept out of `state`: qURL OAuth state is signed
// for integrity, not encrypted, and it travels in browser/Auth0 URLs.
function setQurlOAuthPkceCookie(res, req, codeVerifier) {
  setCookie(res, req, QURL_OAUTH_PKCE_COOKIE, codeVerifier, {
    path: QURL_OAUTH_COOKIE_PATH,
    ttlSeconds: QURL_OAUTH_COOKIE_TTL_SECONDS,
    secure: true,
  });
}

function setDiscordInstallSessionCookie(res, state) {
  setCookie(res, null, DISCORD_INSTALL_SESSION_COOKIE, state, {
    path: DISCORD_INSTALL_COOKIE_PATH,
    ttlSeconds: DISCORD_INSTALL_COOKIE_TTL_SECONDS,
    // __Host- cookies require Secure even on localhost.
    secure: true,
  });
}

// Path MUST match the Set-Cookie path or the browser keeps the cookie
// alive until TTL — locking the path here removes that footgun.
function clearQurlOAuthCookie(res) {
  res.clearCookie(QURL_OAUTH_SESSION_COOKIE, { path: QURL_OAUTH_COOKIE_PATH, secure: true });
}

function clearQurlOAuthPkceCookie(res) {
  res.clearCookie(QURL_OAUTH_PKCE_COOKIE, { path: QURL_OAUTH_COOKIE_PATH, secure: true });
}

function clearDiscordInstallSessionCookie(res) {
  res.clearCookie(DISCORD_INSTALL_SESSION_COOKIE, {
    path: DISCORD_INSTALL_COOKIE_PATH,
    secure: true,
  });
}

module.exports = {
  QURL_OAUTH_SESSION_COOKIE,
  QURL_OAUTH_PKCE_COOKIE,
  QURL_OAUTH_COOKIE_PATH,
  QURL_OAUTH_COOKIE_TTL_SECONDS,
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
};
