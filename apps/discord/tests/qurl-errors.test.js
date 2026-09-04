const {
  isGoneQurlApiError,
  qurlApiError,
  qurlApiErrorMessage,
  qurlApiErrorStatus,
} = require('../src/utils/qurl-errors');
const { resourcePath } = require('../src/utils/resource-id');
const { CRID_RESOURCE_ID } = require('./helpers/qurl-fixtures');

describe('qURL API error contract', () => {
  it('formats and classifies terminal failures structurally or after serialization', () => {
    const path = resourcePath(CRID_RESOURCE_ID);
    const gone = qurlApiErrorMessage('DELETE', path, 404);

    // Load-bearing literal: this pins the wire-error family independently of
    // the shared formatter used by reclaim's mock fixtures and expectations.
    expect(gone).toBe(`qURL API DELETE /resources/${CRID_RESOURCE_ID} failed (404)`);
    expect(isGoneQurlApiError(new Error(gone))).toBe(true);
    expect(isGoneQurlApiError(`${gone} request-id=123`)).toBe(false);

    const wrapped = qurlApiError('DELETE', path, 410);
    wrapped.message += ' request-id=123';
    expect(isGoneQurlApiError(wrapped)).toBe(true);
    expect(qurlApiErrorStatus(wrapped)).toBe(410);
  });

  it('does not attach or infer status for a status-0 SDK error code', () => {
    const error = qurlApiError('GET', '/qurls/id', 'unexpected_response');

    expect(error.status).toBeUndefined();
    expect(qurlApiErrorStatus(error)).toBeNull();
    expect(isGoneQurlApiError(error)).toBe(false);
  });

  it('rejects unrelated and non-terminal status messages', () => {
    expect(isGoneQurlApiError('unrelated operation failed (404)')).toBe(false);
    expect(isGoneQurlApiError(qurlApiErrorMessage('DELETE', resourcePath('abc404def'), 500)))
      .toBe(false);
  });
});
