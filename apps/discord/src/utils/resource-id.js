// Shared bot-side transport guard for resource IDs. Canonical public keys use
// unpadded base64url and CRIDs use lowercase unpadded base32, both subsets of
// [\w-]. The generous ceiling bounds request URLs without duplicating the
// service's exact length grammar; qurl-service owns semantic validation.
// TODO(upstream-contract): Keep this alphabet superset and safety ceiling
// aligned with the ResourceId schema in qurl-service's api/openapi.yaml.
const MAX_RESOURCE_ID_LENGTH = 1024;
// Accepted IDs can appear at their full (bounded) length in API diagnostics;
// this smaller preview applies only to values the transport guard rejects.
const ERROR_PREVIEW_LENGTH = 64;
const RESOURCE_ID_CHARACTERS = /^[\w-]+$/;
const RESOURCE_PATH_PREFIX = '/resources/';
const RESOURCE_ID_MASK = '<id>';
// Status returns the qURL aggregate response shape; whole-resource revoke uses
// the resource action. Keeping both names here makes that split deliberate.
const QURL_PATH_PREFIX = '/qurls/';

function hasSafeResourceIdShape(resourceId) {
  return typeof resourceId === 'string'
    && resourceId.length > 0
    && resourceId.length <= MAX_RESOURCE_ID_LENGTH
    && RESOURCE_ID_CHARACTERS.test(resourceId);
}

function typePreview(value) {
  return `<${value === null ? 'null' : typeof value}>`;
}

function validateResourceId(resourceId) {
  if (!hasSafeResourceIdShape(resourceId)) {
    const preview = typeof resourceId === 'string'
      ? resourceId.slice(0, ERROR_PREVIEW_LENGTH)
      : typePreview(resourceId);
    throw new Error(`Invalid resource ID format: ${preview}`);
  }
}

// Callers must pass validateResourceId first; path helpers intentionally avoid
// encoding because the transport guard already limits IDs to URL-safe bytes.
function resourcePath(resourceId) {
  return `${RESOURCE_PATH_PREFIX}${resourceId}`;
}

function qurlPath(resourceId) {
  return `${QURL_PATH_PREFIX}${resourceId}`;
}

function maskResourceIdPath(message) {
  let masked = typeof message === 'string'
    ? message
    : typePreview(message);
  let searchFrom = 0;

  for (;;) {
    const start = masked.indexOf(RESOURCE_PATH_PREFIX, searchFrom);
    if (start === -1) return masked;

    const idStart = start + RESOURCE_PATH_PREFIX.length;
    const resourceId = masked.slice(idStart).match(/^\S+/)?.[0];
    if (!resourceId) {
      searchFrom = idStart;
      continue;
    }
    masked = `${masked.slice(0, idStart)}${RESOURCE_ID_MASK}${masked.slice(idStart + resourceId.length)}`;
    searchFrom = idStart + RESOURCE_ID_MASK.length;
  }
}

module.exports = {
  hasSafeResourceIdShape,
  maskResourceIdPath,
  qurlPath,
  resourcePath,
  validateResourceId,
};
