jest.mock('../src/logger', () => ({
  audit: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(),
}));

// Require both the emulator and the Node capability used by the real SDK.
// Exporting the dev endpoint alone must not break a normal Jest run.
const localTest = process.env.DDB_TEST_ENDPOINT && typeof require('node:vm').SourceTextModule === 'function' ? test : test.skip;
localTest('real SDK flow transitions retain success/conflict/absence with one update each', async () => {
  const endpoint = process.env.DDB_TEST_ENDPOINT;
  if (!['127.0.0.1', 'localhost', '[::1]'].includes(new URL(endpoint).hostname)) {
    throw new Error('The flow contract test requires a loopback DynamoDB endpoint');
  }
  const { randomUUID, randomBytes } = require('node:crypto');
  const { DynamoDBClient, CreateTableCommand, DeleteTableCommand } = require('@aws-sdk/client-dynamodb');
  const { DynamoDBDocumentClient, PutCommand, UpdateCommand } = require('@aws-sdk/lib-dynamodb');
  const prefix = `flow-contract-${randomUUID()}-`;
  const table = `${prefix}flow-state`;
  const values = {
    DDB_TABLE_PREFIX: prefix, AWS_REGION: 'us-east-2',
    AWS_ACCESS_KEY_ID: 'local', AWS_SECRET_ACCESS_KEY: 'local', AWS_SESSION_TOKEN: undefined,
    KEY_ENCRYPTION_KEY: randomBytes(32).toString('base64'),
  };
  const before = Object.fromEntries(Object.keys(values).map(key => [key, process.env[key]]));
  for (const [key, value] of Object.entries(values)) {
    if (value === undefined) delete process.env[key]; else process.env[key] = value;
  }
  const raw = new DynamoDBClient({ region: 'us-east-2', endpoint,
    credentials: { accessKeyId: 'local', secretAccessKey: 'local' } });
  const doc = DynamoDBDocumentClient.from(raw);
  const send = jest.spyOn(DynamoDBDocumentClient.prototype, 'send');
  let created = false;
  try {
    const { transitionFlow, createFlow, supersedeOrCreate, loadFlow, deleteFlow } = require('../src/flow-state');
    await raw.send(new CreateTableCommand({ TableName: table, BillingMode: 'PAY_PER_REQUEST',
      AttributeDefinitions: [{ AttributeName: 'flow_id', AttributeType: 'S' }],
      KeySchema: [{ AttributeName: 'flow_id', KeyType: 'HASH' }] }));
    created = true;
    await doc.send(new PutCommand({ TableName: table, Item: { flow_id: 'row', version: 1,
      stage: 'old', expires_at: Math.floor(Date.now() / 1000) + 600,
      payload: 'synthetic-ciphertext-must-not-be-decrypted-or-logged' } }));
    await doc.send(new PutCommand({ TableName: table, Item: { flow_id: 'expired', version: 1,
      stage: 'expired-stage', expires_at: Math.floor(Date.now() / 1000) - 30 } }));
    const logger = require('../src/logger');
    const sameExpiry = Math.floor(Date.now() / 1000) + 600;
    await doc.send(new PutCommand({ TableName: table, Item: { flow_id: 'same', version: 1,
      stage: 'same-stage', expires_at: sameExpiry } }));
    send.mockClear();
    expect(await transitionFlow('same', 1, { stage_to: 'same-stage', terminal: false, set_expires_at: sameExpiry })).toEqual({ result: 'success', version: 2 });
    expect(send.mock.calls.map(([command]) => command.constructor.name)).toEqual(['UpdateCommand']);
    expect(logger.audit).toHaveBeenLastCalledWith(expect.any(String), expect.objectContaining({ stage_from: 'same-stage', extended: false }));
    logger.audit.mockClear();
    for (const [id, result, version] of [['row', 'success', 2], ['row', 'conflict', null], ['missing', 'not_found', null], ['expired', 'not_found', null]]) {
      send.mockClear();
      expect(await transitionFlow(id, 1, { stage_to: 'new', terminal: false })).toEqual({ result, version });
      expect(send.mock.calls.map(([command]) => command.constructor.name)).toEqual(['UpdateCommand']);
    }
    expect(logger.audit.mock.calls.map(([, event]) => event.stage_from)).toEqual(['old', 'new', null, 'expired-stage']);
    expect(JSON.stringify(logger.audit.mock.calls)).not.toContain('synthetic-ciphertext');

    // #273/#274: exercise the real conditional delete across the exact TTL boundary.
    const clock = jest.spyOn(Date, 'now').mockReturnValue(Date.now());
    try {
      const now = Math.floor(Date.now() / 1000);
      for (const [suffix, expiry, recovered] of [['live', now + 1, false], ['equal', now, false], ['expired', now - 1, true]]) {
        const flowId = `0:1#fixture-guild#fixture-channel#${suffix}`;
        expect(await createFlow({ flow_id: flowId, stage: 'sibling', payload: { fixture: 'original' }, expires_at: now + 60 })).toEqual({ created: true, version: 1 });
        await doc.send(new UpdateCommand({ TableName: table, Key: { flow_id: flowId },
          UpdateExpression: 'SET expires_at = :expiry', ExpressionAttributeValues: { ':expiry': expiry } }));
        expect((await supersedeOrCreate({ flow_id: flowId, stage: 'target', payload: { fixture: 'replacement' }, ttl_seconds: 60 })).created).toBe(recovered);
        const survivor = await loadFlow(flowId, { include_expired: true });
        expect(survivor.stage).toBe(recovered ? 'target' : 'sibling');
        expect(survivor.payload.fixture).toBe(recovered ? 'replacement' : 'original');
        if (recovered) {
          // The new row reuses version 1: expiry must still protect it from stale cleanup.
          expect(await deleteFlow(flowId, { stage: 'target', reason: 'admin_cleanup', expectedVersion: 1, expiredOnly: true })).toEqual({ deleted: false });
          expect(await transitionFlow(flowId, 1, { stage_to: 'target', terminal: false })).toEqual({ result: 'success', version: 2 });
          expect(await deleteFlow(flowId, { stage: 'target', reason: 'admin_cleanup', expectedVersion: 1 })).toEqual({ deleted: false });
        }
      }
    } finally { clock.mockRestore(); }
  } finally {
    send.mockRestore();
    try {
      if (created) await raw.send(new DeleteTableCommand({ TableName: table }));
    } finally {
      raw.destroy();
      for (const [key, value] of Object.entries(before)) {
        if (value === undefined) delete process.env[key]; else process.env[key] = value;
      }
    }
  }
}, 30_000);
