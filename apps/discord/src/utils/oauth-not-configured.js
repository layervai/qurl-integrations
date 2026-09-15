// Shared 503 "qURL setup is not configured" page used by both
// /oauth/qurl/start (Stage 1) and /oauth/discord/callback (Stage 2).
//
// SECURITY (PR #177 follow-up C.4): the env-var `reason` is logged on
// the operator side but MUST NOT appear in the rendered HTML —
// echoing names like "AUTH0_* unset" or "DISCORD_CLIENT_SECRET unset"
// would tell a probing attacker which secret an operator hasn't
// shipped yet. This module is the single source of truth for the
// wire-vs-log split so the two routers can't drift on it.
const logger = require('../logger');

/**
 * Render a 503 not-configured page. The `surface` arg picks the
 * remediation copy that fits the entry point — Stage-1 (/qurl setup)
 * and Stage-2 (/oauth/discord/callback) land here for different
 * reasons and the admin's next step differs.
 *
 * @param {import('express').Response} res
 * @param {'qurl-setup'|'discord-install'} surface
 * @param {string} [reason] - logged-only env-var hint; do NOT render
 */
function renderNotConfiguredPage(res, surface, reason) {
  // Belt-and-suspenders: pin the log shape so on-call has a uniform
  // grep target across both routers (`/qurl-setup not configured`
  // and `discord-install not configured`).
  const context = surface === 'discord-install' ? 'discord-install' : 'qurl-setup';
  logger.info(`${context} not configured`, { reason });

  const message = surface === 'discord-install'
    ? 'The bot was added to your server. Setup will be available once your layerv.ai operator finishes provisioning — try /qurl setup in your server later, or contact them out of band.'
    : 'The Auth0 application for the qURL Discord bot has not been registered yet. '
      + 'Run /qurl setup again later, or contact your layerv.ai admin.';

  return res.status(503).send(res.renderPage({
    title: 'qURL Setup Not Configured',
    icon: '⚠️',
    heading: 'qURL setup is not configured yet',
    message,
    type: 'warning',
  }));
}

module.exports = { renderNotConfiguredPage };
