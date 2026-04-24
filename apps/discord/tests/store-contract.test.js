/**
 * Tests for the Store backend contract (src/store/contract.js) and the
 * boot-time shape assertion.
 *
 * Coverage goals:
 *   - contract's STORE_METHODS and STORE_CONSTANTS lists are frozen so
 *     a typo / reassign can't silently shrink the contract.
 *   - assertStoreShape throws on missing methods, missing constants,
 *     non-object inputs, and succeeds on a complete backend.
 *   - The default store singleton (store/index.js under the default
 *     STORE_TYPE) exercises every method + constant the contract
 *     lists — so adding an entry to the contract without implementing
 *     it in sqlite-store breaks this suite at PR time.
 */

jest.mock('../src/config', () => ({
  DATABASE_PATH: ':memory:',
  PENDING_LINK_EXPIRY_MINUTES: 30,
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
}));

const { STORE_METHODS, STORE_CONSTANTS, assertStoreShape } = require('../src/store/contract');

describe('store/contract', () => {
  describe('STORE_METHODS', () => {
    it('is frozen', () => {
      expect(Object.isFrozen(STORE_METHODS)).toBe(true);
    });

    it('contains no duplicates (typo in the list would collide silently)', () => {
      const uniq = new Set(STORE_METHODS);
      expect(uniq.size).toBe(STORE_METHODS.length);
    });

    it('contains every entry as a non-empty string', () => {
      for (const m of STORE_METHODS) {
        expect(typeof m).toBe('string');
        expect(m.length).toBeGreaterThan(0);
      }
    });
  });

  describe('STORE_CONSTANTS', () => {
    it('is frozen', () => {
      expect(Object.isFrozen(STORE_CONSTANTS)).toBe(true);
    });

    it('contains no duplicates', () => {
      const uniq = new Set(STORE_CONSTANTS);
      expect(uniq.size).toBe(STORE_CONSTANTS.length);
    });
  });

  describe('assertStoreShape', () => {
    // Build a minimal "complete" backend for the positive test: every
    // contract method becomes a no-op function, every constant becomes
    // a sentinel non-undefined value. Using this here (instead of
    // loading sqlite-store, which runs the real DB init) keeps this
    // suite free of I/O side effects.
    const completeBackend = {};
    for (const m of STORE_METHODS) completeBackend[m] = () => undefined;
    for (const c of STORE_CONSTANTS) completeBackend[c] = {};

    it('passes on a complete backend', () => {
      expect(() => assertStoreShape(completeBackend, 'test-backend')).not.toThrow();
    });

    it('throws when a method is missing', () => {
      const incomplete = { ...completeBackend };
      delete incomplete[STORE_METHODS[0]];
      expect(() => assertStoreShape(incomplete, 'broken-backend'))
        .toThrow(new RegExp(`broken-backend.*${STORE_METHODS[0]}`));
    });

    it('throws when a constant is missing', () => {
      const incomplete = { ...completeBackend };
      delete incomplete[STORE_CONSTANTS[0]];
      expect(() => assertStoreShape(incomplete, 'broken-backend'))
        .toThrow(new RegExp(`broken-backend.*${STORE_CONSTANTS[0]}`));
    });

    it('throws when the backend is not an object', () => {
      expect(() => assertStoreShape(null, 'null-backend')).toThrow(/null-backend.*not an object/);
      expect(() => assertStoreShape(undefined, 'undef-backend')).toThrow(/undef-backend.*not an object/);
      expect(() => assertStoreShape('not-an-object', 'string-backend')).toThrow(/string-backend.*not an object/);
    });

    it('names both missing methods AND missing constants in a single throw (one error, two diagnoses)', () => {
      const incomplete = { ...completeBackend };
      delete incomplete[STORE_METHODS[0]];
      delete incomplete[STORE_CONSTANTS[0]];
      let caught;
      try {
        assertStoreShape(incomplete, 'double-gap');
      } catch (err) {
        caught = err;
      }
      expect(caught).toBeDefined();
      expect(caught.message).toMatch(new RegExp(STORE_METHODS[0]));
      expect(caught.message).toMatch(new RegExp(STORE_CONSTANTS[0]));
    });
  });
});

describe('store/index (default backend)', () => {
  // Load the default-backend singleton AFTER the config + logger
  // mocks above so :memory: SQLite is used (no real file touch).
  let store;
  beforeAll(() => {
    jest.resetModules();
    jest.doMock('../src/config', () => ({
      DATABASE_PATH: ':memory:',
      PENDING_LINK_EXPIRY_MINUTES: 30,
    }));
    jest.doMock('../src/logger', () => ({
      info: jest.fn(),
      warn: jest.fn(),
      error: jest.fn(),
      debug: jest.fn(),
    }));
    store = require('../src/store');
  });

  afterAll(() => {
    if (store && typeof store.close === 'function') store.close();
  });

  it('implements every STORE_METHODS entry', () => {
    const missing = STORE_METHODS.filter(m => typeof store[m] !== 'function');
    expect(missing).toEqual([]);
  });

  it('surfaces every STORE_CONSTANTS entry', () => {
    const missing = STORE_CONSTANTS.filter(c => store[c] === undefined);
    expect(missing).toEqual([]);
  });
});
