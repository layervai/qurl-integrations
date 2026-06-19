/**
 * Pins the @layervai/qurl export contract that connector.js and qurl.js
 * construct against.
 *
 * Every other spec MOCKS `@layervai/qurl` (e.g. connector-coverage.test.js),
 * and a name-keyed mock satisfies whatever name the code imports — so it can't
 * catch an import-name typo. That's exactly how the `QurlClient` vs `QURLClient`
 * bug stayed green. This spec drives the REAL package (no mock), so it fails if
 * the SDK renames or drops the class. It is the one test that would have failed
 * on `main` before this fix, and it's immune to mock drift.
 */
describe('@layervai/qurl export contract', () => {
  it('exports QURLClient as a constructor', () => {
    const sdk = require('@layervai/qurl');
    expect(typeof sdk.QURLClient).toBe('function');
  });
});
