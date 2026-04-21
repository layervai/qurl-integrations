const config = require('./config');
const logger = require('./logger');

const { sanitizeFilename } = require('./utils/sanitize');

const { MAX_FILE_SIZE } = require('./constants');
const MAX_CDN_REDIRECTS = 3;

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
// QURL API error codes the connector can pass back via the `error` string.
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

/**
 * Upload a file to the qurl-s3-connector. Downloads from Discord CDN, then
 * uploads to the connector.
 *
 * @deprecated for NEW code — use `downloadAndUpload` which also returns the
 * buffered file so callers can re-upload without a second round trip. This
 * function is retained because its SSRF-rejection path is directly tested in
 * tests/connector-coverage.test.js and tests/qurl-send.test.js, and those
 * cases would lose coverage if it were removed.
 */
async function uploadToConnector(sourceUrl, filename, contentType, apiKey) {
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
    hash: result.hash,
    resource_id: result.resource_id,
  });

  return result;
}

/**
 * Re-register an already-downloaded file buffer with the connector.
 * Creates a new QURL resource (with a fresh token pool) without
 * re-downloading from Discord CDN. Used when the per-resource token
 * quota (10) is exhausted and more recipients need links.
 */
async function reUploadBuffer(fileBuffer, filename, contentType, apiKey) {
  filename = sanitizeFilename(filename);
  if (!apiKey && !config.QURL_API_KEY) throw new Error('QURL_API_KEY is not configured');

  const blob = new Blob([fileBuffer], { type: contentType || 'application/octet-stream' });
  const form = new FormData();
  form.append('file', blob, filename);

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
    hash: result.hash,
    resource_id: result.resource_id,
  });

  return result;
}

/**
 * Download a file from Discord CDN and return the buffer + upload result.
 * The buffer is cached so subsequent re-uploads don't re-download.
 */
async function downloadAndUpload(sourceUrl, filename, contentType, apiKey) {
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
  const result = await reUploadBuffer(fileBuffer, filename, contentType, apiKey);
  return { ...result, fileBuffer };
}

/**
 * Mint one-time links for an uploaded resource via the connector.
 */
async function mintLinks(resourceId, expiresAt, n, apiKey) {
  if (!apiKey && !config.QURL_API_KEY) throw new Error('QURL_API_KEY is not configured');
  if (!resourceId || !/^[\w-]+$/.test(resourceId)) {
    throw new Error(`Invalid resource ID format: ${resourceId}`);
  }
  // Bound `n` defensively — callers in this codebase already cap at 10
  // (TOKENS_PER_RESOURCE) or 50 (recipient max), but mintLinks is exported
  // so validate at the API boundary. Negative or non-integer values would
  // make the QURL backend behave unpredictably; 100 is a comfortable ceiling.
  if (!Number.isInteger(n) || n < 1 || n > 100) {
    throw new Error(`Invalid link count (n must be integer 1..100): ${n}`);
  }
  const response = await fetch(`${config.CONNECTOR_URL}/api/mint_link/${resourceId}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...connectorAuthHeaders(apiKey) },
    body: JSON.stringify({ expires_at: expiresAt, n }),
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

/**
 * Upload a JSON object to the connector as a file.
 * Used for structured payloads like location data.
 */
async function uploadJsonToConnector(jsonPayload, filename, apiKey) {
  filename = sanitizeFilename(filename);
  if (!apiKey && !config.QURL_API_KEY) throw new Error('QURL_API_KEY is not configured');

  const blob = new Blob([JSON.stringify(jsonPayload)], { type: 'application/json' });
  const form = new FormData();
  form.append('file', blob, filename);

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
    hash: result.hash,
    resource_id: result.resource_id,
  });

  return result;
}

module.exports = { uploadToConnector, downloadAndUpload, reUploadBuffer, mintLinks, uploadJsonToConnector, isAllowedSourceUrl };
