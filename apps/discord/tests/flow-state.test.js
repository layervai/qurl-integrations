/**
 * Unit tests for src/flow-state.js — the DDB-backed state-machine
 * harness.
 *
 * Uses `aws-sdk-client-mock` (same pattern as ddb-store.test.js) to
 * intercept DocumentClient commands without hitting AWS. Covers:
 *   - createFlow happy path + idempotent re-create (OCC conflict)
 *   - loadFlow happy path + missing row + logically expired row
 *   - transitionFlow happy path + OCC conflict + not_found + error
 *   - deleteFlow happy path
 *   - TTL-type guards (expires_at must be a finite integer)
 *   - Payload encryption is mandatory and roundtrips
 *   - Audit events emit with the right shape (FLOW_CREATED,
 *     FLOW_TRANSITION with terminal, FLOW_DELETED with reason)
 */

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

// Crypto mock: pass-through encryption that wraps + unwraps without
// touching the real KEK. The crypto module itself is tested
// elsewhere (crypto.test.js); here we just need to verify the
// harness routes payload through encryptStrict / decrypt.
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
// Canonical test fixtures. `FLOW_ID` is a real parseable shard-aware
// composite key (matches what `buildFlowId({...})` would emit) so it
// passes createFlow's entry-point parseFlowId validation. A future-
// dated expiry is computed at-test-time so it stays strictly in the
// future across `assertExpiresAt`'s `> nowEpochSeconds()` guard.
const FLOW_ID = '0:1#g#c#u';
function futureExpiry(offset_seconds = 600) {
  return Math.floor(Date.now() / 1000) + offset_seconds;
}

// Build a synthetic ConditionalCheckFailedException — the v3 SDK's
// real exception class has a constructor signature that's awkward
// to invoke from a test. Matching by `.name` is what the harness
// does in practice.
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
  test('exposes the four lifecycle methods', () => {
    expect(typeof flowState.createFlow).toBe('function');
    expect(typeof flowState.loadFlow).toBe('function');
    expect(typeof flowState.transitionFlow).toBe('function');
    expect(typeof flowState.deleteFlow).toBe('function');
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
    // Payload was encrypted (mock wraps in enc:v1:...)
    expect(typeof item.payload).toBe('string');
    expect(item.payload.startsWith('enc:v1:')).toBe(true);
    // The encrypted blob is the JSON-serialized payload.
    expect(decrypt(item.payload)).toBe('{"foo":"bar"}');

    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_CREATED, {
      flow_id: FLOW_ID,
      stage: 'awaiting_button',
    });
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
    // The legitimate-redelivery rate would be noise at warn level,
    // but a debug breadcrumb gives an operator something to grep
    // for when investigating "why is the user seeing stale data".
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
    // A row whose TTL is already-past at create time is born expired:
    // FLOW_CREATED fires, but loadFlow returns null forever after,
    // and the SLI's silently_dropped numerator inflates.
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
    // Entry-point validation closes the silent-drop foot-gun where a
    // handler builds a malformed key and flow-state accepts it,
    // leaving forensic queries broken downstream.
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
    const futureExpiry = Math.floor(Date.now() / 1000) + 600;
    ddbMock.on(GetCommand).resolves({
      Item: {
        flow_id: 'id',
        stage: 's',
        version: 3,
        payload: `enc:v1:IV:TAG:${Buffer.from('{"k":"v"}').toString('hex')}`,
        expires_at: futureExpiry,
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
      expires_at: futureExpiry,
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
    // Default grace=0 → null
    expect(await flowState.loadFlow('id')).toBeNull();
    // grace=60 → still alive
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
        // expires_at intentionally absent
      },
    });
    expect(await flowState.loadFlow('id')).toBeNull();
    expect(logger.warn).toHaveBeenCalledWith(
      expect.stringMatching(/missing or non-numeric expires_at/),
      expect.any(Object),
    );
  });

  test('returns null and warns when expires_at is a non-integer float (writer/reader symmetry)', async () => {
    // Writer's assertExpiresAt rejects floats (`Math.floor(x) !== x`);
    // reader must match — a regression writer that puts a float
    // should not round-trip undetected.
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
      .rejects.toThrow(/grace_seconds must be a finite number/);
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
    expect(updCall.args[0].input.ConditionExpression).toBe('attribute_exists(flow_id) AND #v = :expected');
    expect(updCall.args[0].input.ExpressionAttributeValues[':expected']).toBe(4);
    expect(updCall.args[0].input.ExpressionAttributeValues[':stage_to']).toBe('stage_b');
    // Payload was encrypted via encryptStrict
    expect(updCall.args[0].input.ExpressionAttributeValues[':payload'].startsWith('enc:v1:')).toBe(true);

    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_TRANSITION, {
      flow_id: 'id',
      stage_from: 'stage_a',
      stage_to: 'stage_b',
      result: 'success',
      terminal: true,
      extended: false,
    });
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

  test('set_expires_at writes the new expiry, is type-checked, and emits extended=true', async () => {
    ddbMock.on(GetCommand).resolves({ Item: { stage: 'a' } });
    ddbMock.on(UpdateCommand).resolves({ Attributes: { version: 2 } });
    const newExpiry = futureExpiry(1200);

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
    });
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

    // Caller passes terminal: true but the transition didn't actually
    // advance the row — the audit MUST emit terminal=false so a
    // forensic `count_by(terminal=true)` doesn't over-count.
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
    });
    expect(ddbMock.commandCalls(UpdateCommand)).toHaveLength(0);
  });

  test('returns conflict when Update fails OCC and row still exists (forces terminal=false)', async () => {
    // Same terminal=true override semantics as the not_found case
    // above: a transition that didn't advance isn't terminal.
    ddbMock.on(GetCommand).resolves({ Item: { stage: 'a', flow_id: 'id' } });
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
    });
  });

  test('returns not_found when Update fails OCC and row disappeared (TTL race)', async () => {
    // First GetCommand: pre-read sees the row.
    // Update fails (row reaped between pre-read and Update).
    // Second GetCommand (recheck): row gone.
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
    });
  });

  test('post-CCFE recheck failure warns and conservatively reports conflict', async () => {
    // Pre-read sees the row, Update fails OCC, recheck-Get itself
    // throws (DDB availability blip). The harness must NOT silently
    // swallow the recheck error — a warn keeps the signal visible
    // in CloudWatch while still defaulting to the conservative
    // result=conflict bucket.
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
  test('issues a conditional DeleteCommand, emits FLOW_DELETED, returns deleted:true', async () => {
    ddbMock.on(DeleteCommand).resolves({});

    const res = await flowState.deleteFlow('id', { stage: 'completed', reason: 'terminal' });

    expect(res).toEqual({ deleted: true });
    const call = ddbMock.commandCalls(DeleteCommand)[0];
    expect(call.args[0].input.TableName).toBe(EXPECTED_TABLE);
    expect(call.args[0].input.Key).toEqual({ flow_id: 'id' });
    expect(call.args[0].input.ConditionExpression).toBe('attribute_exists(flow_id)');

    expect(logger.audit).toHaveBeenCalledWith(AUDIT_EVENTS.FLOW_DELETED, {
      flow_id: 'id',
      stage: 'completed',
      reason: 'terminal',
    });
  });

  test('returns deleted:false and does NOT emit when row was already absent (redelivery / TTL reap)', async () => {
    // The SLI math requires at-most-once FLOW_DELETED per logical flow.
    // A second delete on an already-gone row must not emit again.
    ddbMock.on(DeleteCommand).rejects(ccfe());

    const res = await flowState.deleteFlow('id', { stage: 's', reason: 'abort' });
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
});

describe('flow-state — payload corruption resilience', () => {
  test('loadFlow propagates decrypt-side throws (fail-loud on KEK misconfig)', async () => {
    // A KEK-unset / KEK-rotated misconfig must fail loudly rather
    // than silently degrade to payload=null — otherwise a config
    // bug looks like "no payload" and the caller proceeds with
    // wrong-shaped data. Symmetric to the harness's broader
    // fail-closed posture on encryptStrict.
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
    // Decrypt yields a non-JSON string → JSON.parse throws → row
    // surfaces with payload=null rather than blowing up the caller.
    ddbMock.on(GetCommand).resolves({
      Item: {
        flow_id: 'id',
        stage: 's',
        version: 1,
        // The mock decrypt strips the enc:v1: prefix and returns the
        // hex-decoded payload. Use a string that decodes to invalid JSON.
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
