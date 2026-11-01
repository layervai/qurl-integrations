const assert = require('node:assert/strict');
const { randomUUID } = require('node:crypto');
const { DynamoDBClient, CreateTableCommand, DeleteTableCommand } = require('@aws-sdk/client-dynamodb');
const { DynamoDBDocumentClient, GetCommand } = require('@aws-sdk/lib-dynamodb');
const { createPeerHeartbeat } = require('../src/gateway-peer-heartbeat');

async function run() {
  const endpoint = process.env.GATEWAY_HEARTBEAT_DDB_TEST_ENDPOINT;
  if (!endpoint) throw new Error('Set GATEWAY_HEARTBEAT_DDB_TEST_ENDPOINT to http://127.0.0.1:<port>');
  const url = new URL(endpoint);
  if (url.protocol !== 'http:' || url.hostname !== '127.0.0.1' || !url.port) {
    throw new Error('Heartbeat fixture must use http://127.0.0.1:<port>');
  }
  const client = DynamoDBDocumentClient.from(new DynamoDBClient({
    endpoint, region: 'us-east-2', maxAttempts: 1,
    credentials: { accessKeyId: 'localheartbeattest', secretAccessKey: 'localheartbeattest' },
  }));
  const tableName = `heartbeat-test-${randomUUID()}`;
  let created = false;
  try {
    await client.send(new CreateTableCommand({
      TableName: tableName, BillingMode: 'PAY_PER_REQUEST',
      KeySchema: [{ AttributeName: 'instance_id', KeyType: 'HASH' }],
      AttributeDefinitions: [{ AttributeName: 'instance_id', AttributeType: 'S' }],
    }));
    created = true;
    const writes = [];
    const events = [[], [], []];
    const now = Date.now();
    const ddbClient = { send: command => { writes.push(command); return client.send(command); } };
    const replicas = events.map(emitted => createPeerHeartbeat({
      ddbClient, tableName, instanceId: 'same-fixture-id', ip: '127.0.0.1', port: 9876,
      shardId: '0:1', clock: () => now,
      logger: { audit: event => emitted.push(event), warn: () => {} },
    }));
    await replicas[0].writeHeartbeat();
    await replicas[1].writeHeartbeat();
    assert.deepEqual(events, [[], [], []]);
    await replicas[0].writeHeartbeat();
    await replicas[1].writeHeartbeat();
    assert.deepEqual(events, [['gateway_instance_id_collision'], ['gateway_instance_id_collision'], []]);
    await replicas[2].writeHeartbeat();
    await replicas[2].writeHeartbeat();
    assert.deepEqual(events[2], []);
    assert.equal(writes.length, 6);
    for (const command of writes) {
      assert.equal(command.constructor.name, 'PutCommand');
      assert.equal(command.input.ReturnValues, 'ALL_OLD');
    }
    assert.equal(new Set(writes.map(command => command.input.Item.writer_id)).size, 3);
    const result = await client.send(new GetCommand({ TableName: tableName, Key: { instance_id: 'same-fixture-id' }, ConsistentRead: true }));
    assert.equal(result.Item.writer_id, writes[5].input.Item.writer_id);
    console.log('PASS actual ALL_OLD: six producer writes, no producer reads, two collision events, no stopped-predecessor event.');
  } finally {
    try { if (created) await client.send(new DeleteTableCommand({ TableName: tableName })); }
    finally { client.destroy(); }
  }
}

run().catch(error => { console.error(error.message); process.exitCode = 1; });
