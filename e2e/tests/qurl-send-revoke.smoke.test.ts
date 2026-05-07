/**
 * Smoke contract for the post-revoke confirmation message format
 * (`/qurl send` → click Revoke → bot edits the ephemeral confirmation).
 *
 * Imports `renderRevokeMsg` from the bot src so a wording change in
 * commands.js fails this smoke gate at deploy time. Without the import
 * (i.e., asserting hand-rolled strings) the smoke would be fixture-only
 * and miss the regression it claims to catch.
 *
 * The unit tests in apps/discord/tests/qurl-send-back-half.test.js
 * cover the same surface during the bot's own CI. This file's value
 * is at the post-deploy smoke layer (qurl-integrations-infra
 * promote-bot-discord-to-prod → e2e-smoke).
 */

// eslint-disable-next-line @typescript-eslint/no-var-requires
const { _test } = require('../../apps/discord/src/commands');
const { renderRevokeMsg, REVOKE_TRUNC_LIMIT } = _test;

describe('qURL send revoke confirmation format (smoke)', () => {
  test('full-list format: "Revoked X/Y links" + "Revoked for: alice, bob"', () => {
    const r = renderRevokeMsg('send-1', ['alice', 'bob', 'carol'], 3, false, 3);
    expect(r.content).toMatch(/^Revoked 3\/3 links\./);
    expect(r.content).toContain('Revoked for: alice, bob, carol');
    expect(r.content).not.toMatch(/\+\d+ more/);
  });

  test(`truncated format: "+N more" when names exceed REVOKE_TRUNC_LIMIT (${REVOKE_TRUNC_LIMIT})`, () => {
    const names = Array.from({ length: REVOKE_TRUNC_LIMIT + 3 }, (_, i) => `u${i}`);
    const r = renderRevokeMsg('send-2', names, names.length, false, names.length);
    expect(r.content).toMatch(/\+3 more$/m);
    expect(r.needsExpand).toBe(true);
  });

  test('no-success format: "Revoked 0/N" omits "Revoked for:" line', () => {
    const r = renderRevokeMsg('send-3', [], 5, false, 0);
    expect(r.content).toMatch(/^Revoked 0\/5 links\./);
    expect(r.content).not.toContain('Revoked for:');
  });

  test('zero-attempt format: "Revoked 0/0" omits the already-opened note', () => {
    const r = renderRevokeMsg('send-4', [], 0, false, 0);
    expect(r.content).not.toContain('already-opened');
  });

  test('singular "link" when total === 1', () => {
    const r = renderRevokeMsg('send-5', ['alice'], 1, false, 1);
    expect(r.content).toMatch(/^Revoked 1\/1 link\./);
    expect(r.content).not.toMatch(/^Revoked 1\/1 links\./);
  });

  test('row-count successCount diverges from names.length on multi-row sends', () => {
    // 1 unique recipient (file + location = 2 rows), both rows revoked.
    // User-facing message shows "2/2 links" (rows), not "1/2".
    const r = renderRevokeMsg('send-6', ['alice'], 2, false, 2);
    expect(r.content).toContain('Revoked 2/2 links');
    expect(r.content).toContain('Revoked for: alice');
  });
});
