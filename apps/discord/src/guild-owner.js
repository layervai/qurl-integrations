const db = require('./store');
const config = require('./config');
const { getIdentity } = require('./qurl');

function ownerConflict() {
  const error = new Error('Guild ownership must be verified before changing its key');
  error.code = 'GUILD_OWNER_CONFLICT';
  return error;
}

async function prepareGuildOwnerBinding(guildId, ownerId, discordUserId) {
  if (typeof ownerId !== 'string' || !ownerId.trim()) throw ownerConflict();
  let installationJoinedAt;
  if (config.ENABLE_CREDENTIAL_LIFECYCLE) {
    const { getGuildSetupInstallation } = require('./discord-rest');
    const installation = typeof discordUserId === 'string' && discordUserId
      ? await getGuildSetupInstallation(guildId, discordUserId) : null;
    if (!installation?.ok) {
      const error = new Error('Current server setup permission could not be verified');
      error.code = installation?.reason === 'not_admin' ? 'GUILD_SETUP_FORBIDDEN' : 'GUILD_INSTALLATION_UNVERIFIED';
      throw error;
    }
    installationJoinedAt = installation.joinedAt;
  }
  const prior = await db.getGuildConfigWithApiKey(guildId);
  if (installationJoinedAt && prior?.qurl_installation_joined_at
      && prior.qurl_installation_joined_at !== installationJoinedAt) {
    return { ownerId, expectedApiKey: prior.qurl_api_key || null,
      installationJoinedAt, previousInstallationJoinedAt: prior.qurl_installation_joined_at };
  }
  // TODO(upstream-contract): /v1/me owner_id and API-key ownership use the JWT sub.
  let priorOwner = prior?.qurl_owner_id;
  if (!priorOwner && prior?.qurl_api_key) {
    // Migrate a legacy row only after the service attests its saved key's owner.
    priorOwner = (await getIdentity(prior.qurl_api_key, guildId)).owner_id;
    if (typeof priorOwner !== 'string' || !priorOwner.trim()) throw new Error('qURL identity owner is missing');
  }
  if (priorOwner && priorOwner !== ownerId) throw ownerConflict();
  return { ownerId, expectedApiKey: prior?.qurl_api_key || null,
    ...(installationJoinedAt ? { installationJoinedAt } : {}) };
}

module.exports = { prepareGuildOwnerBinding };
