
const { withFreshEnv: withFreshConfig } = require('./helpers/fresh-config');
describe('config — DETECT_EXTRA_NON_PROD_QURL_ENDPOINT_HOSTS / DETECT_EXTRA_NON_PROD_HOST_SUFFIXES', () => {
  it('default to empty arrays when unset', () => {
    withFreshConfig({
      DETECT_EXTRA_NON_PROD_QURL_ENDPOINT_HOSTS: undefined,
      DETECT_EXTRA_NON_PROD_HOST_SUFFIXES: undefined,
    }, () => {
      const config = require('../src/config');
      expect(config.DETECT_EXTRA_NON_PROD_QURL_ENDPOINT_HOSTS).toEqual([]);
      expect(config.DETECT_EXTRA_NON_PROD_HOST_SUFFIXES).toEqual([]);
    });
  });

  it('splits on comma, trims, lowercases, and drops empty entries', () => {
    withFreshConfig({
      DETECT_EXTRA_NON_PROD_QURL_ENDPOINT_HOSTS: ' Api.Sandbox.Example , api.other.example ,,',
      DETECT_EXTRA_NON_PROD_HOST_SUFFIXES: ' .Tunnel.Sandbox.Example , .other.example , ,',
    }, () => {
      const config = require('../src/config');
      expect(config.DETECT_EXTRA_NON_PROD_QURL_ENDPOINT_HOSTS).toEqual(['api.sandbox.example', 'api.other.example']);
      expect(config.DETECT_EXTRA_NON_PROD_HOST_SUFFIXES).toEqual(['.tunnel.sandbox.example', '.other.example']);
    });
  });

  it('fails fast at module load when a suffix entry does not start with "."', () => {
    withFreshConfig({
      DETECT_EXTRA_NON_PROD_HOST_SUFFIXES: 'tunnel.sandbox.example',
    }, () => {
      expect(() => require('../src/config')).toThrow(/DETECT_EXTRA_NON_PROD_HOST_SUFFIXES/);
    });
  });

  it('fail-fast message names the specific malformed entry among several', () => {
    withFreshConfig({
      DETECT_EXTRA_NON_PROD_HOST_SUFFIXES: '.tunnel.sandbox.example,bad-suffix.example',
    }, () => {
      expect(() => require('../src/config')).toThrow(/bad-suffix\.example/);
    });
  });

  it('trims the detect tunnel slug before native resource lookup', () => {
    withFreshConfig({ DETECT_TUNNEL_SLUG: '  detect-prod \t' }, () => {
      expect(require('../src/config').DETECT_TUNNEL_SLUG).toBe('detect-prod');
    });
  });
});
