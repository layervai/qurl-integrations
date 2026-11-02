// Loopback-only storage proof; no Discord calls or cloud endpoints.
const assert = require('node:assert/strict');
const { randomBytes, randomUUID } = require('node:crypto');
const { DynamoDBClient, CreateTableCommand, DeleteTableCommand } = require('@aws-sdk/client-dynamodb');
const { DynamoDBDocumentClient, GetCommand, PutCommand, DeleteCommand, UpdateCommand, BatchGetCommand, BatchWriteCommand } = require('@aws-sdk/lib-dynamodb');

async function main() {
  const endpoint = process.env.DDB_TEST_ENDPOINT;
  assert(endpoint, 'Set DDB_TEST_ENDPOINT to http://127.0.0.1:<port>');
  const url = new URL(endpoint);
  assert(url.protocol === 'http:' && url.hostname === '127.0.0.1' && url.port && !url.username && !url.password
    && url.pathname === '/' && !url.search && !url.hash, 'Local loopback endpoint required');
  process.env.AWS_REGION = 'us-east-2';
  process.env.AWS_ACCESS_KEY_ID = 'localflowrecipients';
  process.env.AWS_SECRET_ACCESS_KEY = 'localflowrecipients';
  delete process.env.AWS_SESSION_TOKEN;
  delete process.env.AWS_PROFILE;
  delete process.env.AWS_DEFAULT_PROFILE;
  process.env.AWS_EC2_METADATA_DISABLED = 'true';
  process.env.DDB_TABLE_PREFIX = `flow-recipients-${randomUUID()}-`;
  process.env.KEY_ENCRYPTION_KEY = randomBytes(32).toString('base64');
  const { createFlow, loadFlow, hydrateFlowRecipients, transitionFlow, deleteFlow, supersedeOrCreate, claimFlowWarning } = require('../src/flow-state');
  const { decrypt, encryptStrict } = require('../src/utils/crypto');
  const { gzipSync } = require('node:zlib');
  const client = DynamoDBDocumentClient.from(new DynamoDBClient({ endpoint, region: 'us-east-2', maxAttempts: 1,
    credentials: { accessKeyId: 'localflowrecipients', secretAccessKey: 'localflowrecipients' } }));
  const table = `${process.env.DDB_TABLE_PREFIX}flow-state`;
  const flow_id = '0:1#fixture-guild#fixture-channel#fixture-user';
  const stage = 'awaiting_send_confirm';
  const expires = Math.floor(Date.now() / 1000) + 180;
  const recipients = Array.from({ length: 20000 }, (_, i) => ({ id: String(100000000000000000n + BigInt(i)), alias: `User${String(i).padStart(5, '0')}` }));
  const payload = { recipients, sendNonce: 'fixture-lifecycle', expiresIn: '1h' };
  const get = async key => (await client.send(new GetCommand({ TableName: table, Key: { flow_id: key }, ConsistentRead: true }))).Item;
  const originalSend = DynamoDBDocumentClient.prototype.send;
  let created = false;
  try {
    await client.send(new CreateTableCommand({ TableName: table, BillingMode: 'PAY_PER_REQUEST',
      KeySchema: [{ AttributeName: 'flow_id', KeyType: 'HASH' }], AttributeDefinitions: [{ AttributeName: 'flow_id', AttributeType: 'S' }] }));
    created = true;
    assert.equal((await createFlow({ flow_id, stage, payload, expires_at: expires })).created, true);
    let parent = await get(flow_id);
    assert(Buffer.byteLength(JSON.stringify(parent)) < 400 * 1024);
    let stored = JSON.parse(decrypt(parent.payload));
    assert.equal(stored.recipients, undefined);
    assert.equal(stored.__qurl_recipient_set.parts, 79);
    assert.equal(stored.__qurl_recipient_set.count, 20000);
    assert.deepEqual((await loadFlow(flow_id)).payload, payload);
    const metadata = await loadFlow(flow_id, { defer_recipients: true });
    assert.equal(metadata.recipient_payload_deferred, true);
    assert.equal(metadata.payload.recipients, undefined);
    assert(!Object.keys(metadata).includes('payloadSnapshot'));
    await assert.rejects(createFlow({ flow_id: '0:1#fixture-guild#fixture-channel#reference-copy', stage, payload: metadata.payload, expires_at: expires }), /hydrated before writing/);
    await assert.rejects(transitionFlow(flow_id, 1, { stage_to: stage, payload: metadata.payload, terminal: false,
      set_expires_at: expires + 180, expectedPayload: metadata.payloadSnapshot }), /hydrated before writing/);
    assert.equal((await get(flow_id)).payload, parent.payload);
    const hydrated = await hydrateFlowRecipients(metadata);
    assert.deepEqual(hydrated.payload, payload);
    assert.equal(hydrated.payloadSnapshot, metadata.payloadSnapshot);
    assert(!Object.hasOwn(hydrated, 'recipient_payload_deferred'));
    assert.equal(await claimFlowWarning(flow_id, payload.sendNonce, 'expiry', payload.expiresIn), 'claimed');
    assert.equal(await claimFlowWarning(flow_id, payload.sendNonce, 'expiry', payload.expiresIn), 'seen');
    assert.equal((await transitionFlow(flow_id, 1, { stage_to: stage, payload, terminal: false, set_expires_at: expires + 180 })).result, 'success');
    assert.equal((await transitionFlow(flow_id, 2, { stage_to: stage, terminal: false, set_expires_at: expires + 360 })).result, 'success');
    parent = await get(flow_id);
    stored = JSON.parse(decrypt(parent.payload));
    assert.equal(parent.version, 3);
    assert.deepEqual((await loadFlow(flow_id)).payload, payload);
    const ref = stored.__qurl_recipient_set;
    const chunkKey = `__flow_recipients__${ref.id}#0`;
    const chunk = await get(chunkKey);
    assert.equal(chunk.expires_at, parent.expires_at);
    assert.equal(chunk.expires_at, expires + 360);
    assert(!chunk.payload.includes('User00000'));
    assert.equal((await transitionFlow(flow_id, 1, { stage_to: stage, payload: { ...payload, sendNonce: 'stale' }, terminal: false })).result, 'conflict');
    assert.equal((await get(flow_id)).payload, parent.payload);
    assert.deepEqual((await loadFlow(flow_id)).payload, payload);

    // Model partition pagination while reading real rows: only one key is processed per call.
    DynamoDBDocumentClient.prototype.send = async function (command, options) {
      if (!(command instanceof BatchGetCommand)) return originalSend.call(this, command, options);
      const request = command.input.RequestItems[table];
      assert(request.Keys.length <= 4);
      const result = await originalSend.call(this, new BatchGetCommand({ RequestItems: { [table]: { ...request, Keys: request.Keys.slice(0, 1) } } }), options);
      if (request.Keys.length > 1) result.UnprocessedKeys = { [table]: { ...request, Keys: request.Keys.slice(1) } };
      return result;
    };
    assert.deepEqual((await loadFlow(flow_id)).payload, payload);
    DynamoDBDocumentClient.prototype.send = originalSend;

    // Same version after delete/recreate is still a different flow snapshot.
    const abaID = '0:1#fixture-guild#fixture-channel#aba';
    await createFlow({ flow_id: abaID, stage, payload, expires_at: expires });
    const old = await loadFlow(abaID);
    assert.equal((await deleteFlow(abaID, { stage, reason: 'terminal', expectedVersion: 1, expectedPayload: old.payloadSnapshot })).deleted, true);
    const replacement = { recipients: recipients.slice(0, 1), sendNonce: 'replacement' };
    await createFlow({ flow_id: abaID, stage, payload: replacement, expires_at: expires });
    assert.equal((await deleteFlow(abaID, { stage, reason: 'terminal', expectedVersion: 1, expectedPayload: old.payloadSnapshot })).deleted, false);
    assert.equal((await transitionFlow(abaID, 1, { stage_to: stage, payload: old.payload, terminal: false, expectedPayload: old.payloadSnapshot })).result, 'conflict');
    assert.deepEqual((await loadFlow(abaID)).payload, replacement);

    // A clock advance during staging must be checked by the parent CAS after staging.
    const raceID = '0:1#fixture-guild#fixture-channel#expiry-race';
    const realNow = Date.now;
    await createFlow({ flow_id: raceID, stage, payload: { sendNonce: 'race' }, expires_at: Math.floor(realNow() / 1000) + 2 });
    let advanced = false;
    DynamoDBDocumentClient.prototype.send = async function (command, options) {
      const result = await originalSend.call(this, command, options);
      if (command instanceof BatchWriteCommand && !advanced) {
        advanced = true;
        Date.now = () => realNow() + 3000;
      }
      return result;
    };
    try {
      assert.equal((await transitionFlow(raceID, 1, { stage_to: stage, payload, terminal: false, set_expires_at: expires + 180 })).result, 'not_found');
    } finally { Date.now = realNow; DynamoDBDocumentClient.prototype.send = originalSend; }
    assert.equal((await get(raceID)).version, 1);

    const longID = '0:1#fixture-guild#fixture-channel#long-aliases';
    const longRecipients = recipients.map(recipient => ({ id: recipient.id, alias: '😀'.repeat(64) }));
    await createFlow({ flow_id: longID, stage, payload: { recipients: longRecipients }, expires_at: expires });
    assert.deepEqual((await loadFlow(longID)).payload.recipients, longRecipients);
    const nullID = '0:1#fixture-guild#fixture-channel#null';
    await createFlow({ flow_id: nullID, stage, payload: null, expires_at: expires });
    assert.equal((await deleteFlow(nullID, { stage, reason: 'terminal', expectedVersion: 1, expectedPayload: null })).deleted, true);

    // An incomplete provider batch must not publish a parent or return a partial list.
    DynamoDBDocumentClient.prototype.send = function (command, options) {
      if (command instanceof BatchWriteCommand) return Promise.resolve({ UnprocessedItems: command.input.RequestItems });
      return originalSend.call(this, command, options);
    };
    const unpublished = '0:1#fixture-guild#fixture-channel#unpublished';
    await assert.rejects(createFlow({ flow_id: unpublished, stage, payload, expires_at: expires }), /write incomplete/);
    DynamoDBDocumentClient.prototype.send = originalSend;
    assert.equal(await get(unpublished), undefined);
    DynamoDBDocumentClient.prototype.send = function (command, options) {
      if (command instanceof BatchGetCommand) return Promise.resolve({ UnprocessedKeys: command.input.RequestItems });
      return originalSend.call(this, command, options);
    };
    await assert.rejects(loadFlow(flow_id), /read incomplete/);
    DynamoDBDocumentClient.prototype.send = originalSend;
    assert.deepEqual((await loadFlow(flow_id)).payload, payload);

    const oversizedID = '0:1#fixture-guild#fixture-channel#oversized';
    await assert.rejects(createFlow({ flow_id: oversizedID, stage,
      payload: { recipients: [{ id: recipients[0].id, alias: 'x'.repeat(200 * 1024) }] }, expires_at: expires }), /chunk size limit/);
    assert.equal(await get(oversizedID), undefined);
    // A malformed authenticated row must not bypass the decompression bound.
    const oversizedValues = recipients.slice(0, 256).map(recipient => ({ ...recipient, alias: 'x'.repeat(1000) }));
    await client.send(new PutCommand({ TableName: table, Item: { ...chunk,
      payload: encryptStrict(gzipSync(JSON.stringify(oversizedValues)).toString('base64')) } }));
    await assert.rejects(loadFlow(flow_id));
    await client.send(new PutCommand({ TableName: table, Item: chunk }));
    assert.deepEqual((await loadFlow(flow_id)).payload, payload);

    await client.send(new DeleteCommand({ TableName: table, Key: { flow_id: chunkKey } }));
    await assert.rejects(loadFlow(flow_id), /missing or mismatched/);
    await assert.rejects(transitionFlow(flow_id, parent.version, { stage_to: stage, terminal: false,
      set_expires_at: expires + 720, expectedPayload: parent.payload }), /missing or mismatched/);
    assert.equal((await get(flow_id)).payload, parent.payload);
    await client.send(new PutCommand({ TableName: table, Item: chunk }));
    assert.deepEqual((await loadFlow(flow_id)).payload, payload);
    // A broken live set can be replaced without reading its missing children.
    const brokenID = '0:1#fixture-guild#fixture-channel#broken-live';
    await createFlow({ flow_id: brokenID, stage, payload, expires_at: expires });
    const brokenRef = JSON.parse(decrypt((await get(brokenID)).payload)).__qurl_recipient_set;
    await client.send(new DeleteCommand({ TableName: table, Key: { flow_id: `__flow_recipients__${brokenRef.id}#0` } }));
    assert.equal((await supersedeOrCreate({ flow_id: brokenID, stage, payload: { sendNonce: 'restart' }, ttl_seconds: 180 })).created, true);
    assert.deepEqual((await loadFlow(brokenID)).payload, { sendNonce: 'restart' });
    // Expired slot cleanup must not depend on chunks that TTL may already have reaped.
    await client.send(new DeleteCommand({ TableName: table, Key: { flow_id: chunkKey } }));
    await client.send(new UpdateCommand({ TableName: table, Key: { flow_id }, UpdateExpression: 'SET expires_at = :expired',
      ExpressionAttributeValues: { ':expired': Math.floor(Date.now() / 1000) - 1 } }));
    assert.equal(await loadFlow(flow_id), null);
    const expiredMetadata = await loadFlow(flow_id, { include_expired: true });
    assert.equal(expiredMetadata.stage, stage);
    assert.equal(expiredMetadata.recipient_payload_deferred, true);
    assert.equal(await hydrateFlowRecipients(expiredMetadata), null);
    assert.equal((await supersedeOrCreate({ flow_id, stage, payload: { sendNonce: 'replacement' }, ttl_seconds: 180 })).created, true);
    assert.deepEqual((await loadFlow(flow_id)).payload, { sendNonce: 'replacement' });
    process.stdout.write('PASS local flow recipients: 20,000 aliases round-trip through 79 encrypted chunks; parent fits; compressed chunks are bounded; payload/TTL transitions, same-version replacement and expiry-staging races are safe; incomplete write/read and missing chunks fail closed; expired slot recovery works.\n');
  } finally {
    DynamoDBDocumentClient.prototype.send = originalSend;
    try { if (created) await client.send(new DeleteTableCommand({ TableName: table })); }
    finally { client.destroy(); }
  }
}
main().catch(error => { console.error(error.message); process.exitCode = 1; });
