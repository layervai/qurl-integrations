/**
 * Tests for src/orphan-token-sweeper.js — retry loop for OAuth tokens that
 * failed the initial `finally`-block revocation.
 */

jest.mock('../src/config', () => ({
  DATABASE_PATH: ':memory:',
  PENDING_LINK_EXPIRY_MINUTES: 30,
  GITHUB_CLIENT_ID: 'client-id',
  GITHUB_CLIENT_SECRET: 'client-secret',
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
}));

const db = require('../src/database');
const { sweepOnce } = require('../src/orphan-token-sweeper');

const originalFetch = globalThis.fetch;
afterAll(() => { globalThis.fetch = originalFetch; db.close(); });

describe('orphan token sweeper', () => {
  beforeEach(() => {
    // Wipe any prior rows from earlier tests.
    for (const r of db.listOrphanedTokens(1000)) db.deleteOrphanedToken(r.id);
  });

  it('deletes tokens that GitHub successfully revokes', async () => {
    db.recordOrphanedToken('gho_ok_1');
    db.recordOrphanedToken('gho_ok_2');
    globalThis.fetch = jest.fn().mockResolvedValue({ ok: true, status: 204 });

    await sweepOnce();

    expect(globalThis.fetch).toHaveBeenCalledTimes(2);
    expect(db.countOrphanedTokens()).toBe(0);
  });

  it('treats 404 (already-revoked) as success', async () => {
    db.recordOrphanedToken('gho_404');
    globalThis.fetch = jest.fn().mockResolvedValue({ ok: false, status: 404 });

    await sweepOnce();
    expect(db.countOrphanedTokens()).toBe(0);
  });

  it('leaves rows in place when GitHub is unhappy (retry next sweep)', async () => {
    db.recordOrphanedToken('gho_500');
    globalThis.fetch = jest.fn().mockResolvedValue({ ok: false, status: 500 });

    await sweepOnce();
    expect(db.countOrphanedTokens()).toBe(1);
  });

  it('catches fetch rejection and logs (row survives)', async () => {
    db.recordOrphanedToken('gho_net');
    globalThis.fetch = jest.fn().mockRejectedValue(new Error('network'));

    await sweepOnce();
    expect(db.countOrphanedTokens()).toBe(1);
  });

  it('is a no-op when the queue is empty', async () => {
    globalThis.fetch = jest.fn();
    await sweepOnce();
    expect(globalThis.fetch).not.toHaveBeenCalled();
  });
});
