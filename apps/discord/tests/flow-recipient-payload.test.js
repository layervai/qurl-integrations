const { readFlowRecipients, withFlowRecipients } = require('../src/flow-recipient-payload');

const recipients = [{ id: '100000000000000001', alias: 'Alice' }, { id: '100000000000000002', alias: 'Bob' }];
const legacy = { recipientIds: recipients.map(r => r.id), recipientAliases: Object.fromEntries(recipients.map(r => [r.id, r.alias])) };

test('reads legacy and canonical recipients without losing persisted aliases', () => {
  expect(readFlowRecipients(legacy)).toEqual(recipients);
  expect(readFlowRecipients({ recipients })).toBe(recipients);
  expect(readFlowRecipients({ recipientIds: ['missing-alias'] })).toEqual([{ id: 'missing-alias', alias: undefined }]);
  expect(readFlowRecipients({})).toEqual([]);
});

test('canonical empty selection takes precedence over stale legacy fields', () => {
  expect(readFlowRecipients({ ...legacy, recipients: [] })).toEqual([]);
});

test.each([null, {}, [null], [{ id: '', alias: 'Alice' }], [{ id: 'one' }]])('malformed canonical recipients fail closed: %p', value => {
  expect(() => readFlowRecipients({ ...legacy, recipients: value })).toThrow(TypeError);
});

test('legacy writers stay legacy until the reader-first fleet rollout is complete', () => {
  expect(withFlowRecipients({ note: 'kept' }, recipients)).toEqual({ note: 'kept', ...legacy });
});

test('repicking a canonical flow preserves its format and drops stale legacy fields', () => {
  const old = { ...legacy, recipients, note: 'kept' };
  expect(withFlowRecipients(old, [recipients[1]])).toEqual({ recipients: [recipients[1]], note: 'kept' });
  expect(withFlowRecipients(old, [])).toEqual({ recipients: [], note: 'kept' });
  expect(old.recipients).toBe(recipients);
  expect(old.recipientIds).toHaveLength(2);
});
