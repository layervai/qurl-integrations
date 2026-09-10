// Shared 503 "qURL setup is not configured" page used by /oauth/qurl/start
// (Stage 1) and the /oauth/discord/install + /callback flow (Stage 2).
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
 * and Stage-2 entry/callback routes land here at different points, so the
 * admin's safe next step differs.
 *
 * @param {import('express').Response} res
 * @param {'qurl-setup'|'discord-install'|'discord-install-entry'} surface
 * @param {string} [reason] - logged-only env-var hint; do NOT render
 */
function renderNotConfiguredPage(res, surface, reason) {
  // Belt-and-suspenders: pin the log shape so on-call has a uniform
  // grep target across both routers (`/qurl-setup not configured`
  // and `discord-install not configured`).
  const isDiscordInstall = typeof surface === 'string' && surface.startsWith('discord-install');
  const context = isDiscordInstall ? 'discord-install' : 'qurl-setup';
  logger.info(`${context} not configured`, { surface, reason });

  let message;
  if (surface === 'discord-install-entry') {
    message = 'Nothing was installed. Try Add to Discord again after your layerv.ai operator finishes provisioning, or contact them out of band.';
  } else if (surface === 'discord-install') {
    message = 'The bot may already be in your server. If it is, run /qurl setup after your layerv.ai operator finishes provisioning. Otherwise, try Add to Discord again then, or contact them out of band.';
  } else {
    message = 'The Auth0 application for the qURL Discord bot has not been registered yet. '
      + 'Run /qurl setup again later, or contact your layerv.ai admin.';
  }

  return res.status(503).send(res.renderPage({
    title: isDiscordInstall ? 'Discord Install Not Configured' : 'qURL Setup Not Configured',
    icon: '⚠️',
    heading: isDiscordInstall
      ? 'Discord install is not configured yet'
      : 'qURL setup is not configured yet',
    message,
    type: 'warning',
  }));
}

module.exports = { renderNotConfiguredPage };
