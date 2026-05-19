/**
 * Tests for src/orphan-token-sweeper.js — retry loop for OAuth tokens that
 * failed the initial `finally`-block revocation.
 *
 * Strategy: mock `../src/store` with a stateful in-memory backing Map so
 * the sweeper exercises the real contract (list → decrypt → delete) and
 * each test can assert on row counts after a sweep. An aws-sdk-client-mock
 * setup against the real ddb-store would buy fidelity to the DDB shape
 * but is overkill here — the sweeper only depends on the Store contract,
 * which is unit-tested separately in `ddb-store.test.js` + `store-contract.test.js`.
 */

// Stateful in-memory backing store, addressed by SHA-256 of plaintext to
// match the real `recordOrphanedToken` dedup contract. The `mock` prefix
// satisfies Jest's hoisted-factory variable-allowlist (the factory below
// runs before the test file's top-level body, so any referenced symbol
// has to be either Node-global or a `mock*`-prefixed name).
const mockOrphanedTokens = new Map();

jest.mock('../src/config', () => ({
  PENDING_LINK_EXPIRY_MINUTES: 30,
  GITHUB_CLIENT_ID: 'client-id',
  GITHUB_CLIENT_SECRET: 'client-secret',
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

jest.mock('../src/store', () => {
  // `require('crypto')` inside the factory rather than via a top-level
  // import — the factory is hoisted, so top-level requires aren't in
  // scope yet at evaluation time.
  const cryptoMod = require('crypto');
  return {
    // Surface the Store contract surface the sweeper actually touches.
    // Each method's behavior mirrors the ddb-store implementation closely
    // enough to exercise the sweeper's branches without bringing in a
    // real DDB endpoint.
    recordOrphanedToken: jest.fn(async (token) => {
      const id = cryptoMod.createHash('sha256').update(token).digest('hex');
      // Idempotent on duplicate plaintext — matches the real CCFE-swallow
      // path in ddb-store's recordOrphanedToken.
      if (!mockOrphanedTokens.has(id)) mockOrphanedTokens.set(id, token);
    }),
    listOrphanedTokens: jest.fn(async (limit) => {
      return Array.from(mockOrphanedTokens.entries())
        .slice(0, limit)
        .map(([id, plaintext]) => ({ id, encryptedAccessToken: plaintext }));
    }),
    countOrphanedTokens: jest.fn(async () => mockOrphanedTokens.size),
    decryptOrphanedToken: jest.fn(async (ciphertext) => ciphertext),
    deleteOrphanedToken: jest.fn(async (id) => { mockOrphanedTokens.delete(id); }),
    close: jest.fn(),
  };
});

const db = require('../src/store');
const { sweepOnce } = require('../src/orphan-token-sweeper');

const originalFetch = globalThis.fetch;
afterAll(() => {
  globalThis.fetch = originalFetch;
});

describe('orphan token sweeper', () => {
  beforeEach(() => {
    mockOrphanedTokens.clear();
  });

  it('deletes tokens that GitHub successfully revokes', async () => {
    await db.recordOrphanedToken('gho_ok_1');
    await db.recordOrphanedToken('gho_ok_2');
    globalThis.fetch = jest.fn().mockResolvedValue({ ok: true, status: 204 });

    await sweepOnce();

    expect(globalThis.fetch).toHaveBeenCalledTimes(2);
    expect(await db.countOrphanedTokens()).toBe(0);
  });

  it('treats 404 (already-revoked) as success', async () => {
    await db.recordOrphanedToken('gho_404');
    globalThis.fetch = jest.fn().mockResolvedValue({ ok: false, status: 404 });

    await sweepOnce();
    expect(await db.countOrphanedTokens()).toBe(0);
  });

  it('leaves rows in place when GitHub is unhappy (retry next sweep)', async () => {
    await db.recordOrphanedToken('gho_500');
    globalThis.fetch = jest.fn().mockResolvedValue({ ok: false, status: 500 });

    await sweepOnce();
    expect(await db.countOrphanedTokens()).toBe(1);
  });

  it('catches fetch rejection and logs (row survives)', async () => {
    await db.recordOrphanedToken('gho_net');
    globalThis.fetch = jest.fn().mockRejectedValue(new Error('network'));

    await sweepOnce();
    expect(await db.countOrphanedTokens()).toBe(1);
  });

  it('is a no-op when the queue is empty', async () => {
    globalThis.fetch = jest.fn();
    await sweepOnce();
    expect(globalThis.fetch).not.toHaveBeenCalled();
  });

  it('aborts the batch on 429 rate limit, leaves remaining rows for the next sweep', async () => {
    // Pin the rate-limit branch (`orphan-token-sweeper.js:58-68`):
    // when GitHub returns 429 / 403, the sweeper logs, applies
    // exponential backoff, and aborts the rest of the batch so the
    // next hourly sweep can retry. Without this test, a regression
    // that drops the `break` would silently amplify the rate-limit
    // hit by churning through the full batch on every sweep.
    await db.recordOrphanedToken('gho_first');
    await db.recordOrphanedToken('gho_second');
    globalThis.fetch = jest.fn().mockResolvedValueOnce({ ok: false, status: 429 });

    await sweepOnce();

    // Batch aborted after the first row hits 429 — second row was
    // never attempted. Both rows survive for the next sweep.
    expect(globalThis.fetch).toHaveBeenCalledTimes(1);
    expect(await db.countOrphanedTokens()).toBe(2);
  });

  it('aborts the batch on 403 rate limit (secondary rate-limit shape)', async () => {
    // GitHub's secondary rate limit returns 403 (not 429); the
    // sweeper treats both as retryable. Same shape as the 429 test
    // above — pinned separately so a future refactor that
    // accidentally narrows the check to just `status === 429` is
    // caught.
    await db.recordOrphanedToken('gho_403');
    globalThis.fetch = jest.fn().mockResolvedValueOnce({ ok: false, status: 403 });

    await sweepOnce();

    expect(globalThis.fetch).toHaveBeenCalledTimes(1);
    expect(await db.countOrphanedTokens()).toBe(1);
  });

  it('skips a row whose decrypt throws and leaves it in place (will retry next sweep)', async () => {
    // Pre-PR (when the test used the real SQLite layer) this branch
    // was covered by a corrupt-ciphertext / missing-KEK row in the
    // seeded fixture. The in-memory mock here skips the
    // encrypt/decrypt roundtrip, so the only way to exercise
    // `orphan-token-sweeper.js`'s `db.decryptOrphanedToken` catch
    // branch (line 48-50) is to override the mock to reject. We
    // verify the row survives AND that fetch was never called —
    // a decrypt failure must NOT silently issue a revoke for the
    // wrong token.
    await db.recordOrphanedToken('gho_corrupt');
    db.decryptOrphanedToken.mockRejectedValueOnce(new Error('bad ciphertext'));
    globalThis.fetch = jest.fn();

    await sweepOnce();

    expect(globalThis.fetch).not.toHaveBeenCalled();
    expect(await db.countOrphanedTokens()).toBe(1);
  });
});
