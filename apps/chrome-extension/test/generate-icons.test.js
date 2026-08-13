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

test('generate-icons exports its entry points', function () {
  assert.equal(typeof generateIcons.generateIcons, 'function');
  assert.equal(typeof generateIcons.main, 'function');
  assert.deepEqual(generateIcons.sizes, [16, 48, 128]);
});

// Guards against the committed icons drifting from `icons/logo.png` — see #908, where a
// sharp ^0.34.5 -> ^0.35.0 bump changed the PNG encoder and left the committed 16px and 48px
// files stale (128px happened to survive byte-identical, which is why it went unnoticed).
//
// The comparison is byte-exact rather than pixel-based on purpose: that sharp bump left every
// decoded pixel bit-for-bit identical, so a pixel comparison — at any tolerance — would have
// seen no difference at all and missed the drift. Byte-exact is safe to gate on because the
// generator is deterministic for a fixed sharp version and its output does not vary by platform.
// #1046 tracks the cost of that strictness: any upstream encoder change fails this test until
// the icons are regenerated.
test('committed icons match a fresh "npm run icons"', async function () {
  const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), 'qurl-icon-drift-'));

  try {
    const written = await generateIcons.generateIcons({ outDir: tempDir });
    assert.equal(written.length, generateIcons.sizes.length);

    for (const size of generateIcons.sizes) {
      const committedPath = path.join(generateIcons.defaultIconsDir, `icon${size}.png`);
      const committed = fs.readFileSync(committedPath);
      const fresh = fs.readFileSync(path.join(tempDir, `icon${size}.png`));

      assert.ok(
        committed.equals(fresh),
        `icons/icon${size}.png is stale: it does not match what "npm run icons" generates from icons/logo.png.\n` +
        `  committed: ${committed.length} bytes, sha256 ${sha256(committed)}\n` +
        `  generated: ${fresh.length} bytes, sha256 ${sha256(fresh)}\n` +
        'Fix: run `npm run icons` in apps/chrome-extension and commit the result.\n' +
        'If you did not touch icons/logo.png, a sharp upgrade changed the PNG encoder — ' +
        'regenerating and committing is still the fix (see #1046).'
      );
    }
  } finally {
    fs.rmSync(tempDir, { recursive: true, force: true });
  }
});
