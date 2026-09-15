// Shared product boundary for every Discord interaction surface. qURL's
// workflows depend on guild roles, members, channels, and guild-scoped
// credentials, so a DM or user-only install cannot safely execute them.
const { ApplicationIntegrationType } = require('discord-api-types/v10');

const UNSUPPORTED_CONTEXT_MSG = 'qURL only works inside a server where it is installed, not in DMs or from a user install.';

function isUserInstallOnlyInteraction(interaction) {
  const owners = interaction.authorizingIntegrationOwners;
  // TODO(upstream-contract): discord.js exposes authorizing owners through
  // numeric integration-type properties (0 = guild install, 1 = user install).
  // A missing map intentionally fails open to the guildId boundary for legacy
  // payloads; a dual install remains supported when the guild authorized it.
  return Boolean(
    owners?.[ApplicationIntegrationType.UserInstall]
    && !Object.hasOwn(owners, ApplicationIntegrationType.GuildInstall)
  );
}

function isUnsupportedQurlContext(interaction) {
  return !interaction.guildId || isUserInstallOnlyInteraction(interaction);
}

module.exports = {
  isUnsupportedQurlContext,
  UNSUPPORTED_CONTEXT_MSG,
};
