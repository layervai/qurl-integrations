// Shared cookie constants for the qURL OAuth flows. Extracted from
// routes/qurl-oauth.js so the Stage-2 chain (routes/discord-install.js →
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
const QURL_OAUTH_COOKIE_PATH = '/oauth/qurl';
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
