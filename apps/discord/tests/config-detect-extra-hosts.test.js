
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
});

describe('config — QURL_LINK_DOMAIN', () => {
  it('normalizes the service-configured share-link host', () => {
    withFreshConfig({ QURL_LINK_DOMAIN: ' QURL.EXAMPLE ' }, () => {
      expect(require('../src/config').QURL_LINK_DOMAIN).toBe('qurl.example');
    });
  });

  it.each([
    'https://qurl.example', 'user@qurl.example', 'qurl.example/path',
    'qurl.example:443', 'qurl.example:8443',
  ])('leaves invalid non-host input %s for the private boot gate', (value) => {
    withFreshConfig({ QURL_LINK_DOMAIN: value }, () => {
      expect(require('../src/config').QURL_LINK_DOMAIN).toBeNull();
    });
  });
});
