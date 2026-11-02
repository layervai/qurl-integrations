const { backfillGuildSenderIndex, main } = require('../scripts/backfill-guild-sender-index');
const { ScanCommand, UpdateCommand } = require('@aws-sdk/lib-dynamodb');

const row = {
  send_id: 'send', recipient_discord_id: 'recipient', guild_id: 'guild',
  sender_discord_id: 'sender', created_at: '2026-11-02T00:00:00.000Z',
};
const table = 'qurl-bot-discord-sandbox-qurl-sends';

function client(pages, update = async () => ({})) {
  return { send: jest.fn(async command => {
    if (command instanceof ScanCommand) return pages.shift();
    if (command instanceof UpdateCommand) return update(command.input);
    throw new Error('Unexpected command');
  }) };
}

test('dry run paginates with a bounded non-secret projection and makes no writes', async () => {
  const cursor = { send_id: 'cursor', recipient_discord_id: 'r' };
  const ddb = client([
    { Items: [row], LastEvaluatedKey: cursor },
    { Items: [{ ...row, guild_sender_id: '["guild","sender"]' }] },
  ]);
  await expect(backfillGuildSenderIndex(ddb, table)).resolves.toMatchObject({ scanned: 2, candidates: 1, indexed: 1, updated: 0 });
  expect(ddb.send).toHaveBeenCalledTimes(2);
  const inputs = ddb.send.mock.calls.map(([command]) => command.input);
  expect(inputs[1].ExclusiveStartKey).toEqual(cursor);
  expect(inputs[0]).toMatchObject({ Limit: 100, ConsistentRead: true });
  expect(Object.values(inputs[0].ExpressionAttributeNames).sort())
    .toEqual(['created_at', 'guild_id', 'guild_sender_id', 'recipient_discord_id', 'send_id', 'sender_discord_id']);
});

test('apply changes only the tuple and guards deletion, identity changes and concurrent writers', async () => {
  const ddb = client([{ Items: [row] }]);
  await expect(backfillGuildSenderIndex(ddb, table, { apply: true })).resolves.toMatchObject({ updated: 1 });
  const input = ddb.send.mock.calls[1][0].input;
  expect(input.Key).toEqual({ send_id: 'send', recipient_discord_id: 'recipient' });
  expect(input.UpdateExpression).toBe('SET #tuple = :tuple');
  expect(input.ConditionExpression).toBe('attribute_exists(#pk) AND #guild = :guild AND #sender = :sender AND #created = :created AND attribute_not_exists(#tuple)');
  expect(input.ExpressionAttributeValues).toEqual({ ':guild': 'guild', ':sender': 'sender', ':created': row.created_at, ':tuple': '["guild","sender"]' });
});

test('invalid and conflicting rows remain unchanged and are counted', async () => {
  const ddb = client([{ Items: [
    { ...row, sender_discord_id: 123 }, { ...row, created_at: undefined },
    { ...row, guild_sender_id: '["other","sender"]' }, { ...row, guild_sender_id: null },
  ] }]);
  await expect(backfillGuildSenderIndex(ddb, table, { apply: true })).resolves.toMatchObject({ invalid: 2, conflicting: 2, updated: 0 });
  expect(ddb.send).toHaveBeenCalledTimes(1);
});

test.each([undefined, null, ''])('legacy guild %p gets a separate tuple without changing the stored guild', async (guild_id) => {
  const ddb = client([{ Items: [{ ...row, guild_id }] }]);
  await expect(backfillGuildSenderIndex(ddb, table, { apply: true })).resolves.toMatchObject({ unscoped: 1, updated: 1 });
  const input = ddb.send.mock.calls[1][0].input;
  expect(input.UpdateExpression).toBe('SET #tuple = :tuple');
  expect(input.ExpressionAttributeValues[':tuple']).toBe('[null,"sender"]');
  if (guild_id === undefined) {
    expect(input.ConditionExpression).toContain('attribute_not_exists(#guild)');
    expect(input.ExpressionAttributeValues).not.toHaveProperty(':guild');
  } else {
    expect(input.ConditionExpression).toContain('#guild = :guild');
    expect(input.ExpressionAttributeValues[':guild']).toBe(guild_id);
  }
});

test('conditional races are counted without retrying an unsafe overwrite', async () => {
  const ddb = client([{ Items: [row] }], async () => {
    throw Object.assign(new Error('changed'), { name: 'ConditionalCheckFailedException' });
  });
  await expect(backfillGuildSenderIndex(ddb, table, { apply: true })).resolves.toMatchObject({ candidates: 1, raced: 1, updated: 0 });
  expect(ddb.send).toHaveBeenCalledTimes(2);
});

test('unexpected service errors stop migration instead of claiming completion', async () => {
  const ddb = client([{ Items: [row] }], async () => { throw new Error('service unavailable'); });
  await expect(backfillGuildSenderIndex(ddb, table, { apply: true })).rejects.toThrow('service unavailable');
});

test('a repeated scan cursor stops the migration', async () => {
  const cursor = { send_id: 'cursor', recipient_discord_id: 'r' };
  const ddb = client([{ LastEvaluatedKey: cursor }, { LastEvaluatedKey: cursor }]);
  await expect(backfillGuildSenderIndex(ddb, table)).rejects.toThrow('cursor did not advance');
});

test.each([[], ['wrong-table'], [table, '--aply'], [table, '--apply', '--apply']])('CLI refuses ambiguous arguments %p', async (...args) => {
  await expect(main(args)).rejects.toThrow('Usage:');
});
