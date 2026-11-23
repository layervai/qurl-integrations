const { qurlKeyOwner } = require('../src/utils/qurl-key-owner');

const identity = () => ({ data: { owner_id: 'owner-a', auth_type: 'api_key',
  api_key: { kind: 'api_key', scopes: ['qurl:read', 'qurl:write'] } } });

test('binds only an authenticated read/write API key owner', () => {
  expect(qurlKeyOwner(identity())).toBe('owner-a');
  for (const patch of [{ owner_id: '' }, { owner_id: undefined }, { auth_type: 'session' },
    { api_key: { kind: 'api_key', scopes: ['qurl:read'] } },
    { api_key: { kind: 'agent_key', scopes: ['qurl:read', 'qurl:write'] } }]) {
    expect(() => qurlKeyOwner({ data: { ...identity().data, ...patch } })).toThrow();
  }
});
