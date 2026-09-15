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
 *   - Unknown arguments. Those readers are pull-based, so a token that matched
 *     NO flag was seen by none of them: `--locatoin` and `-count 5` read as
 *     "flag absent" and ran the full window on the defaults, and
 *     `--location false` turned the leg ON while the operator asked for it
 *     off. One rule refuses all of them, and the flag table it reads is the
 *     thing this half mostly guards.
 *   - Numeric flags. `--count abc` and `--count -5` both empty every loop a
 *     round runs — the file leg's batch plan and the location leg's
 *     `i++` alike — so the script holds the target for its full duration
 *     issuing no requests and reports "Total links minted: 0".
 *   - The --file flag. Defaulting sends a generated 1MB payload instead of
 *     the operator's, and a path that cannot be read used to surface as an
 *     ENOENT thrown from inside the first round — after the smoke test had
 *     already minted a real resource.
 *   - The upload call. Hand-rolled, it had no timeout (a stalled connector
 *     hangs the round forever) and no response-shape checks (a 200 with no
 *     resource_id blamed the mint leg for an upload fault).
 *   - The generated payload. It used to be written to /tmp and read straight
 *     back once per round, unlinked by nothing — ~120 files and ~120MB for a
 *     default window. A round that throws is logged and the loop carries on,
 *     so a /tmp filled by the run's own litter turns every remaining round
 *     into a `Round N FAILED` line: the window still runs to completion and
 *     still measures nothing.
 *
 * The upload and payload halves are STATIC checks, for a reason that survived
 * #1173 exporting runRound: what they assert is the SHAPE OF THE SOURCE, and
 * calling a function cannot see that. Running a round proves the bytes that
 * reach reUploadBuffer; it cannot distinguish a memoized buffer from one
 * allocated fresh inside the round, and it can say nothing at all about
 * whether some other line in the file writes a scratch file. Coverage would
 * not have caught either anyway — scripts/ sits outside collectCoverageFrom,
 * and eslint extends only eslint:recommended, so neither can see this file's
 * internals. Parsing the source is what is left — same approach as
 * tests/ddb-reserved-words-static.test.js, and the sibling of the mintLinks
 * call-shape check #1168 added to tests/loadtest-target-guard.test.js. The two
 * stay in separate files because that one's scaffolding is scoped inside its
 * own describe.
 *
 * runRound itself IS reachable now (#1173 exports it for
 * tests/loadtest-round-accounting.test.js). Anything expressible as "run a
 * round and assert on the result" belongs there, not here.
 */

const fs = require('fs');
const os = require('os');
const path = require('path');
const parser = require('@babel/parser');
const traverseModule = require('@babel/traverse');

const traverse = traverseModule.default || traverseModule;

const {
  FLAGS, flagSpec, readFlag, readBooleanFlag, resolveBooleanArgs,
  parsePositiveInt, resolveNumericArgs, resolveFileArg, resolveUnknownArgs,
  checkUploadFile, resolveArgErrors, resolveGuardInputs, DEFAULT_MAX_FAIL_RATE_PCT,
  generateTestPayload,
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

  const PARSE_INT_TRAPS = [
    ['non-numeric', 'abc', NaN],
    ['a negative count', '-5', -5],
    ['zero', '0', 0],
    ['hex notation', '0x64', 100],
    ['exponent notation', '1e9', 1],
    ['a trailing unit', '100ms', 100],
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
    for (const [label, raw, parseIntResult] of PARSE_INT_TRAPS) {
      expect([label, parseInt(raw)]).toEqual([label, parseIntResult]);
    }
  });

  it('refuses a value too large to represent exactly', () => {
    expect(parsePositiveInt('count', '99999999999999999999').error).toMatch(/too large/);
    expect(parsePositiveInt('count', String(Number.MAX_SAFE_INTEGER))).toEqual({
      value: Number.MAX_SAFE_INTEGER,
    });
  });

  it('names the flag that was wrong', () => {
    expect(parsePositiveInt('duration', 'abc').error).toContain('--duration');
    expect(parsePositiveInt('interval', '-1').error).toContain('--interval');
  });

  it('quotes the offending value so an empty one is visible', () => {
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

  it.each([
    ['count', 'count', 200],
    ['duration', 'durationS', 60],
    ['interval', 'intervalS', 30],
  ])('resolves --%s=value, not only the space-separated form', (flag, key, value) => {
    const resolved = resolveNumericArgs([`--${flag}=${value}`]);
    expect(resolved[key]).toBe(value);
    expect(resolved.errors).toEqual([]);
  });

  it('takes the last value when a flag is repeated in either spelling', () => {
    expect(resolveNumericArgs(['--count=5', '--count=9']).count).toBe(9);
    expect(resolveNumericArgs(['--count', '5', '--count=9']).count).toBe(9);
    expect(resolveNumericArgs(['--count=5', '--count', '9']).count).toBe(9);
  });

  it('lets a later inline value override an earlier space-separated one', () => {
    expect(resolveNumericArgs(['--count', '7', '--count=200']).count).toBe(200);
    expect(resolveNumericArgs(['--count=200', '--count', '7']).count).toBe(7);
  });

  it('does not let a longer flag match the one it starts with', () => {
    expect(resolveNumericArgs(['--counter=9'])).toEqual({
      count: 100, durationS: 7200, intervalS: 60, errors: [],
    });
  });

  it('reports an empty value as empty, whichever spelling delivered it', () => {
    const inline = resolveNumericArgs(['--count=']).errors;
    const separated = resolveNumericArgs(['--count', '']).errors;
    expect(inline).toEqual(separated);
    expect(inline[0]).toContain('got ""');
    expect(resolveNumericArgs(['--count']).errors[0]).toContain('was given no value');
  });

  it('splits on the first equals and leaves the rest to the parser', () => {
    expect(resolveNumericArgs(['--count==200']).errors[0]).toContain('got "=200"');
    expect(resolveNumericArgs(['--count=2=0']).errors[0]).toContain('got "2=0"');
  });

  it('resolves a bare token followed by an inline value to the inline one', () => {
    expect(resolveNumericArgs(['--count', '--count=200'])).toEqual({
      count: 200, durationS: 7200, intervalS: 60, errors: [],
    });
  });

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

  it('validates an equals-form value through the same parser', () => {
    expect(resolveNumericArgs(['--count=abc']).errors[0]).toContain('got "abc"');
    expect(resolveNumericArgs(['--duration=0']).errors[0]).toContain('greater than zero');
    expect(resolveNumericArgs(['--interval=-1']).errors[0]).toContain('got "-1"');
  });

  it('refuses the following flag when the value was forgotten', () => {
    const { count, errors } = resolveNumericArgs(['--count', '--location']);
    expect(count).toBeNaN();
    expect(errors[0]).toBe(
      '--count was given no value — the next argument is the flag --location '
      + '(omit it to use the default of 100)',
    );
  });

  it('names every bad flag in one pass', () => {
    const { errors } = resolveNumericArgs(['--count', 'abc', '--duration', '-1', '--interval', '0']);
    expect(errors).toHaveLength(3);
    expect(errors.map(e => e.split(' ')[0])).toEqual(['--count', '--duration', '--interval']);
  });

  it('leaves the valid flags usable when another is wrong', () => {
    const { count, durationS, errors } = resolveNumericArgs(['--count', '50', '--duration', 'nope']);
    expect(count).toBe(50);
    expect(durationS).toBeNaN();
    expect(errors).toHaveLength(1);
  });
});

describe('loadtest flag reading — the argv shapes that used to become the default', () => {

  it('hands back the default when the flag is absent', () => {
    expect(readFlag([], 'file', null)).toEqual({ value: null });
    expect(readFlag(['--location'], 'file', null)).toEqual({ value: null });
  });

  it('reads the separated and inline spellings the same way', () => {
    expect(readFlag(['--file', '/tmp/x'], 'file', null)).toEqual({ value: '/tmp/x' });
    expect(readFlag(['--file=/tmp/x'], 'file', null)).toEqual({ value: '/tmp/x' });
  });

  it('refuses a flag left as the final token', () => {
    expect(readFlag(['--file'], 'file', null, 'an auto-generated 1MB test file')).toEqual({
      error: '--file was given no value (omit it to use the default of an auto-generated 1MB test file)',
    });
  });

  it('names the default in the words the caller chose', () => {
    expect(readFlag(['--count'], 'count', '100').error).toContain('the default of 100');
  });

  it('refuses a value that is itself a flag', () => {
    expect(readFlag(['--file', '--count', '5'], 'file', null, 'a generated file')).toEqual({
      error: '--file was given no value — the next argument is the flag --count '
        + '(omit it to use the default of a generated file)',
    });
  });

  it('leaves a single-dash value alone', () => {
    expect(readFlag(['--count', '-5'], 'count', '100')).toEqual({ value: '-5' });
    expect(readFlag(['--file', '-weird'], 'file', null)).toEqual({ value: '-weird' });
  });

  it('passes an empty value through rather than defaulting', () => {
    expect(readFlag(['--file', ''], 'file', null)).toEqual({ value: '' });
    expect(readFlag(['--file='], 'file', null)).toEqual({ value: '' });
  });

  it('keeps everything after the first = in an inline value', () => {
    expect(readFlag(['--file=a=b'], 'file', null)).toEqual({ value: 'a=b' });
    expect(readFlag(['--file=/tmp/run=3.bin'], 'file', null)).toEqual({ value: '/tmp/run=3.bin' });
  });

  it('reaches a path that genuinely starts with -- through the inline form', () => {
    expect(readFlag(['--file=--weird'], 'file', null)).toEqual({ value: '--weird' });
  });

  it('does not let an inline value be mistaken for another flag', () => {
    expect(readFlag(['--file=--count=9'], 'count', '100')).toEqual({ value: '100' });
  });

  it('takes the last occurrence when a flag is repeated', () => {
    expect(readFlag(['--count', '1', '--count', '2'], 'count', '100')).toEqual({ value: '2' });
    expect(readFlag(['--file', 'a', '--file=b'], 'file', null)).toEqual({ value: 'b' });
    expect(readFlag(['--file=a', '--file', 'b'], 'file', null)).toEqual({ value: 'b' });
  });

  it('reports the last occurrence being malformed even when an earlier one was fine', () => {
    expect(readFlag(['--file', 'a', '--file'], 'file', null, 'a generated file').error)
      .toContain('given no value');
  });

  it('does not match a longer flag that starts with the same letters', () => {
    expect(readFlag(['--filename', 'x'], 'file', null)).toEqual({ value: null });
    expect(readFlag(['--file-count', '3'], 'file', null)).toEqual({ value: null });
  });
});

describe('loadtest boolean flags — the `=` spelling that read as absent', () => {

  it('reads the bare token as on and its absence as off', () => {
    expect(readBooleanFlag([], 'location')).toEqual({ value: false });
    expect(readBooleanFlag(['--location'], 'location')).toEqual({ value: true });
    expect(readBooleanFlag(['--count', '5', '--location'], 'location')).toEqual({ value: true });
    expect(readBooleanFlag(['--count', '5'], 'location')).toEqual({ value: false });
  });

  it.each([
    ['--location=true', 'true'],
    ['--location=false', 'false'],
    ['--location=1', '1'],
    ['--location=0', '0'],
    ['--location=yes', 'yes'],
    ['--location=', ''],
  ])('refuses %s rather than reading a value out of it', (token, echoed) => {
    expect(readBooleanFlag([token], 'location').error).toBe(
      `--location takes no value, got ${JSON.stringify(echoed)} — pass --location on its own to turn it on, or omit it to leave it off`,
    );
  });

  it('reports no value alongside the refusal', () => {
    expect(readBooleanFlag(['--location=true'], 'location').value).toBeUndefined();
  });

  it('refuses the = spelling on --allow-production too', () => {
    expect(readBooleanFlag(['--allow-production=1'], 'allow-production').error)
      .toContain('--allow-production takes no value');
    expect(readBooleanFlag(['--allow-production'], 'allow-production')).toEqual({ value: true });
  });

  it('refuses an = occurrence even when the bare token is also present', () => {
    expect(readBooleanFlag(['--location', '--location=false'], 'location').error).toBeDefined();
    expect(readBooleanFlag(['--location=false', '--location'], 'location').error).toBeDefined();
  });

  it('is unchanged by a repeated bare token', () => {
    expect(readBooleanFlag(['--location', '--location'], 'location')).toEqual({ value: true });
  });

  it('leaves a following positional alone — refusing it is not this reader\'s job', () => {
    expect(readBooleanFlag(['--location', 'true'], 'location')).toEqual({ value: true });
    expect(readBooleanFlag(['--location', 'false'], 'location')).toEqual({ value: true });
    expect(resolveUnknownArgs(['--location', 'false']).errors).toHaveLength(1);
  });

  it('matches the flag name case-sensitively', () => {
    expect(readBooleanFlag(['--LOCATION=true'], 'location')).toEqual({ value: false });
    expect(readBooleanFlag(['--Location'], 'location')).toEqual({ value: false });
    expect(resolveUnknownArgs(['--LOCATION=true']).errors[0]).toContain('did you mean --location?');
  });

  it('does not match a longer flag that starts with the same letters', () => {
    expect(readBooleanFlag(['--locations'], 'location')).toEqual({ value: false });
    expect(readBooleanFlag(['--location-only=1'], 'location')).toEqual({ value: false });
  });
});

describe('loadtest boolean flags — resolving them from argv', () => {
  it('leaves the location leg off when the flag is absent', () => {
    expect(resolveBooleanArgs([])).toEqual({ includeLocation: false, errors: [] });
  });

  it('turns the location leg on for the bare flag', () => {
    expect(resolveBooleanArgs(['--location'])).toEqual({ includeLocation: true, errors: [] });
  });

  it('refuses --location=true instead of running with the leg off', () => {
    const { includeLocation, errors } = resolveBooleanArgs(['--location=true']);
    expect(errors).toHaveLength(1);
    expect(includeLocation).toBe(false);
  });

  it('reports a malformed --allow-production in the argument pass', () => {
    expect(resolveBooleanArgs(['--allow-production=1']).errors).toEqual([
      '--allow-production takes no value, got "1" — pass --allow-production on its own to turn it on, or omit it to leave it off',
    ]);
  });

  it('names every bad boolean flag in one pass', () => {
    const { errors } = resolveBooleanArgs(['--location=1', '--allow-production=1']);
    expect(errors).toHaveLength(2);
    expect(errors.map((e) => e.split(' ')[0])).toEqual(['--location', '--allow-production']);
  });

  it('reports a malformed --allow-production without disturbing --location', () => {
    const { includeLocation, errors } = resolveBooleanArgs(['--location', '--allow-production=1']);
    expect(includeLocation).toBe(true);
    expect(errors).toHaveLength(1);
    expect(errors[0]).toContain('--allow-production');
  });

  it('ignores flags it does not own', () => {
    expect(resolveBooleanArgs(['--file=/tmp/x', '--count=5']))
      .toEqual({ includeLocation: false, errors: [] });
  });

  it("does not read another flag's value as its own", () => {
    expect(resolveBooleanArgs(['--file=--location=true']))
      .toEqual({ includeLocation: false, errors: [] });
    expect(resolveFileArg(['--file=--location=true']).filePath).toBe('--location=true');
  });

  it('reports the boolean and the value-taking flag separately', () => {
    const { includeLocation, errors } = resolveBooleanArgs(['--file', '--location=true']);
    expect(includeLocation).toBe(false);
    expect(errors).toHaveLength(1);
    expect(resolveFileArg(['--file', '--location=true']).errors).toHaveLength(1);
  });

  it('reads --allow-production through the same reader as --location', () => {
    expect(resolveGuardInputs({}, ['node', 'script.js', '--allow-production']).allowProdFlag).toBe(true);
    expect(resolveGuardInputs({}, ['node', 'script.js']).allowProdFlag).toBe(false);
  });

  it('leaves the production override OFF when its shape is refused', () => {
    expect(resolveGuardInputs({}, ['node', 'script.js', '--allow-production=true']).allowProdFlag).toBe(false);
    expect(resolveGuardInputs({}, ['node', 'script.js', '--allow-production=1']).allowProdFlag).toBe(false);
    expect(resolveBooleanArgs(['--allow-production=true']).errors).toHaveLength(1);
  });

  it('still reads the bare flag when a value-taking flag swallowed at it', () => {
    expect(resolveBooleanArgs(['--count', '--location']).includeLocation).toBe(true);
    expect(resolveNumericArgs(['--count', '--location']).errors).toHaveLength(1);
  });
});

describe('loadtest unknown arguments — the tokens no reader ever saw', () => {

  const errorsFor = (argv) => resolveUnknownArgs(argv).errors;

  it('accepts a command line made only of declared flags and their values', () => {
    expect(errorsFor([])).toEqual([]);
    expect(errorsFor(['--count', '5', '--duration=60', '--interval', '2',
      '--file=/tmp/x', '--location', '--allow-production'])).toEqual([]);
  });

  it.each([
    ['a misspelled boolean flag', ['--locatoin']],
    ['a misspelled numeric flag', ['--cont', '5']],
    ['a case slip', ['--LOCATION=true']],
    ['a single-dash boolean flag', ['-location']],
    ['a single-dash numeric flag', ['-count', '5']],
    ['a value handed to a boolean flag positionally', ['--location', 'false']],
    ['a bare positional', ['payload.bin']],
  ])('refuses %s with exactly one message', (_label, argv) => {
    expect(errorsFor(argv)).toHaveLength(1);
  });

  it('says the leg was about to be turned on when a boolean flag was given a value', () => {
    expect(errorsFor(['--location', 'false'])).toEqual([
      'unexpected argument "false" after --location — --location takes no value; '
        + 'pass it on its own to turn it on, or omit it to leave it off',
    ]);
    expect(errorsFor(['--location', 'true'])).toHaveLength(1);
    expect(errorsFor(['--allow-production', '1'])[0]).toContain('--allow-production takes no value');
    expect(errorsFor(['--file=/tmp/x', 'stray'])).toEqual([
      expect.stringContaining('this script takes no positional arguments'),
    ]);
    expect(errorsFor(['--file=/tmp/x', 'stray'])[0]).not.toContain('--file takes no value');
    expect(errorsFor(['--location=x', 'stray'])).toEqual([
      expect.stringContaining('this script takes no positional arguments'),
    ]);
    expect(errorsFor(['--location=x', 'stray'])[0]).not.toContain('after --location');
  });

  it('suggests the flag a near miss was meant to be', () => {
    expect(errorsFor(['-count', '5'])).toEqual([
      '-count is not a flag this script accepts — did you mean --count? '
        + '(flag names are case-sensitive, and take two dashes)',
    ]);
    expect(errorsFor(['--LOCATION=true'])[0]).toContain('did you mean --location?');
    expect(errorsFor(['---count'])[0]).toContain('did you mean --count?');
    expect(errorsFor(['-allow-production'])[0]).toContain('did you mean --allow-production?');
  });

  it('needs exactly two dashes before it will resolve a token to a flag', () => {
    expect(errorsFor(['-fcount', '5'])).toHaveLength(1);
    expect(errorsFor(['-fcount', '5'])[0]).toContain('-fcount is not a flag');
  });

  it('falls back to the flag list when nothing is close enough to suggest', () => {
    expect(errorsFor(['--locatoin'])).toEqual([
      '--locatoin is not a flag this script accepts — accepted flags are '
        + '--count, --duration, --interval, --file, --max-fail-rate, --ledger, --reclaim, '
        + '--location, --allow-production',
    ]);
    expect(errorsFor(['payload.bin'])[0])
      .toContain('this script takes no positional arguments; accepted flags are --count');
  });

  it('builds the accepted-flag list from the table rather than restating it', () => {
    const listed = errorsFor(['--locatoin'])[0].split('accepted flags are ')[1];
    expect(listed).toBe(FLAGS.map((spec) => `--${spec.name}`).join(', '));
  });

  it('costs one message for one mistake when an unknown flag has a value', () => {
    expect(errorsFor(['--cont', '5'])).toEqual([expect.stringContaining('--cont')]);
    expect(errorsFor(['--cont', '5', '7'])).toHaveLength(2);
    expect(errorsFor(['--locatoin', '--cont'])).toHaveLength(2);
    expect(errorsFor(['payload.bin', 'other.bin'])).toHaveLength(2);
    expect(errorsFor(['--cont=5', 'stray'])).toHaveLength(2);
    expect(errorsFor(['--file=/tmp/x', 'stray'])).toHaveLength(1);
  });

  it('skips a declared value-taking flag\'s value on exactly readFlag\'s terms', () => {
    for (const argv of [
      ['--count', '5'], ['--file', '/tmp/x'], ['--file', ''],
      ['--file', '-weird'], ['--count', '-5'], ['--file', 'a', '--file', 'b'],
    ]) {
      expect([argv, errorsFor(argv)]).toEqual([argv, []]);
      expect(readFlag(argv, argv[0].slice(2), null).value).toBe(argv[argv.length - 1]);
    }
  });

  it('leaves a flag-shaped token after a value-taking flag to readFlag', () => {
    expect(errorsFor(['--file', '--count', '5'])).toEqual([]);
    expect(resolveFileArg(['--file', '--count', '5']).errors).toHaveLength(1);
    expect(resolveArgErrors(['--file', '--count', '5'], () => null)).toHaveLength(1);
  });

  it('reads an inline value as a value and not as another flag', () => {
    expect(errorsFor(['--file=--count=9'])).toEqual([]);
    expect(errorsFor(['--file=--weird'])).toEqual([]);
    expect(errorsFor(['--file=/tmp/run=3.bin'])).toEqual([]);
    expect(errorsFor(['--file=/tmp/x', 'stray'])).toHaveLength(1);
  });

  it('leaves a flag with no value at all to readFlag', () => {
    expect(errorsFor(['--file'])).toEqual([]);
    expect(errorsFor(['--count'])).toEqual([]);
    expect(resolveArgErrors(['--file'], () => null)).toHaveLength(1);
  });

  it('refuses -- rather than honouring it', () => {
    expect(errorsFor(['--'])).toEqual([
      '-- has nothing to separate here — this script takes no positional '
        + 'arguments, so every argument is a flag',
    ]);
    expect(errorsFor(['--'])[0]).not.toContain('did you mean');
    expect(errorsFor(['--', '--locatoin'])).toHaveLength(2);
    expect(errorsFor(['--', 'payload.bin'])).toHaveLength(2);
  });

  it('reports every stray token in one pass, in command-line order', () => {
    expect(errorsFor(['--locatoin', '--count', '5', 'stray'])).toEqual([
      expect.stringContaining('--locatoin'), expect.stringContaining('"stray"'),
    ]);
  });
});

describe('loadtest flag table — the one list everything reads', () => {
  const never = () => null;

  it.each(FLAGS.filter((spec) => spec.name !== 'max-fail-rate').map((spec) => [spec.name, spec]))(
    'wires --%s to a reader instead of accepting it and ignoring it',
    (name, spec) => {
      const argv = spec.takesValue ? [`--${name}`] : [`--${name}=x`];
      expect(resolveArgErrors(argv, never)).toEqual([expect.stringContaining(`--${name}`)]);
    },
  );

  it('hands out a table nothing can edit', () => {
    expect(Object.isFrozen(FLAGS)).toBe(true);
    expect(FLAGS.every((spec) => Object.isFrozen(spec))).toBe(true);
    expect(() => FLAGS.push({ name: 'warmup', takesValue: true })).toThrow();
  });

  it('refuses to hand out a spec for a flag it does not declare', () => {
    expect(() => flagSpec('warmup', true)).toThrow('--warmup is read but not declared in FLAGS');
  });

  it('refuses to hand out a spec read at the wrong arity', () => {
    expect(() => flagSpec('location', true)).toThrow(/declared valueless in FLAGS but read as the opposite/);
    expect(() => flagSpec('count', false)).toThrow(/declared value-taking in FLAGS but read as the opposite/);
  });

  it('carries every default the resolvers fall back to', () => {
    expect(resolveNumericArgs([])).toEqual({ count: 100, durationS: 7200, intervalS: 60, errors: [] });
    expect(FLAGS.filter((spec) => spec.takesValue).map((spec) => [spec.name, spec.defaultValue]))
      .toEqual([
        ['count', '100'], ['duration', '7200'], ['interval', '60'],
        ['file', null], ['max-fail-rate', '10'],
        ['ledger', null], ['reclaim', null],
      ]);
    expect(DEFAULT_MAX_FAIL_RATE_PCT).toBe(10);
    expect(String(DEFAULT_MAX_FAIL_RATE_PCT))
      .toBe(FLAGS.find((spec) => spec.name === 'max-fail-rate').defaultValue);
    const file = FLAGS.find((spec) => spec.name === 'file');
    expect(resolveFileArg(['--file']).errors[0]).toContain(`the default of ${file.defaultLabel}`);
    expect(file.defaultLabel).toBe('an auto-generated 1MB test file');
  });
});

describe('loadtest --file — the flag whose default uploads something else', () => {
  it('reports no path and no error when the flag is absent', () => {
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
    expect(resolveFileArg(['--file', '   ']).errors[0]).toContain('got "   "');
  });

  it('keeps a path that legitimately carries surrounding spaces', () => {
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
    if (dir) fs.rmSync(dir, { recursive: true, force: true });
  });

  it('accepts a readable regular file', () => {
    expect(checkUploadFile(readable)).toBeNull();
  });

  it('reports a path that does not exist', () => {
    expect(checkUploadFile(path.join(dir, 'missing.bin'))).toContain('cannot be read');
  });

  it('reports a directory rather than letting readFileSync throw EISDIR', () => {
    expect(checkUploadFile(dir)).toContain('is not a regular file');
  });

  it('names the flag in every message', () => {
    expect(checkUploadFile(path.join(dir, 'missing.bin'))).toMatch(/^--file /);
    expect(checkUploadFile(dir)).toMatch(/^--file /);
  });

  const permissionsApply = process.platform !== 'win32' && process.getuid?.() !== 0;
  (permissionsApply ? it : it.skip)('reports a file that exists but cannot be read', () => {
    const locked = path.join(dir, 'locked.bin');
    fs.writeFileSync(locked, 'x');
    fs.chmodSync(locked, 0o000);
    try {
      const message = checkUploadFile(locked);
      expect(message).toContain('is not readable');
      expect(message).toMatch(/^--file /);
    } finally {
      fs.chmodSync(locked, 0o600);
    }
  });

  it('rejects a non-regular file that is not a directory', () => {
    expect(checkUploadFile('/dev/null')).toContain('is not a regular file');
  });

  it('follows a symlink to a real file', () => {
    const link = path.join(dir, 'link.bin');
    fs.symlinkSync(readable, link);
    expect(checkUploadFile(link)).toBeNull();
  });

  it('reports a dangling symlink as unreadable', () => {
    const dangling = path.join(dir, 'dangling.bin');
    fs.symlinkSync(path.join(dir, 'gone.bin'), dangling);
    expect(checkUploadFile(dangling)).toContain('cannot be read');
  });

  it('quotes the path so whitespace in it stays visible', () => {
    expect(checkUploadFile(path.join(dir, ' spaced '))).toContain(`"${path.join(dir, ' spaced ')}"`);
  });
});

describe('loadtest preflight — the composition main() used to hold inline', () => {

  const never = () => null;

  it('carries the --file errors, not only the numeric ones', () => {
    expect(resolveArgErrors(['--file'], never)).toEqual([
      expect.stringContaining('--file'),
    ]);
    expect(resolveArgErrors(['--file', '--location'], never)).toHaveLength(1);
  });

  it('carries the boolean-flag errors too', () => {
    expect(resolveArgErrors(['--location=true'], never)).toEqual([
      expect.stringContaining('--location takes no value'),
    ]);
    expect(resolveArgErrors(['--allow-production=1'], never)).toHaveLength(1);
  });

  it('carries the unknown-argument errors too', () => {
    expect(resolveArgErrors(['--locatoin'], never)).toEqual([
      expect.stringContaining('--locatoin is not a flag'),
    ]);
    expect(resolveArgErrors(['--location', 'false'], never)).toEqual([
      expect.stringContaining('--location takes no value'),
    ]);
  });

  it('reports unknown arguments after the flags it recognized', () => {
    expect(resolveArgErrors(['--locatoin', '--count', 'abc'], never)).toEqual([
      expect.stringContaining('--count must be'),
      expect.stringContaining('--locatoin'),
    ]);
    expect(resolveArgErrors(['--locatoin', '--location=1'], never)).toEqual([
      expect.stringContaining('--location takes no value'),
      expect.stringContaining('--locatoin'),
    ]);
    expect(resolveArgErrors(['--locatoin', '--file', ''], never)).toEqual([
      expect.stringContaining('--file must name a file'),
      expect.stringContaining('--locatoin'),
    ]);
    expect(resolveArgErrors(['--locatoin', '--file', '/x'], () => 'file is bad')).toEqual([
      expect.stringContaining('--locatoin'), 'file is bad',
    ]);
  });

  it('names a bad boolean flag alongside a bad numeric one', () => {
    expect(resolveArgErrors(['--count', 'abc', '--location=1'], never)).toHaveLength(2);
  });

  it('surfaces what the readability check reported', () => {
    expect(resolveArgErrors(['--file', '/some/path'], () => 'boom')).toEqual(['boom']);
  });

  it('does not touch the filesystem when no --file was given', () => {
    let called = 0;
    const errors = resolveArgErrors([], () => { called += 1; return 'should not run'; });
    expect(called).toBe(0);
    expect(errors).toEqual([]);
  });

  it('does not stat a path the operator never typed', () => {
    let seen = 'untouched';
    resolveArgErrors(['--file', ''], (candidate) => { seen = candidate; return null; });
    expect(seen).toBe('untouched');
  });

  it('reports every fault in one pass, numeric flags before the file flag', () => {
    expect(resolveArgErrors(['--file', '', '--count', 'abc'], () => null)).toEqual([
      expect.stringContaining('--count'),
      expect.stringContaining('--file'),
    ]);
    expect(resolveArgErrors(['--count', 'abc', '--file', '/x'], () => 'file is bad')).toEqual([
      expect.stringContaining('--count'),
      'file is bad',
    ]);
  });

  it('carries the boolean-flag errors too', () => {
    expect(resolveArgErrors(['--location=true'], () => null)).toEqual([
      expect.stringContaining('--location takes no value'),
    ]);
    expect(resolveArgErrors(['--allow-production=1'], () => null)).toHaveLength(1);
  });

  it('names a bad boolean flag alongside a bad numeric one', () => {
    expect(resolveArgErrors(['--count', 'abc', '--location=1'], () => null)).toEqual([
      expect.stringContaining('--count'),
      expect.stringContaining('--location'),
    ]);
  });

  it('defaults to the real readability check', () => {
    expect(resolveArgErrors(['--file', '/nonexistent/loadtest/payload.bin'])).toEqual([
      expect.stringContaining('cannot be read'),
    ]);
  });
});

describe('loadtest generated payload — the temp file that used to litter /tmp', () => {
  it('is still 1MB of the same filler byte', () => {
    const payload = generateTestPayload();
    expect(Buffer.isBuffer(payload)).toBe(true);
    expect(payload).toHaveLength(1024 * 1024);
    expect(payload.equals(Buffer.alloc(1024 * 1024, 'A'))).toBe(true);
  });

  it('hands back one buffer rather than allocating per round', () => {
    expect(generateTestPayload()).toBe(generateTestPayload());
  });
});

describe('loadtest script — static checks on call sites no test can reach', () => {
  const parseFile = (...segments) =>
    parser.parse(fs.readFileSync(path.join(__dirname, '..', ...segments), 'utf8'), {
      sourceType: 'unambiguous',
    });

  const ast = parseFile('scripts', 'loadtest-standalone.js');
  const staticName = (node, computed) => {
    if (node.type === 'StringLiteral') return node.value;
    if (node.type === 'TemplateLiteral' && node.expressions.length === 0) {
      return node.quasis[0]?.value.cooked ?? null;
    }
    if (!computed && node.type === 'Identifier') return node.name;
    return null;
  };

  const resolveAliases = (sourceAst) => {
    const bound = new Map();
    const note = (name, target) => bound.set(name, bound.has(name) ? null : target);
    traverse(sourceAst, {
      VariableDeclarator(p) {
        const { id, init } = p.node;
        if (id.type === 'Identifier') {
          if (init?.type === 'MemberExpression') {
            note(id.name, staticName(init.property, init.computed));
          } else {
            note(id.name, init?.type === 'StringLiteral' ? init.value : null);
          }
          return;
        }
        if (id.type !== 'ObjectPattern') return;
        for (const prop of id.properties) {
          if (prop.type !== 'ObjectProperty' || prop.value.type !== 'Identifier') continue;
          note(prop.value.name, staticName(prop.key, prop.computed));
        }
      },
    });
    return new Map([...bound].filter(([, target]) => target !== null));
  };

  const makeCalleeName = (sourceAst) => {
    const aliases = resolveAliases(sourceAst);
    return (node) => {
      const callee = node.callee;
      if (callee.type === 'Identifier') return aliases.get(callee.name) ?? callee.name;
      if (callee.type !== 'MemberExpression') return null;
      const named = staticName(callee.property, callee.computed);
      if (named !== null) return named;
      if (callee.computed && callee.property.type === 'Identifier') {
        return aliases.get(callee.property.name) ?? null;
      }
      return null;
    };
  };

  const calleeName = makeCalleeName(ast);

  const flagLiterals = () => {
    const found = [];
    traverse(ast, {
      StringLiteral(p) { if (/^--\w/.test(p.node.value)) found.push(p.node.value); },
    });
    return found.sort();
  };

  const flagNameDeclarations = () => {
    const declared = [];
    traverse(ast, {
      StringLiteral(p) {
        const bare = p.node.value.replace(/^--/, '');
        if (!FLAGS.some((spec) => spec.name === bare)) return;
        if (!p.parentPath.isObjectProperty() && !p.parentPath.isArrayExpression()) return;
        let list = null;
        for (let up = p.parentPath; up; up = up.parentPath) {
          if (up.isArrayExpression() || up.isObjectExpression()) list = up;
        }
        declared.push({ name: bare, list: list === null ? null : list.node });
      },
    });
    return declared;
  };

  const mainNode = () => {
    let found = null;
    traverse(ast, {
      FunctionDeclaration(p) { if (p.node.id?.name === 'main') found = p.node; },
    });
    return found;
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

  const findFunction = (name) => {
    let found = null;
    traverse(ast, {
      FunctionDeclaration(p) { if (p.node.id?.name === name) found = p.node; },
      VariableDeclarator(p) {
        if (p.node.id.type !== 'Identifier' || p.node.id.name !== name) return;
        const init = p.node.init;
        if (init?.type === 'ArrowFunctionExpression' || init?.type === 'FunctionExpression') {
          found = init;
        }
      },
    });
    return found;
  };

  const callsWithin = (fn, name) => callsNamed(name)
    .filter((node) => node.start >= fn.start && node.end <= fn.end);

  it('resolves every spelling of a call that reaches the same primitive', () => {
    const fixture = [
      "const fs = require('fs');",
      "const aliased = fs.writeFileSync;",
      "const { writeFileSync: renamed } = require('fs');",
      "const { writeFileSync } = fs;",
      "const { 'writeFileSync': quoted } = fs;",
      "const KEY = 'writeFileSync';",
      "function one() { const dup = fs.writeFileSync; dup('x'); }",
      "function two() { const dup = fs.appendFileSync; return dup; }",
      "fs.writeFileSync('a');",
      "fs['writeFileSync']('b');",
      "fs[`writeFileSync`]('c');",
      "fs[KEY]('d');",
      "aliased('e');",
      "renamed('f');",
      "writeFileSync('h');",
      "quoted('i');",
      "fs[String('writeFileSync')]('g');",
    ].join('\n');
    const fixtureAst = parser.parse(fixture, { sourceType: 'unambiguous' });
    const resolve = makeCalleeName(fixtureAst);
    const resolved = {};
    traverse(fixtureAst, {
      CallExpression(p) {
        const { callee } = p.node;
        resolved[fixture.slice(callee.start, callee.end)] = resolve(p.node);
      },
    });
    expect(resolved).toEqual({
      "require": 'require',
      'fs.writeFileSync': 'writeFileSync',
      "fs['writeFileSync']": 'writeFileSync',
      'fs[`writeFileSync`]': 'writeFileSync',
      'fs[KEY]': 'writeFileSync',
      'aliased': 'writeFileSync',
      'renamed': 'writeFileSync',
      'writeFileSync': 'writeFileSync',
      'quoted': 'writeFileSync',
      'dup': 'dup',
      'String': 'String',
      "fs[String('writeFileSync')]": null,
    });
  });

  it('hand-rolls no HTTP call of its own', () => {
    expect(callsNamed('fetch')).toHaveLength(0);
  });

  it('parses none of its own numeric arguments', () => {
    expect(callsNamed('parseInt')).toHaveLength(0);
  });

  it('declares its flags in exactly one table', () => {
    const declared = flagNameDeclarations();
    expect(declared.map((d) => d.name).sort()).toEqual(FLAGS.map((spec) => spec.name).sort());
    expect(new Set(declared.map((d) => d.list)).size).toBe(1);
  });

  it('reads the flag table everywhere a flag is looked up', () => {
    expect(callsNamed('flagSpec')).toHaveLength(7);
  });

  it('reads --max-fail-rate in main() through the shared reader', () => {
    const main = mainNode();
    expect(main).not.toBeNull();
    const namesReadInMain = callsNamed('readFlag')
      .filter((node) => node.start >= main.start && node.end <= main.end)
      .map((node) => node.arguments[1])
      .filter((arg) => arg && arg.type === 'StringLiteral')
      .map((arg) => arg.value);
    expect(namesReadInMain).toContain('max-fail-rate');
  });

  it('resolves its numeric flags in one place', () => {
    expect(callsNamed('resolveNumericArgs')).toHaveLength(2);
  });

  it('scans argv through the shared reader and nothing else', () => {
    expect(callsNamed('indexOf')).toHaveLength(0);
    expect(callsNamed('getArg')).toHaveLength(0);
    expect(callsNamed('readFlag')).toHaveLength(5);
    expect(callsNamed('resolveUnknownArgs')).toHaveLength(1);
  });

  it('reads every boolean flag through the one shared reader', () => {
    expect(flagLiterals()).toEqual(['--allow-production']);
    expect(callsNamed('readBooleanFlag')).toHaveLength(2);
    expect(callsNamed('resolveBooleanArgs')).toHaveLength(2);
    expect(callsNamed('hasFlag')).toHaveLength(0);
  });

  it('resolves the upload flag and the whole preflight in one place each', () => {
    expect(callsNamed('resolveFileArg')).toHaveLength(2);
    expect(callsNamed('resolveArgErrors')).toHaveLength(1);
  });

  it('decides every argument error before the smoke test mints a resource', () => {
    const main = findFunction('main');
    expect(main).not.toBeNull();
    const firstInMain = (name) => {
      const starts = callsNamed(name)
        .filter((node) => node.start >= main.start && node.end <= main.end)
        .map((node) => node.start);
      return starts.length ? Math.min(...starts) : null;
    };
    const preflight = firstInMain('resolveArgErrors');
    const smokeTest = firstInMain('createOneTimeLink');
    expect(preflight).not.toBeNull();
    expect(smokeTest).not.toBeNull();
    expect(preflight).toBeLessThan(smokeTest);

    const guard = firstInMain('resolveGuardInputs');
    expect(guard).not.toBeNull();
    expect(preflight).toBeLessThan(guard);
  });

  it('uploads through reUploadBuffer, twice and only twice', () => {
    expect(callsNamed('reUploadBuffer')).toHaveLength(2);
  });

  it('passes the first three parameters positionally and omits the last two', () => {
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
    const calls = callsNamed('reUploadBuffer');
    expect(calls).not.toHaveLength(0);
    for (const call of calls) expect(call.arguments).toHaveLength(3);
  });

  const WRITE_PRIMITIVES = [
    'writeFile', 'writeFileSync', 'appendFile', 'appendFileSync',
    'write', 'writeSync', 'writev', 'writevSync',
    'truncate', 'truncateSync', 'ftruncate', 'ftruncateSync',
    'copyFile', 'copyFileSync', 'cp', 'cpSync',
    'open', 'openSync', 'createWriteStream',
    'mkdir', 'mkdirSync', 'mkdtemp', 'mkdtempSync',
    'rename', 'renameSync', 'link', 'linkSync', 'symlink', 'symlinkSync',
  ];

  const DELETE_PRIMITIVES = [
    'unlink', 'unlinkSync', 'rm', 'rmSync', 'rmdir', 'rmdirSync',
  ];

  const LEDGER_WRITES = [
    { writer: 'preflightLedger', primitive: 'openSync' },
    { writer: 'recordResource', primitive: 'appendFileSync' },
    { writer: 'pruneLedger', primitive: 'writeFileSync' },
  ];

  const LEDGER_TARGET = /^(?:LEDGER_PATH|ledgerPath)$/;

  it('writes nothing to disk outside the reclaim ledger', () => {
    const exempt = new Set(LEDGER_WRITES.map(({ writer, primitive }) => {
      const fn = findFunction(writer);
      expect({ writer, found: Boolean(fn) }).toEqual({ writer, found: true });
      const owned = callsWithin(fn, primitive);
      expect({ writer, primitive, calls: owned.length })
        .toEqual({ writer, primitive, calls: 1 });
      const [target] = owned[0].arguments;
      expect({ writer, target: target?.type === 'Identifier' ? target.name : `${target?.type}` })
        .toEqual({ writer, target: expect.stringMatching(LEDGER_TARGET) });
      return owned[0];
    }));
    for (const primitive of WRITE_PRIMITIVES) {
      const stray = callsNamed(primitive).filter((node) => !exempt.has(node));
      expect({ primitive, callsOutsideLedger: stray.length })
        .toEqual({ primitive, callsOutsideLedger: 0 });
    }
    for (const primitive of DELETE_PRIMITIVES) {
      expect({ primitive, calls: callsNamed(primitive).length })
        .toEqual({ primitive, calls: 0 });
    }
  });

  it('writes nothing from the round itself', () => {
    const runRound = findFunction('runRound');
    expect({ found: Boolean(runRound) }).toEqual({ found: true });
    for (const primitive of WRITE_PRIMITIVES) {
      const inRound = callsWithin(runRound, primitive);
      expect({ primitive, callsInRunRound: inRound.length })
        .toEqual({ primitive, callsInRunRound: 0 });
    }
  });

  it('spawns no subprocess to write on its behalf', () => {
    const required = [];
    traverse(ast, {
      CallExpression(p) {
        if (calleeName(p.node) !== 'require') return;
        const [source] = p.node.arguments;
        if (source?.type === 'StringLiteral') required.push(source.value);
      },
    });
    expect({ readsFs: required.some((mod) => /^(?:node:)?fs$/.test(mod)) })
      .toEqual({ readsFs: true });
    expect(required.filter((mod) => /^(?:node:)?child_process$/.test(mod))).toEqual([]);
  });

  it('keeps the per-round read for --file and drops it for the generated payload', () => {
    const runRound = findFunction('runRound');
    expect(runRound).not.toBeNull();
    const inRunRound = (name) => callsWithin(runRound, name);
    expect(inRunRound('readFileSync')).toHaveLength(1);
    expect(inRunRound('generateTestPayload')).toHaveLength(1);
    expect(inRunRound('alloc')).toHaveLength(0);
  });

  it('imports reUploadBuffer from the connector client', () => {
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
