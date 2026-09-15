
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

  it('throws on non-positive freshnessWindowMs (every body would 401-stale)', () => {
    const logger = { info() {}, warn() {}, error() {}, debug() {} };
    const baseArgs = { secrets: { current: 'x' }, logger };
    expect(() => createGatewayHmac({ ...baseArgs, freshnessWindowMs: 0 }))
      .toThrow(/freshnessWindowMs.*positive integer/);
    expect(() => createGatewayHmac({ ...baseArgs, freshnessWindowMs: -1 }))
      .toThrow(/freshnessWindowMs.*positive integer/);
    expect(() => createGatewayHmac({ ...baseArgs, freshnessWindowMs: 1.5 }))
      .toThrow(/freshnessWindowMs.*positive integer/);
    expect(() => createGatewayHmac({ ...baseArgs, freshnessWindowMs: '5000' }))
      .toThrow(/freshnessWindowMs.*positive integer/);
  });

  it('throws on non-positive nonceLruSize — would silently disable replay protection', () => {
    const logger = { info() {}, warn() {}, error() {}, debug() {} };
    const baseArgs = { secrets: { current: 'x' }, logger };
    expect(() => createGatewayHmac({ ...baseArgs, nonceLruSize: 0 }))
      .toThrow(/nonceLruSize.*positive integer/);
    expect(() => createGatewayHmac({ ...baseArgs, nonceLruSize: -1 }))
      .toThrow(/nonceLruSize.*positive integer/);
    expect(() => createGatewayHmac({ ...baseArgs, nonceLruSize: 1.5 }))
      .toThrow(/nonceLruSize.*positive integer/);
    expect(() => createGatewayHmac({ ...baseArgs, nonceLruSize: '1024' }))
      .toThrow(/nonceLruSize.*positive integer/);
  });

  it('rejects secrets.previous = "" (empty string) — would silently disable dual-accept', () => {
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
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    const { bodyBytes } = hmac.sign(freshPayload({ now }));
    expect(() => hmac.verify({ bodyBytes, signature: 'short' })).not.toThrow();
    expect(hmac.verify({ bodyBytes, signature: 'short' }))
      .toEqual({ ok: false, reason: 'bad_signature' });
  });

  it('rejects non-hex signature WITHOUT throwing', () => {
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

  it('rejects a body where ts is Infinity (JSON.parse of `1e1000` → Infinity, typeof === number)', () => {
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    const bodyBytes = Buffer.from('{"ts":1e1000,"nonce":"nnnnnnnnnnnnnnnnnnnnnnnnnnnnnnnn"}', 'utf8');
    expect(JSON.parse(bodyBytes.toString('utf8')).ts).toBe(Infinity); // sanity
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
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now });
    const payload = freshPayload({ now });
    const bodyBytes = Buffer.from(JSON.stringify(payload), 'utf8');
    expect(hmac.verify({ bodyBytes, signature: 'f'.repeat(64) }))
      .toEqual({ ok: false, reason: 'bad_signature' });
    const goodSig = crypto.createHmac('sha256', SECRET_CURRENT)
      .update(bodyBytes).digest('hex');
    expect(hmac.verify({ bodyBytes, signature: goodSig })).toEqual({ ok: true, payload });
  });

  it('does not burn the nonce when stale (freshness rejection precedes nonce remember)', () => {
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

    expect(signAndVerify('1').ok).toBe(true);
    expect(signAndVerify('2').ok).toBe(true);
    expect(signAndVerify('3').ok).toBe(true);

    expect(signAndVerify('4').ok).toBe(true);

    expect(signAndVerify('2')).toEqual({ ok: false, reason: 'replay' });

    expect(signAndVerify('1').ok).toBe(true);
  });

  it('enforces the size cap — verifying more nonces than the LRU holds settles at cap, not above', () => {
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

  it('an evicted nonce is accepted again (size cap is a deliberate ceiling, not a correctness primitive)', async () => {
    const now = 1_700_000_000_000;
    const { hmac } = makeHmac({ clock: () => now, nonceLruSize: 3 });

    function verifyNonce(nonceChar) {
      const payload = { ts: now, nonce: nonceChar.repeat(32) };
      const bodyBytes = Buffer.from(JSON.stringify(payload), 'utf8');
      const signature = crypto.createHmac('sha256', SECRET_CURRENT)
        .update(bodyBytes).digest('hex');
      return hmac.verify({ bodyBytes, signature });
    }

    expect(verifyNonce('1').ok).toBe(true);   // [1]
    expect(verifyNonce('2').ok).toBe(true);   // [1, 2]
    expect(verifyNonce('3').ok).toBe(true);   // [1, 2, 3]
    expect(verifyNonce('4').ok).toBe(true);   // [2, 3, 4] (evicted 1)
    expect(verifyNonce('1').ok).toBe(true);   // [3, 4, 1] (evicted 2)
    expect(verifyNonce('3')).toEqual({ ok: false, reason: 'replay' });
  });

  it('check-then-set is synchronous — two parallel verifies of the same nonce produce exactly one ok:true', async () => {
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
  it('wire envelope key order is exactly [body, signature]', () => {
    const wire = wrapEnvelope({
      bodyBytes: Buffer.from('{"a":1}', 'utf8'),
      signature: 'sig',
    });
    const parsed = JSON.parse(wire.toString('utf8'));
    expect(Object.keys(parsed)).toEqual(['body', 'signature']);
  });

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
