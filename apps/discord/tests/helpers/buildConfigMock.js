
function buildConfigMock({ guildId = null } = {}) {
  const normalizedGuildId = guildId || null;
  const isMultiTenant = !normalizedGuildId;

  return {
    GUILD_ID: normalizedGuildId,
    isMultiTenant,
  };
}

module.exports = { buildConfigMock };
