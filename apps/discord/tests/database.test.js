const Database = require('better-sqlite3');
const path = require('path');
const fs = require('fs');

// Use in-memory database for tests
let db;
let testDb;

beforeEach(() => {
  // Create fresh in-memory database for each test
  testDb = new Database(':memory:');

  // Create tables
  testDb.exec(`
    CREATE TABLE IF NOT EXISTS github_links (
      discord_id TEXT PRIMARY KEY,
      github_username TEXT NOT NULL UNIQUE,
      linked_at TEXT DEFAULT CURRENT_TIMESTAMP,
      updated_at TEXT DEFAULT CURRENT_TIMESTAMP
    );

    CREATE TABLE IF NOT EXISTS pending_links (
      state TEXT PRIMARY KEY,
      discord_id TEXT NOT NULL,
      created_at TEXT DEFAULT CURRENT_TIMESTAMP
    );

    CREATE TABLE IF NOT EXISTS contributions (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      discord_id TEXT NOT NULL,
      github_username TEXT NOT NULL,
      pr_number INTEGER NOT NULL,
      repo TEXT NOT NULL,
      pr_title TEXT,
      merged_at TEXT DEFAULT CURRENT_TIMESTAMP
    );

    CREATE INDEX IF NOT EXISTS idx_contributions_discord ON contributions(discord_id);
    CREATE INDEX IF NOT EXISTS idx_contributions_repo ON contributions(repo);
  `);
});

afterEach(() => {
  testDb.close();
});

describe('Database', () => {
  describe('Pending Links', () => {
    it('creates and retrieves pending link', () => {
      const state = 'test-state-123';
      const discordId = '123456789';

      testDb.prepare('INSERT INTO pending_links (state, discord_id) VALUES (?, ?)').run(state, discordId);

      const pending = testDb.prepare('SELECT * FROM pending_links WHERE state = ?').get(state);
      expect(pending).toBeDefined();
      expect(pending.discord_id).toBe(discordId);
    });

    it('deletes pending link', () => {
      const state = 'test-state-456';
      testDb.prepare('INSERT INTO pending_links (state, discord_id) VALUES (?, ?)').run(state, '123');

      testDb.prepare('DELETE FROM pending_links WHERE state = ?').run(state);

      const pending = testDb.prepare('SELECT * FROM pending_links WHERE state = ?').get(state);
      expect(pending).toBeUndefined();
    });
  });

  describe('GitHub Links', () => {
    it('creates new link', () => {
      const discordId = '123456789';
      const githubUsername = 'testuser';

      testDb.prepare(`
        INSERT INTO github_links (discord_id, github_username)
        VALUES (?, ?)
        ON CONFLICT(discord_id) DO UPDATE SET
          github_username = excluded.github_username,
          updated_at = CURRENT_TIMESTAMP
      `).run(discordId, githubUsername);

      const link = testDb.prepare('SELECT * FROM github_links WHERE discord_id = ?').get(discordId);
      expect(link).toBeDefined();
      expect(link.github_username).toBe(githubUsername);
    });

    it('updates existing link', () => {
      const discordId = '123456789';

      // Create initial link
      testDb.prepare(`
        INSERT INTO github_links (discord_id, github_username) VALUES (?, ?)
      `).run(discordId, 'olduser');

      // Update link
      testDb.prepare(`
        INSERT INTO github_links (discord_id, github_username)
        VALUES (?, ?)
        ON CONFLICT(discord_id) DO UPDATE SET
          github_username = excluded.github_username,
          updated_at = CURRENT_TIMESTAMP
      `).run(discordId, 'newuser');

      const link = testDb.prepare('SELECT * FROM github_links WHERE discord_id = ?').get(discordId);
      expect(link.github_username).toBe('newuser');
    });

    it('finds link by GitHub username', () => {
      testDb.prepare('INSERT INTO github_links (discord_id, github_username) VALUES (?, ?)').run('123', 'testuser');

      const link = testDb.prepare('SELECT * FROM github_links WHERE LOWER(github_username) = LOWER(?)').get('TestUser');
      expect(link).toBeDefined();
      expect(link.discord_id).toBe('123');
    });

    it('deletes link', () => {
      testDb.prepare('INSERT INTO github_links (discord_id, github_username) VALUES (?, ?)').run('123', 'testuser');
      testDb.prepare('DELETE FROM github_links WHERE discord_id = ?').run('123');

      const link = testDb.prepare('SELECT * FROM github_links WHERE discord_id = ?').get('123');
      expect(link).toBeUndefined();
    });
  });

  describe('Contributions', () => {
    it('records contribution', () => {
      testDb.prepare(`
        INSERT INTO contributions (discord_id, github_username, pr_number, repo, pr_title)
        VALUES (?, ?, ?, ?, ?)
      `).run('123', 'testuser', 42, 'OpenNHP/opennhp', 'Add feature');

      const contributions = testDb.prepare('SELECT * FROM contributions WHERE discord_id = ?').all('123');
      expect(contributions).toHaveLength(1);
      expect(contributions[0].pr_number).toBe(42);
      expect(contributions[0].repo).toBe('OpenNHP/opennhp');
    });

    it('gets contributions ordered by date', () => {
      testDb.prepare(`
        INSERT INTO contributions (discord_id, github_username, pr_number, repo, pr_title)
        VALUES (?, ?, ?, ?, ?)
      `).run('123', 'testuser', 1, 'OpenNHP/opennhp', 'First');

      testDb.prepare(`
        INSERT INTO contributions (discord_id, github_username, pr_number, repo, pr_title)
        VALUES (?, ?, ?, ?, ?)
      `).run('123', 'testuser', 2, 'OpenNHP/opennhp', 'Second');

      const contributions = testDb.prepare(
        'SELECT * FROM contributions WHERE discord_id = ? ORDER BY merged_at DESC'
      ).all('123');

      expect(contributions).toHaveLength(2);
    });
  });

  describe('Stats', () => {
    it('calculates correct stats', () => {
      // Add some links
      testDb.prepare('INSERT INTO github_links (discord_id, github_username) VALUES (?, ?)').run('1', 'user1');
      testDb.prepare('INSERT INTO github_links (discord_id, github_username) VALUES (?, ?)').run('2', 'user2');

      // Add contributions
      testDb.prepare(`
        INSERT INTO contributions (discord_id, github_username, pr_number, repo, pr_title)
        VALUES (?, ?, ?, ?, ?)
      `).run('1', 'user1', 1, 'OpenNHP/opennhp', 'PR 1');
      testDb.prepare(`
        INSERT INTO contributions (discord_id, github_username, pr_number, repo, pr_title)
        VALUES (?, ?, ?, ?, ?)
      `).run('1', 'user1', 2, 'OpenNHP/opennhp', 'PR 2');
      testDb.prepare(`
        INSERT INTO contributions (discord_id, github_username, pr_number, repo, pr_title)
        VALUES (?, ?, ?, ?, ?)
      `).run('2', 'user2', 3, 'OpenNHP/StealthDNS', 'PR 3');

      const linkedUsers = testDb.prepare('SELECT COUNT(*) as count FROM github_links').get().count;
      const totalContributions = testDb.prepare('SELECT COUNT(*) as count FROM contributions').get().count;
      const uniqueContributors = testDb.prepare('SELECT COUNT(DISTINCT discord_id) as count FROM contributions').get().count;

      expect(linkedUsers).toBe(2);
      expect(totalContributions).toBe(3);
      expect(uniqueContributors).toBe(2);
    });
  });
});
