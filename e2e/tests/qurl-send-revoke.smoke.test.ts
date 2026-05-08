/**
 * Wording-drift smoke for the post-revoke confirmation message.
 * Imports the discord.js-free `revoke-render` module so the e2e
 * runner can load it without `apps/discord/node_modules`.
 */

// eslint-disable-next-line @typescript-eslint/no-var-requires
const { renderRevokeContent, REVOKE_TRUNC_LIMIT } = require('../../apps/discord/src/revoke-render');

describe('qURL send revoke confirmation format (smoke)', () => {
  test('full-list format: "Revoked X/Y users" + "Revoked for: alice, bob"', () => {
    const r = renderRevokeContent({ names: ['alice', 'bob', 'carol'], total: 3, showAll: false });
    expect(r.content).toMatch(/^Revoked 3\/3 users\./);
    expect(r.content).toContain('Revoked for: alice, bob, carol');
    expect(r.content).not.toMatch(/\+\d+ more/);
  });

  test(`truncated format: "+N more" when names exceed REVOKE_TRUNC_LIMIT (${REVOKE_TRUNC_LIMIT})`, () => {
    const names = Array.from({ length: REVOKE_TRUNC_LIMIT + 3 }, (_, i) => `u${i}`);
    const r = renderRevokeContent({ names, total: names.length, showAll: false });
    expect(r.content).toMatch(/\+3 more$/m);
    expect(r.needsExpand).toBe(true);
  });

  test('no-success format: "Revoked 0/N" omits "Revoked for:" line', () => {
    const r = renderRevokeContent({ names: [], total: 5, showAll: false });
    expect(r.content).toMatch(/^Revoked 0\/5 users\./);
    expect(r.content).not.toContain('Revoked for:');
  });

  test('zero-attempt format: "Revoked 0/0" omits the already-opened note', () => {
    const r = renderRevokeContent({ names: [], total: 0, showAll: false });
    expect(r.content).not.toContain('already-opened');
  });

  test('singular "user" when total === 1', () => {
    const r = renderRevokeContent({ names: ['alice'], total: 1, showAll: false });
    expect(r.content).toMatch(/^Revoked 1\/1 user\./);
    expect(r.content).not.toMatch(/^Revoked 1\/1 users\./);
  });

  test('large lists overflow to file attachment instead of inline truncation', () => {
    const names = Array.from({ length: 200 }, (_, i) => `verylongusername${String(i).padStart(4, '0')}`);
    const r = renderRevokeContent({ names, total: names.length, showAll: true });
    expect(r.content.length).toBeLessThanOrEqual(2000);
    expect(r.content).toContain('(see attached)');
    expect(r.attachmentText).not.toBeNull();
    expect(r.attachmentText.split('\n')).toHaveLength(200);
  });
});
