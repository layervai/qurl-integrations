// Shared cookie constants for the qURL OAuth flows. Extracted from
// routes/qurl-oauth.js so the Stage-2 chain (routes/discord-install.js →
// /oauth/qurl/callback) can't drift on the cookie name or path — both
// MUST match exactly or the qurl-oauth callback's cookie/state CSRF
// check 400s. PR #177 follow-up C.1.
//
// Path is intentionally `/oauth` (not `/oauth/qurl`) because Stage-2
// chains across two callbacks under this prefix (/oauth/discord/callback
// → /oauth/qurl/callback). If a third sibling router lands under
// /oauth/… (say a Slack-link proxy), the session cookie will travel
// there too — narrow the path or rename the cookie to scope away.
const QURL_OAUTH_SESSION_COOKIE = 'qurl_setup_session';
const QURL_OAUTH_COOKIE_PATH = '/oauth';
const QURL_OAUTH_COOKIE_TTL_SECONDS = 5 * 60;

// Single shape for the double-submit CSRF cookie set by both
// /oauth/qurl/start (Stage 1) and /oauth/discord/callback (Stage 2).
// `secure: req.protocol === 'https'` requires `trust proxy` to be on
// in server.js so req.protocol reflects X-Forwarded-Proto from the ALB
// — flipping that off would silently downgrade prod cookies. Keeping
// the cookie shape in one place makes Stage-1/Stage-2 drift impossible.
function setQurlOAuthCookie(res, req, value) {
  res.cookie(QURL_OAUTH_SESSION_COOKIE, value, {
    httpOnly: true,
    secure: req.protocol === 'https',
    sameSite: 'lax',
    maxAge: QURL_OAUTH_COOKIE_TTL_SECONDS * 1000,
    path: QURL_OAUTH_COOKIE_PATH,
  });
}

// Path MUST match the Set-Cookie path or the browser keeps the cookie
// alive until TTL — locking the path here removes that footgun.
function clearQurlOAuthCookie(res) {
  res.clearCookie(QURL_OAUTH_SESSION_COOKIE, { path: QURL_OAUTH_COOKIE_PATH });
}

module.exports = {
  QURL_OAUTH_SESSION_COOKIE,
  QURL_OAUTH_COOKIE_PATH,
  QURL_OAUTH_COOKIE_TTL_SECONDS,
  setQurlOAuthCookie,
  clearQurlOAuthCookie,
};
