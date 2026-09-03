// Stage-2 entry point — "Add to Discord, select server" install flow.
//
// User-facing experience:
//   1. Admin clicks "Add to Discord" on layerv.ai, which opens this
//      router's /install endpoint and redirects to Discord.
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
//   /install mints a random state value and stores the same value in a
//   host-only, HttpOnly, SameSite=Lax cookie. /callback requires the
//   echoed state and cookie to match before exchanging Discord's code.
//   A callback URL forwarded from another browser therefore fails closed
//   before it can bind that browser's qURL account to the attacker's guild.
//   The first-party entrypoint removes the cross-repo signed-state contract
//   previously proposed in #179; the service that verifies state now mints it.

const crypto = require('crypto');
const express = require('express');
const config = require('../config');
const logger = require('../logger');
const { signQurlOAuthState } = require('../utils/qurl-oauth-state');
const { createPkcePair } = require('../utils/oauth-pkce');
const { rateLimit } = require('../utils/oauth-rate-limit');
const {
  DISCORD_INSTALL_SESSION_COOKIE,
  setQurlOAuthCookie,
  setQurlOAuthPkceCookie,
  setDiscordInstallSessionCookie,
  clearDiscordInstallSessionCookie,
} = require('../utils/oauth-cookies');
const { readCookie, timingSafeStringEqual } = require('../utils/cookies');
const { singleStringParam } = require('../utils/query-params');
const { renderNotConfiguredPage } = require('../utils/oauth-not-configured');

// Network-call timeouts — same shape as routes/qurl-oauth.js. Centralized
// so a future "Discord OAuth2 is slow under load" tuning is one constant
// to flip.
const DISCORD_TIMEOUT_MS = 15000;
// View Channels + Send Messages + Embed Links + Use Application Commands.
const DISCORD_BOT_PERMISSIONS = '2147503104';
const DISCORD_INSTALL_SCOPES = 'identify bot applications.commands';

const router = express.Router();

// 503 surface delegates to the shared helper — single source of truth
// for the wire-vs-log split (C.4 invariant). The `surface` arg picks
// the discord-install-specific remediation copy.
function renderNotConfigured(res, reason, beforeInstall = false) {
  const surface = beforeInstall ? 'discord-install-entry' : 'discord-install';
  return renderNotConfiguredPage(res, surface, reason);
}

// `detail` describes the immediate failure; we append a remediation
// hint that fits AFTER a Discord OAuth handshake failure (bot is
// already installed, retry through /qurl setup in-Discord). Other
// surfaces (the encryption-at-rest 503) use res.renderPage directly with
// surface-specific copy — see the inline call site.
function renderError(res, statusCode, headline, detail) {
  return res.status(statusCode).send(res.renderPage({
    title: 'Discord Install Failed',
    icon: '❌',
    heading: headline,
    message: detail + ' If the bot is already in your server, run /qurl setup directly.',
    type: 'error',
  }));
}

function notConfiguredReason() {
  return config.discordInstallNotConfiguredReason;
}

function installStateMatches(req, state) {
  const cookieState = readCookie(req, DISCORD_INSTALL_SESSION_COOKIE);
  if (!cookieState || !state) return false;
  return timingSafeStringEqual(cookieState, state);
}

router.get('/install', rateLimit, (req, res) => {
  if (!config.isDiscordInstallConfigured) {
    return renderNotConfigured(res, notConfiguredReason(), true);
  }
  // Refuse before Discord installs the bot: without the encryption key, the
  // chained qURL authorization cannot persist the new guild credential.
  if (!process.env.KEY_ENCRYPTION_KEY) {
    return renderNotConfigured(res, 'KEY_ENCRYPTION_KEY unset', true);
  }

  const state = crypto.randomBytes(32).toString('base64url');
  setDiscordInstallSessionCookie(res, req, state);
  const authorizeUrl = new URL('https://discord.com/oauth2/authorize');
  authorizeUrl.searchParams.set('client_id', config.DISCORD_CLIENT_ID);
  authorizeUrl.searchParams.set('permissions', DISCORD_BOT_PERMISSIONS);
  // `identify` is load-bearing: the callback uses the resulting user token
  // for GET /users/@me so it can bind setup to the installing Discord admin.
  authorizeUrl.searchParams.set('scope', DISCORD_INSTALL_SCOPES);
  authorizeUrl.searchParams.set('response_type', 'code');
  authorizeUrl.searchParams.set('redirect_uri', `${config.BASE_URL}/oauth/discord/callback`);
  authorizeUrl.searchParams.set('state', state);
  return res.redirect(302, authorizeUrl.toString());
});

router.get('/callback', rateLimit, async (req, res) => {
  if (!config.isDiscordInstallConfigured) {
    // Single log line lives in renderNotConfiguredPage (round-9 item
    // #7). Reason is computed here because the helper would otherwise
    // need access to two config flags.
    return renderNotConfigured(res, notConfiguredReason());
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
    return res.status(503).send(res.renderPage({
      title: 'Discord Install Failed',
      icon: '❌',
      heading: 'qURL setup not provisioned',
      message: 'The bot is in your server, but the operator hasn\'t configured encryption-at-rest yet (KEY_ENCRYPTION_KEY). Once that\'s set, run /qurl setup in your server.',
      type: 'error',
    }));
  }
  const installState = singleStringParam(req.query.state);
  const stateMatches = installStateMatches(req, installState);
  if (!stateMatches) {
    logger.warn('Discord install callback rejected invalid session state', { ip: req.ip });
    return renderError(res, 400, 'Invalid install link', 'Start again from the Add to Discord button on layerv.ai.');
  }
  clearDiscordInstallSessionCookie(res);
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
  const guildHint = singleStringParam(req.query.guild_id);
  if (!code) {
    return renderError(res, 400, 'Missing authorization code', 'Discord did not return an authorization code.');
  }
  if (!guildHint) {
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
  // No DDB read here — the previous round-9 build kicked off
  // shouldPromptConsent in parallel for a "previouslyConfigured"
  // log field, but with prompt=consent unconditional that helper's
  // bias-toward-true semantics were wrong for an informational
  // metric. If we ever want the first-install vs re-install signal,
  // pull it from setGuildApiKey's audit log or call getGuildConfig
  // directly with a try/catch that distinguishes hit/miss/error.
  // Round-9.6 item #3.

  // 1. Exchange code at Discord for an access_token and authoritative
  //    installed guild. The token itself
  //    isn't long-lived state we keep — we only use it to call /users/@me
  //    once and learn the installing user's Discord ID, which we then
  //    bind into the qURL OAuth state.
  let discordUserId;
  let guildId;
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
    // TODO(upstream-contract): Discord's token response `guild` object is
    // currently the authoritative install result. Revalidate this contract
    // when Discord publishes a versioned replacement for OAuth2 bot installs.
    guildId = tokenJson?.guild?.id;
    if (typeof guildId !== 'string' || !guildId) {
      logger.error('Discord token response missing installed guild', {
        responseKeys: tokenJson ? Object.keys(tokenJson) : null,
      });
      return renderError(res, 502, 'Authorization failed', 'Discord returned an unexpected response.');
    }
    // Discord documents callback guild_id as a hint only. Bind qURL setup
    // to the guild returned by the code exchange and reject any tampered hint
    // before calling /users/@me or starting the Auth0 leg.
    if (guildId !== guildHint) {
      logger.warn('Discord install callback guild hint did not match token response', { ip: req.ip });
      return renderError(res, 400, 'Server mismatch', 'The selected server did not match Discord\'s authorization response.');
    }
    // 2. /users/@me — minimal-scope identity probe. The install request's
    //    explicit `identify` scope authorizes this call and gives us the
    //    admin's Discord user ID so the qURL OAuth
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
  const { codeVerifier, codeChallenge } = createPkcePair();
  // Same double-submit CSRF cookie /oauth/qurl/start sets — Stage-2
  // chain shares the cookie with the qurl-oauth callback. Together with
  // the install-session cookie checked above, both OAuth legs remain bound
  // to the browser that began the first-party install flow.
  setQurlOAuthCookie(res, req, qurlState);
  setQurlOAuthPkceCookie(res, req, codeVerifier);
  const authorizeUrl = new URL(`https://${config.AUTH0_DOMAIN}/authorize`);
  authorizeUrl.searchParams.set('response_type', 'code');
  authorizeUrl.searchParams.set('client_id', config.AUTH0_CLIENT_ID);
  authorizeUrl.searchParams.set('redirect_uri', `${config.BASE_URL}/oauth/qurl/callback`);
  // offline_access dropped per PR #177 review item 5; `profile` dropped
  // per follow-up C.2 — only the `email` claim is read from id_token.
  authorizeUrl.searchParams.set('scope', 'qurl:write qurl:read openid email');
  authorizeUrl.searchParams.set('audience', config.AUTH0_AUDIENCE);
  authorizeUrl.searchParams.set('state', qurlState);
  authorizeUrl.searchParams.set('code_challenge', codeChallenge);
  authorizeUrl.searchParams.set('code_challenge_method', 'S256');
  // ALWAYS prompt=consent on Stage-2 (per round-9 item #1) — see the
  // confused-deputy block at the top of this handler.
  authorizeUrl.searchParams.set('prompt', 'consent');
  logger.info('Discord install complete; chaining to Auth0', { guildId, discordUserId });
  return res.redirect(302, authorizeUrl.toString());
});

module.exports = router;
