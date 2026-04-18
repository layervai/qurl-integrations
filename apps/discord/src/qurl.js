const config = require('./config');
const logger = require('./logger');
const dns = require('dns').promises;

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
  // IPv6 common locals (::1 already handled above for exact-match; this
  // catches fc00::/7 unique-local and fe80::/10 link-local prefixes).
  if (h.startsWith('fc') || h.startsWith('fd') || h.startsWith('fe80:')) return true;
  // IPv4-mapped IPv6 literal: ::ffff:127.0.0.1, ::ffff:7f00:1, etc. Strip the
  // prefix (URL parsing already stripped the brackets) and re-check.
  const mapped = h.match(/^::ffff:([0-9.]+)$/);
  if (mapped) return isPrivateHost(mapped[1]);
  // Decimal IPv4 literal (e.g. `2130706433` = 127.0.0.1) — browsers accept,
  // Node's URL does too. Convert to dotted-quad.
  if (/^\d+$/.test(h)) {
    const n = Number(h);
    if (n >= 0 && n <= 0xFFFFFFFF) {
      const dotted = [(n >>> 24) & 0xFF, (n >>> 16) & 0xFF, (n >>> 8) & 0xFF, n & 0xFF].join('.');
      return isPrivateHost(dotted);
    }
    return true; // out-of-range numeric host: reject outright
  }
  // Hex IPv4 literal (e.g. `0x7f000001` = 127.0.0.1)
  if (/^0x[0-9a-f]+$/.test(h)) {
    const n = Number(h);
    if (Number.isFinite(n) && n >= 0 && n <= 0xFFFFFFFF) {
      const dotted = [(n >>> 24) & 0xFF, (n >>> 16) & 0xFF, (n >>> 8) & 0xFF, n & 0xFF].join('.');
      return isPrivateHost(dotted);
    }
    return true;
  }
  // Octal-prefixed IPv4 (e.g. `0177.0.0.1`) — treat any leading-zero component
  // as suspicious and reject conservatively.
  if (/^0\d/.test(h) && /^[0-9.]+$/.test(h)) return true;
  // Standard IPv4 dotted-quad
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

// Resolve all A/AAAA records for a hostname and reject if ANY of them point
// to a private/internal range. Defense against DNS rebinding: a malicious
// domain could answer `isPrivateHost` (which is syntactic) with a public IP
// but then resolve to 169.254.169.254 or 127.0.0.1 at fetch time on the
// QURL backend. We resolve up-front and pass the result to the QURL API so
// the backend can pin to the same IPs we verified — the API has its own
// SSRF guard but we also block here.
async function assertNotPrivateAfterResolve(hostname) {
  // Numeric hosts already covered by syntactic isPrivateHost; only resolve
  // actual names. IPv6-in-brackets is stripped in isPrivateHost already.
  if (/^\d+\.\d+\.\d+\.\d+$/.test(hostname) || hostname.startsWith('[')) return;
  let addrs;
  try {
    addrs = await dns.lookup(hostname, { all: true, verbatim: true });
  } catch (err) {
    // Resolution failure is NOT a pass — reject so a typo or non-existent
    // host fails here rather than leaking to the QURL API as an opaque error.
    throw new Error(`Target URL hostname could not be resolved: ${err.code || err.message}`);
  }
  for (const { address } of addrs) {
    if (isPrivateHost(address)) {
      throw new Error('Target URL points to a private/internal address');
    }
  }
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
    await assertNotPrivateAfterResolve(parsed.hostname);
  } catch (err) {
    if (/(http|private|resolved)/i.test(err.message)) throw err;
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
