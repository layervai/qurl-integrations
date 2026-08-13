const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('fs');
const path = require('path');

const buildRelease = require('../scripts/build-release.js');

const { withTempDirSync } = require('./helpers/temp-dir.js');
const { EXPECTED_ICON_FILES } = require('./helpers/icons.js');

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
  withTempDirSync('qurl-release-test-', function (releaseRoot) {
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
  });
});

test('writeDefaultApiBaseConfig rewrites the declaration, not a matching comment/string', function () {
  withTempDirSync('qurl-release-test-', function (releaseRoot) {
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
  });
});

test('writeDefaultApiBaseConfig preserves $ and apostrophes in the replacement URL', function () {
  withTempDirSync('qurl-release-test-', function (releaseRoot) {
    writeConfigFixture(releaseRoot, 'https://getqurllink.layerv.ai/');
    buildRelease.writeDefaultApiBaseConfig("https://custom.example.com/path/$1/o'connor", releaseRoot);
    assert.equal(readConfigBase(releaseRoot), "https://custom.example.com/path/$1/o'connor/");
  });
});

test('writeDefaultApiBaseConfig leaves the base value unchanged when the override matches', function () {
  withTempDirSync('qurl-release-test-', function (releaseRoot) {
    writeConfigFixture(releaseRoot, 'https://getqurllink.layerv.ai/');
    assert.doesNotThrow(function () {
      buildRelease.writeDefaultApiBaseConfig('https://getqurllink.layerv.ai', releaseRoot);
    });
    assert.equal(readConfigBase(releaseRoot), 'https://getqurllink.layerv.ai/');
  });
});

test('applyBuildOverrides rewrites both the config default and the manifest host permission', function () {
  withTempDirSync('qurl-release-test-', function (releaseRoot) {
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
    }
  });
});

test('applyBuildOverrides drops a port from the manifest pattern but keeps it in the config base', function () {
  withTempDirSync('qurl-release-test-', function (releaseRoot) {
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
    }
  });
});

test('applyBuildOverrides keeps the release bundle self-consistent end to end', function () {
  withTempDirSync('qurl-release-test-', function (releaseRoot) {
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
    }
  });
});

test('rewriteManifestHostPermission rewrites the bundled host entry derived from the config default', function () {
  withTempDirSync('qurl-release-test-', function (releaseRoot) {
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
  });
});

test('validateReleaseManifest fails when localized messages are missing', function () {
  withTempDirSync('qurl-release-test-', function (releaseRoot) {
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
  });
});

test('validateReleaseManifest fails when the bundled host permission drifts from the config default', function () {
  withTempDirSync('qurl-release-test-', function (releaseRoot) {
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
  });
});

test('parseDotEnv strips simple wrapping quotes from values', function () {
  withTempDirSync('qurl-release-test-', function (tempDir) {
    const dotEnvPath = path.join(tempDir, '.env');

    fs.writeFileSync(dotEnvPath, 'QURL_API_BASE="https://custom.example.com"\n');
    assert.deepEqual(buildRelease.parseDotEnv(dotEnvPath), {
      QURL_API_BASE: 'https://custom.example.com',
    });
  });
});

test('validateReleaseManifest fails when a referenced manifest asset is missing', function () {
  withTempDirSync('qurl-release-test-', function (releaseRoot) {
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
  });
});

test('logo.png is excluded from the release bundle and nothing at runtime loads it', function () {
  const projectRoot = path.resolve(__dirname, '..');

  // The exclusion only holds while logo.png is purely a build-time source for
  // generate-icons.js. If a runtime file ever points back at it, the packaged extension would
  // reference an asset the bundle no longer carries — a broken image users see but CI would not.
  assert.ok(
    buildRelease.excludePaths.has(path.join('icons', 'logo.png')),
    'icons/logo.png should be excluded from the release bundle'
  );

  const runtimeFiles = [
    path.join('manifest.json'),
    path.join('popup', 'popup.html'),
    path.join('popup', 'popup.css'),
    path.join('popup', 'popup.js'),
    path.join('background.js'),
    path.join('content', 'gmail-compose.js'),
  ];

  for (const relativePath of runtimeFiles) {
    const contents = fs.readFileSync(path.join(projectRoot, relativePath), 'utf8');
    assert.ok(
      !contents.includes('logo.png'),
      `${relativePath} references icons/logo.png, which build-release.js excludes from the bundle`
    );
  }

  // The source itself must still be present for `npm run icons` to regenerate from.
  assert.ok(fs.existsSync(path.join(projectRoot, 'icons', 'logo.png')));

  // Assert the behavior, not just the config: run the real copy over the real icons/ directory
  // and check what lands. Asserting only that excludePaths contains the entry would still pass
  // if copyRecursive changed how it derives the relative path (projectRoot drift, a
  // path.resolve vs path.join mismatch) and shipped the file anyway.
  withTempDirSync('qurl-release-test-', function (stagingRoot) {
    buildRelease.copyRecursive(path.join(projectRoot, 'icons'), path.join(stagingRoot, 'icons'));

    const copied = fs.readdirSync(path.join(stagingRoot, 'icons')).sort();
    assert.ok(!copied.includes('logo.png'), `logo.png reached the bundle: ${copied.join(', ')}`);
    assert.deepEqual(copied, EXPECTED_ICON_FILES);
  });
});
