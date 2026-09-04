// Shared bot-side transport guard for resource IDs. Canonical public keys use
// unpadded base64url and CRIDs use lowercase unpadded base32, both subsets of
// [\w-]. The generous ceiling bounds request URLs without duplicating the
// service's exact length grammar; qurl-service owns semantic validation.
// TODO(upstream-contract): Keep this alphabet superset and safety ceiling
// aligned with the ResourceId schema in qurl-service's api/openapi.yaml.
const MAX_RESOURCE_ID_LENGTH = 1024;
const { createHash } = require('crypto');

const RESOURCE_ID_CHARACTER_CLASS = '[\\w-]';
const RESOURCE_ID_CHARACTERS = new RegExp(`^${RESOURCE_ID_CHARACTER_CLASS}+$`);
const LEGACY_RESOURCE_ID_PREFIX = 'r_';
// TODO(upstream-contract): qurl-service owns this lowercase bearer-token
// prefix. Reject it before building an HTTP path so a cross-wired qURL fragment
// cannot reach intermediary access logs. Matching is deliberately case-exact:
// issued access tokens are lowercase and public IDs remain opaque.
const QURL_ACCESS_TOKEN_PREFIX = 'at_';
const RESOURCE_PATH_PREFIX = '/resources/';
const RESOURCE_ID_MASK = '<id>';
// Shared sticky state is safe while this helper stays synchronous: lastIndex
// is assigned immediately before every exec, so no caller can interleave.
const RESOURCE_ID_SCAN = new RegExp(`${RESOURCE_ID_CHARACTER_CLASS}+`, 'y');
// Status returns the qURL aggregate response shape; whole-resource revoke uses
// the resource action. Keeping both names here makes that split deliberate.
const QURL_PATH_PREFIX = '/qurls/';

function hasSafeResourceIdShape(resourceId) {
  return typeof resourceId === 'string'
    && resourceId.length > 0
    && resourceId.length <= MAX_RESOURCE_ID_LENGTH
    && RESOURCE_ID_CHARACTERS.test(resourceId)
    && !resourceId.startsWith(QURL_ACCESS_TOKEN_PREFIX);
}

// Deliberately shape-only: consulting constructors or stringifying objects can
// invoke caller-controlled getters and can re-expose a redacted response body.
function typePreview(value) {
  return `<${value === null ? 'null' : typeof value}>`;
}

function resourceIdLogRef(resourceId) {
  if (typeof resourceId !== 'string') return typePreview(resourceId);
  return `sha256:${createHash('sha256').update(resourceId, 'utf8').digest('hex').slice(0, 12)}`;
}

function validateResourceId(resourceId) {
  if (!hasSafeResourceIdShape(resourceId)) {
    // Keep this helper pure and the error generic: the rejected value may be a
    // bearer credential, including under a future namespace unknown here.
    throw new Error('Invalid resource ID format');
  }
}

// Path builders validate structurally and intentionally avoid encoding because
// the transport guard limits IDs to URL-safe bytes.
function resourcePath(resourceId) {
  validateResourceId(resourceId);
  return `${RESOURCE_PATH_PREFIX}${resourceId}`;
}

function qurlPath(resourceId) {
  validateResourceId(resourceId);
  return `${QURL_PATH_PREFIX}${resourceId}`;
}

function maskResourceIdPath(message) {
  let masked = typeof message === 'string'
    ? message
    : typePreview(message);
  if (!masked) return '<empty>';

  for (const prefix of [RESOURCE_PATH_PREFIX, QURL_PATH_PREFIX]) {
    let searchFrom = 0;
    for (;;) {
      const start = masked.indexOf(prefix, searchFrom);
      if (start === -1) break;

      const idStart = start + prefix.length;
      RESOURCE_ID_SCAN.lastIndex = idStart;
      const resourceId = RESOURCE_ID_SCAN.exec(masked)?.[0];
      if (!resourceId) {
        searchFrom = idStart;
        continue;
      }
      masked = `${masked.slice(0, idStart)}${RESOURCE_ID_MASK}${masked.slice(idStart + resourceId.length)}`;
      searchFrom = idStart + RESOURCE_ID_MASK.length;
    }
  }
  return masked;
}

module.exports = {
  hasSafeResourceIdShape,
  LEGACY_RESOURCE_ID_PREFIX,
  maskResourceIdPath,
  qurlPath,
  resourceIdLogRef,
  resourcePath,
  validateResourceId,
};
