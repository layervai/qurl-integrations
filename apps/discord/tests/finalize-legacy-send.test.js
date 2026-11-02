const { spawnSync } = require('node:child_process');
const path = require('node:path');
const { finalizeLegacySend } = require('../scripts/finalize-legacy-send');

const senderId = '100000000000000001';
const row = { recipient_discord_id: '100000000000000002', sender_discord_id: senderId, resource_id: 'res_legacy' };
const target = { prefix: 'operator-test-', sendId: 'send-legacy', senderId };

function fixture({ config, rows = [row], afterBarrier = rows, pages } = {}) {
  const state = { config, barrier: false };
  const client = { send: jest.fn(async command => {
    const input = command.input;
    switch (command.constructor.name) {
      case 'GetCommand': return { Item: state.config };
      case 'QueryCommand': return pages ? pages(input) : { Items: state.barrier ? afterBarrier : rows };
      case 'PutCommand': state.config = { ...input.Item }; state.barrier = true; return {};
      case 'UpdateCommand':
        if (input.UpdateExpression.startsWith('SET revoking_at')) {
          state.config.revoking_at ||= input.ExpressionAttributeValues[':now'];
          state.barrier = true;
          return { Attributes: { revoking_at: state.config.revoking_at } };
        }
        state.config.revoked_at = input.ExpressionAttributeValues[':now'];
        state.config.legacy_close_reason = input.ExpressionAttributeValues[':reason'];
        return {};
      default: throw new Error('Unexpected request');
    }
  }) };
  return { client, state };
}

const writes = client => client.send.mock.calls.filter(([command]) => ['PutCommand', 'UpdateCommand'].includes(command.constructor.name));

it('defaults to a point-query dry-run with no upstream claim or participant output', async () => {
  const { client } = fixture();
  const result = await finalizeLegacySend({ client, ...target });
  expect(result).toEqual({ mode: 'dry-run', status: 'eligible_idless_rows', rows: 1, upstream_closure: 'not_checked' });
  expect(writes(client)).toEqual([]);
  const query = client.send.mock.calls.find(([command]) => command.constructor.name === 'QueryCommand')[0].input;
  expect(query).toMatchObject({ ConsistentRead: true, Limit: 100, KeyConditionExpression: 'send_id = :send' });
  expect(query).not.toHaveProperty('FilterExpression');
  expect(query.ProjectionExpression).not.toContain('qurl_link');
  expect(JSON.stringify(result)).not.toContain(senderId);
  expect(JSON.stringify(result)).not.toContain(target.sendId);
});

it.each([
  { apply: true }, { confirmed: true }, { apply: 'true', confirmed: true },
  { prefix: '' }, { prefix: 'missing-dash' }, { sendId: '' }, { senderId: '' },
])('rejects incomplete or invalid input before AWS work: %p', async override => {
  const { client } = fixture();
  await expect(finalizeLegacySend({ client, ...target, ...override })).rejects.toThrow();
  expect(client.send).not.toHaveBeenCalled();
});

it.each([
  [], [{ ...row, sender_discord_id: '100000000000000003' }],
  [{ ...row, recipient_discord_id: '' }], [{ ...row, resource_id: 'bad/path' }],
  [{ ...row, qurl_id: 'q_existing' }], [{ ...row, qurl_id: 'at_bearer' }],
].map(value => [value]))('refuses unknown, foreign or identified rows before a write: %p', async rows => {
  const { client } = fixture({ rows });
  await expect(finalizeLegacySend({ client, ...target, apply: true, confirmed: true })).rejects.toThrow();
  expect(writes(client)).toEqual([]);
});

it.each([undefined, { sender_discord_id: senderId }])('re-reads under an owned barrier before closing, with config %p', async config => {
  const { client, state } = fixture({ config });
  const result = await finalizeLegacySend({ client, ...target, apply: true, confirmed: true });
  expect(result).toMatchObject({ status: 'local_record_closed', rows: 1, upstream_closure: 'operator_attested' });
  expect(state.config.legacy_close_reason).toBe('operator_attested_upstream_closed');
  const calls = client.send.mock.calls.map(([command]) => command);
  expect(calls.filter(command => command.constructor.name === 'QueryCommand')).toHaveLength(2);
  expect(calls.at(-2).constructor.name).toBe('QueryCommand');
  expect(calls.at(-1).input).toMatchObject({
    ConditionExpression: 'sender_discord_id = :sender AND revoking_at = :barrier AND attribute_not_exists(revoked_at)',
    ExpressionAttributeValues: { ':sender': senderId, ':barrier': state.config.revoking_at },
  });
});

it.each([
  [{ ...row, resource_id: 'res_changed' }],
  [row, { ...row, recipient_discord_id: '100000000000000003', qurl_id: 'q_new' }],
].map(value => [value]))('keeps the intent and refuses finalization when rows change: %p', async afterBarrier => {
  const { client, state } = fixture({ afterBarrier });
  await expect(finalizeLegacySend({ client, ...target, apply: true, confirmed: true })).rejects.toThrow();
  expect(state.config.revoking_at).toBeTruthy();
  expect(state.config.revoked_at).toBeUndefined();
  expect(writes(client)).toHaveLength(1);
});

it('honors an existing barrier and treats closed local records as idempotent', async () => {
  const { client, state } = fixture({ config: { sender_discord_id: senderId, revoking_at: 'existing' } });
  await finalizeLegacySend({ client, ...target, apply: true, confirmed: true });
  expect(writes(client)).toHaveLength(1);
  client.send.mockClear();
  expect(await finalizeLegacySend({ client, ...target })).toMatchObject({ status: 'local_record_already_closed' });
  expect(state.config.revoked_at).toBeTruthy();
  expect(writes(client)).toEqual([]);
});

it('never treats a foreign closed config as permission', async () => {
  const { client } = fixture({ config: { sender_discord_id: '100000000000000003', revoked_at: 'closed' } });
  await expect(finalizeLegacySend({ client, ...target })).rejects.toThrow('owner does not match');
  expect(writes(client)).toEqual([]);
});

it('follows a bounded cursor and refuses an incomplete partition before writing', async () => {
  const { client } = fixture({ pages: () => ({ Items: [row], LastEvaluatedKey: { send_id: target.sendId, recipient_discord_id: row.recipient_discord_id } }) });
  await expect(finalizeLegacySend({ client, ...target, apply: true, confirmed: true })).rejects.toThrow('bounded page budget');
  expect(client.send).toHaveBeenCalledTimes(201);
  expect(writes(client)).toEqual([]);
});

it('shows help without AWS configuration and never echoes an unexpected bearer argument', () => {
  const script = path.join(__dirname, '../scripts/finalize-legacy-send.js');
  const env = { ...process.env, AWS_REGION: '', DDB_TABLE_PREFIX: '', AWS_EC2_METADATA_DISABLED: 'true' };
  const help = spawnSync(process.execPath, [script, '--help'], { env, encoding: 'utf8' });
  expect(help.status).toBe(0);
  expect(help.stdout).toContain('Default: dry-run.');
  const secret = 'at_synthetic_should_not_appear';
  const invalid = spawnSync(process.execPath, [script, secret], { env, encoding: 'utf8' });
  expect(invalid.status).toBe(1);
  expect(invalid.stderr).toContain('use --help');
  expect(invalid.stderr).not.toContain(secret);
});
