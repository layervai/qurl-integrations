const config = require('./config');
const logger = require('./logger');

const { sanitizeFilename } = require('./utils/sanitize');

const MAX_FILE_SIZE = 25 * 1024 * 1024;

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
    while (true) {
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
 * Upload a file to the qurl-s3-connector.
 * Downloads from Discord CDN, then uploads to connector.
 * Note: file is fully buffered in memory (up to 25MB max).
 */
async function uploadToConnector(sourceUrl, filename, contentType, apiKey) {
  filename = sanitizeFilename(filename);
  if (!apiKey && !config.QURL_API_KEY) throw new Error('QURL_API_KEY is not configured');
  if (!isAllowedSourceUrl(sourceUrl)) {
    throw new Error('Source URL is not a valid Discord CDN URL');
  }

  const downloadResponse = await fetch(sourceUrl, { signal: AbortSignal.timeout(30000), redirect: 'error' });
  if (!downloadResponse.ok) {
    throw new Error(`Failed to download from Discord CDN: ${downloadResponse.status}`);
  }

  const contentLength = parseInt(downloadResponse.headers.get('content-length') || '0', 10);
  if (contentLength > MAX_FILE_SIZE) {
    throw new Error(`File too large: ${Math.round(contentLength / 1024 / 1024)}MB, max 25MB`);
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
    const text = await uploadResponse.text();
    throw new Error(`Connector upload failed (${uploadResponse.status}): ${text}`);
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
    const text = await uploadResponse.text();
    throw new Error(`Connector re-upload failed (${uploadResponse.status}): ${text}`);
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

  const downloadResponse = await fetch(sourceUrl, { signal: AbortSignal.timeout(30000), redirect: 'error' });
  if (!downloadResponse.ok) {
    throw new Error(`Failed to download from Discord CDN: ${downloadResponse.status}`);
  }

  const contentLength = parseInt(downloadResponse.headers.get('content-length') || '0', 10);
  if (contentLength > MAX_FILE_SIZE) {
    throw new Error(`File too large: ${Math.round(contentLength / 1024 / 1024)}MB, max 25MB`);
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
  const response = await fetch(`${config.CONNECTOR_URL}/api/mint_link/${resourceId}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...connectorAuthHeaders(apiKey) },
    body: JSON.stringify({ expires_at: expiresAt, n }),
    signal: AbortSignal.timeout(30000),
  });

  if (!response.ok) {
    const text = await response.text();
    throw new Error(`Connector mint_link failed (${response.status}): ${text}`);
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
    const text = await uploadResponse.text();
    throw new Error(`Connector JSON upload failed (${uploadResponse.status}): ${text}`);
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
