const { QURLClient } = require('@layervai/qurl');

const config = require('./config');
const logger = require('./logger');

// Reuse the security-critical, syntactic private/loopback/link-local IP guard
// from qurl.js rather than duplicating ~50 lines of IP-literal parsing that
// could drift out of sync. resolveDetectTarget() self-mints the ephemeral
// detect qURL via the @layervai/qurl SDK (the standardized client), not qurl.js.
// qurl.js has no connector.js dependency, so this require introduces no cycle.
const { isPrivateHost } = require('./qurl');

const { sanitizeFilename } = require('./utils/sanitize');
const { formatSessionDurationSeconds, isPositiveFinite } = require('./utils/time');

const { MAX_FILE_SIZE } = require('./constants');
const MAX_CDN_REDIRECTS = 3;

// Truncate the connector's MD5 of an uploaded file before logging. The full
// hash is treated as sensitive in our broader infrastructure; see internal
// security docs for the threat model. 8 hex chars preserves cross-system
// correlation. Single chokepoint — every upload-success log path goes through
// this helper. The truncation is load-bearing; don't inline `result.hash`
// back into a log call.
function md5Prefix(hash) {
  return typeof hash === 'string' ? hash.slice(0, 8) : undefined;
}

// Fetch from a Discord CDN URL with manual redirect handling. `redirect:
// 'error'` would refuse legitimate Discord redirects (cdn.discordapp.com
// sometimes 302s to media.discordapp.net). This walks the redirect chain
// ourselves, re-validating each Location header against ALLOWED_CDN_HOSTS
// (via isAllowedSourceUrl) so an attacker-controlled redirect target is
// still rejected.
async function cdnFetchFollowSafe(sourceUrl) {
  let url = sourceUrl;
  for (let hop = 0; hop <= MAX_CDN_REDIRECTS; hop++) {
    const resp = await fetch(url, { signal: AbortSignal.timeout(30000), redirect: 'manual' });
    if (resp.status >= 300 && resp.status < 400) {
      const loc = resp.headers.get('location');
      if (!loc) throw new Error(`CDN redirect without Location header (status ${resp.status})`);
      const next = new URL(loc, url).toString();
      if (!isAllowedSourceUrl(next)) {
        throw new Error('CDN redirect points outside the allowed host list');
      }
      url = next;
      continue;
    }
    return resp;
  }
  throw new Error('Too many CDN redirects');
}

// Log the raw connector body at debug level and throw a body-free Error.
// Mirrors qurl.js — connector responses may echo request headers/tokens, and
// upstream callers log err.message into application logs.
// qURL API / connector error codes the connector can pass back via the
// `error` string. Surface them as Error.apiCode so the caller can branch on a
// typed value instead of substring-matching the human-readable message (which
// the connector / upstream API can rephrase without notice).
const BATCH_CAP_EXCEEDED_PATTERNS = [
  // TODO(upstream-contract): keep this in lockstep with qurl-s3-connector's
  // meta-seal batch-cap error until the connector returns a typed code.
  /n must not exceed 1 when invisible watermarking is enabled/i,
];
const QUOTA_EXCEEDED_PATTERNS = [
  /quota[\s_-]?exceeded/i,
  /token limit per QURL reached/i,
  /per[\s_-]?resource (token|link|mint) (limit|cap)/i,
];

async function throwConnectorError(label, response) {
  let bodyText = '';
  let apiCode = null;
  let apiDetail = null;
  try {
    bodyText = await response.text();
    if (bodyText) {
      try {
        const parsed = JSON.parse(bodyText);
        // Connector wraps upstream API errors as `{success:false, error:"..."}`.
        // The wrapped string is what we pattern-match for known codes.
        const errStr = typeof parsed.error === 'string' ? parsed.error : '';
        if (BATCH_CAP_EXCEEDED_PATTERNS.some((rx) => rx.test(errStr))) {
          apiCode = 'batch_cap_exceeded';
          apiDetail = errStr || null;
        } else if (QUOTA_EXCEEDED_PATTERNS.some((rx) => rx.test(errStr))) {
          apiCode = 'quota_exceeded';
          apiDetail = errStr || null;
        }
      } catch { /* not JSON, ignore */ }
    }
  } catch { /* network read failed, fall through with empty body */ }
  logger.debug(`${label} error`, { status: response.status, apiCode, bodyLen: bodyText.length });
  const err = new Error(`${label} failed (${response.status})`);
  err.status = response.status;
  err.apiCode = apiCode;
  err.apiDetail = apiDetail;
  throw err;
}

// Read the response body chunk-by-chunk and abort as soon as we cross the cap.
// Guards against a CDN that returns a missing/incorrect Content-Length — the
// old code would buffer the whole body into memory before noticing it was
// oversized, which is an OOM vector if invoked concurrently.
async function readBodyWithCap(response, capBytes) {
  // Prefer streaming so we can abort as soon as we cross the cap — critical
  // for a lying/missing Content-Length. Fall back to arrayBuffer() when the
  // response lacks a readable body (e.g. jest mocks) and re-check size there.
  if (!response.body || typeof response.body.getReader !== 'function') {
    const buf = await response.arrayBuffer();
    if (buf.byteLength > capBytes) {
      throw new Error(`File too large: ${Math.round(buf.byteLength / 1024 / 1024)}MB, max ${Math.round(capBytes / 1024 / 1024)}MB`);
    }
    return buf;
  }
  const reader = response.body.getReader();
  let received = 0;
  const chunks = [];
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      received += value.byteLength;
      if (received > capBytes) {
        await reader.cancel();
        throw new Error(`File too large: > ${Math.round(capBytes / 1024 / 1024)}MB cap`);
      }
      chunks.push(value);
    }
  } finally {
    try { reader.releaseLock(); } catch { /* already released */ }
  }
  const out = new Uint8Array(received);
  let offset = 0;
  for (const c of chunks) { out.set(c, offset); offset += c.byteLength; }
  return out.buffer;
}

// Allowed Discord CDN domains for attachment URLs (SSRF prevention)
const ALLOWED_CDN_HOSTS = [
  'cdn.discordapp.com',
  'media.discordapp.net',
];

function isAllowedSourceUrl(sourceUrl) {
  try {
    const parsed = new URL(sourceUrl);
    // Reject URLs with userinfo or non-default ports: `https://cdn.discordapp.com@evil.com/...`
    // parses to hostname `evil.com`, and `cdn.discordapp.com:9999` would route to a
    // non-standard port. Only allow plain https on the default port.
    return (
      parsed.protocol === 'https:'
      && ALLOWED_CDN_HOSTS.includes(parsed.hostname)
      && !parsed.username
      && !parsed.password
      && (parsed.port === '' || parsed.port === '443')
    );
  } catch {
    return false;
  }
}

/**
 * Build auth headers for connector requests.
 * Uses the provided API key, or falls back to the global config key.
 */
function connectorAuthHeaders(apiKey) {
  const key = apiKey || config.QURL_API_KEY;
  const headers = {};
  if (key) {
    headers['Authorization'] = `Bearer ${key}`;
  }
  return headers;
}

// Append `viewer_ttl_seconds` to the multipart form when a positive value
// is provided. Centralized so all four upload paths (file initial,
// re-upload, file via Discord CDN download, JSON) thread the same wire
// field name. The connector validates the value (PR #477); we forward
// it as a string and let the connector own the contract.
//
// Asymmetry note: viewer_ttl_seconds is forwarded VERBATIM (so 0.5 →
// "0.5"; the fileviewer's client-side blank reads the value directly).
// The sibling formatSessionDurationSeconds() helper in utils/time.js
// FLOORS 0.5 to "1s" because qurl-service's MinSessionDuration is
// 1 * time.Second. A reader looking at one wire field should know the
// other has the opposite handling for the 0.5s preset.
function appendViewerTtl(form, viewerTtlSeconds) {
  // Strict positive-finite: isPositiveFinite filters non-numbers
  // (Number.isFinite('30') is false), zero, negatives, and ±Infinity
  // — same invariant the modal-prefill setValue uses.
  if (isPositiveFinite(viewerTtlSeconds)) {
    form.append('viewer_ttl_seconds', String(viewerTtlSeconds));
  }
}

/**
 * Upload a file to the qurl-s3-connector. Downloads from Discord CDN, then
 * uploads to the connector.
 *
 * @deprecated for NEW code — use `downloadAndUpload` which also returns the
 * buffered file so callers can re-upload without a second round trip. This
 * function is retained because its SSRF-rejection path is directly tested in
 * tests/connector-coverage.test.js and tests/send-pipeline-helpers.test.js, and those
 * cases would lose coverage if it were removed.
 */
async function uploadToConnector(sourceUrl, filename, contentType, apiKey, viewerTtlSeconds) {
  filename = sanitizeFilename(filename);
  if (!apiKey && !config.QURL_API_KEY) throw new Error('QURL_API_KEY is not configured');
  if (!isAllowedSourceUrl(sourceUrl)) {
    throw new Error('Source URL is not a valid Discord CDN URL');
  }

  const downloadResponse = await cdnFetchFollowSafe(sourceUrl);
  if (!downloadResponse.ok) {
    throw new Error(`Failed to download from Discord CDN: ${downloadResponse.status}`);
  }

  const contentLengthHeader = downloadResponse.headers.get('content-length');
  const contentLength = contentLengthHeader ? parseInt(contentLengthHeader, 10) : null;
  if (contentLength !== null && contentLength > MAX_FILE_SIZE) {
    throw new Error(`File too large: ${Math.round(contentLength / 1024 / 1024)}MB, max 25MB`);
  }
  if (contentLength === null) {
    // Not fatal — readBodyWithCap enforces the real cap by streaming — but
    // flag it so a CDN change that drops Content-Length doesn't silently
    // bypass our pre-check.
    logger.warn('Discord CDN response missing Content-Length, relying on streaming cap', { sourceUrl });
  }

  const fileBuffer = await readBodyWithCap(downloadResponse, MAX_FILE_SIZE);
  const blob = new Blob([fileBuffer], { type: contentType || 'application/octet-stream' });

  const form = new FormData();
  form.append('file', blob, filename);
  appendViewerTtl(form, viewerTtlSeconds);

  const uploadResponse = await fetch(`${config.CONNECTOR_URL}/api/upload`, {
    method: 'POST',
    body: form,
    headers: { ...connectorAuthHeaders(apiKey) },
    signal: AbortSignal.timeout(60000),
  });

  if (!uploadResponse.ok) {
    return throwConnectorError('Connector upload', uploadResponse);
  }

  const result = await uploadResponse.json();
  if (!result.success) {
    throw new Error('Connector upload returned success: false');
  }
  if (!result.resource_id) {
    // Guard against a malformed connector response silently propagating
    // `undefined` as the resource ID into downstream mintLinks/saveSendConfig.
    throw new Error('Connector upload returned no resource_id');
  }

  logger.info('Uploaded to connector', {
    md5_prefix: md5Prefix(result.hash),
    resource_id: result.resource_id,
  });

  return result;
}

/**
 * Re-register an already-downloaded file buffer with the connector.
 * Creates a new qURL resource (with a fresh token pool) without
 * re-downloading from Discord CDN. Used when the per-resource token
 * quota (10) is exhausted and more recipients need links.
 */
async function reUploadBuffer(fileBuffer, filename, contentType, apiKey, viewerTtlSeconds) {
  filename = sanitizeFilename(filename);
  if (!apiKey && !config.QURL_API_KEY) throw new Error('QURL_API_KEY is not configured');

  const blob = new Blob([fileBuffer], { type: contentType || 'application/octet-stream' });
  const form = new FormData();
  form.append('file', blob, filename);
  appendViewerTtl(form, viewerTtlSeconds);

  const uploadResponse = await fetch(`${config.CONNECTOR_URL}/api/upload`, {
    method: 'POST',
    body: form,
    headers: { ...connectorAuthHeaders(apiKey) },
    signal: AbortSignal.timeout(60000),
  });

  if (!uploadResponse.ok) {
    return throwConnectorError('Connector re-upload', uploadResponse);
  }

  const result = await uploadResponse.json();
  if (!result.success) {
    throw new Error('Connector re-upload returned success: false');
  }
  if (!result.resource_id) {
    throw new Error('Connector re-upload returned no resource_id');
  }

  logger.info('Re-uploaded to connector (new resource)', {
    md5_prefix: md5Prefix(result.hash),
    resource_id: result.resource_id,
  });

  return result;
}

/**
 * Download a file from Discord CDN and return the buffer + upload result.
 * The buffer is cached so subsequent re-uploads don't re-download.
 */
async function downloadAndUpload(sourceUrl, filename, contentType, apiKey, viewerTtlSeconds) {
  filename = sanitizeFilename(filename);
  if (!isAllowedSourceUrl(sourceUrl)) {
    throw new Error('Source URL is not a valid Discord CDN URL');
  }

  const downloadResponse = await cdnFetchFollowSafe(sourceUrl);
  if (!downloadResponse.ok) {
    throw new Error(`Failed to download from Discord CDN: ${downloadResponse.status}`);
  }

  const contentLengthHeader = downloadResponse.headers.get('content-length');
  const contentLength = contentLengthHeader ? parseInt(contentLengthHeader, 10) : null;
  if (contentLength !== null && contentLength > MAX_FILE_SIZE) {
    throw new Error(`File too large: ${Math.round(contentLength / 1024 / 1024)}MB, max 25MB`);
  }
  if (contentLength === null) {
    // Not fatal — readBodyWithCap enforces the real cap by streaming — but
    // flag it so a CDN change that drops Content-Length doesn't silently
    // bypass our pre-check.
    logger.warn('Discord CDN response missing Content-Length, relying on streaming cap', { sourceUrl });
  }

  const fileBuffer = await readBodyWithCap(downloadResponse, MAX_FILE_SIZE);
  const result = await reUploadBuffer(fileBuffer, filename, contentType, apiKey, viewerTtlSeconds);
  return { ...result, fileBuffer };
}

/**
 * Mint one-time links for an uploaded resource via the connector.
 *
 * `one_time_use: true` is required — upstream default on some key
 * tiers is unlimited, so omitting it produces reusable links. The
 * field applies PER minted link: `n` recipients get `n` independent
 * one-time tokens, so one recipient opening their link doesn't
 * invalidate anyone else's.
 *
 * `selfDestructSeconds` is forwarded as `session_duration` so every
 * minted token's L7 session window matches the fileviewer's client-
 * side self-destruct timer (closes the mint-side gap left by
 * qurl-integrations-infra#540, tracked in qurl-integrations-infra#764).
 * The seconds→duration-string mapping lives in
 * `utils/time.js::formatSessionDurationSeconds` (co-located with
 * `SELF_DESTRUCT_PRESETS`).
 *
 * @param {string} resourceId — connector resource_id (alphanum + `_-`).
 * @param {object} opts — minting options. Bag-shaped for sibling-consistency
 *   with mintLinksInBatches, whose call-through used to position-align the
 *   adjacent `expiresAt` and `apiKey` strings (cycle-1 footgun on PR #483).
 * @param {string} opts.expiresAt — ISO string forwarded as `expires_at`.
 * @param {number} opts.n — integer 1..100, count of links to mint.
 * @param {?string} [opts.apiKey] — caller API key; falls back to `config.QURL_API_KEY`.
 * @param {?number} [opts.selfDestructSeconds] — see formatSessionDurationSeconds for value mapping. Defaults to null.
 * @param {?string} [opts.guildId] — Discord guild snowflake. When provided,
 *   forwarded as `guild_id` so the connector can scope a future
 *   watermark-attribution `/api/detect` lookup to the minting guild (the
 *   bot side of the per-guild deanonymization-isolation contract, #1101).
 *   Optional + back-compat: omitting it leaves the mint body unchanged, so
 *   legacy callers and pre-#1101 send paths keep working untouched.
 * @returns {Promise<Array<{qurl_id: string, qurl_link: string, expires_at: string}>>}
 */
async function mintLinks(resourceId, { expiresAt, n, apiKey, selfDestructSeconds = null, guildId } = {}) {
  if (!apiKey && !config.QURL_API_KEY) throw new Error('QURL_API_KEY is not configured');
  if (!resourceId || !/^[\w-]+$/.test(resourceId)) {
    throw new Error(`Invalid resource ID format: ${resourceId}`);
  }
  // Bound `n` defensively — callers in this codebase already cap at 10
  // (TOKENS_PER_RESOURCE) or 50 (recipient max), but mintLinks is exported
  // so validate at the API boundary. Negative or non-integer values would
  // make the qURL backend behave unpredictably; 100 is a comfortable ceiling.
  if (!Number.isInteger(n) || n < 1 || n > 100) {
    throw new Error(`Invalid link count (n must be integer 1..100): ${n}`);
  }
  const body = { expires_at: expiresAt, n, one_time_use: true };
  const sessionDuration = formatSessionDurationSeconds(selfDestructSeconds);
  if (sessionDuration !== null) {
    body.session_duration = sessionDuration;
  }
  // Only attach guild_id when truthy — an empty/undefined value would put a
  // useless `guild_id: null` on the wire and (worse) could land as an empty
  // attribution scope on the connector side. Truthy-gate keeps the contract
  // optional, mirroring the session_duration handling above.
  if (guildId) {
    body.guild_id = guildId;
  }
  const response = await fetch(`${config.CONNECTOR_URL}/api/mint_link/${resourceId}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...connectorAuthHeaders(apiKey) },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(30000),
  });

  if (!response.ok) {
    return throwConnectorError('Connector mint_link', response);
  }

  const result = await response.json();
  if (!result.success) {
    throw new Error('Connector mint_link returned success: false');
  }
  if (!result.links || !Array.isArray(result.links)) {
    throw new Error('Connector mint_link returned no links array');
  }

  logger.info('Minted links', { resource_id: resourceId, count: result.links.length });
  return result.links;
}

const DETECT_TARGET_PATH = '/api/detect';
const DETECT_LINK_EXPIRES_IN = '5m';
const DETECT_RESOURCE_LIST_LIMIT = 100;
const DETECT_RESOURCE_FAILURE_BACKOFF_MS = 30 * 1000;
// TODO(upstream-contract): keep these suffixes in lockstep with qurl-service /
// qURL tunnel infra hostnames for production, sandbox, and staging.
const DETECT_TUNNEL_PROD_HOST_SUFFIX = '.qurl.site';
const DETECT_TUNNEL_NON_PROD_HOST_SUFFIXES = [
  '.qurl.site.layerv.xyz',
  '.qurl.site.layerv.ai',
];
const DETECT_TUNNEL_NON_PROD_QURL_ENDPOINT_HOSTS = new Set([
  'localhost',
  '127.0.0.1',
  '[::1]',
  'api.test.local',
  'api.staging.layerv.ai',
]);

function detectTunnelHostSuffixesForEndpoint(endpoint) {
  let host = '';
  try {
    host = new URL(endpoint).hostname.toLowerCase();
  } catch (_err) {
    // A malformed/missing endpoint will fail elsewhere before detect can mint;
    // keep host-pin fail-closed here rather than granting non-prod tunnel hosts
    // to an unknown endpoint shape.
  }
  if (DETECT_TUNNEL_NON_PROD_QURL_ENDPOINT_HOSTS.has(host)) {
    return [DETECT_TUNNEL_PROD_HOST_SUFFIX, ...DETECT_TUNNEL_NON_PROD_HOST_SUFFIXES];
  }
  return [DETECT_TUNNEL_PROD_HOST_SUFFIX];
}

// Intentional load-time computation: QURL_ENDPOINT is static for a bot process,
// and tests that vary it use jest.resetModules() before requiring connector.js.
const DETECT_TUNNEL_HOST_SUFFIXES = detectTunnelHostSuffixesForEndpoint(config.QURL_ENDPOINT);

// Module-level cache for the detect tunnel's resource_id (resolved from
// DETECT_TUNNEL_SLUG via the SDK's listAllResources auto-paginator). The
// resource_id is a stable, NON-secret identifier, so caching it across calls is
// safe and skips a slug lookup on every detect. CACHE ONLY THIS, NEVER the
// minted access token or qurl_site: each detect mints a FRESH ephemeral qURL (a
// short-lived credential — the mint sets expires_in: '5m') and the resolve()/knock
// grants network access to the caller's CURRENT IP/knock-window. A stale token
// would be a long-lived credential to leak; qurl_site is per-mint and must stay
// paired with the fresh knock.
let _detectResourceId = null;
let _detectResourceRetryAfter = 0;
let _detectResourcePreviousFailure = null;
let _detectResourceConsecutiveFailures = 0;
let _detectResourcePreviousFailureAt = 0;

function clearDetectResourceFailureState() {
  _detectResourceRetryAfter = 0;
  _detectResourcePreviousFailure = null;
  _detectResourceConsecutiveFailures = 0;
  _detectResourcePreviousFailureAt = 0;
}

function rememberDetectResourceFailure(error, { immediateBackoff = false, clearResourceCache = true } = {}) {
  // Deliberately shared across cache-clearing failure kinds. For the single
  // dark-launch slug, two consecutive mint/shape/pin/mismatch failures are a
  // tunnel-contract signal, even if the second is a different shape, so fail
  // closed with a short process-wide backoff instead of granting one retry per
  // failure mode. Key this by slug/resource/kind if detect becomes multi-slug
  // or high-volume.
  if (clearResourceCache) _detectResourceId = null;
  const now = Date.now();
  if (_detectResourcePreviousFailureAt && now - _detectResourcePreviousFailureAt > DETECT_RESOURCE_FAILURE_BACKOFF_MS) {
    _detectResourceConsecutiveFailures = 0;
  }
  _detectResourceConsecutiveFailures += 1;
  _detectResourcePreviousFailure = redactAccessToken(error?.message || error);
  _detectResourcePreviousFailureAt = now;
  if (immediateBackoff || _detectResourceConsecutiveFailures >= 2) {
    _detectResourceRetryAfter = now + DETECT_RESOURCE_FAILURE_BACKOFF_MS;
  }
}

function assertDetectResourceFailureBackoffAllowed() {
  if (!_detectResourceRetryAfter) return;
  const retryAfterMs = _detectResourceRetryAfter - Date.now();
  if (retryAfterMs <= 0) {
    clearDetectResourceFailureState();
    return;
  }
  logger.warn('Detect tunnel attempt suppressed by failure backoff', {
    retry_after_ms: retryAfterMs,
    previous_error: _detectResourcePreviousFailure,
  });
  const err = new Error('Detect tunnel attempt is backing off after a previous failure');
  err.retryAfterMs = retryAfterMs;
  throw err;
}

// Lazily-constructed, cached qURL SDK client used solely by
// resolveDetectTarget() to self-mint + resolve the ephemeral detect qURL over
// the reverse-tunnel. Constructed on first use (not at module load) so the bot
// boots even when QURL_API_KEY is unset in non-detect deployments, and so tests
// can inject a mocked @layervai/qurl before the first call.
//
// Cache the client, never the minted qurl_site or access token — resolve()
// re-knocks per call (the full no-cache invariant + rationale live on
// _detectResourceId above and in resolveDetectTarget's docstring).
//
// NOTE: createQurlForResource's `target_path` option needs @layervai/qurl
// >= 0.3.0 (it landed in qurl-typescript#145); package.json pins ~0.3.0.
//
// Bearer note: the SDK's `apiKey` is the qURL API Bearer for the
// listAllResources (read) / createQurlForResource (mint/write) / resolve calls,
// so QURL_API_KEY MUST carry all three scopes — `qurl:read` + `qurl:write` +
// `qurl:resolve`. A key with only `qurl:resolve` passes the mocked tests but
// 403s at runtime on the first slug lookup/mint. This is enforced server-side
// by qURL as a key-provisioning step; the bot already holds read+write for
// /qurl send, so resolve is the scope detect adds. TODO(upstream-contract):
// confirm the scope→endpoint mapping in the soak. This is intentionally the
// global config.QURL_API_KEY, decoupled from the per-call `apiKey` that
// detectWatermark threads only into the detect POST's Bearer via
// connectorAuthHeaders.
let _qurlClient = null;
function getQurlClient() {
  if (!_qurlClient) {
    // baseUrl is the bare qURL API base (no `/v1`) — the SDK prepends the
    // versioned path itself.
    //
    // timeout / maxRetries match the SDK's current defaults but are pinned
    // explicitly so the detect legs' resilience stays stable against
    // SDK-default drift. resolve() is a fast knock+lookup, so 30s sits well
    // under the detect POST's 60s and the retry worst case stays inside
    // Discord's 15-min deferred-interaction window.
    _qurlClient = new QURLClient({
      apiKey: config.QURL_API_KEY,
      baseUrl: config.QURL_ENDPOINT,
      timeout: 30000,
      maxRetries: 3,
    });
  }
  return _qurlClient;
}

function extractAccessToken(qurlLink) {
  let parsed;
  try {
    parsed = new URL(qurlLink);
  } catch {
    throw new Error('detect mint returned an invalid qurl_link');
  }
  const fragment = parsed.hash ? parsed.hash.slice(1) : '';
  if (!fragment) {
    throw new Error('detect mint did not return an access token');
  }
  // TODO(upstream-contract): access tokens are `at_` + base64url-style chars
  // (no `=` padding) and ride as the leading bare qurl_link fragment. Relax
  // this if qurl-service publishes a named token field or changes the alphabet.
  // Do not scan arbitrary later fragment segments: the token is a minted
  // credential and should stay position-pinned unless the service publishes a
  // broader shape.
  const token = fragment.split(/[&?#]/)[0];
  if (!/^at_[A-Za-z0-9_-]+$/.test(token)) {
    throw new Error('detect mint did not return an access token');
  }
  return token;
}

function allowedDetectTunnelHost(hostname, expectedResourceId) {
  const host = hostname.toLowerCase();
  const expectedLabel = String(expectedResourceId || '').toLowerCase();
  return DETECT_TUNNEL_HOST_SUFFIXES.some((suffix) => {
    if (!host.endsWith(suffix)) return false;
    const tunnelLabel = host.slice(0, -suffix.length);
    // TODO(upstream-contract): keep the host label in lockstep with
    // qurl-service's resourceIDPattern (`^r_[a-z0-9_-]{11}$`).
    // The suffix list only scopes known qURL tunnel domains; the exact
    // resource-id equality is the Bearer-carrying POST guard.
    return /^r_[a-z0-9_-]{11}$/.test(tunnelLabel) && tunnelLabel === expectedLabel;
  });
}

class DetectQurlSiteError extends Error {}

// SSRF guard for the qurl_site-derived tunnel target. Must be a PUBLIC
// `https:` URL; reject any non-https scheme, embedded userinfo (the
// `https://good@127.0.0.1/` hostname-confusion bypass), any
// private/loopback/link-local host (reusing qurl.js's syntactic isPrivateHost),
// and any host NOT under an expected qURL reverse-tunnel domain.
// Deliberately NOT port-locked (the tunnel target may sit on a non-standard
// port) and NOT DNS-resolved — a syntactic check ONLY, unlike the link-minting
// path's assertNotPrivateAfterResolve in qurl.js, which adds a DNS-level
// anti-rebinding guard. The asymmetry is intentional: qurl_site here comes
// from a TRUSTED authenticated mint (not user input) and resolve-per-call keeps
// the knock window tight, so a DNS round-trip per detect isn't warranted. A
// future reader should NOT assume this carries the link guard's DNS guarantee.
function assertPublicHttpsTarget(targetUrl, expectedResourceId) {
  let parsed;
  try {
    parsed = new URL(targetUrl);
  } catch {
    // buildDetectTargetUrl passes a serialized URL today; keep this as
    // defense-in-depth if a future caller validates a raw target directly.
    throw new Error('Detect tunnel qurl_site target is unparseable');
  }
  if (parsed.protocol !== 'https:') {
    throw new Error('Detect tunnel qurl_site target must be an https: URL');
  }
  if (parsed.username || parsed.password) {
    throw new Error('Detect tunnel qurl_site target must not contain userinfo');
  }
  if (isPrivateHost(parsed.hostname)) {
    throw new Error('Detect tunnel qurl_site target points to a private/internal address');
  }
  // Host-pin (defense-in-depth on this Bearer-carrying oracle leg): the POST
  // target MUST be an `r_...` qURL reverse-tunnel resource-id host. Production
  // currently uses `*.qurl.site`; sandbox has been observed as
  // `*.qurl.site.layerv.xyz`; staging may use `*.qurl.site.layerv.ai`.
  // Non-prod suffixes are accepted only for an explicit non-prod QURL_ENDPOINT
  // host; unknown endpoint shapes fail closed to prod-only tunnel hosts.
  // A malformed mint returning a public NON-qURL host then can't receive the
  // image bytes + our API-key Bearer. (NOT qurl.link — that's the short-link /
  // ALB domain, not the tunnel.)
  if (!allowedDetectTunnelHost(parsed.hostname, expectedResourceId)) {
    throw new Error('Detect tunnel qurl_site host is not under an expected qURL tunnel domain');
  }
}

function buildDetectTargetUrl(qurlSite, expectedResourceId) {
  let parsed;
  try {
    parsed = new URL(qurlSite);
  } catch {
    throw new DetectQurlSiteError('detect mint returned an unparseable qurl_site');
  }
  // Contract is intentionally host-only. If qurl-service ever starts returning
  // a path-based qurl_site, fail loudly during soak instead of silently discarding
  // path/query/hash state with `new URL('/api/detect', qurl_site)`.
  if (parsed.pathname !== '/' || parsed.search || parsed.hash) {
    throw new DetectQurlSiteError('detect mint qurl_site must be host-only');
  }
  assertPublicHttpsTarget(parsed.toString(), expectedResourceId);
  return new URL(DETECT_TARGET_PATH, parsed.origin).toString();
}

// Scrub any `at_…` access token from a free-text error message before logging.
// The detect access token originates in the mint RESPONSE (qurl_link fragment)
// and is echoed back in the resolve REQUEST, so a future @layervai/qurl that
// surfaced either in a QURLError message would otherwise leak it. As of 0.3.0,
// errors are built from the RFC-7807 response envelope (errors.js), not bodies —
// so this is defense-in-depth that keeps the never-log-the-token invariant
// self-enforced across SDK versions. Applied uniformly to all three breadcrumbs;
// it's a no-op on the token-free slug-lookup leg but keeps that log line null-safe
// + consistent (the `String(... ?? '')` guard).
function redactAccessToken(message) {
  return String(message ?? '').replace(/at_[A-Za-z0-9_-]+/g, 'at_[REDACTED]');
}

/**
 * Resolve the qURL reverse-tunnel target for the watermark-detect endpoint.
 *
 * Self-mints an EPHEMERAL qURL to the detect tunnel resource per call (no
 * pre-seeded token), using the bot's own `QURL_API_KEY` via the @layervai/qurl
 * SDK (getQurlClient):
 *   1. resolve the tunnel resource_id from DETECT_TUNNEL_SLUG
 *      (`listAllResources({ slug, limit: 100 })`, then client-side
 *      `status === 'active'` filtering because the live API rejects
 *      status+slug; the SDK walks all pages so accumulated revoked rows can't
 *      hide the one active detect resource) — CACHED in `_detectResourceId`
 *      (it's stable + non-secret). A short process-global failure backoff
 *      suppresses repeated full slug-history scans after persistent hard
 *      failures. This is keyed process-wide because the dark launch has a
 *      single DETECT_TUNNEL_SLUG; if detect becomes per-guild/per-resource,
 *      key the backoff by slug/resource instead.
 *   2. mint a fresh short-lived qURL on that resource
 *      (`createQurlForResource(resource_id, { target_path: '/api/detect' })`)
 *      whose `qurl_link` fragment carries the `at_…` access token and whose
 *      host-only `qurl_site` is the tunnel POST host;
 *   3. `resolve({ access_token })` — this is the NHP knock: it grants network
 *      access for the CALLER'S CURRENT IP. The live tunnel API returns
 *      `target_url: ""`; do not use it as the POST target.
 * The caller MUST POST to qurl_site within the knock window from the same IP —
 * hence this is invoked immediately before each detect POST
 * (mint-and-resolve-per-call), and NEITHER the minted token NOR qurl_site is
 * cached. A fresh 5m-expiry qURL per detect (the mint passes expires_in: '5m')
 * means there's no long-lived credential to leak. Detect is low-frequency so
 * the extra mint+resolve calls are negligible.
 *
 * SECURITY: never log the minted access token, qurl_link, or raw qurl_site. The
 * mint + resolve breadcrumbs run `err.message` through `redactAccessToken` (the
 * token lives in the mint response / resolve request), and the SSRF assert
 * messages are static/URL-free.
 *
 * @returns {Promise<string>} the SSRF-validated qurl_site + /api/detect target.
 * @throws if DETECT_TUNNEL_SLUG is unset, the tunnel resource can't be resolved,
 *   the mint doesn't return an access token, or the minted qurl_site fails the
 *   public-https SSRF guard.
 */
async function resolveDetectTarget() {
  if (!config.DETECT_TUNNEL_SLUG) {
    throw new Error('DETECT_TUNNEL_SLUG is not configured (required to reach the detect tunnel)');
  }
  assertDetectResourceFailureBackoffAllowed();

  // Resolve the tunnel resource_id from the slug, cached across calls — it's a
  // stable, non-secret identifier. Assign the cache ONLY after a successful
  // extract so a failed lookup doesn't poison it. The SDK owns pagination and
  // response shaping: listAllResources yields resources from every page, and
  // each resource carries `resource_id` (not `id`). There is intentionally no
  // in-flight dedup for concurrent cold-cache lookups: detect is dark-launched
  // and low frequency, while the failure backoff bounds repeated hard failures.
  let resourceId = _detectResourceId;
  if (!resourceId) {
    // Breadcrumb a slug-lookup transport failure (message only — no token, no
    // URL), matching the mint/resolve legs, so a cold-boot activation failure
    // on the FIRST network call is diagnosable rather than an undistinguished
    // throw at the handler.
    const active = [];
    try {
      for await (const resource of getQurlClient().listAllResources({
        slug: config.DETECT_TUNNEL_SLUG,
        limit: DETECT_RESOURCE_LIST_LIMIT,
      })) {
        if (resource?.status === 'active') active.push(resource);
      }
    } catch (err) {
      // Transport blips on the cold slug lookup get one immediate retry, like
      // mint failures. Deterministic slug contract failures below still arm an
      // immediate backoff because no retry can make missing/multiple active
      // resources safe.
      rememberDetectResourceFailure(err, { clearResourceCache: false });
      logger.warn('Detect tunnel slug lookup failed', { error: redactAccessToken(err.message) });
      throw err;
    }
    if (active.length > 1) {
      const err = new Error('Detect tunnel resource slug resolved to multiple active resources');
      rememberDetectResourceFailure(err, { immediateBackoff: true });
      logger.warn('Detect tunnel slug resolved to multiple active resources', {
        slug: config.DETECT_TUNNEL_SLUG,
        count: active.length,
      });
      throw err;
    }
    resourceId = active[0]?.resource_id ? String(active[0].resource_id) : null;
    if (!resourceId) {
      const err = new Error('Detect tunnel resource not found for slug');
      rememberDetectResourceFailure(err, { immediateBackoff: true });
      throw err;
    }
    _detectResourceId = resourceId;
  }

  // Mint a fresh ephemeral qURL on the resource (per call). `expires_in: '5m'`
  // bounds the credential lifetime AND caps accumulation of unused mints — the
  // bot never deletes them, it relies on expiry. Detect uses the token within
  // seconds (mint → resolve), so 5m is generous margin, not a usage window. The
  // 201 carries the `at_…` access token in the `qurl_link` fragment. Breadcrumb a
  // mint failure (message only — no token, no URL) then rethrow so an
  // activation-time failure is diagnosable at the handler.
  // TODO(upstream-contract): confirm qurl-service honors `expires_in` on a
  // resource mint during the sandbox soak (CI mocks the SDK, so this isn't
  // exercised against the live API here).
  let accessToken;
  let targetUrl;
  let minted;
  try {
    minted = await getQurlClient().createQurlForResource(resourceId, {
      target_path: DETECT_TARGET_PATH,
      expires_in: DETECT_LINK_EXPIRES_IN,
    });
  } catch (err) {
    // Self-heal a stale resource_id: if the tunnel resource was deleted/
    // recreated, the cached id would 404 every mint until process restart.
    // Drop the cache so a later detect re-resolves the slug. The first
    // mint transport/API failure gets one immediate self-heal retry; repeated
    // failures arm the short backoff so a broken tunnel does not re-walk slug
    // history on every request.
    rememberDetectResourceFailure(err);
    logger.warn('Detect tunnel mint failed', { error: redactAccessToken(err.message) });
    throw err;
  }
  try {
    accessToken = extractAccessToken(minted?.qurl_link);
  } catch (err) {
    // A malformed qurl_link is a mint response-shape issue, not evidence that
    // the cached resource_id is stale. Keep the resource cache and retry only
    // the mint after the short failure window.
    rememberDetectResourceFailure(err, { clearResourceCache: false });
    logger.warn('Detect tunnel mint failed', { error: redactAccessToken(err.message) });
    throw err;
  }
  try {
    targetUrl = buildDetectTargetUrl(minted?.qurl_site, resourceId);
  } catch (err) {
    // qurl_site/resource-id host-pin failures happen after a successful slug
    // lookup and mint, so keep the cached resource id and retry the mint after
    // the short failure window instead of re-walking slug history. The mint
    // created an unredeemed 5m qURL, but failing before resolve() is the safe
    // trade: no NHP knock and no image POST are issued to an untrusted host.
    rememberDetectResourceFailure(err, { clearResourceCache: false });
    const label = err instanceof DetectQurlSiteError
      ? 'Detect tunnel mint returned an invalid qurl_site'
      : 'Detect tunnel target rejected by SSRF guard';
    logger.warn(label, { error: redactAccessToken(err.message) });
    throw err;
  }

  // Resolve (per call — the NHP knock). Breadcrumb the failure so an
  // activation-time failure is diagnosable rather than an undistinguished throw
  // at the handler. Log ONLY the redacted error message: never the access_token.
  let resolved;
  try {
    resolved = await getQurlClient().resolve({ access_token: accessToken });
  } catch (err) {
    // The lookup and mint succeeded, so do not clear the cached resource id or
    // arm the detect failure backoff for knock/transport failures. Persistent
    // knock failures intentionally retry with a fresh 5m mint per detect (not
    // a slug rewalk); unused qURLs expire quickly, so accumulation is bounded
    // without coupling the NHP leg to the slug/mint/host-pin backoff.
    logger.warn('Detect tunnel resolve failed (knock/transport)', { error: redactAccessToken(err.message) });
    throw err;
  }
  // The live response includes resource_id. Treat it as an integrity check when
  // present, but do not make the tunnel POST depend on optional resolve metadata
  // from older/variant API shapes; qurl_site came from the authenticated mint.
  const resolvedResourceId = resolved?.resource_id;
  if (resolvedResourceId && String(resolvedResourceId).toLowerCase() !== resourceId.toLowerCase()) {
    const err = new Error('Detect tunnel resolve returned a mismatched resource_id');
    // The knock already happened, but the Bearer-carrying image POST must not.
    // The minted qURL is unused and expires quickly; clear the cache so the
    // next attempt rewalks the slug in case the resource was recreated.
    rememberDetectResourceFailure(err);
    logger.warn('Detect tunnel resolve returned mismatched resource_id', {
      expected_resource_id: resourceId,
      actual_resource_id: resolvedResourceId,
    });
    throw err;
  }
  clearDetectResourceFailureState();
  return targetUrl;
}

/**
 * Watermark-attribution detect (the bot side of #1101). Self-mints a fresh
 * qURL to the detect tunnel, resolves that access token to issue the NHP knock
 * (resolveDetectTarget — see its rationale), then POSTs the raw image bytes to
 * the minted `qurl_site + /api/detect`. The detect service
 * reads the invisible meta-seal watermark and resolves it to the qurl_id it
 * was minted for, GUILD-SCOPED via the `X-Guild-Id` header so an image
 * watermarked in guild A never attributes in guild B. This is a
 * deanonymization oracle by design — the caller (handleQurlDetect) owns the
 * cooldown + ephemeral-reply abuse guards and the second-layer same-guild
 * filter on the returned rows.
 *
 * REACH MODEL: the public connector `/api/detect` path is gone; detect now
 * lives behind the qURL reverse-tunnel. resolveDetectTarget() self-mints a
 * fresh ephemeral qURL to the detect tunnel resource and resolves it; the
 * resolve grants network access to the bot's current egress IP and the POST
 * goes to qurl_site from that same IP within the knock window — so
 * mint-and-resolve-then-POST happens per call and neither the minted token nor
 * qurl_site is ever reused.
 *
 * Contract:
 *   - Reach: resolveDetectTarget() self-mints + resolves using the bot's own
 *     `config.QURL_API_KEY` (the SDK Bearer; needs `qurl:read` + `qurl:write` +
 *     `qurl:resolve` — list / mint / resolve) against the DETECT_TUNNEL_SLUG
 *     resource. It calls listAllResources({ slug, limit: 100 }), filters
 *     active resources client-side, mints target_path=/api/detect for 5m,
 *     ignores resolve target_url, and POSTs to qurl_site. No pre-seeded access
 *     token.
 *   - POST headers: Authorization: Bearer <apiKey>, X-Guild-Id: <guildId>,
 *     Content-Type: <imageContentType || 'application/octet-stream'>.
 *   - Body: the raw image bytes (Buffer / ArrayBuffer / Uint8Array).
 *   - 200 JSON: { detected: boolean, qurl_id: string|null,
 *     match_pct: number|null, confidence: number }. detected=false ⇒ no
 *     mark OR no same-guild match (qurl_id / match_pct null). detected=true
 *     ⇒ qurl_id + match_pct (0–100) + confidence (0–1).
 *   - 401 bad auth / 400 missing guild|image / 429 rate-limited / 5xx —
 *     surfaced as a thrown Error (with .status) via throwConnectorError so
 *     the handler can ephemeral-error rather than leak the body.
 *
 * @param {Buffer|ArrayBuffer|Uint8Array} imageBytes — raw image bytes.
 * @param {object} opts
 * @param {string} opts.guildId — Discord guild snowflake (X-Guild-Id scope).
 * @param {?string} [opts.contentType] — image MIME; defaults to octet-stream.
 * @param {?string} [opts.apiKey] — caller API key for the detect POST Bearer;
 *   falls back to config.QURL_API_KEY. (The mint+resolve leg always uses the
 *   global config.QURL_API_KEY as the SDK Bearer — see resolveDetectTarget.)
 * @returns {Promise<{detected: boolean, qurl_id: string|null, match_pct: number|null, confidence: number}>}
 */
async function detectWatermark(imageBytes, { guildId, contentType, apiKey } = {}) {
  // The mint+resolve leg always uses the global config.QURL_API_KEY as the
  // SDK Bearer (see resolveDetectTarget), so this leg requires it even
  // when a per-call `apiKey` is set — the apiKey overrides only the POST Bearer.
  if (!config.QURL_API_KEY) throw new Error('QURL_API_KEY is not configured');
  if (!guildId) throw new Error('detectWatermark requires a guildId (attribution is guild-scoped)');

  // Mint-and-resolve-per-call — never cache the minted token or qurl_site
  // (rationale in the REACH MODEL note above and resolveDetectTarget's docstring).
  const targetUrl = await resolveDetectTarget();

  const response = await fetch(targetUrl, {
    method: 'POST',
    headers: {
      'Content-Type': contentType || 'application/octet-stream',
      'X-Guild-Id': guildId,
      ...connectorAuthHeaders(apiKey),
    },
    body: imageBytes,
    // Neural-net inference is the slow leg here; give it the same 60s
    // headroom the upload paths use rather than the 30s mint window.
    signal: AbortSignal.timeout(60000),
  });

  if (!response.ok) {
    return throwConnectorError('Connector detect', response);
  }

  const result = await response.json();
  // Normalize the shape so the caller can destructure without
  // optional-chaining every field. The connector owns the values;
  // we only coerce `detected` to a hard boolean (a missing/garbled
  // field must read as "no attribution", never as a truthy object).
  return {
    detected: result.detected === true,
    qurl_id: typeof result.qurl_id === 'string' ? result.qurl_id : null,
    match_pct: typeof result.match_pct === 'number' ? result.match_pct : null,
    confidence: typeof result.confidence === 'number' ? result.confidence : 0,
  };
}

/**
 * Upload a JSON object to the connector as a file.
 * Used for structured payloads like location data.
 */
async function uploadJsonToConnector(jsonPayload, filename, apiKey, viewerTtlSeconds) {
  filename = sanitizeFilename(filename);
  if (!apiKey && !config.QURL_API_KEY) throw new Error('QURL_API_KEY is not configured');

  const blob = new Blob([JSON.stringify(jsonPayload)], { type: 'application/json' });
  const form = new FormData();
  form.append('file', blob, filename);
  appendViewerTtl(form, viewerTtlSeconds);

  const uploadResponse = await fetch(`${config.CONNECTOR_URL}/api/upload`, {
    method: 'POST',
    body: form,
    headers: { ...connectorAuthHeaders(apiKey) },
    signal: AbortSignal.timeout(60000),
  });

  if (!uploadResponse.ok) {
    return throwConnectorError('Connector JSON upload', uploadResponse);
  }

  const result = await uploadResponse.json();
  if (!result.success) {
    throw new Error('Connector JSON upload returned success: false');
  }
  if (!result.resource_id) {
    throw new Error('Connector JSON upload returned no resource_id');
  }

  logger.info('Uploaded JSON to connector', {
    md5_prefix: md5Prefix(result.hash),
    resource_id: result.resource_id,
  });

  return result;
}

module.exports = { uploadToConnector, downloadAndUpload, reUploadBuffer, mintLinks, detectWatermark, uploadJsonToConnector, isAllowedSourceUrl };
