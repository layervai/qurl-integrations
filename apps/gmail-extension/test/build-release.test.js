const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('fs');
const os = require('os');
const path = require('path');

const buildRelease = require('../scripts/build-release.js');

function makeTempReleaseRoot() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'qurl-release-test-'));
}

test('rewriteDefaultApiBase tolerates minor formatting differences in qurl-api.js', function () {
  const releaseRoot = makeTempReleaseRoot();

  try {
    fs.mkdirSync(path.join(releaseRoot, 'lib'), { recursive: true });
    fs.writeFileSync(
      path.join(releaseRoot, 'lib', 'qurl-api.js'),
      [
        '/** test fixture */',
        'const DEFAULT_QURL_API_BASE  =  "https://getqurllink.layerv.xyz/" ;',
        'const OTHER_VALUE = true;',
        '',
      ].join('\n')
    );

    buildRelease.rewriteDefaultApiBase('https://custom.example.com/base', releaseRoot);

    const updated = fs.readFileSync(path.join(releaseRoot, 'lib', 'qurl-api.js'), 'utf8');
    assert.match(updated, /const DEFAULT_QURL_API_BASE = "https:\/\/custom\.example\.com\/base\/";/);
  } finally {
    fs.rmSync(releaseRoot, { recursive: true, force: true });
  }
});

test('rewriteDefaultApiBase preserves literal dollar signs in the replacement URL', function () {
  const releaseRoot = makeTempReleaseRoot();

  try {
    fs.mkdirSync(path.join(releaseRoot, 'lib'), { recursive: true });
    fs.writeFileSync(
      path.join(releaseRoot, 'lib', 'qurl-api.js'),
      [
        "const DEFAULT_QURL_API_BASE = 'https://getqurllink.layerv.xyz/';",
        "const DEFAULT_QURL_API_BASE_FALLBACK = 'https://getqurllink.layerv.xyz/';",
        '',
      ].join('\n')
    );

    buildRelease.rewriteDefaultApiBase('https://custom.example.com/path/$1', releaseRoot);

    const updated = fs.readFileSync(path.join(releaseRoot, 'lib', 'qurl-api.js'), 'utf8');
    assert.match(updated, /const DEFAULT_QURL_API_BASE = "https:\/\/custom\.example\.com\/path\/\$1\/";/);
  } finally {
    fs.rmSync(releaseRoot, { recursive: true, force: true });
  }
});

test('rewriteDefaultApiBase safely quotes replacement URLs that contain apostrophes', function () {
  const releaseRoot = makeTempReleaseRoot();

  try {
    fs.mkdirSync(path.join(releaseRoot, 'lib'), { recursive: true });
    fs.writeFileSync(
      path.join(releaseRoot, 'lib', 'qurl-api.js'),
      [
        "const DEFAULT_QURL_API_BASE = 'https://getqurllink.layerv.xyz/';",
        "const DEFAULT_QURL_API_BASE_FALLBACK = 'https://getqurllink.layerv.xyz/';",
        '',
      ].join('\n')
    );

    buildRelease.rewriteDefaultApiBase("https://custom.example.com/o'connor", releaseRoot);

    const updated = fs.readFileSync(path.join(releaseRoot, 'lib', 'qurl-api.js'), 'utf8');
    assert.match(updated, /const DEFAULT_QURL_API_BASE = "https:\/\/custom\.example\.com\/o'connor\/";/);
  } finally {
    fs.rmSync(releaseRoot, { recursive: true, force: true });
  }
});

test('rewriteDefaultApiBase is a no-op when the override matches the bundled default', function () {
  const releaseRoot = makeTempReleaseRoot();

  try {
    fs.mkdirSync(path.join(releaseRoot, 'lib'), { recursive: true });
    const fixture = "const DEFAULT_QURL_API_BASE = 'https://getqurllink.layerv.xyz/';\n";
    fs.writeFileSync(path.join(releaseRoot, 'lib', 'qurl-api.js'), fixture);

    assert.doesNotThrow(function () {
      buildRelease.rewriteDefaultApiBase('https://getqurllink.layerv.xyz', releaseRoot);
    });

    const updated = fs.readFileSync(path.join(releaseRoot, 'lib', 'qurl-api.js'), 'utf8');
    assert.equal(updated, fixture);
  } finally {
    fs.rmSync(releaseRoot, { recursive: true, force: true });
  }
});

test('applyBuildOverrides rewrites both the default API base and release host permission', function () {
  const releaseRoot = makeTempReleaseRoot();
  const originalLog = console.log;
  console.log = function () {};

  try {
    fs.mkdirSync(path.join(releaseRoot, 'lib'), { recursive: true });
    fs.writeFileSync(
      path.join(releaseRoot, 'lib', 'qurl-api.js'),
      [
        "const DEFAULT_QURL_API_BASE = 'https://getqurllink.layerv.xyz/';",
        "const DEFAULT_QURL_API_BASE_FALLBACK = 'https://getqurllink.layerv.xyz/';",
        '',
      ].join('\n')
    );
    fs.writeFileSync(
      path.join(releaseRoot, 'manifest.json'),
      JSON.stringify({
        host_permissions: [
          'https://mail.google.com/*',
          'https://getqurllink.layerv.xyz/*',
        ],
      }, null, 2)
    );

    buildRelease.applyBuildOverrides({
      qurlApiBase: 'https://custom.example.com/api/upload',
    }, releaseRoot);

    const apiClient = fs.readFileSync(path.join(releaseRoot, 'lib', 'qurl-api.js'), 'utf8');
    const manifest = JSON.parse(fs.readFileSync(path.join(releaseRoot, 'manifest.json'), 'utf8'));

    assert.match(apiClient, /const DEFAULT_QURL_API_BASE = "https:\/\/custom\.example\.com\/";/);
    assert.deepEqual(manifest.host_permissions, [
      'https://mail.google.com/*',
      'https://custom.example.com/*',
    ]);
  } finally {
    console.log = originalLog;
    fs.rmSync(releaseRoot, { recursive: true, force: true });
  }
});

test('applyBuildOverrides keeps the release bundle self-consistent end to end', function () {
  const releaseRoot = makeTempReleaseRoot();
  const originalLog = console.log;
  console.log = function () {};

  try {
    fs.mkdirSync(path.join(releaseRoot, 'lib'), { recursive: true });
    fs.mkdirSync(path.join(releaseRoot, '_locales', 'en'), { recursive: true });
    fs.mkdirSync(path.join(releaseRoot, 'popup'), { recursive: true });
    fs.writeFileSync(path.join(releaseRoot, '_locales', 'en', 'messages.json'), '{}\n');
    fs.writeFileSync(path.join(releaseRoot, 'popup', 'popup.html'), '');
    fs.writeFileSync(
      path.join(releaseRoot, 'lib', 'qurl-i18n.js'),
      'module.exports = { getMessage: function (_key, fallback) { return fallback || ""; } };\n'
    );
    fs.writeFileSync(
      path.join(releaseRoot, 'lib', 'qurl-api.js'),
      [
        "const DEFAULT_QURL_API_BASE = 'https://getqurllink.layerv.xyz/';",
        "const DEFAULT_QURL_API_BASE_FALLBACK = 'https://getqurllink.layerv.xyz/';",
        '',
      ].join('\n')
    );
    fs.writeFileSync(
      path.join(releaseRoot, 'manifest.json'),
      JSON.stringify({
        manifest_version: 3,
        action: { default_popup: 'popup/popup.html' },
        host_permissions: [
          'https://mail.google.com/*',
          'https://getqurllink.layerv.xyz/*',
        ],
      }, null, 2)
    );

    buildRelease.applyBuildOverrides({
      qurlApiBase: 'https://custom.example.com/base/api/upload',
    }, releaseRoot);

    assert.doesNotThrow(function () {
      buildRelease.validateReleaseManifest(releaseRoot);
    });

    delete require.cache[require.resolve(path.join(releaseRoot, 'lib', 'qurl-api.js'))];
    assert.doesNotThrow(function () {
      require(path.join(releaseRoot, 'lib', 'qurl-api.js'));
    });
  } finally {
    console.log = originalLog;
    fs.rmSync(releaseRoot, { recursive: true, force: true });
  }
});

test('rewriteManifestHostPermission rewrites the existing bundled host entry from the manifest itself', function () {
  const releaseRoot = makeTempReleaseRoot();

  try {
    // The function now reads the bundled default from the project source to determine
    // which host_permission entry to replace. Use the actual bundled default pattern.
    const bundledDefaultPattern = 'https://getqurllink.layerv.xyz/*';
    fs.writeFileSync(
      path.join(releaseRoot, 'manifest.json'),
      JSON.stringify({
        host_permissions: [
          'https://mail.google.com/*',
          bundledDefaultPattern,
        ],
      }, null, 2)
    );

    buildRelease.rewriteManifestHostPermission('https://custom.example.com/base', releaseRoot);

    const manifest = JSON.parse(fs.readFileSync(path.join(releaseRoot, 'manifest.json'), 'utf8'));
    assert.deepEqual(manifest.host_permissions, [
      'https://mail.google.com/*',
      'https://custom.example.com/*',
    ]);
  } finally {
    fs.rmSync(releaseRoot, { recursive: true, force: true });
  }
});

test('validateReleaseManifest fails when localized messages are missing', function () {
  const releaseRoot = makeTempReleaseRoot();

  try {
    fs.writeFileSync(
      path.join(releaseRoot, 'manifest.json'),
      JSON.stringify({
        manifest_version: 3,
        action: { default_popup: 'popup/popup.html' },
      }, null, 2)
    );

    assert.throws(function () {
      buildRelease.validateReleaseManifest(releaseRoot);
    }, /_locales\/en\/messages\.json/);
  } finally {
    fs.rmSync(releaseRoot, { recursive: true, force: true });
  }
});

test('validateReleaseManifest fails when the bundled host permission drifts from DEFAULT_QURL_API_BASE', function () {
  const releaseRoot = makeTempReleaseRoot();

  try {
    fs.mkdirSync(path.join(releaseRoot, '_locales', 'en'), { recursive: true });
    fs.mkdirSync(path.join(releaseRoot, 'lib'), { recursive: true });
    fs.writeFileSync(path.join(releaseRoot, '_locales', 'en', 'messages.json'), '{}\n');
    fs.writeFileSync(
      path.join(releaseRoot, 'manifest.json'),
      JSON.stringify({
        manifest_version: 3,
        action: { default_popup: 'popup/popup.html' },
        host_permissions: [
          'https://mail.google.com/*',
          'https://mismatch.example.com/*',
        ],
      }, null, 2)
    );
    fs.mkdirSync(path.join(releaseRoot, 'popup'), { recursive: true });
    fs.writeFileSync(path.join(releaseRoot, 'popup', 'popup.html'), '');
    fs.writeFileSync(
      path.join(releaseRoot, 'lib', 'qurl-api.js'),
      [
        "const DEFAULT_QURL_API_BASE = 'https://getqurllink.layerv.xyz/';",
        "const DEFAULT_QURL_API_BASE_FALLBACK = 'https://getqurllink.layerv.xyz/';",
        '',
      ].join('\n')
    );

    assert.throws(function () {
      buildRelease.validateReleaseManifest(releaseRoot);
    }, /host permission mismatch/);
  } finally {
    fs.rmSync(releaseRoot, { recursive: true, force: true });
  }
});

test('validateReleaseManifest fails when DEFAULT_QURL_API_BASE_FALLBACK is malformed', function () {
  const releaseRoot = makeTempReleaseRoot();

  try {
    fs.mkdirSync(path.join(releaseRoot, '_locales', 'en'), { recursive: true });
    fs.mkdirSync(path.join(releaseRoot, 'lib'), { recursive: true });
    fs.mkdirSync(path.join(releaseRoot, 'popup'), { recursive: true });
    fs.writeFileSync(path.join(releaseRoot, '_locales', 'en', 'messages.json'), '{}\n');
    fs.writeFileSync(path.join(releaseRoot, 'popup', 'popup.html'), '');
    fs.writeFileSync(
      path.join(releaseRoot, 'manifest.json'),
      JSON.stringify({
        manifest_version: 3,
        action: { default_popup: 'popup/popup.html' },
        host_permissions: [
          'https://mail.google.com/*',
          'https://getqurllink.layerv.xyz/*',
        ],
      }, null, 2)
    );
    fs.writeFileSync(
      path.join(releaseRoot, 'lib', 'qurl-api.js'),
      [
        "const DEFAULT_QURL_API_BASE = 'https://getqurllink.layerv.xyz/';",
        "const DEFAULT_QURL_API_BASE_FALLBACK = 'http://bad.example.com/';",
        '',
      ].join('\n')
    );

    assert.throws(function () {
      buildRelease.validateReleaseManifest(releaseRoot);
    }, /Bundled fallback QURL API base is invalid/);
  } finally {
    fs.rmSync(releaseRoot, { recursive: true, force: true });
  }
});

test('parseDotEnv strips simple wrapping quotes from values', function () {
  const tempDir = makeTempReleaseRoot();
  const dotEnvPath = path.join(tempDir, '.env');

  try {
    fs.writeFileSync(dotEnvPath, 'QURL_API_BASE="https://custom.example.com"\n');
    assert.deepEqual(buildRelease.parseDotEnv(dotEnvPath), {
      QURL_API_BASE: 'https://custom.example.com',
    });
  } finally {
    fs.rmSync(tempDir, { recursive: true, force: true });
  }
});

test('validateReleaseManifest fails when a referenced manifest asset is missing', function () {
  const releaseRoot = makeTempReleaseRoot();

  try {
    fs.mkdirSync(path.join(releaseRoot, '_locales', 'en'), { recursive: true });
    fs.writeFileSync(path.join(releaseRoot, '_locales', 'en', 'messages.json'), '{}\n');
    fs.writeFileSync(
      path.join(releaseRoot, 'manifest.json'),
      JSON.stringify({
        manifest_version: 3,
        action: {
          default_popup: 'popup/popup.html',
          default_icon: { 16: 'icons/icon16.png' },
        },
        background: { service_worker: 'background.js' },
        content_scripts: [{ js: ['content/gmail-compose.js'] }],
        icons: { 16: 'icons/icon16.png' },
      }, null, 2)
    );
    fs.writeFileSync(path.join(releaseRoot, 'background.js'), '');

    assert.throws(function () {
      buildRelease.validateReleaseManifest(releaseRoot);
    }, /manifest asset: popup\/popup\.html/);
  } finally {
    fs.rmSync(releaseRoot, { recursive: true, force: true });
  }
});
