// Preserve provider precision: Date alone would merge distinct sub-millisecond joins.
function canonicalJoinedAt(value) {
  if (typeof value !== 'string') return null;
  const match = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/.exec(value);
  const time = match && Date.parse(value);
  if (!Number.isFinite(time) || time < 0) return null;
  return `${new Date(time).toISOString().slice(0, 19)}.${(match[2] || '').padEnd(9, '0')}Z`;
}

function isCurrentGuildInstallation(row) {
  return !!row?.qurl_api_key && typeof row.qurl_installation_joined_at === 'string'
    && canonicalJoinedAt(row.qurl_installation_joined_at) === row.qurl_installation_joined_at
    && row.observed_installation_joined_at === row.qurl_installation_joined_at
    && row.qurl_installation_removed !== true;
}

const GUILD_SETUP_REQUIRED_REPLY = 'A server admin must run `/qurl-admin setup` to connect this server installation.';

module.exports = { canonicalJoinedAt, isCurrentGuildInstallation, GUILD_SETUP_REQUIRED_REPLY };
