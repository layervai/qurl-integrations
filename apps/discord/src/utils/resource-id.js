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

function hasSafeResourceIdShape(resourceId) {
  return typeof resourceId === 'string'
    && resourceId.length > 0
    && resourceId.length <= MAX_RESOURCE_ID_LENGTH
    && RESOURCE_ID_CHARACTERS.test(resourceId);
}

function validateResourceId(resourceId) {
  if (!hasSafeResourceIdShape(resourceId)) {
    const preview = typeof resourceId === 'string'
      ? resourceId.slice(0, ERROR_PREVIEW_LENGTH)
      : `<${typeof resourceId}>`;
    throw new Error(`Invalid resource ID format: ${preview}`);
  }
}

function resourcePath(resourceId) {
  return `${RESOURCE_PATH_PREFIX}${resourceId}`;
}

function maskResourceIdPath(message) {
  const start = message.indexOf(RESOURCE_PATH_PREFIX);
  if (start === -1) return message;

  const idStart = start + RESOURCE_PATH_PREFIX.length;
  const resourceId = message.slice(idStart).match(/^\S+/)?.[0];
  if (!resourceId) return message;
  return `${message.slice(0, idStart)}<id>${message.slice(idStart + resourceId.length)}`;
}

module.exports = {
  hasSafeResourceIdShape,
  maskResourceIdPath,
  resourcePath,
  validateResourceId,
};
