const config = require('./config');
const logger = require('./logger');

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
 * Upload a file to the qurl-s3-connector.
 * Downloads from Discord CDN, then uploads to connector.
 * Note: file is fully buffered in memory (up to 25MB max).
 */
async function uploadToConnector(sourceUrl, filename, contentType) {
  if (!config.QURL_API_KEY) throw new Error('QURL_API_KEY is not configured');
  if (!isAllowedSourceUrl(sourceUrl)) {
    throw new Error('Source URL is not a valid Discord CDN URL');
  }

  const downloadResponse = await fetch(sourceUrl, { signal: AbortSignal.timeout(30000), redirect: 'error' });
  if (!downloadResponse.ok) {
    throw new Error(`Failed to download from Discord CDN: ${downloadResponse.status}`);
  }

  const fileBuffer = await downloadResponse.arrayBuffer();
  const blob = new Blob([fileBuffer], { type: contentType || 'application/octet-stream' });

  const form = new FormData();
  form.append('file', blob, filename);

  const uploadResponse = await fetch(`${config.CONNECTOR_URL}/api/upload`, {
    method: 'POST',
    body: form,
    headers: { ...connectorAuthHeaders() },
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
async function reUploadBuffer(fileBuffer, filename, contentType) {
  if (!config.QURL_API_KEY) throw new Error('QURL_API_KEY is not configured');

  const blob = new Blob([fileBuffer], { type: contentType || 'application/octet-stream' });
  const form = new FormData();
  form.append('file', blob, filename);

  const uploadResponse = await fetch(`${config.CONNECTOR_URL}/api/upload`, {
    method: 'POST',
    body: form,
    headers: { ...connectorAuthHeaders() },
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
async function downloadAndUpload(sourceUrl, filename, contentType) {
  if (!isAllowedSourceUrl(sourceUrl)) {
    throw new Error('Source URL is not a valid Discord CDN URL');
  }

  const downloadResponse = await fetch(sourceUrl, { signal: AbortSignal.timeout(30000), redirect: 'error' });
  if (!downloadResponse.ok) {
    throw new Error(`Failed to download from Discord CDN: ${downloadResponse.status}`);
  }

  const fileBuffer = await downloadResponse.arrayBuffer();
  const result = await reUploadBuffer(fileBuffer, filename, contentType);
  return { ...result, fileBuffer };
}

/**
 * Mint one-time links for an uploaded resource via the connector.
 */
async function mintLinks(resourceId, expiresAt, n) {
  if (!config.QURL_API_KEY) throw new Error('QURL_API_KEY is not configured');
  if (!resourceId || !/^[\w-]+$/.test(resourceId)) {
    throw new Error(`Invalid resource ID format: ${resourceId}`);
  }
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

module.exports = { uploadToConnector, downloadAndUpload, reUploadBuffer, mintLinks, isAllowedSourceUrl };
