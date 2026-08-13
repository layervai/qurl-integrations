function withFreshConfig(baseUrl, run) {
  jest.isolateModules(() => {
    const prev = process.env.BASE_URL;
    try {
      if (baseUrl === undefined) {
        delete process.env.BASE_URL;
      } else {
        process.env.BASE_URL = baseUrl;
      }
      run(require('../src/config'));
    } finally {
      if (prev === undefined) {
        delete process.env.BASE_URL;
      } else {
        process.env.BASE_URL = prev;
      }
    }
  });
}

describe('config.BASE_URL normalization', () => {
  it('defaults to localhost when unset', () => {
    withFreshConfig(undefined, (config) => {
      expect(config.BASE_URL).toBe('http://localhost:3000');
    });
  });

  it('treats a whitespace-only value as unset, not as an empty BASE_URL', () => {
    // The default has to be applied AFTER trimming: an operator who
    // parameterized the SSM value but seeded it with spaces must land on the
    // localhost default, so boot-requirements.js's "BASE_URL is always
    // truthy" invariant holds and the boot error can quote a real value.
    withFreshConfig('   ', (config) => {
      expect(config.BASE_URL).toBe('http://localhost:3000');
    });
  });

  it('trims surrounding whitespace off a real origin', () => {
    withFreshConfig('  https://bot.example.com/  ', (config) => {
      expect(config.BASE_URL).toBe('https://bot.example.com');
    });
  });

  it('strips a harmless trailing slash from a bare origin', () => {
    withFreshConfig('https://bot.example.com/', (config) => {
      expect(config.BASE_URL).toBe('https://bot.example.com');
    });
  });

  it('does not hide malformed or non-origin values from boot diagnostics', () => {
    withFreshConfig('https://bot.example.com/prefix/', (config) => {
      expect(config.BASE_URL).toBe('https://bot.example.com/prefix/');
    });
    withFreshConfig('https://', (config) => {
      expect(config.BASE_URL).toBe('https://');
    });
  });
});
