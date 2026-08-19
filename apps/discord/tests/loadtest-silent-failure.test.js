/**
 * Tests for the ways scripts/loadtest-standalone.js could run its whole
 * unattended window and produce nothing — or produce the wrong thing — while
 * looking like it worked.
 *
 * That failure mode is the one this script is worst at showing. A run that
 * mints zero links and exits 0 is outwardly indistinguishable from a run that
 * succeeded, and the two hours it wasted are only noticed afterwards — which
 * is what groups these otherwise unrelated halves into one file.
 *
 *   - Flag reading. One reader now serves every value-taking flag, because
 *     the mistakes it has to catch are the same for all of them: a missing,
 *     empty or flag-shaped value silently became the DEFAULT, so the run did
 *     something real that nobody asked for.
 *   - Numeric flags. `--count abc` and `--count -5` both empty every loop a
 *     round runs — the file leg's `i += 10` batches and the location leg's
 *     `i++` alike — so the script holds the target for its full duration
 *     issuing no requests and reports "Total links minted: 0".
 *   - The --file flag. Defaulting sends a generated 1MB payload instead of
 *     the operator's, and a path that cannot be read used to surface as an
 *     ENOENT thrown from inside the first round — after the smoke test had
 *     already minted a real resource.
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
const os = require('os');
const path = require('path');
const parser = require('@babel/parser');
const traverseModule = require('@babel/traverse');

const traverse = traverseModule.default || traverseModule;

const {
  readFlag, parsePositiveInt, resolveNumericArgs, resolveFileArg, checkUploadFile,
} = require('../scripts/loadtest-standalone');

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
    // readFlag now refuses a flag-shaped value before this is reached, so on
    // the resolver's path this row is belt-and-braces. It stays because
    // parsePositiveInt is exported and callable on its own, and because a
    // number parser that accepts '--location' is wrong regardless of caller.
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

  // The case getArg cannot express: `--count` as the final token has no value
  // after it, and getArg's `args[idx + 1] || defaultVal` collapses that onto
  // the same 100 as not passing the flag at all. Silently running the default
  // is exactly the "did nothing you asked for, reported success" shape.
  it('refuses a flag left without a value', () => {
    const { count, errors } = resolveNumericArgs(['--count']);
    expect(count).toBeNaN();
    expect(errors).toEqual(['--count was given no value (omit it to use the default of 100)']);
  });

  it('refuses an explicitly empty value rather than defaulting', () => {
    const { count, errors } = resolveNumericArgs(['--count', '']);
    expect(count).toBeNaN();
    expect(errors[0]).toContain('got ""');
  });

  it('refuses the following flag when the value was forgotten', () => {
    // Used to consume '--location' as the value AND leave the location leg
    // on, so the run differed from the one typed in two ways at once.
    //
    // The message now comes from readFlag rather than parsePositiveInt, and
    // says what actually went wrong: the value was forgotten. Reporting it as
    // a bad NUMBER ("got \"--location\"") sent the operator looking at the
    // count they typed correctly instead of the value they omitted.
    const { count, errors } = resolveNumericArgs(['--count', '--location']);
    expect(count).toBeNaN();
    expect(errors[0]).toBe('--count was given no value — the next argument is the flag --location');
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

describe('loadtest flag reading — the argv shapes that used to become the default', () => {
  // readFlag is the whole point of this change: --file and the numeric flags
  // used to disagree about what a malformed command line meant, because each
  // had its own reader. These cases are asserted once, here, and every
  // value-taking flag inherits them.

  it('hands back the default when the flag is absent', () => {
    // The one reading that MAY default, and the reason the others must not:
    // if everything defaults, the operator cannot tell the two apart.
    expect(readFlag([], 'file', null)).toEqual({ value: null });
    expect(readFlag(['--location'], 'file', null)).toEqual({ value: null });
  });

  it('reads the separated and inline spellings the same way', () => {
    expect(readFlag(['--file', '/tmp/x'], 'file', null)).toEqual({ value: '/tmp/x' });
    // `--file=/tmp/x` was invisible to an indexOf('--file'), so the flag read
    // as absent and the run generated its own payload — the silent default,
    // reached from a spelling most CLIs accept.
    expect(readFlag(['--file=/tmp/x'], 'file', null)).toEqual({ value: '/tmp/x' });
  });

  it('refuses a flag left as the final token', () => {
    expect(readFlag(['--file'], 'file', null, 'an auto-generated 1MB test file')).toEqual({
      error: '--file was given no value (omit it to use the default of an auto-generated 1MB test file)',
    });
  });

  it('names the default in the words the caller chose', () => {
    // --file's default is not a path, so echoing the value (null) would tell
    // the operator nothing. The numeric flags pass no label and get the
    // value, which is already what they want to say.
    expect(readFlag(['--count'], 'count', '100').error).toContain('the default of 100');
  });

  it('refuses a value that is itself a flag', () => {
    // Positional consumption is what made this silent: the next flag became
    // the value AND stopped being a flag, so two things changed at once.
    expect(readFlag(['--file', '--count', '5'], 'file', null)).toEqual({
      error: '--file was given no value — the next argument is the flag --count',
    });
  });

  it('leaves a single-dash value alone', () => {
    // Only `--` marks a flag here. A lone `-` has to reach the value
    // validators intact, or `--count -5` would report a MISSING value and
    // send the operator looking for a typo they did not make.
    expect(readFlag(['--count', '-5'], 'count', '100')).toEqual({ value: '-5' });
    expect(readFlag(['--file', '-weird'], 'file', null)).toEqual({ value: '-weird' });
  });

  it('passes an empty value through rather than defaulting', () => {
    // Both spellings of empty reach the caller as '', which every validator
    // rejects. Defaulting here instead is the original bug: `--file ""` ran a
    // full window against a payload the operator did not choose.
    expect(readFlag(['--file', ''], 'file', null)).toEqual({ value: '' });
    expect(readFlag(['--file='], 'file', null)).toEqual({ value: '' });
  });

  it('takes the last occurrence when a flag is repeated', () => {
    // indexOf took the FIRST, so appending `--count 5` to a recalled command
    // line left the earlier value in force — the run silently ignored the
    // edit that was just made to it.
    expect(readFlag(['--count', '1', '--count', '2'], 'count', '100')).toEqual({ value: '2' });
    expect(readFlag(['--file', 'a', '--file=b'], 'file', null)).toEqual({ value: 'b' });
    expect(readFlag(['--file=a', '--file', 'b'], 'file', null)).toEqual({ value: 'b' });
  });

  it('reports the last occurrence being malformed even when an earlier one was fine', () => {
    // Last-wins has to apply to the refusal too. Falling back to the earlier
    // good value would run the command the operator had already replaced.
    expect(readFlag(['--file', 'a', '--file'], 'file', null).error).toContain('given no value');
  });

  it('does not match a longer flag that starts with the same letters', () => {
    // The inline form is matched on the `--file=` prefix, so the guard that
    // keeps `--filename` out of it is that prefix carrying the `=`.
    expect(readFlag(['--filename', 'x'], 'file', null)).toEqual({ value: null });
    expect(readFlag(['--file-count', '3'], 'file', null)).toEqual({ value: null });
  });
});

describe('loadtest --file — the flag whose default uploads something else', () => {
  it('reports no path and no error when the flag is absent', () => {
    // null is what runRound reads as "generate a 1MB payload". It has to be
    // reachable ONLY from this case.
    expect(resolveFileArg([])).toEqual({ filePath: null, errors: [] });
    expect(resolveFileArg(['--count', '5', '--location'])).toEqual({ filePath: null, errors: [] });
  });

  it('takes the path in either spelling', () => {
    expect(resolveFileArg(['--file', '/tmp/payload.bin'])).toEqual({
      filePath: '/tmp/payload.bin', errors: [],
    });
    expect(resolveFileArg(['--file=/tmp/payload.bin'])).toEqual({
      filePath: '/tmp/payload.bin', errors: [],
    });
  });

  // The reported bug, in all three of its spellings. Each one used to leave
  // FILE_PATH null and run a full window against a generated 1MB file.
  it.each([
    ['the flag as the final token', ['--file']],
    ['an explicitly empty value', ['--file', '']],
    ['an empty inline value', ['--file=']],
    ['a whitespace-only value', ['--file', '   ']],
    ['the next flag consumed as the path', ['--file', '--location']],
  ])('refuses %s instead of generating a payload', (_label, argv) => {
    const { filePath, errors } = resolveFileArg(argv);
    expect(filePath).toBeNull();
    expect(errors).toHaveLength(1);
    expect(errors[0]).toContain('--file');
  });

  it('quotes a whitespace-only path so it is visible as one', () => {
    // Unquoted, `got    ` reads as a truncated message rather than as the
    // three spaces that were actually typed.
    expect(resolveFileArg(['--file', '   ']).errors[0]).toContain('got "   "');
  });

  it('keeps a path that legitimately carries surrounding spaces', () => {
    // Whitespace is checked but not stripped: trimming would resolve a real
    // filename to a different one, which is a worse failure than the typo it
    // would be papering over.
    expect(resolveFileArg(['--file', ' spaced.bin ']).filePath).toBe(' spaced.bin ');
  });

  it('does not take a later flag as its value', () => {
    expect(resolveFileArg(['--file', '/tmp/x', '--count', '5']).filePath).toBe('/tmp/x');
  });
});

describe('loadtest --file — proving it is readable before anything is minted', () => {
  let dir;
  let readable;

  beforeAll(() => {
    dir = fs.mkdtempSync(path.join(os.tmpdir(), 'loadtest-file-'));
    readable = path.join(dir, 'payload.bin');
    fs.writeFileSync(readable, 'x');
  });

  afterAll(() => {
    fs.rmSync(dir, { recursive: true, force: true });
  });

  it('accepts a readable regular file', () => {
    expect(checkUploadFile(readable)).toBeNull();
  });

  it('reports a path that does not exist', () => {
    // The case that used to throw ENOENT out of runRound, one smoke-test
    // resource and one target-guard verdict too late.
    expect(checkUploadFile(path.join(dir, 'missing.bin'))).toContain('cannot be read');
  });

  it('reports a directory rather than letting readFileSync throw EISDIR', () => {
    // existsSync would call this fine, which is why the check stats instead.
    expect(checkUploadFile(dir)).toContain('is not a regular file');
  });

  it('names the flag in every message', () => {
    // These print alongside the numeric flags' errors under one FATAL: pass,
    // so each has to say which flag it is about.
    expect(checkUploadFile(path.join(dir, 'missing.bin'))).toMatch(/^--file /);
    expect(checkUploadFile(dir)).toMatch(/^--file /);
  });

  it('reports a file that exists but cannot be read', () => {
    // Existence is not readability — a file owned by another user is the
    // realistic way this bites, and statSync succeeds on it.
    //
    // Skipped as root, which bypasses the permission bits entirely and would
    // make this assert the opposite of what it says. Guarded rather than
    // deleted: the check it covers is the one that is only reachable as a
    // non-root operator, which is how this script is actually run.
    if (typeof process.getuid === 'function' && process.getuid() === 0) return;
    const locked = path.join(dir, 'locked.bin');
    fs.writeFileSync(locked, 'x');
    fs.chmodSync(locked, 0o000);
    try {
      expect(checkUploadFile(locked)).toContain('is not readable');
    } finally {
      fs.chmodSync(locked, 0o600);
    }
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

  it('reads every value-taking flag through the one shared reader', () => {
    // The regression this guards is a fourth flag added with its own inline
    // `argv.indexOf(...)`, which is exactly how the file got three parsers
    // with three different answers for `--flag` as the final token.
    //
    // Two call sites today: resolveNumericArgs (which serves all three
    // numeric flags) and resolveFileArg. A new value-taking flag should raise
    // this to three DELIBERATELY, as part of routing it through readFlag —
    // not discover afterwards that it defaulted silently.
    expect(callsNamed('readFlag')).toHaveLength(2);
  });

  it('keeps no permissive reader alongside it', () => {
    // getArg's `args[idx + 1] || defaultVal` is the defect itself, not merely
    // a style: it cannot express "flag present, value missing". Re-adding it
    // for one new flag would reinstate the silent default for that flag while
    // every test above stays green.
    expect(callsNamed('getArg')).toHaveLength(0);
  });

  it('resolves and checks the upload file exactly once each', () => {
    // checkUploadFile is unreachable from main() in a test, so nothing else
    // would notice the preflight call being dropped — and dropping it puts
    // the failure back inside runRound, after the smoke test has minted a
    // resource, which is the whole point of adding it.
    expect(callsNamed('resolveFileArg')).toHaveLength(1);
    expect(callsNamed('checkUploadFile')).toHaveLength(1);
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
