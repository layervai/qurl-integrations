const { QurlClient } = require('@layervai/qurl');

const config = require('./config');
const logger = require('./logger');

// Reuse the security-critical, syntactic private/loopback/link-local IP guard
// from qurl.js rather than duplicating ~50 lines of IP-literal parsing that
// could drift out of sync. qurl.js has no connector.js dependency, so this
// require introduces no cycle.
const { isPrivateHost } = require('./qurl');

const { sanitizeFilename } = require('./utils/sanitize');
const { formatSessionDurationSeconds, isPositiveFinite } = require('./utils/time');

const { MAX_FILE_SIZE } = require('./constants');
const MAX_CDN_REDIRECTS = 3;
const ALLOWED_DETECT_TARGET_HOST_SUFFIXES = [
  '.qurl.site',
  '.qurl.link',
];
let inFlightDetectTargetResolve = null;

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
// qURL API error codes the connector can pass back via the `error` string.
// Surface them as Error.apiCode so the caller can branch on a typed value
// instead of substring-matching the human-readable message (which the
// connector / upstream API can rephrase without notice).
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
        if (QUOTA_EXCEEDED_PATTERNS.some((rx) => rx.test(errStr))) {
          apiCode = 'quota_exceeded';
          apiDetail = errStr;
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

// Lazily-constructed, cached qURL SDK client used solely by
// resolveDetectTarget() to resolve the detect access token over the
// reverse-tunnel. Constructed on first use (not at module load) so the bot
// boots even when QURL_API_KEY is unset in non-detect deployments, and so
// tests can inject a mocked @layervai/qurl before the first call.
//
// CACHE THE CLIENT, NEVER THE RESOLVED target_url: resolve() issues a fresh
// NHP knock per call (see resolveDetectTarget) — a stale target_url's network
// access was granted to a previous IP/knock-window and must not be reused.
//
// Bearer note: the SDK's `apiKey` is the qURL API Bearer for the /v1/resolve
// call and MUST carry the `qurl:resolve` scope (enforced server-side by qURL,
// NOT by this code — it's a key-provisioning step for the bot's QURL_API_KEY).
// This is intentionally the global config.QURL_API_KEY, decoupled from the
// per-call `apiKey` that detectWatermark threads only into the detect POST's
// Bearer via connectorAuthHeaders.
let _qurlClient = null;
function getQurlClient() {
  if (!_qurlClient) {
    // baseUrl is the bare qURL API base (no `/v1`) — the SDK prepends
    // `/v1/resolve` itself. Same base qurl.js uses for qurlFetch
    // (`${config.QURL_ENDPOINT}/v1${path}`).
    //
    // timeout / maxRetries: explicitly bound and harden the resolve()
    // control-plane call. The SDK already defaults to timeout 30s/attempt and
    // maxRetries 3 (on 429/5xx + transport errors), but pinning both keeps the
    // resolve leg's resilience visible and stable against SDK-default drift:
    //   - timeout bounds a stalled qURL endpoint so it degrades like the detect
    //     POST's AbortSignal.timeout instead of hanging with no upper bound.
    //   - maxRetries gives resolve the same transient-failure resilience that
    //     qurlFetch's 3-attempt backoff gives the other qURL calls, so a single
    //     blip doesn't fail the whole detect interaction.
    // resolve() is a fast knock+lookup, so 30s sits well under the POST's 60s;
    // the retry worst case is timeout*(maxRetries+1)+backoff, still inside
    // Discord's 15-min deferred-interaction window.
    _qurlClient = new QurlClient({
      apiKey: config.QURL_API_KEY?.trim(),
      baseUrl: String(config.QURL_ENDPOINT || '').trim(),
      timeout: 30000,
      maxRetries: 3,
    });
  }
  return _qurlClient;
}

// SSRF guard for the resolve()-returned tunnel target. Must be a qURL-hosted
// public `https:` /api/detect URL; reject non-https schemes, embedded userinfo
// (the `https://good@127.0.0.1/` hostname-confusion bypass), private/loopback/
// link-local hosts (reusing qurl.js's syntactic isPrivateHost), non-default
// ports, and path/query/fragment drift. This is still syntactic only, unlike
// the link-minting path's assertNotPrivateAfterResolve in qurl.js, which adds a
// DNS-level anti-rebinding guard. The asymmetry is intentional: target_url here
// comes from a trusted resolve() response and resolve-per-call keeps the knock
// window tight, so a DNS round-trip per detect isn't warranted.
function assertPublicHttpsTarget(targetUrl) {
  let parsed;
  try {
    parsed = new URL(targetUrl);
  } catch {
    throw new Error('Detect tunnel resolved an unparseable target URL');
  }
  if (parsed.protocol !== 'https:') {
    throw new Error('Detect tunnel target must be an https: URL');
  }
  if (parsed.username || parsed.password) {
    throw new Error('Detect tunnel target must not contain userinfo');
  }
  if (isPrivateHost(parsed.hostname)) {
    throw new Error('Detect tunnel target points to a private/internal address');
  }
  const hostname = parsed.hostname.replace(/\.$/, '').toLowerCase();
  if (!ALLOWED_DETECT_TARGET_HOST_SUFFIXES.some(suffix => hostname.endsWith(suffix))) {
    throw new Error('Detect tunnel target host is not an allowed qURL tunnel host');
  }
  if (
    (parsed.port !== '' && parsed.port !== '443')
    || parsed.pathname !== '/api/detect'
    || parsed.search
    || parsed.hash
  ) {
    throw new Error('Detect tunnel target must be a qURL https /api/detect URL');
  }
  return targetUrl;
}

/**
 * Resolve the qURL reverse-tunnel target for the watermark-detect endpoint.
 *
 * Calls `QurlClient.resolve({ access_token: config.DETECT_ACCESS_TOKEN })`,
 * which (per the SDK) triggers an NHP knock granting network access for the
 * CALLER'S CURRENT IP, then returns the `target_url` to POST the image to.
 * The caller MUST POST within the knock window from the same IP. The settled
 * target_url is never cached, but simultaneous detect calls share one in-flight
 * resolve so a short burst does not double-fire the same knock.
 *
 * @returns {Promise<string>} the SSRF-validated public https target_url.
 * @throws if DETECT_ACCESS_TOKEN is unset, or the resolved target fails the
 *   public-https SSRF guard.
 */
async function resolveDetectTarget() {
  const token = config.DETECT_ACCESS_TOKEN?.trim();
  if (!token) {
    throw new Error('DETECT_ACCESS_TOKEN is not configured (required to resolve the detect tunnel target)');
  }
  if (!config.QURL_API_KEY?.trim()) {
    throw new Error('QURL_API_KEY is not configured (required to resolve the detect tunnel target)');
  }
  if (inFlightDetectTargetResolve) return inFlightDetectTargetResolve;

  // Breadcrumb the two distinct failure modes of this oracle path so an
  // activation-time failure is diagnosable — a failed knock/transport vs. a
  // rejected target — rather than an undistinguished throw at the handler. Log
  // ONLY the error message: never the access_token, and never the raw
  // target_url (assertPublicHttpsTarget's messages are static and URL-free, and
  // a malformed target could carry userinfo).
  inFlightDetectTargetResolve = (async () => {
    let targetUrl;
    try {
      ({ target_url: targetUrl } = await getQurlClient().resolve({
        access_token: token,
      }));
    } catch (err) {
      logger.warn('Detect tunnel resolve failed (knock/transport)', { error: err.message });
      throw err;
    }
    try {
      return assertPublicHttpsTarget(targetUrl);
    } catch (err) {
      logger.warn('Detect tunnel target rejected by SSRF guard', { error: err.message });
      throw err;
    }
  })();

  try {
    return await inFlightDetectTargetResolve;
  } finally {
    inFlightDetectTargetResolve = null;
  }
}

/**
 * Watermark-attribution detect (the bot side of #1101). Resolves the qURL
 * reverse-tunnel target (resolveDetectTarget — see its NHP-knock rationale),
 * then POSTs the raw image bytes to that `target_url`. The detect service
 * reads the invisible meta-seal watermark and resolves it to the qurl_id it
 * was minted for, GUILD-SCOPED via the `X-Guild-Id` header so an image
 * watermarked in guild A never attributes in guild B. This is a
 * deanonymization oracle by design — the caller (handleQurlDetect) owns the
 * cooldown + ephemeral-reply abuse guards and the second-layer same-guild
 * filter on the returned rows.
 *
 * REACH MODEL: the public connector `/api/detect` path is gone; detect now
 * lives behind the qURL reverse-tunnel. resolve() grants network access to the
 * bot's current egress IP and the POST goes out from that same IP within the
 * knock window — so resolve-then-POST happens for each settled call and a stale
 * target_url is never reused (a dynamic bot IP would otherwise be locked out).
 *
 * Contract:
 *   - Resolve: client `apiKey` (config.QURL_API_KEY, needs `qurl:resolve`
 *     scope) is the Bearer; `access_token` is config.DETECT_ACCESS_TOKEN.
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
 *   falls back to config.QURL_API_KEY. (The resolve() Bearer is always the
 *   global config.QURL_API_KEY — see getQurlClient.)
 * @returns {Promise<{detected: boolean, qurl_id: string|null, match_pct: number|null, confidence: number}>}
 */
async function detectWatermark(imageBytes, { guildId, contentType, apiKey } = {}) {
  if (!guildId) throw new Error('detectWatermark requires a guildId (attribution is guild-scoped)');

  // Resolve-per-call — never cache the target_url (rationale in the REACH MODEL
  // note above and resolveDetectTarget's docstring).
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
