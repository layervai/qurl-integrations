jest.mock('../src/logger', () => ({
  info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(), audit: jest.fn(),
}));

const { captureFreshConfig } = require('./helpers/fresh-config');

describe('qURL webhook secret trust boundary', () => {
  beforeEach(() => jest.clearAllMocks());
  afterEach(() => {
    jest.resetModules();
    jest.dontMock('../src/config');
  });

  it.each([
    ['unset', undefined, false],
    ['empty', '', false],
    ['terraform sentinel', 'PLACEHOLDER', false],
    ['case-variant sentinel', ' placeholder\n', false],
    ['arbitrary value', 'legacy-secret-value', false],
    ['bare prefix', 'whsec_', false],
    ['too-short prefixed value', `whsec_${'x'.repeat(15)}`, false],
    ['spaces after prefix', `whsec_${' '.repeat(16)}`, false],
    ['server-issued shape', `whsec_${'a'.repeat(16)}`, true],
  ])('classifies %s without weakening the allowlist', (_label, value, expected) => {
    const {
      isInfraSeedSentinel,
      isServerIssuedSecret,
    } = require('../src/utils/webhook-secret');

    expect(isServerIssuedSecret(value)).toBe(expected);
    expect(isInfraSeedSentinel(value)).toBe(
      _label === 'terraform sentinel' || _label === 'case-variant sentinel',
    );
  });

  it('shares the single infra seed sentinel with the Maps boot check', () => {
    const { INFRA_SEED_SENTINEL, isInfraSeedSentinel } = require('../src/utils/webhook-secret');
    const { GOOGLE_MAPS_API_KEY_PLACEHOLDER_SENTINEL } = require('../src/boot-requirements');
    expect(GOOGLE_MAPS_API_KEY_PLACEHOLDER_SENTINEL).toBe(INFRA_SEED_SENTINEL);
    expect(isInfraSeedSentinel(GOOGLE_MAPS_API_KEY_PLACEHOLDER_SENTINEL)).toBe(true);
  });

  it.each([undefined, null, ''])('preserves pure-BYOK startup for absent value %p', (value) => {
    const { assertConfiguredWebhookSecret } = require('../src/utils/webhook-secret');
    expect(assertConfiguredWebhookSecret(value)).toBe(false);
  });

  it.each([
    ['padded server secret', ' whsec_1234567890abcdef\n', 'whsec_1234567890abcdef'],
    ['whitespace-only', '   ', ''],
  ])('trims outer whitespace on config read — %s', (_label, raw, expected) => {
    captureFreshConfig({ QURL_WEBHOOK_SECRET: raw }, (cfg) => {
      expect(cfg.QURL_WEBHOOK_SECRET).toBe(expected);
    });
  });

  it.each([
    'PLACEHOLDER',
    ' placeholder\n',
  ])('fails receiver-tier startup before listening on a configured untrusted secret: %s', (value) => {
    jest.resetModules();
    jest.doMock('../src/config', () => ({ ...jest.requireActual('../src/config'), QURL_WEBHOOK_SECRET: value }));

    // Loading the module must not throw: gateway-only tasks require server.js
    // too and never verify webhook signatures. Only startServer() (http/
    // combined) gates, and it must do so before the listener binds.
    const { app, startServer } = require('../src/server');
    const listen = jest.spyOn(app, 'listen').mockImplementation(() => { throw new Error('listener must not bind'); });
    expect(startServer).toThrow(/QURL_WEBHOOK_SECRET/);
    expect(listen).not.toHaveBeenCalled();
  });

  it.each([42, true, {}])('rejects wrong-type responses: %p', (value) => {
    const { assertUsableResponseSecret } = require('../src/utils/webhook-secret');
    expect(() => assertUsableResponseSecret(value, 'rotateSecret'))
      .toThrow(new RegExp(`rotateSecret.*wrong type ${typeof value}.*whsec_`));
  });

  it.each(['PLACEHOLDER', ' placeholder\n'])('rejects public seed responses: %p', (value) => {
    const { assertUsableResponseSecret } = require('../src/utils/webhook-secret');
    expect(() => assertUsableResponseSecret(value, 'rotateSecret'))
      .toThrow(/rotateSecret.*public infrastructure seed sentinel/);
  });

  it('does not echo an invalid configured secret in its startup error', () => {
    const { assertConfiguredWebhookSecret } = require('../src/utils/webhook-secret');
    const value = '  PLACEHOLDER  ';
    expect(() => assertConfiguredWebhookSecret(value)).toThrow(/QURL_WEBHOOK_SECRET/);
    try {
      assertConfiguredWebhookSecret(value);
    } catch (err) {
      expect(err.message).not.toContain(value);
    }
  });

  // Format drift is intentionally accepted after the upstream rotation commits.
  it.each([
    ['whsec_1234567890abcdef', 0],
    ['new-format-server-secret', 1],
    [' server-key-bytes ', 1],
  ])('accepts persisted usable response on restart: %s (drift warnings: %i)', (value, warnings) => {
    const { assertConfiguredWebhookSecret, assertUsableResponseSecret } = require('../src/utils/webhook-secret');
    const logger = require('../src/logger');
    expect(assertUsableResponseSecret(value, 'rotateSecret')).toBe(value);
    expect(assertConfiguredWebhookSecret(value)).toBe(true);
    expect(logger.warn).toHaveBeenCalledTimes(warnings);
    expect(JSON.stringify(logger.warn.mock.calls)).not.toContain(value.trim());
  });
});
