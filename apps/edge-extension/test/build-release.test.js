const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('fs');
const os = require('os');
const path = require('path');

const buildRelease = require('../scripts/build-release.js');

function makeTempReleaseRoot() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'qurl-release-test-'));
}

// Mirrors lib/qurl-config.js: a tiny CommonJS-compatible module exporting DEFAULT_QURL_API_BASE.
// The marked declaration line is what writeDefaultApiBaseConfig rewrites.
function writeConfigFixture(releaseRoot, base) {
  fs.mkdirSync(path.join(releaseRoot, 'lib'), { recursive: true });
  fs.writeFileSync(
    path.join(releaseRoot, 'lib', 'qurl-config.js'),
    [
      '(function (global) {',
      `  const DEFAULT_QURL_API_BASE = ${JSON.stringify(base)};`,
      '  const QURLConfig = { DEFAULT_QURL_API_BASE };',
      '  if (global) { global.QURLConfig = QURLConfig; }',
      '  if (typeof module !== "undefined" && module.exports) { module.exports = QURLConfig; }',
      "}(typeof globalThis !== 'undefined' ? globalThis : this));",
      '',
    ].join('\n')
  );
}

function readConfigBase(releaseRoot) {
  return buildRelease.readDefaultQurlApiBase(buildRelease.qurlConfigPath(releaseRoot));
}

test('writeDefaultApiBaseConfig regenerates the marked declaration regardless of source formatting', function () {
  const releaseRoot = makeTempReleaseRoot();

  try {
    // Odd spacing + single quotes — the rewrite is anchored to the declaration, not its formatting.
    fs.mkdirSync(path.join(releaseRoot, 'lib'), { recursive: true });
    fs.writeFileSync(
      path.join(releaseRoot, 'lib', 'qurl-config.js'),
      [
        '(function (global) {',
        "  const DEFAULT_QURL_API_BASE  =  'https://getqurllink.layerv.ai/' ;",
        '  const QURLConfig = { DEFAULT_QURL_API_BASE };',
        '  if (typeof module !== "undefined" && module.exports) { module.exports = QURLConfig; }',
        "}(typeof globalThis !== 'undefined' ? globalThis : this));",
        '',
      ].join('\n')
    );

    buildRelease.writeDefaultApiBaseConfig('https://custom.example.com/base', releaseRoot);

    assert.equal(readConfigBase(releaseRoot), 'https://custom.example.com/base/');
  } finally {
    fs.rmSync(releaseRoot, { recursive: true, force: true });
  }
});

test('writeDefaultApiBaseConfig rewrites the declaration, not a matching comment/string', function () {
  const releaseRoot = makeTempReleaseRoot();

  try {
    // The real lib/qurl-config.js carries a marker comment that itself contains the literal
    // `const DEFAULT_QURL_API_BASE = '...';`. The rewrite must target the actual declaration
    // line, not the first textual match (which is the comment).
    fs.mkdirSync(path.join(releaseRoot, 'lib'), { recursive: true });
    fs.writeFileSync(
      path.join(releaseRoot, 'lib', 'qurl-config.js'),
      [
        '(function (global) {',
        "  // build-release.js rewrites the `const DEFAULT_QURL_API_BASE = '...';` declaration below.",
        "  const DEFAULT_QURL_API_BASE = 'https://getqurllink.layerv.ai/';",
        '  const QURLConfig = { DEFAULT_QURL_API_BASE };',
        '  if (typeof module !== "undefined" && module.exports) { module.exports = QURLConfig; }',
        "}(typeof globalThis !== 'undefined' ? globalThis : this));",
        '',
      ].join('\n')
    );

    buildRelease.writeDefaultApiBaseConfig('https://custom.example.com', releaseRoot);

    const written = fs.readFileSync(path.join(releaseRoot, 'lib', 'qurl-config.js'), 'utf8');
    assert.equal(readConfigBase(releaseRoot), 'https://custom.example.com/');
    // The decoy comment is untouched.
    assert.ok(written.includes("// build-release.js rewrites the `const DEFAULT_QURL_API_BASE = '...';` declaration below."));
  } finally {
    fs.rmSync(releaseRoot, { recursive: true, force: true });
  }
});

test('writeDefaultApiBaseConfig preserves $ and apostrophes in the replacement URL', function () {
  const releaseRoot = makeTempReleaseRoot();

  try {
    writeConfigFixture(releaseRoot, 'https://getqurllink.layerv.ai/');
    buildRelease.writeDefaultApiBaseConfig("https://custom.example.com/path/$1/o'connor", releaseRoot);
    assert.equal(readConfigBase(releaseRoot), "https://custom.example.com/path/$1/o'connor/");
  } finally {
    fs.rmSync(releaseRoot, { recursive: true, force: true });
  }
});

test('writeDefaultApiBaseConfig leaves the base value unchanged when the override matches', function () {
  const releaseRoot = makeTempReleaseRoot();

  try {
    writeConfigFixture(releaseRoot, 'https://getqurllink.layerv.ai/');
    assert.doesNotThrow(function () {
      buildRelease.writeDefaultApiBaseConfig('https://getqurllink.layerv.ai', releaseRoot);
    });
    assert.equal(readConfigBase(releaseRoot), 'https://getqurllink.layerv.ai/');
  } finally {
    fs.rmSync(releaseRoot, { recursive: true, force: true });
  }
});

test('applyBuildOverrides rewrites both the config default and the manifest host permission', function () {
  const releaseRoot = makeTempReleaseRoot();
  const originalLog = console.log;
  console.log = function () {};

  try {
    writeConfigFixture(releaseRoot, 'https://getqurllink.layerv.ai/');
    // rewriteManifestHostPermission derives the entry to replace from the PROJECT config
    // (the real production default), so the manifest must carry that production pattern.
    fs.writeFileSync(
      path.join(releaseRoot, 'manifest.json'),
      JSON.stringify({
        host_permissions: [
          'https://mail.google.com/*',
          'https://getqurllink.layerv.ai/*',
        ],
      }, null, 2)
    );

    buildRelease.applyBuildOverrides({
      qurlApiBase: 'https://custom.example.com/api/upload',
    }, releaseRoot);

    const manifest = JSON.parse(fs.readFileSync(path.join(releaseRoot, 'manifest.json'), 'utf8'));

    assert.equal(readConfigBase(releaseRoot), 'https://custom.example.com/');
    assert.deepEqual(manifest.host_permissions, [
      'https://mail.google.com/*',
      'https://custom.example.com/*',
    ]);
  } finally {
    console.log = originalLog;
    fs.rmSync(releaseRoot, { recursive: true, force: true });
  }
});

test('applyBuildOverrides drops a port from the manifest pattern but keeps it in the config base', function () {
  const releaseRoot = makeTempReleaseRoot();
  const originalLog = console.log;
  console.log = function () {};

  try {
    writeConfigFixture(releaseRoot, 'https://getqurllink.layerv.ai/');
    fs.writeFileSync(
      path.join(releaseRoot, 'manifest.json'),
      JSON.stringify({
        host_permissions: [
          'https://mail.google.com/*',
          'https://getqurllink.layerv.ai/*',
        ],
      }, null, 2)
    );

    buildRelease.applyBuildOverrides({
      qurlApiBase: 'https://self.hosted.example:8443',
    }, releaseRoot);

    const manifest = JSON.parse(fs.readFileSync(path.join(releaseRoot, 'manifest.json'), 'utf8'));

    // Chrome match patterns reject ports, so the manifest pattern must be port-less...
    assert.deepEqual(manifest.host_permissions, [
      'https://mail.google.com/*',
      'https://self.hosted.example/*',
    ]);
    // ...while the upload base URL retains the port so requests reach the right endpoint.
    assert.equal(readConfigBase(releaseRoot), 'https://self.hosted.example:8443/');
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
    writeConfigFixture(releaseRoot, 'https://getqurllink.layerv.ai/');
    fs.mkdirSync(path.join(releaseRoot, '_locales', 'en'), { recursive: true });
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
          'https://getqurllink.layerv.ai/*',
        ],
      }, null, 2)
    );

    buildRelease.applyBuildOverrides({
      qurlApiBase: 'https://custom.example.com/base/api/upload',
    }, releaseRoot);

    assert.doesNotThrow(function () {
      buildRelease.validateReleaseManifest(releaseRoot);
    });

    // The regenerated config module still loads and exposes the override base.
    assert.equal(readConfigBase(releaseRoot), 'https://custom.example.com/base/');
  } finally {
    console.log = originalLog;
    fs.rmSync(releaseRoot, { recursive: true, force: true });
  }
});

test('rewriteManifestHostPermission rewrites the bundled host entry derived from the config default', function () {
  const releaseRoot = makeTempReleaseRoot();

  try {
    // The function reads the bundled default from the project config to decide which entry to
    // replace, so the manifest must carry the real production pattern.
    fs.writeFileSync(
      path.join(releaseRoot, 'manifest.json'),
      JSON.stringify({
        host_permissions: [
          'https://mail.google.com/*',
          'https://getqurllink.layerv.ai/*',
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

test('validateReleaseManifest fails when the bundled host permission drifts from the config default', function () {
  const releaseRoot = makeTempReleaseRoot();

  try {
    writeConfigFixture(releaseRoot, 'https://getqurllink.layerv.ai/');
    fs.mkdirSync(path.join(releaseRoot, '_locales', 'en'), { recursive: true });
    fs.writeFileSync(path.join(releaseRoot, '_locales', 'en', 'messages.json'), '{}\n');
    fs.mkdirSync(path.join(releaseRoot, 'popup'), { recursive: true });
    fs.writeFileSync(path.join(releaseRoot, 'popup', 'popup.html'), '');
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

    assert.throws(function () {
      buildRelease.validateReleaseManifest(releaseRoot);
    }, /host permission mismatch/);
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
