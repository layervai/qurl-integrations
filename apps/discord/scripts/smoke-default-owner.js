// Live default-owner linking check. Only GETs may reach qURL; all DynamoDB
// writes use a disposable table. Supply the sandbox default key and secret.
const assert = require('node:assert/strict');
const { randomBytes, randomUUID } = require('node:crypto');
const {
  DynamoDBClient, CreateTableCommand, DeleteTableCommand, waitUntilTableExists,
} = require('@aws-sdk/client-dynamodb');
const { DynamoDBDocumentClient, GetCommand } = require('@aws-sdk/lib-dynamodb');

assert(process.argv.includes('--aws'), 'Pass --aws to create a disposable sandbox table');
for (const name of ['AWS_REGION', 'QURL_API_KEY', 'QURL_WEBHOOK_SECRET', 'QURL_ENDPOINT', 'BASE_URL']) {
  assert(process.env[name], `${name} is required`);
}
process.env.DDB_TABLE_PREFIX = `default-owner-smoke-${randomUUID()}-`;
process.env.KEY_ENCRYPTION_KEY = randomBytes(32).toString('base64');
const store = require('../src/store');
const subscriptions = require('../src/webhook-subscriptions');
const { linkGuildWebhookSubscription } = require('../src/guild-webhook-link');
const table = `${process.env.DDB_TABLE_PREFIX}guild-configs`;
const client = new DynamoDBClient({ region: process.env.AWS_REGION });
const ddb = DynamoDBDocumentClient.from(client);
const originalFetch = global.fetch;
let reads = 0;
global.fetch = (url, options) => {
  assert.equal(options.method, 'GET', 'Linking the default owner must never mutate a qURL subscription');
  reads++;
  return originalFetch(url, options);
};

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
    const guildId = 'smoke-guild';
    await store.setGuildApiKey(guildId, process.env.QURL_API_KEY, 'smoke-admin', 'oauth');
    // Exercise both first-time mapping and repeated linking against real DDB.
    for (let attempt = 0; attempt < 2; attempt++) {
      assert.deepEqual(await linkGuildWebhookSubscription({ guildId, apiKey: process.env.QURL_API_KEY }),
        { ok: true, action: 'reused' });
      const row = (await ddb.send(new GetCommand({
        TableName: table, Key: { guild_id: guildId }, ConsistentRead: true,
      }))).Item;
      assert(row.webhook_owner_id);
      assert(!Object.hasOwn(row, 'webhook_id'));
      assert(!Object.hasOwn(row, 'webhook_secret'));
      await subscriptions.scanOnce();
      assert.equal(subscriptions.getSecretForOwner(row.webhook_owner_id), process.env.QURL_WEBHOOK_SECRET);
    }
    assert(reads >= 2);
  } finally {
    global.fetch = originalFetch;
    try {
      if (created) await client.send(new DeleteTableCommand({ TableName: table }));
    } finally {
      await store.close();
      client.destroy();
    }
  }
  console.log('Default owner smoke passed: repeat links reused the secret; disposable table deleted.');
}
main().catch(err => { console.error(err); process.exitCode = 1; });
