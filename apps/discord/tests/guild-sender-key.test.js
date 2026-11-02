const { guildSenderKey } = require('../src/utils/guild-sender-key');

test('guild/sender index format is fixed and collision-free', () => {
  expect(guildSenderKey('guild', 'sender')).toBe('["guild","sender"]');
  expect(guildSenderKey('a#b', 'c')).not.toBe(guildSenderKey('a', 'b#c'));
});

test.each([undefined, null, '', 123, {}, []])('rejects invalid identity %p instead of changing key encoding', (value) => {
  expect(() => guildSenderKey(value, 'sender')).toThrow(TypeError);
  expect(() => guildSenderKey('guild', value)).toThrow(TypeError);
});
