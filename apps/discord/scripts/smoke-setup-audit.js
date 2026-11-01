// Run against DynamoDB Local (DDB_TEST_ENDPOINT required), or use --aws for
// a disposable table in the selected AWS account. No existing rows are touched.
const assert = require('node:assert/strict');
const { randomBytes, randomUUID } = require('node:crypto');
const {
  DynamoDBClient, CreateTableCommand, DeleteTableCommand, waitUntilTableExists,
} = require('@aws-sdk/client-dynamodb');
const { DynamoDBDocumentClient, GetCommand, ScanCommand } = require('@aws-sdk/lib-dynamodb');

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
  const created = [];
  const lifecycleTable = `${process.env.DDB_TABLE_PREFIX}credential-lifecycle`;
  try {
    await client.send(new CreateTableCommand({
      TableName: table, BillingMode: 'PAY_PER_REQUEST',
      KeySchema: [{ AttributeName: 'guild_id', KeyType: 'HASH' }],
      AttributeDefinitions: [{ AttributeName: 'guild_id', AttributeType: 'S' }],
    }));
    created.push(table);
    await client.send(new CreateTableCommand({
      TableName: lifecycleTable, BillingMode: 'PAY_PER_REQUEST',
      KeySchema: [{ AttributeName: 'pk', KeyType: 'HASH' }, { AttributeName: 'sk', KeyType: 'RANGE' }],
      AttributeDefinitions: [{ AttributeName: 'pk', AttributeType: 'S' }, { AttributeName: 'sk', AttributeType: 'S' }],
    }));
    created.push(lifecycleTable);
    await waitUntilTableExists({ client, maxWaitTime: 60 }, { TableName: lifecycleTable });
    await waitUntilTableExists({ client, maxWaitTime: 60 }, { TableName: table });
    console.log = (line) => lines.push(line);
    const read = async () => (await ddb.send(new GetCommand({
      TableName: table, Key: { guild_id: 'smoke-guild' }, ConsistentRead: true,
    }))).Item;
    await store.setGuildApiKey('smoke-guild', 'smoke-key-one', 'admin-one', 'oauth');
    const first = await read();
    await store.setGuildApiKey('smoke-guild', 'smoke-key-two', 'admin-one', 'oauth', { ownerId: 'synthetic-owner', expectedApiKey: 'smoke-key-one', keyId: 'key-two', keyKind: 'dedicated' });
    const prior = await read();
    assert.equal(lines.length, 0, 'First setup and same-admin re-key must be silent');
    assert.equal(prior.configured_at, first.configured_at);
    await store.setGuildApiKey('smoke-guild', 'smoke-key-three', 'admin-two', 'oauth', { ownerId: 'synthetic-owner', expectedApiKey: 'smoke-key-two', keyId: 'key-three', keyKind: 'dedicated' });
    assert.equal(lines.length, 1, 'Admin change must emit exactly one audit');
    assert.deepEqual(JSON.parse(lines[0]).audit, {
      event: 'qurl_setup_admin_changed', agent: 'discord', guild_id: 'smoke-guild',
      old_admin_id: 'admin-one', new_admin_id: 'admin-two', prior_had_key: true,
      via: 'oauth', prior_configured_at: first.configured_at, prior_updated_at: prior.updated_at,
    });
    assert.equal(await store.getGuildApiKey('smoke-guild'), 'smoke-key-three');
    assert.equal((await read()).qurl_owner_id, 'synthetic-owner');
    const { prepareGuildOwnerBinding } = require('../src/guild-owner');
    const originalFetch = globalThis.fetch;
    globalThis.fetch = async () => { throw new Error('A bound owner must not revalidate its saved key'); };
    try {
      assert.deepEqual(await prepareGuildOwnerBinding('smoke-guild', 'synthetic-owner'), {
        ownerId: 'synthetic-owner', expectedApiKey: 'smoke-key-three',
      });
      await assert.rejects(prepareGuildOwnerBinding('smoke-guild', 'another-owner'),
        { code: 'GUILD_OWNER_CONFLICT' });
    } finally {
      globalThis.fetch = originalFetch;
    }
    await assert.rejects(store.setGuildApiKey('smoke-guild', 'wrong-owner-key', 'admin-two', 'oauth', {
      ownerId: 'another-owner', expectedApiKey: 'smoke-key-three',
    }), { code: 'GUILD_SETUP_CHANGED' });
    const results = await Promise.allSettled(['race-one', 'race-two'].map(key =>
      store.setGuildApiKey('smoke-guild', key, 'admin-two', 'oauth', {
        ownerId: 'synthetic-owner', expectedApiKey: 'smoke-key-three',
      })));
    assert.equal(results.filter(result => result.status === 'fulfilled').length, 1);
    const jobs = (await ddb.send(new ScanCommand({ TableName: lifecycleTable, ConsistentRead: true }))).Items;
    assert.equal(jobs.length, 3, 'Only successful replacements must retain old credentials');
    const decrypt = require('../src/utils/crypto').decrypt;
    assert.deepEqual(jobs.map(job => decrypt(job.qurl_api_key)).sort(),
      ['smoke-key-one', 'smoke-key-two', 'smoke-key-three'].sort());
    assert(jobs.every(job => job.cleanup_state === 'pending' && !('expires_at' in job)));
    assert.equal(jobs.filter(job => job.qurl_key_kind === 'shared').length, 1);
    assert.deepEqual(jobs.filter(job => job.qurl_key_kind === 'dedicated').map(job => job.qurl_key_id).sort(), ['key-three', 'key-two']);
    await store.enqueueGuildWebhookCleanup({
      guildId: 'smoke-guild', apiKey: 'still-active-shared-key', webhookId: 'synthetic-webhook',
      webhookOwnerId: 'synthetic-owner',
    });
    const unlinked = (await ddb.send(new ScanCommand({ TableName: lifecycleTable, ConsistentRead: true })))
      .Items.filter(job => job.reason === 'unlinked_webhook');
    assert.equal(unlinked.length, 1);
    assert.equal(decrypt(unlinked[0].qurl_api_key), 'still-active-shared-key');
    assert.equal(unlinked[0].revoke_key, false);
    assert.equal(unlinked[0].webhook_id, 'synthetic-webhook');
    const winner = await read();
    await client.send(new DeleteTableCommand({ TableName: lifecycleTable }));
    created.splice(created.indexOf(lifecycleTable), 1);
    await assert.rejects(store.setGuildApiKey('smoke-guild', 'must-not-land', 'admin-two', 'oauth', {
      ownerId: 'synthetic-owner', expectedApiKey: decrypt(winner.qurl_api_key),
    }));
    assert.equal((await read()).qurl_api_key, winner.qurl_api_key, 'Failed job persistence must preserve the saved credential');
    await assert.rejects(store.setGuildApiKey('smoke-guild', 'unverified-key', 'admin-two', 'paste'),
      { code: 'GUILD_SETUP_CHANGED' });
    assert(!lines[0].includes('smoke-key'));
  } finally {
    console.log = log;
    try {
      for (const TableName of created.reverse()) await client.send(new DeleteTableCommand({ TableName }));
    } finally {
      await store.close();
      client.destroy();
    }
  }
  console.log(lines[0]);
  console.log('Setup audit smoke passed; disposable table deleted.');
}

main().catch((err) => { console.error(err); process.exitCode = 1; });
