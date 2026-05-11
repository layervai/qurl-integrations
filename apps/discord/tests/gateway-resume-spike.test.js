/**
 * Tests for the session-store helpers in scripts/gateway-resume-spike.js.
 *
 * The spike is a manual validation harness against a real Discord token,
 * but its persistence helpers are pure functions worth pinning so that:
 *
 *   - a future @discordjs/ws bump that renames the SessionInfo shape
 *     fails our shape validator (load returns null instead of yielding
 *     a malformed shape to retrieveSessionInfo, which would surface as
 *     a confusing Discord protocol error);
 *   - the atomic-rename invariant doesn't regress (the spike runs locally
 *     so a SIGKILL mid-write is plausible);
 *   - the malformed-file → null fallback stays in place (the spike must
 *     boot even if the store is wedged, exactly mirroring what the prod
 *     DDB-backed equivalent has to do).
 */

const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');

const { loadSession, persistSession, clearSession, classifyResult } = require('../scripts/gateway-resume-spike');

function tempPath(name) {
  return path.join(os.tmpdir(), `spike-test-${process.pid}-${Date.now()}-${name}.json`);
}

// File-scope spy restoration — applies to every describe in this file
// (not just the outer one). The earlier per-describe afterEach only
// covered the session-store-helpers describe; spy leaks under the
// classifyResult / file-perms describes would silently survive even
// though those describes don't currently use spies. Hoisting closes
// the future-foot-gun.
//
// NOT set globally in jest.config.js because other test files rely on
// jest.spyOn persistence within their own describes — see the comment
// on `restoreMocks` in jest.config.js.
afterEach(() => {
  jest.restoreAllMocks();
});

describe('gateway-resume-spike — session store helpers', () => {
  let testFile;

  beforeEach(() => {
    testFile = tempPath('session');
  });

  afterEach(() => {
    try { fs.unlinkSync(testFile); } catch (_) { /* ok */ }
    try { fs.unlinkSync(`${path.resolve(testFile)}.tmp`); } catch (_) { /* ok */ }
  });

  describe('persistSession + loadSession round-trip', () => {
    test('persists then loads the SessionInfo shape @discordjs/ws expects', () => {
      const info = {
        sessionId: '0123456789abcdef',
        resumeURL: 'wss://gateway-us-east1-d.discord.gg/',
        sequence: 42,
      };

      persistSession(info, testFile);
      const loaded = loadSession(testFile);

      expect(loaded).toEqual(info);
    });

    test('persists additional SessionInfo fields without dropping them', () => {
      // @discordjs/ws's SessionInfo includes resumeURL, sessionId, sequence,
      // and (in newer versions) shardId/shardCount. We forward whatever the
      // updateSessionInfo callback hands us. Pin that we don't strip fields.
      const info = {
        sessionId: 'abc',
        resumeURL: 'wss://example/',
        sequence: 99,
        shardId: 0,
        shardCount: 1,
      };

      persistSession(info, testFile);
      const loaded = loadSession(testFile);

      expect(loaded).toEqual(info);
    });
  });

  describe('loadSession — fallback paths', () => {
    test('returns null when the file does not exist (ENOENT)', () => {
      expect(loadSession(testFile)).toBeNull();
    });

    test('returns null when the file is malformed JSON', () => {
      fs.writeFileSync(testFile, '{not valid json');
      expect(loadSession(testFile)).toBeNull();
    });

    test('returns null when the file is missing sessionId', () => {
      // A partial write that landed mid-shape. retrieveSessionInfo handing
      // this to Discord would produce an opaque protocol error; better to
      // fall back to IDENTIFY.
      fs.writeFileSync(testFile, JSON.stringify({ resumeURL: 'wss://x/', sequence: 1 }));
      expect(loadSession(testFile)).toBeNull();
    });

    test('returns null when sequence is not a number', () => {
      fs.writeFileSync(testFile, JSON.stringify({
        sessionId: 'abc', resumeURL: 'wss://x/', sequence: 'not-a-number',
      }));
      expect(loadSession(testFile)).toBeNull();
    });

    test('returns null for empty file (edge case of write-truncated-then-died)', () => {
      fs.writeFileSync(testFile, '');
      expect(loadSession(testFile)).toBeNull();
    });

    test('propagates non-ENOENT/SyntaxError IO failures so they surface', () => {
      // EACCES would indicate a real environment problem (perms wrong on
      // the file, disk-level issue). The helper deliberately doesn't
      // swallow these — better to fail loud than silently fall back to
      // IDENTIFY and bury the underlying problem.
      //
      // jest.spyOn + mockImplementation + restoreAllMocks (afterEach) is
      // safer than direct monkey-patching of fs.readFileSync: if the test
      // body throws mid-assertion, jest's restoreAllMocks still runs.
      // Direct monkey-patching leaks the patched fn to subsequent tests
      // in the same file under that failure mode.
      jest.spyOn(fs, 'readFileSync').mockImplementation(() => {
        const err = new Error('permission denied');
        err.code = 'EACCES';
        throw err;
      });
      expect(() => loadSession(testFile)).toThrow(/permission denied/);
    });
  });

  describe('persistSession — atomic write', () => {
    test('uses a .tmp sidecar then rename (no half-written target on crash)', () => {
      // We can't actually SIGKILL the test process mid-call, but we can
      // observe that no .tmp file remains after success (proving rename
      // happened) AND that the target file's content is complete.
      const info = { sessionId: 's', resumeURL: 'wss://x/', sequence: 1 };
      persistSession(info, testFile);

      const tmpPath = `${path.resolve(testFile)}.tmp`;
      expect(fs.existsSync(tmpPath)).toBe(false);
      expect(JSON.parse(fs.readFileSync(testFile, 'utf8'))).toEqual(info);
    });

    test('overwriting an existing session is also atomic', () => {
      // First write.
      persistSession({ sessionId: 'old', resumeURL: 'wss://x/', sequence: 1 }, testFile);
      // Second write replaces cleanly.
      persistSession({ sessionId: 'new', resumeURL: 'wss://y/', sequence: 2 }, testFile);

      expect(loadSession(testFile)).toEqual({
        sessionId: 'new', resumeURL: 'wss://y/', sequence: 2,
      });
      expect(fs.existsSync(`${path.resolve(testFile)}.tmp`)).toBe(false);
    });
  });

  describe('clearSession', () => {
    test('removes the file if it exists', () => {
      persistSession({ sessionId: 's', resumeURL: 'wss://x/', sequence: 1 }, testFile);
      expect(fs.existsSync(testFile)).toBe(true);

      clearSession(testFile);
      expect(fs.existsSync(testFile)).toBe(false);
    });

    test('is a no-op when the file does not exist (idempotent)', () => {
      // No throw; safe to call before phase1 starts.
      expect(() => clearSession(testFile)).not.toThrow();
    });
  });
});

describe('gateway-resume-spike — @discordjs/ws option contract', () => {
  // These tests don't run the spike against Discord. They pin that the
  // library surface we depend on (the names retrieveSessionInfo /
  // updateSessionInfo, the SessionInfo shape) is what @discordjs/ws's
  // installed version exposes. A library bump that renames these options
  // fails here before anyone runs the real spike against Discord and gets
  // a confusing protocol error.

  test('@discordjs/ws exports WebSocketManager', () => {
    const ws = require('@discordjs/ws');
    expect(typeof ws.WebSocketManager).toBe('function');
  });

  test('@discordjs/ws exposes WebSocketShardEvents.Dispatch', () => {
    const { WebSocketShardEvents } = require('@discordjs/ws');
    expect(WebSocketShardEvents.Dispatch).toBeDefined();
  });

  test('constructing WebSocketManager accepts retrieveSessionInfo + updateSessionInfo', async () => {
    // We construct but DO NOT connect — no network call. The constructor
    // accepting our callback shape without throwing is what we want to
    // pin.
    //
    // destroy() at the end with try/finally: if @discordjs/ws ever opens
    // an internal timer/keepalive at construction time, this prevents
    // the test from leaking a handle. destroy() is a no-op when no
    // connection exists, so it's safe even though we never connect.
    const { WebSocketManager } = require('@discordjs/ws');
    const { REST } = require('@discordjs/rest');
    const rest = new REST().setToken('not-a-real-token');

    const mgr = new WebSocketManager({
      token: 'not-a-real-token',
      intents: 0,
      rest,
      retrieveSessionInfo: () => null,
      updateSessionInfo: () => {},
    });

    try {
      expect(mgr).toBeDefined();
    } finally {
      await mgr.destroy({ code: 1000 }).catch(() => { /* no connection, harmless */ });
    }
  });
});

describe('gateway-resume-spike — persisted file permissions', () => {
  // Pins that persistSession writes with mode 0o600 (owner-only). The
  // file contains a live Discord session_id + resume_url; within the
  // ~60s buffer window, an unauthorized reader could RESUME the bot
  // session and impersonate it. Even on a dev machine this is sloppy.
  // Skipped on non-Unix where mode bits don't map cleanly.
  const isUnix = process.platform !== 'win32';

  (isUnix ? test : test.skip)('persistSession writes file with mode 0o600 on fresh create', () => {
    const filePath = tempPath('mode-check');
    try {
      persistSession({ sessionId: 'x', resumeURL: 'wss://x/', sequence: 1 }, filePath);
      const mode = fs.statSync(filePath).mode & 0o777;
      expect(mode).toBe(0o600);
    } finally {
      try { fs.unlinkSync(filePath); } catch (_) { /* ok */ }
    }
  });

  (isUnix ? test : test.skip)('persistSession enforces mode 0o600 even when .tmp already exists', () => {
    // Node's fs.writeFileSync `mode` option is only honored on file
    // CREATION. If a previous crashed phase1 left a `.tmp` behind, the
    // writeFileSync would silently preserve whatever perms the existing
    // .tmp had. The chmodSync call after writeFileSync covers this; the
    // test pre-seeds a world-readable .tmp and asserts the post-rename
    // file lands at 0o600 regardless.
    const filePath = tempPath('mode-overwrite');
    const tmpPath = `${path.resolve(filePath)}.tmp`;
    try {
      // Pre-seed the .tmp with permissive perms — simulates a crashed
      // previous run that left a stale .tmp around.
      fs.writeFileSync(tmpPath, 'stale-data', { mode: 0o644 });
      fs.chmodSync(tmpPath, 0o644); // belt-and-suspenders since umask can shift mode

      persistSession({ sessionId: 'y', resumeURL: 'wss://y/', sequence: 2 }, filePath);

      const mode = fs.statSync(filePath).mode & 0o777;
      expect(mode).toBe(0o600);
    } finally {
      try { fs.unlinkSync(filePath); } catch (_) { /* ok */ }
      try { fs.unlinkSync(tmpPath); } catch (_) { /* ok */ }
    }
  });
});

describe('gateway-resume-spike — classifyResult', () => {
  // Pure logic over four booleans. Order is load-bearing: budgetExhausted
  // must be checked BEFORE identified-without-budget because READY fires
  // before the second retrieveSessionInfo throws, so both `identified`
  // and `budgetExhausted` can be true on the same run. The earlier
  // (in-place) classification block had this order reversed and the
  // budget-exhausted exit-code 3 was unreachable. These tests pin the
  // current order against future regressions.

  test('resumed wins over everything (exit 0, "RESUME-OK")', () => {
    const r = classifyResult({
      resumed: true,
      budgetExhausted: false,
      identified: false,
      postResumeSessionCleared: false,
    });
    expect(r.exitCode).toBe(0);
    expect(r.severity).toBe('log');
    expect(r.lines.join('\n')).toMatch(/RESUME-OK/);
  });

  test('resumed wins even with all-flags-set input (defensive only, not reachable today)', () => {
    // A successful RESUME implies no IDENTIFY was ever attempted, so
    // `resumed && budgetExhausted` cannot co-occur on a real run.
    // The test exists as a guardrail against future state-machine
    // drift — e.g., a refactor that splits resume-tracking from the
    // dispatch flow and accidentally lets both flags land true. If
    // that happens, the success classification should win.
    const r = classifyResult({
      resumed: true,
      budgetExhausted: true,
      identified: true,
      postResumeSessionCleared: true,
    });
    expect(r.exitCode).toBe(0);
    expect(r.lines.join('\n')).toMatch(/RESUME-OK/);
  });

  test('budgetExhausted reachable when identified+postResumeSessionCleared also set', () => {
    // This is the exact bug the reorder fixed: in the real spike run
    // against a contending sandbox bot, READY fires (→ identified=true,
    // postResumeSessionCleared=true) BEFORE the second retrieveSessionInfo
    // throws (→ budgetExhausted=true). The old order matched
    // identified-fallback first and exit 3 was unreachable.
    const r = classifyResult({
      resumed: false,
      budgetExhausted: true,
      identified: true,
      postResumeSessionCleared: true,
    });
    expect(r.exitCode).toBe(3);
    expect(r.severity).toBe('error');
    expect(r.lines.join('\n')).toMatch(/IDENTIFY-budget-exhausted/);
    expect(r.lines.join('\n')).toMatch(/token contention/);
  });

  test('graceful IDENTIFY-fallback when RESUME rejected but IDENTIFY succeeds', () => {
    const r = classifyResult({
      resumed: false,
      budgetExhausted: false,
      identified: true,
      postResumeSessionCleared: true,
    });
    expect(r.exitCode).toBe(0);
    expect(r.severity).toBe('log');
    expect(r.lines.join('\n')).toMatch(/IDENTIFY-fallback/);
  });

  test('unclear when no signal fires within timeout', () => {
    const r = classifyResult({
      resumed: false,
      budgetExhausted: false,
      identified: false,
      postResumeSessionCleared: false,
    });
    expect(r.exitCode).toBe(2);
    expect(r.severity).toBe('error');
    expect(r.lines.join('\n')).toMatch(/UNCLEAR/);
  });

  test('budgetExhausted alone (RESUME hung, no READY, retrieveSessionInfo threw)', () => {
    // The exact bug pattern the reorder fix targets in a different
    // shape: RESUME never completed (resumed=false), Discord never
    // delivered a READY (identified=false), library didn't clear the
    // session (postResumeSessionCleared=false), but our budget guard
    // tripped because retrieveSessionInfo kept being called and
    // eventually crossed MAX. Today this hits exit-3 via the budget
    // branch; pin it so a future reorder that moves budgetExhausted
    // below the resumed/identified branches regresses here.
    const r = classifyResult({
      resumed: false,
      budgetExhausted: true,
      identified: false,
      postResumeSessionCleared: false,
    });
    expect(r.exitCode).toBe(3);
    expect(r.severity).toBe('error');
    expect(r.lines.join('\n')).toMatch(/IDENTIFY-budget-exhausted/);
  });

  test('postResumeSessionCleared alone (without identified) is still UNCLEAR', () => {
    // postResumeSessionCleared without identified means: Discord
    // rejected RESUME but no fresh IDENTIFY's READY arrived either.
    // The graceful-fallback branch requires BOTH flags; without the
    // identified flag we don't know if IDENTIFY worked, so the result
    // is genuinely unclear.
    const r = classifyResult({
      resumed: false,
      budgetExhausted: false,
      identified: false,
      postResumeSessionCleared: true,
    });
    expect(r.exitCode).toBe(2);
    expect(r.lines.join('\n')).toMatch(/UNCLEAR/);
  });
});
