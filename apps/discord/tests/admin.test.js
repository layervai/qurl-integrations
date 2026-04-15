/**
 * Tests for src/utils/admin.js
 */

// Mock config before requiring
jest.mock('../src/config', () => ({
  ADMIN_USER_IDS: ['admin-1', 'admin-2'],
}));

const { isAdmin, requireAdmin, replyPermissionDenied } = require('../src/utils/admin');

describe('admin utilities', () => {
  describe('isAdmin', () => {
    it('returns true for listed admin IDs', () => {
      expect(isAdmin('admin-1')).toBe(true);
      expect(isAdmin('admin-2')).toBe(true);
    });

    it('returns false for non-admin IDs', () => {
      expect(isAdmin('regular-user')).toBe(false);
      expect(isAdmin('')).toBe(false);
    });
  });

  describe('replyPermissionDenied', () => {
    it('replies with permission denied message', async () => {
      const interaction = {
        reply: jest.fn().mockResolvedValue(undefined),
      };

      await replyPermissionDenied(interaction);

      expect(interaction.reply).toHaveBeenCalledWith({
        content: expect.stringContaining('permission'),
        ephemeral: true,
      });
    });
  });

  describe('requireAdmin', () => {
    it('returns true for admin user without replying', async () => {
      const interaction = {
        user: { id: 'admin-1' },
        reply: jest.fn(),
      };

      const result = await requireAdmin(interaction);
      expect(result).toBe(true);
      expect(interaction.reply).not.toHaveBeenCalled();
    });

    it('returns false and replies for non-admin user', async () => {
      const interaction = {
        user: { id: 'non-admin' },
        reply: jest.fn().mockResolvedValue(undefined),
      };

      const result = await requireAdmin(interaction);
      expect(result).toBe(false);
      expect(interaction.reply).toHaveBeenCalledWith(
        expect.objectContaining({ ephemeral: true }),
      );
    });
  });
});
