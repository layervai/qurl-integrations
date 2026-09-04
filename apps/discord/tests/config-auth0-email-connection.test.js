const {
  withFreshEnv,
  withFreshConfig,
  captureFreshConfig,
} = require('./helpers/fresh-config');
const {
  baseUrlHttpsProblem,
  invalidStateSecretValues,
  missingKekRequiredKeys,
} = require('../src/boot-requirements');

describe('config.AUTH0_EMAIL_CONNECTION', () => {
  const AUTH0_ENV = {
    AUTH0_DOMAIN: 'layerv-test.auth0.com',
    AUTH0_CLIENT_ID: 'test-client-id',
    AUTH0_CLIENT_SECRET: 'test-client-secret',
    AUTH0_AUDIENCE: 'https://api.layerv.test',
  };

  it('is empty (no pin) when unset', () => {
    withFreshConfig({ AUTH0_EMAIL_CONNECTION: undefined }, (config) => {
      expect(config.AUTH0_EMAIL_CONNECTION).toBe('');
      expect(config.isAuth0EmailConnectionRejected).toBe(false);
      expect(config.auth0EmailConnectionState).toBe('unset');
      expect(config.isQurlSetupAvailable).toBe(false);
    });
  });

  it('treats a whitespace-only value as no pin', () => {
    withFreshConfig({ AUTH0_EMAIL_CONNECTION: '   ' }, (config) => {
      expect(config.AUTH0_EMAIL_CONNECTION).toBe('');
      expect(config.isAuth0EmailConnectionRejected).toBe(false);
    });
  });

  it('trims a real connection name', () => {
    withFreshConfig({ AUTH0_EMAIL_CONNECTION: ' email ' }, (config) => {
      expect(config.AUTH0_EMAIL_CONNECTION).toBe('email');
      expect(config.isAuth0EmailConnectionRejected).toBe(false);
      expect(config.auth0EmailConnectionState).toBe('pinned');
    });
  });

  it('accepts a connection name at the 128-character limit', () => {
    const value = 'a'.repeat(128);
    withFreshConfig({ AUTH0_EMAIL_CONNECTION: value }, (config) => {
      expect(config.AUTH0_EMAIL_CONNECTION).toBe(value);
    });
  });

  it.each([
    'email connection', 'email!', '.email', '-email', 'email_', '_',
    'a'.repeat(129),
  ])(
    'warns and disables the connection pin for invalid value %p',
    (value) => {
      captureFreshConfig({ AUTH0_EMAIL_CONNECTION: value }, (config, warns) => {
        expect(config.AUTH0_EMAIL_CONNECTION).toBe('');
        expect(config.isAuth0EmailConnectionRejected).toBe(true);
        expect(warns).toContainEqual(expect.stringContaining('AUTH0_EMAIL_CONNECTION'));
      });
    },
  );

  it('does not echo a rejected configured value into the boot warning', () => {
    const value = 'private looking value';
    captureFreshConfig({ AUTH0_EMAIL_CONNECTION: value }, (_config, warns) => {
      expect(warns).toContainEqual(expect.stringContaining('AUTH0_EMAIL_CONNECTION'));
      expect(warns.join('\n')).not.toContain(value);
    });
  });

  it('disables only OAuth setup when a configured connection is rejected', () => {
    captureFreshConfig({
      ...AUTH0_ENV,
      AUTH0_EMAIL_CONNECTION: 'email!',
      BASE_URL: 'http://localhost:3000',
      OAUTH_STATE_SECRET: undefined,
      QURL_OAUTH_STATE_SECRET: undefined,
    }, (config) => {
      expect(config.isAuth0EmailConnectionRejected).toBe(true);
      expect(config.auth0EmailConnectionState).toBe('rejected');
      // The core Auth0 settings remain configured so independent production
      // boot checks still run; only the user-facing setup surface is blocked.
      expect(config.isQurlOAuthConfigured).toBe(true);
      expect(config.isQurlSetupAvailable).toBe(false);
      expect(config.isDiscordInstallConfigured).toBe(false);
      expect(config.discordInstallNotConfiguredReason).toBe(
        'AUTH0_EMAIL_CONNECTION rejected',
      );
      expect(missingKekRequiredKeys({}, config.isQurlOAuthConfigured)).toEqual([
        'KEY_ENCRYPTION_KEY',
      ]);
      expect(baseUrlHttpsProblem(config, true)).toContain(
        'BASE_URL must be a public bare https:// origin',
      );
      expect(invalidStateSecretValues(config)).toContainEqual(
        expect.stringContaining('no state-signing secret is available'),
      );
    });
  });

  it('treats the seeded SSM placeholder as intentionally unset', () => {
    captureFreshConfig({
      ...AUTH0_ENV,
      AUTH0_EMAIL_CONNECTION: ' PLACEHOLDER ',
      DISCORD_CLIENT_ID: '234567890123456789',
      DISCORD_CLIENT_SECRET: 'test-discord-secret',
      BASE_URL: 'http://localhost:3000',
    }, (config, warns) => {
      expect(config.AUTH0_EMAIL_CONNECTION).toBe('');
      expect(config.isAuth0EmailConnectionRejected).toBe(false);
      expect(config.auth0EmailConnectionState).toBe('unset');
      expect(config.isQurlOAuthConfigured).toBe(true);
      expect(config.isQurlSetupAvailable).toBe(true);
      expect(config.isDiscordInstallConfigured).toBe(true);
      expect(config.discordInstallNotConfiguredReason).toBeNull();
      expect(warns).toContainEqual(expect.stringContaining(
        'reserved SSM placeholder; treating the connection pin as unset',
      ));
    });
  });

  it('uses the exact shared SSM placeholder sentinel', () => {
    withFreshEnv({ AUTH0_EMAIL_CONNECTION: 'unset' }, () => {
      jest.doMock('../src/utils/ssm-placeholder', () => ({
        SSM_PLACEHOLDER_SENTINEL: 'UNSET',
      }));
      try {
        const config = require('../src/config');
        expect(config.AUTH0_EMAIL_CONNECTION).toBe('unset');
        expect(config.isAuth0EmailConnectionRejected).toBe(false);
        expect(config.auth0EmailConnectionState).toBe('pinned');
      } finally {
        jest.dontMock('../src/utils/ssm-placeholder');
      }
    });
  });

  it.each([
    ['email!', 'contains characters outside'],
    ['a'.repeat(129), 'exceeds the 128-character limit'],
  ])('reports why invalid value %p was rejected without echoing it', (value, reason) => {
    captureFreshConfig({ AUTH0_EMAIL_CONNECTION: value }, (_config, warns) => {
      expect(warns.join('\n')).toContain(reason);
      expect(warns.join('\n')).not.toContain(value);
    });
  });
});
