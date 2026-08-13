/**
 * Unit tests for config.js's parsing of the env-extendable detect-tunnel
 * non-prod allowlist: DETECT_EXTRA_NON_PROD_QURL_ENDPOINT_HOSTS and
 * DETECT_EXTRA_NON_PROD_HOST_SUFFIXES.
 *
 * These two vars let a private infra repo grant connector.js's
 * detectTunnelHostSuffixesForEndpoint() extra non-prod endpoint hosts +
 * qurl_site suffixes (e.g. sandbox) without committing real hostnames to
 * this public repo — the built-in DETECT_TUNNEL_NON_PROD_* sets in
 * connector.js are hardcoded and public. config.js owns parsing + the
 * fail-fast shape guard; connector.js only consumes the resolved arrays
 * (see connector-coverage.test.js for the consumption-side tests).
 *
 * Each test uses jest.isolateModules to get a fresh config module so env
 * changes in one test can't leak into another via the require cache.
 */

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
