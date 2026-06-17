const { QURLClient } = require('@layervai/qurl');
const config = require('./config');
const logger = require('./logger');
const { AUDIT_EVENTS } = require('./constants');
const dns = require('dns').promises;

/**
 * qURL API client for the bot's link create / status / revoke calls, backed by
 * the @layervai/qurl SDK. This is the bot's single qURL client (issue #830 —
 * the prior hand-rolled `qurlFetch` is gone); the detect path in connector.js
 * uses the same SDK. This module adds only the concerns the SDK doesn't own:
 *   - the DEPENDENCY_AUTH_FAILURE audit emit on 401/403 (emit-once) and
 *     error-body redaction — in logs and in the errors it throws — see callQurl();
 *   - the SSRF guards for the user-supplied create target (isPrivateHost +
 *     assertNotPrivateAfterResolve), which are client-independent.
 */

// Per-attempt timeout + retry budget. Pins the SDK's resilience to the budget
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
 * Run an SDK call, layering on the bot-specific behaviors the SDK doesn't own.
 * `method`/`path` are labels for the audit/log/error payload (the same
 * dependency/method/path shape the pre-SDK client emitted) — the SDK owns the
 * actual wire path.
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
 *     log the thrown `.message`, so the body must not reach it). status-0 errors
 *     propagate unchanged: the SDK uses status 0 only for network / timeout /
 *     client-validation / unexpected-shape errors, whose messages it synthesizes
 *     itself (never from a server body), so they're safe to surface and more
 *     useful than a generic string. Pinned by tests/qurl-coverage.test.js.
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
    // `err.message`. status 0 (no server body) propagates unchanged.
    if (status > 0) {
      throw new Error(`qURL API ${method} ${path} failed (${status})`);
    }
    throw err;
  }
}

// Reject hostnames that resolve (by syntax) to loopback, link-local, or
// RFC1918 private ranges. Defense-in-depth against a caller passing
// `http://169.254.169.254/latest/meta-data/...` or similar; even if the
// downstream qURL API is the one that actually fetches, we block at our
// own input validation layer.
function isPrivateHost(host) {
  if (!host) return true;
  const h = host.toLowerCase();
  if (h === 'localhost' || h === '0.0.0.0' || h === '::' || h === '::1') return true;
  if (h.startsWith('[') && h.endsWith(']')) {
    // Bracketed IPv6 literal — strip and check.
    return isPrivateHost(h.slice(1, -1));
  }
  // IPv6 locals reach here bracket-stripped, so they always contain a ':':
  // unique-local fc00::/7 (fc/fd), link-local fe80::/10, and deprecated
  // site-local fec0::/10 — the latter two span first-hextet fe80–feff, i.e.
  // `fe[89a-f][0-9a-f]:` (a real /10 literal always writes the full 4-digit
  // hextet). Gate on the ':' so a PUBLIC DNS name that merely starts with these
  // letters (e.g. `fd-cdn.example.com`, reaching here UNbracketed) is NOT
  // misclassified as an IPv6 local literal — DNS names never contain a colon.
  if (h.includes(':')) {
    if (h.startsWith('fc') || h.startsWith('fd')) return true;  // fc00::/7 unique-local
    if (/^fe[89a-f][0-9a-f]:/.test(h)) return true;             // fe80::/10 + fec0::/10 site-local
  }
  // IPv4-mapped IPv6 literal: ::ffff:127.0.0.1, ::ffff:7f00:1, etc. Strip the
  // prefix (URL parsing already stripped the brackets) and re-check.
  const mapped = h.match(/^::ffff:([0-9.]+)$/);
  if (mapped) return isPrivateHost(mapped[1]);
  // Decimal IPv4 literal (e.g. `2130706433` = 127.0.0.1) — browsers accept,
  // Node's URL does too. Convert to dotted-quad.
  if (/^\d+$/.test(h)) {
    const n = Number(h);
    if (n >= 0 && n <= 0xFFFFFFFF) {
      const dotted = [(n >>> 24) & 0xFF, (n >>> 16) & 0xFF, (n >>> 8) & 0xFF, n & 0xFF].join('.');
      return isPrivateHost(dotted);
    }
    return true; // out-of-range numeric host: reject outright
  }
  // Hex IPv4 literal (e.g. `0x7f000001` = 127.0.0.1)
  if (/^0x[0-9a-f]+$/.test(h)) {
    const n = Number(h);
    if (Number.isFinite(n) && n >= 0 && n <= 0xFFFFFFFF) {
      const dotted = [(n >>> 24) & 0xFF, (n >>> 16) & 0xFF, (n >>> 8) & 0xFF, n & 0xFF].join('.');
      return isPrivateHost(dotted);
    }
    return true;
  }
  // Octal-prefixed IPv4 (e.g. `0177.0.0.1`) — treat any leading-zero component
  // as suspicious and reject conservatively.
  if (/^0\d/.test(h) && /^[0-9.]+$/.test(h)) return true;
  // Standard IPv4 dotted-quad
  const v4 = h.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);
  if (v4) {
    const [a, b] = v4.slice(1).map(Number);
    if (a === 10) return true;                          // 10.0.0.0/8
    if (a === 127) return true;                         // 127.0.0.0/8
    if (a === 169 && b === 254) return true;            // 169.254.0.0/16 (link-local, IMDS)
    if (a === 172 && b >= 16 && b <= 31) return true;   // 172.16.0.0/12
    if (a === 192 && b === 168) return true;            // 192.168.0.0/16
    if (a === 100 && b >= 64 && b <= 127) return true;  // 100.64.0.0/10 (CGNAT)
    if (a === 0) return true;                           // 0.0.0.0/8
    if (a >= 224) return true;                          // multicast + reserved
  }
  return false;
}

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

module.exports = { createOneTimeLink, deleteLink, getResourceStatus, isPrivateHost };
