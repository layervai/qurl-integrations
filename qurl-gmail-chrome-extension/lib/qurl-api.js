/**
 * QURL API Client
 *
 * Handles multipart file uploads to the QURL upload server.
 * Ported from the Gmail Apps Script add-on and Node.js test script.
 */

const QURLI18n = typeof globalThis !== 'undefined' && globalThis.QURLI18n
  ? globalThis.QURLI18n
  : (typeof module !== 'undefined' && module.exports ? require('./qurl-i18n.js') : null);

// ===================== Configuration =====================
// Default QURL server base URL used when no override is configured.
// Release builds may rewrite this constant from QURL_API_BASE in .env or the shell environment.
// Keep this declaration simple so scripts/build-release.js can rewrite it reliably.
// Note: The build rewriter adds a trailing slash, but normalizeQurlApiBase (called via
// resolveDefaultQurlApiConfig) strips it. This "add slash then strip slash" round-trip
// is intentional — the rewriter outputs a canonical form and normalization ensures
// consistent runtime behavior regardless of input formatting.
const DEFAULT_QURL_API_BASE = 'https://getqurllink.layerv.xyz/';
// Do not deduplicate this with DEFAULT_QURL_API_BASE. The build-time rewriter intentionally
// only patches the primary constant so this fallback remains a known-good bundled default.
const DEFAULT_QURL_API_BASE_FALLBACK = 'https://getqurllink.layerv.xyz/';
const QURL_API_BASE_STORAGE_KEY = 'qurlApiBase';
const UPLOAD_REQUEST_TIMEOUT_MS = 5 * 60 * 1000;
const DEFAULT_QURL_API_CONFIG = resolveDefaultQurlApiConfig(DEFAULT_QURL_API_BASE);
const DEFAULT_QURL_API_BASE_NORMALIZED = DEFAULT_QURL_API_CONFIG.normalized;
const DEFAULT_QURL_API_ORIGIN = DEFAULT_QURL_API_CONFIG.origin;

// ==================== Upload Logic ====================

/**
 * Uploads a file to the QURL server.
 *
 * @param {ArrayBuffer|Uint8Array} fileBuffer - Raw file bytes.
 * @param {string} filename - Original filename.
 * @param {string} contentType - MIME type string.
 * @returns {Promise<{success: boolean, resource_id: string|null, qurl_link: string|null,
 *                    resource_url: string|null, expires_at: string|null, error: string|null}>}
 */
async function uploadFile(fileBuffer, filename, contentType) {
  const baseUrl = await getQurlApiBase();
  const hasPermission = await ensureQurlHostPermission(baseUrl, false);
  if (!hasPermission) {
    return {
      success: false,
      resource_id: null,
      qurl_link: null,
      resource_url: null,
      expires_at: null,
      error: getMessage(
        'permission_missing_error',
        'Permission to access the configured QURL server is missing. Open settings and save the server URL again.'
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
 * Returns the effective QURL API base URL.
 * Uses the stored override when available, otherwise falls back to the default.
 *
 * @returns {Promise<string>}
 */
async function getQurlApiBase() {
  const stored = await getStoredQurlApiBase();
  return stored || DEFAULT_QURL_API_BASE_NORMALIZED;
}

/**
 * Reads the stored QURL API base URL override.
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
        console.warn('[QURL] Ignoring invalid stored QURL API base:', err.message);
        resolve(null);
      }
    });
  });
}

/**
 * Stores a QURL API base URL override. Empty values clear the override.
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

  if (normalized) {
    const granted = await ensureQurlHostPermission(normalized, !resolvedOptions.skipPermissionRequest);
    if (!granted) {
      throw new Error(getMessage(
        'permission_request_denied_error',
        'Permission to access this QURL server was not granted.'
      ));
    }
  }

  return new Promise(function (resolve, reject) {
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
}

/**
 * Ensures the extension has host permission for the given QURL server.
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
  // Strip control characters (NUL, CR, LF, etc.) to prevent header injection.
  // Mirrors the control-character stripping in _sanitizeFilename for consistency.
  const normalized = String(contentType || 'application/octet-stream').replace(/[\x00-\x1f]+/g, '');
  return normalized || 'application/octet-stream';
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

  // Empty and zero-like values are treated as "no expiry" for current QURL payloads.
  if (!raw) return null;

  if (typeof raw === 'number') {
    if (!Number.isFinite(raw)) {
      return null;
    }

    // Current Unix timestamps in seconds are ~1e9, while millisecond timestamps are ~1e12.
    const ms = raw > 1e12 ? raw : raw * 1000;
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
  if (globalThis.crypto && typeof globalThis.crypto.getRandomValues === 'function') {
    const bytes = new Uint8Array(16);
    globalThis.crypto.getRandomValues(bytes);
    return '----QurlBoundary' + Array.from(bytes, function (byte) {
      return byte.toString(16).padStart(2, '0');
    }).join('');
  }

  return '----QurlBoundary' + Math.random().toString(36).slice(2) + Date.now().toString(36);
}

/**
 * Normalizes a configured QURL API base URL.
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
      'QURL server URL must be a valid http(s) URL.'
    ));
  }

  if (parsed.protocol !== 'https:') {
    throw new Error(getMessage(
      'config_https_required_error',
      'QURL server URL must start with https://'
    ));
  }

  const pathname = parsed.pathname.replace(/\/+$/, '').replace(/\/api\/upload$/i, '');
  parsed.pathname = pathname || '/';
  parsed.search = '';
  parsed.hash = '';

  return parsed.toString().replace(/\/$/, '');
}

function resolveDefaultQurlApiConfig(baseUrl) {
  try {
    const normalized = normalizeQurlApiBase(baseUrl);
    return {
      normalized,
      origin: new URL(normalized).origin,
    };
  } catch (err) {
    const normalized = normalizeQurlApiBase(DEFAULT_QURL_API_BASE_FALLBACK);
    console.error('[QURL] Invalid bundled QURL API base URL, falling back to built-in default:', err.message);
    return {
      normalized,
      origin: new URL(normalized).origin,
    };
  }
}

/**
 * Builds the host permission pattern for the given QURL base URL.
 *
 * @param {string} baseUrl
 * @returns {string}
 */
function getQurlHostPermissionPattern(baseUrl) {
  const parsed = new URL(baseUrl);
  return `${parsed.origin}/*`;
}

/**
 * Checks whether the given base URL points to the built-in default QURL service.
 * Returns false for invalid URLs to fail closed.
 *
 * @param {string} baseUrl - A normalized QURL API base URL.
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
