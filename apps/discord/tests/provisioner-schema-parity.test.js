const fs = require('node:fs');
const path = require('node:path');
const parser = require('@babel/parser');
const traverseModule = require('@babel/traverse');
const traverse = traverseModule.default || traverseModule;
const { CreateTableCommand, DescribeTableCommand } = require('@aws-sdk/client-dynamodb');

const { tables: provisionerTables, ensureTable } = require('../scripts/provision-ddb-local');

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
  it('every literal store index query names an index on its own provisioned table', () => {
    const ast = parser.parse(fs.readFileSync(path.join(__dirname, '../src/store/ddb-store.js'), 'utf8'));
    let queries = 0;
    traverse(ast, {
      ObjectProperty(indexPath) {
        const node = indexPath.node;
        if ((node.key.name || node.key.value) !== 'IndexName') return;
        queries++;
        expect(node.value.type).toBe('StringLiteral');
        const table = indexPath.parent.properties.find(property => property.key?.name === 'TableName')?.value;
        expect(table).toMatchObject({ type: 'MemberExpression', computed: false, object: { name: 'TABLES' } });
        const suffix = storeTABLES[table.property.name]?.slice(TABLE_PREFIX.length);
        const spec = provisionerTables.find(spec => spec.name === suffix);
        expect(spec).toBeDefined();
        expect(spec.gsis).toEqual(expect.arrayContaining([expect.objectContaining({ IndexName: node.value.value })]));
      },
    });
    expect(queries).toBeGreaterThan(0);
  });

});


describe('existing local tables', () => {
  const sends = provisionerTables.find(table => table.name === 'qurl-sends');
  let log;
  beforeEach(() => { log = jest.spyOn(console, 'log').mockImplementation(() => {}); });
  afterEach(() => log.mockRestore());

  function existingClient(indexes) {
    return { send: jest.fn(async command => {
      if (command instanceof CreateTableCommand) throw Object.assign(new Error('exists'), { name: 'ResourceInUseException' });
      expect(command).toBeInstanceOf(DescribeTableCommand);
      expect(command.input.TableName).toBe('fixture-qurl-sends');
      return { Table: { GlobalSecondaryIndexes: indexes.map(IndexName => ({ IndexName })) } };
    }) };
  }

  it('fails with a local-only repair instruction before claiming a missing index is usable', async () => {
    const client = existingClient(['sender_discord_id-created_at-index']);
    await expect(ensureTable(client, 'fixture-', sends)).rejects.toThrow('lacks GSI qurl_id-index; recreate only this disposable local table. No data was deleted.');
    expect(client.send).toHaveBeenCalledTimes(2);
    expect(log).not.toHaveBeenCalled();
  });

  it('keeps an already matching table without writes or migration', async () => {
    const client = existingClient(sends.gsis.map(index => index.IndexName));
    await ensureTable(client, 'fixture-', sends);
    expect(client.send).toHaveBeenCalledTimes(2);
    expect(log).toHaveBeenCalledWith('exists   fixture-qurl-sends');
  });

  it('propagates an existing-table inspection failure', async () => {
    const client = existingClient([]);
    client.send.mockRejectedValueOnce(Object.assign(new Error('exists'), { name: 'ResourceInUseException' }))
      .mockRejectedValueOnce(new Error('inspection failed'));
    await expect(ensureTable(client, 'fixture-', sends)).rejects.toThrow('inspection failed');
    expect(log).not.toHaveBeenCalled();
  });
});
