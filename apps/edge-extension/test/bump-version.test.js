const test = require('node:test');
const assert = require('node:assert/strict');

const bumpVersionModulePath = require.resolve('../scripts/bump-version.js');

test.afterEach(function () {
  delete require.cache[bumpVersionModulePath];
});

test('resolveNextVersion rejects explicit versions that do not increase monotonically', function () {
  const bumpVersion = require('../scripts/bump-version.js');

  assert.throws(function () {
    bumpVersion.resolveNextVersion('1.2.3', '1.2.3');
  }, /greater than the current version/);

  assert.throws(function () {
    bumpVersion.resolveNextVersion('1.2.3', '1.2.2');
  }, /greater than the current version/);
});

test('resolveNextVersion accepts a higher explicit version and symbolic bumps', function () {
  const bumpVersion = require('../scripts/bump-version.js');

  assert.equal(bumpVersion.resolveNextVersion('1.2.3', '1.2.4'), '1.2.4');
  assert.equal(bumpVersion.resolveNextVersion('1.2.3', 'minor'), '1.3.0');
});
