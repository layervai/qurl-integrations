const { canonicalJoinedAt } = require('../src/utils/guild-installation');

test('installation identity preserves provider precision and normalizes time zones', () => {
  expect(canonicalJoinedAt('2026-10-01T01:02:03.123456+00:00')).toBe('2026-10-01T01:02:03.123456000Z');
  expect(canonicalJoinedAt('2026-10-01T02:02:03.123456+01:00')).toBe('2026-10-01T01:02:03.123456000Z');
  expect(canonicalJoinedAt('2026-10-01T01:02:03.123457Z')).not.toBe(canonicalJoinedAt('2026-10-01T01:02:03.123456Z'));
});

test.each([null, undefined, '', 'not-a-date', '2026-10-01', '2026-99-99T00:00:00Z', 123])('rejects invalid joined_at %p', value => {
  expect(canonicalJoinedAt(value)).toBeNull();
});


test('only a current canonical installation is active', () => {
  const { isCurrentGuildInstallation } = require('../src/utils/guild-installation');
  const joinedAt = '2026-10-01T00:00:00.000000000Z';
  const row = { qurl_api_key: 'encrypted', qurl_installation_joined_at: joinedAt,
    observed_installation_joined_at: joinedAt };
  expect(isCurrentGuildInstallation(row)).toBe(true);
  expect(isCurrentGuildInstallation({ ...row, qurl_installation_removed: true })).toBe(false);
  expect(isCurrentGuildInstallation({ ...row, observed_installation_joined_at: 'newer' })).toBe(false);
  expect(isCurrentGuildInstallation({ ...row, qurl_installation_joined_at: 'bad', observed_installation_joined_at: 'bad' })).toBe(false);
});
