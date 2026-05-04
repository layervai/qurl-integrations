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
// CSRF posture (read together with qurl-oauth.js:renderSuccess):
//   The `state` param Discord echoes is best-effort (admins paste the
//   "Add to Discord" link from layerv.ai, not from a per-session form),
//   so we don't validate it as a session-bound token. The defense
//   stacks across two surfaces:
//   1. Forged-callback rejection — only Discord can mint a `code` that
//      pairs with our DISCORD_CLIENT_ID/SECRET, so an attacker forging
//      a /oauth/discord/callback request without going through Discord
//      OAuth2 can't get past the token exchange.
//   2. Confused-deputy mitigation (LOAD-BEARING — do not remove without
//      replacing) — an attacker who pre-runs Discord install in their
//      own browser then forwards the chained Auth0-redirect URL to a
//      victim WOULD pass step 1, because the Discord code is real.
//      The success page in qurl-oauth.js's renderSuccess surfaces the
//      bound (guildId, qurlAccountEmail, keyPrefix) tuple so the
//      victim can spot the mismatch ("this isn't my server" / "this
//      isn't my qURL email") before usage starts. This is the only
//      thing standing between confused-deputy and a silent attacker
//      key-bind, until the Auth0 consent screen is templated to show
//      the target Discord guild (out of scope here — see PR #177).

const express = require('express');
const config = require('../config');
const logger = require('../logger');
const { renderPage } = require('../templates/page');
const { signQurlOAuthState } = require('../utils/qurl-oauth-state');
const { rateLimit } = require('../utils/oauth-rate-limit');
const { setQurlOAuthCookie } = require('../utils/oauth-cookies');
const { getIsReRun } = require('../utils/guild-config-state');

// Network-call timeouts — same shape as routes/qurl-oauth.js. Centralized
// so a future "Discord OAuth2 is slow under load" tuning is one constant
// to flip.
const DISCORD_TIMEOUT_MS = 15000;

const router = express.Router();

// SECURITY (C.4): `reason` is logged but NOT rendered to the page.
// Echoing 'AUTH0_* unset' or 'DISCORD_CLIENT_SECRET unset' would tell
// a probing attacker which secret the operator hasn't shipped yet.
function renderNotConfigured(res, reason) {
  logger.info('Discord install not configured', { reason });
  return res.status(503).send(renderPage({
    title: 'qURL Setup Not Configured',
    icon: '⚠️',
    heading: 'qURL setup is not configured yet',
    message: 'The bot was added to your server. Setup will be available once your layerv.ai operator finishes provisioning — try /qurl setup in your server later, or contact them out of band.',
    type: 'warning',
  }));
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
    const reason = !config.isQurlOAuthConfigured ? 'AUTH0_* unset' : 'DISCORD_CLIENT_SECRET unset';
    logger.warn('Discord install callback hit but not configured', { reason, ip: req.ip });
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
  if (req.query.error) {
    logger.warn('Discord install callback received error from Discord', {
      error: req.query.error, errorDescription: req.query.error_description, ip: req.ip,
    });
    return renderError(res, 400, 'Authorization declined', 'You declined consent or Discord returned an error.');
  }
  const code = String(req.query.code || '');
  const guildId = String(req.query.guild_id || '');
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

  // First-install vs re-install gate for prompt=consent (C.8). Kicked
  // off in parallel with the Discord handshake below — the result is
  // only consumed at redirect time (it flips one query-param), so
  // serializing it would add DDB latency to every install with no
  // user-facing benefit. getIsReRun's failsafe biases toward `true` on
  // throw — re-prompting an already-consenting admin is mild friction;
  // skipping consent on a true re-install blocks key rotation.
  const isReInstallPromise = getIsReRun(guildId, 'discord-install /callback');

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
  // Resolve the parallel DDB read kicked off above; bias toward consent
  // on throw is built into getIsReRun.
  const isReInstall = await isReInstallPromise;
  const authorizeUrl = new URL(`https://${config.AUTH0_DOMAIN}/authorize`);
  authorizeUrl.searchParams.set('response_type', 'code');
  authorizeUrl.searchParams.set('client_id', config.AUTH0_CLIENT_ID);
  authorizeUrl.searchParams.set('redirect_uri', `${config.BASE_URL}/oauth/qurl/callback`);
  // offline_access dropped per PR #177 review item 5; `profile` dropped
  // per follow-up C.2 — only the `email` claim is read from id_token.
  authorizeUrl.searchParams.set('scope', 'qurl:write qurl:read openid email');
  authorizeUrl.searchParams.set('audience', config.AUTH0_AUDIENCE);
  authorizeUrl.searchParams.set('state', qurlState);
  if (isReInstall) authorizeUrl.searchParams.set('prompt', 'consent');
  logger.info('Discord install complete; chaining to Auth0', { guildId, discordUserId, isReInstall });
  return res.redirect(302, authorizeUrl.toString());
});

module.exports = router;
