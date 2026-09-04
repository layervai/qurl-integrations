const { withFreshConfig, withFreshEnv } = require('./helpers/fresh-config');

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

  it.each(['email connection', 'email!', '.email'])('rejects invalid connection name %p at config load', (value) => {
    withFreshEnv({ AUTH0_EMAIL_CONNECTION: value }, () => {
      expect(() => require('../src/config')).toThrow(/AUTH0_EMAIL_CONNECTION/);
    });
  });
});
