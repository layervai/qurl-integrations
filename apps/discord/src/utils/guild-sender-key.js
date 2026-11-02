// TODO(upstream-contract): layervai/qurl-integrations-infra#1999 indexes this exact tuple for guild-scoped revoke history.
function guildSenderKey(guildId, senderDiscordId) {
  if (typeof guildId !== 'string' || !guildId || typeof senderDiscordId !== 'string' || !senderDiscordId) {
    throw new TypeError('guildSenderKey requires non-empty string guild and sender IDs');
  }
  return JSON.stringify([guildId, senderDiscordId]);
}

module.exports = { guildSenderKey };
