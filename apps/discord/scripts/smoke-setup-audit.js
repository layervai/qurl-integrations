// Run against DynamoDB Local (DDB_TEST_ENDPOINT required), or use --aws for
// a disposable table in the selected AWS account. No existing rows are touched.
const assert = require('node:assert/strict');
const { randomBytes, randomUUID } = require('node:crypto');
const {
  DynamoDBClient, CreateTableCommand, DeleteTableCommand, waitUntilTableExists,
} = require('@aws-sdk/client-dynamodb');
const { DynamoDBDocumentClient, GetCommand } = require('@aws-sdk/lib-dynamodb');

assert(process.argv.includes('--aws') || process.env.DDB_TEST_ENDPOINT,
  'Set DDB_TEST_ENDPOINT for local testing, or explicitly pass --aws');
assert(process.env.DDB_TEST_ENDPOINT || process.env.AWS_REGION, 'Set AWS_REGION for --aws');
process.env.AWS_REGION ||= 'us-east-2';
process.env.DDB_TABLE_PREFIX = `setup-audit-smoke-${randomUUID()}-`;
process.env.KEY_ENCRYPTION_KEY = randomBytes(32).toString('base64');
const store = require('../src/store/ddb-store');
const table = `${process.env.DDB_TABLE_PREFIX}guild-configs`;
const client = new DynamoDBClient({
  region: process.env.AWS_REGION,
  ...(process.env.DDB_TEST_ENDPOINT ? { endpoint: process.env.DDB_TEST_ENDPOINT } : {}),
});
const ddb = DynamoDBDocumentClient.from(client);
const lines = [];
const log = console.log;

async function main() {
  let created = false;
  try {
    await client.send(new CreateTableCommand({
      TableName: table, BillingMode: 'PAY_PER_REQUEST',
      KeySchema: [{ AttributeName: 'guild_id', KeyType: 'HASH' }],
      AttributeDefinitions: [{ AttributeName: 'guild_id', AttributeType: 'S' }],
    }));
    created = true;
    await waitUntilTableExists({ client, maxWaitTime: 60 }, { TableName: table });
    console.log = (line) => lines.push(line);
    const read = async () => (await ddb.send(new GetCommand({
      TableName: table, Key: { guild_id: 'smoke-guild' }, ConsistentRead: true,
    }))).Item;
    await store.setGuildApiKey('smoke-guild', 'smoke-key-one', 'admin-one', 'oauth');
    const first = await read();
    await store.setGuildApiKey('smoke-guild', 'smoke-key-two', 'admin-one', 'paste');
    const prior = await read();
    assert.equal(lines.length, 0, 'First setup and same-admin re-key must be silent');
    assert.equal(prior.configured_at, first.configured_at);
    await store.setGuildApiKey('smoke-guild', 'smoke-key-three', 'admin-two', 'oauth');
    assert.equal(lines.length, 1, 'Admin change must emit exactly one audit');
    assert.deepEqual(JSON.parse(lines[0]).audit, {
      event: 'qurl_setup_admin_changed', agent: 'discord', guild_id: 'smoke-guild',
      old_admin_id: 'admin-one', new_admin_id: 'admin-two', prior_had_key: true,
      via: 'oauth', prior_configured_at: first.configured_at, prior_updated_at: prior.updated_at,
    });
    assert.equal(await store.getGuildApiKey('smoke-guild'), 'smoke-key-three');
    assert(!lines[0].includes('smoke-key'));
  } finally {
    console.log = log;
    try {
      if (created) await client.send(new DeleteTableCommand({ TableName: table }));
    } finally {
      await store.close();
      client.destroy();
    }
  }
  console.log(lines[0]);
  console.log('Setup audit smoke passed; disposable table deleted.');
}

main().catch((err) => { console.error(err); process.exitCode = 1; });
