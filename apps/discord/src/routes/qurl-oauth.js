// qURL OAuth routes — Stage 1 entry (/oauth/qurl/start via /qurl setup)
// and the shared callback (/oauth/qurl/callback) that Stage 2
// (routes/discord-install.js) also chains into. Step-by-step user
// experience and CSRF posture live in the PR #177 description; Stage-2
// confused-deputy mitigation is documented in discord-install.js.
const crypto = require('crypto');
const express = require('express');
const config = require('../config');
const db = require('../store');
const logger = require('../logger');
const { renderPage } = require('../templates/page');
const { sendDM } = require('../discord');
const { verifyQurlOAuthState } = require('../utils/qurl-oauth-state');
const { rateLimit } = require('../utils/oauth-rate-limit');
const { verifyAuth0IdToken } = require('../utils/auth0-jwks');
const { readCookie } = require('../utils/cookies');
const { getIsReRun } = require('../utils/guild-config-state');
const { singleStringParam } = require('../utils/query-params');

// Network-call timeouts. Centralized so a future tuning of "qurl-service
// is slow under load" doesn't require a hunt-and-replace across both
// route files. 15s matches the existing per-call budget; the 5s and 10s
// values are for cleanup paths that should fail faster.
const AUTH0_TIMEOUT_MS = 15000;
const QURL_SERVICE_TIMEOUT_MS = 15000;
const ORPHAN_DELETE_TIMEOUT_MS = 10000;

const router = express.Router();

// Browser-session cookie binding (parallel to /auth/github's qurl_oauth_session
// pattern). /start sets a HttpOnly cookie holding a random 16-byte token;
// /callback re-checks it. If a leaked /qurl setup ephemeral URL is opened
// in a different browser, the cookie won't match and the callback rejects
// before reaching Auth0 — narrows the leaked-URL window from "5 minutes
// to anyone with a layerv.ai login" to "5 minutes AND the same browser
// that opened the link."
//
// Cookie name + setter shape live in utils/oauth-cookies.js so the
// Stage-2 chain (routes/discord-install.js sets the same cookie before
// chaining to Auth0) can't drift — single source of truth. PR #177
// follow-up C.1.
const {
  QURL_OAUTH_SESSION_COOKIE,
  setQurlOAuthCookie,
  clearQurlOAuthCookie,
} = require('../utils/oauth-cookies');

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

// Success page surfaces the guild ID, key prefix, and qURL account email
// as a structured label/value list (renderPage `details`) — the LOAD-
// BEARING visual mitigation against the Stage-2 confused-deputy class:
// if attacker pre-runs Discord install in their browser then forwards
// the chained Auth0-redirect URL to a victim, the readout shows
// (attacker's guild + victim's qURL email) so the victim can spot the
// mismatch before /qurl send usage starts. PR #177 follow-up C.5 —
// upgraded from the prior grey-prose subtext for visual prominence.
function renderSuccess(res, { guildId, keyPrefix, qurlAccountEmail }) {
  const details = [];
  if (guildId) details.push({ label: 'Discord guild', value: guildId });
  if (qurlAccountEmail) details.push({ label: 'qURL account', value: qurlAccountEmail });
  if (keyPrefix) details.push({ label: 'API key prefix', value: keyPrefix });
  return res.status(200).send(renderPage({
    title: 'qURL Connected',
    icon: '✅',
    heading: 'qURL is connected to your Discord server.',
    // CTA folded into message so the "confirm before closing" cue
    // always rides next to the binding readout, including when the
    // qURL-account-email line is missing (JWKS verify failure).
    message: 'Confirm the binding below matches your server, then close this tab and return to Discord. /qurl send is ready.',
    details,
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
// /qurl setup's ephemeral reply. Validate the state, set a session-bound
// cookie carrying the same state, then 302 to Auth0 with the state in
// the URL. The /callback handler re-checks cookie === query.state — a
// classic double-submit CSRF binding. If the link leaks and is opened
// in a different browser, the cookie is absent and /callback rejects
// before any Auth0 token exchange or qurl-service mint runs.
router.get('/start', rateLimit, async (req, res) => {
  if (!config.isQurlOAuthConfigured) {
    logger.warn('qURL OAuth start hit but Auth0 not configured', { ip: req.ip });
    return renderNotConfigured(res);
  }
  // Fail-fast: refuse the OAuth round-trip if KEY_ENCRYPTION_KEY is unset
  // here rather than after the admin completes Auth0 sign-in + consent.
  // The /callback path enforces the same guard, but reaching it burns
  // Auth0's `code` for nothing and produces a confusing "Authorization
  // failed at the very end" UX. Same principle as the legacy
  // modal-paste path's pre-modal check (commands.js).
  if (!process.env.KEY_ENCRYPTION_KEY) {
    logger.error('Refusing /oauth/qurl/start: KEY_ENCRYPTION_KEY is not set');
    return renderError(res, 503, 'qURL setup not provisioned',
      'The bot operator needs to set KEY_ENCRYPTION_KEY (encryption-at-rest) before qURL setup can store keys safely.');
  }
  const state = singleStringParam(req.query.state);
  const verified = verifyQurlOAuthState(state);
  if (!verified.ok) {
    logger.warn('qURL OAuth start rejected invalid state', { reason: verified.reason });
    return renderError(res, 400, 'Invalid setup link', 'This setup link is invalid or has expired (links last 5 minutes).');
  }
  // First-install vs re-run gate for prompt=consent (C.8). On re-run,
  // force prompt=consent so admins can rotate keys; on first install,
  // skip the redundant screen. Failsafe + bias direction live in
  // utils/guild-config-state.js (re-run on DDB error — silently
  // skipping consent on a real re-run would block rotation).
  const isReRun = await getIsReRun(verified.payload.guildId, 'qurl-oauth /start');
  // Double-submit CSRF cookie: value is the same state token the URL
  // carries to Auth0. /callback re-checks cookie === query.state.
  // Same-browser flows pass; leaked URLs in other browsers fail.
  // Cookie shape (path=/oauth so it spans Stage-2 chain, HttpOnly,
  // SameSite=Lax, Secure-when-HTTPS) lives in utils/oauth-cookies.js.
  setQurlOAuthCookie(res, req, state);
  const authorizeUrl = new URL(`https://${config.AUTH0_DOMAIN}/authorize`);
  authorizeUrl.searchParams.set('response_type', 'code');
  authorizeUrl.searchParams.set('client_id', config.AUTH0_CLIENT_ID);
  authorizeUrl.searchParams.set('redirect_uri', `${config.BASE_URL}/oauth/qurl/callback`);
  // Drop offline_access — we don't store/use refresh tokens (the API key
  // mint is one-shot), so requesting them is unnecessary attack surface
  // per PR #177 review.
  // Scope set: qurl:write + qurl:read for the API-key mint, openid +
  // email for the id_token's email claim (used by the success-page
  // binding readout). `profile` was previously requested but never
  // read — narrowing per PR #177 follow-up C.2 to tighten the
  // consent-screen "what is this app asking for?" UX.
  authorizeUrl.searchParams.set('scope', 'qurl:write qurl:read openid email');
  authorizeUrl.searchParams.set('audience', config.AUTH0_AUDIENCE);
  authorizeUrl.searchParams.set('state', state);
  // prompt=consent only on re-run (key-rotation flow) — without it
  // Auth0 silently re-uses prior consent and re-running /qurl setup
  // can't actually issue a new key. On first install, omit so the
  // admin doesn't see a redundant "are you sure?" screen on top of
  // the standard sign-in. Gate evaluated above. PR #177 follow-up C.8.
  if (isReRun) authorizeUrl.searchParams.set('prompt', 'consent');
  return res.redirect(302, authorizeUrl.toString());
});

// /oauth/qurl/callback — Auth0 redirects here after the admin consents.
// Validate state, exchange code → access_token, mint a guild-scoped API
// key on qurl-service, persist it via the Store abstraction, DM the admin.
router.get('/callback', rateLimit, async (req, res) => {
  if (!config.isQurlOAuthConfigured) {
    logger.warn('qURL OAuth callback hit but Auth0 not configured', { ip: req.ip });
    return renderNotConfigured(res);
  }
  // Same encryption-at-rest guard the legacy modal-paste path enforces in
  // commands.js: refuse to persist a billing-sensitive API key when
  // KEY_ENCRYPTION_KEY is unset, even if the OAuth handshake otherwise
  // succeeds. Without this, a non-prod boot with AUTH0_* set but
  // KEY_ENCRYPTION_KEY unset would silently store keys in plaintext via
  // the crypto module's plaintext-tolerated fallback. boot-requirements
  // only marks KEY_ENCRYPTION_KEY as `prodRequired`, so the guard here
  // is the load-bearing check for non-prod environments.
  if (!process.env.KEY_ENCRYPTION_KEY) {
    logger.error('Refusing /oauth/qurl/callback: KEY_ENCRYPTION_KEY is not set');
    return renderError(res, 503, 'qURL setup not provisioned',
      'The bot operator needs to set KEY_ENCRYPTION_KEY (encryption-at-rest) before qURL setup can store keys safely.');
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
  const code = singleStringParam(req.query.code);
  const state = singleStringParam(req.query.state);
  if (!code) {
    return renderError(res, 400, 'Missing authorization code', 'Auth0 did not return an authorization code.');
  }
  const verified = verifyQurlOAuthState(state);
  if (!verified.ok) {
    logger.warn('qURL OAuth callback rejected invalid state', { reason: verified.reason });
    return renderError(res, 400, 'Invalid setup link', 'This setup link is invalid or has expired.');
  }
  // Double-submit CSRF cookie check. /start set a cookie carrying the
  // same state value; /callback verifies cookie === query.state via
  // timing-safe compare. Same-browser flows pass; leaked-URL replays
  // in a different browser fail here BEFORE any Auth0 token exchange
  // or qurl-service mint runs.
  const cookieState = readCookie(req, QURL_OAUTH_SESSION_COOKIE);
  if (!cookieState) {
    logger.warn('qURL OAuth callback missing session cookie', { ip: req.ip });
    return renderError(res, 400, 'Invalid setup link', 'Setup must be completed in the same browser tab where /qurl setup was clicked.');
  }
  // Both inputs are strings (cookieState early-returned if null;
  // state = String(req.query.state || '')), so Buffer.from doesn't
  // throw. Length check before timingSafeEqual handles the only
  // remaining failure mode.
  const cookieBuf = Buffer.from(cookieState);
  const stateBuf = Buffer.from(state);
  const cookieMatches = cookieBuf.length === stateBuf.length
    && crypto.timingSafeEqual(cookieBuf, stateBuf);
  if (!cookieMatches) {
    logger.warn('qURL OAuth callback cookie/state mismatch', { ip: req.ip });
    return renderError(res, 400, 'Invalid setup link', 'Setup must be completed in the same browser tab where /qurl setup was clicked.');
  }
  // Cookie is consumed — clear it so a refreshed callback URL can't
  // re-bind. Path-must-match invariant lives in clearQurlOAuthCookie.
  clearQurlOAuthCookie(res);
  const { guildId, discordUserId } = verified.payload;

  // 1. Exchange the code for an access_token + id_token (Auth0 token
  //    endpoint). The id_token's `email` claim feeds the success-page
  //    binding readout (sanity-check display, not a security boundary).
  let accessToken;
  let qurlAccountEmail;
  try {
    // OAuth2 spec is application/x-www-form-urlencoded for the token
    // endpoint. Auth0 accepts JSON too, but form-urlencoded is the
    // canonical shape and matches the Discord token-exchange call in
    // routes/discord-install.js — symmetric across both providers
    // removes a "why is this one different?" reader question.
    const tokenResp = await fetch(`https://${config.AUTH0_DOMAIN}/oauth/token`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({
        grant_type: 'authorization_code',
        client_id: config.AUTH0_CLIENT_ID,
        client_secret: config.AUTH0_CLIENT_SECRET,
        code,
        redirect_uri: `${config.BASE_URL}/oauth/qurl/callback`,
      }),
      signal: AbortSignal.timeout(AUTH0_TIMEOUT_MS),
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
    // id_token email extraction with full JWKS verification. The email
    // is the load-bearing visual cue against the Stage-2 confused-
    // deputy class — promoting the security claim from "trust Auth0's
    // TLS response" to "verify the JWT signature against Auth0's
    // JWKS" closes the asterisk on that mitigation. See PR #177
    // follow-up issue #178 / section B.
    //
    // Failure to verify is non-fatal: the qURL key still gets minted
    // (the access_token mint already succeeded above) and the success
    // page renders without the qURL-account-email line. Ops sees a
    // logger.debug for triage. We do NOT fall back to unverified
    // decode — a forged email would be worse than no email.
    if (tokenJson.id_token) {
      // Distinct local — the outer `verified` (line ~258) carries the
      // qURL-OAuth-state verification result; this one carries the
      // Auth0 id_token JWKS verification result. Same shape, different
      // payload — keep them lexically separate to avoid reader confusion.
      const idTokenVerified = await verifyAuth0IdToken(tokenJson.id_token);
      if (idTokenVerified.ok && typeof idTokenVerified.payload?.email === 'string') {
        qurlAccountEmail = idTokenVerified.payload.email;
      } else if (!idTokenVerified.ok) {
        logger.debug('id_token verification failed (non-fatal — success page renders without email)', {
          reason: idTokenVerified.reason,
        });
      }
    }
  } catch (err) {
    logger.error('Auth0 token exchange threw', { error: err?.message });
    return renderError(res, 502, 'Authorization failed', 'A network error occurred during the Auth0 handshake. Please run /qurl setup again.');
  }

  // 2. Mint a guild-scoped qURL API key via POST /v1/api-keys, owned by
  //    the admin's qURL account (the Auth0 JWT's sub claim is the owner).
  let apiKey;
  let keyId;
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
      signal: AbortSignal.timeout(QURL_SERVICE_TIMEOUT_MS),
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
    keyId = mintJson?.data?.key_id;
    keyPrefix = mintJson?.data?.key_prefix;
    // Both api_key AND key_id are required: api_key is what we persist
    // for the bot to use; key_id is what the orphan-cleanup DELETE
    // below targets if persistence then fails. Treating a missing
    // key_id as success would leave us unable to clean up an orphan
    // billing-active key on the admin's account if DDB throws —
    // safer to refuse upfront so the admin retries with a complete
    // mint response. PR #177 follow-up C.3.
    if (!apiKey || !keyId) {
      logger.error('qURL API key response missing required fields', {
        guildId, hasApiKey: Boolean(apiKey), hasKeyId: Boolean(keyId),
      });
      return renderError(res, 502, 'Could not provision qURL key',
        'qurl-service returned an unexpected response. Please contact your layerv.ai admin.');
    }
  } catch (err) {
    logger.error('qURL API key mint threw', { error: err?.message, guildId });
    return renderError(res, 502, 'Could not provision qURL key',
      'A network error occurred while provisioning your qURL key. Please run /qurl setup again.');
  }

  // 3. Persist the key. setGuildApiKey is idempotent (upsert) — the
  //    previous key (if any) remains valid on qurl-service until the
  //    admin manually revokes it via layerv.ai.
  try {
    await db.setGuildApiKey(guildId, apiKey, discordUserId);
  } catch (err) {
    logger.error('Failed to persist guild API key after successful mint', {
      error: err?.message, guildId, discordUserId, keyId,
    });
    // Key was minted but not stored. Best-effort delete on qurl-service
    // so retries don't pile up orphan keys under the admin's account.
    // Fire-and-forget — even if the cleanup fails, the user-facing 500
    // response below is the right outcome (admin runs /qurl setup
    // again to retry; the orphan only persists if delete also fails).
    if (keyId) {
      fetch(`${config.QURL_ENDPOINT}/v1/api-keys/${encodeURIComponent(keyId)}`, {
        method: 'DELETE',
        headers: { 'Authorization': `Bearer ${accessToken}` },
        signal: AbortSignal.timeout(ORPHAN_DELETE_TIMEOUT_MS),
      })
        .then((r) => {
          if (!r.ok) {
            logger.warn('Best-effort orphan-key delete returned non-ok', {
              status: r.status, keyId, guildId,
            });
          }
        })
        .catch((dErr) => logger.warn('Best-effort orphan-key delete threw', {
          error: dErr?.message, keyId, guildId,
        }));
    }
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
  //    Include the key prefix so the admin can match it against
  //    `/qurl status` and against the layerv.ai dashboard, which
  //    matters for spotting binding mismatches in the rare confused-
  //    deputy edge case (see PR #177 review item 3).
  const dmLines = ['✅ **qURL is connected to your Discord server.** Your team can now use `/qurl send`. All usage will be billed to your qURL account.'];
  if (keyPrefix) dmLines.push(`Key prefix: \`${keyPrefix}\``);
  sendDM(discordUserId, dmLines.join('\n'))
    .catch((err) => logger.warn('Failed to DM admin after qURL setup', { error: err?.message, discordUserId }));

  return renderSuccess(res, { guildId, keyPrefix, qurlAccountEmail });
});

module.exports = router;
