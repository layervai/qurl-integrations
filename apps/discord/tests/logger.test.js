
describe('logger', () => {
  let logger;
  let originalLogLevel;
  let consoleSpy;

  beforeEach(() => {
    originalLogLevel = process.env.LOG_LEVEL;
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

  it.each(['qurlLink', 'qurl_link', 'qurlLinkUrl', 'qurl_link_url'])(
    'redacts a live qURL access link stored under %s',
    (key) => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.info('minted', { [key]: 'https://qurl.link/#at_live_bearer' });

      const line = consoleSpy.log.mock.calls[0][0];
      expect(line).toContain(`"${key}":"[REDACTED]"`);
      expect(line).not.toContain('https://qurl.link/#at_live_bearer');
      expect(line).not.toContain('at_live_bearer');
    },
  );

  it.each(['qurlLinks', 'qurl_links'])(
    'redacts a matched %s array of bare access-link strings on every log channel',
    (key) => {
      process.env.LOG_LEVEL = 'debug';
      logger = require('../src/logger');
      const secret = 'https://qurl.link/#at_live_bearer';

      for (const method of ['error', 'warn', 'info', 'debug']) {
        logger[method]('minted', { [key]: [secret] });
      }
      logger.audit('qurl_minted', { [key]: [secret] });

      const allOutput = JSON.stringify([
        consoleSpy.error.mock.calls,
        consoleSpy.warn.mock.calls,
        consoleSpy.log.mock.calls,
      ]);
      expect(allOutput).not.toContain(secret);
      expect(allOutput).not.toContain('at_live_bearer');
      expect(allOutput).toContain('[REDACTED]');
    },
  );

  it('redacts nested qURL link arrays while preserving adjacent numeric dimensions', () => {
    process.env.LOG_LEVEL = 'debug';
    logger = require('../src/logger');

    logger.info('minted', {
      payload: {
        orphaned_qurl_links: ['https://qurl.link/#at_live_bearer'],
        qurl_link_count: 3,
      },
    });

    const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0].slice(consoleSpy.log.mock.calls[0][0].indexOf('{')));
    expect(parsed.payload.orphaned_qurl_links).toBe('[REDACTED]');
    expect(parsed.payload.qurl_link_count).toBe(3);
  });

  it('preserves empty qURL link containers because they cannot carry a credential', () => {
    process.env.LOG_LEVEL = 'info';
    logger = require('../src/logger');

    logger.info('minted', { qurl_links: [], qurl_link_map: {} });

    const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0].slice(consoleSpy.log.mock.calls[0][0].indexOf('{')));
    expect(parsed.qurl_links).toEqual([]);
    expect(parsed.qurl_link_map).toEqual({});
  });

  it('omits meta string when no meta keys', () => {
    process.env.LOG_LEVEL = 'info';
    logger = require('../src/logger');

    logger.info('test');

    const output = consoleSpy.log.mock.calls[0][0];
    expect(output).toContain('INFO: test');
    expect(output).not.toContain('{}');
  });

  it('includes ISO timestamp in output', () => {
    process.env.LOG_LEVEL = 'info';
    logger = require('../src/logger');

    logger.info('ts-test');

    const output = consoleSpy.log.mock.calls[0][0];
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

      logger.audit('upload_success', { tokens_minted: 7, send_id: 'send-1' });

      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.tokens_minted).toBe(7);
      expect(parsed.audit.send_id).toBe('send-1');
    });

    it('emits a fallback audit_serialization_failed event when JSON.stringify throws', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      const circ = {};
      circ.self = circ;
      expect(() => logger.audit('upload_success', { circ })).not.toThrow();

      const auditLines = consoleSpy.log.mock.calls.map(c => {
        try { return JSON.parse(c[0]); } catch { return null; }
      }).filter(Boolean);
      const fallback = auditLines.find(l => l.audit && l.audit.event === 'audit_serialization_failed');
      expect(fallback).toBeDefined();
      expect(fallback.audit.agent).toBe('discord');
      expect(fallback.audit.original_event).toBe('upload_success');
      expect(typeof fallback.audit.reason).toBe('string');
    });

    it('redacts secret-shaped key VALUES while keeping siblings intact + warns', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.audit('upload_success', { send_id: 's1', auth_token: 'sk-abc123' });

      expect(consoleSpy.error).toHaveBeenCalled();
      expect(consoleSpy.error.mock.calls[0][0]).toContain('secret-shaped key');  // singular OR plural, matches "secret-shaped key" prefix
      expect(consoleSpy.error.mock.calls[0][0]).toContain('auth_token');
      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.auth_token).toBe('[REDACTED]');
      expect(parsed.audit.send_id).toBe('s1');
    });

    it.each(['qurlLink', 'qurl_link', 'qurlLinkUrl', 'qurl_link_url', 'qurlLinks', 'qurl_links'])(
      'redacts a live qURL access link stored under audit key %s',
      (key) => {
        process.env.LOG_LEVEL = 'info';
        logger = require('../src/logger');

        logger.audit('qurl_minted', { [key]: 'https://qurl.link/#at_live_bearer' });

        const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
        expect(parsed.audit[key]).toBe('[REDACTED]');
        expect(consoleSpy.error.mock.calls[0][0]).toContain(key);
        const allOutput = JSON.stringify([
          consoleSpy.log.mock.calls,
          consoleSpy.error.mock.calls,
        ]);
        expect(allOutput).not.toContain('https://qurl.link/#at_live_bearer');
        expect(allOutput).not.toContain('at_live_bearer');
      },
    );

    it('redacts decorated qURL link containers without flagging numeric dimensions', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.audit('qurl_minted', {
        orphaned_qurl_links: ['https://qurl.link/#at_live_bearer'],
        qurl_link_count: 3,
      });

      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.orphaned_qurl_links).toBe('[REDACTED]');
      expect(parsed.audit.qurl_link_count).toBe(3);
      const warnings = consoleSpy.error.mock.calls.map(c => c[0]).join('\n');
      expect(warnings).toContain('orphaned_qurl_links');
      expect(warnings).not.toContain('qurl_link_count');
    });

    it('preserves empty qURL link containers without a secret-shaped warning', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.audit('qurl_minted', { orphaned_qurl_links: [], qurl_link_map: {} });

      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.orphaned_qurl_links).toEqual([]);
      expect(parsed.audit.qurl_link_map).toEqual({});
      expect(consoleSpy.error).not.toHaveBeenCalled();
    });

    it('redacts interaction_token (the PR-B view-counter bearer cred) in the audit path', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.audit('upload_success', { send_id: 's1', interaction_token: 'tok-LIVE-bearer' });

      expect(consoleSpy.error.mock.calls[0][0]).toContain('interaction_token');
      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.interaction_token).toBe('[REDACTED]');
      expect(JSON.stringify(parsed)).not.toContain('tok-LIVE-bearer');
      expect(parsed.audit.send_id).toBe('s1');
    });

    it('redacts non-empty string values; non-string secret values pass through', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.audit('upload_success', { auth_token: null, api_key: 0 });

      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.auth_token).toBeNull();
      expect(parsed.audit.api_key).toBe(0);
    });

    it('redacts secret-shaped keys nested inside a meta object', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.audit('upload_success', { send_id: 's1', context: { auth_token: 'sk-nested' } });

      expect(consoleSpy.error).toHaveBeenCalled();
      expect(consoleSpy.error.mock.calls[0][0]).toContain('auth_token');
      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.context.auth_token).toBe('[REDACTED]');
      expect(parsed.audit.send_id).toBe('s1');
    });

    it('reports ALL offending secret-shaped keys in a single warn line', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.audit('upload_success', { send_id: 's1', auth_token: 'a', password: 'b' });

      expect(consoleSpy.error).toHaveBeenCalledTimes(1);
      const msg = consoleSpy.error.mock.calls[0][0];
      expect(msg).toContain('auth_token');
      expect(msg).toContain('password');
      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.auth_token).toBe('[REDACTED]');
      expect(parsed.audit.password).toBe('[REDACTED]');
    });

    it('mixed-case secret-shaped keys are still detected via .toLowerCase() lookup', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');
      logger.audit('upload_success', { Auth_Token: 'sk-xyz', PASSWORD: 'p' });
      expect(consoleSpy.error).toHaveBeenCalled();
      const msg = consoleSpy.error.mock.calls[0][0];
      expect(msg.toLowerCase()).toContain('auth_token');
      expect(msg.toLowerCase()).toContain('password');
    });

    it('redacts secret-shaped keys nested inside an array element', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.audit('upload_success', {
        send_id: 's1',
        history: [{ ts: 1, password: 'p1' }, { ts: 2, password: 'p2' }],
      });

      expect(consoleSpy.error).toHaveBeenCalled();
      expect(consoleSpy.error.mock.calls[0][0]).toContain('password');
      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.history[0].password).toBe('[REDACTED]');
      expect(parsed.audit.history[0].ts).toBe(1);
      expect(parsed.audit.history[1].password).toBe('[REDACTED]');
    });

    it('does NOT warn for legitimate dimension keys that contain "token" as a substring', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.audit('upload_success', { send_id: 's1', tokens_minted: 7, token_count: 3 });

      expect(consoleSpy.error).not.toHaveBeenCalled();
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
      const lines = consoleSpy.log.mock.calls.map(c => JSON.parse(c[0]));
      expect(lines).toHaveLength(2);
      for (const line of lines) {
        expect(line.audit.event).toBe('dispatch_sent');
        expect(line.audit.agent).toBe('discord');
      }
    });

    it('coerces array meta to {} so it does not spread into numeric-index keys', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      expect(() => logger.audit('dispatch_sent', [1, 2, 3])).not.toThrow();
      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.event).toBe('dispatch_sent');
      expect(parsed.audit.agent).toBe('discord');
      expect(parsed.audit['0']).toBeUndefined();
      expect(parsed.audit['1']).toBeUndefined();
    });
  });

  describe('hash-family redaction, drift guards, and recursion semantics', () => {
    const FULL_MD5 = '5d41402abc4b2a76b9719d911017c592';

    it('redacts a top-level hash key on info()', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.info('uploaded', { hash: FULL_MD5, resource_id: 'r1' });

      expect(consoleSpy.log).toHaveBeenCalledTimes(1);
      const line = consoleSpy.log.mock.calls[0][0];
      expect(line).toContain('"hash":"[REDACTED]"');
      expect(line).not.toContain(FULL_MD5);
      expect(line).toContain('"resource_id":"r1"');
    });

    it('does NOT redact md5_prefix (the intentional truncated form)', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.info('uploaded', { md5_prefix: '5d41402a', resource_id: 'r1' });

      const line = consoleSpy.log.mock.calls[0][0];
      expect(line).toContain('"md5_prefix":"5d41402a"');
      expect(line).not.toContain('REDACTED');
    });

    it('does NOT redact hash-substring keys like commitHash / webhookHash', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.info('deploy', { commitHash: 'abc1234', webhookHash: 'def5678' });

      const line = consoleSpy.log.mock.calls[0][0];
      expect(line).toContain('"commitHash":"abc1234"');
      expect(line).toContain('"webhookHash":"def5678"');
      expect(line).not.toContain('REDACTED');
    });

    it('pins the canonical content-hash exact-match key set', () => {
      const { __testExports } = require('../src/logger');

      const got = [...__testExports.REDACT_EXACT_KEYS]
        .filter(k => k !== 'private_key')
        .sort();
      const want = [
        'hash',
        'md5', 'sha1', 'sha256', 'sha512',
        'digest', 'checksum',
        'content_hash', 'body_hash',
      ].sort();
      expect(got).toEqual(want);
    });

    it.each([
      'md5', 'sha1', 'sha256', 'sha512',
      'digest', 'checksum',
      'content_hash', 'body_hash',
    ])('redacts content-hash key "%s" (exact-match)', (keyName) => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.info('uploaded', { [keyName]: FULL_MD5 });

      const line = consoleSpy.log.mock.calls[0][0];
      expect(line).toContain(`"${keyName}":"[REDACTED]"`);
      expect(line).not.toContain(FULL_MD5);
    });

    it('info() preserves tokens_minted / token_count with primitive (number) values', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.info('audit-event', { tokens_minted: 7, token_count: 3 });

      const line = consoleSpy.log.mock.calls[0][0];
      expect(line).toContain('"tokens_minted":7');
      expect(line).toContain('"token_count":3');
      expect(line).not.toContain('REDACTED');
    });

    it.each([
      'md5_prefix', 'sha256_prefix', 'commit_hash', 'fileHash',
    ])('does NOT redact adjacent name "%s"', (keyName) => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.info('deploy', { [keyName]: 'value-survives' });

      const line = consoleSpy.log.mock.calls[0][0];
      expect(line).toContain(`"${keyName}":"value-survives"`);
      expect(line).not.toContain('REDACTED');
    });

    it('redacts nested hash key inside a meta object', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.info('uploaded', { context: { hash: FULL_MD5 } });

      const line = consoleSpy.log.mock.calls[0][0];
      expect(line).toContain('"hash":"[REDACTED]"');
      expect(line).not.toContain(FULL_MD5);
    });

    it('redacts hash on audit() AND emits a warn line naming the key', () => {
      logger = require('../src/logger');

      logger.audit('upload_success', { send_id: 's1', hash: FULL_MD5 });

      expect(consoleSpy.error).toHaveBeenCalledTimes(1);
      expect(consoleSpy.error.mock.calls[0][0]).toContain('secret-shaped key');
      expect(consoleSpy.error.mock.calls[0][0]).toContain('hash');
      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.hash).toBe('[REDACTED]');
      expect(parsed.audit.send_id).toBe('s1');
    });

    it('mixed-case hash keys are still caught via .toLowerCase() lookup', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.info('uploaded', { Hash: FULL_MD5, HASH: FULL_MD5, HaSh: FULL_MD5 });

      const line = consoleSpy.log.mock.calls[0][0];
      expect(line).toContain('"Hash":"[REDACTED]"');
      expect(line).toContain('"HASH":"[REDACTED]"');
      expect(line).toContain('"HaSh":"[REDACTED]"');
      expect(line).not.toContain(FULL_MD5);
    });

    it('redacts hash key inside an array element', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.info('uploaded', { items: [{ hash: FULL_MD5 }, { hash: FULL_MD5 }] });

      const line = consoleSpy.log.mock.calls[0][0];
      expect(line).not.toContain(FULL_MD5);
      expect((line.match(/"hash":"\[REDACTED\]"/g) || []).length).toBe(2);
    });

    it.each([
      ['null', null, '"hash":null'],
      ['number', 12345, '"hash":12345'],
      ['empty-string', '', '"hash":""'],
    ])('primitive hash value (%s) passes through unchanged', (_label, hashValue, expectedSubstring) => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.info('uploaded', { hash: hashValue });

      const line = consoleSpy.log.mock.calls[0][0];
      expect(line).toContain(expectedSubstring);
      expect(line).not.toContain('REDACTED');
    });

    it('recurses into matched-key objects so inner sensitive keys are redacted', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.info('uploaded', { hash: { token: 'real-secret', nested_hash: 'also-secret' } });

      const line = consoleSpy.log.mock.calls[0][0];
      expect(line).toContain('"token":"[REDACTED]"');
      expect(line).toContain('"nested_hash":"also-secret"');
      expect(line).not.toContain('real-secret');
    });

    it('every REDACT_EXACT_KEYS entry is also redacted by audit()', () => {
      process.env.LOG_LEVEL = 'info';
      const { __testExports } = require('../src/logger');
      logger = require('../src/logger');

      for (const k of __testExports.REDACT_EXACT_KEYS) {
        consoleSpy.log.mockClear();
        consoleSpy.error.mockClear();
        logger.audit('upload_success', { send_id: 's1', [k]: FULL_MD5 });
        const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
        expect(parsed.audit[k]).toBe('[REDACTED]');
        expect(consoleSpy.error.mock.calls[0][0]).toContain(k);
      }
    });

    it('no sensitive value appears as a substring in the log line', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.info('uploaded', {
        hash: FULL_MD5,
        nested: { token: 'sensitive-1' },
        body: { hash: 'sensitive-2' },
      });
      logger.audit('upload_success', {
        send_id: 's1',
        hash: FULL_MD5,
        nested: { auth_token: 'sensitive-3' },
      });

      const allLines = [
        ...consoleSpy.log.mock.calls.map(c => c[0]),
        ...consoleSpy.error.mock.calls.map(c => c[0]),
      ];
      for (const line of allLines) {
        expect(line).not.toContain(FULL_MD5);
        expect(line).not.toContain('sensitive-1');
        expect(line).not.toContain('sensitive-2');
        expect(line).not.toContain('sensitive-3');
      }
    });

    it('every AUDIT_SECRET_KEYS entry is also redacted by info()', () => {
      process.env.LOG_LEVEL = 'info';
      const { __testExports } = require('../src/logger');
      logger = require('../src/logger');

      for (const k of __testExports.AUDIT_SECRET_KEYS) {
        consoleSpy.log.mockClear();
        logger.info('uploaded', { [k]: 'real-secret-value' });
        const line = consoleSpy.log.mock.calls[0][0];
        expect(line).toContain(`"${k}":"[REDACTED]"`);
      }
    });

    it('recursion semantics: substring (redact) vs exact-match (audit)', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.audit('upload_success', { send_id: 's1', hash: { myToken: 'audit-survives' } });
      const auditParsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(auditParsed.audit.hash.myToken).toBe('audit-survives');

      consoleSpy.log.mockClear();

      logger.info('uploaded', { hash: { myToken: 'redact-blanks' } });
      const infoLine = consoleSpy.log.mock.calls[0][0];
      expect(infoLine).toContain('"myToken":"[REDACTED]"');
      expect(infoLine).not.toContain('redact-blanks');
    });

    it('audit() recurses into matched-key objects', () => {
      process.env.LOG_LEVEL = 'info';
      logger = require('../src/logger');

      logger.audit('upload_success', { send_id: 's1', hash: { auth_token: 'real-secret' } });

      const parsed = JSON.parse(consoleSpy.log.mock.calls[0][0]);
      expect(parsed.audit.hash.auth_token).toBe('[REDACTED]');
      expect(consoleSpy.error.mock.calls[0][0]).toContain('hash');
      expect(consoleSpy.error.mock.calls[0][0]).toContain('auth_token');
    });
  });
});
