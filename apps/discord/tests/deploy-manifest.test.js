const fs = require('node:fs');
const path = require('node:path');
const parser = require('@babel/parser');
const traverse = require('@babel/traverse').default;
const manifest = require('../deploy-manifest.json');
const srcDir = path.join(__dirname, '../src');
const ENV_GROUPS = ['required', 'required_production', 'optional', 'sdk_optional', 'forbidden_production'];

function javascriptFiles(directory) {
  return fs.readdirSync(directory, { withFileTypes: true }).flatMap(entry => {
    const file = path.join(directory, entry.name);
    return entry.isDirectory() ? javascriptFiles(file) : entry.name.endsWith('.js') ? [file] : [];
  });
}

function envReads(files, strict = true) {
  const names = new Set();
  for (const file of files) {
    const ast = parser.parse(fs.readFileSync(file, 'utf8'), { sourceType: 'unambiguous' });
    const member = memberPath => {
      const node = memberPath.node;
      const object = node.object;
      if (object?.name === 'process' && (node.computed ? node.property.value : node.property.name) === 'env') {
        const parent = memberPath.parent;
        const directRead = ['MemberExpression', 'OptionalMemberExpression'].includes(parent.type) && parent.object === node;
        const knownBootCall = file === path.join(srcDir, 'index.js') && parent.type === 'CallExpression'
          && parent.arguments[0] === node && ['missingProdKeys', 'missingKekRequiredKeys'].includes(parent.callee.name);
        if (strict && !directRead && !knownBootCall) throw new Error('Untracked environment alias or destructuring');
      }
      if (file === path.join(srcDir, 'boot-requirements.js') && object?.name === 'env' && !node.computed) names.add(node.property.name);
      if (!['MemberExpression', 'OptionalMemberExpression'].includes(object?.type) || object.object?.name !== 'process' || (object.computed ? object.property?.value : object.property?.name) !== 'env') return;
      const name = node.computed ? node.property.value : node.property.name;
      if (typeof name === 'string') names.add(name);
      else if (strict && (file !== path.join(srcDir, 'config.js') || memberPath.getFunctionParent()?.node.id?.name !== 'intEnv')) {
        throw new Error(`Untracked dynamic environment read in ${path.basename(file)}`);
      }
    };
    traverse(ast, {
      MemberExpression: member,
      OptionalMemberExpression: member,
      CallExpression({ node }) {
        if (file === path.join(srcDir, 'config.js') && node.callee?.name === 'intEnv') {
          if (node.arguments[0]?.type !== 'StringLiteral') throw new Error('intEnv must name its environment key');
          names.add(node.arguments[0].value);
        }
      },
    });
  }
  return names;
}

test.each([
  'const { UNTRACKED } = process.env;',
  'const env = process.env; env.UNTRACKED;',
  'unknownHelper(process.env);',
  'process.env[unknownKey];',
  'process["env"][unknownKey];',
  'process?.env[unknownKey];',
])('untracked environment access fails closed: %s', source => {
  const directory = fs.mkdtempSync(path.join(require('node:os').tmpdir(), 'discord-env-contract-'));
  const file = path.join(directory, 'config.js');
  try {
    fs.writeFileSync(file, source);
    expect(() => envReads([file])).toThrow(/Untracked/);
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});

test('every runtime environment read is declared, and every app declaration has a real reader', () => {
  const reads = envReads(javascriptFiles(path.join(__dirname, '../src')));
  const entries = ENV_GROUPS.flatMap(group => manifest.env[group]);
  const declared = new Set(entries);
  expect(declared.size).toBe(entries.length);
  for (const [flag, required] of Object.entries(manifest.env.required_if_enabled)) {
    expect(declared.has(flag)).toBe(true);
    for (const name of required) expect(declared.has(name)).toBe(true);
  }
  for (const sdkName of manifest.env.sdk_optional) declared.delete(sdkName);
  expect([...declared].sort()).toEqual([...reads].sort());
});

test('SDK environment exceptions have real readers in the installed Node SDK', () => {
  const sdkReads = envReads(javascriptFiles(path.dirname(require.resolve('@layervai/qurl/node'))), false);
  expect(manifest.env.sdk_optional).toEqual(['QURL_DEPLOYMENT']);
  for (const name of manifest.env.sdk_optional) expect(sdkReads.has(name)).toBe(true);
});

test('the manifest contains the boot required lists and supported roles', () => {
  const source = parser.parse(fs.readFileSync(path.join(__dirname, '../src/boot-requirements.js'), 'utf8'));
  const lists = {};
  let roles;
  traverse(source, {
    FunctionDeclaration({ node }) {
      if (!['bootRequired', 'prodRequired'].includes(node.id?.name)) return;
      const returned = node.body.body.find(statement => statement.type === 'ReturnStatement')?.argument;
      expect(returned?.type).toBe('ArrayExpression');
      lists[node.id.name] = returned.elements.map(element => element.value);
    },
    VariableDeclarator({ node }) {
      if (node.id?.name === 'VALID_PROCESS_ROLES') roles = node.init.arguments[0].elements.map(element => element.value);
    },
  });
  expect(manifest.env.required).toEqual(expect.arrayContaining(lists.bootRequired));
  expect(manifest.env.required_production).toEqual(expect.arrayContaining(lists.prodRequired));
  expect(manifest.roles).toEqual(roles);
});

test('the image bundles its contract and matches the configured port and health path', () => {
  const dockerfile = fs.readFileSync(path.join(__dirname, '../Dockerfile'), 'utf8');
  expect(dockerfile).toContain('COPY --chown=node:node deploy-manifest.json ./deploy-manifest.json');
  expect(dockerfile).toContain('ENV NODE_ENV=production');
  expect(dockerfile).toContain(`EXPOSE ${manifest.port}`);
  expect(dockerfile).toContain(`http://127.0.0.1:${manifest.port}${manifest.healthcheck.path}`);
  let port;
  traverse(parser.parse(fs.readFileSync(path.join(__dirname, '../src/config.js'), 'utf8')), {
    CallExpression({ node }) {
      if (node.callee?.name === 'intEnv' && node.arguments[0]?.value === 'PORT') port = node.arguments[1]?.value;
    },
  });
  expect(port).toBe(manifest.port);
  for (const file of ['server.js', 'gateway-health.js']) {
    const source = fs.readFileSync(path.join(__dirname, '../src', file), 'utf8');
    expect(source).toContain(`'${manifest.healthcheck.path}'`);
  }
});
