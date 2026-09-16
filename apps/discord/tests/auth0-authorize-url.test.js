const { withFreshConfig } = require('./helpers/fresh-config');

const AUTH0_ENV = {
  AUTH0_DOMAIN: 'layerv-test.auth0.com',
  AUTH0_CLIENT_ID: 'test-client-id',
  AUTH0_AUDIENCE: 'https://api.layerv.test',
  BASE_URL: 'http://localhost:3000',
};
const PKCE_CHALLENGE = 'a'.repeat(43);

function withBuilder(envOverrides, run) {
  withFreshConfig({ ...AUTH0_ENV, ...envOverrides }, (config) => {
    const authorizeUrl = require('../src/utils/auth0-authorize-url');
    run(authorizeUrl, config);
  });
}

describe('buildAuth0AuthorizeUrl', () => {
  it('builds the shared security contract without a connection pin by default', () => {
    withBuilder({ AUTH0_EMAIL_CONNECTION: undefined }, ({
      buildAuth0AuthorizeUrl,
      qurlOAuthCallbackUrl,
    }) => {
      const url = buildAuth0AuthorizeUrl({ state: 'signed-state', codeChallenge: PKCE_CHALLENGE });

      expect(url.origin).toBe('https://layerv-test.auth0.com');
      expect(url.pathname).toBe('/authorize');
      expect(url.searchParams.get('response_type')).toBe('code');
      expect(url.searchParams.get('client_id')).toBe('test-client-id');
      expect(qurlOAuthCallbackUrl()).toBe('http://localhost:3000/oauth/qurl/callback');
      expect(url.searchParams.get('redirect_uri')).toBe(qurlOAuthCallbackUrl());
      expect(url.searchParams.get('scope')).toBe('qurl:write qurl:read openid email');
      expect(url.searchParams.get('audience')).toBe('https://api.layerv.test');
      expect(url.searchParams.get('state')).toBe('signed-state');
      expect(url.searchParams.get('code_challenge')).toBe(PKCE_CHALLENGE);
      expect(url.searchParams.get('code_challenge_method')).toBe('S256');
      expect(url.searchParams.get('prompt')).toBe('login consent');
      expect(url.search).toContain('prompt=login+consent');
      expect(url.searchParams.get('connection')).toBeNull();
    });
  });

  it('reads the callback base URL from the same live config as other parameters', () => {
    withBuilder({}, ({ qurlOAuthCallbackUrl }, config) => {
      config.BASE_URL = 'https://discord.example.test';
      expect(qurlOAuthCallbackUrl()).toBe('https://discord.example.test/oauth/qurl/callback');
    });
  });

  it('pins a configured passwordless connection after config trimming', () => {
    withBuilder({ AUTH0_EMAIL_CONNECTION: ' email ' }, ({ buildAuth0AuthorizeUrl }) => {
      const url = buildAuth0AuthorizeUrl({ state: 'signed-state', codeChallenge: PKCE_CHALLENGE });

      expect(url.searchParams.get('connection')).toBe('email');
    });
  });

  it('refuses to build an unpinned authorize URL when the configured pin was rejected', () => {
    withBuilder({ AUTH0_EMAIL_CONNECTION: 'email' }, ({ buildAuth0AuthorizeUrl }, config) => {
      config.isAuth0EmailConnectionRejected = true;
      expect(() => buildAuth0AuthorizeUrl({
        state: 'signed-state',
        codeChallenge: PKCE_CHALLENGE,
      })).toThrow('Auth0 email connection policy was rejected');
    });
  });

  it.each([
    { state: undefined, codeChallenge: PKCE_CHALLENGE },
    { state: '', codeChallenge: PKCE_CHALLENGE },
    { state: 42, codeChallenge: PKCE_CHALLENGE },
    { state: 'signed-state', codeChallenge: undefined },
    { state: 'signed-state', codeChallenge: '' },
    { state: 'signed-state', codeChallenge: 42 },
  ])('rejects missing authorize inputs: %p', (input) => {
    withBuilder({}, ({ buildAuth0AuthorizeUrl }) => {
      expect(() => buildAuth0AuthorizeUrl(input)).toThrow(TypeError);
    });
  });
});
