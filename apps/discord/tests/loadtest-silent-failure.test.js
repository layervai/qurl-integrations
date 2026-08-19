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
  readFlag, readBooleanFlag, resolveBooleanArgs,
  parsePositiveInt, resolveNumericArgs, resolveFileArg, checkUploadFile,
  resolveArgErrors, resolveGuardInputs,
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

  // The spelling indexOf cannot see. `--count=200` is not the token `--count`,
  // so it read as the flag being ABSENT and quietly ran the default.
  // `--duration=60` is the one that hurts: a run the operator sized at a
  // minute held the target for the default two hours and reported nothing.
  //
  // Pinned at the RESOLVER, not only at readFlag. The AST guard counts
  // readFlag call sites, so a per-flag shortcut back to an inline
  // `argv.indexOf` inside `read` leaves that count untouched and stays green
  // while `--duration=60` silently resolves to 7200 again.
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
    // Kills a first-wins mutant, which the per-flag rows above do not.
    //
    // This REPLACES the space-form-priority rule #1175 landed, under which the
    // inline form could never override an earlier space form: `--count 7
    // --count=200` resolved to 7 in both argv orders. Appending an override
    // and having it silently ignored is the same fault class as the default
    // this file exists to refuse, so position decides, not spelling.
    expect(resolveNumericArgs(['--count=5', '--count=9']).count).toBe(9);
    expect(resolveNumericArgs(['--count', '5', '--count=9']).count).toBe(9);
    expect(resolveNumericArgs(['--count=5', '--count', '9']).count).toBe(9);
  });

  it('lets a later inline value override an earlier space-separated one', () => {
    // The specific pair #1175 pinned the other way round. Kept as its own case
    // so the reversal is visible rather than folded into the row above.
    expect(resolveNumericArgs(['--count', '7', '--count=200']).count).toBe(200);
    expect(resolveNumericArgs(['--count=200', '--count', '7']).count).toBe(7);
  });

  it('does not let a longer flag match the one it starts with', () => {
    // '--counter=9' starts with '--count', and would set count if the equals
    // form were matched on the flag name rather than on the name plus '='.
    // Pinned here as well as at readFlag because the resolver is where a
    // per-flag shortcut would drop that guard.
    expect(resolveNumericArgs(['--counter=9'])).toEqual({
      count: 100, durationS: 7200, intervalS: 60, errors: [],
    });
  });

  it('reports an empty value as empty, whichever spelling delivered it', () => {
    // A deliberate divergence from #1175, pinned so it stays deliberate:
    // `--count=` reports the empty value it was given rather than "was given
    // no value".
    //
    // These are different operator mistakes. A trailing `--count` supplies no
    // value TOKEN; `--count=` and `--count ""` supply an empty one. #1175
    // mapped `--count=` onto the former, which splits it from `--count ""` —
    // reinstating by spelling exactly the divergence one reader exists to
    // remove. Held together here instead.
    const inline = resolveNumericArgs(['--count=']).errors;
    const separated = resolveNumericArgs(['--count', '']).errors;
    expect(inline).toEqual(separated);
    expect(inline[0]).toContain('got ""');
    expect(resolveNumericArgs(['--count']).errors[0]).toContain('was given no value');
  });

  it('splits on the first equals and leaves the rest to the parser', () => {
    // `--count==200` is a value of '=200', not an empty value and not 200.
    // Deciding what counts as malformed belongs to parsePositiveInt, so the
    // reader hands the whole remainder over untouched. Kept from #1175; it is
    // the resolver-level companion to readFlag's own `--file=a=b` case.
    expect(resolveNumericArgs(['--count==200']).errors[0]).toContain('got "=200"');
    expect(resolveNumericArgs(['--count=2=0']).errors[0]).toContain('got "2=0"');
  });

  it('resolves a bare token followed by an inline value to the inline one', () => {
    // #1175 pinned this the other way — the bare `--count` won and the run
    // failed parsing '--count=200' as its value. Under one reader the last
    // occurrence wins, so the complete spelling the operator typed second is
    // the one that takes effect.
    expect(resolveNumericArgs(['--count', '--count=200'])).toEqual({
      count: 200, durationS: 7200, intervalS: 60, errors: [],
    });
  });

  // The case the old getArg could not express: `--count` as the final token
  // has no value after it, and `args[idx + 1] || defaultVal` collapsed that
  // onto the same 100 as not passing the flag at all. Silently running the
  // default is exactly the "did nothing you asked for, reported success"
  // shape. readFlag separates the two; this holds them apart.
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
    // Otherwise the new spelling would be a way around parsePositiveInt rather
    // than a second way into it.
    expect(resolveNumericArgs(['--count=abc']).errors[0]).toContain('got "abc"');
    expect(resolveNumericArgs(['--duration=0']).errors[0]).toContain('greater than zero');
    expect(resolveNumericArgs(['--interval=-1']).errors[0]).toContain('got "-1"');
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
    expect(errors[0]).toBe(
      '--count was given no value — the next argument is the flag --location '
      + '(omit it to use the default of 100)',
    );
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
    expect(readFlag(['--file', '--count', '5'], 'file', null, 'a generated file')).toEqual({
      error: '--file was given no value — the next argument is the flag --count '
        + '(omit it to use the default of a generated file)',
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

  it('keeps everything after the first = in an inline value', () => {
    // Mutation this kills: `raw.slice(inlinePrefix.length)` ->
    // `raw.split('=')[1]`. That satisfies every other inline assertion here
    // (`--file=/tmp/x`, `--file=`, `--file=a`, `--file=b`) while silently
    // truncating `--file=/tmp/run=3.bin` to `/tmp/run`. Paths with `=` are
    // ordinary, and the failure is the silent-wrong-payload class again.
    expect(readFlag(['--file=a=b'], 'file', null)).toEqual({ value: 'a=b' });
    expect(readFlag(['--file=/tmp/run=3.bin'], 'file', null)).toEqual({ value: '/tmp/run=3.bin' });
  });

  it('reaches a path that genuinely starts with -- through the inline form', () => {
    // The documented escape hatch for the flag-shaped-value refusal. Without
    // a test, a mutation that applied the `--` refusal to the inline branch
    // too would survive and remove the only way to pass such a path.
    expect(readFlag(['--file=--weird'], 'file', null)).toEqual({ value: '--weird' });
  });

  it('does not let an inline value be mistaken for another flag', () => {
    // `--file=--count=9` contains `--count=`, but it is a VALUE, not a token
    // of its own. If it were scanned as one, --count would silently take 9.
    expect(readFlag(['--file=--count=9'], 'count', '100')).toEqual({ value: '100' });
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
    expect(readFlag(['--file', 'a', '--file'], 'file', null, 'a generated file').error)
      .toContain('given no value');
  });

  it('does not match a longer flag that starts with the same letters', () => {
    // The inline form is matched on the `--file=` prefix, so the guard that
    // keeps `--filename` out of it is that prefix carrying the `=`.
    expect(readFlag(['--filename', 'x'], 'file', null)).toEqual({ value: null });
    expect(readFlag(['--file-count', '3'], 'file', null)).toEqual({ value: null });
  });
});

describe('loadtest boolean flags — the `=` spelling that read as absent', () => {
  // This block replaces the deferral the previous change recorded here. Its
  // `does not yet recognize the equals form (known gap)` case asserted
  // `hasFlag(['--location=true'], 'location') === false` and existed so that
  // closing the gap would be a deliberate edit with a failing test to update.
  // This is that edit: the same input is now refused rather than read as
  // absent, and the assertion inverts on purpose.

  it('reads the bare token as on and its absence as off', () => {
    expect(readBooleanFlag([], 'location')).toEqual({ value: false });
    expect(readBooleanFlag(['--location'], 'location')).toEqual({ value: true });
    expect(readBooleanFlag(['--count', '5', '--location'], 'location')).toEqual({ value: true });
    expect(readBooleanFlag(['--count', '5'], 'location')).toEqual({ value: false });
  });

  // Every row here used to read as ABSENT. Refusing them all identically is
  // the decision: interpreting true/false/1/0 would leave `--location=yes` —
  // and the mistyped `--location=flase` — back in the silent-off hole, now
  // with a value in the operator's shell history suggesting it was honoured.
  //
  // The echoed value is pinned per row, not just the refusal. `--location=`
  // is the row that needs it: a wrapper emitting `--location=$WITH_LOCATION`
  // with the variable unset produces exactly that, and without the echo its
  // message is byte-identical to `--location=false` — so the operator is told
  // the wrong thing is wrong.
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
    // `{ error }` alone, so a caller reaching for `.value` finds undefined
    // rather than a `false` that reads like a decision the reader made.
    expect(readBooleanFlag(['--location=true'], 'location').value).toBeUndefined();
  });

  it('refuses the = spelling on --allow-production too', () => {
    // The milder of the two: an unrecognized `--allow-production=1` fails
    // CLOSED, because the guard then refuses the target rather than clearing
    // it. Silent about it either way, which is what this fixes.
    expect(readBooleanFlag(['--allow-production=1'], 'allow-production').error)
      .toContain('--allow-production takes no value');
    expect(readBooleanFlag(['--allow-production'], 'allow-production')).toEqual({ value: true });
  });

  it('refuses an = occurrence even when the bare token is also present', () => {
    // A value-taking flag falls back on last-wins when it is repeated; there
    // is no such rule to reach for here, so `--location --location=false` is
    // a guess whichever way it is read — including the reading where the bare
    // token wins and the run turns on a leg that was just asked to be off.
    expect(readBooleanFlag(['--location', '--location=false'], 'location').error).toBeDefined();
    expect(readBooleanFlag(['--location=false', '--location'], 'location').error).toBeDefined();
  });

  it('is unchanged by a repeated bare token', () => {
    // Repetition of the honest spelling is not a mistake to report — it says
    // the same thing twice.
    expect(readBooleanFlag(['--location', '--location'], 'location')).toEqual({ value: true });
  });

  it('leaves a following positional alone, for better and for worse', () => {
    // `--location true` is deliberately NOT refused: the flag reads as on,
    // which is what was asked for, and `true` stays a positional this script
    // has never read.
    expect(readBooleanFlag(['--location', 'true'], 'location')).toEqual({ value: true });
    // `--location false` gets the SAME treatment, and there the operator asked
    // for the leg off and gets it on with nothing said. Pinned deliberately:
    // it is the honest boundary of this change, not a case it covers. It is
    // the unknown-flag gap still recorded at readFlag in the script, whose
    // push-based argv pass subsumes this one — the two are one change.
    expect(readBooleanFlag(['--location', 'false'], 'location')).toEqual({ value: true });
  });

  it('matches the flag name case-sensitively', () => {
    // Documented, not accidental. `--LOCATION=true` is not this flag, so it is
    // neither refused nor read — the same silent-off the `=` spelling had,
    // reachable by a shift-key slip. Flags are case-sensitive everywhere else
    // in this script, and widening the match here would mean deciding what
    // `--FILE` does too.
    expect(readBooleanFlag(['--LOCATION=true'], 'location')).toEqual({ value: false });
    expect(readBooleanFlag(['--Location'], 'location')).toEqual({ value: false });
  });

  it('does not match a longer flag that starts with the same letters', () => {
    // Both halves have to hold: the bare match is an equality and the refusal
    // is prefixed on `--location=`, so neither reaches `--locations`.
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
    // The regression in one line. This used to return
    // `{ includeLocation: false, errors: [] }` — a full unattended window
    // with no location leg, and nothing said about why.
    const { includeLocation, errors } = resolveBooleanArgs(['--location=true']);
    expect(errors).toHaveLength(1);
    // Fail closed on the refused path: main() exits on the collected errors
    // first, so this is the value nothing reads — but it should still be safe.
    expect(includeLocation).toBe(false);
  });

  it('reports a malformed --allow-production in the argument pass', () => {
    // Its VALUE is resolveGuardInputs's to read; only its SHAPE is checked
    // here. That placement is what puts the message in the same pass as a bad
    // --file, instead of a run later out of the guard — after the operator
    // has already fixed the first message and started again.
    expect(resolveBooleanArgs(['--allow-production=1']).errors).toEqual([
      '--allow-production takes no value, got "1" — pass --allow-production on its own to turn it on, or omit it to leave it off',
    ]);
  });

  it('names every bad boolean flag in one pass', () => {
    const { errors } = resolveBooleanArgs(['--location=1', '--allow-production=1']);
    expect(errors).toHaveLength(2);
    // Order pinned too: the report follows the order the resolver reads them,
    // not the order they were typed.
    expect(errors.map((e) => e.split(' ')[0])).toEqual(['--location', '--allow-production']);
  });

  it('reports a malformed --allow-production without disturbing --location', () => {
    // Read independently. The run still refuses, but the report must not also
    // claim the flag that was typed correctly was wrong.
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
    // The scan runs over the whole argv with no idea which tokens are values
    // belonging to a value-taking flag. `--file=--location=true` names a
    // bizarre but legal path; the only thing keeping it out of the refusal is
    // that the match is anchored on `--location=` and this entry starts with
    // `--file=`. Loosen that anchor and a CORRECT command line starts being
    // refused — the one failure this change could introduce.
    expect(resolveBooleanArgs(['--file=--location=true']))
      .toEqual({ includeLocation: false, errors: [] });
    expect(resolveFileArg(['--file=--location=true']).filePath).toBe('--location=true');
  });

  it('reports the boolean and the value-taking flag separately', () => {
    // `--file --location=true` is wrong twice over and both readers say so:
    // readFlag refuses a flag-shaped value, and this refuses the `=`. main()
    // prints both, which is the point of collecting rather than throwing —
    // the operator fixes one command line instead of two in succession.
    const { includeLocation, errors } = resolveBooleanArgs(['--file', '--location=true']);
    expect(includeLocation).toBe(false);
    expect(errors).toHaveLength(1);
    expect(resolveFileArg(['--file', '--location=true']).errors).toHaveLength(1);
  });

  it('reads --allow-production through the same reader as --location', () => {
    // Carried over from the change that consolidated the guard onto the
    // shared boolean reader. resolveGuardInputs takes the UNSLICED
    // process.argv, so this is pinned in that shape too.
    expect(resolveGuardInputs({}, ['node', 'script.js', '--allow-production']).allowProdFlag).toBe(true);
    expect(resolveGuardInputs({}, ['node', 'script.js']).allowProdFlag).toBe(false);
  });

  it('leaves the production override OFF when its shape is refused', () => {
    // The half that reader could not express before it could refuse. The
    // previous change pinned `--allow-production=true` reading as absent and
    // noted it fails SAFE; that is now a refusal, and this asserts the safe
    // half still holds at the guard — `.value === true` is false on the
    // refused path, so the target stays refused rather than cleared. The
    // operator is told why by resolveBooleanArgs, which checks the same
    // flag's shape and whose error main() exits on before the guard runs.
    expect(resolveGuardInputs({}, ['node', 'script.js', '--allow-production=true']).allowProdFlag).toBe(false);
    expect(resolveGuardInputs({}, ['node', 'script.js', '--allow-production=1']).allowProdFlag).toBe(false);
    expect(resolveBooleanArgs(['--allow-production=true']).errors).toHaveLength(1);
  });

  it('still reads the bare flag when a value-taking flag swallowed at it', () => {
    // `--count --location` is the shape readFlag's own comment calls out: it
    // used to consume `--location` as the value AND leave the leg off, so the
    // command line differed from the run in two ways at once. The count is
    // refused, and the flag that was typed still reads as on.
    expect(resolveBooleanArgs(['--count', '--location']).includeLocation).toBe(true);
    expect(resolveNumericArgs(['--count', '--location']).errors).toHaveLength(1);
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
    // Guarded: if mkdtempSync above threw, `dir` is undefined and
    // rmSync(undefined) throws a TypeError that masks the original failure.
    if (dir) fs.rmSync(dir, { recursive: true, force: true });
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

  // Root bypasses the permission bits entirely, and Windows chmod does not
  // remove read access — under either, this would assert the opposite of what
  // it says. it.skip rather than an early `return`, so the run REPORTS that
  // it did not execute: a test that quietly passes without asserting is the
  // same "did nothing, looked like it worked" shape this whole file is about.
  const permissionsApply = process.platform !== 'win32' && process.getuid?.() !== 0;
  (permissionsApply ? it : it.skip)('reports a file that exists but cannot be read', () => {
    // Existence is not readability — a file owned by another user is the
    // realistic way this bites, and statSync succeeds on it.
    const locked = path.join(dir, 'locked.bin');
    fs.writeFileSync(locked, 'x');
    fs.chmodSync(locked, 0o000);
    try {
      const message = checkUploadFile(locked);
      expect(message).toContain('is not readable');
      // The third branch's flag prefix, which the message test above cannot
      // reach without a locked file.
      expect(message).toMatch(/^--file /);
    } finally {
      fs.chmodSync(locked, 0o600);
    }
  });

  it('rejects a non-regular file that is not a directory', () => {
    // Mutation this kills: `!stats.isFile()` -> `stats.isDirectory()`. Every
    // other assertion in this block still passes under it, but a character
    // device, socket or FIFO then reaches fs.readFileSync inside the round —
    // and runRound re-reads once per round, so a pipe uploads real bytes on
    // round one and nothing after. A FIFO with no writer blocks forever,
    // which is the hang this file's header exists to prevent.
    expect(checkUploadFile('/dev/null')).toContain('is not a regular file');
  });

  it('follows a symlink to a real file', () => {
    // Mutation this kills: statSync -> lstatSync. Passes every other
    // assertion here while rejecting a symlink that points at a perfectly
    // good payload, because readFileSync would have followed it.
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
    // resolveFileArg deliberately PRESERVES a leading or trailing space in a
    // real filename, so this is where such a path surfaces. Rendered raw,
    // `--file /tmp/x/ spaced  is not a regular file` reads as `/tmp/x/spaced`
    // and sends the operator after the wrong file.
    expect(checkUploadFile(path.join(dir, ' spaced '))).toContain(`"${path.join(dir, ' spaced ')}"`);
  });
});

describe('loadtest preflight — the composition main() used to hold inline', () => {
  // Every case here corresponds to a mutation that passed the ENTIRE suite
  // before resolveArgErrors was extracted. The unit tests above all stayed
  // green while the run silently went back to doing the wrong thing, because
  // the only thing wiring them together lived in main(), which no test can
  // reach. Counting call sites could not see any of them.

  const never = () => null;

  it('carries the --file errors, not only the numeric ones', () => {
    // Mutation: `[...numericErrors]`, dropping the file half. Every
    // resolveFileArg test stays green while `--file ""`, `--file=`, `--file`
    // as a final token and `--file --location` all fall back to the generated
    // 1MB payload — exactly the bug this change exists to remove.
    expect(resolveArgErrors(['--file'], never)).toEqual([
      expect.stringContaining('--file'),
    ]);
    expect(resolveArgErrors(['--file', '--location'], never)).toHaveLength(1);
  });

  it('surfaces what the readability check reported', () => {
    // Mutation: call the checker and discard its return. Green suite, ENOENT
    // back inside the first round.
    expect(resolveArgErrors(['--file', '/some/path'], () => 'boom')).toEqual(['boom']);
  });

  it('does not touch the filesystem when no --file was given', () => {
    // Mutation: drop the filePath guard. fs.statSync(null) throws
    // ERR_INVALID_ARG_TYPE, which the catch turns into
    // `--file null cannot be read` on EVERY default run.
    let called = 0;
    const errors = resolveArgErrors([], () => { called += 1; return 'should not run'; });
    expect(called).toBe(0);
    expect(errors).toEqual([]);
  });

  it('does not stat a path the operator never typed', () => {
    // A --file whose SHAPE already failed resolves to null; statting it would
    // append a second message naming that null.
    let seen = 'untouched';
    resolveArgErrors(['--file', ''], (candidate) => { seen = candidate; return null; });
    expect(seen).toBe('untouched');
  });

  it('reports every fault in one pass, numeric flags before the file flag', () => {
    // The order is numeric-then-file, NOT argv order — the concatenation is
    // fixed and the readability error is appended last. Pinned with --file
    // typed FIRST, because the obvious argv-order reading of this list is
    // wrong and only that case can tell the two apart. Cosmetic either way:
    // every message is fatal and they print together, so the operator fixes
    // them in one edit.
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
    // Mutation: dropping `...booleanErrors` from the composition. Every
    // readBooleanFlag and resolveBooleanArgs case above stays green while
    // `--location=true` goes back to running the whole window with the leg
    // off, printing `Location: false` — the exact defect this change removes,
    // reinstated by one spread. This is the seam that makes it reachable at
    // all; before resolveArgErrors was extracted it lived in main().
    expect(resolveArgErrors(['--location=true'], () => null)).toEqual([
      expect.stringContaining('--location takes no value'),
    ]);
    expect(resolveArgErrors(['--allow-production=1'], () => null)).toHaveLength(1);
  });

  it('names a bad boolean flag alongside a bad numeric one', () => {
    // The one-pass claim across resolvers rather than within one. Boolean
    // errors are appended after the numeric and file ones, matching the fixed
    // concatenation order the test above pins.
    expect(resolveArgErrors(['--count', 'abc', '--location=1'], () => null)).toEqual([
      expect.stringContaining('--count'),
      expect.stringContaining('--location'),
    ]);
  });

  it('defaults to the real readability check', () => {
    // The injected checker is a test seam, not the production path. If the
    // default parameter were dropped, main() would pass no checker and the
    // preflight would silently become a no-op — so this is what pins the
    // wiring that the AST call-count deliberately cannot.
    expect(resolveArgErrors(['--file', '/nonexistent/loadtest/payload.bin'])).toEqual([
      expect.stringContaining('cannot be read'),
    ]);
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

  // Every `--`-prefixed string literal in the file, in sorted order and
  // deliberately NOT de-duplicated. The shared readers build their token from
  // the flag NAME, so a `--`-prefixed literal is written only by an ad-hoc
  // read or by prose — and the ad-hoc read is the defect itself.
  //
  // Keeping duplicates is what lets this catch an ad-hoc read of a flag whose
  // name already appears for some other reason. De-duplicating collapsed the
  // two `--allow-production` literals that used to exist — the guard's own
  // `includes` and the warning text below — into one entry, so a check
  // asserting that single entry could not have told them apart.
  //
  // A token built as `` `--${'dry-run'}` `` is a TemplateLiteral and still
  // escapes; not a plausible accident.
  const flagLiterals = () => {
    const found = [];
    traverse(ast, {
      StringLiteral(p) { if (/^--\w/.test(p.node.value)) found.push(p.node.value); },
    });
    return found.sort();
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
    // parse instead of going through the resolver. Two call sites: the
    // module-level constants, and resolveArgErrors, which re-resolves from
    // the argv it is handed so the whole preflight decision stays reachable
    // from a test.
    expect(callsNamed('resolveNumericArgs')).toHaveLength(2);
  });

  it('scans argv through the shared reader and nothing else', () => {
    // Bans the PRIMITIVE, not a name. Counting readFlag call sites cannot
    // catch the regression that actually matters: a new flag reading argv
    // with its own inline `const i = args.indexOf('--warmup')` leaves the
    // readFlag count untouched and stays green while that flag silently
    // defaults — which is how this file came to hold three parsers in the
    // first place. Same reasoning as the parseInt ban above, and the same
    // regression a name-based check misses.
    //
    // There is no indexOf call site today; the only occurrence of the word
    // is inside a comment, which does not parse as a call.
    expect(callsNamed('indexOf')).toHaveLength(0);
    // A name pin as well, since getArg's `args[idx + 1] || defaultVal` is the
    // defect itself. Weak alone — the identical body under any other name
    // passes it — which is precisely what the indexOf ban above covers.
    expect(callsNamed('getArg')).toHaveLength(0);
    expect(callsNamed('readFlag')).toHaveLength(2);
  });

  it('reads every boolean flag through the one shared reader', () => {
    // The counterpart to the readFlag/indexOf ban above, and asserted as a
    // literal search rather than a call count for the same reason that one
    // bans the primitive: a fresh boolean flag added as
    // `args.includes('--newflag')` leaves readBooleanFlag's call count
    // untouched, and that inline shape is precisely how --location came to
    // read `--location=true` as absent.
    //
    // Deliberately not scoped to `.includes` either. The silent-off bug
    // arrives just as easily through `.some((a) => a === '--x')` or
    // `new Set(args).has('--x')`, neither of which the indexOf ban sees.
    // Matching the LITERAL rather than the call is what makes this
    // independent of which one someone reaches for.
    //
    // ONE entry, and it is not a read at all: it is the warning text in
    // targetGuardReport naming which override let a refused target through
    // (`--allow-production` vs `LOADTEST_ALLOW_PRODUCTION=1`). The guard's own
    // read used to add a second copy of the same token and no longer does,
    // now that it goes through the shared reader.
    //
    // Because duplicates are kept, that residual prose entry does not become
    // a hiding place: an ad-hoc `args.includes('--allow-production')` would
    // make this two entries and fail. A third boolean flag added the intended
    // way — `read('dry-run')` inside resolveBooleanArgs — writes 'dry-run'
    // without the dashes and leaves this list alone.
    //
    // `/^--\w/` so readFlag's own `'--'` prefix test and the `'---'` console
    // divider are not swept up.
    expect(flagLiterals()).toEqual(['--allow-production']);
    // Two call sites: resolveBooleanArgs, and resolveGuardInputs reading the
    // production override through the same reader rather than its own scan.
    expect(callsNamed('readBooleanFlag')).toHaveLength(2);
    // Two as well, like resolveNumericArgs and resolveFileArg above: the
    // module-level constant, and resolveArgErrors re-resolving from the argv
    // it is handed so the whole preflight decision stays reachable.
    expect(callsNamed('resolveBooleanArgs')).toHaveLength(2);
    // hasFlag was the boolean half of the getArg defect: an exact-token
    // `includes` cannot express "flag present, carrying a value it should not
    // have", so `--location=true` read as absent and the leg stayed off.
    expect(callsNamed('hasFlag')).toHaveLength(0);
  });

  it('resolves the upload flag and the whole preflight in one place each', () => {
    expect(callsNamed('resolveFileArg')).toHaveLength(2);
    expect(callsNamed('resolveArgErrors')).toHaveLength(1);
    // checkUploadFile is deliberately NOT call-counted: it is now referenced
    // as resolveArgErrors' default parameter rather than called by name, so a
    // call count would read 0 whether or not it is wired. The runtime test
    // 'defaults to the real readability check' is what pins that, and it is
    // the stronger check — it proves the default resolves to a real stat.
  });

  it('decides every argument error before the smoke test mints a resource', () => {
    // The one regression the call counts cannot see: move the preflight below
    // the smoke test. Every unit test stays green while "prove the upload
    // file is readable BEFORE the run starts" — the entire stated value of
    // the check — is gone, because createOneTimeLink has by then minted a
    // live resource.
    //
    // Scoped to main()'s body on purpose: createOneTimeLink is called twice,
    // and the FIRST one lexically is the location leg inside runRound. A
    // whole-file comparison would silently measure the wrong call.
    let main = null;
    traverse(ast, {
      FunctionDeclaration(p) { if (p.node.id?.name === 'main') main = p.node; },
    });
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
