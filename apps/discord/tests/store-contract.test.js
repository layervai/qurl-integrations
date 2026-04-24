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
      // `.toThrow(string)` does a substring match, safer than
      // `new RegExp(...)` if a future contract entry ever contains
      // regex metacharacters ($, ., parens, etc.).
      expect(() => assertStoreShape(incomplete, 'broken-backend')).toThrow('broken-backend');
      expect(() => assertStoreShape(incomplete, 'broken-backend')).toThrow(STORE_METHODS[0]);
    });

    it('throws when a constant is missing', () => {
      const incomplete = { ...completeBackend };
      delete incomplete[STORE_CONSTANTS[0]];
      expect(() => assertStoreShape(incomplete, 'broken-backend')).toThrow('broken-backend');
      expect(() => assertStoreShape(incomplete, 'broken-backend')).toThrow(STORE_CONSTANTS[0]);
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
      // Substring contains — safer than regex if a future entry
      // ever has regex metacharacters.
      expect(caught.message).toContain(STORE_METHODS[0]);
      expect(caught.message).toContain(STORE_CONSTANTS[0]);
    });

    it('METHODS and CONSTANTS lists are disjoint — no name can be both', () => {
      // A name listed in both would produce a confusing shape
      // assertion (would appear as missing/present inconsistently
      // depending on which check runs first). Cheap guard.
      const overlap = STORE_METHODS.filter(m => STORE_CONSTANTS.includes(m));
      expect(overlap).toEqual([]);
    });
  });
});

describe('store/index (default backend)', () => {
  // Load the default-backend singleton. The top-of-file
  // `jest.mock(...)` calls for config + logger are hoisted and
  // already in effect, so `:memory:` SQLite is used (no real file
  // touch). `jest.resetModules()` ensures a fresh require of the
  // singleton if a prior suite in the same worker already cached
  // the store.
  let store;
  beforeAll(() => {
    jest.resetModules();
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

// Child-process tests for boot-time assertion paths. These are the
// fail-loud invariants the Store contract relies on — we can't
// exercise them from inside the same jest process because
// `process.env.JEST_WORKER_ID` is set (and `typeof jest !== 'undefined'`),
// so the assertion is intentionally skipped in-process.
// `child_process.spawnSync('node', [...])` gives us a clean
// non-jest boot that hits the real assertion path.
describe('store/index boot-time assertions (via child_process)', () => {
  const { spawnSync } = require('child_process');
  const path = require('path');
  const appRoot = path.resolve(__dirname, '..');

  it('rejects an unknown STORE_TYPE with a listing of valid backends', () => {
    // JSON.stringify-escape the path so a workspace dir containing
    // a quote, backslash, or `${}` can't break out of the inline
    // `node -e` script literal. Local Linux CI paths are boring
    // today, but this future-proofs against contributors running
    // against weirder paths (spaces + quotes on macOS, WSL, etc.).
    const requirePath = JSON.stringify(path.join(appRoot, 'src/store'));
    const result = spawnSync(
      process.execPath,
      ['-e', `require(${requirePath})`],
      {
        env: {
          ...process.env,
          STORE_TYPE: 'sqlitte', // typo — must NOT silently fall back
          // Intentionally strip the JEST marker so the child runs
          // the real assertion path.
          JEST_WORKER_ID: '',
          NODE_ENV: 'production',
          // SQLite wouldn't get reached, but set :memory: defensively.
          DATABASE_PATH: ':memory:',
        },
        encoding: 'utf8',
      },
    );
    expect(result.status).not.toBe(0);
    expect(result.stderr).toMatch(/Unknown STORE_TYPE/);
    expect(result.stderr).toMatch(/sqlitte/);
    // Names the valid options so the operator knows how to fix.
    expect(result.stderr).toMatch(/sqlite/);
  });

  // Note: exercising the "backend missing a method" throw end-to-end
  // requires swapping out a backend module at require-time, which
  // can't be done from a bare `node -e`. The assertStoreShape
  // assertion itself is unit-tested above (negative cases); the
  // boot-time integration here focuses on the env-var validation
  // branch, which is the most likely real-world misconfiguration.
});
