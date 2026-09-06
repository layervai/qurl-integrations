
const { tables: provisionerTables } = require('../scripts/provision-ddb-local');

describe('scripts/provision-ddb-local.js ↔ ddb-store.js schema parity', () => {
  const TABLE_PREFIX = (process.env.DDB_TABLE_PREFIX ?? '').trim();

  let storeTABLES;
  beforeAll(() => {
    storeTABLES = require('../src/store/ddb-store')._TABLES_FOR_TESTING;
  });

  it('exposes the TABLES map for cross-suite parity checks', () => {
    expect(storeTABLES).toBeDefined();
    expect(typeof storeTABLES).toBe('object');
  });

  it('every provisioner table is referenced by `ddb-store.js`', () => {
    const provisionerSuffixes = new Set(provisionerTables.map(t => t.name));
    const storeSuffixes = new Set(
      Object.values(storeTABLES).map(fullName => fullName.slice(TABLE_PREFIX.length))
    );

    const unreferenced = [...provisionerSuffixes].filter(s => !storeSuffixes.has(s));
    expect(unreferenced).toEqual([]);
  });

  it('every `ddb-store.js` TABLES entry is provisioned (modulo `weekly_stats`)', () => {
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
