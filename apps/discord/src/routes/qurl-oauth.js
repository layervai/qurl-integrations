// qURL OAuth routes — replaces the API-key-paste modal in /qurl setup.
//
// Flow (Stage 1 — existing-install entry point via /qurl setup):
//   1. Admin runs /qurl setup in Discord → bot replies ephemerally with a
//      one-shot link to /oauth/qurl/start?state=<signed JWT>.
//   2. Admin clicks → /oauth/qurl/start verifies state, redirects to Auth0
//      authorize URL (response_type=code, scope=qurl:write+qurl:read).
//   3. Admin signs in to layerv.ai (Auth0) + consents.
//   4. Auth0 → /oauth/qurl/callback?code=…&state=…
//   5. Callback verifies state, exchanges code at Auth0 token endpoint for
//      an access_token JWT, calls qurl-service POST /v1/api-keys with the
//      JWT, persists the minted key in guild_configs (admin-owned —
//      billed to the admin's qURL account), DMs the admin "qURL is ready",
//      renders a success page.
//
// Stage 2 (new-install entry point — bot install + OAuth chained off the
// "Add to Discord" link) is a follow-up: needs Discord developer-portal
// redirect URI registration. Tracked in the PR description.
const express = require('express');
const config = require('../config');
const db = require('../store');
const logger = require('../logger');
const { renderPage } = require('../templates/page');
const { sendDM } = require('../discord');
const { verifyQurlOAuthState } = require('../utils/qurl-oauth-state');

const router = express.Router();

// Coarse-grained 503 page when AUTH0_* env vars aren't set yet (Auth0 app
// not registered / Justin hasn't set the prod SSM secrets). Single source
// of "not configured" surface so the start route + callback route surface
// the same message and the same 503 status.
function renderNotConfigured(res) {
  return res.status(503).send(renderPage({
    title: 'qURL Setup Not Configured',
    icon: '⚠️',
    heading: 'qURL setup is not configured yet',
    message: 'The Auth0 application for the qURL Discord bot has not been registered yet. '
      + 'Run /qurl setup again later, or contact your layerv.ai admin.',
    type: 'warning',
  }));
}

function renderSuccess(res) {
  return res.status(200).send(renderPage({
    title: 'qURL Connected',
    icon: '✅',
    heading: 'qURL is connected to your Discord server.',
    message: 'You can close this tab and return to Discord. /qurl send is ready.',
    type: 'success',
  }));
}

function renderError(res, statusCode, headline, detail) {
  return res.status(statusCode).send(renderPage({
    title: 'qURL Setup Failed',
    icon: '❌',
    heading: headline,
    message: detail + ' Run /qurl setup in Discord to start over.',
    type: 'error',
  }));
}

// /oauth/qurl/start — admin lands here after clicking the link in
// /qurl setup's ephemeral reply. Validate the state, then 302 to Auth0
// with the same state (so we can re-validate it on the callback).
router.get('/start', (req, res) => {
  if (!config.isQurlOAuthConfigured) {
    logger.warn('qURL OAuth start hit but Auth0 not configured', { ip: req.ip });
    return renderNotConfigured(res);
  }
  const state = String(req.query.state || '');
  const verified = verifyQurlOAuthState(state);
  if (!verified.ok) {
    logger.warn('qURL OAuth start rejected invalid state', { reason: verified.reason });
    return renderError(res, 400, 'Invalid setup link', 'This setup link is invalid or has expired (links last 5 minutes).');
  }
  const authorizeUrl = new URL(`https://${config.AUTH0_DOMAIN}/authorize`);
  authorizeUrl.searchParams.set('response_type', 'code');
  authorizeUrl.searchParams.set('client_id', config.AUTH0_CLIENT_ID);
  authorizeUrl.searchParams.set('redirect_uri', `${config.BASE_URL}/oauth/qurl/callback`);
  authorizeUrl.searchParams.set('scope', 'qurl:write qurl:read offline_access openid profile email');
  authorizeUrl.searchParams.set('audience', config.AUTH0_AUDIENCE);
  authorizeUrl.searchParams.set('state', state);
  // prompt=consent so re-running /qurl setup actually re-prompts, even if
  // the admin already authorized — otherwise Auth0 silently re-uses the
  // prior consent and a re-mint can't be triggered by an admin who wants
  // to rotate the key.
  authorizeUrl.searchParams.set('prompt', 'consent');
  return res.redirect(302, authorizeUrl.toString());
});

// /oauth/qurl/callback — Auth0 redirects here after the admin consents.
// Validate state, exchange code → access_token, mint a guild-scoped API
// key on qurl-service, persist it via the Store abstraction, DM the admin.
router.get('/callback', async (req, res) => {
  if (!config.isQurlOAuthConfigured) {
    logger.warn('qURL OAuth callback hit but Auth0 not configured', { ip: req.ip });
    return renderNotConfigured(res);
  }
  // Surface Auth0 error_description verbatim only in logs — render a safe
  // generic message to the browser so an attacker can't smuggle HTML via
  // an error URL.
  if (req.query.error) {
    logger.warn('qURL OAuth callback received error from Auth0', {
      error: req.query.error, errorDescription: req.query.error_description, ip: req.ip,
    });
    return renderError(res, 400, 'Authorization declined', 'You declined consent or Auth0 returned an error.');
  }
  const code = String(req.query.code || '');
  const state = String(req.query.state || '');
  if (!code) {
    return renderError(res, 400, 'Missing authorization code', 'Auth0 did not return an authorization code.');
  }
  const verified = verifyQurlOAuthState(state);
  if (!verified.ok) {
    logger.warn('qURL OAuth callback rejected invalid state', { reason: verified.reason });
    return renderError(res, 400, 'Invalid setup link', 'This setup link is invalid or has expired.');
  }
  const { guildId, discordUserId } = verified.payload;

  // 1. Exchange the code for an access_token (Auth0 token endpoint).
  let accessToken;
  try {
    const tokenResp = await fetch(`https://${config.AUTH0_DOMAIN}/oauth/token`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        grant_type: 'authorization_code',
        client_id: config.AUTH0_CLIENT_ID,
        client_secret: config.AUTH0_CLIENT_SECRET,
        code,
        redirect_uri: `${config.BASE_URL}/oauth/qurl/callback`,
      }),
      signal: AbortSignal.timeout(15000),
    });
    if (!tokenResp.ok) {
      const errBody = await tokenResp.text().catch(() => '');
      logger.error('Auth0 token exchange failed', { status: tokenResp.status, body: errBody.slice(0, 500) });
      return renderError(res, 502, 'Authorization failed', 'Could not complete the Auth0 handshake. Please run /qurl setup again.');
    }
    const tokenJson = await tokenResp.json();
    accessToken = tokenJson.access_token;
    if (!accessToken) {
      logger.error('Auth0 token response missing access_token');
      return renderError(res, 502, 'Authorization failed', 'Auth0 returned an unexpected response. Please run /qurl setup again.');
    }
  } catch (err) {
    logger.error('Auth0 token exchange threw', { error: err?.message });
    return renderError(res, 502, 'Authorization failed', 'A network error occurred during the Auth0 handshake. Please run /qurl setup again.');
  }

  // 2. Mint a guild-scoped qURL API key via POST /v1/api-keys, owned by
  //    the admin's qURL account (the Auth0 JWT's sub claim is the owner).
  let apiKey;
  let keyPrefix;
  try {
    const keyName = `Discord guild ${guildId}`;
    const mintResp = await fetch(`${config.QURL_ENDPOINT}/v1/api-keys`, {
      method: 'POST',
      headers: {
        'Authorization': `Bearer ${accessToken}`,
        'Content-Type': 'application/json',
        'Accept': 'application/json',
      },
      body: JSON.stringify({ name: keyName, scopes: ['qurl:write', 'qurl:read'] }),
      signal: AbortSignal.timeout(15000),
    });
    if (!mintResp.ok) {
      const errBody = await mintResp.text().catch(() => '');
      logger.error('qURL API key mint failed', {
        status: mintResp.status, body: errBody.slice(0, 500), guildId,
      });
      return renderError(res, 502, 'Could not provision qURL key',
        'qurl-service rejected the API-key request. Please run /qurl setup again, or contact your layerv.ai admin.');
    }
    const mintJson = await mintResp.json();
    apiKey = mintJson?.data?.api_key;
    keyPrefix = mintJson?.data?.key_prefix;
    if (!apiKey) {
      logger.error('qURL API key response missing api_key', { guildId });
      return renderError(res, 502, 'Could not provision qURL key',
        'qurl-service returned an unexpected response. Please contact your layerv.ai admin.');
    }
  } catch (err) {
    logger.error('qURL API key mint threw', { error: err?.message, guildId });
    return renderError(res, 502, 'Could not provision qURL key',
      'A network error occurred while provisioning your qURL key. Please run /qurl setup again.');
  }

  // 3. Persist the key. setGuildApiKey is idempotent (upsert), so re-running
  //    /qurl setup overwrites the prior key — the previous key remains valid
  //    on qurl-service until the admin manually revokes it via layerv.ai.
  try {
    await db.setGuildApiKey(guildId, apiKey, discordUserId);
  } catch (err) {
    logger.error('Failed to persist guild API key after successful mint', {
      error: err?.message, guildId, discordUserId,
    });
    // Key was minted but not stored — the admin still has it (it's owned
    // by their qURL account) but the bot can't see it. Surface a clear
    // remediation rather than failing silently.
    return renderError(res, 500, 'qURL key provisioned but not stored',
      'Your qURL API key was created but the bot could not save it. Please run /qurl setup again. '
      + 'If this keeps happening, contact your layerv.ai admin.');
  }
  logger.info('qURL OAuth setup complete', {
    guildId, configuredBy: discordUserId, keyPrefix,
  });

  // 4. DM the admin so they have a confirmation that doesn't depend on the
  //    browser tab. Fire-and-forget — a delivery failure shouldn't block
  //    the success page (DMs may be disabled by the admin's privacy
  //    settings, in which case the bot already has the working key).
  sendDM(discordUserId, '✅ **qURL is connected to your Discord server.** Your team can now use `/qurl send`. All usage will be billed to your qURL account.')
    .catch((err) => logger.warn('Failed to DM admin after qURL setup', { error: err?.message, discordUserId }));

  return renderSuccess(res);
});

module.exports = router;
