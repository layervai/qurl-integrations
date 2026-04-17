const config = require('./config');
const logger = require('./logger');
const { validateResourceId } = require('./qurl');

// Allowed Discord CDN domains for attachment URLs (SSRF prevention)
const ALLOWED_CDN_HOSTS = [
  'cdn.discordapp.com',
  'media.discordapp.net',
];

function isAllowedSourceUrl(sourceUrl) {
  try {
    const parsed = new URL(sourceUrl);
    return parsed.protocol === 'https:' && ALLOWED_CDN_HOSTS.includes(parsed.hostname);
  } catch {
    return false;
  }
}

/**
 * Build auth headers for connector requests.
 * Uses QURL_API_KEY as a shared bearer token.
 */
function connectorAuthHeaders() {
  const headers = {};
  if (config.QURL_API_KEY) {
    headers['Authorization'] = `Bearer ${config.QURL_API_KEY}`;
  }
  return headers;
}

/**
 * Shared helper: POST a FormData body to the connector upload endpoint,
 * check the response, and return the parsed JSON result.
 */
function requireApiKey() {
  if (!config.QURL_API_KEY) throw new Error('QURL_API_KEY is not configured');
}

// TODO: AbortSignal.timeout is not unit-tested — testing it requires real timing which is flaky.
async function postToConnector(form, timeoutMs = 60000) {
  const response = await fetch(`${config.CONNECTOR_URL}/api/upload`, {
    method: 'POST',
    body: form,
    headers: { ...connectorAuthHeaders() },
    signal: AbortSignal.timeout(timeoutMs),
  });
  if (!response.ok) {
    const text = await response.text();
    throw new Error(`Connector upload failed (${response.status}): ${text}`);
  }
  const result = await response.json();
  if (!result.success) {
    throw new Error('Connector upload returned success: false');
  }
  if (!result.resource_id) {
    logger.warn('Connector response missing resource_id', { result });
  }
  return result;
}

/**
 * Upload a file to the qurl-s3-connector.
 * Downloads from Discord CDN, then uploads to connector.
 * Note: file is fully buffered in memory (up to 25MB max).
 */
async function uploadToConnector(sourceUrl, filename, contentType) {
  requireApiKey();
  if (!isAllowedSourceUrl(sourceUrl)) {
    throw new Error('Source URL is not a valid Discord CDN URL');
  }

  // Sanitize filename to prevent path traversal (defense-in-depth; commands.js also sanitizes)
  filename = filename.replace(/[/\\:*?"<>|\x00-\x1f]/g, '_').replace(/\.\./g, '_').substring(0, 200);

  // redirect: 'error' prevents SSRF via open redirects. Discord CDN rarely redirects for direct attachment URLs.
  const downloadResponse = await fetch(sourceUrl, { signal: AbortSignal.timeout(30000), redirect: 'error' });
  if (!downloadResponse.ok) {
    throw new Error(`Failed to download from Discord CDN: ${downloadResponse.status}`);
  }

  // Defense-in-depth file size check (commands.js also validates attachment.size)
  const contentLength = parseInt(downloadResponse.headers.get('content-length') || '0', 10);
  if (contentLength > 25 * 1024 * 1024) {
    throw new Error(`File too large (${Math.round(contentLength / 1024 / 1024)}MB). Maximum is 25MB.`);
  }

  const fileBuffer = await downloadResponse.arrayBuffer();
  const blob = new Blob([fileBuffer], { type: contentType || 'application/octet-stream' });

  const form = new FormData();
  form.append('file', blob, filename);

  const result = await postToConnector(form, 60000);

  logger.info('Uploaded to connector', {
    hash: result.hash,
    resource_id: result.resource_id,
  });

  return result;
}

/**
 * Mint one-time links for an uploaded resource via the connector.
 */
async function mintLinks(resourceId, expiresAt, n) {
  requireApiKey();
  validateResourceId(resourceId);
  const response = await fetch(`${config.CONNECTOR_URL}/api/mint_link/${resourceId}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...connectorAuthHeaders() },
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
 * Upload a JSON payload directly to the connector (e.g., google-map location data).
 * The fileviewer renders these based on their type field.
 */
async function uploadJsonToConnector(jsonData, filename) {
  requireApiKey();
  const jsonStr = JSON.stringify(jsonData);
  const blob = new Blob([jsonStr], { type: 'application/json' });

  const form = new FormData();
  form.append('file', blob, filename || 'location.json');

  const result = await postToConnector(form, 30000);

  logger.info('Uploaded JSON to connector', {
    hash: result.hash,
    resource_id: result.resource_id,
    type: jsonData.type,
  });

  return result;
}

module.exports = { uploadToConnector, uploadJsonToConnector, mintLinks, isAllowedSourceUrl };
