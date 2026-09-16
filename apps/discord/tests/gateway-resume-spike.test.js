
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');

const { loadSession, persistSession, clearSession, classifyResult } = require('../scripts/gateway-resume-spike');

function tempPath(name) {
  return path.join(os.tmpdir(), `spike-test-${process.pid}-${Date.now()}-${name}.json`);
}

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
      const info = { sessionId: 's', resumeURL: 'wss://x/', sequence: 1 };
      persistSession(info, testFile);

      const tmpPath = `${path.resolve(testFile)}.tmp`;
      expect(fs.existsSync(tmpPath)).toBe(false);
      expect(JSON.parse(fs.readFileSync(testFile, 'utf8'))).toEqual(info);
    });

    test('overwriting an existing session is also atomic', () => {
      persistSession({ sessionId: 'old', resumeURL: 'wss://x/', sequence: 1 }, testFile);
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
      expect(() => clearSession(testFile)).not.toThrow();
    });
  });
});

describe('gateway-resume-spike — @discordjs/ws option contract', () => {

  test('@discordjs/ws exports WebSocketManager', () => {
    const ws = require('@discordjs/ws');
    expect(typeof ws.WebSocketManager).toBe('function');
  });

  test('@discordjs/ws exposes WebSocketShardEvents.Dispatch', () => {
    const { WebSocketShardEvents } = require('@discordjs/ws');
    expect(WebSocketShardEvents.Dispatch).toBeDefined();
  });

  test('constructing WebSocketManager accepts retrieveSessionInfo + updateSessionInfo', async () => {
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
    const filePath = tempPath('mode-overwrite');
    const tmpPath = `${path.resolve(filePath)}.tmp`;
    try {
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
