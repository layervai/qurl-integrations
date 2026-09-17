// DELETE /resources returns 204 for a known revoked row. Keep an ambiguous 404
// (missing, wrong owner/key, or unresolved public ID) retryable; only the
// explicit gone status is safe to remove from the reclaim ledger.
const TERMINAL_RECLAIM_STATUS = 410;
// Exact fallback for serialized/rethrown failures that lost the structural
// status attached by qurlApiError. Loosening this changes which reclaim
// failures count as terminal success.
const QURL_API_STATUS_ERROR = /^qURL API [A-Z]+ \S+ failed \((\d{3})\)$/;

function qurlApiErrorMessage(method, path, statusOrCode) {
  return `qURL API ${method} ${path} failed (${statusOrCode})`;
}

function qurlApiError(method, path, statusOrCode) {
  const error = new Error(qurlApiErrorMessage(method, path, statusOrCode));
  if (Number.isInteger(statusOrCode) && statusOrCode > 0) error.status = statusOrCode;
  return error;
}

function qurlApiErrorStatus(error) {
  if (Number.isInteger(error?.status) && error.status > 0) return error.status;
  const message = typeof error === 'string' ? error : error?.message;
  const match = typeof message === 'string' ? message.match(QURL_API_STATUS_ERROR) : null;
  return match ? Number(match[1]) : null;
}

function isGoneQurlApiError(error) {
  return qurlApiErrorStatus(error) === TERMINAL_RECLAIM_STATUS;
}

module.exports = {
  isGoneQurlApiError,
  qurlApiError,
  qurlApiErrorMessage,
  qurlApiErrorStatus,
};
