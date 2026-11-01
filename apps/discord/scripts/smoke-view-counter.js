// Local-only request-count and source-gap proof; no Discord calls or AWS endpoints.
const assert = require('node:assert/strict');
const { randomBytes, randomUUID } = require('node:crypto');
const { DynamoDBClient, CreateTableCommand, DeleteTableCommand } = require('@aws-sdk/client-dynamodb');
const { DynamoDBDocumentClient, BatchGetCommand, TransactWriteCommand } = require('@aws-sdk/lib-dynamodb');

async function main() {
  const endpoint = process.env.DDB_TEST_ENDPOINT;
  assert(endpoint, 'Set DDB_TEST_ENDPOINT to http://127.0.0.1:<port>');
  const url = new URL(endpoint);
  assert(url.protocol === 'http:' && url.hostname === '127.0.0.1' && url.port
    && !url.username && !url.password && url.pathname === '/' && !url.search && !url.hash, 'Local loopback endpoint required');
  process.env.AWS_REGION = 'us-east-2';
  process.env.AWS_ACCESS_KEY_ID = 'localviewcounter';
  process.env.AWS_SECRET_ACCESS_KEY = 'localviewcounter';
  delete process.env.AWS_SESSION_TOKEN;
  const prefix = `view-counter-smoke-${randomUUID()}-`;
  process.env.DDB_TABLE_PREFIX = prefix;
  process.env.KEY_ENCRYPTION_KEY = randomBytes(32).toString('base64');
  const store = require('../src/store/ddb-store');
  const client = DynamoDBDocumentClient.from(new DynamoDBClient({ endpoint, region: 'us-east-2', maxAttempts: 1,
    credentials: { accessKeyId: 'localviewcounter', secretAccessKey: 'localviewcounter' } }));
  const viewsTable = `${prefix}qurl-views`;
  const configsTable = `${prefix}qurl-send-configs`;
  const created = [];
  const originalSend = DynamoDBDocumentClient.prototype.send;
  try {
    for (const [table, key] of [[viewsTable, 'qurl_id'], [configsTable, 'send_id']]) {
      await client.send(new CreateTableCommand({ TableName: table, BillingMode: 'PAY_PER_REQUEST',
        KeySchema: [{ AttributeName: key, KeyType: 'HASH' }], AttributeDefinitions: [{ AttributeName: key, AttributeType: 'S' }] }));
      created.push(table);
    }
    const sendId = 'fixture-send';
    const ids = Array.from({ length: 20000 }, (_, i) => (i + 1).toString(16).padStart(32, '0'));
    assert.equal(await store.getSendRenderedCount(sendId), null);
    await assert.rejects(store.getSendViewedCount(sendId, 20000), /unavailable/);
    await store.saveSendConfirmState(sendId, { expectedCount: 20000, viewedCount: 0 });
    assert.equal(await store.getSendRenderedCount(sendId), 0);
    assert.equal(await store.getSendViewedCount(sendId, 20000), 0);
    let incrementInput;
    DynamoDBDocumentClient.prototype.send = function (command, options) {
      if (command instanceof TransactWriteCommand) incrementInput = command.input;
      return originalSend.call(this, command, options);
    };
    await store.recordQurlView({ qurlId: ids[0], accessCount: 1, consumed: true, eventId: 'fixture-first' });
    await store.incrementSendViewedCount(sendId, ids[0], 20000);
    assert(incrementInput?.ClientRequestToken);
    await client.send(new TransactWriteCommand(incrementInput)); // Retry after a lost response.
    assert.equal(await store.getSendViewedCount(sendId, 20000), 1);
    await store.saveSendConfirmState(sendId, { expectedCount: 20000 });
    assert.equal(await store.getSendViewedCount(sendId, 20000), 1); // Initialization cannot reset a live count.
    assert.equal(await store.getSendRenderedCount(sendId), 0);
    const reads = [];
    DynamoDBDocumentClient.prototype.send = function (command, options) {
      if (command instanceof BatchGetCommand && command.input.RequestItems?.[viewsTable]) {
        reads.push(command.input.RequestItems[viewsTable].Keys.length);
      }
      return originalSend.call(this, command, options);
    };
    const source = await store.getQurlViews(ids);
    assert.equal(source.size, 1);
    assert.equal(source.get(ids[0]).accessCount, 1);
    assert.equal(reads.length, 200);
    assert(reads.every(count => count === 100));
    reads.length = 0;
    assert.equal(await store.getSendViewedCount(sendId, 20000), 1);
    assert.deepEqual(reads, [64]);
    // Crash boundary: source fact committed, but its shard increment never ran.
    await store.recordQurlView({ qurlId: ids[1], accessCount: 1, consumed: true, eventId: 'fixture-source-only' });
    assert.equal((await store.getQurlViews(ids.slice(0, 2))).size, 2);
    assert.equal(await store.getSendViewedCount(sendId, 20000), 1);
    process.stdout.write('PASS local store: 200 source pages versus one 64-key aggregate read; known zero differs from missing; transaction replay counts once; source-only crash gap is visible to source reads.\n');
  } finally {
    DynamoDBDocumentClient.prototype.send = originalSend;
    try { for (const table of created.reverse()) await client.send(new DeleteTableCommand({ TableName: table })); }
    finally { client.destroy(); }
  }
}
main().catch(error => { console.error(error.message); process.exitCode = 1; });
