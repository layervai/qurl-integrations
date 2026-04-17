const config = require('./config');
const logger = require('./logger');

/**
 * Lightweight QURL API client using fetch.
 * Avoids ESM/CJS compatibility issues with the @layerv/qurl SDK.
 */

async function qurlFetch(method, path, body, apiKey) {
  const key = apiKey || config.QURL_API_KEY;
  if (!key) {
    throw new Error('QURL_API_KEY is not configured');
  }
  const url = `${config.QURL_ENDPOINT}/v1${path}`;
  const opts = {
    method,
    headers: {
      'Content-Type': 'application/json',
      'Authorization': `Bearer ${key}`,
      'User-Agent': 'qurl-discord-bot/1.0',
    },
  };
  if (body) opts.body = JSON.stringify(body);
  opts.signal = AbortSignal.timeout(30000);

  const resp = await fetch(url, opts);
  if (!resp.ok) {
    // Keep the raw response body in debug logs only — it may echo request
    // headers/tokens. The thrown error stays generic so it won't leak into
    // Discord ephemeral replies or warn-level logs.
    const text = await resp.text();
    logger.debug('QURL API error response', { method, path, status: resp.status, body: text });
    throw new Error(`QURL API ${method} ${path} failed (${resp.status})`);
  }

  if (resp.status === 204) return null;

  const envelope = await resp.json();
  return envelope.data || envelope;
}

async function createOneTimeLink(targetUrl, expiresIn, description, apiKey) {
  try {
    const parsed = new URL(targetUrl);
    if (!['http:', 'https:'].includes(parsed.protocol)) {
      throw new Error('Only http/https URLs are allowed');
    }
  } catch (err) {
    if (err.message.includes('http')) throw err;
    throw new Error(`Invalid target URL: ${err.message}`);
  }

  const result = await qurlFetch('POST', '/qurls', {
    target_url: targetUrl,
    one_time_use: true,
    expires_in: expiresIn,
    description,
  }, apiKey);

  logger.info('Created one-time QURL', { resource_id: result.resource_id, expires_in: expiresIn });
  return result;
}

function validateResourceId(resourceId) {
  if (!resourceId || !/^[\w-]+$/.test(resourceId)) {
    throw new Error(`Invalid resource ID format: ${resourceId}`);
  }
}

async function deleteLink(resourceId, apiKey) {
  validateResourceId(resourceId);
  await qurlFetch('DELETE', `/qurls/${resourceId}`, null, apiKey);
  logger.info('Revoked QURL', { resource_id: resourceId });
}

async function getResourceStatus(resourceId, apiKey) {
  validateResourceId(resourceId);
  return qurlFetch('GET', `/qurls/${resourceId}`, null, apiKey);
}

module.exports = { createOneTimeLink, deleteLink, getResourceStatus };
