/**
 * Schema-parity test: `scripts/provision-ddb-local.js` is a parallel
 * schema source from `qurl-integrations-infra/modules/qurl-bot-ddb/` and
 * from `src/store/ddb-store.js`'s `TABLES` map. Drift surfaces today
 * only as a runtime `ResourceNotFoundException` on first local
 * `npm start` — by then the operator is already debugging.
 *
 * This test pins the suffix set the provisioner creates against the
 * suffix set the bot expects (after stripping `DDB_TABLE_PREFIX`),
 * so a `ddb-store.js` TABLES addition without a matching provisioner
 * schema (or vice versa) fails at PR CI time.
 *
 * Scope: NAMES only. Key schemas + GSIs aren't compared — `ddb-store.js`
 * doesn't expose them in a structured way, and the runtime call sites
 * already exercise them via the existing `tests/ddb-store.test.js`
 * coverage. A future enhancement could add structural parity (see
 * issue #438), but the suffix-set check catches the most common drift
 * (a new table added to one side without the other).
 *
 * Documented exception: `weekly_stats` is in `ddb-store.js`'s TABLES
 * map but has no DDB call site today, so the provisioner intentionally
 * doesn't create it (header comment in the script flags this). The
 * test allow-lists `weekly_stats` from the comparison.
 */

const { tables: provisionerTables } = require('../scripts/provision-ddb-local');

describe('scripts/provision-ddb-local.js ↔ ddb-store.js schema parity', () => {
  // Match the same TABLES-map prefix shape `ddb-store.js` constructs.
  // The test reads from setup-env.js's `DDB_TABLE_PREFIX = 'jest-test-'`.
  const TABLE_PREFIX = (process.env.DDB_TABLE_PREFIX ?? '').trim();

  // Loaded lazily so a missing-env regression in this test file
  // surfaces here, not in the require graph above.
  let storeTABLES;
  beforeAll(() => {
    // Pull in `ddb-store.js`'s TABLES export by re-requiring through
    // the store module surface. Each value is a fully-prefixed table
    // name (e.g. `jest-test-github-links`).
    storeTABLES = require('../src/store/ddb-store')._TABLES_FOR_TESTING;
  });

  it('exposes the TABLES map for cross-suite parity checks', () => {
    expect(storeTABLES).toBeDefined();
    expect(typeof storeTABLES).toBe('object');
  });

  it('every provisioner table is referenced by `ddb-store.js`', () => {
    // Strip the prefix so we compare bare suffixes (the part after
    // `${TABLE_PREFIX}`, e.g. `github-links`).
    const provisionerSuffixes = new Set(provisionerTables.map(t => t.name));
    const storeSuffixes = new Set(
      Object.values(storeTABLES).map(fullName => fullName.slice(TABLE_PREFIX.length))
    );

    const unreferenced = [...provisionerSuffixes].filter(s => !storeSuffixes.has(s));
    expect(unreferenced).toEqual([]);
  });

  it('every `ddb-store.js` TABLES entry is provisioned (modulo `weekly_stats`)', () => {
    // `weekly_stats` is intentionally absent from the provisioner —
    // there's no DDB call site for it today (see comment in
    // `scripts/provision-ddb-local.js`'s `tables` array). If a future
    // PR adds a reader/writer, drop this allowlist entry and add the
    // schema to the provisioner in the same change.
    const INTENTIONAL_GAPS = new Set(['weekly-stats']);

    const provisionerSuffixes = new Set(provisionerTables.map(t => t.name));
    const storeSuffixes = Object.values(storeTABLES).map(fullName =>
      fullName.slice(TABLE_PREFIX.length)
    );

    const missing = storeSuffixes
      .filter(s => !provisionerSuffixes.has(s))
      .filter(s => !INTENTIONAL_GAPS.has(s));
    expect(missing).toEqual([]);
  });
});
