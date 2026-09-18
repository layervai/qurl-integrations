// DELETE /resources returns 204 for a known revoked row. Keep an ambiguous 404
// (missing, wrong owner/key, or unresolved public ID) retryable; only the
// explicit gone status is safe to remove from the reclaim ledger.
const TERMINAL_RECLAIM_STATUS = 410;
// Exact parse of qurlApiErrorMessage, the fallback for serialized/rethrown
// failures that lost the structural status attached by qurlApiError.
// Loosening this changes which reclaim failures count as terminal success.
const QURL_API_ERROR = /^qURL API [A-Z]+ \S+ failed \(([^()]+)\)$/;

function qurlApiErrorCode(error) {
  const message = typeof error === 'string' ? error : error?.message;
  return typeof message === 'string' ? message.match(QURL_API_ERROR)?.[1] ?? null : null;
}

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
  const code = qurlApiErrorCode(error);
  return code !== null && /^\d{3}$/.test(code) ? Number(code) : null;
}

// callQurl re-wraps an SDK client-side rejection (status 0) with this code.
// The literal mirrors the SDK's ERROR_CODE_CLIENT_VALIDATION; a test pins the
// two together.
function isClientValidationQurlApiError(error) {
  return qurlApiErrorCode(error) === 'client_validation';
}

function isGoneQurlApiError(error) {
  return qurlApiErrorStatus(error) === TERMINAL_RECLAIM_STATUS;
}

module.exports = {
  isClientValidationQurlApiError,
  isGoneQurlApiError,
  qurlApiError,
  qurlApiErrorMessage,
  qurlApiErrorStatus,
};
