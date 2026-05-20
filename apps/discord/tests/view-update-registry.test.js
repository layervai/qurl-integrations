/**
 * Unit tests for src/view-update-registry.js — the process-local
 * Map<qurl_id, Set<callback>> used by view-update push (feat #60).
 *
 * Covers:
 *   - register / unregister round-trip
 *   - dispatch hits all registered callbacks
 *   - dispatch on unknown qurl_id returns false (silent-drop —
 *     LOAD-BEARING for the (N-1)/N replica miss case)
 *   - multiple callbacks per qurl_id (defensive: same qurl in two
 *     monitors via /qurl map → /qurl send chain)
 *   - callback throw is caught + logged, other callbacks still fire
 *   - input validation (non-string qurl_id, non-function callback)
 *   - unregister on a qurl_id with no entries is a no-op
 *   - unregister of one callback when others exist keeps the rest
 *   - dispatch snapshot: callback unregistering itself mid-dispatch
 *     doesn't break iteration
 */

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

const registry = require('../src/view-update-registry');
const logger = require('../src/logger');

describe('view-update-registry', () => {
  beforeEach(() => {
    registry._test._resetForTest();
    jest.clearAllMocks();
  });

  describe('register / unregister', () => {
    test('register adds an entry; unregister removes it', () => {
      const cb = jest.fn();
      registry.register('qrl_a', cb);
      expect(registry._test._sizeForTest()).toBe(1);
      expect(registry._test._entryCountForTest('qrl_a')).toBe(1);
      registry.unregister('qrl_a', cb);
      expect(registry._test._sizeForTest()).toBe(0);
    });

    test('multiple callbacks on same qurl_id', () => {
      const cb1 = jest.fn();
      const cb2 = jest.fn();
      registry.register('qrl_a', cb1);
      registry.register('qrl_a', cb2);
      expect(registry._test._sizeForTest()).toBe(1); // one qurl_id key
      expect(registry._test._entryCountForTest('qrl_a')).toBe(2);
      registry.unregister('qrl_a', cb1);
      expect(registry._test._entryCountForTest('qrl_a')).toBe(1);
      registry.unregister('qrl_a', cb2);
      expect(registry._test._sizeForTest()).toBe(0);
    });

    test('unregister on unknown qurl_id is a no-op', () => {
      expect(() => registry.unregister('qrl_nope', () => {})).not.toThrow();
    });

    test('unregister of an unregistered callback is a no-op', () => {
      const cb1 = jest.fn();
      const cb2 = jest.fn();
      registry.register('qrl_a', cb1);
      registry.unregister('qrl_a', cb2); // cb2 was never registered
      expect(registry._test._entryCountForTest('qrl_a')).toBe(1);
    });

    test('register rejects non-string qurl_id', () => {
      expect(() => registry.register(null, () => {})).toThrow(/non-empty string/);
      expect(() => registry.register('', () => {})).toThrow(/non-empty string/);
      expect(() => registry.register(123, () => {})).toThrow(/non-empty string/);
    });

    test('register rejects non-function callback', () => {
      expect(() => registry.register('qrl_a', null)).toThrow(/must be a function/);
      expect(() => registry.register('qrl_a', 'not-a-fn')).toThrow(/must be a function/);
    });
  });

  describe('dispatch', () => {
    test('returns true and fires callback when qurl_id is registered', () => {
      const cb = jest.fn();
      registry.register('qrl_a', cb);
      const update = { accessCount: 1, consumed: false };
      const hit = registry.dispatch('qrl_a', update);
      expect(hit).toBe(true);
      expect(cb).toHaveBeenCalledWith(update);
      expect(cb).toHaveBeenCalledTimes(1);
    });

    test('returns false (silent-drop) when qurl_id is not registered', () => {
      const hit = registry.dispatch('qrl_nope', { accessCount: 1 });
      expect(hit).toBe(false);
      // Critical: silent-drop is the load-bearing property. No log
      // spam on miss — verified by checking logger.warn was NOT called
      // (only error from a thrown callback would log).
      expect(logger.warn).not.toHaveBeenCalled();
    });

    test('fires every callback registered for the qurl_id', () => {
      const cb1 = jest.fn();
      const cb2 = jest.fn();
      const cb3 = jest.fn();
      registry.register('qrl_a', cb1);
      registry.register('qrl_a', cb2);
      registry.register('qrl_a', cb3);
      registry.dispatch('qrl_a', { accessCount: 5 });
      expect(cb1).toHaveBeenCalledTimes(1);
      expect(cb2).toHaveBeenCalledTimes(1);
      expect(cb3).toHaveBeenCalledTimes(1);
    });

    test('callback that throws is caught + logged; siblings still fire', () => {
      const cb1 = jest.fn(() => { throw new Error('boom'); });
      const cb2 = jest.fn();
      registry.register('qrl_a', cb1);
      registry.register('qrl_a', cb2);
      const hit = registry.dispatch('qrl_a', { accessCount: 1 });
      expect(hit).toBe(true);
      expect(cb1).toHaveBeenCalled();
      expect(cb2).toHaveBeenCalled(); // sibling still fires
      expect(logger.error).toHaveBeenCalledWith(
        'view-update-registry: callback threw',
        expect.objectContaining({ qurl_id: 'qrl_a', error: 'boom' }),
      );
    });

    test('callback unregistering itself mid-dispatch does not break iteration', () => {
      // cb1 must close over its own jest.fn wrapper (the value the
      // registry holds), NOT the inner fn — jest.fn(fn) wraps `fn`,
      // and the wrapper is what gets added to the Set.
      let cb1;
      cb1 = jest.fn(() => {
        registry.unregister('qrl_a', cb1);
      });
      const cb2 = jest.fn();
      registry.register('qrl_a', cb1);
      registry.register('qrl_a', cb2);
      registry.dispatch('qrl_a', { accessCount: 1 });
      expect(cb1).toHaveBeenCalled();
      expect(cb2).toHaveBeenCalled(); // snapshot ensures cb2 still fires
      // After dispatch, cb1 is unregistered but cb2 remains.
      expect(registry._test._entryCountForTest('qrl_a')).toBe(1);
    });
  });

  describe('GC discipline', () => {
    test('unregister down to zero callbacks removes the qurl_id entry', () => {
      const cb = jest.fn();
      registry.register('qrl_a', cb);
      registry.unregister('qrl_a', cb);
      // Internal map should not retain an empty Set for the key.
      expect(registry._test._sizeForTest()).toBe(0);
      expect(registry._test._entryCountForTest('qrl_a')).toBe(0);
    });
  });
});
