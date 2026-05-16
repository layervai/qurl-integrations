// HMAC-SHA256 sign/verify primitive for Pillar 3's control-channel
// push-handoff. Backed by a JSON-shaped SSM secret
// `{"current": "<hex>", "previous": "<hex>"}`. Both keys are loaded
// at boot and held in memory for the task lifetime — no SSM hot-
// reload (matches every other secret in this app:
// DISCORD_TOKEN, KEY_ENCRYPTION_KEY, etc.; rotation is via rolling
// redeploy, see design doc §Pillar 3 "Secret rotation").
//
// ── Sign side ──
// Active always signs with `current`. The signed body is the raw
// UTF-8 bytes of `JSON.stringify(payload)`; the wire shape is
// `{ body: <string>, signature: <hex> }`. We sign the bytes once,
// not the parsed object, so receiver-side JSON-key-reordering or
// re-stringification never breaks the signature.
//
// ── Wire envelope helpers ──
// `wrapEnvelope({bodyBytes, signature}) → Buffer` and
// `unwrapEnvelope(wireBytes) → {bodyBytes, signature} | null` are
// the single source of truth for the on-the-wire shape. The
// control-channel server, control-channel client, and test fixtures
// all funnel through these — so a future change to the envelope
// (rename, version field, etc.) is one edit instead of three
// drifting copies.
//
// ── Verify side ──
// Standby tries `current` first, then `previous`. The dual-accept
// window covers the rotation deploy: between the rolling-deploy
// stages, one replica may still sign with `previous` while the
// other has loaded `current` as new and `previous` as the old
// (or vice-versa). At least one of the two HMACs must validate.
//
// Verify MUST run on the raw bytes BEFORE `JSON.parse`. Two reasons:
//   1. Key-order safety: parsing then re-stringifying for verification
//      would canonicalize the key order, and a body signed with key
//      order A would fail to verify if the receiver's stringify
//      produced key order B.
//   2. DoS protection: parsing an unverified body lets an attacker
//      OOM the standby by sending a giant JSON payload. The body
//      cap (enforced upstream in gateway-control-channel) bounds
//      memory regardless, but verifying first means we don't parse
//      unauthenticated input at all.
//
// Equality is `crypto.timingSafeEqual` — never `===`. We length-check
// first because timingSafeEqual throws on length mismatch (a property
// that's load-bearing for the timing-safe guarantee but inconvenient
// for our "try current then previous" flow where a malformed signature
// could be any length).
//
// ── Freshness window ──
// Payloads carry a `ts` (epoch ms). Anything outside ±freshnessWindowMs
// (default 5000) is rejected. Clock skew >±2s pushes bodies into the
// freshness edge and may bounce; the design doc names this as a
// chrony-failure incident, not an application-level recovery target.
//
// ── Nonce cache (FIFO-by-insertion) ──
// Each verified body carries a `nonce` (16 random bytes hex). The
// verifier maintains a bounded in-memory FIFO (default 1024 entries)
// of seen nonces. Replays within the freshness window are rejected.
// At 5s freshness and 1024 entries, the cache absorbs up to ~200
// handoffs/sec before still-fresh nonces start evicting (which would
// allow replay within the 5s window). Real-world handoff rate is
// one-per-deploy, so the size is ~3 orders of magnitude over-
// provisioned; revisit the size if either freshness or handoff rate
// changes.
//
// FIFO (not LRU): we evict by insertion order, not by access order.
// True LRU would re-rank on each `has()` hit, but the verify path
// returns `replay` before `rememberNonce` runs on a duplicate, so
// the cache never sees an `has()` hit followed by use. Eviction by
// insertion order == eviction by first-seen, which is what we want
// for a replay window: an old nonce is more likely to be past
// freshness than a recently-inserted one. Same behavior either way
// for this call shape; the name reflects the simpler invariant.
//
// The check-then-set MUST be one synchronous microtask. There must
// be no `await` between Map.has and Map.set, otherwise a concurrent
// duplicate POST slips through both checks.

const crypto = require('node:crypto');

const DEFAULT_FRESHNESS_WINDOW_MS = 5_000;
const DEFAULT_NONCE_LRU_SIZE = 1024;

// Verify-rejection reasons. Exported so callers (control-channel
// server logs, tests) reference the constant rather than the literal
// — one typo in a literal across modules silently breaks the
// protocol's observability surface.
const VERIFY_REASONS = Object.freeze({
  BAD_SIGNATURE: 'bad_signature',
  STALE: 'stale',
  REPLAY: 'replay',
  MALFORMED_BODY: 'malformed_body',
  MISSING_FIELD: 'missing_field',
});

function createGatewayHmac({
  secrets,
  logger,
  // Injected for deterministic tests. Production uses Date.now.
  clock = () => Date.now(),
  freshnessWindowMs = DEFAULT_FRESHNESS_WINDOW_MS,
  nonceLruSize = DEFAULT_NONCE_LRU_SIZE,
} = {}) {
  if (!secrets || typeof secrets !== 'object') {
    throw new Error('createGatewayHmac: secrets ({ current, previous? }) is required');
  }
  if (typeof secrets.current !== 'string' || secrets.current.length === 0) {
    throw new Error('createGatewayHmac: secrets.current (non-empty hex string) is required');
  }
  // `previous` is optional — null/undefined accepted, but if present
  // must be a NON-EMPTY string. The use site at the verify path
  // (`if (!matched && secrets.previous)`) treats empty string as
  // "not configured" (falsy); without this validator catching it,
  // a misconfig that set `previous: ""` would silently disable
  // dual-accept during the rolling-deploy rotation window. Fail
  // loud at boot.
  if (secrets.previous != null
      && (typeof secrets.previous !== 'string' || secrets.previous.length === 0)) {
    throw new Error('createGatewayHmac: secrets.previous must be a non-empty string or null');
  }
  if (!logger) throw new Error('createGatewayHmac: logger is required');

  // Map insertion order is preserved per spec — so a Map gives us
  // a size-bounded FIFO by evicting the oldest (first iterator key)
  // when at capacity. We do NOT re-insert on access (which would
  // make this a true LRU) — see the FIFO discussion in the module
  // header for why insertion-order eviction is correct for the
  // replay-window invariant.
  const seenNonces = new Map();

  function rememberNonce(nonce) {
    // Insert-only. The sole caller (`verify`) already returns
    // `replay` BEFORE reaching here on a duplicate, so this never
    // sees an existing key — and a defensive bump-to-newest would
    // be misleading code (it would never run). If a future caller
    // bypasses the verify path and needs bump semantics, add an
    // explicit pre-check at that call site.
    seenNonces.set(nonce, true);
    while (seenNonces.size > nonceLruSize) {
      const oldest = seenNonces.keys().next().value;
      seenNonces.delete(oldest);
    }
  }

  function hmacHex(secret, bodyBytes) {
    return crypto.createHmac('sha256', secret).update(bodyBytes).digest('hex');
  }

  // Signs the payload with `secrets.current` and returns the wire
  // shape. Caller is responsible for adding `ts` and `nonce` to the
  // payload before calling — those are part of the freshness +
  // replay defenses, signed alongside the rest of the body.
  function sign(payload) {
    if (payload == null || typeof payload !== 'object') {
      throw new Error('gateway-hmac: sign requires an object payload');
    }
    const bodyBytes = Buffer.from(JSON.stringify(payload), 'utf8');
    const signature = hmacHex(secrets.current, bodyBytes);
    return { bodyBytes, signature };
  }

  // Verify a body + signature pair. `bodyBytes` MUST be the exact
  // bytes received off the wire, not a re-stringified parsed
  // representation (see module header).
  //
  // Returns one of:
  //   { ok: true, payload }
  //   { ok: false, reason: 'bad_signature' | 'stale' | 'replay' | 'malformed_body' | 'missing_field' }
  //
  // `payload` on the ok-path is the JSON-parsed body. Parse happens
  // AFTER successful HMAC verification.
  function verify({ bodyBytes, signature }) {
    if (!Buffer.isBuffer(bodyBytes) || typeof signature !== 'string') {
      return { ok: false, reason: VERIFY_REASONS.MALFORMED_BODY };
    }

    const expectedCurrent = hmacHex(secrets.current, bodyBytes);
    let matched = timingSafeHexEqual(signature, expectedCurrent);

    // Try `previous` only if `current` didn't match AND `previous`
    // is configured. The rotation procedure relies on the standby
    // accepting both during the rolling-deploy window.
    if (!matched && secrets.previous) {
      const expectedPrevious = hmacHex(secrets.previous, bodyBytes);
      matched = timingSafeHexEqual(signature, expectedPrevious);
    }

    if (!matched) {
      return { ok: false, reason: VERIFY_REASONS.BAD_SIGNATURE };
    }

    let payload;
    try {
      payload = JSON.parse(bodyBytes.toString('utf8'));
    } catch (_err) {
      return { ok: false, reason: VERIFY_REASONS.MALFORMED_BODY };
    }
    // Reject arrays too — `typeof [] === 'object'`, but a handoff
    // payload is always a plain object. The downstream
    // findInvalidHandoffField check would catch an array anyway
    // (array.active_instance_id is undefined), but rejecting at the
    // HMAC module boundary keeps this layer's contract less
    // dependent on receiver-side validation shape.
    if (payload == null || typeof payload !== 'object' || Array.isArray(payload)) {
      return { ok: false, reason: VERIFY_REASONS.MALFORMED_BODY };
    }

    if (typeof payload.ts !== 'number' || typeof payload.nonce !== 'string' || payload.nonce.length === 0) {
      return { ok: false, reason: VERIFY_REASONS.MISSING_FIELD };
    }

    const now = clock();
    if (Math.abs(now - payload.ts) > freshnessWindowMs) {
      logger.warn('gateway-hmac: stale body rejected', {
        ts: payload.ts, now, skewMs: now - payload.ts, freshnessWindowMs,
      });
      return { ok: false, reason: VERIFY_REASONS.STALE };
    }

    // Check-then-set must be ONE microtask — no await between. A
    // concurrent duplicate POST landing here mid-handler would
    // otherwise slip through both checks.
    if (seenNonces.has(payload.nonce)) {
      logger.warn('gateway-hmac: replayed nonce rejected', { noncePrefix: payload.nonce.slice(0, 8) });
      return { ok: false, reason: VERIFY_REASONS.REPLAY };
    }
    rememberNonce(payload.nonce);

    return { ok: true, payload };
  }

  // Generates a fresh nonce. Exposed so callers (the handoff body
  // builder) don't have to reach into `node:crypto` themselves.
  function generateNonce() {
    return crypto.randomBytes(16).toString('hex');
  }

  return {
    sign,
    verify,
    generateNonce,
    // Inspection seams for tests.
    _getSeenNoncesSizeForTest() {
      return seenNonces.size;
    },
  };
}

// Build the wire envelope from `sign()`'s output. The wire shape is
// `{body: <utf-8 string of signed JSON>, signature: <hex>}`,
// serialized as JSON. Stringifying the inner body once and embedding
// it as a STRING (not a parsed object) preserves the inner bytes
// verbatim across `JSON.parse` on the receiver — JSON-string round-
// trip is byte-exact, parsed-object round-trip is not (key order
// canonicalization could break HMAC verify).
function wrapEnvelope({ bodyBytes, signature }) {
  if (!Buffer.isBuffer(bodyBytes) || typeof signature !== 'string') {
    throw new Error('wrapEnvelope: { bodyBytes: Buffer, signature: string } required');
  }
  return Buffer.from(JSON.stringify({
    body: bodyBytes.toString('utf8'),
    signature,
  }), 'utf8');
}

// Parse wire envelope bytes back to `{bodyBytes, signature}`. Returns
// null on any shape mismatch — caller decides whether to reject with
// 400 / log / etc. Does NOT verify HMAC; that's `hmac.verify()`'s job.
function unwrapEnvelope(wireBytes) {
  if (!Buffer.isBuffer(wireBytes)) return null;
  let envelope;
  try {
    envelope = JSON.parse(wireBytes.toString('utf8'));
  } catch (_err) {
    return null;
  }
  if (envelope == null
    || typeof envelope.body !== 'string'
    || typeof envelope.signature !== 'string') {
    return null;
  }
  return {
    bodyBytes: Buffer.from(envelope.body, 'utf8'),
    signature: envelope.signature,
  };
}

// Constant-time hex string compare. timingSafeEqual throws on length
// mismatch (a property that's load-bearing for the timing-safe
// guarantee but inconvenient here, where a malformed or non-hex
// candidate signature could be any length). We:
//   1. Reject up front on string-length mismatch.
//   2. Parse both sides with Buffer.from(..., 'hex'). Buffer.from
//      SILENTLY DROPS non-hex characters rather than throwing, so
//      a same-length-but-non-hex `a` produces a SHORTER buffer than
//      the proper-hex `b` — re-check buffer byte length before the
//      timing-safe compare to keep timingSafeEqual from throwing.
function timingSafeHexEqual(a, b) {
  if (typeof a !== 'string' || typeof b !== 'string' || a.length !== b.length) {
    return false;
  }
  const ba = Buffer.from(a, 'hex');
  const bb = Buffer.from(b, 'hex');
  if (ba.length !== bb.length) {
    return false;
  }
  return crypto.timingSafeEqual(ba, bb);
}

module.exports = {
  createGatewayHmac,
  wrapEnvelope,
  unwrapEnvelope,
  VERIFY_REASONS,
  DEFAULT_FRESHNESS_WINDOW_MS,
  DEFAULT_NONCE_LRU_SIZE,
};
