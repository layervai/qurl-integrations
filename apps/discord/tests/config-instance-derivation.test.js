
const { withFreshConfigMockingOs: withFreshConfig } = require('./helpers/fresh-config');
describe('INSTANCE_ID derivation', () => {
  it('uses INSTANCE_ID env override when set (wins over hostname)', () => {
    withFreshConfig(
      { env: { INSTANCE_ID: 'pinned-override' }, hostname: 'ip-10-0-0-7' },
      (config) => {
        expect(config.INSTANCE_ID).toBe('pinned-override');
      },
    );
  });

  it('falls back to os.hostname() when INSTANCE_ID is unset', () => {
    withFreshConfig(
      { env: {}, hostname: 'fargate-abc123def456' },
      (config) => {
        expect(config.INSTANCE_ID).toBe('fargate-abc123def456');
      },
    );
  });

  it('returns null when both env override and os.hostname() are empty', () => {
    withFreshConfig(
      { env: {}, hostname: '' },
      (config) => {
        expect(config.INSTANCE_ID).toBeNull();
      },
    );
  });

  it('falls back to os.hostname() when INSTANCE_ID is empty string', () => {
    withFreshConfig(
      { env: { INSTANCE_ID: '' }, hostname: 'fargate-empty-env' },
      (config) => {
        expect(config.INSTANCE_ID).toBe('fargate-empty-env');
      },
    );
  });
});

describe('INSTANCE_IP derivation', () => {
  it('falls back to interface scan when INSTANCE_IP is empty string', () => {
    withFreshConfig(
      {
        env: { INSTANCE_IP: '' },
        networkInterfaces: {
          eth0: [{ family: 'IPv4', address: '10.0.0.7', internal: false }],
        },
      },
      (config) => {
        expect(config.INSTANCE_IP).toBe('10.0.0.7');
      },
    );
  });

  it('trims whitespace from env overrides (parity with other env-derived values)', () => {
    withFreshConfig(
      {
        env: { INSTANCE_ID: '  pinned  ', INSTANCE_IP: ' 10.0.0.5 ' },
      },
      (config) => {
        expect(config.INSTANCE_ID).toBe('pinned');
        expect(config.INSTANCE_IP).toBe('10.0.0.5');
      },
    );
  });

  it('whitespace-only env overrides fall through to derivation', () => {
    withFreshConfig(
      {
        env: { INSTANCE_ID: '   ', INSTANCE_IP: '\t\t' },
        hostname: 'derived-host',
        networkInterfaces: {
          eth0: [{ family: 'IPv4', address: '10.0.0.99', internal: false }],
        },
      },
      (config) => {
        expect(config.INSTANCE_ID).toBe('derived-host');
        expect(config.INSTANCE_IP).toBe('10.0.0.99');
      },
    );
  });

  it('uses INSTANCE_IP env override when set (wins over interfaces)', () => {
    withFreshConfig(
      {
        env: { INSTANCE_IP: '10.99.99.99' },
        networkInterfaces: {
          eth0: [{ family: 'IPv4', address: '10.0.0.5', internal: false }],
        },
      },
      (config) => {
        expect(config.INSTANCE_IP).toBe('10.99.99.99');
      },
    );
  });

  it('picks first non-internal IPv4 from eth0 when env unset', () => {
    withFreshConfig(
      {
        env: {},
        networkInterfaces: {
          lo: [{ family: 'IPv4', address: '127.0.0.1', internal: true }],
          eth0: [
            { family: 'IPv6', address: 'fe80::1', internal: false },
            { family: 'IPv4', address: '10.0.0.42', internal: false },
          ],
        },
      },
      (config) => {
        expect(config.INSTANCE_IP).toBe('10.0.0.42');
      },
    );
  });

  it('accepts numeric addr.family (defensive against historical Node 18 regression)', () => {
    withFreshConfig(
      {
        env: {},
        networkInterfaces: {
          eth0: [{ family: 4, address: '10.0.0.55', internal: false }],
        },
      },
      (config) => {
        expect(config.INSTANCE_IP).toBe('10.0.0.55');
      },
    );
  });

  it('falls back to non-eth0 when eth0 has only internal IPv4 addresses', () => {
    withFreshConfig(
      {
        env: {},
        networkInterfaces: {
          eth0: [{ family: 'IPv4', address: '127.0.0.1', internal: true }],
          en0: [{ family: 'IPv4', address: '192.168.5.10', internal: false }],
        },
      },
      (config) => {
        expect(config.INSTANCE_IP).toBe('192.168.5.10');
      },
    );
  });

  it('falls back to non-eth0 interfaces when eth0 has no usable IPv4', () => {
    withFreshConfig(
      {
        env: {},
        networkInterfaces: {
          lo: [{ family: 'IPv4', address: '127.0.0.1', internal: true }],
          en0: [{ family: 'IPv4', address: '192.168.1.42', internal: false }],
        },
      },
      (config) => {
        expect(config.INSTANCE_IP).toBe('192.168.1.42');
      },
    );
  });

  it('returns null when no non-internal IPv4 exists', () => {
    withFreshConfig(
      {
        env: {},
        networkInterfaces: {
          lo: [{ family: 'IPv4', address: '127.0.0.1', internal: true }],
          eth0: [{ family: 'IPv6', address: 'fe80::1', internal: false }],
        },
      },
      (config) => {
        expect(config.INSTANCE_IP).toBeNull();
      },
    );
  });

  it('passes a bad env override through to invalidHotStandbyValues (contract end-to-end)', () => {
    withFreshConfig(
      { env: { INSTANCE_IP: '10.0.0.999' } },
      (config) => {
        const { invalidHotStandbyValues } = require('../src/boot-requirements');
        expect(config.INSTANCE_IP).toBe('10.0.0.999');
        const problems = invalidHotStandbyValues({
          ...config,
          ENABLE_GATEWAY_HOT_STANDBY: true,
        });
        expect(problems.some((p) => p.includes('INSTANCE_IP must be a valid IPv4'))).toBe(true);
      },
    );
  });

  it('skips internal IPv4 addresses on eth0 (loopback aliased)', () => {
    withFreshConfig(
      {
        env: {},
        networkInterfaces: {
          eth0: [
            { family: 'IPv4', address: '127.0.0.1', internal: true },
            { family: 'IPv4', address: '10.0.0.1', internal: false },
          ],
        },
      },
      (config) => {
        expect(config.INSTANCE_IP).toBe('10.0.0.1');
      },
    );
  });

  it('skips link-local 169.254.0.0/16 addresses on eth0 (Fargate platform 1.4+)', () => {
    withFreshConfig(
      {
        env: {},
        networkInterfaces: {
          eth0: [
            { family: 'IPv4', address: '169.254.172.2', internal: false },
            { family: 'IPv4', address: '172.31.44.139', internal: false },
          ],
        },
      },
      (config) => {
        expect(config.INSTANCE_IP).toBe('172.31.44.139');
      },
    );
  });

  it('falls through to non-eth0 when eth0 has only link-local IPv4', () => {
    withFreshConfig(
      {
        env: {},
        networkInterfaces: {
          eth0: [{ family: 'IPv4', address: '169.254.172.2', internal: false }],
          eth1: [{ family: 'IPv4', address: '10.0.5.7', internal: false }],
        },
      },
      (config) => {
        expect(config.INSTANCE_IP).toBe('10.0.5.7');
      },
    );
  });
});
