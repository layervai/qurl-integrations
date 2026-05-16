// Each test uses jest.isolateModules with `os` mocked BEFORE config
// is required — config caches the derived values into the exported
// object at require-time, so a fresh module graph is required per
// test to exercise different `os` shapes.

function withFreshConfig({ env = {}, hostname, networkInterfaces }, run) {
  jest.isolateModules(() => {
    const prevEnv = {};
    for (const key of ['INSTANCE_ID', 'INSTANCE_IP']) {
      prevEnv[key] = process.env[key];
      if (env[key] === undefined) {
        delete process.env[key];
      } else {
        process.env[key] = env[key];
      }
    }
    jest.doMock('os', () => {
      const actual = jest.requireActual('os');
      return {
        ...actual,
        hostname: () => (hostname !== undefined ? hostname : actual.hostname()),
        networkInterfaces: () =>
          networkInterfaces !== undefined ? networkInterfaces : actual.networkInterfaces(),
      };
    });
    try {
      const config = require('../src/config');
      run(config);
    } finally {
      for (const key of Object.keys(prevEnv)) {
        if (prevEnv[key] === undefined) {
          delete process.env[key];
        } else {
          process.env[key] = prevEnv[key];
        }
      }
      jest.dontMock('os');
    }
  });
}

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
    // Env override wins over derivation, so a malformed value
    // (10.0.0.999 — non-IPv4) lands in config.INSTANCE_IP unmodified.
    // The boot-time validator must then catch it. Locks the contract
    // that override-path values still flow through the shape gate.
    withFreshConfig(
      { env: { INSTANCE_IP: '10.0.0.999' } },
      (config) => {
        // Require inside isolateModules so boot-requirements sees the
        // same module graph as config — future-proofs against a graph
        // dependency on the `os` mock.
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
    // Eth0 has both an internal address (127.0.0.1 — a misconfigured
    // alias) AND the real non-internal IP. Without the `!addr.internal`
    // guard this would return 127.0.0.1 because it appears first.
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
});
