jest.mock('../src/logger', () => ({
  audit: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(),
}));

// Opt in with a local emulator; this test never connects to an AWS endpoint.
const localTest = process.env.DDB_TEST_ENDPOINT ? test : test.skip;
localTest('real SDK flow transitions retain success/conflict/absence with one update each', async () => {
  const endpoint = process.env.DDB_TEST_ENDPOINT;
  if (!['127.0.0.1', 'localhost', '[::1]'].includes(new URL(endpoint).hostname)) {
    throw new Error('The flow contract test requires a loopback DynamoDB endpoint');
  }
  const { randomUUID } = require('node:crypto');
  const { DynamoDBClient, CreateTableCommand, DeleteTableCommand } = require('@aws-sdk/client-dynamodb');
  const { DynamoDBDocumentClient, PutCommand } = require('@aws-sdk/lib-dynamodb');
  const prefix = `flow-contract-${randomUUID()}-`;
  const table = `${prefix}flow-state`;
  const values = {
    DDB_TABLE_PREFIX: prefix, AWS_REGION: 'us-east-2',
    AWS_ACCESS_KEY_ID: 'local', AWS_SECRET_ACCESS_KEY: 'local', AWS_SESSION_TOKEN: undefined,
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
    const { transitionFlow } = require('../src/flow-state');
    await raw.send(new CreateTableCommand({ TableName: table, BillingMode: 'PAY_PER_REQUEST',
      AttributeDefinitions: [{ AttributeName: 'flow_id', AttributeType: 'S' }],
      KeySchema: [{ AttributeName: 'flow_id', KeyType: 'HASH' }] }));
    created = true;
    await doc.send(new PutCommand({ TableName: table, Item: { flow_id: 'row', version: 1,
      stage: 'old', expires_at: Math.floor(Date.now() / 1000) + 600,
      payload: 'synthetic-ciphertext-must-not-be-decrypted-or-logged' } }));
    for (const [id, result, version] of [['row', 'success', 2], ['row', 'conflict', null], ['missing', 'not_found', null]]) {
      send.mockClear();
      expect(await transitionFlow(id, 1, { stage_to: 'new', terminal: false })).toEqual({ result, version });
      expect(send.mock.calls.map(([command]) => command.constructor.name)).toEqual(['UpdateCommand']);
    }
    const logger = require('../src/logger');
    expect(logger.audit.mock.calls.map(([, event]) => event.stage_from)).toEqual(['old', 'new', null]);
    expect(JSON.stringify(logger.audit.mock.calls)).not.toContain('synthetic-ciphertext');
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
