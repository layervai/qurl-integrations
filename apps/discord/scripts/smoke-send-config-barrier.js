// Local-only check: a late initial send cannot erase a revocation barrier.
const assert = require('node:assert/strict');
const { randomBytes, randomUUID } = require('node:crypto');
const { DynamoDBClient, CreateTableCommand, DeleteTableCommand } = require('@aws-sdk/client-dynamodb');
const { DynamoDBDocumentClient, PutCommand, GetCommand } = require('@aws-sdk/lib-dynamodb');

assert(process.env.DDB_TEST_ENDPOINT, 'Set DDB_TEST_ENDPOINT to a loopback DynamoDB Local URL');
const endpoint = new URL(process.env.DDB_TEST_ENDPOINT);
assert(['localhost', '127.0.0.1', '[::1]'].includes(endpoint.hostname), 'Use a loopback DynamoDB Local endpoint');
assert.equal(endpoint.protocol, 'http:', 'Use local HTTP');
process.env.AWS_REGION = 'us-east-2';
process.env.AWS_ACCESS_KEY_ID = 'synthetic';
process.env.AWS_SECRET_ACCESS_KEY = 'synthetic';
delete process.env.AWS_SESSION_TOKEN;
delete process.env.AWS_PROFILE;
delete process.env.AWS_DEFAULT_PROFILE;
process.env.DDB_TABLE_PREFIX = `barrier-smoke-${randomUUID()}-`;
process.env.KEY_ENCRYPTION_KEY = randomBytes(32).toString('base64');
const store = require('../src/store/ddb-store');
const table = `${process.env.DDB_TABLE_PREFIX}qurl-send-configs`;
const client = new DynamoDBClient({ region: process.env.AWS_REGION, endpoint: endpoint.href });
const ddb = DynamoDBDocumentClient.from(client);

async function main() {
  let created = false;
  try {
    await client.send(new CreateTableCommand({
      TableName: table, BillingMode: 'PAY_PER_REQUEST',
      KeySchema: [{ AttributeName: 'send_id', KeyType: 'HASH' }],
      AttributeDefinitions: [{ AttributeName: 'send_id', AttributeType: 'S' }],
    }));
    created = true;
    const sendId = randomUUID();
    const params = { sendId, senderDiscordId: 'synthetic-sender', resourceType: 'file', expiresIn: '24h' };
    await store.saveSendConfig(params);
    for (const marker of ['revoking_at', 'revoked_at', 'revoke_acknowledged_at']) {
      await ddb.send(new PutCommand({
        TableName: table, Item: { send_id: sendId, sender_discord_id: params.senderDiscordId, [marker]: 'retained' },
      }));
      await assert.rejects(store.saveSendConfig(params), { name: 'ConditionalCheckFailedException' });
      const row = (await ddb.send(new GetCommand({ TableName: table, Key: { send_id: sendId }, ConsistentRead: true }))).Item;
      assert.equal(row[marker], 'retained');
    }
    console.log('All three revocation barriers survived real conditional writes.');
  } finally {
    try {
      if (created) await client.send(new DeleteTableCommand({ TableName: table }));
    } finally {
      await store.close();
      client.destroy();
    }
  }
}

main().catch(err => { console.error(err); process.exitCode = 1; });
