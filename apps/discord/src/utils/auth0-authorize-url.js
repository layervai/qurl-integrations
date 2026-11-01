const config = require('../config');

// Both /qurl-admin setup and Add to Discord end at the same callback and bind a
// guild to the Auth0 account selected here. Keep every security-relevant
// authorize parameter in one builder so the entry paths cannot drift.
function qurlOAuthCallbackUrl() {
  return `${config.BASE_URL}/oauth/qurl/callback`;
}

function buildAuth0AuthorizeUrl({ state, codeChallenge, nonce }) {
  if (typeof state !== 'string' || !state
      || typeof codeChallenge !== 'string' || !codeChallenge
      || typeof nonce !== 'string' || !/^[a-f0-9]{32}$/.test(nonce)) {
    throw new TypeError('state/codeChallenge and a 32-hex-character nonce are required');
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
  authorizeUrl.searchParams.set('nonce', nonce);
  authorizeUrl.searchParams.set('state', state);
  authorizeUrl.searchParams.set('code_challenge', codeChallenge);
  authorizeUrl.searchParams.set('code_challenge_method', 'S256');
  // `login` is a best-effort policy hint requesting fresh Auth0
  // authentication instead of ambient-session reuse. The client does not
  // independently attest that freshness; #1366 owns the verifiable,
  // nonce-bound max_age/auth_time guarantee. Until #1365 pins the passwordless
  // connection, a live upstream social/enterprise IdP session can still select
  // an account. `consent` lets a setup re-run mint a new key instead of
  // silently reusing an earlier grant.
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
