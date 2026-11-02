// TODO(upstream-contract): the infra repo indexes this exact tuple in guild_sender_id-created_at-index.
function guildSenderKey(guildId, senderDiscordId) {
  if (typeof guildId !== 'string' || !guildId || typeof senderDiscordId !== 'string' || !senderDiscordId) {
    throw new TypeError('guildSenderKey requires non-empty string guild and sender IDs');
  }
  return JSON.stringify([guildId, senderDiscordId]);
}

module.exports = { guildSenderKey };
