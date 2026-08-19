/**
 * Tests for the two ways scripts/loadtest-standalone.js could run its whole
 * unattended window and produce nothing while looking like it worked.
 *
 * That failure mode is the one this script is worst at showing. A run that
 * mints zero links and exits 0 is outwardly indistinguishable from a run that
 * succeeded, and the two hours it wasted are only noticed afterwards — which
 * is what groups these two otherwise unrelated halves into one file.
 *
 *   - Numeric flags. `--count abc` and `--count -5` both make
 *     `for (let i = 0; i < COUNT; i += 10)` skip entirely, so the script
 *     holds the target for its full duration issuing no requests and reports
 *     "Total links minted: 0".
 *   - The upload call. Hand-rolled, it had no timeout (a stalled connector
 *     hangs the round forever) and no response-shape checks (a 200 with no
 *     resource_id blamed the mint leg for an upload fault).
 *
 * The upload half is a STATIC check because runRound is unreachable from a
 * test: it is not exported, its only caller main() is behind
 * `require.main === module`, and scripts/ sits outside collectCoverageFrom,
 * so neither jest nor the coverage gate can see it. eslint extends only
 * eslint:recommended, so it cannot see it either. Parsing the source is what
 * is left — same approach as tests/ddb-reserved-words-static.test.js, and the
 * sibling of the mintLinks call-shape check #1168 added to
 * tests/loadtest-target-guard.test.js. The two stay in separate files because
 * that one's scaffolding is scoped inside its own describe.
 */

const fs = require('fs');
const path = require('path');
const parser = require('@babel/parser');
const traverseModule = require('@babel/traverse');

const traverse = traverseModule.default || traverseModule;

const { parsePositiveInt } = require('../scripts/loadtest-standalone');

describe('loadtest numeric flags — values that would run the loops zero times', () => {
  it.each([
    ['a positive count', 'count', '100', 100],
    ['the smallest useful value', 'count', '1', 1],
    ['the default duration', 'duration', '7200', 7200],
    ['surrounding whitespace', 'interval', '  60  ', 60],
  ])('accepts %s', (_label, flag, raw, expected) => {
    expect(parsePositiveInt(flag, raw)).toEqual({ value: expected });
  });

  // Every row here is a value parseInt accepts, or converts into one that
  // silently disables the run. The second column is what parseInt actually
  // returns for it — asserted below, so this table cannot rot into a list of
  // inputs that were never dangerous in the first place.
  const PARSE_INT_TRAPS = [
    ['non-numeric', 'abc', NaN],
    ['a negative count', '-5', -5],
    ['zero', '0', 0],
    // No radix: parseInt reads the 0x prefix and returns 100, so `--count
    // 0x64` quietly runs a different test than the one that was typed.
    ['hex notation', '0x64', 100],
    // Exponent notation truncates at the 'e': a billion becomes one.
    ['exponent notation', '1e9', 1],
    ['a trailing unit', '100ms', 100],
    // getArg returns the next argv token verbatim, so `--count --location`
    // lands the following flag here as the value — and turns on the location
    // leg at the same time.
    ['the next flag as the value', '--location', NaN],
    ['a fractional value', '1.5', 1],
    ['an empty value', '', NaN],
  ];

  it.each(PARSE_INT_TRAPS)('rejects %s', (_label, raw) => {
    const result = parsePositiveInt('count', raw);
    expect(result.value).toBeUndefined();
    expect(result.error).toMatch(/^--count /);
  });

  it('rejects inputs parseInt would have accepted or mangled', () => {
    // Pins that this is a real parse and not a wrapper around parseInt: for
    // every row above, parseInt's answer is either a usable-looking number
    // that is not the number typed, or the NaN/negative that empties the
    // loop.
    for (const [label, raw, parseIntResult] of PARSE_INT_TRAPS) {
      expect([label, parseInt(raw)]).toEqual([label, parseIntResult]);
    }
  });

  it('refuses a value too large to represent exactly', () => {
    // 2^53 and beyond: Number(text) rounds, so the count the loops run on
    // would differ from the count echoed back to the operator.
    expect(parsePositiveInt('count', '99999999999999999999').error).toMatch(/too large/);
    expect(parsePositiveInt('count', String(Number.MAX_SAFE_INTEGER))).toEqual({
      value: Number.MAX_SAFE_INTEGER,
    });
  });

  it('names the flag that was wrong', () => {
    // Three numeric flags are validated together and all errors print before
    // the exit, so each message has to identify its own flag.
    expect(parsePositiveInt('duration', 'abc').error).toContain('--duration');
    expect(parsePositiveInt('interval', '-1').error).toContain('--interval');
  });

  it('quotes the offending value so an empty one is visible', () => {
    // An unquoted empty string renders as `got ` and reads like a truncated
    // message rather than the empty value it is.
    expect(parsePositiveInt('count', '').error).toContain('got ""');
    expect(parsePositiveInt('count', '--location').error).toContain('got "--location"');
  });
});

describe('loadtest upload — goes through the connector client, not a raw fetch', () => {
  const ast = parser.parse(
    fs.readFileSync(path.join(__dirname, '..', 'scripts', 'loadtest-standalone.js'), 'utf8'),
    { sourceType: 'unambiguous' },
  );

  const calleeName = (node) => {
    const callee = node.callee;
    if (callee.type === 'Identifier') return callee.name;
    if (callee.type === 'MemberExpression' && callee.property.type === 'Identifier') {
      return callee.property.name;
    }
    return null;
  };

  const callsNamed = (name) => {
    const found = [];
    traverse(ast, {
      CallExpression(p) {
        if (calleeName(p.node) === name) found.push(p.node);
      },
    });
    return found;
  };

  it('hand-rolls no HTTP call of its own', () => {
    // The three guards that went missing — AbortSignal.timeout(60000), the
    // `success` check and the `resource_id` check — live in connector.js and
    // cannot be inherited by a local fetch. So the rule is not "re-add them
    // here" but "do not open a second door": any fetch in this file is a copy
    // of a request the connector client already owns.
    expect(callsNamed('fetch')).toHaveLength(0);
  });

  it('uploads through reUploadBuffer', () => {
    // Fails closed if the call disappears or a second one appears unreviewed.
    expect(callsNamed('reUploadBuffer')).toHaveLength(1);
  });

  it('imports reUploadBuffer from the connector client', () => {
    // A local helper of the same name would satisfy the checks above while
    // reinstating exactly the unguarded request they exist to prevent.
    const imported = [];
    traverse(ast, {
      VariableDeclarator(p) {
        const init = p.node.init;
        if (!init || init.type !== 'CallExpression' || calleeName(init) !== 'require') return;
        const [source] = init.arguments;
        if (source?.type !== 'StringLiteral' || source.value !== '../src/connector') return;
        if (p.node.id.type !== 'ObjectPattern') return;
        for (const prop of p.node.id.properties) {
          if (prop.type === 'ObjectProperty') imported.push(prop.key.name);
        }
      },
    });
    expect(imported).toContain('reUploadBuffer');
  });
});
