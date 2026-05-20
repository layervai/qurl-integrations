// Static check: no bare DDB-reserved-word attribute names in any
// UpdateExpression / ConditionExpression / KeyConditionExpression /
// FilterExpression / ProjectionExpression string literal under
// apps/discord/src.
//
// Filed as #468 after PR #467 fixed a real prod-500 from `consumed`
// (a reserved word) appearing bare in recordQurlView's expression.
// aws-sdk-client-mock + the in-memory mock both accept bare reserved
// words; only real DDB validates them. This test catches that class
// of bug at CI time instead of letting it surface as a sandbox 500.

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

// DDB property names whose string value is parsed as an expression.
// PutItem/UpdateItem/Query/Scan/BatchGet all use this name set.
const EXPRESSION_PROP_NAMES = new Set([
  'UpdateExpression',
  'ConditionExpression',
  'KeyConditionExpression',
  'FilterExpression',
  'ProjectionExpression',
]);

// Negative lookbehind on `#` and `:` so the regex skips already-
// aliased names (`#consumed`) and value placeholders (`:c`).
// `\b` alone treats `#` as a word boundary, which means
// `\b\w+\b` would match `consumed` out of `#consumed`. The
// lookbehind closes that gap explicitly.
const ATTR_NAME_PATTERN = /(?<![#:])\b[A-Za-z_][A-Za-z0-9_]*\b/g;

// Tokens that LOOK like attribute names by the regex but are
// expression keywords / function calls — not table column names.
// DDB's expression grammar includes these as reserved syntax, not
// as attributes you'd alias.
//
// `size` is intentionally NOT here even though it's a DDB function.
// It's also a reserved word, and a bare `SET size = :s` would
// silently pass if the keyword exemption ran first. The trade-off:
// a future `size(attr)` function call would false-positive; a
// codebase grep shows zero such usages, so the cost is theoretical.
// A future legit `size(...)` use can either dodge the check via
// `#size_alias` or this list can grow a narrowly-targeted exception.
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

// Evaluate a Babel string-expression node into its concrete string
// value, supporting string-literal, template-literal (no exprs), and
// + concatenation of those. Returns null if the value can't be
// statically resolved (e.g. references a variable) — those get
// flagged separately so the static check fails open rather than
// silently passing.
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
        // Dynamic value — can't statically check. Surface so reviewer knows.
        violations.push({
          file,
          line: propPath.node.loc?.start.line,
          expression: keyName,
          dynamic: true,
          note: 'expression value is not a static string literal — the static check cannot validate it',
        });
        return;
      }
      // Walk the literal expression text + flag any BARE token that
      // matches a reserved word. A peer `ExpressionAttributeNames`
      // declaring `#consumed: 'consumed'` does NOT exempt a bare
      // `consumed` from this check — DDB validates each occurrence
      // independently, and the bare reference is what tripped the
      // original prod-500. The fix is to write `#consumed` in the
      // expression itself, not just to add the alias map.
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

// In-memory fixture self-tests — pin the detection logic itself so
// a future refactor of evalString / the lookbehind regex / the
// EXPRESSION_KEYWORDS set can't silently neuter the check. Each
// fixture writes a synthetic .js source to /tmp, runs findViolations
// against it, and asserts the expected outcome.
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
    // Dynamic expressions can't be statically validated (they
    // interpolate runtime values). Ceiling so a refactor that turns
    // many literals into template-with-substitutions doesn't quietly
    // defeat the check. Today's expected dynamic sites:
    //   - flow-state.js (a few template-with-expression Filter/Update)
    // If this assertion fires, audit each newly-dynamic expression
    // against the reserved-words list manually + raise the ceiling.
    const dynamic = [];
    for (const f of files) {
      dynamic.push(...findViolations(f).filter(v => v.dynamic));
    }
    expect(dynamic.length).toBeLessThan(8);
  });
});
