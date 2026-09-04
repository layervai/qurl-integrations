const { withFreshConfig, captureFreshConfig } = require('./helpers/fresh-config');

describe('config.AUTH0_EMAIL_CONNECTION', () => {
  it('is empty (no pin) when unset', () => {
    withFreshConfig({ AUTH0_EMAIL_CONNECTION: undefined }, (config) => {
      expect(config.AUTH0_EMAIL_CONNECTION).toBe('');
    });
  });

  it('treats a whitespace-only value as no pin', () => {
    withFreshConfig({ AUTH0_EMAIL_CONNECTION: '   ' }, (config) => {
      expect(config.AUTH0_EMAIL_CONNECTION).toBe('');
    });
  });

  it('trims a real connection name', () => {
    withFreshConfig({ AUTH0_EMAIL_CONNECTION: ' email ' }, (config) => {
      expect(config.AUTH0_EMAIL_CONNECTION).toBe('email');
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
});
