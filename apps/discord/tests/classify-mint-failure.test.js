/**
 * Unit pin for classifyMintFailure (qurl-integrations#276).
 *
 * The helper maps thrown errors from /qurl send's upload + mint phase
 * into a small enum of reason classes — `timeout`, `upstream_4xx`,
 * `upstream_5xx`, `unknown` — that drive the CloudWatch metric filter
 * dimension. Without a test pinning the contract, the docstring's
 * "don't add per-API-code branches" cardinality discipline silently
 * drifts the first time someone adds a "helpful" special case.
 */

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
      // ECONNABORTED is axios-shaped; the connector helper uses native
      // fetch + throwConnectorError so this code path is currently
      // unreachable. Kept for defensive coverage against a future HTTP-
      // client swap (axios / got / etc.) that surfaces ECONNABORTED on
      // socket-level timeouts.
      expect(classifyMintFailure({ code: 'ECONNABORTED' })).toBe('timeout');
    });

    test('undici / node fetch TimeoutError DOMException', () => {
      // Node's built-in fetch surfaces timeout as { name: 'TimeoutError' }
      // with no code field. Without the .name check, this would have
      // bucketed as `unknown` and tonight's incident would be in a
      // different category from the next undici-shaped one.
      expect(classifyMintFailure({ name: 'TimeoutError' })).toBe('timeout');
    });

    test('AbortError with timeout cause → timeout (deadline-fired abort)', () => {
      // Real undici deadline-fired aborts populate error.cause with the
      // reason string. Pin both the cause-string and cause-with-message
      // shapes (cause can be a string OR an Error object).
      expect(classifyMintFailure({ name: 'AbortError', cause: 'timeout' })).toBe('timeout');
      expect(classifyMintFailure({ name: 'AbortError', cause: new Error('request timeout') })).toBe('timeout');
    });
  });

  describe('AbortError without timeout cause → unknown (PR #300 review)', () => {
    test('bare AbortError → unknown (ambiguous between deadline and user-cancel)', () => {
      // Justin: if AbortController gets adopted upstream for user-
      // cancellation (e.g. a future "cancel send" button), bare
      // AbortError without a corroborating timeout signal should NOT
      // mis-bucket as timeout. Bare AbortError now buckets as unknown;
      // a deliberate cause-tagged AbortError still buckets as timeout
      // (see the cause-corroborated test above).
      expect(classifyMintFailure({ name: 'AbortError' })).toBe('unknown');
      expect(classifyMintFailure({ name: 'AbortError', cause: 'user-cancelled' })).toBe('unknown');
    });

    test('AbortError with timeout-shaped message + no cause → unknown', () => {
      // AbortError still requires cause-corroboration to bucket as
      // timeout, regardless of the message string. Pinned for explicit
      // coverage — the message-regex branch was removed in round 11
      // so this is also implicitly covered by "no message-regex".
      expect(classifyMintFailure({ name: 'AbortError', message: 'aborted due to timeout' })).toBe('unknown');
    });

    test('message-string fallback removed (round 11) — bare message → unknown', () => {
      // The message-regex /timeout/i was removed because it over-matched
      // ("not a timeout related error" used to bucket as timeout). The
      // name/code paths above cover every real shape we've seen.
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

    test('message-only error with no name/code → unknown (round-11 false-positive fix)', () => {
      // Pre-round-11 this bucketed as `timeout` due to the /timeout/i
      // substring match — the fix dropped the message-regex branch
      // entirely. A bare message-only error now correctly resolves to
      // `unknown`, preventing dashboard contamination from a future
      // upstream message like "completed after timeout retry".
      expect(classifyMintFailure({ message: 'not a timeout related error' })).toBe('unknown');
    });

    test('2xx status (should never happen in this code path, but pinned)', () => {
      // The catch block only fires on thrown errors, but if a caller
      // somehow synthesized a 2xx error object, it should NOT bucket
      // as upstream_4xx or upstream_5xx.
      expect(classifyMintFailure({ status: 200 })).toBe('unknown');
    });
  });

  describe('priority ordering: timeout beats status when both present', () => {
    test('ETIMEDOUT + status 504 → timeout (not upstream_5xx)', () => {
      // If a future axios-like lib attaches BOTH a timeout code AND
      // a synthesized 504, the more-specific timeout label should win
      // — operators want to distinguish "we never got a response" from
      // "upstream actively returned 504".
      expect(classifyMintFailure({ code: 'ETIMEDOUT', status: 504 })).toBe('timeout');
    });
  });
});
