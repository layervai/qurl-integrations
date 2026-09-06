
jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

jest.mock('../src/utils/crypto', () => ({
  encryptStrict: jest.fn((v) => (v == null ? v : `enc:v1:IV:TAG:${Buffer.from(String(v)).toString('hex')}`)),
  decrypt: jest.fn((v) => {
    if (v == null || typeof v !== 'string') return v;
    if (!v.startsWith('enc:v1:')) return v;
    const parts = v.split(':');
    return Buffer.from(parts[4], 'hex').toString();
  }),
}));

const { mockClient } = require('aws-sdk-client-mock');
const {
  DynamoDBDocumentClient,
  GetCommand,
  PutCommand,
  DeleteCommand,
  UpdateCommand,
} = require('@aws-sdk/lib-dynamodb');

const ddbMock = mockClient(DynamoDBDocumentClient);

process.env.DDB_TABLE_PREFIX = 'test-prefix-';
process.env.AWS_REGION = 'us-east-2';

const flowState = require('../src/flow-state');
const logger = require('../src/logger');
const { encryptStrict, decrypt } = require('../src/utils/crypto');
const { AUDIT_EVENTS } = require('../src/constants');

const EXPECTED_TABLE = 'test-prefix-flow-state';
const FLOW_ID = '0:1#g#c#u';
function futureExpiry(offset_seconds = 600) {
  return Math.floor(Date.now() / 1000) + offset_seconds;
}

function ccfe() {
  const e = new Error('The conditional request failed');
  e.name = 'ConditionalCheckFailedException';
  return e;
}

beforeEach(() => {
  ddbMock.reset();
  logger.audit.mockReset();
  logger.error.mockReset();
  logger.warn.mockReset();
  logger.debug.mockReset();
  encryptStrict.mockClear();
  decrypt.mockClear();
});

describe('flow-state — module sanity', () => {
  test('exposes the five lifecycle methods', () => {
    expect(typeof flowState.createFlow).toBe('function');
    expect(typeof flowState.loadFlow).toBe('function');
    expect(typeof flowState.transitionFlow).toBe('function');
    expect(typeof flowState.deleteFlow).toBe('function');
    expect(typeof flowState.supersedeOrCreate).toBe('function');
  });

  test('computes the canonical table name from DDB_TABLE_PREFIX', () => {
    expect(flowState.__TABLE_NAME).toBe(EXPECTED_TABLE);
  });
});

describe('flow-state.createFlow', () => {
  test('writes the row with version=1, encrypts payload, emits FLOW_CREATED', async () => {
    ddbMock.on(PutCommand).resolves({});
    const expiresAt = futureExpiry();

    const res = await flowState.createFlow({
      flow_id: FLOW_ID,
      stage: 'awaiting_button',
      payload: { foo: 'bar' },
      expires_at: expiresAt,
    });

    expect(res).toEqual({ created: true, version: 1 });

    const call = ddbMock.commandCalls(PutCommand)[0];
    expect(call.args[0].input.TableName).toBe(EXPECTED_TABLE);
    expect(call.args[0].input.ConditionExpression).toBe('attribute_not_exists(flow_id)');
    const item = call.args[0].input.Item;
    expect(item.flow_id).toBe(FLOW_ID);
    expect(item.stage).toBe('awaiting_button');
    expect(item.version).toBe(1);
    expect(item.expires_at).toBe(expiresAt);
    expect(typeof item.created_at).toBe('number');
    expect(typeof item.updated_at).toBe('number');
    expect(typeof item.payload).toBe('string');
    expect(item.payload.startsWith('enc:v1:')).toBe(true);
    expect(decrypt(item.payload)).toBe('{"foo":"bar"}');

    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_CREATED, {
      flow_id: FLOW_ID,
      stage: 'awaiting_button',
    });
  });

  test('undefined payload persists as null (symmetric with explicit null)', async () => {
    ddbMock.on(PutCommand).resolves({});
    await flowState.createFlow({
      flow_id: FLOW_ID,
      stage: 's',
      expires_at: futureExpiry(),
    });
    const call = ddbMock.commandCalls(PutCommand)[0];
    expect(call.args[0].input.Item.payload).toBeNull();
    expect(encryptStrict).not.toHaveBeenCalled();
  });

  test('null payload persists as null (not encrypted, not silently coerced)', async () => {
    ddbMock.on(PutCommand).resolves({});
    await flowState.createFlow({
      flow_id: FLOW_ID,
      stage: 's',
      payload: null,
      expires_at: futureExpiry(),
    });
    const call = ddbMock.commandCalls(PutCommand)[0];
    expect(call.args[0].input.Item.payload).toBeNull();
    expect(encryptStrict).not.toHaveBeenCalled();
  });

  test('created_at and updated_at are equal on row birth (single now() call)', async () => {
    ddbMock.on(PutCommand).resolves({});
    await flowState.createFlow({
      flow_id: FLOW_ID,
      stage: 's',
      expires_at: futureExpiry(),
    });
    const item = ddbMock.commandCalls(PutCommand)[0].args[0].input.Item;
    expect(item.created_at).toBe(item.updated_at);
  });

  test('returns { created: false } and skips FLOW_CREATED when row exists (OCC)', async () => {
    ddbMock.on(PutCommand).rejects(ccfe());

    const res = await flowState.createFlow({
      flow_id: FLOW_ID,
      stage: 's',
      expires_at: futureExpiry(),
    });

    expect(res).toEqual({ created: false });
    expect(logger.audit).not.toHaveBeenCalled();
  });

  test('redelivery emits a debug breadcrumb for triage', async () => {
    ddbMock.on(PutCommand).rejects(ccfe());

    await flowState.createFlow({
      flow_id: FLOW_ID,
      stage: 's',
      expires_at: futureExpiry(),
    });

    expect(logger.debug).toHaveBeenCalledWith(
      expect.stringMatching(/idempotent redelivery/),
      expect.objectContaining({ flow_id: FLOW_ID }),
    );
  });

  test('rethrows unexpected DDB errors', async () => {
    ddbMock.on(PutCommand).rejects(new Error('AccessDenied'));
    await expect(flowState.createFlow({
      flow_id: FLOW_ID,
      stage: 's',
      expires_at: futureExpiry(),
    })).rejects.toThrow('AccessDenied');
  });

  test.each([
    ['string', 'not-a-number'],
    ['float', 1.5],
    ['NaN', NaN],
    ['Infinity', Infinity],
    ['undefined', undefined],
  ])('rejects expires_at that is not a finite integer: %s', async (_label, badValue) => {
    await expect(flowState.createFlow({
      flow_id: FLOW_ID,
      stage: 's',
      expires_at: badValue,
    })).rejects.toThrow(/expires_at must be a finite integer/);
  });

  test('rejects expires_at in the past (silent-SLI-inflation foot-gun)', async () => {
    await expect(flowState.createFlow({
      flow_id: FLOW_ID,
      stage: 's',
      expires_at: Math.floor(Date.now() / 1000) - 1,
    })).rejects.toThrow(/must be strictly in the future/);
  });

  test('rejects expires_at equal to now', async () => {
    await expect(flowState.createFlow({
      flow_id: FLOW_ID,
      stage: 's',
      expires_at: Math.floor(Date.now() / 1000),
    })).rejects.toThrow(/must be strictly in the future/);
  });

  test('rejects malformed flow_id (parseFlowId returns null)', async () => {
    await expect(flowState.createFlow({
      flow_id: 'not-a-shard-aware-key',
      stage: 's',
      expires_at: futureExpiry(),
    })).rejects.toThrow(/is not a parseable shard-aware composite key/);
  });

  test('rejects empty flow_id', async () => {
    await expect(flowState.createFlow({
      flow_id: '',
      stage: 's',
      expires_at: futureExpiry(),
    })).rejects.toThrow(/flow_id must be a non-empty string/);
  });

  test('rejects empty stage', async () => {
    await expect(flowState.createFlow({
      flow_id: FLOW_ID,
      stage: '',
      expires_at: futureExpiry(),
    })).rejects.toThrow(/stage must be a non-empty string/);
  });
});

describe('flow-state.loadFlow', () => {
  test('returns the row with payload decrypted on happy path', async () => {
    const expiresAt = futureExpiry();
    ddbMock.on(GetCommand).resolves({
      Item: {
        flow_id: 'id',
        stage: 's',
        version: 3,
        payload: `enc:v1:IV:TAG:${Buffer.from('{"k":"v"}').toString('hex')}`,
        expires_at: expiresAt,
        created_at: 1000,
        updated_at: 1100,
      },
    });

    const res = await flowState.loadFlow('id');
    expect(res).toEqual({
      flow_id: 'id',
      stage: 's',
      version: 3,
      payload: { k: 'v' },
      expires_at: expiresAt,
      created_at: 1000,
      updated_at: 1100,
    });

    const call = ddbMock.commandCalls(GetCommand)[0];
    expect(call.args[0].input.TableName).toBe(EXPECTED_TABLE);
    expect(call.args[0].input.ConsistentRead).toBe(true);
  });

  test('returns null when row is absent', async () => {
    ddbMock.on(GetCommand).resolves({});
    expect(await flowState.loadFlow('id')).toBeNull();
  });

  test('returns null when now > expires_at (logically expired but not yet reaped)', async () => {
    const pastExpiry = Math.floor(Date.now() / 1000) - 60;
    ddbMock.on(GetCommand).resolves({
      Item: {
        flow_id: 'id',
        stage: 's',
        version: 1,
        payload: null,
        expires_at: pastExpiry,
      },
    });
    expect(await flowState.loadFlow('id')).toBeNull();
  });

  test('grace_seconds tolerates a small clock-skew window', async () => {
    const recentExpiry = Math.floor(Date.now() / 1000) - 5;
    ddbMock.on(GetCommand).resolves({
      Item: {
        flow_id: 'id',
        stage: 's',
        version: 1,
        payload: null,
        expires_at: recentExpiry,
      },
    });
    expect(await flowState.loadFlow('id')).toBeNull();
    const withGrace = await flowState.loadFlow('id', { grace_seconds: 60 });
    expect(withGrace).not.toBeNull();
    expect(withGrace.flow_id).toBe('id');
  });

  test('returns null and warns when expires_at is missing (fail-safe vs corrupted row)', async () => {
    ddbMock.on(GetCommand).resolves({
      Item: {
        flow_id: 'id',
        stage: 's',
        version: 1,
        payload: null,
      },
    });
    expect(await flowState.loadFlow('id')).toBeNull();
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringMatching(/missing or non-numeric expires_at/),
      expect.any(Object),
    );
  });

  test('returns null and warns when expires_at is a non-integer float (writer/reader symmetry)', async () => {
    ddbMock.on(GetCommand).resolves({
      Item: {
        flow_id: 'id',
        stage: 's',
        version: 1,
        payload: null,
        expires_at: 1234567890.5,
      },
    });
    expect(await flowState.loadFlow('id')).toBeNull();
    expect(logger.warn).toHaveBeenCalled();
  });

  test('returns null and warns when expires_at is a string (legacy/corrupt writer)', async () => {
    ddbMock.on(GetCommand).resolves({
      Item: {
        flow_id: 'id',
        stage: 's',
        version: 1,
        payload: null,
        expires_at: '2026-05-11T00:00:00Z',
      },
    });
    expect(await flowState.loadFlow('id')).toBeNull();
    expect(logger.warn).toHaveBeenCalled();
  });

  test('rejects empty flow_id', async () => {
    await expect(flowState.loadFlow('')).rejects.toThrow(/flow_id must be a non-empty string/);
  });

  test.each([
    ['string', 'forever'],
    ['NaN', NaN],
    ['Infinity', Infinity],
    ['null', null],
    ['object', {}],
  ])('rejects non-finite-number grace_seconds: %s', async (_label, badValue) => {
    await expect(flowState.loadFlow('id', { grace_seconds: badValue }))
      .rejects.toThrow(/grace_seconds must be a non-negative finite number/);
  });

  test('rejects negative grace_seconds (typo guard)', async () => {
    await expect(flowState.loadFlow('id', { grace_seconds: -1 }))
      .rejects.toThrow(/grace_seconds must be a non-negative finite number/);
    await expect(flowState.loadFlow('id', { grace_seconds: -3600 }))
      .rejects.toThrow(/grace_seconds must be a non-negative finite number/);
  });

  test('include_expired:true surfaces a logically-expired row (stuck-orphan recovery)', async () => {
    const pastExpiry = Math.floor(Date.now() / 1000) - 60;
    ddbMock.on(GetCommand).resolves({
      Item: {
        flow_id: 'id',
        stage: 'awaiting_setup_modal',
        version: 3,
        payload: null,
        expires_at: pastExpiry,
        created_at: 100,
        updated_at: 200,
      },
    });

    expect(await flowState.loadFlow('id')).toBeNull();

    const row = await flowState.loadFlow('id', { include_expired: true });
    expect(row).not.toBeNull();
    expect(row.stage).toBe('awaiting_setup_modal');
    expect(row.version).toBe(3);
    expect(row.expires_at).toBe(pastExpiry);
  });

  test('include_expired:true STILL filters corrupted rows (missing expires_at)', async () => {
    ddbMock.on(GetCommand).resolves({
      Item: { flow_id: 'id', stage: 's', version: 1, payload: null },
    });
    expect(await flowState.loadFlow('id', { include_expired: true })).toBeNull();
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringMatching(/missing or non-numeric expires_at/),
      expect.any(Object),
    );
  });

  test('rejects non-boolean include_expired', async () => {
    await expect(flowState.loadFlow('id', { include_expired: 'yes' }))
      .rejects.toThrow(/include_expired must be a boolean/);
    await expect(flowState.loadFlow('id', { include_expired: 1 }))
      .rejects.toThrow(/include_expired must be a boolean/);
  });
});

describe('flow-state.transitionFlow', () => {
  test('happy path: emits FLOW_TRANSITION with terminal=true and bumps version', async () => {
    ddbMock.on(GetCommand).resolves({ Item: { stage: 'stage_a' } });
    ddbMock.on(UpdateCommand).resolves({ Attributes: { version: 5 } });

    const res = await flowState.transitionFlow('id', 4, {
      stage_to: 'stage_b',
      payload: { next: 1 },
      terminal: true,
    });

    expect(res).toEqual({ result: 'success', version: 5 });

    const updCall = ddbMock.commandCalls(UpdateCommand)[0];
    expect(updCall.args[0].input.TableName).toBe(EXPECTED_TABLE);
    expect(updCall.args[0].input.ConditionExpression).toBe('attribute_exists(flow_id) AND #v = :expected AND #e >= :now');
    expect(typeof updCall.args[0].input.ExpressionAttributeValues[':now']).toBe('number');
    expect(updCall.args[0].input.ExpressionAttributeValues[':now'])
      .toBe(updCall.args[0].input.ExpressionAttributeValues[':updated_at']);
    expect(updCall.args[0].input.ExpressionAttributeValues[':expected']).toBe(4);
    expect(updCall.args[0].input.ExpressionAttributeValues[':stage_to']).toBe('stage_b');
    expect(updCall.args[0].input.ExpressionAttributeValues[':payload'].startsWith('enc:v1:')).toBe(true);

    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_TRANSITION, {
      flow_id: 'id',
      stage_from: 'stage_a',
      stage_to: 'stage_b',
      result: 'success',
      terminal: true,
      extended: false,
      version: 5,
    });
  });

  test('success with terminal: false stays false (negative-control vs forced-false)', async () => {
    ddbMock.on(GetCommand).resolves({ Item: { stage: 'a' } });
    ddbMock.on(UpdateCommand).resolves({ Attributes: { version: 3 } });

    await flowState.transitionFlow('id', 2, {
      stage_to: 'b',
      terminal: false,
    });

    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_TRANSITION, {
      flow_id: 'id',
      stage_from: 'a',
      stage_to: 'b',
      result: 'success',
      terminal: false,
      extended: false,
      version: 3,
    });
  });

  test('payload: null clears existing payload (asymmetric with createFlow)', async () => {
    ddbMock.on(GetCommand).resolves({ Item: { stage: 'a' } });
    ddbMock.on(UpdateCommand).resolves({ Attributes: { version: 2 } });

    await flowState.transitionFlow('id', 1, {
      stage_to: 'b',
      payload: null,
      terminal: false,
    });

    const upd = ddbMock.commandCalls(UpdateCommand)[0];
    expect(upd.args[0].input.UpdateExpression).toMatch(/#p = :payload/);
    expect(upd.args[0].input.ExpressionAttributeValues[':payload']).toBeNull();
  });

  test('created_at is never written by transitionFlow (preserves row birth time)', async () => {
    ddbMock.on(GetCommand).resolves({ Item: { stage: 'a' } });
    ddbMock.on(UpdateCommand).resolves({ Attributes: { version: 2 } });

    await flowState.transitionFlow('id', 1, {
      stage_to: 'b',
      terminal: false,
    });

    const upd = ddbMock.commandCalls(UpdateCommand)[0];
    expect(upd.args[0].input.UpdateExpression).not.toMatch(/created_at/);
    expect(upd.args[0].input.ExpressionAttributeNames).not.toMatchObject({ '#c': 'created_at' });
  });

  test('skips payload encrypt when payload is omitted', async () => {
    ddbMock.on(GetCommand).resolves({ Item: { stage: 'a' } });
    ddbMock.on(UpdateCommand).resolves({ Attributes: { version: 2 } });

    await flowState.transitionFlow('id', 1, {
      stage_to: 'b',
      terminal: false,
    });

    const upd = ddbMock.commandCalls(UpdateCommand)[0];
    expect(upd.args[0].input.ExpressionAttributeValues[':payload']).toBeUndefined();
    expect(upd.args[0].input.UpdateExpression).not.toMatch(/payload/);
  });

  test('set_expires_at writes new expiry; extended=true when new > prior', async () => {
    const priorExpires = futureExpiry(600);
    const newExpiry = futureExpiry(1800);
    ddbMock.on(GetCommand).resolves({ Item: { stage: 'a', expires_at: priorExpires } });
    ddbMock.on(UpdateCommand).resolves({ Attributes: { version: 2 } });

    await flowState.transitionFlow('id', 1, {
      stage_to: 'b',
      terminal: false,
      set_expires_at: newExpiry,
    });

    const upd = ddbMock.commandCalls(UpdateCommand)[0];
    expect(upd.args[0].input.ExpressionAttributeValues[':expires_at']).toBe(newExpiry);
    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_TRANSITION, {
      flow_id: 'id',
      stage_from: 'a',
      stage_to: 'b',
      result: 'success',
      terminal: false,
      extended: true,
      version: 2,
    });
  });

  test('set_expires_at that SHORTENS the lifetime emits extended=false (honest forensics)', async () => {
    const priorExpires = futureExpiry(3600);
    const shorterExpiry = futureExpiry(600);
    ddbMock.on(GetCommand).resolves({ Item: { stage: 'a', expires_at: priorExpires } });
    ddbMock.on(UpdateCommand).resolves({ Attributes: { version: 2 } });

    await flowState.transitionFlow('id', 1, {
      stage_to: 'b',
      terminal: false,
      set_expires_at: shorterExpiry,
    });

    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_TRANSITION, {
      flow_id: 'id',
      stage_from: 'a',
      stage_to: 'b',
      result: 'success',
      terminal: false,
      extended: false,
      version: 2,
    });
  });

  test('set_expires_at equal to prior emits extended=false (strict > semantics)', async () => {
    const sameExpiry = futureExpiry(600);
    ddbMock.on(GetCommand).resolves({ Item: { stage: 'a', expires_at: sameExpiry } });
    ddbMock.on(UpdateCommand).resolves({ Attributes: { version: 2 } });

    await flowState.transitionFlow('id', 1, {
      stage_to: 'b',
      terminal: false,
      set_expires_at: sameExpiry,
    });

    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_TRANSITION,
      expect.objectContaining({ extended: false }),
    );
  });

  test('set_expires_at on a row with missing prior expires_at emits extended=false (no honest baseline)', async () => {
    ddbMock.on(GetCommand).resolves({ Item: { stage: 'a' } });
    ddbMock.on(UpdateCommand).resolves({ Attributes: { version: 2 } });

    await flowState.transitionFlow('id', 1, {
      stage_to: 'b',
      terminal: false,
      set_expires_at: futureExpiry(600),
    });

    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_TRANSITION,
      expect.objectContaining({ extended: false }),
    );
  });

  test('set_expires_at rejects non-integer', async () => {
    await expect(flowState.transitionFlow('id', 1, {
      stage_to: 'b',
      terminal: false,
      set_expires_at: futureExpiry() + 0.5,
    })).rejects.toThrow(/expires_at must be a finite integer/);
  });

  test('set_expires_at rejects values in the past (writer guard)', async () => {
    await expect(flowState.transitionFlow('id', 1, {
      stage_to: 'b',
      terminal: false,
      set_expires_at: Math.floor(Date.now() / 1000) - 100,
    })).rejects.toThrow(/must be strictly in the future/);
  });

  test('returns not_found when pre-read finds no row (forces terminal=false in audit)', async () => {
    ddbMock.on(GetCommand).resolves({});

    const res = await flowState.transitionFlow('id', 1, {
      stage_to: 's',
      terminal: true,
    });
    expect(res).toEqual({ result: 'not_found', version: null });
    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_TRANSITION, {
      flow_id: 'id',
      stage_from: null,
      stage_to: 's',
      result: 'not_found',
      terminal: false,
      extended: false,
      version: 1,
    });
    expect(ddbMock.commandCalls(UpdateCommand)).toHaveLength(0);
  });

  test('returns conflict when Update fails OCC and row still exists (forces terminal=false)', async () => {
    ddbMock.on(GetCommand).resolves({
      Item: { stage: 'a', flow_id: 'id', expires_at: futureExpiry() },
    });
    ddbMock.on(UpdateCommand).rejects(ccfe());

    const res = await flowState.transitionFlow('id', 1, {
      stage_to: 'b',
      terminal: true,
    });
    expect(res).toEqual({ result: 'conflict', version: null });
    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_TRANSITION, {
      flow_id: 'id',
      stage_from: 'a',
      stage_to: 'b',
      result: 'conflict',
      terminal: false,
      extended: false,
      version: 1,
    });
  });

  test('returns not_found when Update fails OCC and row disappeared (TTL race)', async () => {
    let getCalls = 0;
    ddbMock.on(GetCommand).callsFake(() => {
      getCalls += 1;
      if (getCalls === 1) return { Item: { stage: 'a' } };
      return {};
    });
    ddbMock.on(UpdateCommand).rejects(ccfe());

    const res = await flowState.transitionFlow('id', 1, {
      stage_to: 'b',
      terminal: false,
    });
    expect(res).toEqual({ result: 'not_found', version: null });
    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_TRANSITION, {
      flow_id: 'id',
      stage_from: 'a',
      stage_to: 'b',
      result: 'not_found',
      terminal: false,
      extended: false,
      version: 1,
    });
  });

  test('post-CCFE recheck reports not_found when row is present but logically expired', async () => {
    const pastExpiry = Math.floor(Date.now() / 1000) - 30;
    let getCalls = 0;
    ddbMock.on(GetCommand).callsFake(() => {
      getCalls += 1;
      if (getCalls === 1) return { Item: { stage: 'a' } };
      return { Item: { flow_id: 'id', expires_at: pastExpiry } };
    });
    ddbMock.on(UpdateCommand).rejects(ccfe());

    const res = await flowState.transitionFlow('id', 1, {
      stage_to: 'b',
      terminal: false,
    });
    expect(res).toEqual({ result: 'not_found', version: null });
    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_TRANSITION, {
      flow_id: 'id',
      stage_from: 'a',
      stage_to: 'b',
      result: 'not_found',
      terminal: false,
      extended: false,
      version: 1,
    });
  });

  test('post-CCFE recheck warns when row has corrupted expires_at and reports not_found', async () => {
    let getCalls = 0;
    ddbMock.on(GetCommand).callsFake(() => {
      getCalls += 1;
      if (getCalls === 1) return { Item: { stage: 'a' } };
      return { Item: { flow_id: 'id', expires_at: 'not-a-number' } };
    });
    ddbMock.on(UpdateCommand).rejects(ccfe());

    const res = await flowState.transitionFlow('id', 1, {
      stage_to: 'b',
      terminal: false,
    });
    expect(res).toEqual({ result: 'not_found', version: null });
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringMatching(/row has missing or non-numeric expires_at on recheck/),
      expect.objectContaining({ flow_id: 'id' }),
    );
  });

  test('post-CCFE recheck failure warns and conservatively reports conflict', async () => {
    let getCalls = 0;
    ddbMock.on(GetCommand).callsFake(() => {
      getCalls += 1;
      if (getCalls === 1) return { Item: { stage: 'a' } };
      throw new Error('NetworkingError');
    });
    ddbMock.on(UpdateCommand).rejects(ccfe());

    const res = await flowState.transitionFlow('id', 1, {
      stage_to: 'b',
      terminal: false,
    });
    expect(res).toEqual({ result: 'conflict', version: null });
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringMatching(/post-CCFE recheck failed/),
      expect.objectContaining({ flow_id: 'id' }),
    );
  });

  test('result=error emits and rethrows on non-conditional Update failure (forces terminal=false)', async () => {
    ddbMock.on(GetCommand).resolves({ Item: { stage: 'a' } });
    ddbMock.on(UpdateCommand).rejects(new Error('ProvisionedThroughputExceeded'));

    await expect(flowState.transitionFlow('id', 1, {
      stage_to: 'b',
      terminal: true,  // caller's terminal claim must be overridden
    })).rejects.toThrow('ProvisionedThroughputExceeded');

    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_TRANSITION, {
      flow_id: 'id',
      stage_from: 'a',
      stage_to: 'b',
      result: 'error',
      terminal: false,
      extended: false,
      version: 1,
    });
  });

  test('result=error emits and rethrows when pre-read fails', async () => {
    ddbMock.on(GetCommand).rejects(new Error('NetworkingError'));

    await expect(flowState.transitionFlow('id', 1, {
      stage_to: 'b',
      terminal: false,
    })).rejects.toThrow('NetworkingError');

    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_TRANSITION, {
      flow_id: 'id',
      stage_from: null,
      stage_to: 'b',
      result: 'error',
      terminal: false,
      extended: false,
      version: 1,
    });
  });

  test('rejects non-positive-integer expectedVersion', async () => {
    await expect(flowState.transitionFlow('id', 0, { stage_to: 's', terminal: false }))
      .rejects.toThrow(/expectedVersion must be a positive integer/);
    await expect(flowState.transitionFlow('id', 1.5, { stage_to: 's', terminal: false }))
      .rejects.toThrow(/expectedVersion must be a positive integer/);
    await expect(flowState.transitionFlow('id', -1, { stage_to: 's', terminal: false }))
      .rejects.toThrow(/expectedVersion must be a positive integer/);
  });

  test('rejects non-boolean terminal', async () => {
    await expect(flowState.transitionFlow('id', 1, { stage_to: 's', terminal: 'yes' }))
      .rejects.toThrow(/terminal must be a boolean/);
    await expect(flowState.transitionFlow('id', 1, { stage_to: 's', terminal: 1 }))
      .rejects.toThrow(/terminal must be a boolean/);
  });

  test('rejects empty stage_to', async () => {
    await expect(flowState.transitionFlow('id', 1, { stage_to: '', terminal: false }))
      .rejects.toThrow(/stage_to must be a non-empty string/);
  });
});

describe('flow-state.deleteFlow', () => {
  test('issues a stage-gated DeleteCommand, emits FLOW_DELETED, returns deleted:true', async () => {
    ddbMock.on(DeleteCommand).resolves({});

    const res = await flowState.deleteFlow('id', { stage: 'completed', reason: 'terminal' });

    expect(res).toEqual({ deleted: true });
    const call = ddbMock.commandCalls(DeleteCommand)[0];
    expect(call.args[0].input.TableName).toBe(EXPECTED_TABLE);
    expect(call.args[0].input.Key).toEqual({ flow_id: 'id' });
    expect(call.args[0].input.ConditionExpression)
      .toBe('attribute_exists(flow_id) AND #s = :stage');
    expect(call.args[0].input.ExpressionAttributeNames).toEqual({ '#s': 'stage' });
    expect(call.args[0].input.ExpressionAttributeValues).toEqual({ ':stage': 'completed' });

    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_DELETED, {
      flow_id: 'id',
      stage: 'completed',
      reason: 'terminal',
    });
  });

  test('returns deleted:false and does NOT emit when row was already absent (redelivery / TTL reap)', async () => {
    ddbMock.on(DeleteCommand).rejects(ccfe());
    ddbMock.on(GetCommand).resolves({ Item: undefined });

    const res = await flowState.deleteFlow('id', { stage: 's', reason: 'abort' });
    expect(res).toEqual({ deleted: false });
    expect(logger.audit).not.toHaveBeenCalled();
  });

  test('returns deleted:false when stage mismatch (sibling flow at same flow_id)', async () => {
    ddbMock.on(DeleteCommand).rejects(ccfe());
    ddbMock.on(GetCommand).resolves({
      Item: {
        flow_id: 'id',
        stage: 'awaiting_setup_modal',
        version: 1,
        expires_at: Math.floor(Date.now() / 1000) + 600,
      },
    });

    const res = await flowState.deleteFlow('id', {
      stage: 'awaiting_revoke_select',
      reason: 'admin_cleanup',
    });
    expect(res).toEqual({ deleted: false });
    expect(logger.audit).not.toHaveBeenCalled();
  });

  test('rethrows unexpected DDB errors', async () => {
    ddbMock.on(DeleteCommand).rejects(new Error('AccessDenied'));
    await expect(flowState.deleteFlow('id', { stage: 's', reason: 'terminal' }))
      .rejects.toThrow('AccessDenied');
  });

  test.each(['terminal', 'abort', 'admin_cleanup'])('accepts reason=%s', async (reason) => {
    ddbMock.on(DeleteCommand).resolves({});
    await flowState.deleteFlow('id', { stage: 's', reason });
    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_DELETED, {
      flow_id: 'id',
      stage: 's',
      reason,
    });
  });

  test('rejects invalid reason', async () => {
    await expect(flowState.deleteFlow('id', { stage: 's', reason: 'bogus' }))
      .rejects.toThrow(/reason must be one of/);
  });

  test('rejects empty flow_id and empty stage', async () => {
    await expect(flowState.deleteFlow('', { stage: 's', reason: 'terminal' }))
      .rejects.toThrow(/flow_id must be a non-empty string/);
    await expect(flowState.deleteFlow('id', { stage: '', reason: 'terminal' }))
      .rejects.toThrow(/stage must be a non-empty string/);
  });

  test('with expectedVersion: condition includes version gate', async () => {
    ddbMock.on(DeleteCommand).resolves({});

    const res = await flowState.deleteFlow('id', {
      stage: 'awaiting_setup_button',
      reason: 'admin_cleanup',
      expectedVersion: 4,
    });
    expect(res).toEqual({ deleted: true });

    const call = ddbMock.commandCalls(DeleteCommand)[0];
    expect(call.args[0].input.ConditionExpression)
      .toBe('attribute_exists(flow_id) AND #s = :stage AND #v = :expected');
    expect(call.args[0].input.ExpressionAttributeNames).toEqual({
      '#s': 'stage', '#v': 'version',
    });
    expect(call.args[0].input.ExpressionAttributeValues).toEqual({
      ':stage': 'awaiting_setup_button', ':expected': 4,
    });
  });

  test('with expectedVersion: returns deleted:false when version mismatch (concurrent advance)', async () => {
    ddbMock.on(DeleteCommand).rejects(ccfe());
    ddbMock.on(GetCommand).resolves({
      Item: { flow_id: 'id', stage: 'awaiting_setup_button', version: 5 },
    });

    const res = await flowState.deleteFlow('id', {
      stage: 'awaiting_setup_button',
      reason: 'admin_cleanup',
      expectedVersion: 4,
    });
    expect(res).toEqual({ deleted: false });
    expect(logger.audit).not.toHaveBeenCalled();
  });

  test('rejects non-integer expectedVersion when provided', async () => {
    await expect(flowState.deleteFlow('id', {
      stage: 's', reason: 'terminal', expectedVersion: 1.5,
    })).rejects.toThrow(/expectedVersion must be a positive integer/);
    await expect(flowState.deleteFlow('id', {
      stage: 's', reason: 'terminal', expectedVersion: 0,
    })).rejects.toThrow(/expectedVersion must be a positive integer/);
    await expect(flowState.deleteFlow('id', {
      stage: 's', reason: 'terminal', expectedVersion: '1',
    })).rejects.toThrow(/expectedVersion must be a positive integer/);
  });

  test('omitting expectedVersion preserves the pre-existing stage-gate-only contract', async () => {
    ddbMock.on(DeleteCommand).resolves({});
    await flowState.deleteFlow('id', { stage: 's', reason: 'terminal' });

    const call = ddbMock.commandCalls(DeleteCommand)[0];
    expect(call.args[0].input.ConditionExpression)
      .toBe('attribute_exists(flow_id) AND #s = :stage');
    expect(call.args[0].input.ExpressionAttributeNames).toEqual({ '#s': 'stage' });
    expect(call.args[0].input.ExpressionAttributeValues).toEqual({ ':stage': 's' });
  });
});

describe('flow-state — payload corruption resilience', () => {
  test('loadFlow propagates decrypt-side throws (fail-loud on KEK misconfig)', async () => {
    ddbMock.on(GetCommand).resolves({
      Item: {
        flow_id: 'id',
        stage: 's',
        version: 1,
        payload: 'enc:v1:IV:TAG:cafef00d',
        expires_at: Math.floor(Date.now() / 1000) + 600,
      },
    });
    decrypt.mockImplementationOnce(() => {
      throw new Error('KEY_ENCRYPTION_KEY is required to decrypt');
    });

    await expect(flowState.loadFlow('id')).rejects.toThrow(/KEY_ENCRYPTION_KEY is required/);
  });

  test('loadFlow returns payload=null and logs error when payload JSON is corrupt', async () => {
    ddbMock.on(GetCommand).resolves({
      Item: {
        flow_id: 'id',
        stage: 's',
        version: 1,
        payload: `enc:v1:IV:TAG:${Buffer.from('not-valid-json{').toString('hex')}`,
        expires_at: Math.floor(Date.now() / 1000) + 600,
      },
    });

    const res = await flowState.loadFlow('id');
    expect(res).not.toBeNull();
    expect(res.payload).toBeNull();
    expect(logger.error).toHaveBeenCalledWith(
      expect.stringMatching(/payload JSON\.parse failed/),
      expect.any(Object),
    );
  });
});

describe('flow-state.supersedeOrCreate', () => {

  test('first-try create succeeds when no row exists at flow_id', async () => {
    ddbMock.on(PutCommand).resolves({});

    const res = await flowState.supersedeOrCreate({
      flow_id: FLOW_ID,
      stage: 'awaiting_setup_button',
      payload: null,
      ttl_seconds: 120,
    });

    expect(res).toEqual({ created: true, version: 1 });
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1);
    expect(ddbMock.commandCalls(GetCommand)).toHaveLength(0);
    expect(ddbMock.commandCalls(DeleteCommand)).toHaveLength(0);
    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_CREATED, expect.objectContaining({
      flow_id: FLOW_ID, stage: 'awaiting_setup_button',
    }));
  });

  test('claims a same-stage predecessor via version-gated delete + retry create', async () => {
    const priorRow = {
      flow_id: FLOW_ID,
      stage: 'awaiting_setup_button',
      version: 3,
      payload: null,
      expires_at: futureExpiry(),
    };
    ddbMock.on(PutCommand)
      .rejectsOnce(ccfe())
      .resolves({});
    ddbMock.on(GetCommand).resolves({ Item: priorRow });
    ddbMock.on(DeleteCommand).resolves({});

    const res = await flowState.supersedeOrCreate({
      flow_id: FLOW_ID,
      stage: 'awaiting_setup_button',
      payload: null,
      ttl_seconds: 120,
    });

    expect(res).toEqual({ created: true, version: 1 });

    const delCall = ddbMock.commandCalls(DeleteCommand)[0];
    expect(delCall.args[0].input.ConditionExpression)
      .toBe('attribute_exists(flow_id) AND #s = :stage AND #v = :expected');
    expect(delCall.args[0].input.ExpressionAttributeValues[':expected']).toBe(3);
    expect(delCall.args[0].input.ExpressionAttributeValues[':stage']).toBe('awaiting_setup_button');
  });

  test('returns surviving (no claim) when predecessor is at a DIFFERENT stage (sibling flow)', async () => {
    const siblingRow = {
      flow_id: FLOW_ID,
      stage: 'awaiting_revoke_select',
      version: 1,
      payload: null,
      expires_at: futureExpiry(),
    };
    ddbMock.on(PutCommand).rejects(ccfe());
    ddbMock.on(GetCommand).resolves({ Item: siblingRow });

    const res = await flowState.supersedeOrCreate({
      flow_id: FLOW_ID,
      stage: 'awaiting_setup_button',
      payload: null,
      ttl_seconds: 120,
    });

    expect(res.created).toBe(false);
    expect(res.surviving.stage).toBe('awaiting_revoke_select');
    expect(res.surviving.version).toBe(1);
    expect(ddbMock.commandCalls(DeleteCommand)).toHaveLength(0);
  });

  test('uses include_expired:true on the peek so stuck-orphans (logically expired, not reaped) are observable', async () => {
    const pastExpiry = Math.floor(Date.now() / 1000) - 120;
    const orphanRow = {
      flow_id: FLOW_ID,
      stage: 'awaiting_setup_modal',
      version: 2,
      payload: null,
      expires_at: pastExpiry,
    };
    ddbMock.on(PutCommand).rejects(ccfe());
    ddbMock.on(GetCommand).resolves({ Item: orphanRow });

    const res = await flowState.supersedeOrCreate({
      flow_id: FLOW_ID,
      stage: 'awaiting_setup_button',
      payload: null,
      ttl_seconds: 120,
    });

    expect(res.created).toBe(false);
    expect(res.surviving).not.toBeNull();
    expect(res.surviving.stage).toBe('awaiting_setup_modal');
    expect(res.surviving.expires_at).toBe(pastExpiry);
  });

  test('returns surviving (no claim) when predecessor advances between peek and delete (OCC race)', async () => {
    const observedRow = {
      flow_id: FLOW_ID,
      stage: 'awaiting_setup_button',
      version: 3,
      payload: null,
      expires_at: futureExpiry(),
    };
    const advancedRow = {
      ...observedRow,
      stage: 'awaiting_setup_modal',
      version: 4,
    };
    ddbMock.on(PutCommand).rejects(ccfe());
    ddbMock.on(GetCommand)
      .resolvesOnce({ Item: observedRow })
      .resolvesOnce({ Item: { stage: 'awaiting_setup_modal', version: 4 } })
      .resolves({ Item: advancedRow });
    ddbMock.on(DeleteCommand).rejects(ccfe());

    const res = await flowState.supersedeOrCreate({
      flow_id: FLOW_ID,
      stage: 'awaiting_setup_button',
      payload: null,
      ttl_seconds: 120,
    });

    expect(res.created).toBe(false);
    expect(res.surviving.version).toBe(4);
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(1);
  });

  test('retries create when the row vanishes between conflict and peek (TTL reap or peer cleanup)', async () => {
    ddbMock.on(PutCommand)
      .rejectsOnce(ccfe())
      .resolves({});
    ddbMock.on(GetCommand).resolves({ Item: undefined });

    const res = await flowState.supersedeOrCreate({
      flow_id: FLOW_ID,
      stage: 'awaiting_setup_button',
      payload: null,
      ttl_seconds: 120,
    });

    expect(res).toEqual({ created: true, version: 1 });
    expect(ddbMock.commandCalls(PutCommand)).toHaveLength(2);
    expect(ddbMock.commandCalls(DeleteCommand)).toHaveLength(0);
  });

  test('surfaces surviving:null when both attempts collide and the peek then sees nothing', async () => {
    ddbMock.on(PutCommand)
      .rejectsOnce(ccfe()) // first create
      .rejectsOnce(ccfe()); // retry create after vanished-peek
    ddbMock.on(GetCommand).resolves({ Item: undefined });

    const res = await flowState.supersedeOrCreate({
      flow_id: FLOW_ID,
      stage: 'awaiting_setup_button',
      payload: null,
      ttl_seconds: 120,
    });

    expect(res).toEqual({ created: false, surviving: null });
  });

  test('recomputes expires_at on the retry create (full TTL budget after deleteFlow RTT)', async () => {
    const priorRow = {
      flow_id: FLOW_ID,
      stage: 'awaiting_setup_button',
      version: 1,
      payload: null,
      expires_at: futureExpiry(),
    };
    ddbMock.on(PutCommand)
      .rejectsOnce(ccfe())
      .resolves({});
    ddbMock.on(GetCommand).resolves({ Item: priorRow });
    ddbMock.on(DeleteCommand).resolves({});

    const beforeSec = Math.floor(Date.now() / 1000);
    await flowState.supersedeOrCreate({
      flow_id: FLOW_ID,
      stage: 'awaiting_setup_button',
      payload: null,
      ttl_seconds: 120,
    });
    const afterSec = Math.floor(Date.now() / 1000);

    const putCalls = ddbMock.commandCalls(PutCommand);
    const retryPut = putCalls[1].args[0].input.Item;
    expect(retryPut.expires_at).toBeGreaterThanOrEqual(beforeSec + 120);
    expect(retryPut.expires_at).toBeLessThanOrEqual(afterSec + 120);
  });

  test('rejects invalid arguments at entry', async () => {
    await expect(flowState.supersedeOrCreate({
      flow_id: '', stage: 's', payload: null, ttl_seconds: 60,
    })).rejects.toThrow(/flow_id must be a non-empty string/);
    await expect(flowState.supersedeOrCreate({
      flow_id: FLOW_ID, stage: '', payload: null, ttl_seconds: 60,
    })).rejects.toThrow(/stage must be a non-empty string/);
    await expect(flowState.supersedeOrCreate({
      flow_id: FLOW_ID, stage: 's', payload: null, ttl_seconds: 0,
    })).rejects.toThrow(/ttl_seconds must be a positive integer/);
    await expect(flowState.supersedeOrCreate({
      flow_id: FLOW_ID, stage: 's', payload: null, ttl_seconds: 60.5,
    })).rejects.toThrow(/ttl_seconds must be a positive integer/);
  });
});
