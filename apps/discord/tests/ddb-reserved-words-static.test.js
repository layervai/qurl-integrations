
const fs = require('fs');
const path = require('path');
const parser = require('@babel/parser');
const traverseModule = require('@babel/traverse');
const traverse = traverseModule.default || traverseModule;

const RESERVED_WORDS = (() => {
  const json = JSON.parse(
    fs.readFileSync(path.join(__dirname, 'helpers', 'ddb-reserved-words.json'), 'utf8'),
  );
  return new Set(json.words.map(w => w.toLowerCase()));
})();

const EXPRESSION_PROP_NAMES = new Set([
  'UpdateExpression',
  'ConditionExpression',
  'KeyConditionExpression',
  'FilterExpression',
  'ProjectionExpression',
]);

const ATTR_NAME_PATTERN = /(?<![#:])\b[A-Za-z_][A-Za-z0-9_]*\b/g;

const EXPRESSION_KEYWORDS = new Set([
  'set', 'remove', 'add', 'delete',
  'and', 'or', 'not', 'between', 'in',
  'attribute_exists', 'attribute_not_exists', 'attribute_type',
  'begins_with', 'contains',
  'if_not_exists', 'list_append',
]);

function walkJsFiles(dir, out = []) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (entry.name === 'node_modules' || entry.name.startsWith('.')) continue;
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) walkJsFiles(full, out);
    else if (entry.name.endsWith('.js')) out.push(full);
  }
  return out;
}

function evalString(node) {
  if (!node) return null;
  if (node.type === 'StringLiteral') return node.value;
  if (node.type === 'TemplateLiteral' && node.expressions.length === 0) {
    return node.quasis.map(q => q.value.cooked).join('');
  }
  if (node.type === 'BinaryExpression' && node.operator === '+') {
    const l = evalString(node.left);
    const r = evalString(node.right);
    if (l === null || r === null) return null;
    return l + r;
  }
  return null;
}

function findViolations(file) {
  const src = fs.readFileSync(file, 'utf8');
  let ast;
  try {
    ast = parser.parse(src, { sourceType: 'unambiguous', allowReturnOutsideFunction: true });
  } catch {
    return []; // Skip unparseable files — Jest itself will error on syntax bugs.
  }
  const violations = [];
  traverse(ast, {
    Property(propPath) {
      const key = propPath.node.key;
      const keyName = key && (key.name || key.value);
      if (!EXPRESSION_PROP_NAMES.has(keyName)) return;
      const value = evalString(propPath.node.value);
      if (value === null) {
        violations.push({
          file,
          line: propPath.node.loc?.start.line,
          expression: keyName,
          dynamic: true,
          note: 'expression value is not a static string literal — the static check cannot validate it',
        });
        return;
      }
      const matches = value.match(ATTR_NAME_PATTERN) || [];
      for (const tok of matches) {
        const lower = tok.toLowerCase();
        if (EXPRESSION_KEYWORDS.has(lower)) continue;
        if (!RESERVED_WORDS.has(lower)) continue;
        violations.push({
          file,
          line: propPath.node.loc?.start.line,
          expression: keyName,
          word: tok,
          fix: `Replace bare '${tok}' with '#${tok}' in the expression + add { '#${tok}': '${tok}' } to ExpressionAttributeNames`,
        });
      }
    },
  });
  return violations;
}

function withFixture(source, fn) {
  const fixturePath = path.join(require('os').tmpdir(), `ddb-static-fixture-${process.pid}-${Date.now()}-${Math.random().toString(36).slice(2)}.js`);
  fs.writeFileSync(fixturePath, source);
  try { return fn(fixturePath); } finally { fs.unlinkSync(fixturePath); }
}

describe('DDB reserved-words static check — detection logic', () => {
  it('flags a bare reserved word in UpdateExpression', () => {
    withFixture(`
      module.exports = {
        params: { UpdateExpression: 'SET consumed = :c' },
      };
    `, file => {
      const v = findViolations(file);
      expect(v.filter(x => !x.dynamic)).toEqual([expect.objectContaining({
        expression: 'UpdateExpression',
        word: 'consumed',
      })]);
    });
  });

  it('does NOT flag an aliased reserved word (#consumed)', () => {
    withFixture(`
      module.exports = {
        params: {
          UpdateExpression: 'SET #consumed = :c',
          ExpressionAttributeNames: { '#consumed': 'consumed' },
        },
      };
    `, file => {
      const v = findViolations(file).filter(x => !x.dynamic);
      expect(v).toEqual([]);
    });
  });

  it('flags a reserved word in ConditionExpression assembled via + concat', () => {
    withFixture(`
      module.exports = {
        params: {
          ConditionExpression:
            'attribute_not_exists(x) OR (' +
            'x <> :y AND consumed = :c)',
        },
      };
    `, file => {
      const v = findViolations(file).filter(x => !x.dynamic);
      expect(v.length).toBe(1);
      expect(v[0].word).toBe('consumed');
    });
  });

  it('marks a dynamic expression (template-with-substitution) as dynamic, not as a violation', () => {
    withFixture(`
      const col = 'foo';
      module.exports = {
        params: { FilterExpression: \`\${col} = :v\` },
      };
    `, file => {
      const v = findViolations(file);
      expect(v.filter(x => !x.dynamic)).toEqual([]);
      expect(v.filter(x => x.dynamic).length).toBe(1);
    });
  });

  it('treats `size` as a reserved word (catches the original bug class)', () => {
    withFixture(`
      module.exports = {
        params: { UpdateExpression: 'SET size = :s' },
      };
    `, file => {
      const v = findViolations(file).filter(x => !x.dynamic);
      expect(v.length).toBe(1);
      expect(v[0].word).toBe('size');
    });
  });
});

describe('DDB reserved-words static check — full src scan', () => {
  const SRC_DIR = path.join(__dirname, '..', 'src');
  const files = walkJsFiles(SRC_DIR);

  it('the walker finds .js files (otherwise the rest of this suite is a no-op)', () => {
    expect(files.length).toBeGreaterThan(0);
  });

  it('every UpdateExpression / ConditionExpression / etc. is free of bare reserved words', () => {
    const allViolations = [];
    for (const f of files) {
      allViolations.push(...findViolations(f).filter(v => !v.dynamic));
    }
    if (allViolations.length > 0) {
      const formatted = allViolations.map(v =>
        `  ${path.relative(SRC_DIR, v.file)}:${v.line} ${v.expression}: bare reserved word '${v.word}'\n    Fix: ${v.fix}`,
      ).join('\n');
      throw new Error(`DDB reserved-word violations found:\n${formatted}`);
    }
  });

  it('the ceiling on non-static-literal expressions prevents silent coverage shrink', () => {
    const dynamic = [];
    for (const f of files) {
      dynamic.push(...findViolations(f).filter(v => v.dynamic));
    }
    expect(dynamic.length).toBeLessThan(8);
  });
});
