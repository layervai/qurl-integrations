// UX-gate decision for `prompt=consent` on the Auth0 redirect.
//
// Pattern was triplicated across qurl-oauth.js (`/start` + `/callback`
// log enrichment) and discord-install.js (`/callback`). All three try
// the DDB read, fold to `Boolean(existing && existing.configured_by)`,
// and bias toward "treat as re-run" on throw — re-prompting an
// already-consenting admin is mild friction; silently skipping consent
// on a true re-run blocks key rotation entirely. Single helper keeps
// the bias direction in one place.
//
// Named for what it DOES (UX gate) rather than what it READS (re-run
// detection) — the bias-on-throw policy means callers should treat
// the return value as "should the consent screen fire?" not "is this
// strictly a re-run?". Per Justin's PR #177 round-9 review item #9.
const db = require('../store');
const logger = require('../logger');

/**
 * Returns true when the Auth0 redirect should set `prompt=consent`.
 * That happens when the guild was previously configured (has a
 * `configured_by` field), OR when the DDB read fails — bias on
 * throw goes toward "show consent" because re-prompting is mild
 * friction while silently skipping consent on a real re-run blocks
 * key rotation.
 *
 * @param {string} guildId
 * @param {string} contextLabel — short tag for the log line (e.g.
 *                                "qurl-oauth /start", "discord-install
 *                                /callback") so on-call can grep the
 *                                source.
 * @returns {Promise<boolean>}
 */
async function shouldPromptConsent(guildId, contextLabel) {
  try {
    const existing = await db.getGuildConfig(guildId);
    // `configured_by` is the canonical "this guild has been set up
    // before" signal — `setGuildApiKey` writes it atomically as part
    // of the upsert that lands the API key, so the field is present
    // iff a successful setup ran on this guild. Hand-edits or partial
    // rollbacks could in theory leave a row with the api_key column
    // but no `configured_by`; in that case we treat as first-install
    // and let Auth0 default-flow run.
    return Boolean(existing && existing.configured_by);
  } catch (err) {
    // info-level — benign fallback, not an error condition. A flaky
    // DDB shouldn't spam warns.
    logger.info('Failed to read guild config for prompt=consent gating; defaulting to consent-prompt', {
      context: contextLabel, error: err?.message, guildId,
    });
    return true;
  }
}

module.exports = { shouldPromptConsent };
