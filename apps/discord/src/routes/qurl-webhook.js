// qURL webhook receiver — POST /webhooks/qurl
//
// Wire contract (qurl-service/internal/domain/webhook.go::WebhookEvent +
// SignPayload):
//   header  `QURL-Signature`  bare hex HMAC-SHA256 (NO `sha256=` prefix —
//                             that's GitHub's shape)
//   body    {id, type, data:{qurl_id, resource_id, access_count, consumed},
//            owner_id, timestamp, api_version}
//
// `src_ip` and `user_agent` are stripped server-side for type=transit
// resources (connector-owned privacy boundary). Discord bot sends are
// always transit.
//
// Multi-secret routing (BYOK view counter): inbound events carry the
// qurl-service auth0 `owner_id` in the body envelope. The bot looks
// the secret up by owner_id in the in-process subscription registry
// BEFORE HMAC verification. That's safe because the lookup itself
// grants no trust — HMAC verification still gates every accepted
// event. Industry-standard pattern (Stripe, GitHub, Linear all do
// this); the only alternative is making qurl-service include a
// QURL-Webhook-Id header, which is a 2-repo coordination we don't
// need for a problem the bot can solve itself.

const express = require('express');
const db = require('../store');
const config = require('../config');
const logger = require('../logger');
const { AUDIT_EVENTS, QURL_WEBHOOK_EVENTS, DM_STATUS } = require('../constants');
const { createBadSigLimiter, verifyHmacSha256 } = require('../utils/webhook-hardening');
const subs = require('../webhook-subscriptions');
const { editDM, editInteractionReply } = require('../discord-rest');
const { buildExpiredDMPayload, buildConsumedDMPayload } = require('../dm-payloads');
const { renderViewCounter } = require('../view-counter-render');
const { parseExpiryMs } = require('../utils/time');

const router = express.Router();

const SIGNATURE_HEADER = 'qurl-signature';
// Lowercase only by design: Node's crypto.digest('hex') and Go's
// hex.EncodeToString both emit lowercase. A future cross-language
// emitter defaulting to uppercase hex would silently 401 every
// webhook — make the regex flag the regression instead of the /i flag
// silently absorbing it.
const SIGNATURE_PATTERN = /^[0-9a-f]{64}$/;

// Replace control chars (0x00-0x1f + DEL) with '?' so attacker-
// controlled strings can't spoof field separators or inject newlines
// in line-oriented log shippers. The control-regex IS the point here.
// eslint-disable-next-line no-control-regex
const CONTROL_CHARS = /[\x00-\x1f\x7f]/g;
function sanitizeForLog(s) {
  return s.replace(CONTROL_CHARS, '?');
}

const badSigLimiter = createBadSigLimiter({ max: 30, windowMs: 60_000 });
// Looser limiter for OWNER_UNKNOWN — operational drift is the
// expected failure mode (not an attacker brute-forcing HMAC), but
// without ANY ceiling an attacker could flood the receiver with
// signed-shaped requests carrying bogus owner_id and never trip a
// rate limit. Threshold is 5× the HMAC limiter to avoid throttling
// during a real registry-rebuild incident.
//
// CROSS-TENANT FAIRNESS TRADE-OFF: keyed on IP alone. qurl-service
// egresses from a small NAT/proxy pool so a single guild whose
// webhook_owner_id has drifted vs. qurl-service's signing identity
// can burn 150/min on the shared source IP and 429 other guilds'
// legitimate events. Acceptable at low BYOK count today (≤10 in
// prod); at scale this should key on (ip, owner_id) or skip per-IP
// limiting for OWNER_UNKNOWN entirely.
const unknownOwnerLimiter = createBadSigLimiter({ max: 150, windowMs: 60_000 });

// Reason codes returned from verifyAndResolve. Caller maps them to
// HTTP status + audit event + rate-limiter interaction in one place
// so the policy table stays grep-able. Frozen for parity with
// LINK_RESULTS — typo in a string value would silently break the
// downstream switch.
const VERIFY_RESULTS = Object.freeze({
  OK: 'ok',
  RAW_BODY_MISSING: 'raw_body_missing',         // middleware bug; should never happen
  SIG_HEADER_MISSING: 'sig_header_missing',
  SIG_HEADER_MALFORMED: 'sig_header_malformed',
  OWNER_ID_MISSING: 'owner_id_missing',         // parse failed OR field absent
  CACHE_UNPRIMED: 'cache_unprimed',             // 503 — registry hasn't completed first scan
  OWNER_UNKNOWN: 'owner_unknown',               // 401 — registry primed, owner not registered
  SIG_INVALID: 'sig_invalid',                   // HMAC mismatch on real secret
});

function verifyAndResolve(req) {
  if (!req.rawBody) {
    logger.error('qURL webhook middleware did not populate rawBody — check server.js middleware ordering. Signature verification is BLOCKED until fixed.');
    return { result: VERIFY_RESULTS.RAW_BODY_MISSING };
  }

  const signature = req.headers[SIGNATURE_HEADER];
  if (!signature) {
    logger.warn('qURL webhook missing QURL-Signature header');
    return { result: VERIFY_RESULTS.SIG_HEADER_MISSING };
  }
  if (typeof signature !== 'string' || !SIGNATURE_PATTERN.test(signature)) {
    logger.warn('qURL webhook signature has unexpected format', {
      length: typeof signature === 'string' ? signature.length : 0,
    });
    return { result: VERIFY_RESULTS.SIG_HEADER_MALFORMED };
  }

  // SECURITY: pre-HMAC parse depth/size MUST stay bounded by the
  // rawBodyJson middleware cap (server.js, `limit: '1mb'`). Loosening
  // that cap or adding extended type coercion here widens the
  // pre-trust window — this parsed body is attacker-controlled JSON
  // until the HMAC check below succeeds.
  // The lookup itself grants no trust: a forged owner_id either
  // misses the cache (→ 401) or hits a real secret the attacker
  // can't forge against (→ 401).
  // Under today's express.json wiring, malformed JSON is rejected
  // before this route runs; this catch is forward defense for raw-body
  // parser refactors and parser-differential cases.
  // Deliberately do not strip a UTF-8 BOM: qurl-service's Go JSON
  // encoder never emits one, and a BOM-prefixed request is outside the
  // signed wire shape we accept.
  // rawBody.toString() intentionally uses Node's UTF-8 default; other
  // charsets are outside qurl-service's webhook contract.
  let body;
  try {
    body = JSON.parse(req.rawBody.toString());
  } catch (err) {
    logger.warn('qURL webhook raw body JSON parse failed', { error: err.message });
    return { result: VERIFY_RESULTS.OWNER_ID_MISSING };
  }
  const ownerId = body && typeof body.owner_id === 'string' ? body.owner_id : null;
  if (!ownerId) {
    logger.warn('qURL webhook raw body missing or non-string owner_id');
    return { result: VERIFY_RESULTS.OWNER_ID_MISSING };
  }

  const secret = subs.getSecretForOwner(ownerId);
  if (!secret) {
    if (!subs.isPrimed()) return { result: VERIFY_RESULTS.CACHE_UNPRIMED, ownerId };
    // Sibling-replica eventual-consistency lag: a freshly-linked
    // guild's row is in DDB but THIS replica's last scan hasn't
    // picked it up yet (linking-replica's upsertGuild is local-only;
    // siblings catch up on the 30s tick). Treat as transient (503,
    // retriable) until enough time has passed for at least two scan
    // cycles to have seen the new owner — after that, 401 is the
    // truthful response.
    if (subs.isWithinSiblingLagWindow()) {
      return { result: VERIFY_RESULTS.CACHE_UNPRIMED, ownerId };
    }
    return { result: VERIFY_RESULTS.OWNER_UNKNOWN, ownerId };
  }

  if (!verifyHmacSha256(req.rawBody, secret, signature)) {
    return { result: VERIFY_RESULTS.SIG_INVALID, ownerId };
  }
  return { result: VERIFY_RESULTS.OK, ownerId, body };
}

// Reconstruct the absolute expiry instant from the recipient row's
// `created_at` (ISO-8601) + `expires_in` (the symbolic label, one of
// EXPIRY_LABELS keys like '24h'). Returns UNIX seconds, or null if
// either field is missing/unparseable.
//
// Why reconstruct rather than read from the wire payload: the
// qurl.expired event's `data` is exactly `{qurl_id, resource_id}` —
// transit-safe by design, so target-side fields like an absolute
// `expires_at` aren't carried on the wire. The row already carries
// everything needed to render the marker.
//
// Drift direction is consistently FORWARD (reconstructed > true): the
// bot computes the true `expiresAt` pre-mint and `created_at` is
// stamped post-mint, so `created_at + expires_in` always lands later
// than the upstream-enforced expiry by the mint round-trip duration.
// Magnitude: sub-second on small sends, single-digit seconds on
// large multi-batch mints (each batch is its own network round-trip).
// At the firing edge this can briefly render "expired in 3 seconds"
// before Discord re-evaluates the `<t:N:R>` marker forward across
// the boundary. Bounded and cosmetic — both the magnitude and the
// brief future-render window are well inside the rounding window of
// a relative-time render at the 30m–7d horizon.
function rowExpiresAtSeconds(row) {
  if (typeof row?.created_at !== 'string' || typeof row?.expires_in !== 'string') return null;
  const createdMs = Date.parse(row.created_at);
  if (!Number.isFinite(createdMs)) return null;
  // parseExpiryMs (NOT expiryToMs) — expiryToMs silently falls back to
  // 24h on any unparseable input, so an off-set string like
  // 'garbage' / '99x' would render a wrong-time marker instead of
  // taking the cannot-reconstruct-expiry skip below.
  //
  // Latent edge: parseExpiryMs caps at MAX_EXPIRY_MS (30d). Today the
  // write path only writes EXPIRY_LABELS keys (max 7d), so an over-
  // cap value is unreachable. If EXPIRY_LABELS ever grows past 30d,
  // an in-set-but-over-cap value would produce a wrong-time marker
  // (capped instead of skipped). Re-evaluate at that label-set
  // expansion.
  const ms = parseExpiryMs(row.expires_in);
  if (ms === null) return null;
  return Math.floor((createdMs + ms) / 1000);
}

// Shared recipient-DM tense-flip core for both close-the-door webhook
// paths (qurl.expired and qurl.accessed-with-consumed). The flip
// sequence is identical — GSI lookup of the recipient row, editability
// gate, sibling-marker cross-check (don't clobber the OTHER path's more-
// specific copy), revoke guard, idempotency-marker claim, editDM, and
// marker rollback on transient failure — only the marker attribute, the
// payload, and the log label differ. This helper RETURNS a verdict and
// never touches `res`: each caller owns its HTTP-status mapping (the
// expired path maps to 200/503 so the upstream's webhook retry can
// recover; the consumed path runs fire-and-forget off the already-sent
// 200 and ignores the verdict beyond logging).
//
// Verdict shape: { status, transient }. `transient: true` is the only
// retry-worthy outcome (PRE-marker GSI/mark throw, or POST-marker
// transient editDM failure WITH a successful marker rollback). Every
// other status is a permanent can't-or-needn't-action condition.
//
// Options:
//   - opts.label         human log label (e.g. 'qurl.expired')
//   - opts.skipIfAttr    sibling marker attribute name to honor; if the
//                        row already carries it, the OTHER path flipped
//                        the DM first — skip so we don't overwrite its
//                        more-specific copy. (expired skips when
//                        consumed_edited_at is set, and vice versa.)
//   - opts.buildPayload(row) → payload | null. null means "cannot
//                        render for this row" (e.g. expiry couldn't be
//                        reconstructed); the helper skips the edit.
//   - opts.markEdited / opts.clearEdited  the path's own idempotency
//                        marker claim + rollback.
async function flipRecipientDMToClosed({ qurlId, eventId, label, skipIfAttr, buildPayload, markEdited, clearEdited }) {
  let rows;
  try {
    rows = await db.findSendsByQurlId(qurlId);
  } catch (err) {
    // Pre-marker transient: the marker hasn't been claimed yet, so a
    // retry (the upstream's for the expired path, a redelivery for the
    // consumed path) can re-enter cleanly. DDB throttle / transient
    // AWS-side 5xx is exactly the recoverable case.
    logger.error(`qURL webhook ${label}: GSI lookup failed`, {
      error: err.message, qurl_id: qurlId, event_id: eventId,
    });
    return { status: 'lookup-error', transient: true };
  }

  if (rows.length === 0) {
    // Sparse-GSI miss. Two distinct causes, both end here:
    //   - Pre-rollout: qURL minted before this handler shipped, so
    //     the row has no qurl_id attribute and isn't in the GSI.
    //   - Permanent: a send where the upstream didn't surface a
    //     qurl_id at mint time (mintLinksInBatches stores `qurl_id
    //     || ''`, and the sparse-write gate omits empty values).
    //     Theoretical today — the view-counter path already depends
    //     on qurl_id presence — but a regression there would
    //     silently land here forever, not just during the rollout
    //     window. Forward-only behavior is intentional either way.
    //
    // GSI eventual consistency is a non-issue: the EXPIRY_LABELS
    // floor is 30m (utils/time.js), orders of magnitude larger than
    // DDB GSI propagation (sub-second in normal operation), so a
    // Count=0 miss can never be a not-yet-propagated row — every
    // mint that qualifies for expiry has had time to land in the
    // GSI hundreds of times over.
    logger.debug(`qURL webhook ${label}: no recipient row for qurl_id (pre-rollout or missing-from-mint)`, {
      qurl_id: qurlId, event_id: eventId,
    });
    return { status: 'no-recipient-row' };
  }
  if (rows.length > 1) {
    // DDB doesn't enforce GSI hash-key uniqueness; the write-path
    // invariant should keep this at 1. Log + skip rather than blind-
    // index [0] so the regression is greppable and we don't edit a
    // wrong recipient's DM.
    logger.warn(`qURL webhook ${label}: GSI returned multiple rows for one qurl_id`, {
      qurl_id: qurlId, count: rows.length, event_id: eventId,
    });
    return { status: 'ambiguous-recipient' };
  }

  const row = rows[0];
  const { send_id: sendId, recipient_discord_id: recipientId, dm_channel_id: channelId, dm_message_id: messageId, dm_status: dmStatus } = row;

  // DM never made it — no message exists to PATCH. Same skip set the
  // revoke editTargets loop uses (commands.js:7251).
  if (dmStatus !== DM_STATUS.SENT || !channelId || !messageId) {
    logger.debug(`qURL webhook ${label}: row not editable`, {
      qurl_id: qurlId, send_id: sendId, dm_status: dmStatus, event_id: eventId,
    });
    return { status: 'dm-not-editable' };
  }

  // Sibling-path cross-check: the OTHER close path already flipped this
  // DM to its (more-specific) copy. The consumed copy ("you opened it")
  // and the expired marker ("expired N ago") describe the same dead
  // door, but whichever landed first is the truthful one for the
  // recipient — re-editing would overwrite it with a redundant or
  // less-accurate message. `findSendsByQurlId` projects ALL (the
  // qurl_id-index GSI, see ddb-store.js), so the sibling marker is
  // already on `row` (zero extra read) — same pre-existing dependency
  // the expired path leans on for created_at/dm_status. If that GSI's
  // projection ever narrows to KEYS_ONLY/INCLUDE, this cross-check
  // silently always-misses and starts clobbering the sibling copy. Read
  // straight off the row rather than the marker function so a redelivery
  // sees a marker the local UpdateItem already wrote.
  //
  // TOCTOU window (same shape as the revoke race below): if the
  // consumed and expired events run their GSI lookup before EITHER
  // marker is written, both pass this check, both claim their own
  // (distinct) marker, and both editDM — last writer wins. End state is
  // still "closed" either way, just possibly the less-specific copy.
  // Vanishingly rare here: a one-time link is consumed well before its
  // 30m TTL fires qurl.expired, so the two events are almost never
  // in-flight together. (A single shared marker would serialize the two
  // paths at the conditional write and close this, but that's a schema
  // migration of the pre-existing expired_edited_at attribute; not worth
  // it for a race whose worst case is the less-specific copy.)
  if (skipIfAttr && row[skipIfAttr]) {
    logger.debug(`qURL webhook ${label}: sibling path already flipped the DM; skipping`, {
      qurl_id: qurlId, send_id: sendId, skip_attr: skipIfAttr, event_id: eventId,
    });
    return { status: 'sibling-already-flipped' };
  }

  // buildPayload is the one synchronous caller-supplied step in this
  // helper. Guard it so a throw becomes a permanent skip rather than
  // rejecting up to the caller — on the expired path handleQurlExpired
  // awaits this OUTSIDE its try (the route's try wraps recordQurlView,
  // not the expired delegation), so an unguarded throw here would escape
  // into Express v4, which doesn't catch async rejections. The consumed
  // path is already insulated by flipConsumedDMInBackground's .catch;
  // guarding here covers both callers at the seam rather than per-caller.
  // Realistically defensive — the expired builder validates and returns
  // null (not throws) on bad input, and the consumed builder is static.
  let payload;
  try {
    payload = buildPayload(row);
  } catch (err) {
    logger.error(`qURL webhook ${label}: buildPayload threw — skipping`, {
      error: err.message, qurl_id: qurlId, send_id: sendId, event_id: eventId,
    });
    return { status: 'payload-build-error' };
  }
  if (!payload) {
    // The caller's renderer declined (e.g. the expired path couldn't
    // reconstruct expires_at from a corrupt row). Skip rather than ship
    // a malformed/wrong-time marker. Checked BEFORE the marker claim so
    // a future repair of the row lets the next event render cleanly.
    logger.warn(`qURL webhook ${label}: cannot build DM payload for row`, {
      qurl_id: qurlId, send_id: sendId,
      created_at: row.created_at, expires_in: row.expires_in,
      event_id: eventId,
    });
    return { status: 'cannot-reconstruct-expiry' };
  }

  // Sender revoked first — the DM already says "Alice closed the door".
  // Re-editing would overwrite the more specific revoke copy.
  //
  // TOCTOU race window: between `isSendRevoked` returning false and
  // `editDM` landing, a concurrent /qurl revoke could PATCH the DM
  // to "closed the door" and we'd then clobber it. Window is tens of ms
  // (one DDB GetItem + one Discord PATCH). The resulting state is still
  // correct ("closed"), just less specific than the revoke copy. Out-of-
  // scope to close — pessimistically locking would need a leader
  // election the bot doesn't have today.
  try {
    if (await db.isSendRevoked(sendId)) {
      logger.debug(`qURL webhook ${label}: send was revoked; skipping DM edit`, {
        qurl_id: qurlId, send_id: sendId, event_id: eventId,
      });
      return { status: 'send-revoked' };
    }
  } catch (err) {
    // 5xx-style here would tempt the upstream to retry; the revoked-
    // check is best-effort. Logging + continuing is preferred to
    // dead-lettering the event over a transient GetItem failure.
    //
    // Failure-mode asymmetry: if the send WAS revoked and isSendRevoked
    // throws transiently, continuing claims the marker, edits the DM,
    // and the revoke copy is permanently lost. End state is still
    // correct ("closed"), just less specific. Rare, and the alternative
    // (dead-letter over a GetItem blip) is strictly worse.
    logger.warn(`qURL webhook ${label}: revoke-check failed; continuing`, {
      error: err.message, qurl_id: qurlId, send_id: sendId, event_id: eventId,
    });
  }

  // Idempotency marker — first call wins, repeats short-circuit. Each
  // path has its OWN marker (expired_edited_at / consumed_edited_at) so
  // a redelivery / dual-emission of the same event re-enters and the
  // conditional UpdateItem short-circuits the second.
  //
  // At-most-once gap (claim-before-act): if the process dies between
  // the marker write and the edit attempt, the rollback below never
  // runs. A retry then short-circuits at `already-edited` and the edit
  // is lost. For the consumed path this also suppresses the qurl.expired
  // backstop (the marker IS set, so the expired handler's
  // sibling-already-flipped skip honors it), so the DM never flips at
  // all in that narrow window. Bounded by the 8-day S3 lifecycle, same
  // as the expired path's own gap — and far rarer than a clean failure,
  // which DOES roll back.
  let claimed;
  try {
    claimed = await markEdited(sendId, recipientId);
  } catch (err) {
    // Pre-marker transient: UpdateItem threw before the write landed
    // for a non-CCFE reason (e.g. ProvisionedThroughput exceeded), so a
    // retry can re-enter cleanly. CCFE ("already edited") is NOT routed
    // here — markEdited returns false on CCFE, handled as already-edited
    // below.
    logger.error(`qURL webhook ${label}: marker claim failed`, {
      error: err.message, qurl_id: qurlId, send_id: sendId, event_id: eventId,
    });
    return { status: 'mark-error', transient: true };
  }
  if (!claimed) {
    logger.debug(`qURL webhook ${label}: already edited`, {
      qurl_id: qurlId, send_id: sendId, event_id: eventId,
    });
    return { status: 'already-edited' };
  }

  // editDM returns {ok, expected} on failure and {ok: true} on success.
  //   - ok:true                          → edited (success)
  //   - ok:false, expected:true          → edit-failed-expected
  //         (permanent — recipient blocked the bot / deleted the DM;
  //          a retry can't recover, so keep the marker)
  //   - ok:false, expected:false / throw → edit-failed-transient
  //         (transient — Discord 5xx or network; roll back the marker so
  //          a retry can recover the edit on the next attempt)
  //
  // Marker rollback is best-effort: if clearEdited itself throws, the
  // marker stays, the next retry short-circuits at `already-edited`, and
  // the missed edit falls back to the 8-day S3 lifecycle.
  let editRes;
  try {
    editRes = await editDM(channelId, messageId, payload);
  } catch (err) {
    editRes = { ok: false, expected: false, threwErr: err };
  }
  if (editRes.ok) {
    logger.info(`qURL webhook ${label}: DM edited`, {
      qurl_id: qurlId, send_id: sendId, ok: true, event_id: eventId,
    });
    return { status: 'edited' };
  }
  if (editRes.expected) {
    logger.info(`qURL webhook ${label}: DM edit failed-expected`, {
      qurl_id: qurlId, send_id: sendId, ok: false, expected: true, event_id: eventId,
    });
    return { status: 'edit-failed-expected' };
  }
  // Transient — roll back the marker so a retry can recover.
  const transientLog = {
    qurl_id: qurlId, send_id: sendId, ok: false, expected: false, event_id: eventId,
  };
  if (editRes.threwErr) transientLog.error = editRes.threwErr.message;
  logger.warn(`qURL webhook ${label}: transient editDM failure — rolling back marker for retry`, transientLog);
  try {
    await clearEdited(sendId, recipientId);
  } catch (rollbackErr) {
    // Marker rollback failed → next retry will short-circuit at
    // `already-edited`. Edit is permanently missed; 8-day S3 lifecycle
    // bounds the blast radius. Report non-transient so the caller's
    // retry backoff doesn't loop on the doomed event.
    logger.error(`qURL webhook ${label}: marker rollback failed — missed edit`, {
      qurl_id: qurlId, send_id: sendId, event_id: eventId,
      error: rollbackErr.message,
    });
    return { status: 'edit-failed-rollback-failed' };
  }
  return { status: 'edit-failed-transient', transient: true };
}

// qurl.expired handler — the upstream fires this when a qURL reaches
// expires_at (regardless of prior revoke/consume state). The bot looks
// the recipient row up via the qurl_id-index GSI and PATCHes the DM body
// so Discord's <t:UNIX:R> relative-time marker flips tense from
// "Closes in N" to "Closed N ago" without further edits.
//
// Status policy:
//   - 200 on permanent can't-action conditions (no matching row, send
//     was revoked first, consumed-flip already closed the DM, DM was
//     already edited, dm_status not SENT, hard shape-mismatch, recipient
//     blocked the bot / deleted the DM, marker-rollback failed).
//   - 503 on transient infra failures that a retry can recover from
//     (verdict.transient — pre-marker throws, or a transient editDM
//     failure with a successful marker rollback). 503 trips the
//     upstream's 5-attempt retry (1+2+4+8+16=31s backoff).
async function handleQurlExpired(req, res, { data, eventId }) {
  if (!data || typeof data.qurl_id !== 'string' || !data.qurl_id) {
    logger.warn('qURL webhook qurl.expired missing qurl_id', {
      qurl_id: data?.qurl_id, resource_id: data?.resource_id, event_id: eventId,
    });
    return res.status(200).json({ status: 'invalid-payload' });
  }

  const verdict = await flipRecipientDMToClosed({
    qurlId: data.qurl_id,
    eventId,
    label: 'qurl.expired',
    // Don't overwrite the consumed-flip copy ("you opened it") with a
    // less-accurate "expired N ago" once a one-time link was consumed.
    skipIfAttr: 'consumed_edited_at',
    // Reconstruct the absolute expiry instant from the row and render
    // the <t:N:R> marker. Returns null (→ cannot-reconstruct-expiry) on
    // a corrupt/off-set row. buildExpiredDMPayload re-validates the same
    // way, so a null from it after a finite reconstruction would mean
    // the two validators drifted — handled by the null-skip in the
    // helper, same as a failed reconstruction.
    buildPayload: (row) => {
      const expiresAtSeconds = rowExpiresAtSeconds(row);
      if (expiresAtSeconds === null) return null;
      return buildExpiredDMPayload({ expiresAtSeconds });
    },
    markEdited: db.markExpiredDMEdited,
    clearEdited: db.clearExpiredDMEdited,
  });

  return res.status(verdict.transient ? 503 : 200).json({ status: verdict.status });
}

// Consumed-flip for the qurl.accessed path: when a recipient opens a
// ONE-TIME qURL and consumes it (`data.consumed === true`), the link is
// dead for them even though its 30m TTL hasn't elapsed. Flip their DM
// from the present-tense "🕐 Closes <t:...:R>" embed to the past-tense
// "🔒 You opened this one-time qURL … no longer active" copy so they
// don't see "Closes in ~25m" on a link they can no longer reach.
//
// FIRE-AND-FORGET by design (mirrors the view-counter publish above, NOT
// handleQurlExpired's response coupling): the view is already recorded
// and the 200 already returned to qurl-service. Surfacing a flip failure
// as a 503 would lie about the primary op (the view DID record) and make
// qurl-service retry the whole accessed event. The flip's own backstops
// make blocking the response unnecessary:
//   - a REDELIVERED qurl.accessed re-enters here (we gate on
//     `consumed === true`, not `dbResult === 'recorded'`, and the
//     consumed_edited_at marker short-circuits the redundant edit); and
//   - the eventual qurl.expired event (which fires regardless of prior
//     consume state — see the EventQurlExpired contract) is the
//     last-resort flip, skipping only when consumed_edited_at is set.
// So a transiently-failed flip that rolls its marker back is recovered
// with the consumed copy on the next delivery. The one degraded case is
// a process bounce (deploy/crash) after the 200 but before the deferred
// flip runs: the consumed marker was never claimed, so the qurl.expired
// backstop is what eventually flips the DM — to the less-specific
// "expired" copy, not "you opened it". Self-heals to a correct "closed"
// state either way; only the copy specificity degrades, bounded by the
// 30m TTL.
function flipConsumedDMInBackground({ qurlId, eventId }) {
  // Defer into the microtask queue so a future sync-throw refactor of
  // the flip surfaces as a rejection instead of escaping the handler.
  Promise.resolve()
    .then(() => flipRecipientDMToClosed({
      qurlId,
      eventId,
      label: 'qurl.accessed-consumed',
      // Don't overwrite a sender-revoke or expired flip already on the DM.
      skipIfAttr: 'expired_edited_at',
      // Static past-tense copy — no expiry marker (see buildConsumedDMPayload for why).
      buildPayload: () => buildConsumedDMPayload(),
      markEdited: db.markConsumedDMEdited,
      clearEdited: db.clearConsumedDMEdited,
    }))
    .then((verdict) => {
      // Verdict is for observability only on this path — there's no
      // HTTP response left to map it to. The redelivery + expired
      // backstop recover anything `transient`.
      logger.debug('qURL webhook qurl.accessed-consumed: flip verdict', {
        qurl_id: qurlId, event_id: eventId, status: verdict.status, transient: Boolean(verdict.transient),
      });
    })
    .catch((err) => {
      logger.error('qURL webhook qurl.accessed-consumed: flip threw', {
        error: err?.message, qurl_id: qurlId, event_id: eventId,
      });
    });
}

// Terminal verdict log message for the sender view-counter fast-path.
// Mirrors flipConsumedDMInBackground's verdict line — the single
// guaranteed end signal on EVERY branch (skip, edited, edit-failed) so a
// fire-and-forget caller and the unit tests have a uniform drain anchor.
const COUNTER_VERDICT_MSG = 'qURL webhook sender-counter: fast-path verdict';
const senderCounterFlushTimers = new Map();

// Sub-second sender view-counter fast-path (feat #60, PR-B). On a real
// new view (dbResult === 'recorded'), edits the sender's "/qurl send"
// confirmation to "👀 N viewed / M pending" from ANY replica using the
// persisted interaction-webhook token — no Gateway, no in-memory monitor
// needed. (Supersedes the old SQS view-update push — see the call site's
// SUPERSESSION note for why the two editors can't coexist.)
//
// FIRE-AND-FORGET (mirrors flipConsumedDMInBackground): the view is
// already recorded and the 200 already returned, so a counter-edit miss
// must not make qurl-service retry the whole accessed event. Everything
// runs inside the deferred chain and a .catch keeps a throw out of
// Express.
//
// ORDERING IS LOAD-BEARING — DO NOT REORDER. The advance-the-count step
// (8) commits ONLY after a confirmed edit (7). Advancing before the edit
// would re-introduce the stuck-counter regression: on a transient edit
// failure last_rendered_count would move forward without the display
// moving, and the poll backstop's pre-read compare would then skip the
// re-render forever. Each step's skip is intentional defense — see the
// inline notes.
//
function scheduleSenderCounterFlush({ sendId, qurlId, delayMs, repairFloor = false }) {
  const existing = senderCounterFlushTimers.get(sendId);
  if (existing) clearTimeout(existing);
  const timer = setTimeout(() => {
    senderCounterFlushTimers.delete(sendId);
    editSenderCounterInBackground({ qurlId, force: true, repairFloor });
  }, Math.max(0, delayMs));
  if (typeof timer.unref === 'function') timer.unref();
  senderCounterFlushTimers.set(sendId, timer);
}

// COALESCING (step 4b): leading-edge debounce per send. It bounds
// Discord edits; distinct first-view aggregate writes are sharded across
// qurl_views counter rows so high-fanout sends avoid a single hot DDB
// item. If a shard write or shard sum fails, the poll reader is still the
// correctness backstop.
// Multiple replicas can still each pass a stale/unwritten last_rendered_at
// and PATCH once; the CAS keeps count monotonic, while discord.js 429
// backoff is the final safety net.
//
// SECURITY: state.interactionToken is a live bearer cred — NEVER log it.
// Only sendId / qurlId / counts appear in the verdict log.
function editSenderCounterInBackground({
  qurlId, firstView = false, force = false, repairFloor = false,
}) {
  // Defer into the microtask queue so a future sync-throw refactor
  // surfaces as a rejection instead of escaping the handler (same shape
  // as flipConsumedDMInBackground).
  Promise.resolve()
    .then(async () => {
      // 1. GSI lookup → the single send owning this qurl_id. Defensive
      //    skip on != 1 (0 = pre-rollout / no persisted send; > 1 =
      //    ambiguous duplicate key) — same shape as the expired handler.
      const rows = await db.findSendsByQurlId(qurlId);
      if (rows.length !== 1) {
        logger.debug('qURL webhook sender-counter: skip — row count != 1', {
          qurl_id: qurlId, count: rows.length,
        });
        return { status: 'no-single-send' };
      }
      const sendId = rows[0].send_id;
      const senderId = rows[0].sender_discord_id;

      // 2. Render state for this send. Absent → no persisted confirm row
      //    (legacy send predating the feature, or a saveSendConfirmState
      //    that failed); the poll backstop is the sole renderer.
      const state = await db.getSendRenderState(sendId);
      if (!state) {
        logger.debug('qURL webhook sender-counter: skip — no render state', { qurl_id: qurlId, send_id: sendId });
        return { status: 'no-state' };
      }

      // 3. TERMINAL CHECK FIRST. A revoked / window-closed / otherwise
      //    frozen confirmation must NOT be resurrected by a late view
      //    edit — checked before anything else so we never even read the
      //    send items for a dead display.
      if (state.terminal) {
        logger.debug('qURL webhook sender-counter: skip — terminal', { qurl_id: qurlId, send_id: sendId });
        return { status: 'terminal' };
      }

      // 4. ABSENT-GUARD. No token / app id / base means this send predates
      //    the persistence window (or the token TTL'd away) — the
      //    fast-path has nothing to edit with, so the poll backstop
      //    covers it. (PR-A maps confirm_base_msg → state.baseMsg.)
      if (!state.interactionToken || !state.interactionAppId || typeof state.baseMsg !== 'string') {
        logger.debug('qURL webhook sender-counter: skip — render state absent/partial (legacy or pre-TTL)', { qurl_id: qurlId, send_id: sendId });
        return { status: 'absent' };
      }

      if (firstView) {
        try {
          await db.incrementSendViewedCount(sendId, qurlId);
        } catch (err) {
          logger.warn('qURL webhook sender-counter: sharded aggregate increment failed; poll backstop will count qurl views', {
            qurl_id: qurlId, send_id: sendId, error: err?.message,
          });
          return { status: 'aggregate-update-error' };
        }
      }

      // 4b. COALESCE (leading-edge debounce). A high-fan-out send (cap
      //    QURL_SEND_MAX_RECIPIENTS, default 20000) fires one
      //    qurl.accessed per recipient's first view; un-coalesced each
      //    would PATCH this confirmation. Skip the edit if the last
      //    CONFIRMED edit is younger than the cooldown. Placed before the
      //    legacy getQurlViews fallback and before the fixed-size shard
      //    sum so normal coalesced events stay constant-shape: a first
      //    distinct view already advanced its shard above, and the
      //    delayed flush reads the aggregate. Leading-edge keeps the
      //    counter visibly live; the trailing flush renders rapid
      //    followers inside the same sub-second window.
      const sinceLastEditMs = Date.now() - state.lastRenderedAt;
      if (!force && sinceLastEditMs < config.QURL_VIEW_COUNTER_COALESCE_MS) {
        const pendingCount = firstView
          ? state.lastRenderedCount + 1
          : (typeof state.viewedCount === 'number' ? state.viewedCount : state.lastRenderedCount);
        if (pendingCount > state.lastRenderedCount) {
          const delayMs = config.QURL_VIEW_COUNTER_COALESCE_MS - sinceLastEditMs;
          scheduleSenderCounterFlush({ sendId, qurlId, delayMs });
          logger.debug('qURL webhook sender-counter: coalesced — scheduled trailing flush', {
            qurl_id: qurlId, send_id: sendId, since_last_edit_ms: sinceLastEditMs, delay_ms: delayMs,
          });
        } else {
          logger.debug('qURL webhook sender-counter: skip — within coalesce window (no distinct advance)', {
            qurl_id: qurlId, send_id: sendId, since_last_edit_ms: sinceLastEditMs,
          });
        }
        return { status: 'coalesced' };
      }

      // 5. Compute N — the count of DISTINCT viewed qurl_ids across the
      //    WHOLE send (NOT this event's per-qurl access_count). New rows
      //    render from a fixed 64-shard counter sum in qurl_views, advanced
      //    only when recordQurlView proves this qurl_id's first recorded
      //    view. That keeps a 20k-recipient burst sub-second without
      //    funnelling every first-view write into one qurl_send_configs
      //    item. The old BatchGet-all-qurl_ids path is now a legacy
      //    fallback for live rows created before the aggregate existed.
      let N = null;
      try {
        const shardedCount = await db.getSendViewedCount(sendId);
        const legacyFloor = typeof state.viewedCount === 'number' ? state.viewedCount : 0;
        N = Math.max(shardedCount, legacyFloor);
      } catch (err) {
        logger.warn('qURL webhook sender-counter: sharded aggregate read failed; falling back to qurl views', {
          qurl_id: qurlId, send_id: sendId, error: err?.message,
        });
      }
      if (typeof N !== 'number' || (N === 0 && typeof state.viewedCount !== 'number')) {
        let qurlIds;
        if (state.qurlIds.length > 0) {
          qurlIds = state.qurlIds;
        } else if (!senderId) {
          logger.debug('qURL webhook sender-counter: skip — no aggregate/qurlIds and senderId missing (GSI projection narrowed?)', { qurl_id: qurlId, send_id: sendId });
          return { status: 'no-count-source' };
        } else {
          qurlIds = (await db.getSendItems(sendId, senderId)).map(i => i.qurl_id).filter(Boolean);
        }
        const views = await db.getQurlViews(qurlIds);
        N = qurlIds.filter(id => views.get(id)?.accessCount > 0).length;
      }

      // 6. PRE-READ COMPARE (advisory dedup — NOT a commit). If N is no
      //    higher than the last count we confirmed-rendered, skip: this is
      //    a redelivery, or a concurrent multi-recipient view already
      //    rendered an equal-or-higher count. Prevents a backwards flicker
      //    without a write; the authoritative monotonic guard is the CAS
      //    in step 8.
      const L = state.lastRenderedCount;
      if (N <= L && !(repairFloor && N === L && N > 0)) {
        logger.debug('qURL webhook sender-counter: skip — N <= last rendered (redelivery / already-rendered)', {
          qurl_id: qurlId, send_id: sendId, n: N, last_rendered: L,
        });
        return { status: 'no-advance', n: N };
      }

      // 7. Render the SAME pure body the monitor renders (byte-identical),
      //    then edit CONTENT ONLY. Per the verified Discord API behavior,
      //    PATCH .../messages/@original is a PARTIAL update: OMITTING
      //    `components` PRESERVES the existing Add/Revoke buttons. So we
      //    send `{content}` alone — NO components key — and the buttons
      //    stay. (Sending components:[] would clear them.)
      const content = renderViewCounter({
        baseMsg: state.baseMsg,
        viewed: N,
        expectedCount: state.expectedCount,
        degraded: false,
      });
      const r = await editInteractionReply(state.interactionAppId, state.interactionToken, { content });

      // 8. COMMIT AFTER SUCCESS ONLY. Advance last_rendered_count via the
      //    monotonic CAS only when the edit confirmed. On a failed edit we
      //    do NOT advance — the poll backstop will re-render and self-heal
      //    (this is THE invariant that prevents the stuck-counter
      //    regression on a transient edit failure).
      //
      //    BUT still refresh the debounce clock on failure (touchRenderedAt
      //    stamps last_rendered_at WITHOUT advancing the count). Otherwise
      //    coalescing would collapse precisely when it's needed most: a
      //    burst against a transiently-erroring Discord never stamps
      //    last_rendered_at (it's success-only), so every view in the burst
      //    re-attempts a PATCH — the exact 429 storm the gate exists to
      //    prevent, with only discord.js's backoff as the floor. Stamping
      //    on attempt keeps the failure path at ~M/window like the success
      //    path. Best-effort + logged-swallowed (the poll covers the miss).
      if (!r.ok) {
        try {
          await db.touchRenderedAt(sendId);
        } catch (touchErr) {
          logger.debug('qURL webhook sender-counter: touchRenderedAt failed (coalesce clock not refreshed; rate limiter is the floor)', {
            qurl_id: qurlId, send_id: sendId, error: touchErr?.message,
          });
        }
        return { status: 'edit-failed', n: N };
      }
      const advanced = await db.tryAdvanceRenderedCount(sendId, N);
      if (!repairFloor && !advanced && N < state.expectedCount) {
        scheduleSenderCounterFlush({ sendId, qurlId, delayMs: 0, repairFloor: true });
      }
      return { status: 'edited', n: N };
    })
    .then((verdict) => {
      // Observability-only terminal verdict (no HTTP response to map to).
      // The single uniform end signal across every branch — the unit
      // tests poll for this line to drain the deferred chain.
      logger.debug(COUNTER_VERDICT_MSG, { qurl_id: qurlId, status: verdict.status });
    })
    .catch((err) => {
      // NEVER log the token; err.message from the store/edit fns carries
      // none. A throw here is swallowed — the poll backstop renders.
      logger.error('qURL webhook sender-counter: fast-path threw', {
        error: err?.message, qurl_id: qurlId,
      });
    });
}

router.post('/qurl', async (req, res) => {
  const ip = req.ip || 'unknown';
  // OR-coupling note: an IP that already burned the HMAC limiter
  // (30/min) will get 429'd here even if its current request would
  // have been OWNER_UNKNOWN (separately budgeted at 150/min). That's
  // acceptable — the IP is already flagged as suspicious — so the
  // looser unknown-owner threshold protects legitimate operational
  // drift from OTHER IPs, not from the same already-bad-actor IP.
  if (badSigLimiter.shouldThrottle(ip) || unknownOwnerLimiter.shouldThrottle(ip)) {
    logger.warn('qURL webhook rate limit exceeded', { ip });
    logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_RATE_LIMITED, {});
    return res.status(429).json({ error: 'Too many invalid webhook attempts' });
  }

  const { result, ownerId, body } = verifyAndResolve(req);

  // 503 is the only retriable failure mode — qurl-service retries 503
  // (1+2+4+8+16=31s backoff, 5 attempts) but NOT 401. The cache-
  // unprimed case happens at cold start and during sibling-replica
  // lag after a peer's setGuildApiKey; treating it as 401 would
  // silently drop a guild's first views post-link.
  if (result === VERIFY_RESULTS.CACHE_UNPRIMED) {
    // Intentionally NOT counted toward unknownOwnerLimiter — the
    // unprimed window is one-shot at boot (primed never flips back to
    // false), so attacker exploitation here is bounded to the
    // cold-start window. qurl-service retries 503 so the event isn't
    // lost.
    logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_CACHE_MISS_UNPRIMED, {});
    return res.status(503).json({ error: 'Webhook receiver warming up' });
  }
  if (result === VERIFY_RESULTS.RAW_BODY_MISSING) {
    // Middleware bug — 503 so qurl-service retries while we investigate.
    return res.status(503).json({ error: 'Webhook receiver misconfigured' });
  }

  // Two limiters, two threat models:
  //   - HMAC failures count toward badSigLimiter (attacker brute-
  //     forcing the secret).
  //   - OWNER_UNKNOWN / OWNER_ID_MISSING count toward unknownOwnerLimiter
  //     (operational drift OR attacker probing with garbage owner_id;
  //     looser threshold so a real registry-rebuild burst doesn't
  //     throttle legitimate traffic on the same source IP).
  //
  // SIG_HEADER_MISSING / SIG_HEADER_MALFORMED route to badSigLimiter
  // (30/min) on purpose: a missing/malformed signature is
  // indistinguishable from attacker probing — a future qurl-service
  // contract drift that dropped the header would throttle legitimate
  // traffic, BUT loosening this would also weaken the brute-force
  // ceiling against the auth0-key-rotation attack model. Trade-off
  // chosen deliberately; do not loosen without re-deriving the
  // threat model.
  if (result !== VERIFY_RESULTS.OK) {
    const isHmacFailure = result === VERIFY_RESULTS.SIG_INVALID
      || result === VERIFY_RESULTS.SIG_HEADER_MISSING
      || result === VERIFY_RESULTS.SIG_HEADER_MALFORMED;
    const isUnknownOwner = result === VERIFY_RESULTS.OWNER_UNKNOWN
      || result === VERIFY_RESULTS.OWNER_ID_MISSING;
    let totalInWindow = null;
    if (isHmacFailure) totalInWindow = badSigLimiter.recordBadSig(ip);
    else if (isUnknownOwner) totalInWindow = unknownOwnerLimiter.recordBadSig(ip);
    // Audit-event split mirrors the threat-model split: OWNER_* are
    // operational/payload-shape signal (route both to the same
    // unknown-owner dashboard line); SIG_* are HMAC-failure signal.
    const auditEvent = isUnknownOwner
      ? AUDIT_EVENTS.QURL_WEBHOOK_CACHE_MISS_UNKNOWN_OWNER
      : AUDIT_EVENTS.QURL_WEBHOOK_SIGNATURE_INVALID;
    // Truncate + strip non-printables on the attacker-controlled
    // ownerId before logging. Mega-string blows up log volume; control
    // chars (\n, \r, escape sequences) confuse line-oriented log
    // parsers and can spoof field separators in some shippers.
    // 64 chars is well over an auth0 sub.
    const safeOwnerId = typeof ownerId === 'string'
      ? sanitizeForLog(ownerId.slice(0, 64))
      : ownerId;
    logger.warn('qURL webhook verification failed', { ip, totalInWindow, result, ownerId: safeOwnerId });
    logger.audit(auditEvent, { total_in_window: totalInWindow, result });
    return res.status(401).json({ error: 'Invalid signature' });
  }

  const eventType = body?.type;
  const data = body?.data;
  const eventId = body?.id;
  // typeof guard (not just falsy): a payload with `id: { weird: true }`
  // would otherwise pass through DDB's UpdateExpression and persist a
  // non-scalar replay key — silent corruption of dedup semantics.
  if (typeof eventId !== 'string' || !eventId) {
    logger.warn('qURL webhook missing or non-string body.id (per-event replay key)');
    return res.status(200).json({ status: 'invalid-payload' });
  }

  // TODO(freshness): the signature pins payload integrity but not
  // freshness. A captured event with a never-seen `id` and a
  // sufficiently-advanced access_count would still be accepted long
  // after capture. qurl-service includes a signed `timestamp` field;
  // adding a `now() - body.timestamp > N` rejection would close the
  // replay window. Out of scope for this PR (qurl-service is the only
  // valid emitter today).

  if (eventType === QURL_WEBHOOK_EVENTS.EXPIRED) {
    return handleQurlExpired(req, res, { data, eventId });
  }

  if (eventType !== QURL_WEBHOOK_EVENTS.ACCESSED) {
    logger.debug('qURL webhook event ignored', { type: eventType });
    return res.status(200).json({ status: 'ignored', type: eventType });
  }

  if (!data || typeof data.qurl_id !== 'string' || !data.qurl_id) {
    // Limit logged fields — `data` is post-trust but unbounded in shape
    // and may include attacker-influenced src_ip/user_agent for non-
    // transit resources. Log only the keys the pipeline actually reads.
    logger.warn('qURL webhook qurl.accessed missing qurl_id', { qurl_id: data?.qurl_id, resource_id: data?.resource_id });
    return res.status(200).json({ status: 'invalid-payload' });
  }

  // Strict integer gate — qurl-service emits access_count as Go int64
  // (`WebhookEvent.Data['access_count']`), so a non-integer or a value
  // past 2^53 here would be a wire-shape regression. isSafeInteger
  // (NOT just isFinite + >= 0) catches the float case + the unsafe-
  // int case in one check. Also blocks Number(null)===0 from slipping
  // through, which would otherwise cost a DDB write per such event.
  // Strict positive integer — qurl.accessed events always carry
  // access_count >= 1 by contract. Reject 0 here (vs accepting 0 +
  // writing to DDB + publisher dropping at SQS layer) to avoid the
  // asymmetric log pair where the webhook returns "recorded" and the
  // view-update-publisher then warns "invalid accessCount" on the
  // same event. One source of truth at the wire boundary.
  if (!Number.isSafeInteger(data.access_count) || data.access_count <= 0) {
    logger.warn('qURL webhook access_count not a positive integer', { qurl_id: data.qurl_id, access_count: data.access_count });
    return res.status(200).json({ status: 'invalid-payload' });
  }
  const accessCount = data.access_count;

  // Strict equality — Boolean(data.consumed) would coerce the string
  // "false" to true. Pinning to the boolean catches a future emit-side
  // regression that JSON-encodes the field as a string.
  const consumed = data.consumed === true;

  try {
    const viewRecord = await db.recordQurlView({
      qurlId: data.qurl_id,
      accessCount,
      consumed,
      eventId,
    });
    const { result: dbResult, firstView } = viewRecord;
    logger.info('qURL view recorded', { qurl_id: data.qurl_id, access_count: accessCount, consumed, result: dbResult });
    logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_RECEIVED, { result: dbResult });
    // Sub-second view counter (feat #60, PR-B): on a real new view
    // (result === 'recorded', NOT a per-event dedup replay) edit the
    // sender's confirmation to "👀 N viewed" directly from this replica
    // via the persisted interaction token. Fire-and-forget off the
    // already-decided 200 — the polling backstop in monitorLinkStatus
    // re-renders anything this edit misses.
    //
    // SUPERSESSION: do not also publish to the old SQS view-update path.
    // Two editors would fight over the same confirmation; the content-only
    // interaction-token edit is now the single fast-path. Follow-up #875
    // removes the dead publisher/consumer/registry wiring.
    if (dbResult === 'recorded') {
      editSenderCounterInBackground({ qurlId: data.qurl_id, firstView });
    }
    // One-time link consumed → flip the recipient's DM to "closed".
    // Fire-and-forget off the already-decided 200 (see
    // flipConsumedDMInBackground). Gated on `consumed`, NOT on
    // dbResult === 'recorded': the flip's own consumed_edited_at marker
    // is the idempotency layer, so attempting on any consumed event lets
    // a redelivery recover a transiently-missed flip while the marker
    // short-circuits the redundant edit.
    //
    // Reached only after the access_count gate above (a consumed event
    // with a malformed/zero access_count short-circuits at
    // invalid-payload and never flips here — by contract a consumed
    // qurl.accessed always carries access_count >= 1, so that's
    // theoretical; if it ever weren't, the qurl.expired backstop still
    // flips the DM, just with the less-specific copy).
    if (consumed) {
      flipConsumedDMInBackground({ qurlId: data.qurl_id, eventId });
    }
    return res.status(200).json({ status: dbResult });
  } catch (err) {
    // 5xx so qurl-service retries — transient DDB throttle should not
    // silently drop a view event.
    logger.error('Failed to record qURL view', { error: err.message, qurl_id: data.qurl_id });
    logger.audit(AUDIT_EVENTS.QURL_WEBHOOK_STORE_ERROR, { error_type: err.name || 'unknown' });
    return res.status(500).json({ error: 'Internal error' });
  }
});

router.stopIntervals = function stopIntervals() {
  badSigLimiter.stopSweep();
  unknownOwnerLimiter.stopSweep();
  for (const timer of senderCounterFlushTimers.values()) clearTimeout(timer);
  senderCounterFlushTimers.clear();
};
module.exports = router;
