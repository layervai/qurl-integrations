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
// Stage 2 (new-install entry point — "Add to Discord" link → Discord
// OAuth2 install → server pick → consent → chained Auth0 leg) ships in
// this same PR via routes/discord-install.js, which terminates at the
// same /oauth/qurl/callback handler below. Both flows share the cookie
// (path=/oauth) and the qURL OAuth state shape, so the callback's
// CSRF check, KEY_ENCRYPTION_KEY guard, mint, persist, and DM are
// stage-agnostic.
const crypto = require('crypto');
const express = require('express');
const config = require('../config');
const db = require('../store');
const logger = require('../logger');
const { renderPage } = require('../templates/page');
const { sendDM } = require('../discord');
const { verifyQurlOAuthState, b64urlDecode } = require('../utils/qurl-oauth-state');
const { rateLimit } = require('../utils/oauth-rate-limit');

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
const QURL_OAUTH_SESSION_COOKIE = 'qurl_setup_session';
const COOKIE_TTL_SECONDS = 5 * 60;

function readCookie(req, name) {
  const header = req.headers.cookie;
  if (!header) return null;
  // Reject duplicate cookies (browser extension / sibling-subdomain race) —
  // ambiguity = no binding, treat as "no cookie".
  const matches = [];
  for (const part of header.split(';')) {
    const eq = part.indexOf('=');
    if (eq === -1) continue;
    const k = part.slice(0, eq).trim();
    if (k !== name) continue;
    try {
      matches.push(decodeURIComponent(part.slice(eq + 1).trim()));
    } catch {
      return null;
    }
    if (matches.length > 1) return null;
  }
  return matches.length === 1 ? matches[0] : null;
}

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
// so the admin can sanity-check the binding before closing the tab. This
// is the LOAD-BEARING user-visible mitigation against the Stage-2
// confused-deputy class of attack flagged in the PR #177 review: in a
// hypothetical attacker-pre-runs-the-Discord-callback scenario, the
// state would carry the attacker's discord_user_id + guild_id while the
// qURL account email is the victim's. Showing both halves of the
// binding lets the admin spot a mismatch ("this isn't my server" /
// "this isn't my qURL email") before usage starts.
//
// Subtext is plain prose — `renderPage` HTML-escapes the whole string at
// render time, so any inline tags (<code>, <strong>, etc.) would render
// as literal angle brackets. The values themselves still ride through
// renderPage's escapeHtml so untrusted input can't smuggle script tags;
// the trade-off is that the values aren't visually distinguished
// (no monospace formatting). That's an acceptable cost for the
// security guarantee: the goal is "admin reads the values" not
// "admin enjoys nice typography."
function renderSuccess(res, { guildId, keyPrefix, qurlAccountEmail }) {
  const detailLines = [];
  if (guildId) detailLines.push(`Discord guild: ${guildId}`);
  if (qurlAccountEmail) detailLines.push(`qURL account: ${qurlAccountEmail}`);
  if (keyPrefix) detailLines.push(`API key prefix: ${keyPrefix}`);
  const subtext = detailLines.length > 0
    ? detailLines.join(' · ') + ' — confirm these match before closing.'
    : undefined;
  return res.status(200).send(renderPage({
    title: 'qURL Connected',
    icon: '✅',
    heading: 'qURL is connected to your Discord server.',
    message: 'You can close this tab and return to Discord. /qurl send is ready.',
    subtext,
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
router.get('/start', rateLimit, (req, res) => {
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
  const state = String(req.query.state || '');
  const verified = verifyQurlOAuthState(state);
  if (!verified.ok) {
    logger.warn('qURL OAuth start rejected invalid state', { reason: verified.reason });
    return renderError(res, 400, 'Invalid setup link', 'This setup link is invalid or has expired (links last 5 minutes).');
  }
  // Double-submit CSRF cookie: cookie value is the same state token the
  // URL carries to Auth0. /callback re-checks (timing-safe) that
  // cookie === query.state. Same-browser flows have both; leaked URLs
  // opened in other browsers don't have the cookie and fail.
  // Path is /oauth (not /oauth/qurl) so the same cookie also covers the
  // Stage-2 "Add to Discord" entry path /oauth/discord/callback when
  // that flow chains through to /oauth/qurl/callback.
  // `secure: req.protocol === 'https'` relies on `trust proxy` being set
  // in server.js so req.protocol reflects the X-Forwarded-Proto header
  // from the ALB. Flipping `trust proxy` off would silently downgrade
  // production cookies to insecure.
  res.cookie(QURL_OAUTH_SESSION_COOKIE, state, {
    httpOnly: true,
    secure: req.protocol === 'https',
    sameSite: 'lax',
    maxAge: COOKIE_TTL_SECONDS * 1000,
    path: '/oauth',
  });
  const authorizeUrl = new URL(`https://${config.AUTH0_DOMAIN}/authorize`);
  authorizeUrl.searchParams.set('response_type', 'code');
  authorizeUrl.searchParams.set('client_id', config.AUTH0_CLIENT_ID);
  authorizeUrl.searchParams.set('redirect_uri', `${config.BASE_URL}/oauth/qurl/callback`);
  // Drop offline_access — we don't store/use refresh tokens (the API key
  // mint is one-shot), so requesting them is unnecessary attack surface
  // per PR #177 review.
  authorizeUrl.searchParams.set('scope', 'qurl:write qurl:read openid profile email');
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
  let cookieMatches = false;
  try {
    const cookieBuf = Buffer.from(cookieState);
    const stateBuf = Buffer.from(state);
    cookieMatches = cookieBuf.length === stateBuf.length
      && crypto.timingSafeEqual(cookieBuf, stateBuf);
  } catch {
    cookieMatches = false;
  }
  if (!cookieMatches) {
    logger.warn('qURL OAuth callback cookie/state mismatch', { ip: req.ip });
    return renderError(res, 400, 'Invalid setup link', 'Setup must be completed in the same browser tab where /qurl setup was clicked.');
  }
  // Cookie is consumed — clear it so a refreshed callback URL can't
  // re-bind. Fresh /qurl setup runs mint a new state and set a new
  // cookie.
  res.clearCookie(QURL_OAUTH_SESSION_COOKIE, { path: '/oauth' });
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
    // Best-effort id_token email extraction. JWT decode without
    // signature verification — the id_token came from Auth0 over TLS
    // in this same response, so signature verification is redundant
    // for a display-only readout. (See PR #177 review note: the email
    // becomes a load-bearing visual cue against confused-deputy on
    // Stage 2, but the security claim of the readout is still "trust
    // Auth0's TLS response" — full verification would require JWKS
    // caching and is out of scope here.) Reuses b64urlDecode from
    // utils/qurl-oauth-state.js for consistent base64-url handling.
    try {
      const idToken = tokenJson.id_token;
      if (typeof idToken === 'string') {
        const parts = idToken.split('.');
        if (parts.length === 3) {
          const payload = JSON.parse(b64urlDecode(parts[1]).toString('utf8'));
          if (typeof payload.email === 'string') qurlAccountEmail = payload.email;
        }
      }
    } catch (err) {
      logger.debug('id_token email extraction failed (non-fatal)', { error: err?.message });
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

  // 3. Persist the key. setGuildApiKey is idempotent (upsert), so
  //    re-running /qurl setup overwrites the prior key — the previous
  //    key remains valid on qurl-service until the admin manually
  //    revokes it via layerv.ai.
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
