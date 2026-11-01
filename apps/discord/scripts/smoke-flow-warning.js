// Local-only conditional-write proof; never uses an AWS endpoint or real credentials.
const assert = require('node:assert/strict');
const { randomBytes, randomUUID } = require('node:crypto');
const { DynamoDBClient, CreateTableCommand, DeleteTableCommand } = require('@aws-sdk/client-dynamodb');
const { DynamoDBDocumentClient, PutCommand, GetCommand, DeleteCommand, UpdateCommand } = require('@aws-sdk/lib-dynamodb');

async function main() {
  const endpoint = process.env.DDB_TEST_ENDPOINT;
  assert(endpoint, 'Set DDB_TEST_ENDPOINT to http://127.0.0.1:<port>');
  const url = new URL(endpoint);
  assert(url.protocol === 'http:' && url.hostname === '127.0.0.1' && url.port
    && !url.username && !url.password && url.pathname === '/' && !url.search && !url.hash, 'Local loopback endpoint required');
  process.env.AWS_REGION = 'us-east-2';
  process.env.AWS_ACCESS_KEY_ID = 'localflowwarning';
  process.env.AWS_SECRET_ACCESS_KEY = 'localflowwarning';
  delete process.env.AWS_SESSION_TOKEN;
  process.env.DDB_TABLE_PREFIX = `flow-warning-smoke-${randomUUID()}-`;
  process.env.KEY_ENCRYPTION_KEY = randomBytes(32).toString('base64');
  const { claimFlowWarning, transitionFlow } = require('../src/flow-state');
  const { encryptStrict } = require('../src/utils/crypto');
  const client = DynamoDBDocumentClient.from(new DynamoDBClient({
    endpoint, region: 'us-east-2', maxAttempts: 1,
    credentials: { accessKeyId: 'localflowwarning', secretAccessKey: 'localflowwarning' },
  }));
  const table = `${process.env.DDB_TABLE_PREFIX}flow-state`;
  const flow_id = '0:1#fixture-guild#fixture-channel#fixture-user';
  const nonce = 'first-lifecycle';
  const value = '999d';
  const expires = Math.floor(Date.now() / 1000) + 600;
  const makeRow = (sendNonce = nonce, version = 7) => ({
    flow_id, version, stage: 'awaiting_confirm', expires_at: expires,
    payload: encryptStrict(JSON.stringify({ sendNonce, expiresIn: value, selfDestructSeconds: 60 })),
  });
  const put = Item => client.send(new PutCommand({ TableName: table, Item }));
  const read = async () => (await client.send(new GetCommand({ TableName: table, Key: { flow_id }, ConsistentRead: true }))).Item;
  const claim = (sendNonce = nonce) => claimFlowWarning(flow_id, sendNonce, 'expiry', value);
  let created = false;
  const originalSend = DynamoDBDocumentClient.prototype.send;
  try {
    await client.send(new CreateTableCommand({
      TableName: table, BillingMode: 'PAY_PER_REQUEST',
      KeySchema: [{ AttributeName: 'flow_id', KeyType: 'HASH' }],
      AttributeDefinitions: [{ AttributeName: 'flow_id', AttributeType: 'S' }],
    }));
    created = true;
    const first = makeRow();
    await put(first);
    let reads = 0;
    let releaseReads;
    let rejectReads;
    const readBarrier = new Promise((resolve, reject) => { releaseReads = resolve; rejectReads = reject; });
    const barrierTimer = setTimeout(() => rejectReads(new Error('Concurrent local reads did not finish')), 2500);
    DynamoDBDocumentClient.prototype.send = async function (command, options) {
      const result = await originalSend.call(this, command, options);
      if (command instanceof GetCommand && command.input.TableName === table) {
        if (++reads === 16) releaseReads();
        await readBarrier;
      }
      return result;
    };
    let results;
    try { results = await Promise.all(Array.from({ length: 16 }, () => claim())); }
    finally { clearTimeout(barrierTimer); DynamoDBDocumentClient.prototype.send = originalSend; }
    assert.equal(reads, 16);
    assert.equal(results.filter(result => result === 'claimed').length, 1);
    assert.equal(await claim(), 'seen');
    let item = await read();
    assert.deepEqual(item, { ...first, corruption_warned_expiry: true });
    assert.equal(await claimFlowWarning(flow_id, nonce, 'self_destruct', 60), 'claimed');
    assert.deepEqual(await read(), { ...first, corruption_warned_expiry: true, corruption_warned_self_destruct: true });
    const transitioned = await transitionFlow(flow_id, 7, {
      stage_to: 'awaiting_confirm', terminal: false, payload: { sendNonce: nonce, expiresIn: value, selfDestructSeconds: 60 },
    });
    assert.equal(transitioned.result, 'success');
    item = await read();
    assert.equal(item.version, 8);
    assert.equal(item.expires_at, first.expires_at);
    assert.equal(item.corruption_warned_expiry, true);
    assert.equal(item.corruption_warned_self_destruct, true);


    // Replace the row after the strong read but before the actual conditional update.
    for (const scenario of ['new lifecycle', 'payload transition', 'expired', 'deleted']) {
      const before = makeRow();
      await put(before);
      const replacement = scenario === 'payload transition' ? makeRow(nonce, 8) : makeRow('replacement', 1);
      if (scenario === 'expired') Object.assign(replacement, { payload: before.payload, expires_at: Math.floor(Date.now() / 1000) - 1 });
      let changed = false;
      DynamoDBDocumentClient.prototype.send = async function (command, options) {
        if (!changed && command instanceof UpdateCommand && command.input.TableName === table) {
          changed = true;
          if (scenario === 'deleted') await client.send(new DeleteCommand({ TableName: table, Key: { flow_id } }));
          else await put(replacement);
        }
        return originalSend.call(this, command, options);
      };
      try { assert.equal(await claim(), 'retry', scenario); }
      finally { DynamoDBDocumentClient.prototype.send = originalSend; }
      item = await read();
      assert.deepEqual(item, scenario === 'deleted' ? undefined : replacement, scenario);
      if (scenario === 'new lifecycle') {
        assert.equal(await claim(), 'seen');
        assert.equal(await claim('replacement'), 'claimed');
      }
      if (scenario === 'payload transition') {
        assert.equal(await claim(), 'claimed');
        assert.equal((await read()).version, 8);
      }
    }

    const legacy = makeRow();
    legacy.payload = encryptStrict(JSON.stringify({ expiresIn: value }));
    await put(legacy);
    assert.equal(await claimFlowWarning(flow_id, undefined, 'expiry', value), 'claimed');
    assert.equal(await claimFlowWarning(flow_id, undefined, 'expiry', value), 'seen');
    await put({ ...legacy, version: 1, payload: encryptStrict(JSON.stringify({ expiresIn: value })) });
    assert.equal(await claimFlowWarning(flow_id, undefined, 'expiry', value), 'claimed');
    assert.equal((await read()).expires_at, expires);
    process.stdout.write('PASS local conditional claims: one concurrent winner; no version/TTL/payload changes; lifecycle, transition, expiry and deletion races; legacy reuse.\n');
  } finally {
    DynamoDBDocumentClient.prototype.send = originalSend;
    try { if (created) await client.send(new DeleteTableCommand({ TableName: table })); }
    finally { client.destroy(); }
  }
}
main().catch(error => { console.error(error.message); process.exitCode = 1; });
