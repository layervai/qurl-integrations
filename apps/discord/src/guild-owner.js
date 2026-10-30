const db = require('./store');
const { getIdentity } = require('./qurl');

function ownerConflict() {
  const error = new Error('Guild ownership must be verified before changing its key');
  error.code = 'GUILD_OWNER_CONFLICT';
  return error;
}

async function prepareGuildOwnerBinding(guildId, ownerId) {
  if (typeof ownerId !== 'string' || !ownerId) throw ownerConflict();
  const prior = await db.getGuildConfigWithApiKey(guildId);
  // TODO(upstream-contract): /v1/me owner_id and API-key ownership use the JWT sub.
  let priorOwner = prior?.qurl_owner_id;
  if (!priorOwner && prior?.qurl_api_key) {
    // Migrate a legacy row only after the service attests its saved key's owner.
    priorOwner = (await getIdentity(prior.qurl_api_key, guildId)).owner_id;
    if (typeof priorOwner !== 'string' || !priorOwner) throw ownerConflict();
  }
  if (priorOwner && priorOwner !== ownerId) throw ownerConflict();
  return { ownerId, expectedApiKey: prior?.qurl_api_key || null };
}

module.exports = { prepareGuildOwnerBinding };
