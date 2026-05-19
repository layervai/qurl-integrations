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
// `\b` alone treats `#` as a word boundary and would match
// `consumed` out of `#consumed` — the exact pattern that hid the
// original bug from the first version of this static check.
const ATTR_NAME_PATTERN = /(?<![#:])\b[A-Za-z_][A-Za-z0-9_]*\b/g;

// Tokens that LOOK like attribute names by the regex but are
// expression keywords / function calls — not table column names.
// DDB's expression grammar includes these as reserved syntax, not
// as attributes you'd alias. NOT all-caps in the source — DDB
// expressions are case-sensitive only for attribute names.
const EXPRESSION_KEYWORDS = new Set([
  'set', 'remove', 'add', 'delete',
  'and', 'or', 'not', 'between', 'in',
  'attribute_exists', 'attribute_not_exists', 'attribute_type',
  'begins_with', 'contains', 'size',
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

describe('DDB expression strings — no bare reserved-word attribute names', () => {
  const SRC_DIR = path.join(__dirname, '..', 'src');
  const files = walkJsFiles(SRC_DIR);

  it('the test infrastructure itself works (scan finds real .js files)', () => {
    expect(files.length).toBeGreaterThan(50); // Sanity floor — bot has many src files
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

  it('logs (but does not fail on) dynamic-string expressions that the check cannot validate', () => {
    const dynamic = [];
    for (const f of files) {
      dynamic.push(...findViolations(f).filter(v => v.dynamic));
    }
    // Dynamic expressions are rare in this codebase — if the count
    // grows, that's a signal the static check coverage is shrinking.
    // Hard ceiling so a refactor that converts many literals to
    // template-with-substitutions doesn't quietly defeat the check.
    expect(dynamic.length).toBeLessThan(5);
  });
});
