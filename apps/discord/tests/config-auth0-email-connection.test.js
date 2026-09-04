const {
  withFreshEnv,
  withFreshConfig,
  captureFreshConfig,
} = require('./helpers/fresh-config');

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
    'PLACEHOLDER', 'a'.repeat(129),
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
    }, (config) => {
      expect(config.isAuth0EmailConnectionRejected).toBe(true);
      expect(config.isQurlOAuthConfigured).toBe(false);
      expect(config.isDiscordInstallConfigured).toBe(false);
      expect(config.discordInstallNotConfiguredReason).toBe(
        'AUTH0_EMAIL_CONNECTION rejected',
      );
    });
  });

  it('uses the shared SSM placeholder sentinel case-insensitively', () => {
    withFreshEnv({ AUTH0_EMAIL_CONNECTION: 'unset' }, () => {
      jest.doMock('../src/utils/ssm-placeholder', () => ({
        SSM_PLACEHOLDER_SENTINEL: 'UNSET',
      }));
      try {
        expect(require('../src/config').AUTH0_EMAIL_CONNECTION).toBe('');
      } finally {
        jest.dontMock('../src/utils/ssm-placeholder');
      }
    });
  });

  it.each([
    ['email!', 'contains characters outside'],
    ['a'.repeat(129), 'exceeds the 128-character limit'],
    ['PLACEHOLDER', 'reserved SSM placeholder'],
  ])('reports why invalid value %p was rejected without echoing it', (value, reason) => {
    captureFreshConfig({ AUTH0_EMAIL_CONNECTION: value }, (_config, warns) => {
      expect(warns.join('\n')).toContain(reason);
      expect(warns.join('\n')).not.toContain(value);
    });
  });
});
