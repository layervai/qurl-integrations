/**
 * Tests for src/routes/oauth.js — covers rate limiting, OAuth flow, callback,
 * and historical contribution checking.
 */

jest.mock('../src/discord', () => ({
  assignContributorRole: jest.fn().mockResolvedValue({ success: true }),
  notifyPRMerge: jest.fn(),
  notifyBadgeEarned: jest.fn(),
  sendDM: jest.fn().mockResolvedValue(true),
}));

jest.mock('../src/database', () => ({
  getPendingLink: jest.fn(),
  deletePendingLink: jest.fn(),
  consumePendingLink: jest.fn(),
  createLink: jest.fn(),
  getLinkByGithub: jest.fn(),
  recordContribution: jest.fn(() => true),
  checkAndAwardBadges: jest.fn(() => []),
  getStats: jest.fn(() => ({ linkedUsers: 0, totalContributions: 0, uniqueContributors: 0, byRepo: [] })),
}));

process.env.GITHUB_CLIENT_ID = 'test-client-id';
process.env.GITHUB_CLIENT_SECRET = 'test-client-secret';
process.env.GITHUB_WEBHOOK_SECRET = 'test-webhook-secret';
process.env.BASE_URL = 'http://localhost:3000';

const request = require('supertest');
const { app } = require('../src/server');
const db = require('../src/database');
const discord = require('../src/discord');

const originalFetch = globalThis.fetch;

afterEach(() => {
  jest.clearAllMocks();
  globalThis.fetch = originalFetch;
});

describe('OAuth routes', () => {
  describe('GET /auth/github', () => {
    it('returns 400 for missing state', async () => {
      const res = await request(app).get('/auth/github');
      expect(res.status).toBe(400);
      expect(res.text).toContain('Invalid Link');
    });

    it('returns 400 for expired state', async () => {
      db.getPendingLink.mockReturnValue(null);
      const res = await request(app).get('/auth/github?state=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0');
      expect(res.status).toBe(400);
      expect(res.text).toContain('Link Expired');
    });

    it('redirects to GitHub for valid state', async () => {
      db.getPendingLink.mockReturnValue({ discord_id: '123' });
      const res = await request(app).get('/auth/github?state=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1');
      expect(res.status).toBe(302);
      expect(res.headers.location).toContain('github.com/login/oauth/authorize');
    });
  });

  describe('GET /auth/github/callback', () => {
    it('handles OAuth error parameter', async () => {
      const res = await request(app).get('/auth/github/callback?error=access_denied&error_description=User+denied');
      expect(res.status).toBe(400);
      expect(res.text).toContain('Authorization Denied');
    });

    it('rejects missing code or state', async () => {
      const res = await request(app).get('/auth/github/callback');
      expect(res.status).toBe(400);
      expect(res.text).toContain('Invalid Request');
    });

    it('rejects expired state in callback', async () => {
      db.consumePendingLink.mockReturnValue(null);
      const res = await request(app).get('/auth/github/callback?code=test-code&state=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0');
      expect(res.status).toBe(400);
      expect(res.text).toContain('Session Expired');
    });

    it('handles GitHub token exchange error', async () => {
      db.consumePendingLink.mockReturnValue({ discord_id: '123' });
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        json: async () => ({ error: 'bad_verification_code', error_description: 'Code expired' }),
      });

      const res = await request(app).get('/auth/github/callback?code=bad-code&state=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1');
      expect(res.status).toBe(400);
      expect(res.text).toContain('GitHub Error');
    });

    it('handles missing user login', async () => {
      db.consumePendingLink.mockReturnValue({ discord_id: '123' });
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({ ok: true, json: async () => ({ access_token: 'token123' }) })
        .mockResolvedValueOnce({ ok: true, json: async () => ({ id: 1 }) }); // no login field

      const res = await request(app).get('/auth/github/callback?code=good&state=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1');
      expect(res.status).toBe(400);
      expect(res.text).toContain('Failed to Get User Info');
    });

    it('completes OAuth flow with no historical contributions', async () => {
      db.consumePendingLink.mockReturnValue({ discord_id: '123' });
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({ ok: true, json: async () => ({ access_token: 'token123' }) })
        .mockResolvedValueOnce({ ok: true, json: async () => ({ login: 'ghuser' }) })
        // Historical PR search returns empty
        .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [] }) });

      const res = await request(app).get('/auth/github/callback?code=good&state=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1');
      expect(res.status).toBe(200);
      expect(res.text).toContain('Linked Successfully');
      expect(db.createLink).toHaveBeenCalledWith('123', 'ghuser');
      expect(db.consumePendingLink).toHaveBeenCalledWith('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1');
      expect(discord.sendDM).toHaveBeenCalled();
    });

    it('completes OAuth flow with historical contributions', async () => {
      db.consumePendingLink.mockReturnValue({ discord_id: '456' });
      db.recordContribution.mockReturnValue(true);
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({ ok: true, json: async () => ({ access_token: 'token456' }) })
        .mockResolvedValueOnce({ ok: true, json: async () => ({ login: 'contributor' }) })
        // Historical PR search returns PRs
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({
            items: [
              { number: 10, repository_url: 'https://api.github.com/repos/OpenNHP/opennhp', title: 'Fix thing', html_url: 'https://github.com/pr/10', closed_at: '2025-01-01' },
              { number: 11, repository_url: 'https://api.github.com/repos/OpenNHP/opennhp', title: 'Add feature', html_url: 'https://github.com/pr/11', closed_at: '2025-02-01' },
            ],
          }),
        });

      const res = await request(app).get('/auth/github/callback?code=good&state=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa2');
      expect(res.status).toBe(200);
      expect(res.text).toContain('Linked Successfully');
      expect(res.text).toContain('past contribution');
      expect(db.recordContribution).toHaveBeenCalled();
    });

    it('handles fetch exception during token exchange', async () => {
      db.consumePendingLink.mockReturnValue({ discord_id: '789' });
      globalThis.fetch = jest.fn().mockRejectedValue(new Error('Network error'));

      const res = await request(app).get('/auth/github/callback?code=good&state=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa3');
      expect(res.status).toBe(500);
      expect(res.text).toContain('Something Went Wrong');
    });

    it('handles historical check failure gracefully', async () => {
      db.consumePendingLink.mockReturnValue({ discord_id: '101' });
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({ ok: true, json: async () => ({ access_token: 'tok' }) })
        .mockResolvedValueOnce({ ok: true, json: async () => ({ login: 'user2' }) })
        // Historical check: HTTP error
        .mockResolvedValueOnce({ ok: false, status: 403 });

      const res = await request(app).get('/auth/github/callback?code=good&state=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa4');
      expect(res.status).toBe(200);
      expect(res.text).toContain('Linked Successfully');
    });

    it('awards badges during historical check', async () => {
      db.consumePendingLink.mockReturnValue({ discord_id: '202' });
      db.checkAndAwardBadges.mockReturnValue(['first_pr']);
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({ ok: true, json: async () => ({ access_token: 'tok2' }) })
        .mockResolvedValueOnce({ ok: true, json: async () => ({ login: 'badgeuser' }) })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({
            items: [{ number: 1, repository_url: 'https://api.github.com/repos/OpenNHP/opennhp', title: 'PR', html_url: 'url', closed_at: '2025-01-01' }],
          }),
        });

      const res = await request(app).get('/auth/github/callback?code=good&state=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa5');
      expect(res.status).toBe(200);
      expect(discord.notifyBadgeEarned).toHaveBeenCalled();
    });
  });

  describe('rate limiting', () => {
    it('allows requests within limit', async () => {
      db.consumePendingLink.mockReturnValue(null);
      // Send several requests within the rate limit window
      for (let i = 0; i < 5; i++) {
        const res = await request(app).get('/auth/github?state=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa6');
        expect(res.status).toBeLessThan(429);
      }
    });
  });
});
