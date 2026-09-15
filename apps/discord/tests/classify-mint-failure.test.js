
const { _test } = require('../src/commands');
const { classifyMintFailure } = _test;

describe('classifyMintFailure (qurl-integrations#276 reason taxonomy)', () => {
  test('null / undefined → unknown', () => {
    expect(classifyMintFailure(null)).toBe('unknown');
    expect(classifyMintFailure(undefined)).toBe('unknown');
  });

  describe('timeout class', () => {
    test('libuv ETIMEDOUT (axios / http.request socket-level)', () => {
      expect(classifyMintFailure({ code: 'ETIMEDOUT' })).toBe('timeout');
    });

    test('libuv ECONNABORTED — kept defensively (not produced by current native fetch stack)', () => {
      expect(classifyMintFailure({ code: 'ECONNABORTED' })).toBe('timeout');
    });

    test('undici / node fetch TimeoutError DOMException', () => {
      expect(classifyMintFailure({ name: 'TimeoutError' })).toBe('timeout');
    });

    test('AbortError with timeout cause → timeout (deadline-fired abort)', () => {
      expect(classifyMintFailure({ name: 'AbortError', cause: 'timeout' })).toBe('timeout');
      expect(classifyMintFailure({ name: 'AbortError', cause: new Error('request timeout') })).toBe('timeout');
    });
  });

  describe('AbortError without timeout cause → unknown', () => {
    test('bare AbortError → unknown (ambiguous between deadline and user-cancel)', () => {
      expect(classifyMintFailure({ name: 'AbortError' })).toBe('unknown');
      expect(classifyMintFailure({ name: 'AbortError', cause: 'user-cancelled' })).toBe('unknown');
    });

    test('AbortError with timeout-shaped message + no cause → unknown', () => {
      expect(classifyMintFailure({ name: 'AbortError', message: 'aborted due to timeout' })).toBe('unknown');
    });

    test('AbortError + error.status → unknown (abort precedence over status)', () => {
      expect(classifyMintFailure({ name: 'AbortError', status: 503 })).toBe('unknown');
    });

    test('message-string fallback removed — bare message → unknown', () => {
      expect(classifyMintFailure({ message: 'request timeout exceeded' })).toBe('unknown');
      expect(classifyMintFailure({ message: 'Timeout while waiting for response' })).toBe('unknown');
    });
  });

  describe('upstream_5xx class', () => {
    test('500 / 502 / 503 / 504', () => {
      for (const status of [500, 502, 503, 504, 599]) {
        expect(classifyMintFailure({ status })).toBe('upstream_5xx');
      }
    });
  });

  describe('upstream_4xx class', () => {
    test('400 / 401 / 403 / 404 / 429', () => {
      for (const status of [400, 401, 403, 404, 429, 499]) {
        expect(classifyMintFailure({ status })).toBe('upstream_4xx');
      }
    });
  });

  describe('unknown class', () => {
    test('non-HTTP error with no code/name/message-keyword', () => {
      expect(classifyMintFailure({ message: 'something else broke' })).toBe('unknown');
    });

    test('message-only error with no name/code → unknown', () => {
      expect(classifyMintFailure({ message: 'not a timeout related error' })).toBe('unknown');
    });

    test('2xx status (should never happen in this code path, but pinned)', () => {
      expect(classifyMintFailure({ status: 200 })).toBe('unknown');
    });
  });

  describe('priority ordering: timeout beats status when both present', () => {
    test('ETIMEDOUT + status 504 → timeout (not upstream_5xx)', () => {
      expect(classifyMintFailure({ code: 'ETIMEDOUT', status: 504 })).toBe('timeout');
    });
  });
});
