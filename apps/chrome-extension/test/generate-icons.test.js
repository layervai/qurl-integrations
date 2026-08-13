const test = require('node:test');
const assert = require('node:assert/strict');
const crypto = require('crypto');
const fs = require('fs');
const os = require('os');
const path = require('path');

const generateIcons = require('../scripts/generate-icons.js');

const generateIconsModulePath = require.resolve('../scripts/generate-icons.js');
const sharpModulePath = require.resolve('sharp');

// The three files this guard exists to protect, spelled out rather than derived from
// `generateIcons.sizes`: assertions built from the value under test go vacuously green if that
// value ever becomes empty.
const EXPECTED_ICON_FILES = ['icon128.png', 'icon16.png', 'icon48.png'];

function sha256(buffer) {
  return crypto.createHash('sha256').update(buffer).digest('hex');
}

function committedIconPath(size) {
  return path.join(generateIcons.defaultIconsDir, `icon${size}.png`);
}

// Hash plus mtime. The hash keeps an assertion failure readable instead of dumping kilobytes of
// PNG, and the mtime is what actually detects an unwanted write: a generator that ignored
// `outDir` would rewrite `icons/` with byte-identical content, which a hash alone cannot see.
function committedIconFingerprints() {
  const fingerprints = {};

  for (const size of generateIcons.sizes) {
    const iconPath = committedIconPath(size);

    fingerprints[size] = {
      sha256: sha256(fs.readFileSync(iconPath)),
      mtimeMs: fs.statSync(iconPath).mtimeMs,
    };
  }

  return fingerprints;
}

// `await run(...)` inside the try, not `return run(...)`: the callback is async, and returning its
// pending promise would let `finally` delete the directory while sharp was still writing into it.
async function withTempDir(prefix, run) {
  const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), prefix));

  try {
    return await run(tempDir);
  } finally {
    fs.rmSync(tempDir, { recursive: true, force: true });
  }
}

test('generate-icons exports its entry points', function () {
  assert.equal(typeof generateIcons.generateIcons, 'function');
  assert.equal(typeof generateIcons.main, 'function');
  assert.deepEqual(generateIcons.sizes, [16, 48, 128]);
});

// `sizes` on its own is a restatement of the module's own constant. The coupling that can actually
// break is with manifest.json, which declares the icon sizes twice: a size declared there but not
// generated ships an icon path Chrome resolves to nothing and renders blank.
test('manifest icon declarations match the generated sizes', function () {
  const manifestPath = path.join(path.dirname(generateIcons.defaultIconsDir), 'manifest.json');
  const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));
  const expectedSizes = generateIcons.sizes.map(String).sort();

  const declarations = [
    ['icons', manifest.icons],
    ['action.default_icon', manifest.action && manifest.action.default_icon],
  ];

  for (const [label, declaration] of declarations) {
    assert.ok(declaration, `manifest.json has no ${label} block`);
    assert.deepEqual(Object.keys(declaration).sort(), expectedSizes, `manifest.json ${label} declares sizes "npm run icons" does not generate`);

    for (const [size, iconPath] of Object.entries(declaration)) {
      assert.equal(iconPath, `icons/icon${size}.png`, `manifest.json ${label}["${size}"] points somewhere unexpected`);
    }
  }
});

// This is the one failure mode that would leave a green but useless guard. If the
// `require.main === module` guard in the script regressed, the `require` at the top of this file
// would regenerate the real `icons/` before any assertion ran — and the drift test below would
// then compare freshly written files against themselves and pass forever while dirtying the tree.
//
// Stubbing the side-effecting primitive is how `package-release.test.js` proves the same property
// for its sibling script. It works here because `loadSharp()` requires sharp lazily, inside
// `generateIcons`, so the stub is reachable. It is not racy: an auto-run would call `main()` during
// `require`, and an async function runs synchronously up to its first `await` — which is after
// `sharp(sourcePath)` is invoked — so the call is recorded before `require` returns.
test('requiring the module does not generate icons', function () {
  const realSharp = require.cache[sharpModulePath];
  const calls = [];

  require.cache[sharpModulePath] = {
    id: sharpModulePath,
    filename: sharpModulePath,
    loaded: true,
    exports: function () {
      calls.push(Array.from(arguments));
      throw new Error('sharp stub must not be invoked');
    },
  };
  delete require.cache[generateIconsModulePath];

  try {
    require('../scripts/generate-icons.js');
    assert.deepEqual(calls, [], 'requiring generate-icons.js invoked sharp — the require.main guard is not holding');
  } finally {
    if (realSharp) {
      require.cache[sharpModulePath] = realSharp;
    } else {
      delete require.cache[sharpModulePath];
    }
    delete require.cache[generateIconsModulePath];
  }
});

test('generateIcons refuses to run without an explicit outDir', async function () {
  // The default would be the committed icons/ directory, so a caller that forgets the argument
  // must fail loudly rather than quietly rewriting tracked files.
  await assert.rejects(
    function () { return generateIcons.generateIcons(); },
    /requires an explicit outDir/
  );
});

// Guards against the committed icons drifting from `icons/logo.png` — see #908, where a
// sharp ^0.34.5 -> ^0.35.0 bump changed the PNG encoder and left the committed 16px and 48px
// files stale (128px happened to survive byte-identical, which is why it went unnoticed).
//
// The comparison is byte-exact rather than pixel-based on purpose: that sharp bump left every
// decoded pixel bit-for-bit identical, and the stale and correct icon16 were both 783 bytes, so
// neither a pixel comparison at any tolerance nor a size check would have seen a difference.
// Byte-exact is safe to gate on because the generator is deterministic for a fixed sharp version,
// and the prebuilt sharp binaries agree byte-for-byte across the platforms this runs on.
// #1046 tracks the cost of that strictness: any upstream encoder change fails this test until
// the icons are regenerated.
test('committed icons match a fresh "npm run icons"', async function () {
  await withTempDir('qurl-icon-drift-', async function (tempDir) {
    await generateIcons.generateIcons({ outDir: tempDir });

    // Pins the comparison below to all three files: without it, a generator that wrote nothing
    // would leave the loop with no iterations and this test vacuously green.
    assert.deepEqual(fs.readdirSync(tempDir).sort(), EXPECTED_ICON_FILES);

    for (const size of generateIcons.sizes) {
      const committedPath = committedIconPath(size);

      // Without this, a deleted icon fails as a bare ENOENT stack rather than the remediation below.
      assert.ok(
        fs.existsSync(committedPath),
        `icons/icon${size}.png is missing. Fix: run \`npm run icons\` in apps/chrome-extension and commit the result.`
      );

      const committed = fs.readFileSync(committedPath);
      const fresh = fs.readFileSync(path.join(tempDir, `icon${size}.png`));

      // Built only on failure — the happy path runs on every `npm test`.
      if (!committed.equals(fresh)) {
        assert.fail(
          `icons/icon${size}.png is stale: it does not match what "npm run icons" generates from icons/logo.png.\n` +
          `  committed: ${committed.length} bytes, sha256 ${sha256(committed)}\n` +
          `  generated: ${fresh.length} bytes, sha256 ${sha256(fresh)}\n` +
          'Fix: run `npm run icons` in apps/chrome-extension and commit the result.\n' +
          'If you did not touch icons/logo.png, the encoder changed under you — most likely a sharp\n' +
          'upgrade, or a sharp built against a different libvips (musl/Alpine, or a global-libvips\n' +
          'build). Regenerating and committing is still the fix (see #1046).'
        );
      }
    }
  });
});

// The drift test is only trustworthy if `outDir` really redirects the output: were it ignored, that
// test would regenerate `icons/` in place and compare those files against themselves, passing no
// matter what. This pins both halves — a nested `outDir` that does not exist yet is created and
// written to, and `icons/` is left untouched.
test('generateIcons honors a custom outDir and leaves icons/ untouched', async function () {
  const before = committedIconFingerprints();

  await withTempDir('qurl-icon-outdir-', async function (tempDir) {
    const outDir = path.join(tempDir, 'nested', 'icons');
    const generated = [];

    const written = await generateIcons.generateIcons({
      outDir,
      onGenerated: function (pngPath) { generated.push(pngPath); },
    });

    assert.deepEqual(written.map(function (pngPath) { return path.basename(pngPath); }).sort(), EXPECTED_ICON_FILES);
    // The CLI reports progress through this callback, so it must fire once per file, in order.
    assert.deepEqual(generated, written);

    for (const pngPath of written) {
      assert.equal(path.dirname(pngPath), outDir);
      assert.ok(fs.statSync(pngPath).size > 0, `${pngPath} is empty`);
    }
  });

  assert.deepEqual(committedIconFingerprints(), before, 'generating into a custom outDir wrote to icons/');
});
