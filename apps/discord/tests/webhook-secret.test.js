describe('qURL webhook secret trust boundary', () => {
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

  it.each([undefined, null, ''])('preserves pure-BYOK startup for absent value %p', (value) => {
    const { assertConfiguredWebhookSecret } = require('../src/utils/webhook-secret');
    expect(assertConfiguredWebhookSecret(value)).toBe(false);
  });

  it.each([
    'PLACEHOLDER',
    ' placeholder\n',
    '   ',
  ])('fails server startup before accepting a configured untrusted secret: %s', (value) => {
    jest.resetModules();
    jest.doMock('../src/config', () => ({ QURL_WEBHOOK_SECRET: value }));

    expect(() => require('../src/server'))
      .toThrow(/QURL_WEBHOOK_SECRET/);
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
it.each(['whsec_1234567890abcdef', 'new-format-server-secret', ' server-key-bytes '])('accepts persisted usable response on restart: %s', (value) => {
  const { assertConfiguredWebhookSecret, assertUsableResponseSecret } = require('../src/utils/webhook-secret');
  expect(assertUsableResponseSecret(value, 'rotateSecret')).toBe(value);
  expect(assertConfiguredWebhookSecret(value)).toBe(true);
});
});
