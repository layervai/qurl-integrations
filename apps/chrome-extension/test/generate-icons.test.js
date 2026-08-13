const test = require('node:test');
const assert = require('node:assert/strict');
const crypto = require('crypto');
const fs = require('fs');
const os = require('os');
const path = require('path');

const generateIcons = require('../scripts/generate-icons.js');

function sha256(buffer) {
  return crypto.createHash('sha256').update(buffer).digest('hex');
}

function committedIconPath(size) {
  return path.join(generateIcons.defaultIconsDir, `icon${size}.png`);
}

// Hashes rather than buffers: they compare just as strictly but keep an assertion
// failure readable instead of dumping kilobytes of PNG.
function committedIconHashes() {
  const hashes = {};

  for (const size of generateIcons.sizes) {
    hashes[size] = sha256(fs.readFileSync(committedIconPath(size)));
  }

  return hashes;
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
    const written = await generateIcons.generateIcons({ outDir: tempDir });
    assert.equal(written.length, generateIcons.sizes.length);

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

// The drift test above is only trustworthy if `outDir` really redirects the output: were it
// ignored, the test would regenerate `icons/` in place and then compare those files against
// themselves, passing no matter what. This pins both halves of that — a nested `outDir` that
// does not exist yet is created and written to, and `icons/` is left untouched.
test('generateIcons honors a custom outDir and leaves icons/ untouched', async function () {
  const before = committedIconHashes();

  await withTempDir('qurl-icon-outdir-', async function (tempDir) {
    const outDir = path.join(tempDir, 'nested', 'icons');
    const written = await generateIcons.generateIcons({ outDir });

    assert.deepEqual(
      written.map(function (pngPath) { return path.basename(pngPath); }),
      generateIcons.sizes.map(function (size) { return `icon${size}.png`; })
    );

    for (const pngPath of written) {
      assert.equal(path.dirname(pngPath), outDir);
      assert.ok(fs.statSync(pngPath).size > 0, `${pngPath} is empty`);
    }
  });

  assert.deepEqual(committedIconHashes(), before, 'generating into a custom outDir rewrote icons/');
});
