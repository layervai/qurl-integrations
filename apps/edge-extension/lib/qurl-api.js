/**
 * qURL API Client
 *
 * Handles multipart file uploads to the qURL upload server.
 * Ported from the Gmail Apps Script add-on and Node.js test script.
 */

'use strict';

const QURLI18n = typeof globalThis !== 'undefined' && globalThis.QURLI18n
  ? globalThis.QURLI18n
  : (typeof module !== 'undefined' && module.exports ? require('./qurl-i18n.js') : null);

// Centralized config is the single source of truth for the default server base URL.
// Loaded before this script in popup.html and required directly in Node tests.
const QURLConfig = typeof globalThis !== 'undefined' && globalThis.QURLConfig
  ? globalThis.QURLConfig
  : (typeof module !== 'undefined' && module.exports ? require('./qurl-config.js') : null);

// ===================== Configuration =====================
// Default qURL server base URL used when no override is configured. The value lives in
// lib/qurl-config.js (build-time configurable via QURL_API_BASE); see that file. The
// trailing slash it carries is stripped by normalizeQurlApiBase via resolveDefaultQurlApiConfig.
const DEFAULT_QURL_API_BASE = QURLConfig ? QURLConfig.DEFAULT_QURL_API_BASE : null;
// Pre-flight (storage + permission lookups) must finish within this budget so a torn-down
// MV3 service worker that drops a Chrome callback can't hang the upload forever.
const UPLOAD_PREFLIGHT_TIMEOUT_MS = 10 * 1000;
const QURL_API_BASE_STORAGE_KEY = 'qurlApiBase';
const UPLOAD_REQUEST_TIMEOUT_MS = 5 * 60 * 1000;
const DEFAULT_QURL_API_CONFIG = resolveDefaultQurlApiConfig(DEFAULT_QURL_API_BASE);
const DEFAULT_QURL_API_BASE_NORMALIZED = DEFAULT_QURL_API_CONFIG.normalized;
const DEFAULT_QURL_API_ORIGIN = DEFAULT_QURL_API_CONFIG.origin;

// ==================== Upload Logic ====================

/**
 * Uploads a file to the qURL server.
 *
 * @param {ArrayBuffer|Uint8Array} fileBuffer - Raw file bytes.
 * @param {string} filename - Original filename.
 * @param {string} contentType - MIME type string.
 * @returns {Promise<{success: boolean, resource_id: string|null, qurl_link: string|null,
 *                    resource_url: string|null, expires_at: string|null, error: string|null}>}
 */
async function uploadFile(fileBuffer, filename, contentType) {
  let baseUrl;
  let hasPermission;
  try {
    // Bound the pre-flight: storage.local.get and permissions.contains are Promise-wrapped
    // Chrome callbacks with no native timeout. If the service worker is torn down mid-call
    // the callback can be dropped, which would otherwise hang the popup on "Uploading…"
    // forever (the fetch AbortController below is not armed until after this resolves).
    const preflight = (async function () {
      const resolvedBase = await getQurlApiBase();
      const granted = await ensureQurlHostPermission(resolvedBase, false);
      return { resolvedBase, granted };
    })();
    const result = await withTimeout(
      preflight,
      UPLOAD_PREFLIGHT_TIMEOUT_MS,
      getMessage('upload_preflight_timeout_error', 'Timed out preparing the upload. Please try again.')
    );
    baseUrl = result.resolvedBase;
    hasPermission = result.granted;
  } catch (err) {
    return {
      success: false,
      resource_id: null,
      qurl_link: null,
      resource_url: null,
      expires_at: null,
      error: err && err.message ? err.message : String(err),
    };
  }
  if (!hasPermission) {
    return {
      success: false,
      resource_id: null,
      qurl_link: null,
      resource_url: null,
      expires_at: null,
      error: getMessage(
        'permission_missing_error',
        'Permission to access the configured qURL server is missing. Open settings and save the server URL again.'
      ),
    };
  }
  // Use URL constructor to safely join paths and avoid double-slash issues
  // if baseUrl ever ends with a trailing slash.
  const apiUrl = new URL('api/upload', baseUrl + '/').toString();

  const boundary = createMultipartBoundary();

  const body = _buildMultipartBody(boundary, fileBuffer, filename, contentType);

  const headers = {
    'Content-Type': `multipart/form-data; boundary=${boundary}`,
  };

  const abortController = typeof AbortController === 'function' ? new AbortController() : null;
  const timeoutId = abortController
    ? setTimeout(function () {
      abortController.abort();
    }, UPLOAD_REQUEST_TIMEOUT_MS)
    : null;

  try {
    const response = await fetch(apiUrl, {
      method: 'POST',
      headers,
      body,
      signal: abortController ? abortController.signal : undefined,
    });

    const fallbackBodyReader = typeof response.clone === 'function'
      ? response.clone()
      : null;
    let data;
    try {
      data = await response.json();
    } catch {
      const text = fallbackBodyReader && typeof fallbackBodyReader.text === 'function'
        ? await fallbackBodyReader.text()
        : '';
      return {
        success: false,
        resource_id: null,
        qurl_link: null,
        resource_url: null,
        expires_at: null,
        error: getMessage(
          'api_invalid_json_error',
          'Invalid JSON response: $1',
          [text.substring(0, 200)]
        ),
      };
    }

    const payload = _extractPayload(data);
    const payloadObject = payload && typeof payload === 'object' ? payload : null;
    const dataObject = data && typeof data === 'object' ? data : null;

    if (!dataObject) {
      return {
        success: false,
        resource_id: null,
        qurl_link: null,
        resource_url: null,
        expires_at: null,
        error: getMessage(
          'api_invalid_payload_error',
          'Invalid API response payload.'
        ),
      };
    }

    if (!response.ok || dataObject.success === false) {
      return {
        success: false,
        resource_id: null,
        qurl_link: null,
        resource_url: null,
        expires_at: null,
        error: (payloadObject && payloadObject.error) || dataObject.error || `HTTP ${response.status}`,
      };
    }

    const resourceId = _get(payloadObject, 'resource_id', 'resourceId', 'id');
    // _get also coerces finite numbers to strings (useful for numeric resource_id). For the URL
    // fields that's harmless: a backend emitting a number would yield e.g. "123", which
    // normalizeAllowedLink rejects, so it surfaces as "no download link" rather than a bad link.
    const qurlLink = _get(payloadObject, 'qurl_link', 'qurlLink');
    const resourceUrl = _get(payloadObject, 'resource_url', 'resourceUrl');

    // Treat "success but no usable link" as an error so the popup does not report
    // empty results. At least one of qurl_link or resource_url must be present.
    if (!qurlLink && !resourceUrl) {
      return {
        success: false,
        resource_id: null,
        qurl_link: null,
        resource_url: null,
        expires_at: null,
        error: getMessage(
          'api_missing_link_error',
          'Server returned success but no download link was provided.'
        ),
      };
    }

    return {
      success: true,
      resource_id: resourceId,
      qurl_link: qurlLink,
      resource_url: resourceUrl,
      expires_at: _parseExpiry(payloadObject),
      error: null,
    };
  } catch (err) {
    if (err && err.name === 'AbortError') {
      return {
        success: false,
        resource_id: null,
        qurl_link: null,
        resource_url: null,
        expires_at: null,
        error: getMessage('upload_timeout_error', 'Upload timed out after 5 minutes.'),
      };
    }
    return {
      success: false,
      resource_id: null,
      qurl_link: null,
      resource_url: null,
      expires_at: null,
      error: err.message || String(err),
    };
  } finally {
    if (timeoutId !== null) {
      clearTimeout(timeoutId);
    }
  }
}

/**
 * Returns the effective qURL API base URL.
 * Uses the stored override when available, otherwise falls back to the default.
 *
 * @returns {Promise<string>}
 */
async function getQurlApiBase() {
  const stored = await getStoredQurlApiBase();
  return stored || DEFAULT_QURL_API_BASE_NORMALIZED;
}

/**
 * Reads the stored qURL API base URL override.
 *
 * @returns {Promise<string|null>}
 */
async function getStoredQurlApiBase() {
  if (typeof chrome === 'undefined' || !chrome.storage || !chrome.storage.local) {
    return null;
  }

  return new Promise(function (resolve) {
    chrome.storage.local.get([QURL_API_BASE_STORAGE_KEY], function (items) {
      if (chrome.runtime && chrome.runtime.lastError) {
        resolve(null);
        return;
      }
      try {
        resolve(normalizeQurlApiBase(items[QURL_API_BASE_STORAGE_KEY]));
      } catch (err) {
        console.warn('[qURL] Ignoring invalid stored qURL API base:', err.message);
        resolve(null);
      }
    });
  });
}

/**
 * Stores a qURL API base URL override. Empty values clear the override.
 *
 * @param {string} value
 * @returns {Promise<string|null>} Normalized stored value, or null if cleared.
 */
async function setStoredQurlApiBase(value, options) {
  if (typeof chrome === 'undefined' || !chrome.storage || !chrome.storage.local) {
    throw new Error('Chrome storage is not available.');
  }

  const normalized = normalizeQurlApiBase(value);
  const resolvedOptions = options || {};

  // Capture the previous override up front so its host permission can be revoked once we've
  // switched away from it — whether the user clears the override (custom → default) OR moves to
  // a different custom server (custom-A → custom-B). Least privilege: don't leave a granted
  // origin that the UI can no longer reach.
  const previousBase = await getStoredQurlApiBase();

  if (normalized) {
    const granted = await ensureQurlHostPermission(normalized, !resolvedOptions.skipPermissionRequest);
    if (!granted) {
      throw new Error(getMessage(
        'permission_request_denied_error',
        'Permission to access this qURL server was not granted.'
      ));
    }
  }

  const stored = await new Promise(function (resolve, reject) {
    if (!normalized) {
      chrome.storage.local.remove(QURL_API_BASE_STORAGE_KEY, function () {
        if (chrome.runtime && chrome.runtime.lastError) {
          reject(new Error(chrome.runtime.lastError.message));
          return;
        }
        resolve(null);
      });
      return;
    }

    const payload = {};
    payload[QURL_API_BASE_STORAGE_KEY] = normalized;
    chrome.storage.local.set(payload, function () {
      if (chrome.runtime && chrome.runtime.lastError) {
        reject(new Error(chrome.runtime.lastError.message));
        return;
      }
      resolve(normalized);
    });
  });

  // Only after the new value is persisted (so a failed grant/store leaves the old permission in
  // place) do we drop the previous custom origin if it is no longer the active base.
  await revokeStaleCustomOrigin(previousBase, normalized);
  return stored;
}

/**
 * Best-effort revocation of a previous custom origin's host permission once it is no longer the
 * active base URL. No-op for the bundled default and when the host is unchanged (custom-A →
 * custom-A, or a path-only change on the same host). Revocation failure is non-fatal.
 *
 * @param {string|null} previousBase
 * @param {string|null} normalizedBase
 * @returns {Promise<void>}
 */
function revokeStaleCustomOrigin(previousBase, normalizedBase) {
  if (!previousBase || isDefaultQurlOrigin(previousBase)) {
    return Promise.resolve();
  }

  const previousPattern = getQurlHostPermissionPattern(previousBase);
  if (normalizedBase && getQurlHostPermissionPattern(normalizedBase) === previousPattern) {
    // Same host still in use — keep the permission.
    return Promise.resolve();
  }

  if (typeof chrome === 'undefined' || !chrome.permissions || !chrome.permissions.remove) {
    return Promise.resolve();
  }

  return new Promise(function (res) {
    try {
      chrome.permissions.remove({ origins: [previousPattern] }, function () {
        void (chrome.runtime && chrome.runtime.lastError);
        res();
      });
    } catch (_err) {
      res();
    }
  });
}

/**
 * Ensures the extension has host permission for the given qURL server.
 *
 * @param {string} baseUrl
 * @param {boolean} requestIfMissing
 * @returns {Promise<boolean>}
 */
async function ensureQurlHostPermission(baseUrl, requestIfMissing) {
  const pattern = getQurlHostPermissionPattern(baseUrl);

  if (isDefaultQurlOrigin(baseUrl)) {
    return true;
  }

  if (typeof chrome === 'undefined' || !chrome.permissions) {
    return false;
  }

  const hasPermission = await new Promise(function (resolve) {
    chrome.permissions.contains({ origins: [pattern] }, function (result) {
      if (chrome.runtime && chrome.runtime.lastError) {
        resolve(false);
        return;
      }
      resolve(Boolean(result));
    });
  });

  if (hasPermission || !requestIfMissing) {
    return hasPermission;
  }

  return requestQurlHostPermission(baseUrl);
}

function requestQurlHostPermission(baseUrl) {
  if (isDefaultQurlOrigin(baseUrl)) {
    return Promise.resolve(true);
  }

  if (typeof chrome === 'undefined' || !chrome.permissions) {
    return Promise.resolve(false);
  }

  const pattern = getQurlHostPermissionPattern(baseUrl);
  return new Promise(function (resolve) {
    chrome.permissions.request({ origins: [pattern] }, function (granted) {
      if (chrome.runtime && chrome.runtime.lastError) {
        resolve(false);
        return;
      }
      resolve(Boolean(granted));
    });
  });
}

// ===================== Internal Helpers =====================

/**
 * Builds a multipart/form-data body from a file buffer.
 *
 * @param {string} boundary
 * @param {ArrayBuffer|Uint8Array} fileBuffer
 * @param {string} filename
 * @param {string} contentType
 * @returns {Blob}
 */
// Builds the multipart body with an explicit boundary rather than via FormData (which would
// let fetch set the boundary). The explicit form keeps the header sanitization above under our
// control and yields deterministic bytes the unit tests assert against.
function _buildMultipartBody(boundary, fileBuffer, filename, contentType) {
  const safeFilename = _sanitizeFilename(filename);
  const safeContentType = _sanitizeContentType(contentType);
  const encoder = new TextEncoder();
  const header = `--${boundary}\r\n`
    + `Content-Disposition: form-data; name="file"; filename="${safeFilename}"\r\n`
    + `Content-Type: ${safeContentType}\r\n\r\n`;
  const footer = `\r\n--${boundary}--\r\n`;

  const headerBytes = encoder.encode(header);
  let contentBytes;
  if (fileBuffer instanceof Uint8Array) {
    contentBytes = fileBuffer;
  } else {
    contentBytes = new Uint8Array(fileBuffer);
  }
  const footerBytes = encoder.encode(footer);

  return new Blob([headerBytes, contentBytes, footerBytes]);
}

function _sanitizeContentType(contentType) {
  // Strip the same control + line-separator classes as _sanitizeFilename so the two
  // multipart header fields have matching injection resistance (CR/LF/NUL plus the
  // Unicode line separators \u0085\u2028\u2029). Note: ';' and '"' are deliberately
  // NOT stripped — they are legitimate in MIME types (e.g. text/plain; charset="utf-8").
  const stripped = String(contentType || 'application/octet-stream')
    // eslint-disable-next-line no-control-regex -- intentionally strips control chars to block header injection
    .replace(/[\x00-\x1f\u0085\u2028\u2029]+/g, '')
    .trim();
  // Accept only a well-formed MIME type (type/subtype with optional parameters);
  // anything else falls back to the generic octet-stream rather than being emitted raw.
  if (!/^[!#$%&'*+.^_`|~0-9A-Za-z-]+\/[!#$%&'*+.^_`|~0-9A-Za-z-]+(\s*;.*)?$/.test(stripped)) {
    return 'application/octet-stream';
  }
  return stripped;
}

/**
 * Rejects a pending promise if it does not settle within the given budget.
 * Used to bound Chrome callback-backed promises that have no native timeout.
 *
 * @param {Promise<T>} promise
 * @param {number} timeoutMs
 * @param {string} timeoutMessage
 * @returns {Promise<T>}
 * @template T
 */
function withTimeout(promise, timeoutMs, timeoutMessage) {
  return new Promise(function (resolve, reject) {
    const timerId = setTimeout(function () {
      reject(new Error(timeoutMessage));
    }, timeoutMs);
    Promise.resolve(promise).then(
      function (value) {
        clearTimeout(timerId);
        resolve(value);
      },
      function (err) {
        clearTimeout(timerId);
        reject(err);
      }
    );
  });
}

/**
 * Extracts the payload from the API response.
 * Supports both flat {success, ...} and nested {success, data: {...}} formats.
 *
 * @param {Object} data
 * @returns {Object}
 */
function _extractPayload(data) {
  if (typeof data === 'object' && data !== null && typeof data.data === 'object' && data.data !== null) {
    return data.data;
  }
  return data;
}

/**
 * Returns the first non-empty string among multiple candidate keys.
 * Numeric values are coerced to strings to support backends that serialize
 * IDs as numbers (e.g., some Rails/Django configurations).
 * Booleans, null, undefined, and empty strings are ignored.
 *
 * @param {Object|null} obj
 * @param {...string} keys
 * @returns {string|null}
 */
function _get(obj, ...keys) {
  if (!obj || typeof obj !== 'object') {
    return null;
  }
  for (const key of keys) {
    const v = obj[key];
    if (typeof v === 'string' && v !== '') return v;
    if (typeof v === 'number' && Number.isFinite(v)) return String(v);
  }
  return null;
}

/**
 * Parses an expiry value from the API response payload.
 * Handles ISO strings, Unix timestamps (seconds), and milliseconds.
 *
 * @param {Object} payload
 * @returns {string|null}
 */
function _parseExpiry(payload) {
  if (!payload) return null;

  const raw = payload.expires_at
    || payload.expiresAt
    || payload.expiration
    || payload.expires;

  // Empty and zero-like values are treated as "no expiry" for current qURL payloads.
  if (!raw) return null;

  if (typeof raw === 'number') {
    if (!Number.isFinite(raw)) {
      return null;
    }

    // Current Unix timestamps in seconds are ~1e9, while millisecond timestamps are ~1e12.
    // Treat exactly 1e12 as milliseconds (year 2001 in ms vs year ~33658 in seconds).
    const ms = raw >= 1e12 ? raw : raw * 1000;
    return new Date(ms).toISOString();
  }

  if (typeof raw === 'string') {
    const trimmed = raw.trim();
    if (!trimmed) return null;

    const parsedMs = Date.parse(trimmed);
    if (Number.isNaN(parsedMs)) {
      return null;
    }

    return new Date(parsedMs).toISOString();
  }

  return null;
}

/**
 * Sanitizes a filename for safe inclusion in a Content-Disposition header.
 * Enforces a 255-byte UTF-8 cap so header values stay bounded without splitting
 * multi-byte characters mid-sequence.
 *
 * @param {string} name
 * @returns {string}
 */
function _sanitizeFilename(name) {
  if (!name) return 'unnamed';
  // eslint-disable-next-line no-control-regex -- intentionally strips control chars from the Content-Disposition filename
  const sanitized = String(name).replace(/[\x00-\x1f\u0085\u2028\u2029"\\]/g, '_');
  const encoder = new TextEncoder();
  let totalBytes = 0;
  let result = '';

  for (const char of sanitized) {
    const encoded = encoder.encode(char);
    if (totalBytes + encoded.length > 255) {
      break;
    }
    result += char;
    totalBytes += encoded.length;
  }

  return result || 'unnamed';
}

function createMultipartBoundary() {
  // Use the browser crypto API when available; the Node test runner falls back to Node's
  // crypto implementation so the extension code stays browser-safe without a test-only shim.
  const bytes = new Uint8Array(16);
  const randomBytes = getRandomBytes;
  randomBytes(bytes);
  return '----QurlBoundary' + Array.from(bytes, function (byte) {
    return byte.toString(16).padStart(2, '0');
  }).join('');
}

function getRandomBytes(bytes) {
  const browserCrypto = typeof globalThis !== 'undefined' ? globalThis.crypto : null;
  if (browserCrypto && typeof browserCrypto.getRandomValues === 'function') {
    return browserCrypto.getRandomValues(bytes);
  }

  if (typeof require === 'function') {
    try {
      const nodeCrypto = require('node:crypto');
      if (nodeCrypto.webcrypto && typeof nodeCrypto.webcrypto.getRandomValues === 'function') {
        return nodeCrypto.webcrypto.getRandomValues(bytes);
      }
      if (typeof nodeCrypto.randomFillSync === 'function') {
        return nodeCrypto.randomFillSync(bytes);
      }
    } catch (_err) {
      // Ignore and fall through to the error below.
    }
  }

  throw new Error('crypto.getRandomValues is not available.');
}

/**
 * Normalizes a configured qURL API base URL.
 *
 * @param {string} value
 * @returns {string|null}
 */
function normalizeQurlApiBase(value) {
  if (!value) return null;

  const trimmed = String(value).trim();
  if (!trimmed) return null;

  let parsed;
  try {
    parsed = new URL(trimmed);
  } catch (err) {
    throw new Error(getMessage(
      'config_invalid_url_error',
      'qURL server URL must be a valid http(s) URL.'
    ));
  }

  if (parsed.protocol !== 'https:') {
    throw new Error(getMessage(
      'config_https_required_error',
      'qURL server URL must start with https://'
    ));
  }

  const pathname = parsed.pathname.replace(/\/+$/, '').replace(/\/api\/upload$/i, '');
  parsed.pathname = pathname || '/';
  parsed.search = '';
  parsed.hash = '';
  // Drop any embedded credentials so they don't get persisted to chrome.storage.local or shown
  // back to the user; the upload sends no auth via the URL.
  parsed.username = '';
  parsed.password = '';

  return parsed.toString().replace(/\/$/, '');
}

function resolveDefaultQurlApiConfig(baseUrl) {
  // baseUrl comes from lib/qurl-config.js (the single source of truth). If it is
  // missing or malformed the extension cannot function, so fail loudly rather than
  // silently uploading somewhere unexpected.
  const normalized = normalizeQurlApiBase(baseUrl);
  if (!normalized) {
    throw new Error('Missing default qURL API base — lib/qurl-config.js did not load.');
  }
  return {
    normalized,
    origin: new URL(normalized).origin,
  };
}

/**
 * Builds the host permission pattern for the given qURL base URL.
 *
 * Chrome match patterns do not permit a port in the host, so a base URL with an
 * explicit port (e.g. https://host:8443) must yield a port-less pattern. Host match
 * patterns authorize any port on the host, which is the intended behavior here.
 *
 * @param {string} baseUrl
 * @returns {string}
 */
function getQurlHostPermissionPattern(baseUrl) {
  const parsed = new URL(baseUrl);
  return `${parsed.protocol}//${parsed.hostname}/*`;
}

/**
 * Checks whether the given base URL points to the built-in default qURL service.
 * Returns false for invalid URLs to fail closed.
 *
 * @param {string} baseUrl - A normalized qURL API base URL.
 * @returns {boolean}
 */
function isDefaultQurlOrigin(baseUrl) {
  try {
    return new URL(baseUrl).origin === DEFAULT_QURL_API_ORIGIN;
  } catch (_err) {
    return false;
  }
}

function getMessage(key, fallback, substitutions) {
  if (QURLI18n && typeof QURLI18n.getMessage === 'function') {
    return QURLI18n.getMessage(key, fallback, substitutions);
  }
  return fallback || '';
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    DEFAULT_QURL_API_BASE,
    UPLOAD_REQUEST_TIMEOUT_MS,
    UPLOAD_PREFLIGHT_TIMEOUT_MS,
    withTimeout,
    _buildMultipartBody,
    _extractPayload,
    _get,
    _parseExpiry,
    _sanitizeContentType,
    _sanitizeFilename,
    createMultipartBoundary,
    ensureQurlHostPermission,
    getQurlApiBase,
    getQurlHostPermissionPattern,
    getStoredQurlApiBase,
    getMessage,
    isDefaultQurlOrigin,
    normalizeQurlApiBase,
    requestQurlHostPermission,
    resolveDefaultQurlApiConfig,
    setStoredQurlApiBase,
    uploadFile,
  };
}
