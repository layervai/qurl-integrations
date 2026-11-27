// TODO(upstream-contract): the infra repo indexes this exact tuple in guild_sender_id-created_at-index.
function guildSenderKey(guildId, senderDiscordId) {
  if ((guildId !== null && (typeof guildId !== 'string' || !guildId)) || typeof senderDiscordId !== 'string' || !senderDiscordId) {
    throw new TypeError('guildSenderKey requires a non-empty string guild ID (or null for legacy) and sender ID');
  }
  // null is the explicit legacy bucket; never infer a guild for historical rows.
  return JSON.stringify([guildId, senderDiscordId]);
}

module.exports = { guildSenderKey };
