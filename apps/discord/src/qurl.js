const config = require('./config');
const logger = require('./logger');

/**
 * Lightweight QURL API client using fetch.
 * Avoids ESM/CJS compatibility issues with the @layerv/qurl SDK.
 */

async function qurlFetch(method, path, body) {
  const url = `${config.QURL_ENDPOINT}/v1${path}`;
  const opts = {
    method,
    headers: {
      'Content-Type': 'application/json',
      'Authorization': `Bearer ${config.QURL_API_KEY}`,
      'User-Agent': 'opennhp-discord-bot/1.0',
    },
  };
  if (body) opts.body = JSON.stringify(body);
  opts.signal = AbortSignal.timeout(30000);

  const resp = await fetch(url, opts);
  if (!resp.ok) {
    const text = await resp.text();
    throw new Error(`QURL API ${method} ${path} failed (${resp.status}): ${text}`);
  }

  if (resp.status === 204) return null;

  const envelope = await resp.json();
  return envelope.data || envelope;
}

async function createOneTimeLink(targetUrl, expiresIn, description) {
  const result = await qurlFetch('POST', '/qurls', {
    target_url: targetUrl,
    one_time_use: true,
    expires_in: expiresIn,
    description,
  });

  logger.info('Created one-time QURL', { resource_id: result.resource_id, expires_in: expiresIn });
  return result;
}

async function deleteLink(resourceId) {
  await qurlFetch('DELETE', `/qurls/${resourceId}`);
  logger.info('Revoked QURL', { resource_id: resourceId });
}

async function getResourceStatus(resourceId) {
  return qurlFetch('GET', `/qurls/${resourceId}`);
}

module.exports = { createOneTimeLink, deleteLink, getResourceStatus };
