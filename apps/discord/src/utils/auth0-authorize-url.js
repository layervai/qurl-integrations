const config = require('../config');

// Both /qurl setup and Add to Discord end at the same callback and bind a
// guild to the Auth0 account selected here. Keep every security-relevant
// authorize parameter in one builder so the entry paths cannot drift.
function buildAuth0AuthorizeUrl({ state, codeChallenge }) {
  const authorizeUrl = new URL(`https://${config.AUTH0_DOMAIN}/authorize`);
  authorizeUrl.searchParams.set('response_type', 'code');
  authorizeUrl.searchParams.set('client_id', config.AUTH0_CLIENT_ID);
  authorizeUrl.searchParams.set('redirect_uri', `${config.BASE_URL}/oauth/qurl/callback`);
  // The API-key mint needs qurl:read/write. openid + email provide the
  // id_token email claim shown on the success-page binding readout. There is
  // no refresh-token use, so offline_access is deliberately absent.
  authorizeUrl.searchParams.set('scope', 'qurl:write qurl:read openid email');
  authorizeUrl.searchParams.set('audience', config.AUTH0_AUDIENCE);
  authorizeUrl.searchParams.set('state', state);
  authorizeUrl.searchParams.set('code_challenge', codeChallenge);
  authorizeUrl.searchParams.set('code_challenge_method', 'S256');
  // `login` prevents an ambient auth.layerv.ai session from selecting the
  // account that will own the guild key. `consent` lets a setup re-run mint
  // a new key instead of silently reusing an earlier grant.
  authorizeUrl.searchParams.set('prompt', 'login consent');

  // This setting is deliberately opt-in for Discord, unlike Slack: unset
  // means no connection pin until #1365 enables the passwordless connection
  // on every Discord Auth0 application. A configured but disabled or misspelled
  // connection makes Auth0 reject every setup at /authorize.
  // TODO(upstream-contract): keep the configured connection in lockstep with
  // qurl-desktop src/main/auth.ts (browserAuthorizationOptions). Otherwise the
  // same human can receive different Auth0 subjects across the two surfaces.
  if (config.AUTH0_EMAIL_CONNECTION) {
    authorizeUrl.searchParams.set('connection', config.AUTH0_EMAIL_CONNECTION);
  }

  return authorizeUrl;
}

module.exports = { buildAuth0AuthorizeUrl };
