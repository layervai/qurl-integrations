const crypto = require('crypto');

// Mock dependencies before requiring modules
jest.mock('../src/discord', () => ({
  assignContributorRole: jest.fn(),
  notifyPRMerge: jest.fn(),
  notifyBadgeEarned: jest.fn(),
  postGoodFirstIssue: jest.fn(),
  postReleaseAnnouncement: jest.fn(),
  postStarMilestone: jest.fn(),
  postToGitHubFeed: jest.fn(),
  sendDM: jest.fn(),
}));

jest.mock('../src/database', () => ({
  getPendingLink: jest.fn(),
  deletePendingLink: jest.fn(),
  createLink: jest.fn(),
  getLinkByGithub: jest.fn(),
  recordContribution: jest.fn(() => true),
  checkAndAwardBadges: jest.fn(() => []),
  awardFirstIssueBadge: jest.fn(() => []),
  hasMilestoneBeenAnnounced: jest.fn(() => false),
  recordMilestone: jest.fn(() => true),
  getStats: jest.fn(() => ({
    linkedUsers: 5,
    totalContributions: 10,
    uniqueContributors: 3,
    byRepo: [],
  })),
  healthCheck: jest.fn(() => ({ ok: true })),
}));

// Set required env vars. GUILD_ID must be a valid Discord snowflake AND
// ENABLE_OPENNHP_FEATURES must be "true" to put the bot in OpenNHP mode
// so /auth and /webhook routes are mounted — this suite tests those
// routes.
process.env.GUILD_ID = '123456789012345678';
process.env.ENABLE_OPENNHP_FEATURES = 'true';
process.env.GITHUB_CLIENT_ID = 'test-client-id';
process.env.GITHUB_CLIENT_SECRET = 'test-client-secret';
process.env.GITHUB_WEBHOOK_SECRET = 'test-webhook-secret';
process.env.BASE_URL = 'http://localhost:3000';

const request = require('supertest');
const { app } = require('../src/server');
const db = require('../src/database');
const discord = require('../src/discord');

describe('Server', () => {
  beforeEach(() => {
    jest.clearAllMocks();
  });

  describe('GET /', () => {
    it('returns health check', async () => {
      const res = await request(app).get('/');
      expect(res.status).toBe(200);
      expect(res.body.status).toBe('ok');
      expect(res.body.service).toBe('qURL Discord Bot');
    });
  });

  describe('GET /health', () => {
    it('calls db.healthCheck (NOT db.getStats — the latter scans on DDB)', async () => {
      const res = await request(app).get('/health');
      expect(res.status).toBe(200);
      expect(res.body.status).toBe('ok');
      expect(db.healthCheck).toHaveBeenCalledTimes(1);
      // Critical regression guard: at LB cadence /health must never
      // hit the aggregation path. getStats() on the DDB backend is
      // a paginated full-table Scan whose cost grows with table
      // size. If a future refactor wires getStats() back into
      // /health, this assertion fires.
      expect(db.getStats).not.toHaveBeenCalled();
    });

    it('returns 503 when db.healthCheck throws', async () => {
      db.healthCheck.mockImplementationOnce(() => { throw new Error('db unreachable'); });
      const res = await request(app).get('/health');
      expect(res.status).toBe(503);
      expect(res.body.status).toBe('unhealthy');
      // Don't leak backend internals — only the high-level status
      // surfaces to the unauthenticated probe.
      expect(res.body.error).toBeUndefined();
    });
  });

  describe('GET /metrics', () => {
    afterEach(() => { delete process.env.METRICS_TOKEN; });

    it('returns 503 when METRICS_TOKEN unset (default-deny)', async () => {
      const res = await request(app).get('/metrics');
      expect(res.status).toBe(503);
      expect(res.body.error).toBe('Metrics not configured');
    });

    it('returns 401 when token configured but wrong/missing auth', async () => {
      process.env.METRICS_TOKEN = 'secret-token';
      const res = await request(app).get('/metrics');
      expect(res.status).toBe(401);
    });

    it('returns metrics when auth matches configured token', async () => {
      process.env.METRICS_TOKEN = 'secret-token';
      const res = await request(app).get('/metrics').set('Authorization', 'Bearer secret-token');
      expect(res.status).toBe(200);
      expect(res.body.status).toBe('ok');
      expect(res.body.stats).toBeDefined();
      expect(res.body.uptime).toBeDefined();
    });

    it('returns 429 after exceeding the per-IP rate limit', async () => {
      process.env.METRICS_TOKEN = 'secret-token';
      // 30/min/IP is the limit — fire 31 and expect the last to 429
      let last;
      for (let i = 0; i < 31; i++) {
        last = await request(app).get('/metrics').set('Authorization', 'Bearer secret-token');
      }
      expect(last.status).toBe(429);
      expect(last.body.error).toBe('Rate limit exceeded');
    });
  });

  describe('GET /auth/github', () => {
    it('rejects missing state', async () => {
      const res = await request(app).get('/auth/github');
      expect(res.status).toBe(400);
      expect(res.text).toContain('Invalid Link');
    });

    it('rejects invalid state format', async () => {
      const res = await request(app).get('/auth/github?state=invalid');
      expect(res.status).toBe(400);
      expect(res.text).toContain('Invalid Link');
    });

    it('rejects valid-format state not in DB', async () => {
      db.getPendingLink.mockReturnValue(null);
      const res = await request(app).get('/auth/github?state=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa');
      expect(res.status).toBe(400);
      expect(res.text).toContain('Link Expired');
    });

    it('redirects to GitHub with valid state', async () => {
      db.getPendingLink.mockReturnValue({ discord_id: '123' });
      const res = await request(app).get('/auth/github?state=aabbccddaabbccddaabbccddaabbccdd.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa');
      expect(res.status).toBe(302);
      expect(res.headers.location).toContain('github.com/login/oauth/authorize');
    });
  });

  describe('POST /webhook/github', () => {
    function signPayload(payload, secret = 'test-webhook-secret') {
      const hmac = crypto.createHmac('sha256', secret);
      const body = JSON.stringify(payload);
      return 'sha256=' + hmac.update(body).digest('hex');
    }

    it('rejects missing signature', async () => {
      const res = await request(app)
        .post('/webhook/github')
        .send({});
      expect(res.status).toBe(401);
    });

    it('rejects invalid signature', async () => {
      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', 'sha256=invalid')
        .set('x-github-event', 'pull_request')
        .send({ action: 'opened' });
      expect(res.status).toBe(401);
    });

    it('handles unknown events gracefully', async () => {
      const payload = { action: 'created' };
      const signature = signPayload(payload);

      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signature)
        .set('x-github-event', 'unknown_event')
        .send(payload);

      expect(res.status).toBe(200);
      expect(discord.assignContributorRole).not.toHaveBeenCalled();
    });

    it('ignores non-merged PRs', async () => {
      const payload = {
        action: 'opened',
        pull_request: {
          number: 1,
          title: 'Test',
          html_url: 'https://github.com/OpenNHP/opennhp/pull/1',
          user: { login: 'testuser' },
          merged: false,
        },
        repository: { full_name: 'OpenNHP/opennhp' },
      };
      const signature = signPayload(payload);

      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signature)
        .set('x-github-event', 'pull_request')
        .send(payload);

      expect(res.status).toBe(200);
      expect(discord.assignContributorRole).not.toHaveBeenCalled();
      // Should post to activity feed for opened PRs
      expect(discord.postToGitHubFeed).toHaveBeenCalled();
    });

    it('ignores non-OpenNHP repos', async () => {
      const payload = {
        action: 'closed',
        pull_request: {
          merged: true,
          number: 123,
          title: 'Test PR',
          html_url: 'https://github.com/other/repo/pull/123',
          user: { login: 'testuser' },
        },
        repository: { full_name: 'other/repo' },
      };
      const signature = signPayload(payload);

      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signature)
        .set('x-github-event', 'pull_request')
        .send(payload);

      expect(res.status).toBe(200);
      expect(res.text).toContain('ignored');
      expect(discord.assignContributorRole).not.toHaveBeenCalled();
    });

    it('assigns role for linked user', async () => {
      db.getLinkByGithub.mockReturnValue({
        discord_id: '123456',
        github_username: 'testuser',
      });
      discord.assignContributorRole.mockResolvedValue({ success: true });

      const payload = {
        action: 'closed',
        pull_request: {
          merged: true,
          number: 123,
          title: 'Add feature',
          html_url: 'https://github.com/OpenNHP/opennhp/pull/123',
          user: { login: 'testuser' },
        },
        repository: { full_name: 'OpenNHP/opennhp' },
      };
      const signature = signPayload(payload);

      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signature)
        .set('x-github-event', 'pull_request')
        .send(payload);

      expect(res.status).toBe(200);
      expect(discord.assignContributorRole).toHaveBeenCalledWith(
        '123456',
        123,
        'OpenNHP/opennhp',
        'testuser'
      );
      expect(db.recordContribution).toHaveBeenCalled();
      expect(db.checkAndAwardBadges).toHaveBeenCalled();
    });

    it('notifies for unlinked user', async () => {
      db.getLinkByGithub.mockReturnValue(null);

      const payload = {
        action: 'closed',
        pull_request: {
          merged: true,
          number: 456,
          title: 'Fix bug',
          html_url: 'https://github.com/OpenNHP/opennhp/pull/456',
          user: { login: 'newuser' },
        },
        repository: { full_name: 'OpenNHP/opennhp' },
      };
      const signature = signPayload(payload);

      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signature)
        .set('x-github-event', 'pull_request')
        .send(payload);

      expect(res.status).toBe(200);
      expect(discord.notifyPRMerge).toHaveBeenCalledWith(
        456,
        'OpenNHP/opennhp',
        'newuser',
        'Fix bug',
        'https://github.com/OpenNHP/opennhp/pull/456'
      );
    });

    it('handles issue events with good-first-issue label', async () => {
      const payload = {
        action: 'labeled',
        issue: {
          number: 10,
          title: 'Easy fix needed',
          html_url: 'https://github.com/OpenNHP/opennhp/issues/10',
          user: { login: 'maintainer' },
          labels: [{ name: 'good first issue' }],
        },
        repository: { full_name: 'OpenNHP/opennhp' },
      };
      const signature = signPayload(payload);

      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signature)
        .set('x-github-event', 'issues')
        .send(payload);

      expect(res.status).toBe(200);
      expect(discord.postGoodFirstIssue).toHaveBeenCalledWith(
        'OpenNHP/opennhp',
        10,
        'Easy fix needed',
        'https://github.com/OpenNHP/opennhp/issues/10',
        ['good first issue']
      );
    });

    it('handles release events', async () => {
      const payload = {
        action: 'published',
        release: {
          tag_name: 'v1.0.0',
          name: 'First Release',
          html_url: 'https://github.com/OpenNHP/opennhp/releases/tag/v1.0.0',
          body: 'Release notes here',
        },
        repository: { full_name: 'OpenNHP/opennhp' },
      };
      const signature = signPayload(payload);

      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signature)
        .set('x-github-event', 'release')
        .send(payload);

      expect(res.status).toBe(200);
      expect(discord.postReleaseAnnouncement).toHaveBeenCalledWith(
        'OpenNHP/opennhp',
        'v1.0.0',
        'First Release',
        'https://github.com/OpenNHP/opennhp/releases/tag/v1.0.0',
        'Release notes here'
      );
    });

    it('handles star milestone events', async () => {
      // When stars = 100 and no milestones announced, it announces all milestones up to 100
      const payload = {
        action: 'created',
        repository: {
          full_name: 'OpenNHP/opennhp',
          stargazers_count: 100,
          html_url: 'https://github.com/OpenNHP/opennhp',
        },
      };
      const signature = signPayload(payload);

      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signature)
        .set('x-github-event', 'star')
        .send(payload);

      expect(res.status).toBe(200);
      expect(db.hasMilestoneBeenAnnounced).toHaveBeenCalled();
      expect(db.recordMilestone).toHaveBeenCalled();
      // Announces the highest reached milestone
      expect(discord.postStarMilestone).toHaveBeenCalledWith(
        'OpenNHP/opennhp',
        100,
        'https://github.com/OpenNHP/opennhp'
      );
    });
  });
});
