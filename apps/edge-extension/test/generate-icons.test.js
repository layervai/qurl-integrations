const test = require('node:test');
const assert = require('node:assert/strict');
const childProcess = require('child_process');
const crypto = require('crypto');
const fs = require('fs');
const os = require('os');
const path = require('path');

const generateIcons = require('../scripts/generate-icons.js');

const generateIconsModulePath = require.resolve('../scripts/generate-icons.js');
const sharpModulePath = require.resolve('sharp');

// Spelled out here rather than derived from `generateIcons.sizes`: assertions built from the value
// under test go vacuously green if that value ever becomes empty.
const EXPECTED_SIZES = [16, 48, 128];
const EXPECTED_ICON_FILES = EXPECTED_SIZES.map(function (size) { return `icon${size}.png`; }).sort();

function sha256(buffer) {
  return crypto.createHash('sha256').update(buffer).digest('hex');
}

function committedIconPath(size) {
  return path.join(generateIcons.defaultIconsDir, `icon${size}.png`);
}

// mtime is what detects an unwanted write, since a generator that ignored `outDir` would rewrite
// `icons/` with byte-identical content. The hash covers the converse case a coarse-granularity
// filesystem could hide: same mtime tick, different bytes.
function committedIconFingerprints() {
  const fingerprints = {};

  for (const size of EXPECTED_SIZES) {
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

// `sizes` alone would just restate the module's own constant. The coupling that can actually break
// is with manifest.json, which declares the icon sizes twice: a size declared there but never
// generated ships an icon path Edge resolves to nothing and renders blank.
test('manifest icon declarations match the generated sizes', function () {
  assert.deepEqual(generateIcons.sizes, EXPECTED_SIZES);

  const manifestPath = path.join(generateIcons.projectRoot, 'manifest.json');
  const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));
  const expectedKeys = EXPECTED_SIZES.map(String).sort();

  const declarations = [
    ['icons', manifest.icons],
    ['action.default_icon', manifest.action && manifest.action.default_icon],
  ];

  for (const [label, declaration] of declarations) {
    assert.ok(declaration, `manifest.json has no ${label} block`);
    assert.deepEqual(Object.keys(declaration).sort(), expectedKeys, `manifest.json ${label} declares sizes "npm run icons" does not generate`);

    for (const [size, iconPath] of Object.entries(declaration)) {
      assert.equal(iconPath, `icons/icon${size}.png`, `manifest.json ${label}["${size}"] points somewhere unexpected`);
    }
  }
});

// This is the one failure mode that would leave a green but useless guard. If the
// `require.main === module` guard in the script regressed, the `require` at the top of this file
// would regenerate the real `icons/` before any assertion ran — and the drift test below would then
// compare freshly written files against themselves and pass forever while dirtying the tree.
//
// Must not run concurrently with the tests that invoke real sharp: this swaps a stub into
// `require.cache` and restores it in `finally`, and `node:test` runs top-level tests sequentially.
// Stubbing the side-effecting primitive is how `package-release.test.js` proves the same property
// for its sibling script. It works here because `loadSharp()` requires sharp lazily, inside
// `generateIcons`, so the stub is reachable. It is not racy: an auto-run would call `main()` during
// `require`, and an async function runs synchronously up to its first `await` — which is after
// `sharp(...)` is invoked — so the call is recorded before `require` returns.
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

    // Pins the comparison to all three files: a generator that wrote nothing would otherwise leave
    // the loop with no iterations and this test vacuously green.
    assert.deepEqual(fs.readdirSync(tempDir).sort(), EXPECTED_ICON_FILES);

    for (const size of EXPECTED_SIZES) {
      const committedPath = committedIconPath(size);

      // Without this, a deleted icon fails as a bare ENOENT stack rather than the remediation below.
      assert.ok(
        fs.existsSync(committedPath),
        `icons/icon${size}.png is missing. Fix: run \`npm run icons\` in apps/edge-extension and commit the result.`
      );

      const committed = fs.readFileSync(committedPath);
      const fresh = fs.readFileSync(path.join(tempDir, `icon${size}.png`));

      // Kept out of `assert.ok` so the hashes are computed only on failure.
      if (!committed.equals(fresh)) {
        assert.fail(
          `icons/icon${size}.png is stale: it does not match what "npm run icons" generates from icons/logo.png.\n` +
          `  committed: ${committed.length} bytes, sha256 ${sha256(committed)}\n` +
          `  generated: ${fresh.length} bytes, sha256 ${sha256(fresh)}\n` +
          'Fix: run `npm run icons` in apps/edge-extension and commit the result.\n' +
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
    const written = await generateIcons.generateIcons({ outDir });

    assert.deepEqual(written.map(function (pngPath) { return path.basename(pngPath); }).sort(), EXPECTED_ICON_FILES);

    for (const pngPath of written) {
      assert.equal(path.dirname(pngPath), outDir);
      assert.ok(fs.statSync(pngPath).size > 0, `${pngPath} is empty`);
    }
  });

  assert.deepEqual(committedIconFingerprints(), before, 'generating into a custom outDir wrote to icons/');
});

// The one genuinely new runtime behaviour is that a broken `npm run icons` fails loudly; it used to
// print the error and exit 0. Asserting that needs a real process, and it must not be the repo's own
// script — running that on the happy path would rewrite `icons/` and move the mtimes the test above
// depends on. Running a copy from a temp directory fails hermetically instead: `projectRoot` derives
// from `__dirname`, so the copy looks for a `logo.png` that isn't there.
test('the CLI reports failures and exits non-zero', async function () {
  await withTempDir('qurl-icon-cli-', function (tempDir) {
    const scriptDir = path.join(tempDir, 'scripts');
    fs.mkdirSync(scriptDir);
    fs.copyFileSync(
      path.join(generateIcons.projectRoot, 'scripts', 'generate-icons.js'),
      path.join(scriptDir, 'generate-icons.js')
    );

    const result = childProcess.spawnSync(process.execPath, [path.join(scriptDir, 'generate-icons.js')], { encoding: 'utf8' });

    assert.equal(result.status, 1, `expected exit 1, got ${result.status}. stderr: ${result.stderr}`);
    assert.match(result.stderr, /Missing icon source/);
    assert.equal(result.stdout, '', 'a failed run should not claim it generated anything');
  });
});
