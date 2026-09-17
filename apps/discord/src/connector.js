const { QURLClient } = require('@layervai/qurl');

const config = require('./config');
const logger = require('./logger');
const { validateResourceId, resourceIdLogRef } = require('./utils/resource-id');
const { qurlIdForCleanup } = require('./utils/qurl-id');

// Reuse the security-critical, syntactic private/loopback/link-local IP guard
// from qurl.js rather than duplicating ~50 lines of IP-literal parsing that
// could drift out of sync. resolveDetectTarget() self-mints the ephemeral
// detect qURL via the @layervai/qurl SDK (the standardized client), not qurl.js.
// qurl.js has no connector.js dependency, so this require introduces no cycle.
const { isPrivateHost, revokeOrdinaryLinks, REVOKE_BATCH_MAX_IDS } = require('./qurl');

const { sanitizeFilename } = require('./utils/sanitize');
const { formatSessionDurationSeconds, isPositiveFinite, settlesWithin } = require('./utils/time');

const { MAX_FILE_SIZE, MAX_OVERFLOW_REVOKE_IDS } = require('./constants');
const MAX_CDN_REDIRECTS = 3;
// TODO(upstream-contract): qurl-integrations-infra#1551's POST /api/revoke_links
// processes at most 10 unique ids under one 55s handler deadline. Leave 10s for
// response transport so the caller, not an accidental race, owns the bound.
// The endpoint rejects larger requests atomically, so chunk rather than couple
// to commands.js's independently tunable TOKENS_PER_RESOURCE. The same chunk
// feeds the SDK fallback, so its size is CONNECTOR_REVOKE_MAX_IDS below. A 404
// is remembered only within one call, so each resource re-probes the route on
// purpose: a process-wide negative cache would hide the route once enabled.
const REVOKE_LINKS_TIMEOUT_MS = 65_000;
// Chunk size: #1551's 10-id request cap, bounded by construction by the SDK
// fallback's per-call cap because every chunk may be handed to it whole. (A
// missing import yields NaN, which the coverage check in revokeMintedLinks
// turns into a throw, never a false success.)
const CONNECTOR_REVOKE_MAX_IDS = Math.min(10, REVOKE_BATCH_MAX_IDS);
const REVOKE_RETRY_AFTER_MAX_SECONDS = 2;
// Waiting budget for inline partial-mint cleanup before the mint error is
// rethrown: one connector revoke chunk plus slack. Larger partial sets (up to
// n + MAX_OVERFLOW_REVOKE_IDS ids, several chunks) routinely outlast it. The revoke keeps running
// and a timeout (expected while the SDK fallback is slow) is logged as a warning
// with the ids for reconciliation.
const PARTIAL_MINT_CLEANUP_WAIT_MS = 70_000;
// Terminal per-id outcomes. not_connector_managed is not itself a revoke: it
// hands an ordinary child back to the SDK below.
const REVOKE_TERMINAL_STATUSES = new Set(['revoked', 'already_gone', 'not_connector_managed']);

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

function parseConnectorBody(bodyText) {
  let parsed = null;
  let apiCode = null;
  let apiDetail = null;
  if (!bodyText) return { parsed, apiCode, apiDetail };

  try {
    parsed = JSON.parse(bodyText);
    // Connector wraps upstream API errors as `{success:false, error:"..."}`.
    // The wrapped string is what we pattern-match for known codes.
    const errStr = typeof parsed.error === 'string' ? parsed.error : '';
    if (QUOTA_EXCEEDED_PATTERNS.some((rx) => rx.test(errStr))) {
      apiCode = 'quota_exceeded';
      apiDetail = errStr;
    }
  } catch { /* not JSON, ignore */ }

  return { parsed, apiCode, apiDetail };
}

// One identity rule for the thrown error and cleanup: only bounded ids that
// the revoke endpoint accepts. Unidentified entries (an upstream id-shape
// change) and ids beyond the `n` requested (the connector broke the mint
// contract, so live children may be left) are counted separately; the cap
// bounds compensation work driven by an untrusted body.
function partialQurlIdsFromLinks(links, n) {
  if (!Array.isArray(links)) {
    return { partialQurlIds: [], unrevokedQurlIds: [], unidentifiedCount: 0, overMintedCount: 0, cappedCount: 0 };
  }
  // This runs while reporting a failed mint: never let a bad `n` replace that
  // error. Skip cleanup instead and let the capped count below surface it.
  const cap = Number.isInteger(n) && n > 0 ? n + MAX_OVERFLOW_REVOKE_IDS : 0;
  const normalized = links.map(link => qurlIdForCleanup(link?.qurl_id));
  const identified = [...new Set(normalized.filter(id => id !== null))];
  return {
    partialQurlIds: identified.slice(0, cap),
    // Bounded, non-secret slice of ids left for hand reconciliation.
    unrevokedQurlIds: identified.slice(cap, cap + MAX_OVERFLOW_REVOKE_IDS),
    unidentifiedCount: normalized.filter(id => id === null).length,
    overMintedCount: cap ? Math.max(0, identified.length - n) : 0,
    cappedCount: Math.max(0, identified.length - cap),
  };
}

function throwConnectorErrorFromBody(label, response, {
  bodyText = '',
  apiCode = null,
  apiDetail = null,
  partialQurlIds = [],
} = {}) {
  if (partialQurlIds.length === 0) {
    logger.debug(`${label} error`, {
      status: response.status,
      apiCode,
      bodyLen: bodyText.length,
    });
  }
  const err = new Error(`${label} failed (${response.status})`);
  err.status = response.status;
  err.apiCode = apiCode;
  err.apiDetail = apiDetail;
  if (partialQurlIds.length > 0) {
    err.partialLinkCount = partialQurlIds.length;
    err.partialQurlIds = partialQurlIds;
  }
  throw err;
}

async function throwConnectorError(label, response) {
  let bodyText = '';
  try {
    bodyText = await response.text();
  } catch { /* network read failed, fall through with empty body */ }
  const { apiCode, apiDetail } = parseConnectorBody(bodyText);
  throwConnectorErrorFromBody(label, response, { bodyText, apiCode, apiDetail });
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
  // Same public-resource boundary as status/revoke: mintLinks receives a
  // connector-returned public ID, never a qURL bearer token. Reuse the shared
  // generic-error guard so the duplicate validation cannot drift or echo a
  // cross-wired token into a caller's logs.
  validateResourceId(resourceId);
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
    let bodyText = '';
    try {
      bodyText = await response.text();
    } catch { /* network read failed, fall through with empty body */ }
    const { parsed, apiCode, apiDetail } = parseConnectorBody(bodyText);
    const {
      partialQurlIds, unrevokedQurlIds, unidentifiedCount, overMintedCount, cappedCount,
    } = partialQurlIdsFromLinks(parsed?.links, n);
    if (overMintedCount > 0 || cappedCount > 0) {
      // Over-minted children up to MAX_OVERFLOW_REVOKE_IDS are revoked below;
      // capped_qurl_count is what is left for hand reconciliation.
      logger.error('Connector mint_link returned more partial links than requested', {
        resource_ref: resourceIdLogRef(resourceId),
        requested: n,
        over_minted_count: overMintedCount,
        capped_qurl_count: cappedCount,
        unrevoked_overflow_qurl_ids: unrevokedQurlIds,
      });
    }
    if (partialQurlIds.length > 0 || unidentifiedCount > 0) {
      // TODO(upstream-contract): Best-effort reconciliation signal; connector
      // error bodies must only include qurl_ids for links that were actually minted.
      logger.warn('Connector mint_link returned partial links on non-2xx', {
        resource_ref: resourceIdLogRef(resourceId),
        status: response.status,
        apiCode,
        bodyLen: bodyText.length,
        partial_link_count: partialQurlIds.length,
        // qurl_ids are non-secret revoke handles (at_ tokens are rejected);
        // log-redaction consistency is tracked in #1479.
        partial_qurl_ids: partialQurlIds,
        unidentified_qurl_count: unidentifiedCount,
      });
    }
    // TODO(upstream-contract): qurl-integrations-infra#1551 returns id-only
    // compensation entries for children whose view write failed, and callers
    // must revoke them. Revoke every returned child before rethrowing so a
    // failed mint never strands a live shared-tunnel token; a cleanup failure
    // is logged and never masks the mint error.
    if (partialQurlIds.length > 0) {
      const cleanup = revokeMintedLinks(resourceId, partialQurlIds, apiKey).catch(cleanupError => {
        logger.error('Connector partial mint cleanup failed', {
          resource_ref: resourceIdLogRef(resourceId),
          partial_link_count: partialQurlIds.length,
          cleanup_status: cleanupError?.status,
          error: cleanupError?.message,
        });
      });
      if (!await settlesWithin(cleanup, PARTIAL_MINT_CLEANUP_WAIT_MS)) {
        logger.warn('Connector partial mint cleanup still running at its wait budget', {
          resource_ref: resourceIdLogRef(resourceId),
          partial_qurl_ids: partialQurlIds,
        });
      }
    }
    return throwConnectorErrorFromBody('Connector mint_link', response, {
      bodyText,
      apiCode,
      apiDetail,
      partialQurlIds,
    });
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

async function discardBody(response) {
  try {
    await response.body?.cancel();
  } catch { /* discarding the body is best-effort */ }
}

// POST one revoke chunk. A 429 from the connector's local admission gate
// (Retry-After: 1) is retried once, so /qurl revoke's own 5-resource fan-out
// does not fail itself; a second 429 is returned for the caller to fail closed.
// Both attempts share one REVOKE_LINKS_TIMEOUT_MS budget.
async function postRevokeLinks(resourceId, batchIds, apiKey) {
  const signal = AbortSignal.timeout(REVOKE_LINKS_TIMEOUT_MS);
  const post = () => fetch(`${config.CONNECTOR_URL}/api/revoke_links`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...connectorAuthHeaders(apiKey) },
    body: JSON.stringify({ resource_id: resourceId, qurl_ids: batchIds }),
    redirect: 'error',
    signal,
  });
  const response = await post();
  if (response.status !== 429) return response;
  const retryAfter = response.headers?.get?.('retry-after');
  const trimmedRetryAfter = retryAfter?.trim() ?? '';
  // Only RFC 9110 delta-seconds is waited on; the regex turns every other form
  // (HTTP-date, 1e0, 0x2, 1.5, -1) into NaN, which fails closed below instead
  // of adding load after a wait the connector did not ask for. An absent or
  // empty header defaults to 1s, and 0 is floored to 1s so the retry never
  // lands immediately on an overloaded connector.
  const retryAfterSeconds = trimmedRetryAfter === '' ? 1 : (/^\d{1,3}$/.test(trimmedRetryAfter) ? Number(trimmedRetryAfter) : NaN);
  if (!Number.isInteger(retryAfterSeconds) || retryAfterSeconds > REVOKE_RETRY_AFTER_MAX_SECONDS) return response;
  await discardBody(response);
  const waitMs = Math.max(1, retryAfterSeconds) * 1000;
  await new Promise((resolve) => {
    const timer = setTimeout(resolve, waitMs);
    timer.unref?.();
  });
  // The retry shares the request budget; if it lapsed while waiting, return the
  // 429 (fail closed) rather than issue a request that aborts immediately.
  if (signal.aborted) return response;
  return post();
}

/**
 * Revoke the recipient links minted from an uploaded resource, never the
 * resource itself (upload deduplication can share it with other sends).
 *
 * Watermarked views are minted on the connector's shared fileviewer tunnel, not
 * on the resource the guild owns, so a qURL resource revoke cannot reach them
 * (qurl-integrations-infra#1552). The connector revokes them on our behalf after
 * proving `apiKey` owns `resourceId` and each child maps to it; children it
 * classifies `not_connector_managed` are ordinary tokens the SDK revokes here.
 *
 * TODO(upstream-contract): qurl-integrations-infra#1551 keeps the route
 * default-off, so a 404 means it is not registered (the registered route never
 * returns 404; it denies with 401/403), never that a child is gone.
 * Fall back to the SDK, which succeeds only for ordinary children of this exact
 * source; a watermarked child fails closed until the route is enabled. The same
 * fallback runs when the connector cannot answer (transport failure, timeout,
 * 5xx), rethrowing the connector error if it fails. A 429 (Retry-After: 1) is
 * retried once; 401/403/413, a repeated 429, and anything short of one terminal
 * outcome per requested id throw. Callers keep the send and its parent resource
 * as retry anchors for the user's next revoke.
 *
 * @throws when any requested link may still be live.
 */
async function revokeMintedLinks(resourceId, qurlIds, apiKey) {
  validateResourceId(resourceId);
  if (!Array.isArray(qurlIds)) throw new Error('Invalid connector revoke token list');
  const normalizedIds = qurlIds.map(qurlIdForCleanup);
  if (normalizedIds.includes(null)) throw new Error('Invalid connector revoke token identity');
  const ids = [...new Set(normalizedIds)];
  // An empty list would "succeed" for every recipient a caller maps onto it.
  if (ids.length === 0) throw new Error('No connector revoke token ids to revoke');

  let routeAbsent = false;
  // Per-status tally so rollout can see revoked vs already_gone vs handed back.
  const outcomes = {};
  // Ids revoked through the SDK (route absent or not_connector_managed), so
  // count reconciles with the outcomes tally.
  let fallbackCount = 0;
  // Connector outages must not block ordinary revokes that never needed it:
  // on a transport failure or 5xx, try the SDK fallback, which is fail-closed
  // on its own (it can only confirm children it DELETEs under the verified
  // parent), and rethrow the connector error if that fallback fails too.
  const fallbackOrThrow = async (batchIds, connectorError) => {
    try {
      await revokeOrdinaryLinks(resourceId, batchIds, apiKey);
    } catch (fallbackError) {
      // Keep the connector error (and its api_code) as the verdict, but carry
      // the fallback's diagnosis so failed_child_count survives.
      // A dedicated field: transport errors already carry their own `cause`.
      connectorError.fallbackError = fallbackError;
      if (fallbackError?.failedCount !== undefined) connectorError.failedCount ??= fallbackError.failedCount;
      throw connectorError;
    }
    fallbackCount += batchIds.length;
  };
  let confirmedCount = 0;
  // Ids the connector itself confirmed on a 200 (not via any SDK fallback),
  // counted before any handoff so a failed handoff still shows its progress.
  let connectorDirectCount = 0;
  try {
    for (let offset = 0; offset < ids.length; offset += CONNECTOR_REVOKE_MAX_IDS) {
      const batchIds = ids.slice(offset, offset + CONNECTOR_REVOKE_MAX_IDS);
      // An empty chunk (a broken cap) would confirm vacuously; never let it.
      if (batchIds.length === 0) throw new Error('Connector revoke chunk is empty');
      if (routeAbsent) {
        await revokeOrdinaryLinks(resourceId, batchIds, apiKey);
        fallbackCount += batchIds.length;
        confirmedCount += batchIds.length;
        continue;
      }
      let response;
      try {
        response = await postRevokeLinks(resourceId, batchIds, apiKey);
      } catch (transportError) {
        logger.warn('Connector revoke_links unreachable', {
          resource_ref: resourceIdLogRef(resourceId),
          error_name: transportError?.name,
          count: batchIds.length,
        });
        await fallbackOrThrow(batchIds, transportError);
        confirmedCount += batchIds.length;
        continue;
      }

      if (response.status === 404) {
        routeAbsent = true;
        await discardBody(response);
        await revokeOrdinaryLinks(resourceId, batchIds, apiKey);
        fallbackCount += batchIds.length;
        confirmedCount += batchIds.length;
        continue;
      }
      if (!response.ok) {
        let bodyText = '';
        try {
          bodyText = await response.text();
        } catch { /* network read failed, fall through with empty body */ }
        const { parsed } = parseConnectorBody(bodyText);
        // TODO(upstream-contract): #1551's revoke route puts its enum in a top-level
        // `code` (revoke_not_available, request_rate_limited), unlike mint/upload's
        // `error` string. Surface only that enum; the rest of the body stays out.
        const apiCode = typeof parsed?.code === 'string' && /^[a-z_]{1,64}$/.test(parsed.code) ? parsed.code : null;
        // The durable "route is live but refusing" signal during enablement.
        logger.warn('Connector revoke_links refused', {
          resource_ref: resourceIdLogRef(resourceId),
          status: response.status,
          api_code: apiCode,
          count: batchIds.length,
          // 5xx means the connector could not answer and the SDK fallback runs.
          will_fallback: response.status >= 500,
        });
        let connectorError;
        try {
          throwConnectorErrorFromBody('Connector revoke_links', response, { bodyText, apiCode });
        } catch (err) {
          connectorError = err;
        }
        // 401/403/413/429 mean this request is wrong or must slow down; only a
        // connector that cannot answer (5xx) falls back.
        connectorError ??= new Error(`Connector revoke_links failed (${response.status})`);
        if (response.status < 500) throw connectorError;
        await fallbackOrThrow(batchIds, connectorError);
        confirmedCount += batchIds.length;
        continue;
      }

      let parsed;
      try {
        parsed = await response.json();
      } catch {
        throw new Error('Connector revoke_links returned invalid JSON');
      }

      // Results are unordered: require exactly one confirmed outcome per
      // requested id. An empty, short, duplicate or foreign result set fails.
      if (parsed?.success !== true) {
        throw new Error('Connector revoke_links returned success: false');
      }
      const requested = new Set(batchIds);
      const statuses = new Map();
      const results = Array.isArray(parsed.results) ? parsed.results : [];
      if (results.length !== batchIds.length) {
        throw new Error('Connector revoke_links did not confirm every requested link');
      }
      for (const result of results) {
        if (requested.has(result?.qurl_id) && REVOKE_TERMINAL_STATUSES.has(result.status)
            && !statuses.has(result.qurl_id)) {
          statuses.set(result.qurl_id, result.status);
        }
      }
      if (statuses.size !== batchIds.length) {
        throw new Error('Connector revoke_links did not confirm every requested link');
      }
      const ordinaryIds = batchIds.filter(id => statuses.get(id) === 'not_connector_managed');
      connectorDirectCount += batchIds.length - ordinaryIds.length;
      if (ordinaryIds.length > 0) {
        await revokeOrdinaryLinks(resourceId, ordinaryIds, apiKey);
        fallbackCount += ordinaryIds.length;
      }
      // Tally only once the chunk is confirmed, so outcomes stay exact on failure.
      for (const status of statuses.values()) outcomes[status] = (outcomes[status] || 0) + 1;
      confirmedCount += batchIds.length;
    }
    // Belt and braces for the chunk loop: never report success short of every id.
    if (confirmedCount !== ids.length) throw new Error('Connector revoke did not cover every requested link');
  } finally {
    // route_absent tells rollout verification whether #1551 is live here; the
    // tally is logged on failure too so partial progress stays visible.
    const summary = {
      resource_ref: resourceIdLogRef(resourceId),
      count: ids.length,
      route_absent: routeAbsent,
      outcomes,
      fallback_count: fallbackCount,
    };
    if (confirmedCount === ids.length) {
      logger.info('Revoked minted links', summary);
    } else {
      logger.warn('Minted link revoke incomplete', {
        ...summary,
        confirmed_count: confirmedCount,
        connector_direct_count: connectorDirectCount,
      });
    }
  }
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
// Extended (built-ins ∪ extras) via config.DETECT_EXTRA_NON_PROD_QURL_ENDPOINT_HOSTS
// — env-injected by the private infra repo so real sandbox/staging hostnames
// never need to be committed to this public repo. `|| []` keeps every mock of
// ../src/config in the test suite that omits the field working unchanged
// (empty extra set == today's behavior). See config.js for the parsing +
// fail-fast shape validation of both DETECT_EXTRA_NON_PROD_* env vars.
const DETECT_TUNNEL_NON_PROD_QURL_ENDPOINT_HOSTS = new Set([
  'localhost',
  '127.0.0.1',
  '[::1]',
  'api.test.local',
  'api.staging.layerv.ai',
  ...(config.DETECT_EXTRA_NON_PROD_QURL_ENDPOINT_HOSTS || []),
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
    return [
      DETECT_TUNNEL_PROD_HOST_SUFFIX,
      ...DETECT_TUNNEL_NON_PROD_HOST_SUFFIXES,
      ...(config.DETECT_EXTRA_NON_PROD_HOST_SUFFIXES || []),
    ];
  }
  return [DETECT_TUNNEL_PROD_HOST_SUFFIX];
}

// Intentional load-time computation: QURL_ENDPOINT is static for a bot process,
// and tests that vary it use jest.resetModules() before requiring connector.js.
const DETECT_TUNNEL_HOST_SUFFIXES = detectTunnelHostSuffixesForEndpoint(config.QURL_ENDPOINT);

// Module-level cache for the detect tunnel's CRID (resolved from
// DETECT_TUNNEL_SLUG via the SDK's listAllResources auto-paginator). The
// CRID is a stable, NON-secret identifier, so caching it across calls is
// safe and skips a slug lookup on every detect. CACHE ONLY THIS, NEVER the
// minted access token or qurl_site: each detect mints a FRESH ephemeral qURL (a
// short-lived credential — mint and session durations are both '5m') and the native opening/knock
// grants network access to the caller's CURRENT IP/knock-window. A stale token
// would be a long-lived credential to leak; qurl_site is per-mint and must stay
// paired with the fresh knock.
let _detectCrid = null;
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
  // or high-volume, or mint failures become guild-specific.
  if (clearResourceCache) _detectCrid = null;
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
// resolveDetectTarget() to self-mint the ephemeral detect qURL over
// the reverse-tunnel. Constructed on first use (not at module load) so the bot
// boots even when QURL_API_KEY is unset in non-detect deployments, and so tests
// can inject a mocked @layervai/qurl before the first call.
//
// Cache the client, never the minted qurl_site or access token — native opening
// re-knocks per call (the full no-cache invariant + rationale live on
// _detectCrid above and in resolveDetectTarget's docstring).
//
// The bot credential owns the detect tunnel. Mint only the exact guild path
// taken from the authenticated Discord interaction; the image request carries no API credential.
let _qurlClient = null;
function getQurlClient() {
  if (!_qurlClient) {
    // baseUrl is the bare qURL API base (no `/v1`) — the SDK prepends the
    // versioned path itself.
    //
    // timeout / maxRetries match the SDK's current defaults but are pinned
    // explicitly so the detect legs' resilience stays stable against
    // SDK-default drift. List and mint use 30s request timeouts, below
    // the detect POST's 60s; the retry worst case stays inside
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

// Safe host-only context for the qurl_site rejection breadcrumb. The guards
// intentionally throw constant URL-free messages, so this hostname is the
// operator signal that distinguishes an infra suffix drift from an SSRF probe.
// `hostname` excludes credentials, port, path, query, and fragment; undefined
// on malformed input lets JSON logging omit the field instead of echoing a URL.
function detectTargetHostname(qurlSite) {
  try {
    return new URL(qurlSite).hostname;
  } catch {
    return undefined;
  }
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
// from a TRUSTED authenticated mint (not user input) and fresh native opening keeps
// the knock window tight, so a DNS round-trip per detect isn't warranted. A
// future reader should NOT assume this carries the link guard's DNS guarantee.
function assertPublicHttpsTarget(targetUrl, expectedQurlSiteHost) {
  let parsed;
  try {
    parsed = new URL(targetUrl);
  } catch {
    // buildDetectTargetUrl passes the serialized target URL today; keep this as
    // defense-in-depth if a future caller validates a raw target directly.
    throw new Error('Detect tunnel qurl_site target is unparseable');
  }
  if (parsed.protocol !== 'https:') {
    throw new Error('Detect tunnel qurl_site target must be an https: URL');
  }
  // buildDetectTargetUrl deliberately retains any credentials in its candidate
  // URL so this final-target guard can reject them before returning the
  // credential-free target, issuing the NHP knock, or posting image bytes.
  if (parsed.username || parsed.password) {
    throw new Error('Detect tunnel qurl_site target must not contain userinfo');
  }
  if (isPrivateHost(parsed.hostname)) {
    throw new Error('Detect tunnel qurl_site target points to a private/internal address');
  }
  // Pin the authenticated mint host to the configured tunnel namespace.
  // The native ACK and exact guild path are checked before sending bytes.
  const targetHost = parsed.hostname;
  if (targetHost !== expectedQurlSiteHost) {
    throw new Error('Detect tunnel qurl_site host does not match the returned qurl_site');
  }
  // TODO(upstream-contract): qurl_site is an authenticated, host-only tunnel
  // origin whose routing labels are opaque. If qurl-service changes its tunnel
  // hostname shapes, update this namespace pin and its deployment allowlists.
  const hasAllowedSuffix = DETECT_TUNNEL_HOST_SUFFIXES.some((suffix) => {
    if (!targetHost.endsWith(suffix)) return false;
    const prefix = targetHost.slice(0, -suffix.length);
    return prefix.split('.').every(Boolean);
  });
  if (!hasAllowedSuffix) {
    throw new Error('Detect tunnel qurl_site host is not under an expected qURL tunnel domain');
  }
}

// Join the trusted origin with the bot-constructed path, checked against the mint echo.
function buildDetectTargetUrl(qurlSite, targetPath) {
  let parsed;
  try {
    parsed = new URL(qurlSite);
  } catch {
    throw new DetectQurlSiteError('detect mint returned an unparseable qurl_site');
  }
  if (parsed.pathname !== '/' || parsed.search || parsed.hash) {
    throw new DetectQurlSiteError('detect mint qurl_site must be host-only');
  }
  const target = new URL(targetPath, parsed);
  assertPublicHttpsTarget(target.href, parsed.hostname);
  return new URL(targetPath, parsed.origin).href;
}

// Scrub legacy access tokens and qv2t1 credentials before logging.
// Native credentials originate in the mint response fragment. Keep redaction
// independent of SDK error formatting; also scrub rejected legacy credentials.
function redactAccessToken(message) {
  return String(message ?? '').replace(/at_[A-Za-z0-9_-]+/g, 'at_[REDACTED]')
    .replace(/qv2t1\.[A-Za-z0-9_.-]+/g, 'qv2t1.[REDACTED]');
}

async function closeDetectOpener(opener) {
  try {
    await opener.close();
  } catch (err) {
    logger.warn('Detect native opener close failed', { error: redactAccessToken(err?.message) });
  }
}

/** Mint a fresh signed qURL for the authenticated guild's exact detect path. */
async function resolveDetectTarget(guildId) {
  if (!config.DETECT_TUNNEL_SLUG) {
    throw new Error('DETECT_TUNNEL_SLUG is not configured (required to reach the detect tunnel)');
  }
  assertDetectResourceFailureBackoffAllowed();

  // Resolve the tunnel CRID from the slug, cached across calls — it's a
  // stable, non-secret identifier. Assign the cache ONLY after a successful
  // extract so a failed lookup doesn't poison it. The SDK owns pagination:
  // listAllResources yields resources from every page. SDK 2.x resource item
  // methods accept only the `crid`, never the public-key `resource_id`. There
  // is intentionally no in-flight dedup for concurrent cold-cache lookups; the
  // failure backoff bounds repeated hard failures.
  let crid = _detectCrid;
  if (!crid) {
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
    if (!active[0]) {
      const err = new Error('Detect tunnel resource not found for slug');
      rememberDetectResourceFailure(err, { immediateBackoff: true });
      throw err;
    }
    // TODO(upstream-contract): GET /v1/resources items carry `crid`.
    crid = typeof active[0].crid === 'string' ? active[0].crid : null;
    if (!crid) {
      const err = new Error('Detect tunnel resource listing returned no crid');
      rememberDetectResourceFailure(err, { immediateBackoff: true });
      throw err;
    }
    _detectCrid = crid;
  }

  // Mint a fresh qURL and bound its access session separately. Expiring a
  // qURL does not shorten an already-open native access grant.
  const targetPath = `${DETECT_TARGET_PATH}/discord/${guildId}`;
  let targetUrl;
  let minted;
  try {
    minted = await getQurlClient().createQurlForResource(crid, {
      expires_in: DETECT_LINK_EXPIRES_IN,
      session_duration: DETECT_LINK_EXPIRES_IN,
      target_path: targetPath,
    });
  } catch (err) {
    // Self-heal a stale CRID: if the tunnel resource was deleted/
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
    // NHP authorizes the path against active grants; infra scopes lookup by guild.
    if (minted?.target_path !== targetPath) {
      throw new Error('detect mint returned a mismatched guild path');
    }
    targetUrl = buildDetectTargetUrl(minted?.qurl_site, targetPath);
  } catch (err) {
    // qurl_site hostname-pin failures happen after a successful slug
    // lookup and mint, so keep the cached CRID and retry the mint after
    // the short failure window instead of re-walking slug history. The mint
    // created an unredeemed 5m qURL, but failing before native opening is the safe
    // trade: no NHP knock and no image POST are issued to an untrusted host.
    rememberDetectResourceFailure(err, { clearResourceCache: false });
    const label = err instanceof DetectQurlSiteError
      ? 'Detect tunnel mint returned an invalid qurl_site'
      : 'Detect tunnel target rejected';
    logger.warn(label, {
      error: redactAccessToken(err.message),
      hostname: detectTargetHostname(minted?.qurl_site),
    });
    throw err;
  }

  let clearResourceCache = false;
  try {
    // qv2t1 carries an offline credential, not an at_ API-resolve token.
    // The native SDK verifies the issuer and cell against deployment trust,
    // and expectedCRID binds the signed resource key to the slug-resolved CRID.
    if (typeof minted?.qurl_link === 'string' && minted.qurl_link.split('#')[1]?.startsWith('qv2t1.')) {
      // TODO(upstream-contract): POST /v1/resources/{crid}/qurls echoes the
      // addressed CRID; a missing echo fails closed here like a mismatch.
      if (minted.crid !== crid) {
        const err = new Error('Detect mint returned a mismatched crid');
        clearResourceCache = true;
        throw err;
      }
      const { createPortalOpener } = require('@layervai/qurl/node');
      const opener = createPortalOpener({ qurl: minted.qurl_link, expectedCRID: crid });
      try {
        // SDK 2.x bounds native opening to 15 seconds and aborts it on close.
        await opener.start();
        clearDetectResourceFailureState();
        return { targetUrl, opener };
      } catch (err) {
        await closeDetectOpener(opener);
        // The command handler also logs this error; keep credentials out of it.
        throw new Error(redactAccessToken(err.message));
      }
    }
    throw new Error('Detect requires a signed native qURL');
  } catch (err) {
    // A malformed qurl_link is a mint response-shape issue, not evidence that
    // the cached CRID is stale. Keep the resource cache and retry only
    // the mint after the short failure window.
    rememberDetectResourceFailure(err, { clearResourceCache });
    logger.warn('Detect native open or link validation failed', { error: redactAccessToken(err.message) });
    throw err;
  }
}

/** Recover attribution through an exact, signed external guild path. */
async function detectWatermark(imageBytes, { guildId, contentType } = {}) {
  if (!config.QURL_API_KEY) throw new Error('QURL_API_KEY is not configured');
  // TODO(upstream-contract): Discord snowflakes are canonical 17–20 digit strings.
  if (typeof guildId !== 'string' || !/^[0-9]{17,20}$/.test(guildId)) {
    throw new Error('detectWatermark requires a valid Discord guild id');
  }
  const { targetUrl, opener } = await resolveDetectTarget(guildId);

  try {
    const request = {
      method: 'POST',
      headers: {
        'Content-Type': contentType || 'application/octet-stream',
      },
      body: imageBytes,
      // Neural-net inference is the slow leg here; give it the same 60s
      // headroom the upload paths use rather than the 30s mint window.
      signal: AbortSignal.timeout(60000),
    };
    // TODO(upstream-contract): SDK 2.x fetch authenticates the signed target before this callback.
    const response = await opener.fetch((authenticatedTarget) => {
      // Check the signed ACK target before sending image bytes.
      if (authenticatedTarget.href !== targetUrl) {
        throw new Error('Detect native target does not match the minted tunnel');
      }
      return request;
    }, { redirects: 'error' });

    if (!response.ok) {
      return await throwConnectorError('Connector detect', response);
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
  } catch (err) {
    const message = redactAccessToken(err?.message);
    if (typeof err?.message === 'string' && message !== err.message) {
      const safeError = new Error(message);
      safeError.status = err?.status;
      throw safeError;
    }
    throw err;
  } finally {
    await closeDetectOpener(opener);
  }
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

module.exports = { uploadToConnector, downloadAndUpload, reUploadBuffer, mintLinks, revokeMintedLinks, detectWatermark, uploadJsonToConnector, isAllowedSourceUrl, detectTunnelHostSuffixesForEndpoint };
