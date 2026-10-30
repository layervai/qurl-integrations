const config = require('../config');

// Both /qurl setup and Add to Discord end at the same callback and bind a
// guild to the Auth0 account selected here. Keep every security-relevant
// authorize parameter in one builder so the entry paths cannot drift.
function qurlOAuthCallbackUrl() {
  return `${config.BASE_URL}/oauth/qurl/callback`;
}

function buildAuth0AuthorizeUrl({ state, codeChallenge }) {
  if (typeof state !== 'string' || !state
      || typeof codeChallenge !== 'string' || !codeChallenge) {
    throw new TypeError('state and codeChallenge must be non-empty strings');
  }
  if (config.isAuth0EmailConnectionRejected) {
    throw new Error('Auth0 email connection policy was rejected');
  }

  const authorizeUrl = new URL(`https://${config.AUTH0_DOMAIN}/authorize`);
  authorizeUrl.searchParams.set('response_type', 'code');
  authorizeUrl.searchParams.set('client_id', config.AUTH0_CLIENT_ID);
  authorizeUrl.searchParams.set('redirect_uri', qurlOAuthCallbackUrl());
  // The API-key mint needs qurl:read/write. openid + email provide the
  // id_token email claim shown on the success-page binding readout. There is
  // no refresh-token use, so offline_access is deliberately absent.
  authorizeUrl.searchParams.set('scope', 'qurl:write qurl:read openid email');
  authorizeUrl.searchParams.set('audience', config.AUTH0_AUDIENCE);
  authorizeUrl.searchParams.set('nonce', state);
  authorizeUrl.searchParams.set('max_age', '0');
  authorizeUrl.searchParams.set('state', state);
  authorizeUrl.searchParams.set('code_challenge', codeChallenge);
  authorizeUrl.searchParams.set('code_challenge_method', 'S256');
  // Force fresh authentication; the callback checks nonce and auth_time.
  authorizeUrl.searchParams.set('prompt', 'login consent');

  // Discord is deliberately unpinned until #1365 enables passwordless on each
  // Auth0 application. While unset, the tenant login page can select a different
  // connection from qurl-desktop and give the same human another Auth0 subject.
  // TODO(upstream-contract): #1365 must close that gap by pinning the same
  // passwordless connection as qurl-desktop src/main/auth.ts
  // (browserAuthorizationOptions). A social or database connection can silently
  // fork the qURL account; a disabled or misspelled connection rejects every
  // setup at /authorize.
  if (config.AUTH0_EMAIL_CONNECTION) {
    authorizeUrl.searchParams.set('connection', config.AUTH0_EMAIL_CONNECTION);
  }

  return authorizeUrl;
}

module.exports = { buildAuth0AuthorizeUrl, qurlOAuthCallbackUrl };
