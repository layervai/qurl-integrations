/**
 * Additional webhook tests — covers automated bot detection, star milestones,
 * issue events (opened + first issue badge), activity feed events, and
 * webhook secret not configured.
 */
const crypto = require('crypto');

jest.mock('../src/discord', () => ({
  assignContributorRole: jest.fn().mockResolvedValue({ success: true }),
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
  getStats: jest.fn(() => ({ linkedUsers: 0, totalContributions: 0, uniqueContributors: 0, byRepo: [] })),
}));

// GUILD_ID gates whether /auth and /webhook routes are mounted (single-guild
// mode only). This test suite exercises webhook routes, so it must run in
// single-guild mode — set a valid Discord snowflake (17-20 digits).
process.env.GUILD_ID = '123456789012345678';
process.env.GITHUB_CLIENT_ID = 'test-client-id';
process.env.GITHUB_CLIENT_SECRET = 'test-client-secret';
process.env.GITHUB_WEBHOOK_SECRET = 'test-webhook-secret';
process.env.BASE_URL = 'http://localhost:3000';

const request = require('supertest');
const { app } = require('../src/server');
const db = require('../src/database');
const discord = require('../src/discord');

function signPayload(payload, secret = 'test-webhook-secret') {
  const hmac = crypto.createHmac('sha256', secret);
  return 'sha256=' + hmac.update(JSON.stringify(payload)).digest('hex');
}

beforeEach(() => { jest.clearAllMocks(); });

describe('Webhook routes — extra coverage', () => {
  describe('automated bot detection', () => {
    it('skips notification for dependabot PRs', async () => {
      db.getLinkByGithub.mockReturnValue(null);
      const payload = {
        action: 'closed',
        pull_request: {
          merged: true, number: 99, title: 'Bump deps',
          html_url: 'https://github.com/OpenNHP/opennhp/pull/99',
          user: { login: 'dependabot[bot]', type: 'Bot' },
        },
        repository: { full_name: 'OpenNHP/opennhp' },
      };
      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signPayload(payload))
        .set('x-github-event', 'pull_request')
        .send(payload);

      expect(res.status).toBe(200);
      expect(discord.notifyPRMerge).not.toHaveBeenCalled();
    });
  });

  describe('star events', () => {
    it('announces highest unannounced milestone', async () => {
      db.hasMilestoneBeenAnnounced.mockReturnValue(false);
      db.recordMilestone.mockReturnValue(true);

      const payload = {
        action: 'created',
        repository: { full_name: 'OpenNHP/opennhp', stargazers_count: 150, html_url: 'https://github.com/OpenNHP/opennhp' },
      };
      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signPayload(payload))
        .set('x-github-event', 'star')
        .send(payload);

      expect(res.status).toBe(200);
      expect(discord.postStarMilestone).toHaveBeenCalled();
    });

    it('ignores star deleted events', async () => {
      const payload = {
        action: 'deleted',
        repository: { full_name: 'OpenNHP/opennhp', stargazers_count: 99 },
      };
      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signPayload(payload))
        .set('x-github-event', 'star')
        .send(payload);

      expect(res.status).toBe(200);
      expect(discord.postStarMilestone).not.toHaveBeenCalled();
    });
  });

  describe('issue events', () => {
    it('awards first issue badge for linked user', async () => {
      db.getLinkByGithub.mockReturnValue({ discord_id: '123' });
      db.awardFirstIssueBadge.mockReturnValue(['first_issue']);

      const payload = {
        action: 'opened',
        issue: {
          number: 5, title: 'Bug report',
          html_url: 'https://github.com/OpenNHP/opennhp/issues/5',
          user: { login: 'ghuser' },
          labels: [],
        },
        repository: { full_name: 'OpenNHP/opennhp' },
      };
      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signPayload(payload))
        .set('x-github-event', 'issues')
        .send(payload);

      expect(res.status).toBe(200);
      expect(db.awardFirstIssueBadge).toHaveBeenCalledWith('123');
      expect(discord.notifyBadgeEarned).toHaveBeenCalled();
      // Also posts to github feed
      expect(discord.postToGitHubFeed).toHaveBeenCalled();
    });

    it('posts opened issue with labels to activity feed', async () => {
      db.getLinkByGithub.mockReturnValue(null);
      const payload = {
        action: 'opened',
        issue: {
          number: 6, title: 'Feature request',
          html_url: 'https://github.com/OpenNHP/opennhp/issues/6',
          user: { login: 'someone' },
          labels: [{ name: 'enhancement' }, { name: 'priority-high' }],
        },
        repository: { full_name: 'OpenNHP/opennhp' },
      };
      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signPayload(payload))
        .set('x-github-event', 'issues')
        .send(payload);

      expect(res.status).toBe(200);
      expect(discord.postToGitHubFeed).toHaveBeenCalled();
    });
  });

  describe('push events', () => {
    it('posts push event to activity feed', async () => {
      const payload = {
        ref: 'refs/heads/main',
        pusher: { name: 'dev' },
        commits: [
          { id: 'abc1234567890', message: 'Fix stuff\n\nLong body' },
          { id: 'def1234567890', message: 'Update docs' },
        ],
        repository: { full_name: 'OpenNHP/opennhp' },
      };
      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signPayload(payload))
        .set('x-github-event', 'push')
        .send(payload);

      expect(res.status).toBe(200);
      expect(discord.postToGitHubFeed).toHaveBeenCalled();
    });

    it('ignores push with no commits', async () => {
      const payload = {
        ref: 'refs/heads/main',
        commits: [],
        repository: { full_name: 'OpenNHP/opennhp' },
      };
      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signPayload(payload))
        .set('x-github-event', 'push')
        .send(payload);

      expect(res.status).toBe(200);
    });
  });

  describe('create events', () => {
    it('posts branch creation to feed', async () => {
      const payload = {
        ref_type: 'branch',
        ref: 'feat/new-feature',
        repository: { full_name: 'OpenNHP/opennhp' },
      };
      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signPayload(payload))
        .set('x-github-event', 'create')
        .send(payload);

      expect(res.status).toBe(200);
      expect(discord.postToGitHubFeed).toHaveBeenCalled();
    });

    it('posts tag creation to feed', async () => {
      const payload = {
        ref_type: 'tag',
        ref: 'v1.0.0',
        repository: { full_name: 'OpenNHP/opennhp' },
      };
      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signPayload(payload))
        .set('x-github-event', 'create')
        .send(payload);

      expect(res.status).toBe(200);
      expect(discord.postToGitHubFeed).toHaveBeenCalled();
    });
  });

  describe('delete events', () => {
    it('posts branch deletion to feed', async () => {
      const payload = {
        ref_type: 'branch',
        ref: 'old-branch',
        repository: { full_name: 'OpenNHP/opennhp' },
      };
      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signPayload(payload))
        .set('x-github-event', 'delete')
        .send(payload);

      expect(res.status).toBe(200);
      expect(discord.postToGitHubFeed).toHaveBeenCalled();
    });
  });

  describe('release events', () => {
    it('posts to both announcements and feed', async () => {
      const payload = {
        action: 'published',
        release: {
          tag_name: 'v2.0', name: 'Big Release',
          html_url: 'https://github.com/release/v2.0', body: 'Notes',
        },
        repository: { full_name: 'OpenNHP/opennhp' },
      };
      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signPayload(payload))
        .set('x-github-event', 'release')
        .send(payload);

      expect(res.status).toBe(200);
      expect(discord.postReleaseAnnouncement).toHaveBeenCalled();
      expect(discord.postToGitHubFeed).toHaveBeenCalled();
    });

    it('ignores non-published release events', async () => {
      const payload = {
        action: 'created',
        release: { tag_name: 'v1.0' },
        repository: { full_name: 'OpenNHP/opennhp' },
      };
      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signPayload(payload))
        .set('x-github-event', 'release')
        .send(payload);

      expect(res.status).toBe(200);
      expect(discord.postReleaseAnnouncement).not.toHaveBeenCalled();
    });
  });

  describe('merged PR with badges', () => {
    it('notifies badge earned when badges are awarded', async () => {
      db.getLinkByGithub.mockReturnValue({ discord_id: '999', github_username: 'badgedev' });
      db.checkAndAwardBadges.mockReturnValue(['first_pr', 'bug_hunter']);

      const payload = {
        action: 'closed',
        pull_request: {
          merged: true, number: 50, title: 'Fix critical bug',
          html_url: 'https://github.com/OpenNHP/opennhp/pull/50',
          user: { login: 'badgedev' },
        },
        repository: { full_name: 'OpenNHP/opennhp' },
      };
      const res = await request(app)
        .post('/webhook/github')
        .set('x-hub-signature-256', signPayload(payload))
        .set('x-github-event', 'pull_request')
        .send(payload);

      expect(res.status).toBe(200);
      expect(discord.notifyBadgeEarned).toHaveBeenCalledWith('999', ['first_pr', 'bug_hunter']);
    });
  });
});
