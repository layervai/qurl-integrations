const test = require('node:test');
const assert = require('node:assert/strict');
const childProcess = require('child_process');
const path = require('path');

const packageReleaseModulePath = require.resolve('../scripts/package-release.js');
const originalExecFileSync = childProcess.execFileSync;

test.afterEach(function () {
  childProcess.execFileSync = originalExecFileSync;
  delete require.cache[packageReleaseModulePath];
});

test('requiring package-release does not execute the packaging pipeline', function () {
  const execCalls = [];
  childProcess.execFileSync = function () {
    execCalls.push(Array.from(arguments));
    return '';
  };

  const packageRelease = require('../scripts/package-release.js');

  assert.equal(typeof packageRelease.main, 'function');
  assert.equal(typeof packageRelease.createZipFromRelease, 'function');
  assert.deepEqual(execCalls, []);
});

test('escapePowerShell doubles embedded single quotes', function () {
  const packageRelease = require('../scripts/package-release.js');
  assert.equal(
    packageRelease.escapePowerShell("C:\\Users\\o'connor\\build"),
    "C:\\Users\\o''connor\\build"
  );
});

test('createZipFromRelease shells out to zip on non-Windows platforms', function () {
  const execCalls = [];
  childProcess.execFileSync = function () {
    execCalls.push(Array.from(arguments));
    return '';
  };

  const packageRelease = require('../scripts/package-release.js');
  const zipPath = path.join(__dirname, '..', 'dist', 'qurl-package.zip');
  packageRelease.createZipFromRelease(zipPath);

  assert.equal(execCalls.length, 1);
  assert.equal(execCalls[0][0], 'zip');
  assert.deepEqual(execCalls[0][1], ['-r', '../dist/qurl-package.zip', '.', '-x', '*.DS_Store', '*/.DS_Store']);
});
