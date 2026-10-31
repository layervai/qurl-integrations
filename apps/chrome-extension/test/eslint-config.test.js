const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const { ESLint } = require('eslint');

const linter = new ESLint({ cwd: path.resolve(__dirname, '..') });

for (const [name, filePath, source, expected] of [
  ['browser globals', 'lib/config-fixture.js', 'QURLConfig.read(); chrome.runtime.getManifest();', []],
  ['CommonJS globals', 'scripts/config-fixture.js', "const crypto = require('node:crypto'); module.exports = crypto; console.log('build');", []],
  ['test overrides', 'test/config-fixture.js', "const unused = 1; console.log('test');", []],
  ['undefined name', 'lib/config-fixture.js', 'missingGlobal();', ['no-undef']],
  ['duplicate declaration', 'lib/config-fixture.js', 'var value; var value = 2; console.warn(value);', ['no-redeclare']],
  ['unused variable', 'lib/config-fixture.js', 'const unused = 1;', ['no-unused-vars']],
  ['runtime console log', 'lib/config-fixture.js', "console.log('runtime');", ['no-console']],
]) {
  test('flat ESLint configuration preserves ' + name, async function () {
    const [result] = await linter.lintText(source, { filePath });
    assert.deepEqual(result.messages.map(function (message) { return message.ruleId; }), expected);
  });
}
