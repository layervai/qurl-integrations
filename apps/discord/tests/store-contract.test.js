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
 *     it in ddb-store breaks this suite at PR time.
 */

jest.mock('../src/config', () => ({
  PENDING_LINK_EXPIRY_MINUTES: 30,
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

const { STORE_METHODS, STORE_CONSTANTS, assertStoreShape } = require('../src/store/contract');

describe('store/contract', () => {
  describe('STORE_METHODS', () => {
    it('is frozen (mutation throws under strict mode)', () => {
      expect(Object.isFrozen(STORE_METHODS)).toBe(true);
      // ES modules run in strict mode, so mutation should throw
      // rather than silently no-op. Asserting the throw is sharper
      // than just asserting frozen — catches a hypothetical future
      // regression that e.g. returns a non-frozen proxy.
      expect(() => STORE_METHODS.push('newMethod')).toThrow();
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
    it('is frozen (mutation throws under strict mode)', () => {
      expect(Object.isFrozen(STORE_CONSTANTS)).toBe(true);
      expect(() => STORE_CONSTANTS.push('NEW_CONSTANT')).toThrow();
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
    // loading ddb-store, which constructs the real DDB client at
    // module-load) keeps this suite free of I/O side effects.
    const completeBackend = {};
    for (const m of STORE_METHODS) completeBackend[m] = () => undefined;
    for (const c of STORE_CONSTANTS) completeBackend[c] = {};

    it('passes on a complete backend', () => {
      expect(() => assertStoreShape(completeBackend, 'test-backend')).not.toThrow();
    });

    it('throws when a method is missing', () => {
      const incomplete = { ...completeBackend };
      delete incomplete['createPendingLink'];
      // `.toThrow(string)` does a substring match, safer than
      // `new RegExp(...)` if a future contract entry ever contains
      // regex metacharacters ($, ., parens, etc.).
      expect(() => assertStoreShape(incomplete, 'broken-backend')).toThrow('broken-backend');
      expect(() => assertStoreShape(incomplete, 'broken-backend')).toThrow('createPendingLink');
    });

    it('throws when a constant is missing', () => {
      const incomplete = { ...completeBackend };
      delete incomplete['BADGE_TYPES'];
      expect(() => assertStoreShape(incomplete, 'broken-backend')).toThrow('broken-backend');
      expect(() => assertStoreShape(incomplete, 'broken-backend')).toThrow('BADGE_TYPES');
    });

    it('throws when the backend is not an object', () => {
      expect(() => assertStoreShape(null, 'null-backend')).toThrow(/null-backend.*not an object/);
      expect(() => assertStoreShape(undefined, 'undef-backend')).toThrow(/undef-backend.*not an object/);
      expect(() => assertStoreShape('not-an-object', 'string-backend')).toThrow(/string-backend.*not an object/);
    });

    it('names both missing methods AND missing constants in a single throw (one error, two diagnoses)', () => {
      const incomplete = { ...completeBackend };
      delete incomplete['createPendingLink'];
      delete incomplete['BADGE_TYPES'];
      let caught;
      try {
        assertStoreShape(incomplete, 'double-gap');
      } catch (err) {
        caught = err;
      }
      expect(caught).toBeDefined();
      // Substring contains — safer than regex if a future entry
      // ever has regex metacharacters.
      expect(caught.message).toContain('createPendingLink');
      expect(caught.message).toContain('BADGE_TYPES');
    });

    it('METHODS and CONSTANTS lists are disjoint — no name can be both', () => {
      // A name listed in both would produce a confusing shape
      // assertion (would appear as missing/present inconsistently
      // depending on which check runs first). Cheap guard.
      const overlap = STORE_METHODS.filter(m => STORE_CONSTANTS.includes(m));
      expect(overlap).toEqual([]);
    });

    it('no apps/discord/src caller references the pre-rename `removeGuildApiKey` name', () => {
      // The contract method was renamed `removeGuildApiKey` →
      // `_removeGuildApiKeyRaw` so the leading underscore signals
      // "raw operation that does not also clean up sibling rows
      // (webhook subscription)." Defensive guard: a future caller
      // that accidentally re-types the old short name would silently
      // skip the cleanup. This grep-style assertion makes the
      // regression LOUD at test-time.
      const fs = require('fs');
      const path = require('path');
      const SRC = path.resolve(__dirname, '../src');
      const offenders = [];
      function walk(dir) {
        for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
          const full = path.join(dir, entry.name);
          if (entry.isDirectory()) { walk(full); continue; }
          if (!entry.name.endsWith('.js')) continue;
          const body = fs.readFileSync(full, 'utf8');
          // Word-boundary match catches `db.removeGuildApiKey(` and
          // `removeGuildApiKey,` without flagging the renamed
          // `_removeGuildApiKeyRaw`.
          if (/\bremoveGuildApiKey\b(?!Raw)/.test(body)) offenders.push(path.relative(SRC, full));
        }
      }
      walk(SRC);
      expect(offenders).toEqual([]);
    });
  });
});

describe('store/index (default backend)', () => {
  // Load the default-backend singleton. The top-of-file
  // `jest.mock(...)` calls for config + logger are hoisted and
  // already in effect; ddb-store's module-load env guards
  // (DDB_TABLE_PREFIX + AWS_REGION) are satisfied by
  // `tests/setup-env.js`, so requiring `../src/store` here
  // constructs a real DDB client object but issues no network calls
  // until a method is invoked (which this suite intentionally
  // doesn't do). `jest.resetModules()` ensures a fresh require of
  // the singleton if a prior suite in the same worker already
  // cached the store.
  let store;
  beforeAll(() => {
    jest.resetModules();
    store = require('../src/store');
  });

  afterAll(() => {
    // `close` is in STORE_METHODS and validated above, so this
    // conditional is belt-and-suspenders — kept because beforeAll's
    // `jest.resetModules()` + require() can fail to populate `store`
    // under a future regression that breaks store/index.js's
    // require-time validation; the close-call would then crash with
    // "Cannot read property 'close' of undefined" and obscure the
    // real failure.
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

  // Helper: spawn a child `node -e` that requires the store module
  // with a specific STORE_TYPE env, forcing the real boot path
  // (JEST_WORKER_ID='' strips the skip-guard). Returns
  // { status, stdout, stderr }. `JSON.stringify`-escaping the
  // require path so a workspace dir with a quote / backslash /
  // `${}` can't break out of the inline `node -e` script literal.
  //
  // ddb-store's module-load guards refuse to boot without
  // DDB_TABLE_PREFIX + AWS_REGION. The spawn inherits the full
  // parent env via `{...process.env}`, so the values would arrive
  // via setup-env.js anyway — pinning them explicitly here insulates
  // the test from a future setup-env.js sentinel change (these are
  // the two values whose default is changing in the PR that
  // introduced this rewrite). KEY_ENCRYPTION_KEY / NODE_ENV / any
  // other inherited values stay implicit.
  //
  // Note for future contributors: when a new table is added to
  // `ddb-store.js`'s TABLES map, the local-dev provisioner at
  // `scripts/provision-ddb-local.js` must learn the schema too — the
  // contract test below loads the default backend without invoking
  // its methods, so a missing table only surfaces as a
  // `ResourceNotFoundException` once a test or `npm start` actually
  // queries it.
  function spawnStoreBoot(storeTypeValue) {
    const requirePath = JSON.stringify(path.join(appRoot, 'src/store'));
    const env = {
      ...process.env,
      JEST_WORKER_ID: '',
      DDB_TABLE_PREFIX: 'jest-spawn-',
      AWS_REGION: 'us-east-1',
    };
    // Distinguish "not set at all" (`undefined`) from "set to
    // empty/whitespace" (both should fall back to the default).
    if (storeTypeValue === undefined) {
      delete env.STORE_TYPE;
    } else {
      env.STORE_TYPE = storeTypeValue;
    }
    return spawnSync(process.execPath, ['-e', `require(${requirePath})`], { env, encoding: 'utf8' });
  }

  it('rejects an unknown STORE_TYPE with a listing of valid backends', () => {
    const result = spawnStoreBoot('ddbb'); // typo — must NOT silently fall back
    expect(result.status).not.toBe(0);
    expect(result.stderr).toMatch(/Unknown STORE_TYPE/);
    expect(result.stderr).toMatch(/ddbb/);
    // Names the valid options so the operator knows how to fix.
    expect(result.stderr).toMatch(/ddb/);
  });

  it('rejects a stale STORE_TYPE=sqlite from a pre-DDB-only env file', () => {
    // Defense-in-depth: an operator carrying over an .env file from
    // before SQLite was stripped would fail loud rather than fall
    // back silently. The error message lists `ddb` as the only valid
    // backend, pointing at the fix.
    const result = spawnStoreBoot('sqlite');
    expect(result.status).not.toBe(0);
    expect(result.stderr).toMatch(/Unknown STORE_TYPE/);
    expect(result.stderr).toMatch(/sqlite/);
    expect(result.stderr).toMatch(/ddb/);
  });

  it('falls back to ddb when STORE_TYPE is unset', () => {
    const result = spawnStoreBoot(undefined);
    expect(result.status).toBe(0);
    expect(result.stderr).toBe('');
  });

  it('falls back to ddb when STORE_TYPE is empty-string (typical container-templating bug)', () => {
    const result = spawnStoreBoot('');
    expect(result.status).toBe(0);
    expect(result.stderr).toBe('');
  });

  it('falls back to ddb when STORE_TYPE is whitespace-only (another container-templating bug)', () => {
    const result = spawnStoreBoot('   ');
    expect(result.status).toBe(0);
    expect(result.stderr).toBe('');
  });

  // Note: exercising the "backend missing a method" throw end-to-end
  // requires swapping out a backend module at require-time, which
  // can't be done from a bare `node -e`. The assertStoreShape
  // assertion itself is unit-tested above (negative cases); the
  // boot-time integration here focuses on the env-var validation
  // branch, which is the most likely real-world misconfiguration.
});
