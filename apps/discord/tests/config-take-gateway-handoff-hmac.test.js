/**
 * Unit tests for config.takeGatewayHandoffHmac (apps/discord/src/config.js).
 *
 * The one-shot getter is defense in depth against heap-dump key
 * exposure. After the first call, the raw GATEWAY_HANDOFF_HMAC
 * string must be unreachable through any reference in the config
 * module — `config.GATEWAY_HANDOFF_HMAC` no longer exists, the
 * module-private binding is nulled, and a second call returns
 * undefined.
 *
 * Each test uses jest.isolateModules to get a fresh config module
 * (with a fresh module-private binding) so the one-shot semantics
 * don't bleed across tests.
 */

function withFreshConfig(envValue, run) {
  jest.isolateModules(() => {
    const prev = process.env.GATEWAY_HANDOFF_HMAC;
    if (envValue === undefined) {
      delete process.env.GATEWAY_HANDOFF_HMAC;
    } else {
      process.env.GATEWAY_HANDOFF_HMAC = envValue;
    }
    try {
      const config = require('../src/config');
      run(config);
    } finally {
      if (prev === undefined) {
        delete process.env.GATEWAY_HANDOFF_HMAC;
      } else {
        process.env.GATEWAY_HANDOFF_HMAC = prev;
      }
    }
  });
}

describe('config.takeGatewayHandoffHmac (one-shot getter)', () => {
  it('returns the raw env value on first call', () => {
    const raw = '{"current":"' + 'a'.repeat(64) + '"}';
    withFreshConfig(raw, (config) => {
      expect(config.takeGatewayHandoffHmac()).toBe(raw);
    });
  });

  it('returns undefined on second call (binding nulled)', () => {
    // The load-bearing contract. After consumption, the secret string
    // is unreachable through the config module — a callback that
    // captures the config object at require-time and re-reads later
    // would get undefined, NOT a stale captured copy.
    const raw = '{"current":"' + 'b'.repeat(64) + '"}';
    withFreshConfig(raw, (config) => {
      expect(config.takeGatewayHandoffHmac()).toBe(raw);
      expect(config.takeGatewayHandoffHmac()).toBeUndefined();
      expect(config.takeGatewayHandoffHmac()).toBeUndefined();
    });
  });

  it('returns undefined immediately when env var is unset', () => {
    withFreshConfig(undefined, (config) => {
      expect(config.takeGatewayHandoffHmac()).toBeUndefined();
    });
  });

  it('returns empty string on first call when env var is set to empty string', () => {
    // The env-var-present check (missingHotStandbyKeys → hasGatewayHandoffHmac)
    // is what rejects an empty value at boot; the getter itself
    // doesn't filter empty strings. Pin the contract so a future
    // refactor that adds empty-string filtering here doesn't
    // silently shift the "missing" semantic.
    withFreshConfig('', (config) => {
      expect(config.takeGatewayHandoffHmac()).toBe('');
      expect(config.takeGatewayHandoffHmac()).toBeUndefined();
    });
  });

  it('does NOT expose the raw value as a config-object property', () => {
    // Closes the heap-dump vector. `config.GATEWAY_HANDOFF_HMAC` MUST
    // NOT exist on the exported object — that property is what a
    // future contributor would grep for, and finding it would
    // silently re-open the redundant-reference hazard.
    const raw = '{"current":"' + 'c'.repeat(64) + '"}';
    withFreshConfig(raw, (config) => {
      expect(Object.prototype.hasOwnProperty.call(config, 'GATEWAY_HANDOFF_HMAC')).toBe(false);
      expect(config.GATEWAY_HANDOFF_HMAC).toBeUndefined();
    });
  });

  it('hasGatewayHandoffHmac reflects env-var presence without exposing the value', () => {
    const raw = '{"current":"' + 'd'.repeat(64) + '"}';
    withFreshConfig(raw, (config) => {
      expect(config.hasGatewayHandoffHmac).toBe(true);
    });
    withFreshConfig(undefined, (config) => {
      expect(config.hasGatewayHandoffHmac).toBe(false);
    });
    withFreshConfig('', (config) => {
      expect(config.hasGatewayHandoffHmac).toBe(false);
    });
  });

  it('hasGatewayHandoffHmac stays true after the value is taken', () => {
    // The flag captures env-var presence at module-load time and
    // doesn't track consumption. The boot-presence check runs BEFORE
    // takeGatewayHandoffHmac is called, so the flag's frozen-at-load
    // semantic is intentional. Pinning so a future refactor that
    // flips the flag on take() flags itself as a behavior change.
    const raw = '{"current":"' + 'e'.repeat(64) + '"}';
    withFreshConfig(raw, (config) => {
      expect(config.hasGatewayHandoffHmac).toBe(true);
      config.takeGatewayHandoffHmac();
      expect(config.hasGatewayHandoffHmac).toBe(true);
    });
  });
});
