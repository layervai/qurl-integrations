// Local-only check: a late initial send cannot erase a revocation barrier.
const assert = require('node:assert/strict');
const { randomBytes, randomUUID } = require('node:crypto');
const { DynamoDBClient, CreateTableCommand, DeleteTableCommand } = require('@aws-sdk/client-dynamodb');
const { DynamoDBDocumentClient, PutCommand, GetCommand, UpdateCommand } = require('@aws-sdk/lib-dynamodb');

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
const { tables } = require('./provision-ddb-local');
const table = `${process.env.DDB_TABLE_PREFIX}qurl-send-configs`;
const client = new DynamoDBClient({ region: process.env.AWS_REGION, endpoint: endpoint.href });
const ddb = DynamoDBDocumentClient.from(client);

async function main() {
  const created = [];
  try {
    for (const spec of tables.filter(t => ['qurl-sends', 'qurl-send-configs'].includes(t.name))) {
      const name = `${process.env.DDB_TABLE_PREFIX}${spec.name}`;
      await client.send(new CreateTableCommand({
        TableName: name, BillingMode: 'PAY_PER_REQUEST',
        KeySchema: spec.keySchema, AttributeDefinitions: spec.attributes,
        ...(spec.gsis ? { GlobalSecondaryIndexes: spec.gsis } : {}),
      }));
      created.push(name);
    }
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
    const recoveryId = randomUUID();
    const sender = 'synthetic-recovery-sender';
    const recoveryParams = { ...params, sendId: recoveryId, senderDiscordId: sender };
    await store.saveSendConfig(recoveryParams);
    assert.equal(await store.acknowledgeSendRevocation(recoveryId, sender), false, 'A live send cannot be acknowledged');
    assert.equal(await store.acknowledgeSendRevocation(randomUUID(), sender), false, 'A missing send cannot be acknowledged');
    assert.equal(await store.markSendRevoking(recoveryId, sender), true);
    assert.equal(await store.getSendRevocationState(recoveryId, 'other-sender'), undefined);
    assert.equal(await store.acknowledgeSendRevocation(recoveryId, 'other-sender'), false);
    assert.equal(await store.acknowledgeSendRevocation(recoveryId, sender), true);
    assert.equal(await store.acknowledgeSendRevocation(recoveryId, sender), false, 'Duplicate acknowledgement is not another transition');
    const acknowledged = await store.getSendRevocationState(recoveryId, sender);
    assert(acknowledged.revoking_at);
    assert(acknowledged.revoke_acknowledged_at);
    assert.equal(acknowledged.revoke_acknowledged_by, sender);
    assert.equal(acknowledged.revoked_at, undefined);
    // Even a malformed row missing the old marker must keep Add blocked.
    await ddb.send(new UpdateCommand({ TableName: table, Key: { send_id: recoveryId }, UpdateExpression: 'REMOVE revoking_at' }));
    await assert.rejects(store.recordQURLSendBatch([{
      sendId: recoveryId, senderDiscordId: sender, recipientDiscordId: 'synthetic-recipient',
      resourceId: 'synthetic-resource', resourceType: 'file', qurlLink: 'https://example.test/#synthetic',
      expiresIn: '24h', channelId: 'synthetic-channel', targetType: 'user',
    }], { requireSendConfigUnrevoked: true }), { code: 'SEND_CONFIG_REVOKED' });
    const recipient = await ddb.send(new GetCommand({
      TableName: `${process.env.DDB_TABLE_PREFIX}qurl-sends`,
      Key: { send_id: recoveryId, recipient_discord_id: 'synthetic-recipient' }, ConsistentRead: true,
    }));
    assert.equal(recipient.Item, undefined);
    assert.equal(await store.markSendRevoking(recoveryId, sender), true, 'An explicit retry can restore the pending barrier');
    assert.equal(await store.markSendRevoked(recoveryId, sender), true, 'Confirmed retry can finalize');
    assert((await store.getSendRevocationState(recoveryId, sender)).revoked_at);
    assert.equal(await store.acknowledgeSendRevocation(recoveryId, sender), false, 'Confirmed state cannot be relabeled unconfirmed');

    const menuSender = 'synthetic-menu-sender';
    const liveId = randomUUID();
    for (let i = 0; i <= 100; i++) {
      const hidden = i < 100;
      const id = hidden ? randomUUID() : liveId;
      await ddb.send(new PutCommand({ TableName: table, Item: {
        send_id: id, sender_discord_id: menuSender,
        ...(hidden ? { revoking_at: 'retained', revoke_acknowledged_at: 'retained' } : {}),
      } }));
      await ddb.send(new PutCommand({ TableName: `${process.env.DDB_TABLE_PREFIX}qurl-sends`, Item: {
        send_id: id, sender_discord_id: menuSender, recipient_discord_id: 'synthetic-recipient',
        created_at: hidden ? '2026-01-01T00:00:00.000Z' : '2000-01-01T00:00:00.000Z',
        resource_type: 'file', target_type: 'user', expires_in: '24h',
      } }));
    }
    assert.deepEqual((await store.getRecentSends(menuSender, 5)).map(row => row.send_id), [liveId]);
    const history = await store.getRecentSends(menuSender, 5, { acknowledgedOnly: true });
    assert.equal(history.length, 5);
    assert(history.every(row => row.send_id !== liveId && !row.revocation_confirmed));
    console.log('Barrier writes, owner-only acknowledgement, blocked Add transaction, later retry, and pagination past 100 hidden sends passed.');
  } finally {
    try {
      for (const name of created.reverse()) await client.send(new DeleteTableCommand({ TableName: name }));
    } finally {
      await store.close();
      client.destroy();
    }
  }
}

main().catch(err => { console.error(err); process.exitCode = 1; });
