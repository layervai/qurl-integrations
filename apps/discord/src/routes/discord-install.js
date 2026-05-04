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
const db = require('../store');
const logger = require('../logger');
const { renderPage } = require('../templates/page');
const { signQurlOAuthState } = require('../utils/qurl-oauth-state');
const { rateLimit } = require('../utils/oauth-rate-limit');
const {
  QURL_OAUTH_SESSION_COOKIE,
  QURL_OAUTH_COOKIE_PATH,
  QURL_OAUTH_COOKIE_TTL_SECONDS,
} = require('../utils/oauth-cookies');

// Network-call timeouts — same shape as routes/qurl-oauth.js. Centralized
// so a future "Discord OAuth2 is slow under load" tuning is one constant
// to flip.
const DISCORD_TIMEOUT_MS = 15000;

const router = express.Router();

function renderNotConfigured(res, reason) {
  // Wording: "/qurl setup later" is the right next step ONLY when this
  // 503 is from a partial config (DISCORD_CLIENT_SECRET unset, but
  // AUTH0_* set — the slash command would still take the OAuth path).
  // When AUTH0_* itself is unset, /qurl setup falls back to modal-paste
  // and the admin needs an out-of-band channel to layerv.ai. Branch the
  // remediation copy on `reason`.
  //
  // SECURITY: `reason` is logged but NOT rendered to the page. Echoing
  // 'AUTH0_* unset' or 'DISCORD_CLIENT_SECRET unset' to the browser
  // would tell a probing attacker exactly which secret an operator
  // hasn't provisioned yet. Match qurl-oauth.js renderNotConfigured's
  // generic wire shape. PR #177 follow-up C.4.
  logger.info('renderNotConfigured', { reason });
  const remediation = reason && reason.startsWith('AUTH0')
    ? 'The bot was added to your server. Contact your layerv.ai admin to finish setup once the qURL OAuth application is registered.'
    : 'The bot was added to your server successfully — run /qurl setup in-Discord once the operator finishes provisioning.';
  return res.status(503).send(renderPage({
    title: 'qURL Setup Not Configured',
    icon: '⚠️',
    heading: 'qURL setup is not configured yet',
    message: remediation,
    type: 'warning',
  }));
}

// `detail` should describe the immediate failure; we append a remediation
// hint specific to the surface the failure landed on. The default hint
// ("run /qurl setup directly") makes sense after a Discord OAuth handshake
// failure — bot is already installed, qURL setup can be retried in-Discord
// — but DOESN'T make sense on the not-configured 503, where /qurl setup
// would also fail because the same env vars are missing. The
// `remediation` override lets the caller pick the right copy.
function renderError(res, statusCode, headline, detail, remediation) {
  const tail = remediation ?? ' If the bot is already in your server, run /qurl setup directly.';
  return res.status(statusCode).send(renderPage({
    title: 'Discord Install Failed',
    icon: '❌',
    heading: headline,
    message: detail + tail,
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
    return renderError(res, 503, 'qURL setup not provisioned',
      'The bot is in your server, but the operator hasn\'t configured encryption-at-rest yet (KEY_ENCRYPTION_KEY).',
      ' Once that\'s set, run /qurl setup in your server.');
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

  // First-install vs re-install gate for prompt=consent. Hoisted above
  // the Discord handshake on purpose — the lookup needs only `guildId`
  // (already in req.query), not `discordUserId`, so doing it here means
  // a slow-DDB blip can't delay the Discord token-exchange + /users/@me
  // round-trips for an installer who's just sitting on the redirect
  // page. The handshake-timeout window stays bounded by DISCORD_TIMEOUT_MS
  // alone. PR #177 follow-up C.8 + post-round-8 review.
  let isReInstall = false;
  try {
    const existing = await db.getGuildConfig(guildId);
    isReInstall = Boolean(existing && existing.configured_by);
  } catch (err) {
    // Bias toward consent prompt on DDB failure — the cost is one extra
    // click for an admin who just signed in (mild), versus silently
    // skipping consent on a true re-install which blocks key rotation
    // entirely (worse).
    logger.info('Failed to read guild config for prompt=consent gating; defaulting to re-install path', {
      error: err?.message, guildId,
    });
    isReInstall = true;
  }

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
  // Set the same double-submit CSRF cookie that /oauth/qurl/start sets,
  // so /oauth/qurl/callback's cookie === state check passes for the
  // Stage-2 chain too. Path /oauth (not /oauth/discord) so the cookie
  // is visible to /oauth/qurl/callback further along the chain.
  // Mitigates leaked install-callback URL replay across browsers; does
  // not fully prevent confused-deputy where attacker pre-runs
  // /oauth/discord/callback in their own browser then forwards the
  // Auth0-redirect URL to victim — see PR #177 review item 3 + the
  // success-page binding readout that surfaces guild + qURL email
  // for visual sanity-check before the admin closes the tab.
  res.cookie(QURL_OAUTH_SESSION_COOKIE, qurlState, {
    httpOnly: true,
    secure: req.protocol === 'https',
    sameSite: 'lax',
    maxAge: QURL_OAUTH_COOKIE_TTL_SECONDS * 1000,
    path: QURL_OAUTH_COOKIE_PATH,
  });
  const authorizeUrl = new URL(`https://${config.AUTH0_DOMAIN}/authorize`);
  authorizeUrl.searchParams.set('response_type', 'code');
  authorizeUrl.searchParams.set('client_id', config.AUTH0_CLIENT_ID);
  authorizeUrl.searchParams.set('redirect_uri', `${config.BASE_URL}/oauth/qurl/callback`);
  // offline_access dropped per PR #177 review item 5 — no refresh-token
  // use. `profile` dropped per follow-up C.2 — only the `email` claim
  // is read from the id_token.
  authorizeUrl.searchParams.set('scope', 'qurl:write qurl:read openid email');
  authorizeUrl.searchParams.set('audience', config.AUTH0_AUDIENCE);
  authorizeUrl.searchParams.set('state', qurlState);
  // prompt=consent only on re-install — gate evaluated above the
  // handshake. See the `isReInstall` block at the top of this handler
  // for rationale.
  if (isReInstall) authorizeUrl.searchParams.set('prompt', 'consent');
  logger.info('Discord install complete; chaining to Auth0', { guildId, discordUserId, isReInstall });
  return res.redirect(302, authorizeUrl.toString());
});

module.exports = router;
