// First-install vs re-run detection for the OAuth setup flows.
//
// Pattern was triplicated across qurl-oauth.js (`/start` + `/callback`
// log enrichment) and discord-install.js (`/callback`). All three try
// the DDB read, fold to `Boolean(existing && existing.configured_by)`,
// and bias toward "treat as re-run" on throw — re-prompting an
// already-consenting admin is mild friction; silently skipping consent
// on a true re-run blocks key rotation entirely. Single helper keeps
// the bias direction in one place.
const db = require('../store');
const logger = require('../logger');

/**
 * Returns true when the guild was previously configured (has a
 * `configured_by` field on its guild_configs row). On DDB error,
 * biases toward `true` — see header comment for rationale.
 *
 * @param {string} guildId
 * @param {string} contextLabel — short tag for the log line (e.g.
 *                                "qurl-oauth /start", "discord-install
 *                                /callback") so on-call can grep the
 *                                source.
 * @returns {Promise<boolean>}
 */
async function getIsReRun(guildId, contextLabel) {
  try {
    const existing = await db.getGuildConfig(guildId);
    return Boolean(existing && existing.configured_by);
  } catch (err) {
    // info-level — benign fallback, not an error condition. A flaky
    // DDB shouldn't spam warns.
    logger.info('Failed to read guild config for prompt=consent gating; defaulting to re-run path', {
      context: contextLabel, error: err?.message, guildId,
    });
    return true;
  }
}

module.exports = { getIsReRun };
