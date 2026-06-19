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
