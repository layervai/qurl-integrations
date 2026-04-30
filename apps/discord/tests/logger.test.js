/**
 * Tests for src/logger.js
 */

describe('logger', () => {
  let logger;
  let originalLogLevel;
  let consoleSpy;

  beforeEach(() => {
    originalLogLevel = process.env.LOG_LEVEL;
    // Reset modules to pick up env changes
    jest.resetModules();
    consoleSpy = {
      error: jest.spyOn(console, 'error').mockImplementation(),
      warn: jest.spyOn(console, 'warn').mockImplementation(),
      log: jest.spyOn(console, 'log').mockImplementation(),
    };
  });

  afterEach(() => {
    if (originalLogLevel !== undefined) {
      process.env.LOG_LEVEL = originalLogLevel;
    } else {
      delete process.env.LOG_LEVEL;
    }
    jest.restoreAllMocks();
  });

  it('info level logs info, warn, error but not debug', () => {
    process.env.LOG_LEVEL = 'info';
    logger = require('../src/logger');

    logger.error('err msg', { key: 'val' });
    logger.warn('warn msg');
    logger.info('info msg');
    logger.debug('debug msg');

    expect(consoleSpy.error).toHaveBeenCalledTimes(1);
    expect(consoleSpy.error.mock.calls[0][0]).toContain('ERROR: err msg');
    expect(consoleSpy.warn).toHaveBeenCalledTimes(1);
    expect(consoleSpy.warn.mock.calls[0][0]).toContain('WARN: warn msg');
    expect(consoleSpy.log).toHaveBeenCalledTimes(1); // only info, not debug
    expect(consoleSpy.log.mock.calls[0][0]).toContain('INFO: info msg');
  });

  it('debug level logs everything', () => {
    process.env.LOG_LEVEL = 'debug';
    logger = require('../src/logger');

    logger.error('e');
    logger.warn('w');
    logger.info('i');
    logger.debug('d');

    expect(consoleSpy.error).toHaveBeenCalledTimes(1);
    expect(consoleSpy.warn).toHaveBeenCalledTimes(1);
    expect(consoleSpy.log).toHaveBeenCalledTimes(2); // info + debug
  });

  it('error level only logs errors', () => {
    process.env.LOG_LEVEL = 'error';
    logger = require('../src/logger');

    logger.error('e');
    logger.warn('w');
    logger.info('i');
    logger.debug('d');

    expect(consoleSpy.error).toHaveBeenCalledTimes(1);
    expect(consoleSpy.warn).not.toHaveBeenCalled();
    expect(consoleSpy.log).not.toHaveBeenCalled();
  });

  it('warn level logs warn and error', () => {
    process.env.LOG_LEVEL = 'warn';
    logger = require('../src/logger');

    logger.error('e');
    logger.warn('w');
    logger.info('i');

    expect(consoleSpy.error).toHaveBeenCalledTimes(1);
    expect(consoleSpy.warn).toHaveBeenCalledTimes(1);
    expect(consoleSpy.log).not.toHaveBeenCalled();
  });

  it('defaults to info level when LOG_LEVEL is not set', () => {
    delete process.env.LOG_LEVEL;
    logger = require('../src/logger');

    logger.info('i');
    logger.debug('d');

    expect(consoleSpy.log).toHaveBeenCalledTimes(1);
  });

  it('includes meta as JSON when provided', () => {
    process.env.LOG_LEVEL = 'info';
    logger = require('../src/logger');

    logger.info('test', { foo: 'bar' });

    expect(consoleSpy.log.mock.calls[0][0]).toContain('{"foo":"bar"}');
  });

  it('omits meta string when no meta keys', () => {
    process.env.LOG_LEVEL = 'info';
    logger = require('../src/logger');

    logger.info('test');

    const output = consoleSpy.log.mock.calls[0][0];
    expect(output).toContain('INFO: test');
    // Should not have trailing JSON
    expect(output).not.toContain('{}');
  });

  it('includes ISO timestamp in output', () => {
    process.env.LOG_LEVEL = 'info';
    logger = require('../src/logger');

    logger.info('ts-test');

    const output = consoleSpy.log.mock.calls[0][0];
    // ISO format: [2026-...T...Z]
    expect(output).toMatch(/\[\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/);
  });

  describe('audit', () => {
    it('emits a parseable JSON line with event, agent, and ts', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.audit('upload_success', { send_id: 'abc', count: 3 });

      expect(consoleSpy.log).toHaveBeenCalledTimes(1);
      const line = consoleSpy.log.mock.calls[0][0];
      const parsed = JSON.parse(line);
      expect(parsed.audit.event).toBe('upload_success');
      expect(parsed.audit.agent).toBe('discord');
      expect(parsed.audit.send_id).toBe('abc');
      expect(parsed.audit.count).toBe(3);
      expect(parsed.ts).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/);
    });

    it('bypasses currentLevel (emits even at error level)', () => {
      process.env.LOG_LEVEL = 'error';
      logger = require('../src/logger');

      logger.audit('dispatch_sent', { send_id: 'x' });

      expect(consoleSpy.log).toHaveBeenCalledTimes(1);
      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.event).toBe('dispatch_sent');
    });

    it('does not redact meta keys whose names match REDACT_SUBSTRINGS', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      // redact() matches on KEY name, not value. A future audit meta
      // field like `tokens_minted` or `token_count` would otherwise
      // get blanked — which would corrupt a CloudWatch metric
      // dimension. Verify audit() bypasses redact for these keys.
      logger.audit('upload_success', { tokens_minted: 7, send_id: 'send-1' });

      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.tokens_minted).toBe(7);
      expect(parsed.audit.send_id).toBe('send-1');
    });

    it('emits a fallback audit_serialization_failed event when JSON.stringify throws', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      // Circular ref — JSON.stringify throws TypeError on the primary
      // emission. audit() must (a) not propagate that to the caller
      // (would fail per-recipient dispatch in batchSettled), and
      // (b) still emit a CloudWatch-visible audit line so the gap is
      // discoverable via metric filter, not just a free-text error log.
      const circ = {};
      circ.self = circ;
      expect(() => logger.audit('upload_success', { circ })).not.toThrow();

      // Fallback audit line was emitted with the synthetic event name.
      const auditLines = consoleSpy.log.mock.calls.map(c => {
        try { return JSON.parse(c[0]); } catch { return null; }
      }).filter(Boolean);
      const fallback = auditLines.find(l => l.audit && l.audit.event === 'audit_serialization_failed');
      expect(fallback).toBeDefined();
      expect(fallback.audit.agent).toBe('discord');
      expect(fallback.audit.original_event).toBe('upload_success');
      expect(typeof fallback.audit.reason).toBe('string');
    });

    it('warns but still emits when meta contains a secret-shaped key', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      // Defense-in-depth — the contract is "callers MUST not pass
      // secrets," and audit() does not redact. The warn surfaces the
      // violation as a CloudWatch-visible error log so a misbehaving
      // caller is catchable in dashboards rather than silent. The
      // value still emits unredacted because dropping would corrupt
      // any legitimate dimension a CloudWatch filter is keying on.
      logger.audit('upload_success', { send_id: 's1', auth_token: 'sk-abc123' });

      // Warn fired
      expect(consoleSpy.error).toHaveBeenCalled();
      expect(consoleSpy.error.mock.calls[0][0]).toContain('secret-shaped key');
      expect(consoleSpy.error.mock.calls[0][0]).toContain('auth_token');
      // Audit line still emitted with the value verbatim
      expect(consoleSpy.log).toHaveBeenCalled();
      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.auth_token).toBe('sk-abc123');
    });

    it('warns on secret-shaped keys nested inside a meta object', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      // Top-level-only iteration would miss this. Recursion is the
      // whole point of the defense-in-depth — a caller passing
      // `{ context: { auth_token: '...' } }` is the most realistic
      // accidental-leak path (e.g., dumping an error context object).
      logger.audit('upload_success', { send_id: 's1', context: { auth_token: 'sk-nested' } });

      expect(consoleSpy.error).toHaveBeenCalled();
      expect(consoleSpy.error.mock.calls[0][0]).toContain('auth_token');
      // Value still emits — audit doesn't redact, by contract.
      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.context.auth_token).toBe('sk-nested');
    });

    it('warns on secret-shaped keys nested inside an array element', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.audit('upload_success', {
        send_id: 's1',
        history: [{ ts: 1, password: 'p1' }],
      });

      expect(consoleSpy.error).toHaveBeenCalled();
      expect(consoleSpy.error.mock.calls[0][0]).toContain('password');
    });

    it('does NOT warn for legitimate dimension keys that contain "token" as a substring', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      // tokens_minted contains "token" as a substring but is NOT in
      // AUDIT_SECRET_KEYS — it's a legitimate audit dimension. The
      // warn must use exact-match (not substring) so this kind of
      // key doesn't trigger a false positive every emission.
      logger.audit('upload_success', { send_id: 's1', tokens_minted: 7, token_count: 3 });

      // No warn for these substring-matching but non-secret keys.
      expect(consoleSpy.error).not.toHaveBeenCalled();
      // Values emit verbatim.
      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.tokens_minted).toBe(7);
      expect(parsed.audit.token_count).toBe(3);
    });

    it('agent is not overridable via meta', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.audit('upload_success', { agent: 'slack', send_id: 'x' });

      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.agent).toBe('discord');
    });

    it('handles missing meta gracefully', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.audit('revoke_success');

      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.event).toBe('revoke_success');
      expect(parsed.audit.agent).toBe('discord');
    });

    it('coerces null meta to {} so Object.keys() does not throw', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      // Default-param `meta = {}` only fires for `undefined`. A caller
      // doing `logger.audit('x', someObj?.meta)` could easily pass null.
      // The coerce-to-{} guard inside audit() must run BEFORE the
      // Object.keys(meta) loop, otherwise null would crash audit() and
      // bubble out of batchSettled — defeating the never-throw contract.
      expect(() => logger.audit('dispatch_sent', null)).not.toThrow();
      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.event).toBe('dispatch_sent');
      expect(parsed.audit.agent).toBe('discord');
    });

    it('coerces non-object meta (string, number) to {} without crashing', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      expect(() => logger.audit('dispatch_sent', 'oops-stringly-typed')).not.toThrow();
      expect(() => logger.audit('dispatch_sent', 42)).not.toThrow();
      // Both emit a clean audit line with just event + agent.
      const lines = consoleSpy.log.mock.calls.map(c => JSON.parse(c[0]));
      expect(lines).toHaveLength(2);
      for (const line of lines) {
        expect(line.audit.event).toBe('dispatch_sent');
        expect(line.audit.agent).toBe('discord');
      }
    });
  });
});
