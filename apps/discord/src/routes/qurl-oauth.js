// qURL OAuth routes — Stage 1 entry (/oauth/qurl/start via /qurl-admin setup)
// and the shared callback (/oauth/qurl/callback) that Stage 2
// (routes/discord-install.js) also chains into. Step-by-step user
// experience and CSRF posture live in the PR #177 description; Stage-2
// confused-deputy mitigation is documented in discord-install.js.
const express = require('express');
const crypto = require('crypto');
const config = require('../config');
const db = require('../store');
const logger = require('../logger');
const { prepareGuildOwnerBinding } = require('../guild-owner');
const { qurlApiErrorStatus } = require('../utils/qurl-errors');
const { AUDIT_EVENTS, SETUP_VIA, GUILD_WEBHOOK_PENDING_NOTICE } = require('../constants');
const { sendDM } = require('../discord');
const {
  verifyQurlOAuthState,
  signQurlOAuthRedirectState,
  fingerprintQurlAccountSubject,
  qurlAccountFingerprintKeyEpoch,
} = require('../utils/qurl-oauth-state');
const { rateLimit } = require('../utils/oauth-rate-limit');
const { verifyAuth0IdToken } = require('../utils/auth0-jwks');
const { readCookie, timingSafeStringEqual } = require('../utils/cookies');
const { createPkcePair, isPkceVerifier } = require('../utils/oauth-pkce');
const {
  buildAuth0AuthorizeUrl,
  qurlOAuthCallbackUrl,
} = require('../utils/auth0-authorize-url');
const { singleStringParam } = require('../utils/query-params');
const {
  renderNotConfiguredPage,
  QURL_SETUP_AUTH_POLICY_REJECTED,
} = require('../utils/oauth-not-configured');
const { linkGuildWebhookSubscriptionForSetup } = require('../guild-webhook-link');

// Network-call timeouts. Centralized so a future tuning of "qurl-service
// is slow under load" doesn't require a hunt-and-replace across both
// route files. 15s matches the existing per-call budget; the 5s and 10s
// values are for cleanup paths that should fail faster.
const AUTH0_TIMEOUT_MS = 15000;
const QURL_SERVICE_TIMEOUT_MS = 15000;
const ORPHAN_DELETE_TIMEOUT_MS = 10000;
// TODO(upstream-contract): keep mint codes/statuses and owner-scoped response replay aligned with qURL.
const KEY_MINT_FAILURES = new Map([
  ['email_verification_required', [403, 'Verify your qURL account email', 'Verify your qURL account email, then start setup again.']],
  ['account_frozen', [403, 'qURL account unavailable', 'Your qURL account is frozen. Contact your layerv.ai admin before trying setup again.']],
  ['scope_delegation_denied', [403, 'qURL permissions unavailable', 'Your account does not permit the required qURL permissions. Ask your layerv.ai admin to allow qURL read and write access.']],
  ['invalid_audience', [401, 'qURL sign-in configuration error', 'The bot operator must correct the Auth0 audience for this qURL endpoint before setup can continue.']],
  ['insufficient_scope', [401, 'qURL sign-in permissions missing', 'Sign in again and grant qURL read and write access. If this repeats, ask the bot operator to check the Auth0 scopes.']],
]);


async function retainSetupCompensation({ guildId, apiKey, keyId, ownerId, uncertain }) {
  if (!config.ENABLE_CREDENTIAL_LIFECYCLE) return false;
  try {
    await db.enqueueUnstoredGuildCredential({ guildId, apiKey, keyId, ownerId, uncertain });
    return true;
  } catch {
    logger.audit(AUDIT_EVENTS.CREDENTIAL_CLEANUP_FAILED, {
      reason: 'setup_compensation_not_saved', guild_id: guildId, key_id: keyId,
    });
    return false;
  }
}

const router = express.Router();

const readyCommandsPlain = config.MAP_COMMAND_ENABLED
  ? '/qurl send and /qurl map are ready.'
  : '/qurl send is ready.';
const readyCommandsMarkdown = config.MAP_COMMAND_ENABLED
  ? '`/qurl send` and `/qurl map`'
  : '`/qurl send`';

// Browser-session cookie binding (parallel to /auth/github's qurl_oauth_session
// pattern). /start sets a HttpOnly cookie holding a random 16-byte token;
// /callback re-checks it. If a leaked /qurl-admin setup ephemeral URL is opened
// in a different browser, the cookie won't match and the callback rejects
// before reaching Auth0 — narrows the leaked-URL window from "the state TTL
// to anyone with a layerv.ai login" to "the state TTL AND the same browser
// that opened the link."
//
// Cookie name + setter shape live in utils/oauth-cookies.js so the
// Stage-2 chain (routes/discord-install.js sets the same cookie before
// chaining to Auth0) can't drift — single source of truth. PR #177
// follow-up C.1.
const {
  QURL_OAUTH_SESSION_COOKIE,
  QURL_OAUTH_PKCE_COOKIE,
  setQurlOAuthCookie,
  setQurlOAuthPkceCookie,
  clearQurlOAuthCookie,
  clearQurlOAuthPkceCookie,
} = require('../utils/oauth-cookies');

// 503 not-configured surface — shared with discord-install.js via
// utils/oauth-not-configured.js (single source of truth for the
// wire-vs-log split per PR #177 / C.4).
function renderNotConfigured(res) {
  return renderNotConfiguredPage(res, 'qurl-setup', config.isAuth0EmailConnectionRejected
    ? QURL_SETUP_AUTH_POLICY_REJECTED
    : 'AUTH0_* unset');
}

// Success page surfaces the guild ID, key prefix, and qURL account email
// as a structured label/value list (renderPage `details`) — the LOAD-
// BEARING visual mitigation against the Stage-2 confused-deputy class:
// if attacker pre-runs Discord install in their browser then forwards
// the chained Auth0-redirect URL to a victim, the readout shows
// (attacker's guild + victim's qURL email) so the victim can spot the
// mismatch before /qurl send or /qurl map usage starts. PR #177 follow-up C.5 —
// upgraded from the prior grey-prose subtext for visual prominence.
function renderSuccess(res, { guildId, keyPrefix, qurlAccountEmail, webhookLinked }) {
  const details = [];
  if (guildId) details.push({ label: 'Discord guild', value: guildId });
  if (qurlAccountEmail) details.push({ label: 'qURL account', value: qurlAccountEmail });
  if (keyPrefix) details.push({ label: 'API key prefix', value: keyPrefix });
  // Cache-Control: no-store is set as a router-level default in
  // server.js for every /oauth/* response — see noStoreHeaders.
  return res.status(200).send(res.renderPage({
    title: 'qURL Connected',
    icon: '✅',
    heading: 'qURL is connected to your Discord server.',
    // CTA folded into message so the "confirm before closing" cue
    // always rides next to the binding readout, including when the
    // qURL-account-email line is missing (JWKS verify failure).
    message: `Confirm the binding below matches your server, then close this tab and return to Discord. ${readyCommandsPlain}${webhookLinked ? '' : ' ' + GUILD_WEBHOOK_PENDING_NOTICE}`,
    details,
    type: 'success',
  }));
}

function renderError(res, statusCode, headline, detail) {
  return res.status(statusCode).send(res.renderPage({
    title: 'qURL Setup Failed',
    icon: '❌',
    heading: headline,
    message: detail + ' Run /qurl-admin setup in Discord to start over.',
    type: 'error',
  }));
}

function clearQurlOAuthCookies(res) {
  clearQurlOAuthCookie(res);
  clearQurlOAuthPkceCookie(res);
}

// /oauth/qurl/start — admin lands here after clicking the link in
// /qurl-admin setup's ephemeral reply. Validate the state, set a session-bound
// cookie carrying the same state, then 302 to Auth0 with the state in
// the URL. The /callback handler re-checks cookie === query.state — a
// classic double-submit CSRF binding. If the link leaks and is opened
// in a different browser, the cookie is absent and /callback rejects
// before any Auth0 token exchange or qurl-service mint runs.
router.get('/start', rateLimit, async (req, res) => {
  if (!config.isQurlSetupAvailable) {
    // Single log line per request lives in renderNotConfiguredPage —
    // dropping the route-level warn (round-9 item #7 harmonization
    // with discord-install.js's surface).
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
  let state = singleStringParam(req.query.state);
  const verified = verifyQurlOAuthState(state);
  if (!verified.ok) {
    logger.warn('qURL OAuth start rejected invalid state', { reason: verified.reason });
    return renderError(res, 400, 'Invalid setup link', 'This setup link is invalid or has expired (links last 15 minutes). Run /qurl-admin setup again, or start again from Add to Discord.');
  }
  // Double-submit CSRF cookie: value is the same state token the URL
  // carries to Auth0. /callback re-checks cookie === query.state.
  // Same-browser flows pass; leaked URLs in other browsers fail.
  // Cookie shape (__Host- prefix, Path=/, HttpOnly, SameSite=Lax,
  // Secure) lives in utils/oauth-cookies.js.
  // Start the authentication clock at this redirect, not when the setup link was made.
  state = signQurlOAuthRedirectState(state);
  if (!state) {
    return renderError(res, 400, 'Invalid setup link', 'This setup link has expired.');
  }
  const redirectState = verifyQurlOAuthState(state);
  if (!redirectState.ok) {
    return renderError(res, 500, 'Could not start qURL setup', 'Please start again from Discord.');
  }
  const { codeVerifier, codeChallenge } = createPkcePair();
  setQurlOAuthCookie(res, req, state);
  setQurlOAuthPkceCookie(res, req, codeVerifier);
  const authorizeUrl = buildAuth0AuthorizeUrl({ state, codeChallenge, nonce: redirectState.payload.nonce });
  return res.redirect(302, authorizeUrl.toString());
});

// /oauth/qurl/callback — Auth0 redirects here after the admin consents.
// Validate state, exchange code → access_token, mint a guild-scoped API
// key on qurl-service, persist it via the Store abstraction, DM the admin.
router.get('/callback', rateLimit, async (req, res) => {
  if (!config.isQurlSetupAvailable) {
    clearQurlOAuthCookies(res);
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
    clearQurlOAuthCookies(res);
    return renderError(res, 503, 'qURL setup not provisioned',
      'The bot operator needs to set KEY_ENCRYPTION_KEY (encryption-at-rest) before qURL setup can store keys safely.');
  }
  // Surface Auth0 error_description verbatim only in logs — render a safe
  // generic message to the browser so an attacker can't smuggle HTML via
  // an error URL.
  // Funnel error params through singleStringParam for symmetry with
  // code/state — array shapes from `?error=a&error=b` log as empty
  // strings rather than stringified arrays. Round-9 item #5.
  const errorParam = singleStringParam(req.query.error);
  if (errorParam) {
    logger.warn('qURL OAuth callback received error from Auth0', {
      error: errorParam,
      errorDescription: singleStringParam(req.query.error_description),
      ip: req.ip,
    });
    clearQurlOAuthCookies(res);
    return renderError(res, 400, 'Authorization declined', 'You declined consent or Auth0 returned an error.');
  }
  const code = singleStringParam(req.query.code);
  const state = singleStringParam(req.query.state);
  if (!code) {
    clearQurlOAuthCookies(res);
    return renderError(res, 400, 'Missing authorization code', 'Auth0 did not return an authorization code.');
  }
  const verified = verifyQurlOAuthState(state);
  if (!verified.ok) {
    logger.warn('qURL OAuth callback rejected invalid state', { reason: verified.reason });
    clearQurlOAuthCookies(res);
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
    clearQurlOAuthCookies(res);
    return renderError(res, 400, 'Invalid setup link', 'Setup must be completed in the same browser tab where /qurl-admin setup was clicked.');
  }
  const cookieMatches = timingSafeStringEqual(cookieState, state);
  if (!cookieMatches) {
    logger.warn('qURL OAuth callback cookie/state mismatch', { ip: req.ip });
    clearQurlOAuthCookies(res);
    return renderError(res, 400, 'Invalid setup link', 'Setup must be completed in the same browser tab where /qurl-admin setup was clicked.');
  }
  const codeVerifier = readCookie(req, QURL_OAUTH_PKCE_COOKIE);
  // Shape check keeps malformed cookies out of Auth0; Auth0's stored
  // challenge/verifier match is the PKCE security boundary. Pre-PKCE
  // in-flight redirects without this cookie fail here; state TTL bounds
  // that deploy cutover to the state TTL.
  if (!isPkceVerifier(codeVerifier)) {
    logger.warn('qURL OAuth callback missing or invalid PKCE verifier cookie', { ip: req.ip });
    clearQurlOAuthCookies(res);
    return renderError(res, 400, 'Invalid setup link', 'This setup link could not be completed.');
  }
  // Cookies are consumed — clear them so a refreshed callback URL can't
  // re-bind or replay the verifier. Path-must-match invariant lives in
  // the clear helpers.
  clearQurlOAuthCookies(res);
  const { guildId, discordUserId, authStartedAt } = verified.payload;
  if (!Number.isSafeInteger(authStartedAt)) {
    return renderError(res, 400, 'Invalid setup link', 'Start a new setup flow to verify a fresh sign-in.');
  }

  // Exchange the code, then verify the identity before any owner or key change.
  let accessToken;
  let qurlAccountEmail;
  let qurlAccountSubject;
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
        // Guaranteed valid by the cookie gate above; Auth0 matches it against
        // the S256 challenge stored for this authorization code.
        code_verifier: codeVerifier,
        redirect_uri: qurlOAuthCallbackUrl(),
      }),
      signal: AbortSignal.timeout(AUTH0_TIMEOUT_MS),
    });
    if (!tokenResp.ok) {
      // Drain for connection reuse; upstream errors can reflect OAuth credentials.
      await tokenResp.text().catch(() => undefined);
      logger.error('Auth0 token exchange failed', { status: tokenResp.status });
      return renderError(res, 502, 'Authorization failed', 'Could not complete the Auth0 handshake. Please run /qurl-admin setup again.');
    }
    const tokenJson = await tokenResp.json();
    accessToken = tokenJson.access_token;
    // Type-narrow before we consume the value: a non-string here would
    // flow into the `Authorization: Bearer ${accessToken}` template
    // literal of the qURL-service mint call (and the orphan-cleanup
    // DELETE), corrupting the upstream auth header. Auth0's spec
    // mandates string, but defense-in-depth — Justin's PR #177 review
    // round 9 item #4.
    if (typeof accessToken !== 'string' || !accessToken) {
      logger.error('Auth0 token response missing or non-string access_token');
      return renderError(res, 502, 'Authorization failed', 'Auth0 returned an unexpected response. Please run /qurl-admin setup again.');
    }
    if (typeof tokenJson.id_token !== 'string' || !tokenJson.id_token) {
      return renderError(res, 403, 'Fresh sign-in required', 'The sign-in did not return a verified identity.');
    }
    const identity = await verifyAuth0IdToken(tokenJson.id_token, {
      nonce: verified.payload.nonce,
      startedAt: authStartedAt,
    });
    if (!identity.ok || typeof identity.payload?.sub !== 'string' || !identity.payload.sub) {
      logger.warn('qURL setup identity verification refused', { guildId, reason: identity.reason });
      return renderError(res, 403, 'Fresh sign-in required',
        'Sign in again with the qURL account that owns this Discord server setup.');
    }
    qurlAccountSubject = identity.payload.sub;
    if (typeof identity.payload.email === 'string') qurlAccountEmail = identity.payload.email;
  } catch (err) {
    logger.error('Auth0 token exchange threw', { error: err?.message });
    return renderError(res, 502, 'Authorization failed', 'A network error occurred during the Auth0 handshake. Please run /qurl-admin setup again.');
  }

  let ownerBinding;
  try {
    ownerBinding = await prepareGuildOwnerBinding(guildId, qurlAccountSubject, discordUserId);
  } catch (err) {
    if (err?.code === 'GUILD_SETUP_FORBIDDEN') {
      return renderError(res, 403, 'Server setup permission required',
        'A current server admin with Manage Server permission must run /qurl-admin setup.');
    }

    const status = qurlApiErrorStatus(err);
    const conflict = err?.code === 'GUILD_OWNER_CONFLICT' || status === 401 || status === 403;
    const log = conflict ? logger.warn : logger.error;
    log('qURL setup owner verification refused', { guildId, code: err?.code, status, errorType: err?.name });
    return conflict
      ? renderError(res, 409, 'Server ownership could not be verified',
        'Use the existing qURL owner account. If its saved key is no longer valid or ownership must change, contact the bot operator.')
      : renderError(res, 503, 'Setup is temporarily unavailable', 'The existing server connection is unchanged. Try again shortly.');
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
        // A fresh grant must not replay a key revoked after a failed save.
        'Idempotency-Key': crypto.createHmac('sha256', verified.payload.nonce).update(code).digest('hex'),
        'Content-Type': 'application/json',
        'Accept': 'application/json',
      },
      body: JSON.stringify({ kind: 'api_key', name: keyName, scopes: ['qurl:write', 'qurl:read'] }),
      signal: AbortSignal.timeout(QURL_SERVICE_TIMEOUT_MS),
    });
    if (!mintResp.ok) {
      const errBody = await mintResp.text().catch(() => '');
      // Parse RFC 7807 problem JSON to discriminate user-quota hits
      // (warn-level, dedicated 429 page) from service failures (error-
      // level, generic 502 page). `<unparseable>` distinguishes "body
      // wasn't JSON at all" from "JSON but no error.code" in logs.
      let problemCode = '';
      try {
        const parsed = JSON.parse(errBody);
        if (typeof parsed?.error?.code === 'string') problemCode = parsed.error.code;
      } catch { problemCode = '<unparseable>'; }

      if (mintResp.status === 403 && problemCode === 'api_key_limit') {
        // logger.warn (not error) so prod alerting can distinguish
        // user-quota hits from qurl-service outages. Status 429
        // matches the actual semantic — qurl-service's 403 is off-
        // spec for a quota hit (RFC 7231 §6.5.3 vs RFC 6585 §4).
        logger.warn('qURL API key mint refused: api_key_limit', {
          status: mintResp.status, problemCode, guildId,
        });
        return renderError(res, 429, 'qURL API key limit reached',
          'Your qURL account has hit its API key limit. Delete an unused key in your layerv.ai dashboard, or upgrade your plan, then run /qurl-admin setup again.');
      }

      const knownFailure = KEY_MINT_FAILURES.get(problemCode);
      if (knownFailure && mintResp.status === knownFailure[0]) {
        logger.warn('qURL API key mint refused', { status: mintResp.status, problemCode, guildId });
        return renderError(res, ...knownFailure);
      }

      logger.error('qURL API key mint failed', {
        status: mintResp.status, problemCode, body: errBody.slice(0, 500), guildId,
      });
      return renderError(res, 502, 'Could not provision qURL key',
        'qurl-service rejected the API-key request. Please run /qurl-admin setup again, or contact your layerv.ai admin.');
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
    // safer to refuse upfront. Strict-string check (not just truthy)
    // so an upstream contract drift to numeric/object can't poison
    // db.setGuildApiKey or the encodeURIComponent in the DELETE URL.
    // PR #177 follow-up C.3 + round-9 review item #4.
    if (typeof apiKey !== 'string' || !apiKey
        || typeof keyId !== 'string' || !keyId) {
      if (typeof apiKey === 'string' && apiKey) {
        await retainSetupCompensation({ guildId, apiKey,
          keyId: typeof keyId === 'string' && keyId.trim() ? keyId : null,
          ownerId: ownerBinding.ownerId, uncertain: true });
      }
      logger.error('qURL API key response missing or non-string required fields', {
        guildId,
        apiKeyType: typeof apiKey,
        keyIdType: typeof keyId,
      });
      return renderError(res, 502, 'Could not provision qURL key',
        'qurl-service returned an unexpected response. Please contact your layerv.ai admin.');
    }
    // key_prefix is informational (used in the success readout + DM); a
    // non-string value from a bad upstream would render as `[object
    // Object]` in HTML. Coerce to undefined so renderSuccess simply
    // skips the row rather than exposing a contract drift.
    if (typeof keyPrefix !== 'string') keyPrefix = undefined;
  } catch (err) {
    logger.error('qURL API key mint threw', { error: err?.message, guildId });
    return renderError(res, 502, 'Could not provision qURL key',
      'A network error occurred while provisioning your qURL key. Please run /qurl-admin setup again.');
  }

  // Retain any replaced credential in the same transaction for durable cleanup.
  try {
    await db.setGuildApiKey(guildId, apiKey, discordUserId, SETUP_VIA.OAUTH, {
      ...ownerBinding, keyId, keyKind: 'dedicated',
    });
  } catch (err) {
    logger.error('Failed to persist guild API key after successful mint', {
      error: err?.message, guildId, discordUserId, keyId,
    });
    const cleanupQueued = await retainSetupCompensation({ guildId, apiKey, keyId, ownerId: ownerBinding.ownerId,
      uncertain: err?.code === 'GUILD_SETUP_UNCONFIRMED' });
    if (err?.code === 'GUILD_SETUP_UNCONFIRMED') {
      return renderError(res, 503, 'Server setup needs confirmation',
        'The key was created, but its saved state could not be confirmed. '
        + 'Contact your layerv.ai admin before starting setup again.');
    }
    // If durable compensation is unavailable, retain the existing known-failure
    // best-effort delete. An uncertain save never reaches this path.
    // Key was minted but not stored. Best-effort delete on qurl-service
    // so retries don't pile up orphan keys under the admin's account.
    // Fire-and-forget — even if the cleanup fails, the user-facing 500
    // response below is the right outcome (admin runs /qurl-admin setup
    // again to retry; the orphan only persists if delete also fails).
    // `keyId` is guaranteed non-empty here — the missing-key_id 502
    // earlier in the handler returned before this block can run, so
    // the historical `if (keyId)` guard was dead code (round-9 #5).
    if (!cleanupQueued) fetch(`${config.QURL_ENDPOINT}/v1/api-keys/${encodeURIComponent(keyId)}`, {
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
    if (err?.code === 'GUILD_OWNER_CONFLICT' || err?.code === 'GUILD_SETUP_CHANGED'
        || err?.name === 'ConditionalCheckFailedException') {
      return renderError(res, 409, 'Server setup changed',
        'Another setup changed this server. Start again with the existing qURL owner account.');
    }
    return renderError(res, 500, 'qURL key provisioned but not stored',
      'Your qURL API key was created but the bot could not save it. Please run /qurl-admin setup again. '
      + 'If this keeps happening, contact your layerv.ai admin.');
  }
  logger.info('qURL OAuth setup complete', {
    guildId, configuredBy: discordUserId, keyPrefix,
  });
  // Audit evidence must never break setup. Compute only after persistence,
  // on the path that emits it. A fingerprint is never emitted without its
  // epoch; an epoch without a fingerprint is expected when no verified Auth0
  // subject was available and is inert for owner comparisons.
  let qurlAccountSubjectFingerprint = null;
  let qurlAccountFingerprintEpoch = null;
  try {
    if (qurlAccountSubject) {
      qurlAccountSubjectFingerprint = fingerprintQurlAccountSubject(
        qurlAccountSubject,
      );
    }
    qurlAccountFingerprintEpoch = qurlAccountFingerprintKeyEpoch();
  } catch (err) {
    qurlAccountSubjectFingerprint = null;
    qurlAccountFingerprintEpoch = null;
    logger.warn('qURL OAuth audit fingerprint unavailable (setup continues)', {
      error: err?.message,
      guildId,
    });
  }
  // Audit fingerprints remain optional; authorization uses the durable owner ID.
  try {
    logger.audit(AUDIT_EVENTS.QURL_GUILD_KEY_CONFIGURED, {
      guild_id: guildId,
      configured_by: discordUserId,
      // Prefix is already shown in the success page and info log; including it
      // makes this forensic event self-contained without exposing the API key.
      key_prefix: keyPrefix || null,
      qurl_account_subject_fingerprint: qurlAccountSubjectFingerprint,
      qurl_account_fingerprint_key_epoch: qurlAccountFingerprintEpoch,
    });
  } catch (err) {
    logger.warn('qURL OAuth audit emission failed (setup continues)', {
      error: err?.message,
      guildId,
    });
  }

  // 3a. Link webhook view counting: reuse the default owner or provision
  //     a BYOK subscription. Bound the page wait; timed-out work stays tracked.
  const webhookLinked = await linkGuildWebhookSubscriptionForSetup({
    guildId, apiKey, via: SETUP_VIA.OAUTH, configuredBy: discordUserId,
  });

  // 4. DM the admin so they have a confirmation that doesn't depend on the
  //    browser tab. Fire-and-forget — a delivery failure shouldn't block
  //    the success page (DMs may be disabled by the admin's privacy
  //    settings, in which case the bot already has the working key).
  //    Include the key prefix so the admin can match it against
  //    `/qurl-admin status` and against the layerv.ai dashboard, which
  //    matters for spotting binding mismatches in the rare confused-
  //    deputy edge case (see PR #177 review item 3).
  const dmLines = [`✅ **qURL is connected to your Discord server.** Your team can now use ${readyCommandsMarkdown}. All usage will be billed to your qURL account.`];
  if (!webhookLinked) dmLines.push(GUILD_WEBHOOK_PENDING_NOTICE);
  if (keyPrefix) dmLines.push(`Key prefix: \`${keyPrefix}\``);
  sendDM(discordUserId, dmLines.join('\n'))
    .catch((err) => logger.warn('Failed to DM admin after qURL setup', { error: err?.message, discordUserId }));

  return renderSuccess(res, { guildId, keyPrefix, qurlAccountEmail, webhookLinked });
});

module.exports = router;
