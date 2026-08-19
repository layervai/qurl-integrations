/**
 * Tests for the two ways scripts/loadtest-standalone.js could run its whole
 * unattended window and produce nothing while looking like it worked.
 *
 * That failure mode is the one this script is worst at showing. A run that
 * mints zero links and exits 0 is outwardly indistinguishable from a run that
 * succeeded, and the two hours it wasted are only noticed afterwards — which
 * is what groups these two otherwise unrelated halves into one file.
 *
 *   - Numeric flags. `--count abc` and `--count -5` both empty every loop a
 *     round runs — the file leg's `i += 10` batches and the location leg's
 *     `i++` alike — so the script holds the target for its full duration
 *     issuing no requests and reports "Total links minted: 0".
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

const { parsePositiveInt, resolveNumericArgs } = require('../scripts/loadtest-standalone');

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

describe('loadtest numeric flags — resolving them from argv', () => {
  it('falls back to the documented defaults when no flag is given', () => {
    expect(resolveNumericArgs([])).toEqual({
      count: 100, durationS: 7200, intervalS: 60, errors: [],
    });
  });

  it('reads each flag independently', () => {
    expect(resolveNumericArgs(['--count', '5', '--duration', '30', '--interval', '2']))
      .toEqual({ count: 5, durationS: 30, intervalS: 2, errors: [] });
  });

  it('ignores flags it does not own', () => {
    expect(resolveNumericArgs(['--location', '--file', '/tmp/x', '--count', '7']).count).toBe(7);
  });

  // The spelling indexOf cannot see. `--count=200` is not the token `--count`,
  // so it read as the flag being ABSENT and quietly ran the default — the same
  // silent fallback the tests below refuse for a dropped value, reached by
  // typing the value the other way round. `--duration=60` is the one that
  // hurts: a run the operator sized at a minute held the target for the
  // default two hours and reported nothing wrong.
  it.each([
    ['count', 'count', 200],
    ['duration', 'durationS', 60],
    ['interval', 'intervalS', 30],
  ])('reads --%s=value as well as the space-separated form', (flag, key, value) => {
    const resolved = resolveNumericArgs([`--${flag}=${value}`]);
    expect(resolved[key]).toBe(value);
    expect(resolved.errors).toEqual([]);
  });

  it('does not let a longer flag match the one it starts with', () => {
    // '--counter=9' starts with '--count', and would set count if the equals
    // form were matched on the flag name rather than on the name plus '='.
    expect(resolveNumericArgs(['--counter=9'])).toEqual({
      count: 100, durationS: 7200, intervalS: 60, errors: [],
    });
  });

  it('takes the space-separated value when a flag is given both ways', () => {
    // Pinned in both argv orders: reading the equals form must not change what
    // an argv that already had a usable space-separated value resolved to.
    expect(resolveNumericArgs(['--count=200', '--count', '7']).count).toBe(7);
    expect(resolveNumericArgs(['--count', '7', '--count=200']).count).toBe(7);
  });

  it('lets the bare token win even when that makes the run fail', () => {
    // It is the space-form TOKEN that wins, not a usable value. `--count`
    // followed by `--count=200` consumes the next argv entry as its raw value
    // and fails the parse on it rather than reaching past it to the 200.
    // Pinned because readFlagToken's doc comment now states this, and a
    // reordering of the two lookups would make that comment quietly false
    // with nothing else here failing.
    const { count, errors } = resolveNumericArgs(['--count', '--count=200']);
    expect(count).toBeNaN();
    expect(errors).toEqual(['--count must be a positive whole number, got "--count=200"']);
  });

  it('splits on the first equals and leaves the rest to the parser', () => {
    // `--count==200` is a value of '=200', not an empty value and not 200.
    // Deciding what counts as malformed belongs to parsePositiveInt, so the
    // token splitter must hand the whole remainder over untouched.
    expect(resolveNumericArgs(['--count==200']).errors[0]).toContain('got "=200"');
    expect(resolveNumericArgs(['--count=2=0']).errors[0]).toContain('got "2=0"');
  });

  // The case getArg cannot express: `--count` as the final token has no value
  // after it, and getArg's `args[idx + 1] || defaultVal` collapses that onto
  // the same 100 as not passing the flag at all. Silently running the default
  // is exactly the "did nothing you asked for, reported success" shape.
  it('refuses a flag left without a value', () => {
    const { count, errors } = resolveNumericArgs(['--count']);
    expect(count).toBeNaN();
    expect(errors).toEqual(['--count was given no value (omit it to use the default of 100)']);
  });

  it('refuses the equals form with the value left off', () => {
    // '--count=' and a trailing '--count' are the same dropped value, so they
    // report the same way. Distinct from `--count ''` just above, which is a
    // value that was passed and is unusable rather than one that went missing.
    const { count, errors } = resolveNumericArgs(['--count=']);
    expect(count).toBeNaN();
    expect(errors).toEqual(['--count was given no value (omit it to use the default of 100)']);
  });

  it('validates an equals-form value through the same parser', () => {
    // Otherwise the new spelling would be a way around parsePositiveInt rather
    // than a second way into it.
    expect(resolveNumericArgs(['--count=abc']).errors[0]).toContain('got "abc"');
    expect(resolveNumericArgs(['--duration=0']).errors[0]).toContain('greater than zero');
    expect(resolveNumericArgs(['--interval=-1']).errors[0]).toContain('got "-1"');
  });

  it('refuses the following flag when the value was forgotten', () => {
    // Consumes '--location' as the value AND leaves the location leg on, so
    // the run would differ from the one typed in two ways at once.
    const { count, errors } = resolveNumericArgs(['--count', '--location']);
    expect(count).toBeNaN();
    expect(errors[0]).toContain('got "--location"');
  });

  it('names every bad flag in one pass', () => {
    // All three print before the exit, so a run with three typos is fixed in
    // one edit rather than three.
    const { errors } = resolveNumericArgs(['--count', 'abc', '--duration', '-1', '--interval', '0']);
    expect(errors).toHaveLength(3);
    expect(errors.map(e => e.split(' ')[0])).toEqual(['--count', '--duration', '--interval']);
  });

  it('leaves the valid flags usable when another is wrong', () => {
    // The errors are fatal in main(), but the resolver must not let one bad
    // flag corrupt the others' values.
    const { count, durationS, errors } = resolveNumericArgs(['--count', '50', '--duration', 'nope']);
    expect(count).toBe(50);
    expect(durationS).toBeNaN();
    expect(errors).toHaveLength(1);
  });
});

describe('loadtest script — static checks on call sites no test can reach', () => {
  const parseFile = (...segments) =>
    parser.parse(fs.readFileSync(path.join(__dirname, '..', ...segments), 'utf8'), {
      sourceType: 'unambiguous',
    });

  const ast = parseFile('scripts', 'loadtest-standalone.js');

  // Matches a member expression on its property name too, so `client.fetch()`
  // counts as a fetch. Wider than strictly needed, in the direction these
  // checks want to err.
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

  it('parses none of its own numeric arguments', () => {
    // The regression this file exists for is a call site quietly reverted to
    // `const COUNT = parseInt(getArg('count', '100'))`. That leaves
    // parsePositiveInt exported and every unit test above green while the
    // constants the loops actually read go back to being unvalidated, so the
    // ban has to be on parseInt itself, not on the parser's internals.
    expect(callsNamed('parseInt')).toHaveLength(0);
  });

  it('resolves its numeric flags in one place', () => {
    // Fails closed if a fourth numeric flag is added with its own ad-hoc
    // parse instead of going through the resolver.
    expect(callsNamed('resolveNumericArgs')).toHaveLength(1);
  });

  it('uploads through reUploadBuffer', () => {
    // Fails closed if the call disappears or a second one appears unreviewed.
    expect(callsNamed('reUploadBuffer')).toHaveLength(1);
  });

  it('passes the first three parameters positionally and omits the last two', () => {
    // The call site is positional, so a reorder in connector.js would change
    // what this script uploads with nothing here to notice. Read from the
    // real signature rather than hard-coded, so a rename surfaces as a
    // failure instead of leaving this asserting a stale contract.
    //
    // The list is pinned whole, not just its first three: omitting the last
    // two is only safe because of what those two specifically are — apiKey
    // falls back to config.QURL_API_KEY, and appendViewerTtl drops
    // viewerTtlSeconds unless it is positive-finite. A sixth parameter, or a
    // different one in fourth place, needs that reasoning done again.
    //
    // Matched across node shapes on purpose. This check exists to fail on a
    // REORDER, not on a refactor that preserves the order: moving to an arrow
    // or function expression, or giving a parameter a default, changes the
    // AST without changing what position three means.
    let params = null;
    const paramName = (param) =>
      (param.type === 'AssignmentPattern' ? param.left.name : param.name);
    const capture = (fn) => { params = fn.params.map(paramName); };
    traverse(parseFile('src', 'connector.js'), {
      FunctionDeclaration(p) {
        if (p.node.id?.name === 'reUploadBuffer') capture(p.node);
      },
      VariableDeclarator(p) {
        if (p.node.id.type !== 'Identifier' || p.node.id.name !== 'reUploadBuffer') return;
        const init = p.node.init;
        if (init?.type === 'ArrowFunctionExpression' || init?.type === 'FunctionExpression') {
          capture(init);
        }
      },
    });
    expect(params).toEqual([
      'fileBuffer', 'filename', 'contentType', 'apiKey', 'viewerTtlSeconds',
    ]);
    expect(callsNamed('reUploadBuffer')[0].arguments).toHaveLength(3);
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
