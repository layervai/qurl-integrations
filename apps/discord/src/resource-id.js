// Shared bot-side transport guard for resource IDs. Canonical public keys use
// unpadded base64url and CRIDs use lowercase unpadded base32, both subsets of
// [\w-]. The generous ceiling bounds request URLs without duplicating the
// service's exact length grammar; qurl-service owns semantic validation.
// TODO(upstream-contract): Keep this alphabet superset and safety ceiling
// aligned with the ResourceId schema in qurl-service's api/openapi.yaml.
const MAX_RESOURCE_ID_LENGTH = 256;
const RESOURCE_ID_CHARACTERS = /^[\w-]+$/;

function hasSafeResourceIdShape(resourceId) {
  return typeof resourceId === 'string'
    && resourceId.length > 0
    && resourceId.length <= MAX_RESOURCE_ID_LENGTH
    && RESOURCE_ID_CHARACTERS.test(resourceId);
}

function validateResourceId(resourceId) {
  if (!hasSafeResourceIdShape(resourceId)) {
    throw new Error(`Invalid resource ID format: ${resourceId}`);
  }
}

module.exports = { hasSafeResourceIdShape, validateResourceId };
