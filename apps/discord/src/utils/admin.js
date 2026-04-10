// Admin utilities
const config = require('../config');

/**
 * Check if a user ID is an admin
 * @param {string} userId - Discord user ID
 * @returns {boolean}
 */
function isAdmin(userId) {
  return config.ADMIN_USER_IDS.includes(userId);
}

/**
 * Reply with permission denied message
 * @param {import('discord.js').CommandInteraction} interaction
 */
async function replyPermissionDenied(interaction) {
  return interaction.reply({
    content: '❌ You don\'t have permission to use this command.',
    ephemeral: true,
  });
}

/**
 * Check admin permission and reply if denied
 * Returns true if user is admin, false if denied
 * @param {import('discord.js').CommandInteraction} interaction
 * @returns {Promise<boolean>}
 */
async function requireAdmin(interaction) {
  if (!isAdmin(interaction.user.id)) {
    await replyPermissionDenied(interaction);
    return false;
  }
  return true;
}

module.exports = {
  isAdmin,
  replyPermissionDenied,
  requireAdmin,
};
