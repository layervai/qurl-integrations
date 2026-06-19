/**
 * Pins the @layervai/qurl export contract that connector.js and qurl.js use.
 *
 * Keep this as a small, unmocked package-contract test. It intentionally
 * overlaps with connector-coverage.test.js's jest.requireActual pagination
 * smoke test, but stays discoverable and also pins the qurl.js error-code
 * exports that drive status-0 error classification.
 */
describe('@layervai/qurl export contract', () => {
  it('exports the constructor and error codes used by the bot', () => {
    const {
      QURLClient,
      ERROR_CODE_NETWORK,
      ERROR_CODE_TIMEOUT,
      ERROR_CODE_CLIENT_VALIDATION,
    } = require('@layervai/qurl');

    const client = new QURLClient({
      apiKey: 'test-key',
      baseUrl: 'https://qurl.invalid',
    });

    expect(client).toBeInstanceOf(QURLClient);
    for (const errorCode of [
      ERROR_CODE_NETWORK,
      ERROR_CODE_TIMEOUT,
      ERROR_CODE_CLIENT_VALIDATION,
    ]) {
      expect(typeof errorCode).toBe('string');
      expect(errorCode).toBeTruthy();
    }
  });
});
