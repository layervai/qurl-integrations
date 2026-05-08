/**
 * Smoke contract for the post-revoke confirmation message format
 * (`/qurl send` → click Revoke → bot edits the ephemeral confirmation).
 *
 * Imports `renderRevokeMsg` from the bot src so a wording change in
 * commands.js fails this smoke gate at deploy time.
 *
 * The unit tests in apps/discord/tests/qurl-send-back-half.test.js
 * cover the same surface during the bot's own CI. This file's value
 * is at the post-deploy smoke layer.
 */

// eslint-disable-next-line @typescript-eslint/no-var-requires
const { _test } = require('../../apps/discord/src/commands');
const { renderRevokeMsg, REVOKE_TRUNC_LIMIT } = _test;

describe('qURL send revoke confirmation format (smoke)', () => {
  test('full-list format: "Revoked X/Y users" + "Revoked for: alice, bob"', () => {
    const r = renderRevokeMsg('send-1', ['alice', 'bob', 'carol'], 3, false);
    expect(r.content).toMatch(/^Revoked 3\/3 users\./);
    expect(r.content).toContain('Revoked for: alice, bob, carol');
    expect(r.content).not.toMatch(/\+\d+ more/);
  });

  test(`truncated format: "+N more" when names exceed REVOKE_TRUNC_LIMIT (${REVOKE_TRUNC_LIMIT})`, () => {
    const names = Array.from({ length: REVOKE_TRUNC_LIMIT + 3 }, (_, i) => `u${i}`);
    const r = renderRevokeMsg('send-2', names, names.length, false);
    expect(r.content).toMatch(/\+3 more$/m);
    expect(r.needsExpand).toBe(true);
  });

  test('no-success format: "Revoked 0/N" omits "Revoked for:" line', () => {
    const r = renderRevokeMsg('send-3', [], 5, false);
    expect(r.content).toMatch(/^Revoked 0\/5 users\./);
    expect(r.content).not.toContain('Revoked for:');
  });

  test('zero-attempt format: "Revoked 0/0" omits the already-opened note', () => {
    const r = renderRevokeMsg('send-4', [], 0, false);
    expect(r.content).not.toContain('already-opened');
  });

  test('singular "user" when total === 1', () => {
    const r = renderRevokeMsg('send-5', ['alice'], 1, false);
    expect(r.content).toMatch(/^Revoked 1\/1 user\./);
    expect(r.content).not.toMatch(/^Revoked 1\/1 users\./);
  });

  test('large lists overflow to file attachment instead of inline truncation', () => {
    const names = Array.from({ length: 200 }, (_, i) => `verylongusername${String(i).padStart(4, '0')}`);
    const r = renderRevokeMsg('send-6', names, names.length, true);
    expect(r.content.length).toBeLessThanOrEqual(2000);
    expect(r.content).toContain('(see attached)');
    expect(r.attachmentText).not.toBeNull();
    expect(r.attachmentText.split('\n')).toHaveLength(200);
  });
});
