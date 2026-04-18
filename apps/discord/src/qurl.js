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
    // Log only status + body length. Including the raw body would be
    // dangerous: the QURL API error response may echo request headers or
    // tokens, and anyone flipping LOG_LEVEL=debug during an incident would
    // then tail those into logs.
    let bodyLen = 0;
    try { bodyLen = (await resp.text()).length; } catch { /* ignore */ }
    logger.debug('QURL API error', { method, path, status: resp.status, bodyLen });
    throw new Error(`QURL API ${method} ${path} failed (${resp.status})`);
  }

  if (resp.status === 204) return null;

  const envelope = await resp.json();
  return envelope.data || envelope;
}

// Reject hostnames that resolve (by syntax) to loopback, link-local, or
// RFC1918 private ranges. Defense-in-depth against a caller passing
// `http://169.254.169.254/latest/meta-data/...` or similar; even if the
// downstream QURL API is the one that actually fetches, we block at our
// own input validation layer.
function isPrivateHost(host) {
  if (!host) return true;
  const h = host.toLowerCase();
  if (h === 'localhost' || h === '0.0.0.0' || h === '::' || h === '::1') return true;
  if (h.startsWith('[') && h.endsWith(']')) {
    // Bracketed IPv6 literal — strip and check.
    return isPrivateHost(h.slice(1, -1));
  }
  // IPv6 common locals
  if (h === '::1' || h.startsWith('fc') || h.startsWith('fd') || h.startsWith('fe80:')) return true;
  // IPv4 literal
  const v4 = h.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);
  if (v4) {
    const [a, b] = v4.slice(1).map(Number);
    if (a === 10) return true;                          // 10.0.0.0/8
    if (a === 127) return true;                         // 127.0.0.0/8
    if (a === 169 && b === 254) return true;            // 169.254.0.0/16 (link-local, IMDS)
    if (a === 172 && b >= 16 && b <= 31) return true;   // 172.16.0.0/12
    if (a === 192 && b === 168) return true;            // 192.168.0.0/16
    if (a === 100 && b >= 64 && b <= 127) return true;  // 100.64.0.0/10 (CGNAT)
    if (a === 0) return true;                           // 0.0.0.0/8
    if (a >= 224) return true;                          // multicast + reserved
  }
  return false;
}

async function createOneTimeLink(targetUrl, expiresIn, description, apiKey) {
  try {
    const parsed = new URL(targetUrl);
    if (!['http:', 'https:'].includes(parsed.protocol)) {
      throw new Error('Only http/https URLs are allowed');
    }
    if (isPrivateHost(parsed.hostname)) {
      throw new Error('Target URL points to a private/internal address');
    }
  } catch (err) {
    if (/(http|private)/i.test(err.message)) throw err;
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
