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
 * Upload a file to the qurl-s3-connector by streaming from a source URL.
 * The file is piped from Discord CDN -> bot (in transit) -> connector -> S3.
 * Memory usage is O(chunk size), not O(file size).
 */
async function uploadToConnector(sourceUrl, filename, contentType) {
  if (!isAllowedSourceUrl(sourceUrl)) {
    throw new Error('Source URL is not a valid Discord CDN URL');
  }

  const downloadResponse = await fetch(sourceUrl);
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
 * Mint one-time links for an uploaded resource via the connector.
 */
async function mintLinks(resourceId, expiresAt, n) {
  const response = await fetch(`${config.CONNECTOR_URL}/api/mint_link/${resourceId}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...connectorAuthHeaders() },
    body: JSON.stringify({ expires_at: expiresAt, n }),
  });

  if (!response.ok) {
    const text = await response.text();
    throw new Error(`Connector mint_link failed (${response.status}): ${text}`);
  }

  const result = await response.json();
  if (!result.success) {
    throw new Error('Connector mint_link returned success: false');
  }

  logger.info('Minted links', { resource_id: resourceId, count: result.links.length });
  return result.links;
}

module.exports = { uploadToConnector, mintLinks, isAllowedSourceUrl };
