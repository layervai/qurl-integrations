// Unit tests for src/gateway-hmac.js — Pillar 3 push-handoff
// HMAC primitive. Pins the load-bearing contracts:
//
//   1. Sign produces { bodyBytes, signature } with HMAC computed
//      over the exact UTF-8 bytes of JSON.stringify(payload).
//   2. Verify runs HMAC check BEFORE JSON.parse. A malformed body
//      with a valid HMAC is reported as malformed_body AFTER parse;
//      an unsigned/bad-sig body never reaches parse.
//   3. Dual-secret accept: verify tries `current`, then `previous`
//      iff configured. `previous` may be null/undefined.
//   4. Timing-safe equality — length mismatches MUST NOT throw
//      (since timingSafeEqual itself throws on length mismatch).
//   5. Freshness window: bodies outside ±freshnessWindowMs of clock()
//      are rejected with reason 'stale'.
//   6. Nonce LRU: duplicate nonce within freshness window is rejected
//      with reason 'replay'. LRU is bounded; oldest evicts when full.
//      Check-then-set must be synchronous (one microtask).
//   7. Wire shape: { bodyBytes: Buffer, signature: hex-string };
//      anything else is malformed_body.
//
// Each contract maps to a concrete production failure mode:
//   - (1) parsing-then-re-stringifying would canonicalize key order
//     and break verification on a sig produced for the original order.
//   - (2) parsing first lets an attacker OOM the standby with a
//     giant unsigned JSON body.
//   - (3) without dual-accept, every rotation deploy would lose
//     handoffs across the rolling-deploy window.
//   - (4) timingSafeEqual throws on length mismatch — a naive caller
//     would crash the verifier on any malformed signature.
//   - (5)/(6) without freshness+nonce, a replayed body from a prior
//     handoff could trigger a second standby takeover.

const crypto = require('node:crypto');
const {
  createGatewayHmac,
  wrapEnvelope,
  unwrapEnvelope,
  DEFAULT_FRESHNESS_WINDOW_MS,
  DEFAULT_NONCE_LRU_SIZE,
} = require('../src/gateway-hmac');

const SECRET_CURRENT = 'a'.repeat(64);
const SECRET_PREVIOUS = 'b'.repeat(64);

function makeHmac({
  secrets = { current: SECRET_CURRENT, previous: SECRET_PREVIOUS },
  clock,
  freshnessWindowMs,
  nonceLruSize,
} = {}) {
  const logger = {
    info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(),
  };
  const hmac = createGatewayHmac({
    secrets, logger, clock, freshnessWindowMs, nonceLruSize,
  });
  return { hmac, logger };
}

// Builds a fresh handoff-shaped payload pinned to a clock tick.
function freshPayload({ now = 1_700_000_000_000, nonce = 'n'.repeat(32), extras = {} } = {}) {
  return { ts: now, nonce, ...extras };
}

describe('createGatewayHmac — factory validation', () => {
  it('throws when secrets is missing or wrong shape', () => {
    const logger = { info() {}, warn() {}, error() {}, debug() {} };
    expect(() => createGatewayHmac()).toThrow(/secrets/);
    expect(() => createGatewayHmac({ logger })).toThrow(/secrets/);
    expect(() => createGatewayHmac({ secrets: 'string', logger })).toThrow(/secrets/);
    expect(() => createGatewayHmac({ secrets: {}, logger })).toThrow(/secrets\.current/);
    expect(() => createGatewayHmac({ secrets: { current: '' }, logger })).toThrow(/secrets\.current/);
    expect(() => createGatewayHmac({ secrets: { current: 'x', previous: 42 }, logger }))
      .toThrow(/secrets\.previous must be a non-empty string/);
  });

  it('throws when logger is missing', () => {
    expect(() => createGatewayHmac({ secrets: { current: 'x' } })).toThrow(/logger is required/);
  });

  it('accepts secrets with null/undefined previous (single-secret post-rotation state)', () => {
    const logger = { info() {}, warn() {}, error() {}, debug() {} };
    expect(() => createGatewayHmac({ secrets: { current: 'x' }, logger })).not.toThrow();
    expect(() => createGatewayHmac({ secrets: { current: 'x', previous: null }, logger })).not.toThrow();
    expect(() => createGatewayHmac({ secrets: { current: 'x', previous: undefined }, logger })).not.toThrow();
  });

  it('rejects secrets.previous = "" (empty string) — would silently disable dual-accept', () => {
    // The verify use site treats falsy `previous` as "not configured"
    // and skips the second hmac check. Without this validator, a
    // misconfig writing `previous: ""` would pass at boot and silently
    // disable the rotation window's dual-accept. Fail loud.
    const logger = { info() {}, warn() {}, error() {}, debug() {} };
    expect(() => createGatewayHmac({
      secrets: { current: 'x', previous: '' }, logger,
    })).toThrow(/non-empty string or null/);
  });

  it('exposes default constants', () => {
    expect(DEFAULT_FRESHNESS_WINDOW_MS).toBe(5_000);
    expect(DEFAULT_NONCE_LRU_SIZE).toBe(1024);
  });

  it('exposes frozen VERIFY_REASONS to keep cross-module reason codes typo-safe', () => {
    // eslint-disable-next-line global-require
    const { VERIFY_REASONS } = require('../src/gateway-hmac');
    expect(VERIFY_REASONS).toEqual({
      BAD_SIGNATURE: 'bad_signature',
      STALE: 'stale',
      REPLAY: 'replay',
      MALFORMED_BODY: 'malformed_body',
      MISSING_FIELD: 'missing_field',
    });
    expect(Object.isFrozen(VERIFY_REASONS)).toBe(true);
  });
});

describe('sign', () => {
  it('signs the raw UTF-8 bytes of JSON.stringify(payload) with `current`', () => {
    const { hmac } = makeHmac();
    const payload = freshPayload({ extras: { hello: 'world' } });
    const { bodyBytes, signature } = hmac.sign(payload);

    expect(Buffer.isBuffer(bodyBytes)).toBe(true);
    expect(bodyBytes.toString('utf8')).toBe(JSON.stringify(payload));

    // Recompute deterministically using `current` and confirm equality.
    const expected = crypto.createHmac('sha256', SECRET_CURRENT)
      .update(bodyBytes)
      .digest('hex');
    expect(signature).toBe(expected);
  });

  it('rejects non-object payloads', () => {
    const { hmac } = makeHmac();
    expect(() => hmac.sign(null)).toThrow(/object payload/);
    expect(() => hmac.sign('string')).toThrow(/object payload/);
    expect(() => hmac.sign(42)).toThrow(/object payload/);
  });

  it('produces a signature distinct from the one `previous` would produce', () => {
    // If the sign path ever drifted to using `previous`, the standby
    // would never verify a fresh handoff body in steady state.
    const { hmac } = makeHmac();
    const payload = freshPayload();
    const { bodyBytes, signature } = hmac.sign(payload);
    const previousSig = crypto.createHmac('sha256', SECRET_PREVIOUS)
      .update(bodyBytes).digest('hex');
    expect(signature).not.toBe(previousSig);
  });
});

describe('verify — happy path', () => {
  it('verifies a fresh body+signature pair signed by `current`', () => {
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    const payload = freshPayload({ now });
    const { bodyBytes, signature } = hmac.sign(payload);
    const result = hmac.verify({ bodyBytes, signature });
    expect(result.ok).toBe(true);
    expect(result.payload).toEqual(payload);
  });

  it('verifies a body signed by `previous` (dual-accept during rotation)', () => {
    // Simulate: peer is on `previous`, this replica has rotated to
    // a new `current` with the old now in `previous`.
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({
      secrets: { current: 'new'.repeat(20), previous: SECRET_CURRENT },
      clock: () => now,
    });
    const payload = freshPayload({ now });
    const bodyBytes = Buffer.from(JSON.stringify(payload), 'utf8');
    const signature = crypto.createHmac('sha256', SECRET_CURRENT)
      .update(bodyBytes).digest('hex');

    const result = hmac.verify({ bodyBytes, signature });
    expect(result.ok).toBe(true);
    expect(result.payload).toEqual(payload);
  });
});

describe('verify — rejection reasons', () => {
  it('rejects bodyBytes that is not a Buffer', () => {
    const { hmac } = makeHmac();
    expect(hmac.verify({ bodyBytes: '{"ts":1,"nonce":"x"}', signature: 'ab' }))
      .toEqual({ ok: false, reason: 'malformed_body' });
  });

  it('rejects signature that is not a string', () => {
    const { hmac } = makeHmac();
    expect(hmac.verify({ bodyBytes: Buffer.from('{}'), signature: 12345 }))
      .toEqual({ ok: false, reason: 'malformed_body' });
  });

  it('rejects a tampered signature (bad_signature)', () => {
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    const { bodyBytes } = hmac.sign(freshPayload({ now }));
    const badSig = 'f'.repeat(64);
    expect(hmac.verify({ bodyBytes, signature: badSig }))
      .toEqual({ ok: false, reason: 'bad_signature' });
  });

  it('rejects a signature with wrong length WITHOUT throwing', () => {
    // crypto.timingSafeEqual throws on length mismatch — the module's
    // timingSafeHexEqual MUST length-check first or this throws and
    // takes the verifier down.
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    const { bodyBytes } = hmac.sign(freshPayload({ now }));
    expect(() => hmac.verify({ bodyBytes, signature: 'short' })).not.toThrow();
    expect(hmac.verify({ bodyBytes, signature: 'short' }))
      .toEqual({ ok: false, reason: 'bad_signature' });
  });

  it('rejects non-hex signature WITHOUT throwing', () => {
    // Buffer.from(s, 'hex') silently drops non-hex chars rather than
    // throwing. Length check guards us against panic, but a same-
    // length-non-hex signature still must return bad_signature.
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    const { bodyBytes } = hmac.sign(freshPayload({ now }));
    const nonHex = 'z'.repeat(64);
    expect(() => hmac.verify({ bodyBytes, signature: nonHex })).not.toThrow();
    expect(hmac.verify({ bodyBytes, signature: nonHex }))
      .toEqual({ ok: false, reason: 'bad_signature' });
  });

  it('rejects a body with a valid HMAC but non-JSON content (malformed_body AFTER parse)', () => {
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    const bodyBytes = Buffer.from('not-json', 'utf8');
    const signature = crypto.createHmac('sha256', SECRET_CURRENT)
      .update(bodyBytes).digest('hex');
    expect(hmac.verify({ bodyBytes, signature }))
      .toEqual({ ok: false, reason: 'malformed_body' });
  });

  it('rejects a body whose JSON parses to a non-object', () => {
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    const bodyBytes = Buffer.from('42', 'utf8');
    const signature = crypto.createHmac('sha256', SECRET_CURRENT)
      .update(bodyBytes).digest('hex');
    expect(hmac.verify({ bodyBytes, signature }))
      .toEqual({ ok: false, reason: 'malformed_body' });
  });

  it('rejects a body missing ts (missing_field)', () => {
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    const payload = { nonce: 'n'.repeat(32) };
    const bodyBytes = Buffer.from(JSON.stringify(payload), 'utf8');
    const signature = crypto.createHmac('sha256', SECRET_CURRENT)
      .update(bodyBytes).digest('hex');
    expect(hmac.verify({ bodyBytes, signature }))
      .toEqual({ ok: false, reason: 'missing_field' });
  });

  it('rejects a body missing nonce (missing_field)', () => {
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    const payload = { ts: now };
    const bodyBytes = Buffer.from(JSON.stringify(payload), 'utf8');
    const signature = crypto.createHmac('sha256', SECRET_CURRENT)
      .update(bodyBytes).digest('hex');
    expect(hmac.verify({ bodyBytes, signature }))
      .toEqual({ ok: false, reason: 'missing_field' });
  });

  it('rejects a body where ts is not a number', () => {
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    const payload = { ts: String(now), nonce: 'n'.repeat(32) };
    const bodyBytes = Buffer.from(JSON.stringify(payload), 'utf8');
    const signature = crypto.createHmac('sha256', SECRET_CURRENT)
      .update(bodyBytes).digest('hex');
    expect(hmac.verify({ bodyBytes, signature }))
      .toEqual({ ok: false, reason: 'missing_field' });
  });

  it('rejects a body where nonce is empty', () => {
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    const payload = { ts: now, nonce: '' };
    const bodyBytes = Buffer.from(JSON.stringify(payload), 'utf8');
    const signature = crypto.createHmac('sha256', SECRET_CURRENT)
      .update(bodyBytes).digest('hex');
    expect(hmac.verify({ bodyBytes, signature }))
      .toEqual({ ok: false, reason: 'missing_field' });
  });

  it('does not try `previous` when not configured (and bad_signature when current fails)', () => {
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({
      secrets: { current: SECRET_CURRENT },
      clock: () => now,
    });
    const payload = freshPayload({ now });
    const bodyBytes = Buffer.from(JSON.stringify(payload), 'utf8');
    const signature = crypto.createHmac('sha256', SECRET_PREVIOUS)
      .update(bodyBytes).digest('hex');
    expect(hmac.verify({ bodyBytes, signature }))
      .toEqual({ ok: false, reason: 'bad_signature' });
  });
});

describe('verify — freshness window', () => {
  it('accepts ts inside ±freshnessWindowMs', () => {
    let now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    // Body signed at t=now, verified at t=now+4999 (just inside 5s window).
    const payload = freshPayload({ now });
    const { bodyBytes, signature } = hmac.sign(payload);
    now += 4_999;
    expect(hmac.verify({ bodyBytes, signature })).toEqual({ ok: true, payload });
  });

  it('rejects ts older than freshnessWindowMs (stale)', () => {
    let now = 1_700_000_000_000;
    const { hmac, logger } = makeHmac({ clock: () => now });
    const payload = freshPayload({ now });
    const { bodyBytes, signature } = hmac.sign(payload);
    now += 5_001;
    expect(hmac.verify({ bodyBytes, signature }))
      .toEqual({ ok: false, reason: 'stale' });
    expect(logger.warn).toHaveBeenCalledWith(
      'gateway-hmac: stale body rejected', expect.objectContaining({ ts: payload.ts }),
    );
  });

  it('rejects ts far in the future (clock-skew on sender side) as stale', () => {
    // Symmetric window: a sender whose clock is way ahead is also rejected.
    let now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    const payload = freshPayload({ now: now + 5_001 });
    const bodyBytes = Buffer.from(JSON.stringify(payload), 'utf8');
    const signature = crypto.createHmac('sha256', SECRET_CURRENT)
      .update(bodyBytes).digest('hex');
    expect(hmac.verify({ bodyBytes, signature }))
      .toEqual({ ok: false, reason: 'stale' });
  });

  it('honors a custom freshnessWindowMs', () => {
    let now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now, freshnessWindowMs: 100 });
    const payload = freshPayload({ now });
    const { bodyBytes, signature } = hmac.sign(payload);
    now += 101;
    expect(hmac.verify({ bodyBytes, signature })).toEqual({ ok: false, reason: 'stale' });
  });
});

describe('verify — nonce LRU', () => {
  it('rejects a replayed nonce within the freshness window', () => {
    const now = 1_700_000_000_000;
    const { hmac, logger } = makeHmac({ clock: () => now });
    const payload = freshPayload({ now });
    const { bodyBytes, signature } = hmac.sign(payload);

    const first = hmac.verify({ bodyBytes, signature });
    const second = hmac.verify({ bodyBytes, signature });
    expect(first.ok).toBe(true);
    expect(second).toEqual({ ok: false, reason: 'replay' });
    expect(logger.warn).toHaveBeenCalledWith(
      'gateway-hmac: replayed nonce rejected', expect.objectContaining({ noncePrefix: 'nnnnnnnn' }),
    );
  });

  it('rejects replay only AFTER signature passes — bad-signature replays do not poison the LRU', () => {
    // Verifying a body with a bad signature must NOT add its nonce
    // to the LRU; otherwise an attacker could pre-burn legitimate
    // nonces with garbage signatures and DoS the standby.
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    const payload = freshPayload({ now });
    const bodyBytes = Buffer.from(JSON.stringify(payload), 'utf8');
    // First call: valid body, bad signature → bad_signature
    expect(hmac.verify({ bodyBytes, signature: 'f'.repeat(64) }))
      .toEqual({ ok: false, reason: 'bad_signature' });
    // Now the legitimate signed body must still verify (nonce not burned).
    const goodSig = crypto.createHmac('sha256', SECRET_CURRENT)
      .update(bodyBytes).digest('hex');
    expect(hmac.verify({ bodyBytes, signature: goodSig })).toEqual({ ok: true, payload });
  });

  it('does not burn the nonce when stale (freshness rejection precedes nonce remember)', () => {
    // A stale body shouldn't burn its nonce; otherwise a clock-skew
    // bounce could permanently lock out a still-valid retry.
    let now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    const payload = freshPayload({ now });
    const { bodyBytes, signature } = hmac.sign(payload);

    now += 5_001;
    expect(hmac.verify({ bodyBytes, signature })).toEqual({ ok: false, reason: 'stale' });

    now -= 5_001; // clock recovers; same body now in-window again
    expect(hmac.verify({ bodyBytes, signature })).toEqual({ ok: true, payload });
  });

  it('evicts oldest nonce when LRU is full (bounded memory)', () => {
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now, nonceLruSize: 3 });

    function signAndVerify(nonceChar) {
      const payload = { ts: now, nonce: nonceChar.repeat(32) };
      const bodyBytes = Buffer.from(JSON.stringify(payload), 'utf8');
      const signature = crypto.createHmac('sha256', SECRET_CURRENT)
        .update(bodyBytes).digest('hex');
      return hmac.verify({ bodyBytes, signature });
    }

    // Fill the LRU with 3 distinct nonces.
    expect(signAndVerify('1').ok).toBe(true);
    expect(signAndVerify('2').ok).toBe(true);
    expect(signAndVerify('3').ok).toBe(true);

    // Oldest nonce (nonce='1') is still pinned. Add a 4th — now '1' evicts.
    expect(signAndVerify('4').ok).toBe(true);

    // Replaying nonce='2' (still in LRU) is rejected.
    expect(signAndVerify('2')).toEqual({ ok: false, reason: 'replay' });

    // Replaying nonce='1' (evicted) is ACCEPTED — bounded LRU semantics.
    // (At default LRU size of 1024 + 5s freshness, real-world traffic
    // never approaches eviction; this only validates the bound.)
    expect(signAndVerify('1').ok).toBe(true);
  });

  it('bumps a nonce to newest on re-access (would not happen via verify but exercised via the size cap)', () => {
    // We can't directly call rememberNonce; but bump-to-newest is an
    // internal property that only matters via the size cap. Since
    // verify rejects duplicates, the bump-on-re-insert path is only
    // reached by the explicit `seenNonces.has(...) → delete → set`
    // dance. We approximate by checking the inspection seam reports
    // a stable size after many distinct nonces with capacity.
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now, nonceLruSize: 5 });
    for (let i = 0; i < 50; i += 1) {
      const payload = { ts: now, nonce: `nonce-${i}`.padEnd(32, '0') };
      const bodyBytes = Buffer.from(JSON.stringify(payload), 'utf8');
      const signature = crypto.createHmac('sha256', SECRET_CURRENT)
        .update(bodyBytes).digest('hex');
      hmac.verify({ bodyBytes, signature });
    }
    expect(hmac._getSeenNoncesSizeForTest()).toBe(5);
  });

  it('check-then-set is synchronous — two parallel verifies of the same nonce produce exactly one ok:true', async () => {
    // The check-then-set MUST run as one microtask. A future refactor
    // that drops an `await` between `seenNonces.has(nonce)` and
    // `rememberNonce(nonce)` would let two parallel verifies BOTH
    // observe "not seen" before either sets — and both would return
    // ok:true on the same body. We drive two parallel `verify()`
    // calls of the same body+signature and assert exactly one wins.
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    const payload = freshPayload({ now });
    const { bodyBytes, signature } = hmac.sign(payload);

    const results = await Promise.all([
      Promise.resolve().then(() => hmac.verify({ bodyBytes, signature })),
      Promise.resolve().then(() => hmac.verify({ bodyBytes, signature })),
    ]);
    const oks = results.filter((r) => r.ok);
    const replays = results.filter((r) => !r.ok && r.reason === 'replay');
    expect(oks).toHaveLength(1);
    expect(replays).toHaveLength(1);
  });
});

describe('generateNonce', () => {
  it('returns a 32-char hex string (16 random bytes)', () => {
    const { hmac } = makeHmac();
    const a = hmac.generateNonce();
    const b = hmac.generateNonce();
    expect(a).toMatch(/^[0-9a-f]{32}$/);
    expect(b).toMatch(/^[0-9a-f]{32}$/);
    expect(a).not.toBe(b);
  });
});

describe('wrapEnvelope / unwrapEnvelope', () => {
  it('round-trips bodyBytes + signature byte-exact', () => {
    const bodyBytes = Buffer.from('{"a":1,"b":"hello"}', 'utf8');
    const signature = 'a'.repeat(64);
    const wire = wrapEnvelope({ bodyBytes, signature });
    expect(Buffer.isBuffer(wire)).toBe(true);
    const unwrapped = unwrapEnvelope(wire);
    expect(unwrapped).not.toBeNull();
    expect(unwrapped.bodyBytes.equals(bodyBytes)).toBe(true);
    expect(unwrapped.signature).toBe(signature);
  });

  it('preserves inner JSON key order across the round-trip (HMAC-verify-safe)', () => {
    // The wire wrapping must preserve the inner body verbatim. If
    // it ever JSON.parsed and re-stringified the inner body, a
    // sender that signed `{"b":1,"a":2}` would have its key-order
    // canonicalized to `{"a":2,"b":1}` on the receiver and HMAC
    // verify would fail.
    const innerStr = '{"z":1,"a":2,"m":3}';
    const bodyBytes = Buffer.from(innerStr, 'utf8');
    const wire = wrapEnvelope({ bodyBytes, signature: 'sig' });
    const unwrapped = unwrapEnvelope(wire);
    expect(unwrapped.bodyBytes.toString('utf8')).toBe(innerStr);
  });

  it('wrapEnvelope throws on bad input', () => {
    expect(() => wrapEnvelope({})).toThrow();
    expect(() => wrapEnvelope({ bodyBytes: 'string', signature: 'sig' })).toThrow();
    expect(() => wrapEnvelope({ bodyBytes: Buffer.from(''), signature: 42 })).toThrow();
  });

  it('unwrapEnvelope returns null on shape mismatch (does not throw)', () => {
    expect(unwrapEnvelope(null)).toBeNull();
    expect(unwrapEnvelope('string')).toBeNull();
    expect(unwrapEnvelope(Buffer.from('not-json'))).toBeNull();
    expect(unwrapEnvelope(Buffer.from('42'))).toBeNull();
    expect(unwrapEnvelope(Buffer.from('{}'))).toBeNull();
    expect(unwrapEnvelope(Buffer.from('{"body":"x"}'))).toBeNull();
    expect(unwrapEnvelope(Buffer.from('{"signature":"y"}'))).toBeNull();
    expect(unwrapEnvelope(Buffer.from('{"body":42,"signature":"y"}'))).toBeNull();
  });

  it('wrap + verify end-to-end — sign → wrap → unwrap → verify', () => {
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    const payload = freshPayload({ now });
    const { bodyBytes, signature } = hmac.sign(payload);
    const wire = wrapEnvelope({ bodyBytes, signature });
    const unwrapped = unwrapEnvelope(wire);
    const result = hmac.verify(unwrapped);
    expect(result.ok).toBe(true);
    expect(result.payload).toEqual(payload);
  });
});

describe('round-trip integration — sign then verify (steady-state handoff)', () => {
  it('signs a full handoff body and verifies it on the receiver side', () => {
    const now = 1_700_000_000_000;
    const { hmac: sender } = makeHmac({ clock: () => now });
    const { hmac: receiver } = makeHmac({ clock: () => now });

    const payload = {
      ts: now,
      nonce: sender.generateNonce(),
      active_instance_id: 'inst-A',
      peer_instance_id: 'inst-B',
      expected_version: 7,
    };
    const { bodyBytes, signature } = sender.sign(payload);
    const result = receiver.verify({ bodyBytes, signature });
    expect(result.ok).toBe(true);
    expect(result.payload).toEqual(payload);
  });
});
