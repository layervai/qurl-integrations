const { withFreshConfig } = require('./helpers/fresh-config');

const oauthEnv = {
  AUTH0_DOMAIN: 'auth.example.test', AUTH0_CLIENT_ID: 'client', AUTH0_CLIENT_SECRET: 'secret',
  AUTH0_AUDIENCE: 'https://api.example.test', AUTH0_EXPECTED_AUDIENCE: undefined,
};

describe('Auth0 audience drift guard', () => {
  it.each([undefined, '', '  ', 'https://api.example.test', ' https://api.example.test '])('permits an unset or matching expected audience %s', expected => {
    withFreshConfig({ ...oauthEnv, AUTH0_EXPECTED_AUDIENCE: expected }, config => {
      expect(config.isQurlOAuthConfigured).toBe(true);
    });
  });

  it.each(['https://wrong.example.test', 'https://api.example.test/', ' https://api.example.test'])('rejects a mismatched audience before startup: %s', audience => {
    expect(() => withFreshConfig({ ...oauthEnv, AUTH0_AUDIENCE: audience, AUTH0_EXPECTED_AUDIENCE: 'https://api.example.test' }, () => {}))
      .toThrow(/AUTH0_AUDIENCE.*AUTH0_EXPECTED_AUDIENCE/);
  });
});
