// Temp-directory scaffolding shared by the test suites.
//
// Not collected as a suite itself: package.json runs `node --test test/*.test.js`, and that glob
// matches the top level only, so nothing under test/helpers/ is picked up as a test file.

const fs = require('fs');
const os = require('os');
const path = require('path');

function makeTempDir(prefix) {
  return fs.mkdtempSync(path.join(os.tmpdir(), prefix));
}

function removeTempDir(tempDir) {
  fs.rmSync(tempDir, { recursive: true, force: true });
}

// `return await run(...)` inside the try, not `return run(...)`: the callback is async, and
// returning its pending promise would hand the directory back to `finally` for deletion while the
// callback was still writing into it.
async function withTempDir(prefix, run) {
  const tempDir = makeTempDir(prefix);

  try {
    return await run(tempDir);
  } finally {
    removeTempDir(tempDir);
  }
}

// Synchronous counterpart, for test bodies that never await. Kept separate rather than folded into
// withTempDir so a sync caller stays sync: awaiting would make every one of them async and hand
// node:test a promise to settle for no reason.
function withTempDirSync(prefix, run) {
  const tempDir = makeTempDir(prefix);

  try {
    const result = run(tempDir);

    // An async callback slipped in here would reintroduce exactly the race withTempDir's `await`
    // exists to prevent, and it would surface as a flake rather than a failure. Reject it loudly.
    if (result !== null && typeof result === 'object' && typeof result.then === 'function') {
      throw new TypeError('withTempDirSync callback returned a promise; use withTempDir instead');
    }

    return result;
  } finally {
    removeTempDir(tempDir);
  }
}

module.exports = { withTempDir, withTempDirSync };
