const { AUDIT_EVENTS, QUERY_ONLY_AUDIT_EVENTS } = require('../src/constants');

test('metric and query-only audit names are immutable and disjoint', () => {
  expect(Object.isFrozen(AUDIT_EVENTS)).toBe(true);
  expect(Object.isFrozen(QUERY_ONLY_AUDIT_EVENTS)).toBe(true);
  expect(QUERY_ONLY_AUDIT_EVENTS.QURL_GUILD_KEY_CONFIGURED).toBe('qurl_guild_key_configured');
  expect(Object.hasOwn(AUDIT_EVENTS, 'QURL_GUILD_KEY_CONFIGURED')).toBe(false);
  const keys = [...Object.keys(AUDIT_EVENTS), ...Object.keys(QUERY_ONLY_AUDIT_EVENTS)];
  const values = [...Object.values(AUDIT_EVENTS), ...Object.values(QUERY_ONLY_AUDIT_EVENTS)];
  expect(new Set(keys).size).toBe(keys.length);
  expect(new Set(values).size).toBe(values.length);
});
