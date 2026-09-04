const {
  QURLClient,
  ERROR_CODE_NETWORK,
  ERROR_CODE_TIMEOUT,
  ERROR_CODE_CLIENT_VALIDATION,
} = require('@layervai/qurl');
const config = require('./config');
const logger = require('./logger');
const { AUDIT_EVENTS } = require('./constants');
const dns = require('dns').promises;

const { isPrivateHost } = require('./utils/private-host');

/**
 * qURL API client for the bot's link create / status / revoke calls, backed by
 * the @layervai/qurl SDK where it exposes the required route. This remains the
 * command-side client consolidated in #830; the small GET /v1/me shim below
 * uses fetch because SDK 0.3.x has no identity method.
 *
 * This module adds only the concerns the SDK doesn't own:
 *   - the DEPENDENCY_AUTH_FAILURE audit emit on 401/403 (emit-once) and
 *     error-body redaction — in logs and in the errors it throws — see callQurl();
 *   - the SSRF guards for the user-supplied create target (isPrivateHost +
 *     assertNotPrivateAfterResolve), which are client-independent.
 */

// Request timeout (per-attempt for the SDK paths, whole-request for the
// no-retry getIdentity shim) + SDK retry budget. Pins the SDK's resilience to the budget
// the hand-rolled client documented before this consolidation: "3 attempts
// total (initial + 2 retries)". `maxRetries` counts RETRIES, so 2 ⇒ 3 total
// attempts; `timeout` is the per-attempt deadline (matching the old
// AbortSignal.timeout(30000)). We pin both rather than inherit SDK defaults so
// a future default drift can't silently change this path's behavior.
// (connector.js's resolve path pins maxRetries:3 — a separate call site we
// deliberately leave untouched here.)
const REQUEST_TIMEOUT_MS = 30000;
const MAX_RETRIES = 2;
// User-Agent the qURL service sees for the bot's calls. Preserved verbatim
// across the SDK migration (a literal wire identifier — see CLAUDE.md).
const USER_AGENT = 'qurl-discord-bot/1.0';

// status-0 SDK error codes whose message the SDK synthesizes itself (no server
// body) — the only status-0 errors callQurl surfaces verbatim. See its
// REDACTION note: anything else at status 0 is re-wrapped, so the no-body-leak
// invariant holds structurally rather than by trusting SDK internals.
const SAFE_STATUS0_CODES = new Set([
  ERROR_CODE_NETWORK,
  ERROR_CODE_TIMEOUT,
  ERROR_CODE_CLIENT_VALIDATION,
]);

// Construct a per-call SDK client. Per-call (not cached) because each call
// carries its own apiKey (the bot is multi-tenant) and because these are rare
// control-plane calls, not a hot path; constructing here also means the client
// binds the live globalThis.fetch at call time. baseUrl is the bare API origin
// — the SDK prepends `/v1/...` itself.
function makeClient(apiKey) {
  const key = apiKey || config.QURL_API_KEY;
  if (!key) {
    throw new Error('QURL_API_KEY is not configured');
  }
  return new QURLClient({
    apiKey: key,
    baseUrl: config.QURL_ENDPOINT,
    timeout: REQUEST_TIMEOUT_MS,
    maxRetries: MAX_RETRIES,
    userAgent: USER_AGENT,
  });
}

/**
 * Run a qURL call, layering on the bot-specific behaviors the SDK doesn't own.
 * `method`/`path` are labels for the audit/log/error payload (the same
 * dependency/method/path shape the pre-SDK client emitted) — the callee owns
 * the actual wire path. The raw-fetch identity call sets the same `.status`
 * error field as the SDK so it receives the same audit and redaction behavior.
 *
 *   - AUDIT: emit DEPENDENCY_AUTH_FAILURE on a 401/403 so the dependency-auth
 *     alarm fires independently of any caller's catch path.
 *   - EMIT-ONCE INVARIANT: the SDK never retries 401/403 (its retryable set is
 *     {429, 502, 503, 504}), so this fires once per request, not once per
 *     attempt. If that ever changes, the audit count would multiply on a single
 *     auth failure. Pinned by tests/qurl-coverage.test.js.
 *   - REDACTION: never let a qURL error body escape this module. On an
 *     HTTP-status failure the SDK's `QURLError.message` is `Title (status):
 *     detail`, where `detail` is parsed from the server body (which can echo
 *     request headers or tokens). So for any positive status we log only status
 *     + code and re-throw a status-only Error (callers such as the revoke path
 *     log the thrown `.message`, so the body must not reach it). At status 0,
 *     a coded SDK error outside the body-free SAFE set (see SAFE_STATUS0_CODES)
 *     — e.g. an unexpected-response shape error that could embed a body snippet
 *     — is re-wrapped to a code-only message, so the invariant holds structurally
 *     rather than by trusting SDK internals. Body-free SDK errors (network /
 *     timeout / client-validation) and non-SDK throws (programming errors, which
 *     carry no server body) propagate verbatim so their stack survives. Pinned
 *     by tests/qurl-coverage.test.js.
 */
async function callQurl(method, path, fn) {
  try {
    return await fn();
  } catch (err) {
    // The SDK uses status 0 for its client-side validation / network / timeout
    // errors; a positive status is a real HTTP status from the API.
    const status = Number.isInteger(err?.status) ? err.status : 0;
    // Redaction: status + error code only — never err.message / err.detail.
    logger.debug('qURL API error', { method, path, status, code: err?.code });
    if (status === 401 || status === 403) {
      logger.audit(AUDIT_EVENTS.DEPENDENCY_AUTH_FAILURE, {
        dependency: 'qurl_service',
        status,
        method,
        path,
      });
    }
    // A real HTTP status means the SDK error wraps a server response body — throw
    // a status-only error so that body can't leak through a caller that logs
    // `err.message`.
    if (status > 0) {
      const sanitizedError = new Error(`qURL API ${method} ${path} failed (${status})`);
      sanitizedError.status = status;
      throw sanitizedError;
    }
    // status 0: re-wrap ONLY a coded SDK error outside the body-free SAFE set —
    // i.e. one whose synthesized message could embed a body snippet (e.g.
    // `unexpected_response`). Defense-in-depth: the SDK doesn't embed bodies in
    // status-0 messages today, but we don't rely on it. A body-free SDK error
    // (network / timeout / client-validation) or a non-SDK throw (a programming
    // error like a TypeError, which carries no server body) propagates verbatim,
    // so its message and stack survive for debugging.
    if (typeof err?.code === 'string' && !SAFE_STATUS0_CODES.has(err.code)) {
      throw new Error(`qURL API ${method} ${path} failed (${err.code})`);
    }
    throw err;
  }
}

async function getIdentity(apiKey) {
  if (!apiKey) {
    throw new Error('Guild qURL API key is not configured');
  }

  // One attempt only: an interactive check surfaces a transient failure to the
  // admin rather than spending the 3-attempt budget MAX_RETRIES pins for the
  // SDK paths. Replace this shim when the SDK exposes GET /v1/me.
  //
  // Unlike makeClient, there is deliberately NO `apiKey || config.QURL_API_KEY`
  // fallback: a guild status check must validate the guild's own stored key,
  // never the bot's, or a guild with no key would read as configured.
  return callQurl('GET', '/me', async () => {
    const endpoint = config.QURL_ENDPOINT.replace(/\/+$/, '');
    const response = await globalThis.fetch(`${endpoint}/v1/me`, {
      method: 'GET',
      headers: {
        Authorization: `Bearer ${apiKey}`,
        Accept: 'application/json',
        'User-Agent': USER_AGENT,
      },
      signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
    });
    if (!response.ok) {
      try {
        await response.body?.cancel();
      } catch {}
      const error = new Error('qURL identity request failed');
      error.status = response.status;
      throw error;
    }

    let body;
    try {
      body = await response.json();
    } catch {
      throw new Error('qURL identity response was not valid JSON');
    }
    // TODO(upstream-contract): mirrors qurl-service's GET /v1/me envelope.
    const identity = body?.data;
    const key = identity?.api_key;
    if (typeof key?.key_prefix !== 'string' || !Array.isArray(key.scopes) ||
        key.scopes.some(scope => typeof scope !== 'string')) {
      throw new Error('qURL identity response had an unexpected shape');
    }
    return identity;
  });
}

// The syntactic private/loopback/link-local screen lives in utils/private-host.js
// so the boot path can consume the same range table without pulling in the
// @layervai/qurl SDK, constants.js and `dns` that this module requires. Re-exported
// here (see module.exports) because connector.js imports it from qurl.js.
//
// The classifier stays pure. Each rejection site below logs the host that
// triggered it before returning the deliberately shape-independent error to the
// caller; connector.js does the same for its detect guard. URL parsing strips
// CR/LF from hostname input, and logger.js JSON-encodes metadata, so those
// breadcrumbs cannot forge log lines.

// Resolve all A/AAAA records for a hostname and reject if ANY of them point
// to a private/internal range. Defense against DNS rebinding: a malicious
// domain could answer `isPrivateHost` (which is syntactic) with a public IP
// but then resolve to 169.254.169.254 or 127.0.0.1 at fetch time on the
// qURL backend. We resolve up-front and pass the result to the qURL API so
// the backend can pin to the same IPs we verified — the API has its own
// SSRF guard but we also block here.
//
// DEPENDENCY: This is defense-in-depth ONLY — there is an unavoidable
// TOCTOU window between our dns.lookup() here and the actual fetch on the
// qURL API backend (DNS can rebind in that gap). The qURL API MUST have
// its own DNS-level SSRF guard (resolve + check in the same syscall, or
// IP-pinned fetch). Do not remove this check assuming the API layer is
// enough, and do not remove the API-layer check assuming this is enough.
async function assertNotPrivateAfterResolve(hostname) {
  // Numeric hosts already covered by syntactic isPrivateHost; only resolve
  // actual names. IPv6-in-brackets is stripped in isPrivateHost already.
  if (/^\d+\.\d+\.\d+\.\d+$/.test(hostname) || hostname.startsWith('[')) return;
  let addrs;
  try {
    addrs = await dns.lookup(hostname, { all: true, verbatim: true });
  } catch (err) {
    // Resolution failure is NOT a pass — reject so a typo or non-existent
    // host fails here rather than leaking to the qURL API as an opaque error.
    throw new Error(`Target URL hostname could not be resolved: ${err.code || err.message}`);
  }
  for (const { address } of addrs) {
    if (isPrivateHost(address)) {
      // Name the resolved address as well as the host: on this leg the hostname
      // alone doesn't explain the rejection (it looked public syntactically), so
      // the address distinguishes a rebinding attempt from a name that
      // legitimately points inside. dns.lookup() returns an inet_ntop-rendered
      // IP string, so it is safe to include in structured log metadata.
      logger.warn('Target URL rejected by SSRF guard (DNS resolved to a private address)', {
        hostname,
        address,
      });
      throw new Error('Target URL points to a private/internal address');
    }
  }
}

async function createOneTimeLink(targetUrl, expiresIn, label, apiKey) {
  try {
    const parsed = new URL(targetUrl);
    if (!['http:', 'https:'].includes(parsed.protocol)) {
      throw new Error('Only http/https URLs are allowed');
    }
    if (isPrivateHost(parsed.hostname)) {
      // Keep this distinct from the DNS-leg message so operators can tell which
      // guard fired. Warn deliberately: a typo and an SSRF probe are
      // indistinguishable here, and Discord's command rate limits bound volume.
      logger.warn('Target URL rejected by SSRF guard (private host literal)', {
        hostname: parsed.hostname,
      });
      throw new Error('Target URL points to a private/internal address');
    }
    await assertNotPrivateAfterResolve(parsed.hostname);
  } catch (err) {
    if (/(http|private|resolved)/i.test(err.message)) throw err;
    throw new Error(`Invalid target URL: ${err.message}`);
  }

  const client = makeClient(apiKey);
  const result = await callQurl('POST', '/qurls', () =>
    client.create({
      target_url: targetUrl,
      one_time_use: true,
      expires_in: expiresIn,
      // The create endpoint uses `label`, not `description` (qurl-service
      // CreateQurlRequest); the SDK rejects a `description` here as an unknown
      // field, so don't send one.
      label,
    }),
  );

  logger.info('Created one-time qURL', { resource_id: result.resource_id, expires_in: expiresIn });
  return result;
}

// Bot-side charset guard on the resource ID, independent of the SDK client (in
// the same defense-in-depth spirit as the SSRF guards): rejects malformed IDs
// with a stable bot-side message before any network work. The SDK's delete()
// adds the semantic `r_` resource-ID check on top.
function validateResourceId(resourceId) {
  if (!resourceId || !/^[\w-]+$/.test(resourceId)) {
    throw new Error(`Invalid resource ID format: ${resourceId}`);
  }
}

async function deleteLink(resourceId, apiKey) {
  validateResourceId(resourceId);
  const client = makeClient(apiKey);
  // delete() requires a qurl-service resource ID (r_ prefix); the bot's send
  // rows store exactly that, so the revoke path satisfies it.
  await callQurl('DELETE', `/qurls/${resourceId}`, () => client.delete(resourceId));
  logger.info('Revoked qURL', { resource_id: resourceId });
}

async function getResourceStatus(resourceId, apiKey) {
  validateResourceId(resourceId);
  const client = makeClient(apiKey);
  // Returns the SDK's QURL shape — access tokens are under `access_tokens`
  // (the SDK renames the API's wire-format `qurls` field).
  return callQurl('GET', `/qurls/${resourceId}`, () => client.get(resourceId));
}

module.exports = { createOneTimeLink, deleteLink, getResourceStatus, getIdentity, isPrivateHost };
