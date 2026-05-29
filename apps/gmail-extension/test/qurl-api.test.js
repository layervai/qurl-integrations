const test = require('node:test');
const assert = require('node:assert/strict');

const qurlApi = require('../lib/qurl-api.js');

const originalFetch = global.fetch;
const originalChrome = global.chrome;
const originalSetTimeout = global.setTimeout;
const originalClearTimeout = global.clearTimeout;

test.beforeEach(function () {
  global.fetch = originalFetch;
  global.chrome = originalChrome;
  global.setTimeout = originalSetTimeout;
  global.clearTimeout = originalClearTimeout;
});

test.afterEach(function () {
  global.fetch = originalFetch;
  global.chrome = originalChrome;
  global.setTimeout = originalSetTimeout;
  global.clearTimeout = originalClearTimeout;
});

test('normalizeQurlApiBase strips /api/upload and requires https', function () {
  assert.equal(
    qurlApi.normalizeQurlApiBase('https://example.com/api/upload?foo=bar#frag'),
    'https://example.com'
  );

  assert.equal(
    qurlApi.normalizeQurlApiBase('https://example.com/custom/path/'),
    'https://example.com/custom/path'
  );

  assert.throws(function () {
    qurlApi.normalizeQurlApiBase('http://example.com');
  }, /https:\/\//i);

  assert.equal(qurlApi.normalizeQurlApiBase(null), null);
  assert.equal(qurlApi.normalizeQurlApiBase(undefined), null);
  assert.equal(qurlApi.normalizeQurlApiBase('   '), null);

  assert.throws(function () {
    qurlApi.normalizeQurlApiBase('not-a-url');
  }, /valid http\(s\) URL/i);
});

test('normalizeQurlApiBase uses localized validation messages when available', function () {
  global.chrome = {
    i18n: {
      getMessage(key) {
        if (key === 'config_invalid_url_error') {
          return 'Localized invalid URL';
        }
        if (key === 'config_https_required_error') {
          return 'Localized HTTPS required';
        }
        return '';
      },
    },
  };

  assert.throws(function () {
    qurlApi.normalizeQurlApiBase('not-a-url');
  }, /Localized invalid URL/);

  assert.throws(function () {
    qurlApi.normalizeQurlApiBase('http://example.com');
  }, /Localized HTTPS required/);
});

test('_sanitizeFilename removes header-breaking characters and control bytes', function () {
  assert.equal(
    qurlApi._sanitizeFilename('bad"\0\\\r\n\u2028name.txt'),
    'bad______name.txt'
  );
});

test('_sanitizeFilename truncates on UTF-8 byte boundaries without splitting emoji', function () {
  const filename = 'a'.repeat(252) + '😀.txt';
  const sanitized = qurlApi._sanitizeFilename(filename);

  assert.equal(new TextEncoder().encode(sanitized).length <= 255, true);
  assert.equal(sanitized.includes('😀'), false);
  assert.equal(sanitized, 'a'.repeat(252));
});

test('_buildMultipartBody produces a valid multipart payload', async function () {
  const body = qurlApi._buildMultipartBody(
    'BOUNDARY',
    new Uint8Array([65, 66, 67]),
    'report.txt',
    'text/plain'
  );

  const text = await body.text();

  assert.match(text, /--BOUNDARY/);
  assert.match(text, /Content-Disposition: form-data; name="file"; filename="report.txt"/);
  assert.match(text, /Content-Type: text\/plain/);
  assert.match(text, /ABC/);
  assert.match(text, /--BOUNDARY--/);
});

test('_buildMultipartBody rejects synthetic content types that try to inject headers', async function () {
  const body = qurlApi._buildMultipartBody(
    'BOUNDARY',
    new Uint8Array([65]),
    'report.txt',
    'text/plain\r\nX-Injected: yes'
  );

  const text = await body.text();

  // The line-break is stripped and the result is no longer a valid MIME token, so the
  // sanitizer falls back to the generic type rather than emitting attacker-controlled text.
  assert.match(text, /Content-Type: application\/octet-stream/);
  assert.doesNotMatch(text, /X-Injected: yes/);
});

test('_sanitizeContentType preserves valid parameterized MIME types', function () {
  assert.equal(qurlApi._sanitizeContentType('text/plain; charset="utf-8"'), 'text/plain; charset="utf-8"');
  assert.equal(qurlApi._sanitizeContentType('image/png'), 'image/png');
  // A stray token (space, not a valid MIME separator) fails the grammar → generic fallback.
  assert.equal(qurlApi._sanitizeContentType('text/plain evil'), 'application/octet-stream');
  assert.equal(qurlApi._sanitizeContentType(''), 'application/octet-stream');
});

test('_buildMultipartBody accepts ArrayBuffer input without changing the payload', async function () {
  const body = qurlApi._buildMultipartBody(
    'BOUNDARY',
    new Uint8Array([65, 66, 67]).buffer,
    'report.txt',
    'text/plain'
  );

  const text = await body.text();

  assert.match(text, /ABC/);
});

test('_extractPayload and _parseExpiry support wrapped and timestamp values', function () {
  assert.deepEqual(
    qurlApi._extractPayload({ success: true, data: { qurl_link: 'https://example.com/q/1' } }),
    { qurl_link: 'https://example.com/q/1' }
  );

  assert.equal(
    qurlApi._parseExpiry({ expires_at: 1710000000 }),
    new Date(1710000000 * 1000).toISOString()
  );

  assert.equal(
    qurlApi._parseExpiry({ expires_at: '2026-05-01T12:00:00Z' }),
    '2026-05-01T12:00:00.000Z'
  );
  assert.equal(
    qurlApi._parseExpiry({ expires_at: ' 2026-05-01T12:00:00Z  ' }),
    '2026-05-01T12:00:00.000Z'
  );
  assert.equal(qurlApi._parseExpiry({ expires_at: 'never' }), null);
  assert.equal(qurlApi._parseExpiry({ expires_at: Number.POSITIVE_INFINITY }), null);
});

test('_get returns the first populated candidate key', function () {
  // Empty strings are skipped, but finite numbers are coerced to strings
  // to support backends that serialize IDs as numbers.
  assert.equal(qurlApi._get({ a: '', b: 123, c: 'ok' }, 'a', 'b', 'c'), '123');
  assert.equal(qurlApi._get({ a: '', b: 0, c: 'ok' }, 'a', 'b', 'c'), '0');
  assert.equal(qurlApi._get({ a: false, b: '0' }, 'a', 'b'), '0');
  assert.equal(qurlApi._get({ a: null, b: undefined }, 'a', 'b'), null);
  // NaN and Infinity are not valid IDs
  assert.equal(qurlApi._get({ a: NaN, b: 'ok' }, 'a', 'b'), 'ok');
  assert.equal(qurlApi._get({ a: Infinity, b: 'ok' }, 'a', 'b'), 'ok');
});

test('resolveDefaultQurlApiConfig normalizes the centralized default and throws when it is malformed', function () {
  assert.deepEqual(qurlApi.resolveDefaultQurlApiConfig('https://getqurllink.layerv.ai/'), {
    normalized: 'https://getqurllink.layerv.ai',
    origin: 'https://getqurllink.layerv.ai',
  });

  // The default is the single source of truth (lib/qurl-config.js); a malformed value is a
  // build error, so fail loudly rather than silently uploading to an unexpected host.
  assert.throws(function () {
    qurlApi.resolveDefaultQurlApiConfig('http://bad.example.com');
  }, /https:\/\//i);
});

test('getQurlHostPermissionPattern drops the port and isDefaultQurlOrigin uses normalized origins', function () {
  assert.equal(
    qurlApi.getQurlHostPermissionPattern('https://example.com/custom/path'),
    'https://example.com/*'
  );

  // Chrome match patterns reject ports, so a ported base must yield a port-less pattern.
  assert.equal(
    qurlApi.getQurlHostPermissionPattern('https://self.hosted.example:8443/base'),
    'https://self.hosted.example/*'
  );

  assert.equal(qurlApi.isDefaultQurlOrigin('https://getqurllink.layerv.ai/custom/path'), true);
  assert.equal(qurlApi.isDefaultQurlOrigin('https://getqurllink.layerv.xyz'), false);
  assert.equal(qurlApi.isDefaultQurlOrigin('https://example.com'), false);
});

test('ensureQurlHostPermission skips chrome.permissions for the bundled default origin', async function () {
  let containsCalled = false;
  global.chrome = {
    runtime: { lastError: null },
    permissions: {
      contains(_details, callback) {
        containsCalled = true;
        callback(true);
      },
    },
  };

  const granted = await qurlApi.ensureQurlHostPermission('https://getqurllink.layerv.ai', false);

  assert.equal(granted, true);
  assert.equal(containsCalled, false);
});

test('createMultipartBoundary uses the expected prefix and enough entropy characters', function () {
  const boundary = qurlApi.createMultipartBoundary();
  assert.match(boundary, /^----QurlBoundary[a-f0-9]{32,}$/);
});

test('getQurlApiBase prefers the stored override over the bundled default', async function () {
  global.chrome = {
    runtime: { lastError: null },
    storage: {
      local: {
        get(_keys, callback) {
          callback({ qurlApiBase: 'https://custom.example.com/base' });
        },
      },
    },
  };

  assert.equal(await qurlApi.getQurlApiBase(), 'https://custom.example.com/base');
});

test('getStoredQurlApiBase ignores malformed stored values', async function () {
  const originalConsoleWarn = console.warn;
  console.warn = function () {};
  global.chrome = {
    runtime: { lastError: null },
    storage: {
      local: {
        get(_keys, callback) {
          callback({ qurlApiBase: 'http://example.com' });
        },
      },
    },
  };

  try {
    await assert.doesNotReject(async function () {
      assert.equal(await qurlApi.getStoredQurlApiBase(), null);
    });
  } finally {
    console.warn = originalConsoleWarn;
  }
});

test('uploadFile returns parsed success payload', async function () {
  global.chrome = undefined;
  global.fetch = async function (url, options) {
    assert.equal(url, 'https://getqurllink.layerv.ai/api/upload');
    assert.equal(options.method, 'POST');
    assert.match(options.headers['Content-Type'], /^multipart\/form-data; boundary=/);

    return {
      ok: true,
      async json() {
        return {
          success: true,
          data: {
            resource_id: 'abc123',
            qurl_link: 'https://files.example.com/q/abc123',
            expires_at: '2026-05-01T12:00:00Z',
          },
        };
      },
    };
  };

  const result = await qurlApi.uploadFile(new Uint8Array([1, 2, 3]), 'demo.txt', 'text/plain');

  assert.deepEqual(result, {
    success: true,
    resource_id: 'abc123',
    qurl_link: 'https://files.example.com/q/abc123',
    resource_url: null,
    expires_at: '2026-05-01T12:00:00.000Z',
    error: null,
  });
});

test('uploadFile preserves a custom base path when building the upload endpoint', async function () {
  global.chrome = {
    runtime: { lastError: null },
    storage: {
      local: {
        get(_keys, callback) {
          callback({ qurlApiBase: 'https://custom.example.com/custom/path' });
        },
      },
    },
    permissions: {
      contains(_details, callback) {
        callback(true);
      },
    },
  };
  global.fetch = async function (url) {
    assert.equal(url, 'https://custom.example.com/custom/path/api/upload');
    return {
      ok: true,
      async json() {
        return {
          success: true,
          data: {
            resource_id: 'custom123',
            qurl_link: 'https://files.example.com/q/custom123',
          },
        };
      },
    };
  };

  const result = await qurlApi.uploadFile(new Uint8Array([1, 2, 3]), 'demo.txt', 'text/plain');

  assert.equal(result.success, true);
  assert.equal(result.resource_id, 'custom123');
});

test('uploadFile returns a permission error when a custom QURL origin is no longer granted', async function () {
  let fetchCalled = false;
  global.fetch = async function () {
    fetchCalled = true;
    throw new Error('fetch should not be called without host permission');
  };
  global.chrome = {
    runtime: { lastError: null },
    storage: {
      local: {
        get(_keys, callback) {
          callback({ qurlApiBase: 'https://custom.example.com/api/upload' });
        },
      },
    },
    permissions: {
      contains(_details, callback) {
        callback(false);
      },
    },
  };

  const result = await qurlApi.uploadFile(new Uint8Array([1, 2, 3]), 'demo.txt', 'text/plain');

  assert.equal(fetchCalled, false);
  assert.deepEqual(result, {
    success: false,
    resource_id: null,
    qurl_link: null,
    resource_url: null,
    expires_at: null,
    error: 'Permission to access the configured qURL server is missing. Open settings and save the server URL again.',
  });
});

test('uploadFile fails closed for custom origins when the permissions API is unavailable', async function () {
  let fetchCalled = false;
  global.fetch = async function () {
    fetchCalled = true;
    throw new Error('fetch should not run without host permission support');
  };
  global.chrome = {
    runtime: { lastError: null },
    storage: {
      local: {
        get(_keys, callback) {
          callback({ qurlApiBase: 'https://custom.example.com/api/upload' });
        },
      },
    },
  };

  const result = await qurlApi.uploadFile(new Uint8Array([1, 2, 3]), 'demo.txt', 'text/plain');

  assert.equal(fetchCalled, false);
  assert.equal(result.success, false);
  assert.equal(result.error, 'Permission to access the configured qURL server is missing. Open settings and save the server URL again.');
});

test('uploadFile surfaces API error payloads on non-OK responses', async function () {
  global.chrome = undefined;
  global.fetch = async function () {
    return {
      ok: false,
      status: 500,
      async json() {
        return {
          success: false,
          error: 'server exploded',
        };
      },
    };
  };

  const result = await qurlApi.uploadFile(new Uint8Array([1]), 'demo.txt', 'text/plain');

  assert.deepEqual(result, {
    success: false,
    resource_id: null,
    qurl_link: null,
    resource_url: null,
    expires_at: null,
    error: 'server exploded',
  });
});

test('uploadFile reports invalid JSON responses without rereading the same body stream', async function () {
  global.chrome = undefined;
  global.fetch = async function () {
    return new Response('<html>oops</html>', {
      status: 200,
      headers: {
        'Content-Type': 'application/json',
      },
    });
  };

  const result = await qurlApi.uploadFile(new Uint8Array([1]), 'demo.txt', 'text/plain');

  assert.equal(result.success, false);
  assert.match(result.error, /^Invalid JSON response: <html>oops<\/html>/);
});

test('uploadFile localizes invalid JSON and payload errors when messages are available', async function () {
  global.chrome = {
    i18n: {
      getMessage(key, substitutions) {
        if (key === 'api_invalid_json_error') {
          return `Localized invalid JSON: ${substitutions[0]}`;
        }
        if (key === 'api_invalid_payload_error') {
          return 'Localized invalid payload';
        }
        return '';
      },
    },
  };
  global.fetch = async function () {
    return new Response('<html>oops</html>', {
      status: 200,
      headers: {
        'Content-Type': 'application/json',
      },
    });
  };

  const invalidJson = await qurlApi.uploadFile(new Uint8Array([1]), 'demo.txt', 'text/plain');
  assert.equal(invalidJson.error, 'Localized invalid JSON: <html>oops</html>');

  global.fetch = async function () {
    return {
      ok: true,
      async json() {
        return null;
      },
    };
  };

  const invalidPayload = await qurlApi.uploadFile(new Uint8Array([1]), 'demo.txt', 'text/plain');
  assert.equal(invalidPayload.error, 'Localized invalid payload');
});

test('uploadFile aborts when the request timeout elapses', async function () {
  global.chrome = undefined;
  // Fire only the upload-abort timer; leave the pre-flight timeout pending so the pre-flight
  // resolves normally (it completes in ms in production). Firing every timer immediately would
  // trip the pre-flight guard instead of exercising the fetch-abort path under test.
  global.setTimeout = function (callback, delay) {
    if (delay === qurlApi.UPLOAD_REQUEST_TIMEOUT_MS) {
      callback();
    }
    return 1;
  };
  global.clearTimeout = function () {};
  global.fetch = async function (_url, options) {
    if (!(options.signal && options.signal.aborted)) {
      throw new Error('request should have been aborted');
    }
    const abortError = new Error('This operation was aborted');
    abortError.name = 'AbortError';
    throw abortError;
  };

  const result = await qurlApi.uploadFile(new Uint8Array([1]), 'demo.txt', 'text/plain');

  assert.equal(result.success, false);
  assert.equal(result.error, 'Upload timed out after 5 minutes.');
});

test('uploadFile rejects null JSON payloads as invalid API responses', async function () {
  global.chrome = undefined;
  global.fetch = async function () {
    return {
      ok: true,
      async json() {
        return null;
      },
    };
  };

  const result = await qurlApi.uploadFile(new Uint8Array([1]), 'demo.txt', 'text/plain');

  assert.deepEqual(result, {
    success: false,
    resource_id: null,
    qurl_link: null,
    resource_url: null,
    expires_at: null,
    error: 'Invalid API response payload.',
  });
});

test('setStoredQurlApiBase surfaces a permission denial before saving', async function () {
  let setCalled = false;
  global.chrome = {
    runtime: { lastError: null },
    storage: {
      local: {
        get(_keys, callback) {
          callback({});
        },
        set() {
          setCalled = true;
        },
      },
    },
    permissions: {
      contains(_details, callback) {
        callback(false);
      },
      request(_details, callback) {
        callback(false);
      },
    },
  };

  await assert.rejects(
    qurlApi.setStoredQurlApiBase('https://custom.example.com'),
    /Permission to access this qURL server was not granted\./
  );
  assert.equal(setCalled, false);
});

test('setStoredQurlApiBase skips a second permission prompt when the caller already secured access', async function () {
  let requestCalled = false;
  let setCalled = false;
  global.chrome = {
    runtime: { lastError: null },
    storage: {
      local: {
        get(_keys, callback) {
          callback({});
        },
        set(_payload, callback) {
          setCalled = true;
          callback();
        },
      },
    },
    permissions: {
      contains(_details, callback) {
        callback(true);
      },
      request(_details, callback) {
        requestCalled = true;
        callback(true);
      },
    },
  };

  const saved = await qurlApi.setStoredQurlApiBase('https://custom.example.com', {
    skipPermissionRequest: true,
  });

  assert.equal(saved, 'https://custom.example.com');
  assert.equal(setCalled, true);
  assert.equal(requestCalled, false);
});

test('setStoredQurlApiBase clears the override without requesting host permission', async function () {
  let removeCalled = false;
  let permissionRevoked = false;
  global.chrome = {
    runtime: { lastError: null },
    storage: {
      local: {
        get(key, callback) {
          // Simulate a previously-stored custom origin
          callback({ qurlApiBase: 'https://custom.example.com' });
        },
        remove(key, callback) {
          removeCalled = key === 'qurlApiBase';
          callback();
        },
      },
    },
    permissions: {
      remove(opts, callback) {
        permissionRevoked = opts.origins && opts.origins[0] === 'https://custom.example.com/*';
        callback(true);
      },
    },
  };

  const cleared = await qurlApi.setStoredQurlApiBase('');

  assert.equal(cleared, null);
  assert.equal(removeCalled, true);
  assert.equal(permissionRevoked, true);
});

test('setStoredQurlApiBase revokes the previous custom origin when switching custom-A -> custom-B', async function () {
  const revokedOrigins = [];
  global.chrome = {
    runtime: { lastError: null },
    storage: {
      local: {
        get(_keys, callback) {
          callback({ qurlApiBase: 'https://custom-a.example.com' });
        },
        set(_payload, callback) {
          callback();
        },
      },
    },
    permissions: {
      contains(_details, callback) {
        callback(true);
      },
      request(_details, callback) {
        callback(true);
      },
      remove(opts, callback) {
        revokedOrigins.push(opts.origins[0]);
        callback(true);
      },
    },
  };

  const saved = await qurlApi.setStoredQurlApiBase('https://custom-b.example.com');

  assert.equal(saved, 'https://custom-b.example.com');
  // The no-longer-reachable custom-A origin is revoked; custom-B is not.
  assert.deepEqual(revokedOrigins, ['https://custom-a.example.com/*']);
});

test('setStoredQurlApiBase does not revoke when re-saving the same custom origin', async function () {
  let revokeCalled = false;
  global.chrome = {
    runtime: { lastError: null },
    storage: {
      local: {
        get(_keys, callback) {
          callback({ qurlApiBase: 'https://custom-a.example.com' });
        },
        set(_payload, callback) {
          callback();
        },
      },
    },
    permissions: {
      contains(_details, callback) {
        callback(true);
      },
      request(_details, callback) {
        callback(true);
      },
      remove(_opts, callback) {
        revokeCalled = true;
        callback(true);
      },
    },
  };

  // Same host, only a path change — the host permission is still needed, so no revoke.
  const saved = await qurlApi.setStoredQurlApiBase('https://custom-a.example.com/team');

  assert.equal(saved, 'https://custom-a.example.com/team');
  assert.equal(revokeCalled, false);
});

test('uploadFile reports network failures', async function () {
  global.chrome = undefined;
  global.fetch = async function () {
    throw new Error('network down');
  };

  const result = await qurlApi.uploadFile(new Uint8Array([1]), 'demo.txt', 'text/plain');

  assert.deepEqual(result, {
    success: false,
    resource_id: null,
    qurl_link: null,
    resource_url: null,
    expires_at: null,
    error: 'network down',
  });
});
