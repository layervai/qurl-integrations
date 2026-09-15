
const { withFreshConfig } = require('./helpers/fresh-config');
describe('config.takeGatewayHandoffHmac (one-shot getter)', () => {
  it('returns the raw env value on first call', () => {
    const raw = '{"current":"' + 'a'.repeat(64) + '"}';
    withFreshConfig({ GATEWAY_HANDOFF_HMAC: raw }, (config) => {
      expect(config.takeGatewayHandoffHmac()).toBe(raw);
    });
  });

  it('returns undefined on second call (binding nulled)', () => {
    const raw = '{"current":"' + 'b'.repeat(64) + '"}';
    withFreshConfig({ GATEWAY_HANDOFF_HMAC: raw }, (config) => {
      expect(config.takeGatewayHandoffHmac()).toBe(raw);
      expect(config.takeGatewayHandoffHmac()).toBeUndefined();
      expect(config.takeGatewayHandoffHmac()).toBeUndefined();
    });
  });

  it('returns undefined immediately when env var is unset', () => {
    withFreshConfig({ GATEWAY_HANDOFF_HMAC: undefined }, (config) => {
      expect(config.takeGatewayHandoffHmac()).toBeUndefined();
    });
  });

  it('returns empty string on first call when env var is set to empty string', () => {
    withFreshConfig({ GATEWAY_HANDOFF_HMAC: '' }, (config) => {
      expect(config.takeGatewayHandoffHmac()).toBe('');
      expect(config.takeGatewayHandoffHmac()).toBeUndefined();
    });
  });

  it('does NOT expose the raw value as a config-object property', () => {
    const raw = '{"current":"' + 'c'.repeat(64) + '"}';
    withFreshConfig({ GATEWAY_HANDOFF_HMAC: raw }, (config) => {
      expect(Object.prototype.hasOwnProperty.call(config, 'GATEWAY_HANDOFF_HMAC')).toBe(false);
      expect(config.GATEWAY_HANDOFF_HMAC).toBeUndefined();
    });
  });

  it('hasGatewayHandoffHmac reflects env-var presence without exposing the value', () => {
    const raw = '{"current":"' + 'd'.repeat(64) + '"}';
    withFreshConfig({ GATEWAY_HANDOFF_HMAC: raw }, (config) => {
      expect(config.hasGatewayHandoffHmac).toBe(true);
    });
    withFreshConfig({ GATEWAY_HANDOFF_HMAC: undefined }, (config) => {
      expect(config.hasGatewayHandoffHmac).toBe(false);
    });
    withFreshConfig({ GATEWAY_HANDOFF_HMAC: '' }, (config) => {
      expect(config.hasGatewayHandoffHmac).toBe(false);
    });
  });

  it('hasGatewayHandoffHmac stays true after the value is taken', () => {
    const raw = '{"current":"' + 'e'.repeat(64) + '"}';
    withFreshConfig({ GATEWAY_HANDOFF_HMAC: raw }, (config) => {
      expect(config.hasGatewayHandoffHmac).toBe(true);
      config.takeGatewayHandoffHmac();
      expect(config.hasGatewayHandoffHmac).toBe(true);
    });
  });
});
