// Shared cookie constants for the Discord-install and qURL OAuth flows.
// Extracted from routes/qurl-oauth.js so the Stage-2 chain (routes/discord-install.js →
// /oauth/qurl/callback) can't drift on the cookie name or path — both
// MUST match exactly or the qurl-oauth callback's cookie/state CSRF
// check 400s. PR #177 follow-up C.1.
//
// Path is `/oauth/qurl` (NOT the broader `/oauth`). The only reader
// is the qurl-oauth callback at `/oauth/qurl/callback`; both Stage-1
// (/oauth/qurl/start) and Stage-2 (/oauth/discord/callback) only
// SET the cookie, so the Set-Cookie request URL doesn't constrain
// the path attribute (the browser stores the cookie either way).
// Narrow scope means a future router under `/oauth/...` (Slack link
// proxy, Teams, etc.) won't silently inherit this cookie. Per
// Justin's PR #177 round-9 item #2.
const QURL_OAUTH_SESSION_COOKIE = 'qurl_setup_session';
const QURL_OAUTH_PKCE_COOKIE = 'qurl_setup_pkce';
const QURL_OAUTH_COOKIE_PATH = '/oauth/qurl';
const QURL_OAUTH_COOKIE_TTL_SECONDS = 5 * 60;
// The install flow accepts a Discord guild binding, so prevent sibling
// subdomains from shadowing this cookie. __Host- requires Secure, Path=/,
// and no Domain attribute.
const DISCORD_INSTALL_SESSION_COOKIE = '__Host-qurl_discord_install_session';
const DISCORD_INSTALL_COOKIE_PATH = '/';
// Discord's authorization-code callback is interactive and can include a
// server picker, so its browser binding gets ten minutes rather than the
// five-minute qURL setup window.
const DISCORD_INSTALL_COOKIE_TTL_SECONDS = 10 * 60;

// Single shape for the OAuth double-submit CSRF cookies.
// `secure: req.protocol === 'https'` requires `trust proxy` to be on
// in server.js so req.protocol reflects X-Forwarded-Proto from the ALB
// — flipping that off would silently downgrade prod cookies. Keeping
// the cookie shape in one place makes Stage-1/Stage-2 drift impossible.
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
  });
}

// PKCE verifier cookie. Kept out of `state`: qURL OAuth state is signed
// for integrity, not encrypted, and it travels in browser/Auth0 URLs.
function setQurlOAuthPkceCookie(res, req, codeVerifier) {
  setCookie(res, req, QURL_OAUTH_PKCE_COOKIE, codeVerifier, {
    path: QURL_OAUTH_COOKIE_PATH,
    ttlSeconds: QURL_OAUTH_COOKIE_TTL_SECONDS,
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
  res.clearCookie(QURL_OAUTH_SESSION_COOKIE, { path: QURL_OAUTH_COOKIE_PATH });
}

function clearQurlOAuthPkceCookie(res) {
  res.clearCookie(QURL_OAUTH_PKCE_COOKIE, { path: QURL_OAUTH_COOKIE_PATH });
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
