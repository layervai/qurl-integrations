// Local-only initial-persistence/revoke interleaving check. No connector or DM calls.
const assert = require('node:assert/strict');
const { randomUUID, randomBytes } = require('node:crypto');
const { DynamoDBClient, CreateTableCommand, DeleteTableCommand } = require('@aws-sdk/client-dynamodb');
const { DynamoDBDocumentClient, TransactWriteCommand, GetCommand } = require('@aws-sdk/lib-dynamodb');
assert(process.env.DDB_TEST_ENDPOINT, 'Set DDB_TEST_ENDPOINT to a loopback DynamoDB Local URL');
const endpoint = new URL(process.env.DDB_TEST_ENDPOINT);
assert(['localhost', '127.0.0.1', '[::1]'].includes(endpoint.hostname));
assert.equal(endpoint.protocol, 'http:');
process.env.DDB_TABLE_PREFIX = `initial-fence-${randomUUID()}-`;
process.env.AWS_REGION = 'us-east-2';
process.env.AWS_ACCESS_KEY_ID = 'synthetic';
process.env.AWS_SECRET_ACCESS_KEY = 'synthetic';
delete process.env.AWS_SESSION_TOKEN;
delete process.env.AWS_PROFILE;
delete process.env.AWS_DEFAULT_PROFILE;
process.env.KEY_ENCRYPTION_KEY = randomBytes(32).toString('base64');
const store = require('../src/store/ddb-store');
const { tables } = require('./provision-ddb-local');
const client = new DynamoDBClient({ region: process.env.AWS_REGION, endpoint: endpoint.href });
const ddb = DynamoDBDocumentClient.from(client);
const originalSend = DynamoDBDocumentClient.prototype.send;
let reachedFirst;
let resume;
const first = new Promise(resolve => { reachedFirst = resolve; });
const continued = new Promise(resolve => { resume = resolve; });
const created = [];
let initial;
const sender = 'synthetic-sender';
const rows = (sendId, count) => Array.from({ length: count }, (_, i) => ({
  sendId, senderDiscordId: sender, recipientDiscordId: `recipient-${i}`,
  resourceId: `synthetic-resource-${i}`, resourceType: 'file',
  qurlLink: 'https://example.test/#synthetic', expiresIn: '24h', targetType: 'user',
}));
const readConfig = async sendId => (await ddb.send(new GetCommand({
  TableName: process.env.DDB_TABLE_PREFIX + 'qurl-send-configs', Key: { send_id: sendId }, ConsistentRead: true,
}))).Item;

async function main() {
  try {
    for (const spec of tables.filter(t => ['qurl-sends', 'qurl-send-configs'].includes(t.name))) {
      const name = process.env.DDB_TABLE_PREFIX + spec.name;
      await client.send(new CreateTableCommand({
        TableName: name, BillingMode: 'PAY_PER_REQUEST', KeySchema: spec.keySchema,
        AttributeDefinitions: spec.attributes, ...(spec.gsis ? { GlobalSecondaryIndexes: spec.gsis } : {}),
      }));
      created.push(name);
    }
    const sendId = randomUUID();
    await store.saveSendConfig({ sendId, senderDiscordId: sender, initialSend: true });
    assert((await readConfig(sendId)).initial_persistence_pending);
    await assert.rejects(store.saveSendConfig({ sendId, senderDiscordId: 'other-owner', initialSend: true }), { name: 'ConditionalCheckFailedException' });
    await assert.rejects(store.saveSendConfig({ sendId, senderDiscordId: sender }), { name: 'ConditionalCheckFailedException' });
    await assert.rejects(store.recordQURLSendBatch(rows(sendId, 1)), { code: 'SEND_CONFIG_REVOKED' });
    let writes = 0;
    DynamoDBDocumentClient.prototype.send = async function(command, ...args) {
      const result = await originalSend.call(this, command, ...args);
      if (command instanceof TransactWriteCommand && ++writes === 1) {
        reachedFirst();
        await continued;
      }
      return result;
    };
    initial = store.recordQURLSendBatch(rows(sendId, 199), { initialSend: true })
      .then(() => ({ complete: true }), error => ({ error }));
    await Promise.race([first, initial.then(result => { throw result.error || new Error('Writer finished before the guarded-chunk checkpoint'); })]);
    assert.equal(await store.markSendRevoking(sendId, sender), true);
    const snapshot = await store.getSendItems(sendId, sender, { consistentRead: true });
    assert.equal(snapshot.length, 99);
    // Synthetic storage finalization only; no real resource deletion is asserted.
    assert.equal(await store.markSendRevoked(sendId, sender), true);
    resume();
    const outcome = await initial;
    assert.equal(outcome.error?.code, 'SEND_CONFIG_REVOKED');
    assert.equal((await store.getSendItems(sendId, sender, { consistentRead: true })).length, snapshot.length);
    const state = await readConfig(sendId);
    assert(state.revoked_at);
    assert(state.initial_persistence_pending, 'Failed later chunk must not enable Add');
    assert.equal(writes, 1, 'Only the first chunk committed');
    DynamoDBDocumentClient.prototype.send = originalSend;

    const closedId = randomUUID();
    await store.saveSendConfig({ sendId: closedId, senderDiscordId: sender, initialSend: true });
    assert.equal(await store.markSendRevoking(closedId, sender), true);
    await assert.rejects(store.recordQURLSendBatch(rows(closedId, 55), { initialSend: true }), { code: 'SEND_CONFIG_REVOKED' });
    assert.equal((await store.getSendItems(closedId, sender, { consistentRead: true })).length, 0);
    assert((await readConfig(closedId)).initial_persistence_pending);

    const completeId = randomUUID();
    await store.saveSendConfig({ sendId: completeId, senderDiscordId: sender, initialSend: true });
    await store.recordQURLSendBatch(rows(completeId, 199), { initialSend: true });
    assert.equal((await store.getSendItems(completeId, sender, { consistentRead: true })).length, 199);
    assert.equal((await readConfig(completeId)).initial_persistence_pending, undefined);
    await store.recordQURLSendBatch([{ ...rows(completeId, 1)[0], recipientDiscordId: 'added-recipient' }]);
    assert.equal((await store.getSendItems(completeId, sender, { consistentRead: true })).length, 200);
    console.log('Revoke snapshot 99 stays 99; later initial chunk rejected. A complete 199-row send clears preparation and permits guarded Add.');
  } finally {
    resume();
    if (initial) await initial;
    DynamoDBDocumentClient.prototype.send = originalSend;
    try {
      for (const name of created.reverse()) await client.send(new DeleteTableCommand({ TableName: name }));
    } finally { await store.close(); client.destroy(); }
  }
}
main().catch(err => { console.error(err); process.exitCode = 1; });
