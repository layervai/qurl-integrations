const path = require('node:path');
const { ESLint } = require('eslint');

const root = path.join(__dirname, '..');
const eslint = new ESLint({ cwd: root });

test.each([
  "const { revokeResource } = require('./qurl'); void revokeResource;",
  "require('./qurl').revokeResource('resource');",
  "require('./qurl')['revokeResource']('resource');",
])('bot code cannot revoke a shared resource: %s', async source => {
  const [result] = await eslint.lintText(source, { filePath: path.join(root, 'src/revocation-guard.js') });
  expect(result.messages.filter(message => message.ruleId === 'no-restricted-syntax')).toHaveLength(1);
});

test('the load-test script can revoke its own resources', async () => {
  const [result] = await eslint.lintText("require('../src/qurl').revokeResource('resource');", {
    filePath: path.join(root, 'scripts/loadtest-standalone.js'),
  });
  expect(result.errorCount).toBe(0);
});
