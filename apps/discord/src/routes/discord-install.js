// Stage-2 entry point — "Add to Discord, select server" install flow.
//
// User-facing experience:
//   1. Admin clicks the static "Add to Discord" link on layerv.ai
//      (URL shape documented in project_qurl_bot_onboarding_model.md memory).
//   2. Discord shows the standard "Which server?" picker → admin selects.
//   3. Discord shows the bot's permission consent → admin clicks Authorize.
//   4. Discord redirects HERE: /oauth/discord/callback?code=…&guild_id=…&state=…
//   5. This route exchanges the Discord code for an access_token, calls
//      /users/@me to get the admin's Discord user ID, then 302-chains to
//      Auth0 with a qURL OAuth state binding (guildId + discordUserId)
//      so the existing /oauth/qurl/callback can finish the flow (mint
//      qURL API key on qurl-service, persist to guild_configs, DM admin).
//
// One unbroken click chain from "Add to Discord" → "qURL is ready" — no
// admin-visible step between Discord consent and Auth0 consent.
//
// CSRF posture (LOAD-BEARING — do not remove without replacing):
//   Discord's echoed `state` param is intentionally NOT validated here —
//   admins land via a static "Add to Discord" link on layerv.ai, not a
//   per-session form, so there's no session-bound token to check
//   against. Defense stacks on two surfaces:
//     1. Token exchange — only Discord can mint a `code` that pairs
//        with our DISCORD_CLIENT_ID/SECRET; a forged callback can't
//        get past the POST /oauth2/token call.
//     2. Success-page binding readout in qurl-oauth.js's renderSuccess
//        surfaces (guildId, qurlAccountEmail, keyPrefix) so a victim
//        of attacker-pre-runs-install-then-forwards-URL can spot the
//        mismatch before usage starts.
//   Issue #179 tracks the cross-repo signed-install-state work that
//   would close the asterisk on (2) by validating state at the entry.

const express = require('express');
const config = require('../config');
const logger = require('../logger');
const { renderPage } = require('../templates/page');
const { signQurlOAuthState } = require('../utils/qurl-oauth-state');
const { rateLimit } = require('../utils/oauth-rate-limit');
const { setQurlOAuthCookie } = require('../utils/oauth-cookies');
const { shouldPromptConsent } = require('../utils/guild-config-state');
const { singleStringParam } = require('../utils/query-params');
const { renderNotConfiguredPage } = require('../utils/oauth-not-configured');

// Network-call timeouts — same shape as routes/qurl-oauth.js. Centralized
// so a future "Discord OAuth2 is slow under load" tuning is one constant
// to flip.
const DISCORD_TIMEOUT_MS = 15000;

const router = express.Router();

// 503 surface delegates to the shared helper — single source of truth
// for the wire-vs-log split (C.4 invariant). The `surface` arg picks
// the discord-install-specific remediation copy.
function renderNotConfigured(res, reason) {
  return renderNotConfiguredPage(res, 'discord-install', reason);
}

// `detail` describes the immediate failure; we append a remediation
// hint that fits AFTER a Discord OAuth handshake failure (bot is
// already installed, retry through /qurl setup in-Discord). Other
// surfaces (the encryption-at-rest 503) use renderPage directly with
// surface-specific copy — see the inline call site.
function renderError(res, statusCode, headline, detail) {
  return res.status(statusCode).send(renderPage({
    title: 'Discord Install Failed',
    icon: '❌',
    heading: headline,
    message: detail + ' If the bot is already in your server, run /qurl setup directly.',
    type: 'error',
  }));
}

router.get('/callback', rateLimit, async (req, res) => {
  if (!config.isDiscordInstallConfigured) {
    // Single log line lives in renderNotConfiguredPage (round-9 item
    // #7). Reason is computed here because the helper would otherwise
    // need access to two config flags.
    const reason = !config.isQurlOAuthConfigured ? 'AUTH0_* unset' : 'DISCORD_CLIENT_SECRET unset';
    return renderNotConfigured(res, reason);
  }
  // Fail-fast: same encryption-at-rest guard as /oauth/qurl/start.
  // Bot is already in the server at this point (Discord install ran
  // before this redirect), so failing here just blocks the chained
  // qURL OAuth — admin can run /qurl setup later to retry. Without
  // this, we'd burn the Discord code on a token exchange + a Users
  // /@me round-trip + an Auth0 round-trip before failing at the qURL
  // callback's persist-time guard.
  if (!process.env.KEY_ENCRYPTION_KEY) {
    logger.error('Refusing /oauth/discord/callback: KEY_ENCRYPTION_KEY is not set');
    // Inline copy: the standard renderError tail ("run /qurl setup
    // directly") would fail too — same env-var gap. Specific copy
    // tells the admin to wait for the operator.
    return res.status(503).send(renderPage({
      title: 'Discord Install Failed',
      icon: '❌',
      heading: 'qURL setup not provisioned',
      message: 'The bot is in your server, but the operator hasn\'t configured encryption-at-rest yet (KEY_ENCRYPTION_KEY). Once that\'s set, run /qurl setup in your server.',
      type: 'error',
    }));
  }
  // Round-9 item #5: funnel through singleStringParam for symmetry.
  const errorParam = singleStringParam(req.query.error);
  if (errorParam) {
    logger.warn('Discord install callback received error from Discord', {
      error: errorParam,
      errorDescription: singleStringParam(req.query.error_description),
      ip: req.ip,
    });
    return renderError(res, 400, 'Authorization declined', 'You declined consent or Discord returned an error.');
  }
  const code = singleStringParam(req.query.code);
  const guildId = singleStringParam(req.query.guild_id);
  if (!code) {
    return renderError(res, 400, 'Missing authorization code', 'Discord did not return an authorization code.');
  }
  if (!guildId) {
    // Discord only includes guild_id when the user actually installed the
    // bot to a server. Missing guild_id means the install was abandoned
    // mid-flow or the bot's OAuth2 install link was invoked without
    // scope=bot — flag for triage.
    logger.warn('Discord install callback missing guild_id', { ip: req.ip });
    return renderError(res, 400, 'Bot install incomplete', 'Discord did not return the server you selected. Please click "Add to Discord" again.');
  }

  // Stage-2 ALWAYS sets prompt=consent on the chained Auth0 redirect,
  // independent of first-install vs re-install. Stage-2 is the
  // confused-deputy attack surface (a forwarded /oauth/discord/callback
  // URL); the explicit consent screen is one extra defense per
  // Justin's PR #177 round-9 item #1. Stage-1 (/oauth/qurl/start)
  // gates differently — it's reached from inside the guild via the
  // /qurl setup slash command, so guild-membership-proof has already
  // happened and the redundant consent screen on first install adds
  // friction without security gain.
  //
  // We still kick off the guild-config read in parallel so the
  // completion log line carries `previouslyConfigured` for ops
  // (distinguishes a fresh-add from a re-install in metrics).
  const previouslyConfiguredPromise = shouldPromptConsent(guildId, 'discord-install /callback');

  // 1. Exchange code at Discord for an access_token. The token itself
  //    isn't long-lived state we keep — we only use it to call /users/@me
  //    once and learn the installing user's Discord ID, which we then
  //    bind into the qURL OAuth state.
  let discordUserId;
  try {
    const tokenResp = await fetch('https://discord.com/api/oauth2/token', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({
        client_id: config.DISCORD_CLIENT_ID,
        client_secret: config.DISCORD_CLIENT_SECRET,
        grant_type: 'authorization_code',
        code,
        redirect_uri: `${config.BASE_URL}/oauth/discord/callback`,
      }),
      signal: AbortSignal.timeout(DISCORD_TIMEOUT_MS),
    });
    if (!tokenResp.ok) {
      const errBody = await tokenResp.text().catch(() => '');
      logger.error('Discord token exchange failed', {
        status: tokenResp.status, body: errBody.slice(0, 500),
      });
      return renderError(res, 502, 'Authorization failed', 'Could not complete the Discord install.');
    }
    const tokenJson = await tokenResp.json();
    const accessToken = tokenJson.access_token;
    if (!accessToken) {
      logger.error('Discord token response missing access_token');
      return renderError(res, 502, 'Authorization failed', 'Discord returned an unexpected response.');
    }
    // 2. /users/@me — minimal-scope identity probe. The bot install
    //    grants us `identify`-equivalent access via the bot scope; this
    //    call gives us the admin's Discord user ID so the qURL OAuth
    //    state can bind to it (matches the existing /qurl setup state).
    const userResp = await fetch('https://discord.com/api/users/@me', {
      headers: { 'Authorization': `Bearer ${accessToken}` },
      signal: AbortSignal.timeout(DISCORD_TIMEOUT_MS),
    });
    if (!userResp.ok) {
      logger.error('Discord /users/@me failed', { status: userResp.status });
      return renderError(res, 502, 'Authorization failed', 'Could not identify the installing user.');
    }
    const user = await userResp.json();
    discordUserId = user?.id;
    if (typeof discordUserId !== 'string' || !discordUserId) {
      // Log the response shape (key set) but NOT the values — Discord's
      // /users/@me payload can include username, global_name, avatar
      // hash, locale, and (with email scope) email. None of those are
      // safe to retain in operational logs without an explicit infosec
      // sign-off; key names alone tell us why the contract drifted.
      logger.error('Discord /users/@me returned no user id', {
        responseKeys: user ? Object.keys(user) : null,
      });
      return renderError(res, 502, 'Authorization failed', 'Discord returned an unexpected response.');
    }
  } catch (err) {
    logger.error('Discord OAuth handshake threw', { error: err?.message });
    return renderError(res, 502, 'Authorization failed', 'A network error occurred during the Discord handshake.');
  }

  // 3. Now we have (guildId, discordUserId) — the same shape as the
  //    /qurl setup slash-command state. Mint a qURL OAuth state and
  //    redirect to Auth0; the existing /oauth/qurl/callback will finish
  //    the flow (mint qurl-service API key, persist, DM admin).
  const qurlState = signQurlOAuthState(guildId, discordUserId);
  // Same double-submit CSRF cookie /oauth/qurl/start sets — Stage-2
  // chain shares the cookie with the qurl-oauth callback. Mitigates
  // leaked install-callback URL replay across browsers; does NOT fully
  // close confused-deputy (attacker pre-runs /oauth/discord/callback in
  // their own browser then forwards the Auth0-redirect URL to victim) —
  // the success-page binding readout in qurl-oauth.js's renderSuccess
  // surfaces (guild, qURL email) for visual sanity-check.
  setQurlOAuthCookie(res, req, qurlState);
  // Resolve the parallel DDB read for the completion log only; the
  // Auth0 prompt=consent decision is fixed for Stage-2 above.
  const previouslyConfigured = await previouslyConfiguredPromise;
  const authorizeUrl = new URL(`https://${config.AUTH0_DOMAIN}/authorize`);
  authorizeUrl.searchParams.set('response_type', 'code');
  authorizeUrl.searchParams.set('client_id', config.AUTH0_CLIENT_ID);
  authorizeUrl.searchParams.set('redirect_uri', `${config.BASE_URL}/oauth/qurl/callback`);
  // offline_access dropped per PR #177 review item 5; `profile` dropped
  // per follow-up C.2 — only the `email` claim is read from id_token.
  authorizeUrl.searchParams.set('scope', 'qurl:write qurl:read openid email');
  authorizeUrl.searchParams.set('audience', config.AUTH0_AUDIENCE);
  authorizeUrl.searchParams.set('state', qurlState);
  // ALWAYS prompt=consent on Stage-2 (per round-9 item #1) — see
  // `previouslyConfiguredPromise` block at the top of this handler.
  authorizeUrl.searchParams.set('prompt', 'consent');
  logger.info('Discord install complete; chaining to Auth0', { guildId, discordUserId, previouslyConfigured });
  return res.redirect(302, authorizeUrl.toString());
});

module.exports = router;
