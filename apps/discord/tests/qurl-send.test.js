/**
 * Tests for /qurl send feature — helper functions, QURL client, connector client,
 * places client, and database methods.
 */

const Database = require('better-sqlite3');

// ---------------------------------------------------------------------------
// Mocks — set up BEFORE requiring modules under test
// ---------------------------------------------------------------------------

// Minimal config stub (avoids pulling in real env vars)
jest.mock('../src/config', () => ({
  QURL_API_KEY: 'test-api-key',
  QURL_ENDPOINT: 'https://api.test.local',
  CONNECTOR_URL: 'https://connector.test.local',
  GOOGLE_MAPS_API_KEY: 'test-google-key',
  QURL_SEND_COOLDOWN_MS: 30000,
  QURL_SEND_MAX_RECIPIENTS: 50,
  DATABASE_PATH: ':memory:',
  PENDING_LINK_EXPIRY_MINUTES: 30,
  ADMIN_USER_IDS: [],
}));

// Silence logger output during tests
jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
}));

// Mock dns.lookup so createOneTimeLink's DNS-rebinding guard doesn't hit
// the network. Any public-looking hostname resolves to a public IP.
jest.mock('dns', () => ({
  promises: {
    lookup: jest.fn().mockResolvedValue([{ address: '93.184.216.34', family: 4 }]),
  },
}));

// Mock discord.js (commands.js imports it at top-level)
jest.mock('discord.js', () => ({
  SlashCommandBuilder: jest.fn().mockImplementation(() => {
    const builder = {
      setName: jest.fn().mockReturnThis(),
      setDescription: jest.fn().mockReturnThis(),
      addSubcommand: jest.fn().mockReturnThis(),
      addStringOption: jest.fn().mockReturnThis(),
      addUserOption: jest.fn().mockReturnThis(),
      addAttachmentOption: jest.fn().mockReturnThis(),
      addIntegerOption: jest.fn().mockReturnThis(),
      setDefaultMemberPermissions: jest.fn().mockReturnThis(),
      toJSON: jest.fn().mockReturnValue({}),
    };
    return builder;
  }),
  EmbedBuilder: jest.fn().mockImplementation(() => ({
    setColor: jest.fn().mockReturnThis(),
    setTitle: jest.fn().mockReturnThis(),
    setDescription: jest.fn().mockReturnThis(),
    addFields: jest.fn().mockReturnThis(),
    setFooter: jest.fn().mockReturnThis(),
    setTimestamp: jest.fn().mockReturnThis(),
    setThumbnail: jest.fn().mockReturnThis(),
  })),
  PermissionFlagsBits: { ManageRoles: 1n },
  ActionRowBuilder: jest.fn().mockImplementation(() => {
    const row = { components: [], addComponents: jest.fn(function (...args) {
      row.components.push(...args.flat());
      return row;
    }) };
    return row;
  }),
  ButtonBuilder: jest.fn().mockImplementation(() => ({
    setCustomId: jest.fn().mockReturnThis(),
    setLabel: jest.fn().mockReturnThis(),
    setStyle: jest.fn().mockReturnThis(),
    setURL: jest.fn().mockReturnThis(),
  })),
  ButtonStyle: { Primary: 1, Secondary: 2, Success: 3, Danger: 4, Link: 5 },
  ComponentType: { Button: 2, StringSelect: 3 },
  StringSelectMenuBuilder: jest.fn().mockImplementation(() => ({
    setCustomId: jest.fn().mockReturnThis(),
    setPlaceholder: jest.fn().mockReturnThis(),
    addOptions: jest.fn().mockReturnThis(),
  })),
  UserSelectMenuBuilder: jest.fn().mockImplementation(() => ({
    setCustomId: jest.fn().mockReturnThis(),
    setPlaceholder: jest.fn().mockReturnThis(),
    setMinValues: jest.fn().mockReturnThis(),
    setMaxValues: jest.fn().mockReturnThis(),
  })),
}));

// We do NOT want database.js to open a real file or start intervals
jest.mock('../src/database', () => {
  // Return a thin stub — the real DB tests below use a fresh in-memory SQLite
  return {
    recordQURLSend: jest.fn(),
    recordQURLSendBatch: jest.fn(),
    updateSendDMStatus: jest.fn(),
    getRecentSends: jest.fn(() => []),
    getSendResourceIds: jest.fn(() => []),
    saveSendConfig: jest.fn(),
    getSendConfig: jest.fn(),
  };
});

// Mock discord helper module
jest.mock('../src/discord', () => ({
  assignContributorRole: jest.fn(),
  notifyPRMerge: jest.fn(),
  notifyBadgeEarned: jest.fn(),
  postGoodFirstIssue: jest.fn(),
  postReleaseAnnouncement: jest.fn(),
  postStarMilestone: jest.fn(),
  postToGitHubFeed: jest.fn(),
  sendDM: jest.fn(),
  getVoiceChannelMembers: jest.fn(),
  getTextChannelMembers: jest.fn(),
}));

// Mock admin util
jest.mock('../src/utils/admin', () => ({
  requireAdmin: jest.fn(() => true),
  isAdmin: jest.fn(() => false),
}));

// Mock form-data (for connector.js)
jest.mock('form-data', () => {
  return jest.fn().mockImplementation(() => ({
    append: jest.fn(),
    getHeaders: jest.fn(() => ({ 'content-type': 'multipart/form-data; boundary=---' })),
  }));
});

// Mock Readable.fromWeb to avoid needing a real ReadableStream in tests
const { Readable } = require('stream');
const originalFromWeb = Readable.fromWeb;
Readable.fromWeb = jest.fn(() => new Readable({ read() { this.push(null); } }));

// Global fetch mock (node 18+ built-in)
const originalFetch = globalThis.fetch;

// ---------------------------------------------------------------------------
// Now require modules under test
// ---------------------------------------------------------------------------

const { _test } = require('../src/commands');
const {
  isGoogleMapsURL,
  sanitizeFilename,
  sanitizeMessage,
  isAllowedFileType,
  isOnCooldown,
  setCooldown,
  batchSettled,
  expiryToISO,
  sendCooldowns,
} = _test;

// =========================================================================
// 1. Helper Functions (pure logic)
// =========================================================================

describe('Helper functions', () => {
  // -----------------------------------------------------------------------
  // isGoogleMapsURL
  // -----------------------------------------------------------------------
  describe('isGoogleMapsURL', () => {
    it('matches google.com/maps paths', () => {
      expect(isGoogleMapsURL('https://www.google.com/maps/place/Eiffel+Tower')).toBe(true);
      expect(isGoogleMapsURL('http://google.com/maps?q=paris')).toBe(true);
      expect(isGoogleMapsURL('https://google.co.uk/maps/')).toBe(true);
    });

    it('matches maps.google.* domains', () => {
      expect(isGoogleMapsURL('https://maps.google.com/')).toBe(true);
      expect(isGoogleMapsURL('https://maps.google.co.uk/')).toBe(true);
      expect(isGoogleMapsURL('http://maps.google.de/?q=berlin')).toBe(true);
    });

    it('matches goo.gl/maps short links', () => {
      expect(isGoogleMapsURL('https://goo.gl/maps/abc123')).toBe(true);
      expect(isGoogleMapsURL('http://goo.gl/maps/xyz')).toBe(true);
    });

    it('matches maps.app.goo.gl short links', () => {
      expect(isGoogleMapsURL('https://maps.app.goo.gl/abc123')).toBe(true);
    });

    it('rejects non-maps URLs', () => {
      expect(isGoogleMapsURL('https://www.google.com/search?q=test')).toBe(false);
      expect(isGoogleMapsURL('https://example.com/maps')).toBe(false);
      expect(isGoogleMapsURL('https://github.com')).toBe(false);
    });

    it('rejects spoofed domains', () => {
      expect(isGoogleMapsURL('https://maps.google.evil.com/exploit')).toBe(false);
      expect(isGoogleMapsURL('https://fakegoogle.com/maps')).toBe(false);
      expect(isGoogleMapsURL('https://goo.gl.evil.com/maps/abc')).toBe(false);
    });

    it('handles edge cases', () => {
      expect(isGoogleMapsURL('')).toBe(false);
      expect(isGoogleMapsURL('not a url')).toBe(false);
      expect(isGoogleMapsURL('ftp://google.com/maps')).toBe(false);
    });
  });

  // -----------------------------------------------------------------------
  // sanitizeFilename
  // -----------------------------------------------------------------------
  describe('sanitizeFilename', () => {
    it('replaces path traversal sequences', () => {
      const result = sanitizeFilename('../../etc/passwd');
      expect(result).not.toContain('..');
      expect(result).not.toContain('/');
    });

    it('replaces special characters', () => {
      const result = sanitizeFilename('file<name>:with*bad?"chars|.txt');
      expect(result).not.toMatch(/[<>:*?"\\|]/);
    });

    it('removes control characters', () => {
      const result = sanitizeFilename('file\x00name\x1f.txt');
      expect(result).toBe('filename.txt');
    });

    it('truncates names longer than 200 characters', () => {
      const longName = 'a'.repeat(300) + '.txt';
      const result = sanitizeFilename(longName);
      expect(result.length).toBeLessThanOrEqual(200);
    });

    it('passes through normal filenames', () => {
      expect(sanitizeFilename('photo.png')).toBe('photo.png');
      expect(sanitizeFilename('my-file_v2.pdf')).toBe('my-file_v2.pdf');
    });

    it('replaces backslash (Windows-style paths)', () => {
      const result = sanitizeFilename('path\\to\\file.txt');
      expect(result).not.toContain('\\');
    });
  });

  // -----------------------------------------------------------------------
  // sanitizeMessage
  // -----------------------------------------------------------------------
  describe('sanitizeMessage', () => {
    it('defuses @everyone', () => {
      const result = sanitizeMessage('hey @everyone look');
      expect(result).not.toContain('@everyone');
      expect(result).toContain('@\u200beveryone');
    });

    it('defuses @here', () => {
      const result = sanitizeMessage('yo @here check this');
      expect(result).not.toContain('@here');
      expect(result).toContain('@\u200bhere');
    });

    it('replaces user mentions (<@123>)', () => {
      const result = sanitizeMessage('thanks <@123456>');
      expect(result).toContain('[mention]');
      expect(result).not.toContain('<@123456>');
    });

    it('replaces nickname mentions (<@!123>)', () => {
      const result = sanitizeMessage('hi <@!789>');
      expect(result).toContain('[mention]');
    });

    it('replaces role mentions (<@&123>)', () => {
      const result = sanitizeMessage('ping <@&456>');
      expect(result).toContain('[mention]');
      expect(result).not.toContain('<@&456>');
    });

    it('passes clean text through unchanged', () => {
      const clean = 'Hello, this is a normal message.';
      expect(sanitizeMessage(clean)).toBe(clean);
    });

    it('handles case-insensitive @Everyone / @HERE', () => {
      expect(sanitizeMessage('@Everyone')).toContain('@\u200b');
      expect(sanitizeMessage('@HERE')).toContain('@\u200b');
    });
  });

  // -----------------------------------------------------------------------
  // isAllowedFileType
  // -----------------------------------------------------------------------
  describe('isAllowedFileType', () => {
    it.each([
      ['image/png', true],
      ['image/jpeg', true],
      ['image/gif', true],
      ['application/pdf', true],
      ['video/mp4', true],
      ['audio/mpeg', true],
      ['text/plain', true],
      ['text/csv', true],
      ['application/zip', true],
      ['application/x-zip-compressed', true],
      ['application/vnd.openxmlformats-officedocument.spreadsheetml.sheet', true],
      ['application/vnd.ms-excel', true],
      ['application/msword', true],
    ])('allows %s -> %s', (type, expected) => {
      expect(isAllowedFileType(type)).toBe(expected);
    });

    it.each([
      ['application/x-executable', false],
      ['text/html', false],
      ['application/javascript', false],
      ['application/x-sh', false],
    ])('blocks %s -> %s', (type, expected) => {
      expect(isAllowedFileType(type)).toBe(expected);
    });

    it('returns false for null', () => {
      expect(isAllowedFileType(null)).toBe(false);
    });

    it('returns false for undefined', () => {
      expect(isAllowedFileType(undefined)).toBe(false);
    });

    it('returns false for empty string', () => {
      expect(isAllowedFileType('')).toBe(false);
    });
  });

  // -----------------------------------------------------------------------
  // batchSettled
  // -----------------------------------------------------------------------
  describe('batchSettled', () => {
    it('processes items in batches of 5', async () => {
      const items = [1, 2, 3, 4, 5, 6, 7];
      const fn = jest.fn(async (x) => x * 2);

      const results = await batchSettled(items, fn, 5);

      expect(results).toHaveLength(7);
      expect(fn).toHaveBeenCalledTimes(7);
      results.forEach((r, i) => {
        expect(r.status).toBe('fulfilled');
        expect(r.value).toBe(items[i] * 2);
      });
    });

    it('captures partial failures without aborting', async () => {
      const items = ['a', 'b', 'c'];
      const fn = jest.fn(async (x) => {
        if (x === 'b') throw new Error('boom');
        return x.toUpperCase();
      });

      const results = await batchSettled(items, fn, 5);

      expect(results).toHaveLength(3);
      expect(results[0].status).toBe('fulfilled');
      expect(results[0].value).toBe('A');
      expect(results[1].status).toBe('rejected');
      expect(results[1].reason.message).toBe('boom');
      expect(results[2].status).toBe('fulfilled');
      expect(results[2].value).toBe('C');
    });

    it('handles an empty array', async () => {
      const results = await batchSettled([], jest.fn(), 5);
      expect(results).toEqual([]);
    });

    it('handles a single item', async () => {
      const results = await batchSettled([42], async (x) => x, 5);
      expect(results).toHaveLength(1);
      expect(results[0].value).toBe(42);
    });

    it('respects custom batch size', async () => {
      let concurrentCalls = 0;
      let maxConcurrent = 0;
      const items = [1, 2, 3, 4, 5, 6];

      const fn = jest.fn(async (x) => {
        concurrentCalls++;
        maxConcurrent = Math.max(maxConcurrent, concurrentCalls);
        // Simulate async work
        await new Promise(r => setTimeout(r, 10));
        concurrentCalls--;
        return x;
      });

      await batchSettled(items, fn, 2);
      // Within each batch of 2, both fire concurrently
      expect(maxConcurrent).toBeLessThanOrEqual(2);
    });
  });

  // -----------------------------------------------------------------------
  // expiryToISO
  // -----------------------------------------------------------------------
  describe('expiryToISO', () => {
    beforeEach(() => {
      jest.useFakeTimers();
      jest.setSystemTime(new Date('2026-01-15T00:00:00.000Z'));
    });

    afterEach(() => {
      jest.useRealTimers();
    });

    it('parses 30m correctly', () => {
      const result = expiryToISO('30m');
      const expected = new Date('2026-01-15T00:30:00.000Z').toISOString();
      expect(result).toBe(expected);
    });

    it('parses 1h correctly', () => {
      const result = expiryToISO('1h');
      const expected = new Date('2026-01-15T01:00:00.000Z').toISOString();
      expect(result).toBe(expected);
    });

    it('parses 6h correctly', () => {
      const result = expiryToISO('6h');
      const expected = new Date('2026-01-15T06:00:00.000Z').toISOString();
      expect(result).toBe(expected);
    });

    it('parses 24h correctly', () => {
      const result = expiryToISO('24h');
      const expected = new Date('2026-01-16T00:00:00.000Z').toISOString();
      expect(result).toBe(expected);
    });

    it('parses 7d correctly', () => {
      const result = expiryToISO('7d');
      const expected = new Date('2026-01-22T00:00:00.000Z').toISOString();
      expect(result).toBe(expected);
    });

    it('defaults to 24h for invalid input', () => {
      const result = expiryToISO('invalid');
      const expected = new Date('2026-01-16T00:00:00.000Z').toISOString();
      expect(result).toBe(expected);
    });

    it('defaults to 24h for empty string', () => {
      const result = expiryToISO('');
      const expected = new Date('2026-01-16T00:00:00.000Z').toISOString();
      expect(result).toBe(expected);
    });

    it('defaults to 24h for input with wrong units', () => {
      const result = expiryToISO('5w');
      const expected = new Date('2026-01-16T00:00:00.000Z').toISOString();
      expect(result).toBe(expected);
    });
  });

  // -----------------------------------------------------------------------
  // isOnCooldown / setCooldown
  // -----------------------------------------------------------------------
  describe('isOnCooldown / setCooldown', () => {
    beforeEach(() => {
      sendCooldowns.clear();
    });

    it('returns false when user has no cooldown', () => {
      expect(isOnCooldown('user1')).toBe(false);
    });

    it('returns true immediately after setCooldown', () => {
      setCooldown('user2');
      expect(isOnCooldown('user2')).toBe(true);
    });

    it('returns false after cooldown period expires', () => {
      jest.useFakeTimers();
      setCooldown('user3');
      expect(isOnCooldown('user3')).toBe(true);
      jest.advanceTimersByTime(31000); // 31s > 30s cooldown
      expect(isOnCooldown('user3')).toBe(false);
      jest.useRealTimers();
    });
  });
});

// =========================================================================
// 2. QURL Client (src/qurl.js)
// =========================================================================

describe('QURL client', () => {
  let qurl;

  beforeEach(() => {
    // Reset module registry so fetch mock is fresh each time
    jest.resetModules();
    // Re-apply essential mocks after resetModules
    jest.mock('../src/config', () => ({
      QURL_API_KEY: 'test-api-key',
      QURL_ENDPOINT: 'https://api.test.local',
    }));
    jest.mock('../src/logger', () => ({
      info: jest.fn(),
      warn: jest.fn(),
      error: jest.fn(),
      debug: jest.fn(),
    }));

    qurl = require('../src/qurl');
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  describe('createOneTimeLink', () => {
    it('sends correct POST body with one_time_use: true', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({
          data: { resource_id: 'res-1', qurl_link: 'https://q.test/abc' },
        }),
      });

      const result = await qurl.createOneTimeLink('https://example.com', '24h', 'test desc');

      expect(globalThis.fetch).toHaveBeenCalledTimes(1);
      const [url, opts] = globalThis.fetch.mock.calls[0];
      expect(url).toBe('https://api.test.local/v1/qurls');
      expect(opts.method).toBe('POST');
      const body = JSON.parse(opts.body);
      expect(body.one_time_use).toBe(true);
      expect(body.target_url).toBe('https://example.com');
      expect(body.expires_in).toBe('24h');
      expect(body.description).toBe('test desc');

      expect(result.resource_id).toBe('res-1');
      expect(result.qurl_link).toBe('https://q.test/abc');
    });

    it('throws on API error', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: false,
        status: 500,
        text: async () => 'Internal Server Error',
      });

      await expect(qurl.createOneTimeLink('https://example.com', '1h', 'desc'))
        .rejects.toThrow(/QURL API POST.*failed.*500/);
    });

    it('includes authorization header', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({ data: { resource_id: 'r1', qurl_link: 'l1' } }),
      });

      await qurl.createOneTimeLink('https://example.com', '1h', 'd');

      const headers = globalThis.fetch.mock.calls[0][1].headers;
      expect(headers.Authorization).toBe('Bearer test-api-key');
    });
  });

  describe('deleteLink', () => {
    it('sends DELETE request with correct path', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        status: 204,
      });

      await qurl.deleteLink('resource-42');

      expect(globalThis.fetch).toHaveBeenCalledTimes(1);
      const [url, opts] = globalThis.fetch.mock.calls[0];
      expect(url).toBe('https://api.test.local/v1/qurls/resource-42');
      expect(opts.method).toBe('DELETE');
    });

    it('throws on API error', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: false,
        status: 404,
        text: async () => 'Not Found',
      });

      await expect(qurl.deleteLink('bad-id')).rejects.toThrow(/QURL API DELETE.*failed.*404/);
    });
  });
});

// =========================================================================
// 3. Connector Client (src/connector.js)
// =========================================================================

describe('Connector client', () => {
  let connector;

  beforeEach(() => {
    jest.resetModules();
    jest.mock('../src/config', () => ({
      CONNECTOR_URL: 'https://connector.test.local',
      QURL_API_KEY: 'test-api-key',
    }));
    jest.mock('../src/logger', () => ({
      info: jest.fn(),
      warn: jest.fn(),
      error: jest.fn(),
      debug: jest.fn(),
    }));
    jest.mock('form-data', () => {
      return jest.fn().mockImplementation(() => ({
        append: jest.fn(),
        getHeaders: jest.fn(() => ({ 'content-type': 'multipart/form-data' })),
      }));
    });

    connector = require('../src/connector');
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  describe('uploadToConnector', () => {
    it('downloads from source URL then uploads to connector', async () => {
      // First call: Discord CDN download. Second call: connector upload.
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true,
          headers: { get: jest.fn(() => '0') },
          arrayBuffer: async () => new ArrayBuffer(0),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({
            success: true,
            hash: 'abc123',
            resource_id: 'conn-res-1',
          }),
        });

      const result = await connector.uploadToConnector(
        'https://cdn.discordapp.com/file.png',
        'photo.png',
        'image/png',
      );

      expect(globalThis.fetch).toHaveBeenCalledTimes(2);

      // First call: download from Discord CDN
      expect(globalThis.fetch.mock.calls[0][0]).toBe('https://cdn.discordapp.com/file.png');

      // Second call: upload to connector
      const [uploadUrl, uploadOpts] = globalThis.fetch.mock.calls[1];
      expect(uploadUrl).toBe('https://connector.test.local/api/upload');
      expect(uploadOpts.method).toBe('POST');

      expect(result.success).toBe(true);
      expect(result.resource_id).toBe('conn-res-1');
    });

    it('throws when Discord CDN download fails', async () => {
      globalThis.fetch = jest.fn().mockResolvedValueOnce({
        ok: false,
        status: 403,
      });

      await expect(connector.uploadToConnector('https://cdn.discordapp.com/file.png', 'f.png', 'image/png'))
        .rejects.toThrow(/Failed to download from Discord CDN: 403/);
    });

    it('throws when connector upload fails', async () => {
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true,
          headers: { get: jest.fn(() => '0') },
          arrayBuffer: async () => new ArrayBuffer(0),
        })
        .mockResolvedValueOnce({
          ok: false,
          status: 500,
          text: async () => 'Server Error',
        });

      await expect(connector.uploadToConnector('https://cdn.discordapp.com/f.png', 'f.png', 'image/png'))
        .rejects.toThrow(/Connector upload failed.*500/);
    });

    it('throws when connector returns success: false', async () => {
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true,
          headers: { get: jest.fn(() => '0') },
          arrayBuffer: async () => new ArrayBuffer(0),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ success: false }),
        });

      await expect(connector.uploadToConnector('https://cdn.discordapp.com/f.png', 'f.png', 'image/png'))
        .rejects.toThrow(/success: false/);
    });
  });

  describe('mintLinks', () => {
    it('sends POST with correct body and returns links', async () => {
      const mockLinks = [
        { qurl_link: 'https://q.test/1' },
        { qurl_link: 'https://q.test/2' },
      ];

      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        json: async () => ({ success: true, links: mockLinks }),
      });

      const result = await connector.mintLinks('conn-res-1', '2026-01-16T00:00:00.000Z', 2);

      expect(globalThis.fetch).toHaveBeenCalledTimes(1);
      const [url, opts] = globalThis.fetch.mock.calls[0];
      expect(url).toBe('https://connector.test.local/api/mint_link/conn-res-1');
      expect(opts.method).toBe('POST');
      const body = JSON.parse(opts.body);
      expect(body.expires_at).toBe('2026-01-16T00:00:00.000Z');
      expect(body.n).toBe(2);
      // Regression guard: bot MUST send one_time_use: true so each
      // minted link is single-use. Dropping this field produces
      // reusable links on some API-key tiers.
      expect(body.one_time_use).toBe(true);
      expect(result).toEqual(mockLinks);
    });

    it('throws on API error', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: false,
        status: 400,
        text: async () => 'Bad Request',
      });

      await expect(connector.mintLinks('bad', 'date', 1))
        .rejects.toThrow(/Connector mint_link failed.*400/);
    });

    it('throws when success is false', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        json: async () => ({ success: false }),
      });

      await expect(connector.mintLinks('id', 'date', 1))
        .rejects.toThrow(/success: false/);
    });
  });
});

// =========================================================================
// 4. Places Client (src/places.js)
// =========================================================================

describe('Places client', () => {
  let places;
  const configModule = require('../src/config');

  beforeEach(() => {
    jest.resetModules();
    jest.mock('../src/logger', () => ({
      info: jest.fn(),
      warn: jest.fn(),
      error: jest.fn(),
      debug: jest.fn(),
    }));
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  describe('searchPlaces', () => {
    it('calls Google Places API with correct parameters', async () => {
      jest.mock('../src/config', () => ({
        GOOGLE_MAPS_API_KEY: 'test-google-key',
      }));
      places = require('../src/places');

      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        json: async () => ({
          status: 'OK',
          predictions: [
            {
              place_id: 'ChIJ1',
              structured_formatting: {
                main_text: 'Eiffel Tower',
                secondary_text: 'Paris, France',
              },
              description: 'Eiffel Tower, Paris, France',
            },
          ],
        }),
      });

      const results = await places.searchPlaces('Eiffel Tower');

      expect(globalThis.fetch).toHaveBeenCalledTimes(1);
      const calledUrl = globalThis.fetch.mock.calls[0][0];
      expect(calledUrl).toContain('maps.googleapis.com/maps/api/place/autocomplete');
      expect(calledUrl).toContain('input=Eiffel');
      expect(calledUrl).toContain('key=test-google-key');
      expect(calledUrl).toContain('types=');

      expect(results).toHaveLength(1);
      expect(results[0].placeId).toBe('ChIJ1');
      expect(results[0].name).toBe('Eiffel Tower');
      expect(results[0].address).toBe('Paris, France');
    });

    it('returns empty array when API key is not set', async () => {
      jest.mock('../src/config', () => ({
        GOOGLE_MAPS_API_KEY: undefined,
      }));
      places = require('../src/places');

      const results = await places.searchPlaces('test');
      expect(results).toEqual([]);
    });

    it('throws on HTTP error', async () => {
      jest.mock('../src/config', () => ({
        GOOGLE_MAPS_API_KEY: 'key',
      }));
      places = require('../src/places');

      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: false,
        status: 500,
      });

      await expect(places.searchPlaces('test')).rejects.toThrow(/Places API error: 500/);
    });

    it('throws on non-OK API status', async () => {
      jest.mock('../src/config', () => ({
        GOOGLE_MAPS_API_KEY: 'key',
      }));
      places = require('../src/places');

      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        json: async () => ({ status: 'REQUEST_DENIED' }),
      });

      await expect(places.searchPlaces('test')).rejects.toThrow(/Places API status: REQUEST_DENIED/);
    });

    it('returns empty array for ZERO_RESULTS', async () => {
      jest.mock('../src/config', () => ({
        GOOGLE_MAPS_API_KEY: 'key',
      }));
      places = require('../src/places');

      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        json: async () => ({ status: 'ZERO_RESULTS', predictions: [] }),
      });

      const results = await places.searchPlaces('xyznonexistent');
      expect(results).toEqual([]);
    });

    it('handles missing structured_formatting', async () => {
      jest.mock('../src/config', () => ({
        GOOGLE_MAPS_API_KEY: 'key',
      }));
      places = require('../src/places');

      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        json: async () => ({
          status: 'OK',
          predictions: [
            {
              place_id: 'p1',
              description: 'Some Place, Country',
              // no structured_formatting
            },
          ],
        }),
      });

      const results = await places.searchPlaces('some');
      expect(results[0].name).toBe('Some Place, Country');
      expect(results[0].address).toBe('');
    });
  });
});

// =========================================================================
// 5. Database qurl_sends methods (in-memory SQLite)
// =========================================================================

describe('Database qurl_sends methods', () => {
  let testDb;

  // Helper methods that mirror the real database module's qurl_sends methods
  let recordQURLSend;
  let updateSendDMStatus;
  let getRecentSends;
  let getSendResourceIds;
  let cleanupOldSends;

  beforeEach(() => {
    testDb = new Database(':memory:');
    testDb.exec(`
      CREATE TABLE IF NOT EXISTS qurl_sends (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        send_id TEXT NOT NULL,
        sender_discord_id TEXT NOT NULL,
        recipient_discord_id TEXT NOT NULL,
        resource_id TEXT NOT NULL,
        resource_type TEXT NOT NULL,
        qurl_link TEXT NOT NULL,
        expires_in TEXT,
        channel_id TEXT,
        target_type TEXT NOT NULL,
        dm_status TEXT DEFAULT 'pending',
        created_at TEXT DEFAULT CURRENT_TIMESTAMP
      );
      CREATE INDEX IF NOT EXISTS idx_qurl_sends_sender ON qurl_sends(sender_discord_id);
      CREATE INDEX IF NOT EXISTS idx_qurl_sends_send_id ON qurl_sends(send_id);
    `);

    // Mirror database module methods using the in-memory db
    recordQURLSend = (sendId, senderDiscordId, recipientDiscordId, resourceId, resourceType, qurlLink, expiresIn, channelId, targetType) => {
      const stmt = testDb.prepare(`
        INSERT INTO qurl_sends (send_id, sender_discord_id, recipient_discord_id, resource_id, resource_type, qurl_link, expires_in, channel_id, target_type)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
      `);
      stmt.run(sendId, senderDiscordId, recipientDiscordId, resourceId, resourceType, qurlLink, expiresIn, channelId, targetType);
    };

    updateSendDMStatus = (sendId, recipientDiscordId, status) => {
      const stmt = testDb.prepare('UPDATE qurl_sends SET dm_status = ? WHERE send_id = ? AND recipient_discord_id = ?');
      stmt.run(status, sendId, recipientDiscordId);
    };

    getRecentSends = (senderDiscordId, limit = 10) => {
      const stmt = testDb.prepare(`
        SELECT send_id, resource_type, target_type, channel_id, expires_in, created_at,
               COUNT(*) as recipient_count,
               SUM(CASE WHEN dm_status = 'sent' THEN 1 ELSE 0 END) as delivered_count
        FROM qurl_sends
        WHERE sender_discord_id = ?
        GROUP BY send_id
        ORDER BY created_at DESC
        LIMIT ?
      `);
      return stmt.all(senderDiscordId, limit);
    };

    getSendResourceIds = (sendId) => {
      const stmt = testDb.prepare('SELECT resource_id FROM qurl_sends WHERE send_id = ?');
      return stmt.all(sendId).map(r => r.resource_id);
    };

    cleanupOldSends = () => {
      const result = testDb.prepare(`
        DELETE FROM qurl_sends
        WHERE datetime(created_at) < datetime('now', '-30 days')
      `).run();
      return result.changes;
    };
  });

  afterEach(() => {
    testDb.close();
  });

  describe('recordQURLSend + getSendResourceIds roundtrip', () => {
    it('inserts a row and retrieves the resource_id by send_id', () => {
      recordQURLSend('send-1', 'sender-1', 'rcpt-1', 'res-1', 'url', 'https://q.test/1', '24h', 'ch-1', 'user');

      const ids = getSendResourceIds('send-1');
      expect(ids).toEqual(['res-1']);
    });

    it('returns multiple resource IDs for multi-recipient send', () => {
      recordQURLSend('send-2', 'sender-1', 'rcpt-1', 'res-a', 'url', 'https://q.test/a', '1h', 'ch-1', 'channel');
      recordQURLSend('send-2', 'sender-1', 'rcpt-2', 'res-b', 'url', 'https://q.test/b', '1h', 'ch-1', 'channel');
      recordQURLSend('send-2', 'sender-1', 'rcpt-3', 'res-c', 'url', 'https://q.test/c', '1h', 'ch-1', 'channel');

      const ids = getSendResourceIds('send-2');
      expect(ids).toHaveLength(3);
      expect(ids).toContain('res-a');
      expect(ids).toContain('res-b');
      expect(ids).toContain('res-c');
    });

    it('returns empty array for non-existent send_id', () => {
      expect(getSendResourceIds('does-not-exist')).toEqual([]);
    });
  });

  describe('updateSendDMStatus', () => {
    it('updates dm_status from pending to sent', () => {
      recordQURLSend('send-3', 'sender-1', 'rcpt-1', 'res-1', 'url', 'https://q.test/1', '24h', 'ch-1', 'user');

      // Verify initial status
      const before = testDb.prepare('SELECT dm_status FROM qurl_sends WHERE send_id = ? AND recipient_discord_id = ?').get('send-3', 'rcpt-1');
      expect(before.dm_status).toBe('pending');

      updateSendDMStatus('send-3', 'rcpt-1', 'sent');

      const after = testDb.prepare('SELECT dm_status FROM qurl_sends WHERE send_id = ? AND recipient_discord_id = ?').get('send-3', 'rcpt-1');
      expect(after.dm_status).toBe('sent');
    });

    it('updates dm_status to failed', () => {
      recordQURLSend('send-4', 'sender-1', 'rcpt-2', 'res-2', 'file', 'https://q.test/2', '6h', 'ch-1', 'voice');

      updateSendDMStatus('send-4', 'rcpt-2', 'failed');

      const row = testDb.prepare('SELECT dm_status FROM qurl_sends WHERE send_id = ? AND recipient_discord_id = ?').get('send-4', 'rcpt-2');
      expect(row.dm_status).toBe('failed');
    });

    it('only updates the targeted recipient in a multi-recipient send', () => {
      recordQURLSend('send-5', 'sender-1', 'rcpt-1', 'res-1', 'url', 'https://q.test/1', '24h', 'ch-1', 'channel');
      recordQURLSend('send-5', 'sender-1', 'rcpt-2', 'res-2', 'url', 'https://q.test/2', '24h', 'ch-1', 'channel');

      updateSendDMStatus('send-5', 'rcpt-1', 'sent');

      const rcpt1 = testDb.prepare('SELECT dm_status FROM qurl_sends WHERE send_id = ? AND recipient_discord_id = ?').get('send-5', 'rcpt-1');
      const rcpt2 = testDb.prepare('SELECT dm_status FROM qurl_sends WHERE send_id = ? AND recipient_discord_id = ?').get('send-5', 'rcpt-2');

      expect(rcpt1.dm_status).toBe('sent');
      expect(rcpt2.dm_status).toBe('pending');
    });
  });

  describe('getRecentSends', () => {
    it('groups by send_id and counts recipients', () => {
      recordQURLSend('send-6', 'sender-1', 'rcpt-1', 'res-1', 'url', 'https://q.test/1', '24h', 'ch-1', 'channel');
      recordQURLSend('send-6', 'sender-1', 'rcpt-2', 'res-2', 'url', 'https://q.test/2', '24h', 'ch-1', 'channel');

      updateSendDMStatus('send-6', 'rcpt-1', 'sent');

      const sends = getRecentSends('sender-1');
      expect(sends).toHaveLength(1);
      expect(sends[0].send_id).toBe('send-6');
      expect(sends[0].recipient_count).toBe(2);
      expect(sends[0].delivered_count).toBe(1);
      expect(sends[0].resource_type).toBe('url');
      expect(sends[0].target_type).toBe('channel');
    });

    it('orders by created_at descending', () => {
      // Insert older send
      testDb.prepare(`
        INSERT INTO qurl_sends (send_id, sender_discord_id, recipient_discord_id, resource_id, resource_type, qurl_link, expires_in, channel_id, target_type, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
      `).run('send-old', 'sender-1', 'rcpt-1', 'res-1', 'url', 'link1', '24h', 'ch-1', 'user', '2026-01-01T00:00:00Z');

      // Insert newer send
      testDb.prepare(`
        INSERT INTO qurl_sends (send_id, sender_discord_id, recipient_discord_id, resource_id, resource_type, qurl_link, expires_in, channel_id, target_type, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
      `).run('send-new', 'sender-1', 'rcpt-2', 'res-2', 'file', 'link2', '1h', 'ch-2', 'voice', '2026-04-01T00:00:00Z');

      const sends = getRecentSends('sender-1');
      expect(sends).toHaveLength(2);
      expect(sends[0].send_id).toBe('send-new');
      expect(sends[1].send_id).toBe('send-old');
    });

    it('respects the limit parameter', () => {
      for (let i = 0; i < 5; i++) {
        recordQURLSend(`send-${i}`, 'sender-1', 'rcpt-1', `res-${i}`, 'url', `link-${i}`, '24h', 'ch-1', 'user');
      }

      const sends = getRecentSends('sender-1', 3);
      expect(sends).toHaveLength(3);
    });

    it('returns empty array when no sends exist', () => {
      const sends = getRecentSends('no-sends-user');
      expect(sends).toEqual([]);
    });

    it('only returns sends for the requested sender', () => {
      recordQURLSend('send-a', 'sender-1', 'rcpt-1', 'res-1', 'url', 'link1', '24h', 'ch-1', 'user');
      recordQURLSend('send-b', 'sender-2', 'rcpt-1', 'res-2', 'url', 'link2', '24h', 'ch-1', 'user');

      const sender1Sends = getRecentSends('sender-1');
      expect(sender1Sends).toHaveLength(1);
      expect(sender1Sends[0].send_id).toBe('send-a');
    });
  });

  describe('cleanupOldSends', () => {
    it('deletes rows older than 30 days', () => {
      // Insert a row with old created_at
      testDb.prepare(`
        INSERT INTO qurl_sends (send_id, sender_discord_id, recipient_discord_id, resource_id, resource_type, qurl_link, expires_in, channel_id, target_type, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now', '-31 days'))
      `).run('old-send', 'sender-1', 'rcpt-1', 'res-1', 'url', 'link1', '24h', 'ch-1', 'user');

      // Insert a recent row
      recordQURLSend('recent-send', 'sender-1', 'rcpt-1', 'res-2', 'url', 'link2', '24h', 'ch-1', 'user');

      const deleted = cleanupOldSends();
      expect(deleted).toBe(1);

      // Verify the recent one still exists
      const remaining = testDb.prepare('SELECT COUNT(*) as count FROM qurl_sends').get();
      expect(remaining.count).toBe(1);
    });

    it('does nothing when no old rows exist', () => {
      recordQURLSend('fresh', 'sender-1', 'rcpt-1', 'res-1', 'url', 'link1', '24h', 'ch-1', 'user');

      const deleted = cleanupOldSends();
      expect(deleted).toBe(0);
    });
  });
});

// =========================================================================
// 6. Database saveSendConfig + getSendConfig roundtrip (in-memory SQLite)
// =========================================================================

describe('Database saveSendConfig + getSendConfig', () => {
  let testDb;
  let saveSendConfig;
  let getSendConfig;

  beforeEach(() => {
    testDb = new Database(':memory:');
    testDb.exec(`
      CREATE TABLE IF NOT EXISTS qurl_send_configs (
        send_id TEXT PRIMARY KEY,
        sender_discord_id TEXT NOT NULL,
        resource_type TEXT NOT NULL,
        connector_resource_id TEXT,
        actual_url TEXT,
        expires_in TEXT NOT NULL,
        personal_message TEXT,
        location_name TEXT,
        attachment_name TEXT,
        created_at TEXT DEFAULT CURRENT_TIMESTAMP
      );
    `);

    saveSendConfig = (sendId, senderDiscordId, resourceType, connectorResourceId, actualUrl, expiresIn, personalMessage, locationName, attachmentName) => {
      const stmt = testDb.prepare(`
        INSERT OR REPLACE INTO qurl_send_configs (send_id, sender_discord_id, resource_type, connector_resource_id, actual_url, expires_in, personal_message, location_name, attachment_name)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
      `);
      stmt.run(sendId, senderDiscordId, resourceType, connectorResourceId, actualUrl, expiresIn, personalMessage, locationName, attachmentName);
    };

    getSendConfig = (sendId, senderDiscordId) => {
      const stmt = testDb.prepare('SELECT * FROM qurl_send_configs WHERE send_id = ? AND sender_discord_id = ?');
      return stmt.get(sendId, senderDiscordId);
    };
  });

  afterEach(() => {
    testDb.close();
  });

  it('saves and retrieves a URL send config with all fields', () => {
    saveSendConfig('send-100', 'user-A', 'url', null, 'https://example.com', '24h', 'Check this out', null, null);

    const config = getSendConfig('send-100', 'user-A');
    expect(config).toBeDefined();
    expect(config.send_id).toBe('send-100');
    expect(config.sender_discord_id).toBe('user-A');
    expect(config.resource_type).toBe('url');
    expect(config.connector_resource_id).toBeNull();
    expect(config.actual_url).toBe('https://example.com');
    expect(config.expires_in).toBe('24h');
    expect(config.personal_message).toBe('Check this out');
    expect(config.location_name).toBeNull();
    expect(config.attachment_name).toBeNull();
    expect(config.created_at).toBeDefined();
  });

  it('saves and retrieves a file send config', () => {
    saveSendConfig('send-101', 'user-A', 'file', 'conn-res-99', null, '6h', null, null, 'report.pdf');

    const config = getSendConfig('send-101', 'user-A');
    expect(config).toBeDefined();
    expect(config.resource_type).toBe('file');
    expect(config.connector_resource_id).toBe('conn-res-99');
    expect(config.actual_url).toBeNull();
    expect(config.attachment_name).toBe('report.pdf');
  });

  it('saves and retrieves a maps send config', () => {
    saveSendConfig('send-102', 'user-A', 'maps', null, 'https://www.google.com/maps/place/Eiffel+Tower', '1h', 'Meet here', 'Eiffel Tower', null);

    const config = getSendConfig('send-102', 'user-A');
    expect(config).toBeDefined();
    expect(config.resource_type).toBe('maps');
    expect(config.actual_url).toBe('https://www.google.com/maps/place/Eiffel+Tower');
    expect(config.location_name).toBe('Eiffel Tower');
    expect(config.personal_message).toBe('Meet here');
  });

  it('enforces ownership — user B cannot read user A config', () => {
    saveSendConfig('send-103', 'user-A', 'url', null, 'https://secret.com', '24h', null, null, null);

    const configA = getSendConfig('send-103', 'user-A');
    expect(configA).toBeDefined();

    const configB = getSendConfig('send-103', 'user-B');
    expect(configB).toBeUndefined();
  });

  it('returns undefined for non-existent send_id', () => {
    const config = getSendConfig('nonexistent', 'user-A');
    expect(config).toBeUndefined();
  });

  it('upserts on duplicate send_id (INSERT OR REPLACE)', () => {
    saveSendConfig('send-104', 'user-A', 'url', null, 'https://old.com', '24h', null, null, null);
    saveSendConfig('send-104', 'user-A', 'url', null, 'https://new.com', '1h', 'updated', null, null);

    const config = getSendConfig('send-104', 'user-A');
    expect(config.actual_url).toBe('https://new.com');
    expect(config.expires_in).toBe('1h');
    expect(config.personal_message).toBe('updated');
  });
});

// =========================================================================
// 7. handleAddRecipients logic
// =========================================================================

describe('handleAddRecipients', () => {
  let handleAddRecipients;
  let mockDb;
  let mockMintLinks;
  let mockDownloadAndUpload;
  let mockReUploadBuffer;
  let mockUploadJsonToConnector;
  let mockCreateOneTimeLink;
  let mockSendDM;

  beforeEach(() => {
    jest.resetModules();

    // Mock config
    jest.mock('../src/config', () => ({
      QURL_API_KEY: 'test-api-key',
      QURL_ENDPOINT: 'https://api.test.local',
      CONNECTOR_URL: 'https://connector.test.local',
      GOOGLE_MAPS_API_KEY: 'test-google-key',
      QURL_SEND_COOLDOWN_MS: 30000,
      QURL_SEND_MAX_RECIPIENTS: 50,
      DATABASE_PATH: ':memory:',
      PENDING_LINK_EXPIRY_MINUTES: 30,
      ADMIN_USER_IDS: [],
    }));

    // Mock logger
    jest.mock('../src/logger', () => ({
      info: jest.fn(),
      warn: jest.fn(),
      error: jest.fn(),
      debug: jest.fn(),
    }));

    // Mock discord.js
    jest.mock('discord.js', () => ({
      SlashCommandBuilder: jest.fn().mockImplementation(() => {
        const builder = {
          setName: jest.fn().mockReturnThis(),
          setDescription: jest.fn().mockReturnThis(),
          addSubcommand: jest.fn().mockReturnThis(),
          addStringOption: jest.fn().mockReturnThis(),
          addUserOption: jest.fn().mockReturnThis(),
          addAttachmentOption: jest.fn().mockReturnThis(),
          addIntegerOption: jest.fn().mockReturnThis(),
          setDefaultMemberPermissions: jest.fn().mockReturnThis(),
          toJSON: jest.fn().mockReturnValue({}),
        };
        return builder;
      }),
      EmbedBuilder: jest.fn().mockImplementation(() => ({
        setColor: jest.fn().mockReturnThis(),
        setTitle: jest.fn().mockReturnThis(),
        setDescription: jest.fn().mockReturnThis(),
        setAuthor: jest.fn().mockReturnThis(),
        addFields: jest.fn().mockReturnThis(),
        setFooter: jest.fn().mockReturnThis(),
        setTimestamp: jest.fn().mockReturnThis(),
        setThumbnail: jest.fn().mockReturnThis(),
        setURL: jest.fn().mockReturnThis(),
      })),
      PermissionFlagsBits: { ManageRoles: 1n, Administrator: 8n },
      ActionRowBuilder: jest.fn().mockImplementation(() => {
        const row = { components: [], addComponents: jest.fn(function (...args) {
          row.components.push(...args.flat());
          return row;
        }) };
        return row;
      }),
      ButtonBuilder: jest.fn().mockImplementation(() => ({
        setCustomId: jest.fn().mockReturnThis(),
        setLabel: jest.fn().mockReturnThis(),
        setStyle: jest.fn().mockReturnThis(),
        setURL: jest.fn().mockReturnThis(),
      })),
      ButtonStyle: { Primary: 1, Secondary: 2, Success: 3, Danger: 4, Link: 5 },
      ComponentType: { Button: 2, StringSelect: 3, UserSelect: 5 },
      StringSelectMenuBuilder: jest.fn().mockImplementation(() => ({
        setCustomId: jest.fn().mockReturnThis(),
        setPlaceholder: jest.fn().mockReturnThis(),
        addOptions: jest.fn().mockReturnThis(),
      })),
      UserSelectMenuBuilder: jest.fn().mockImplementation(() => ({
        setCustomId: jest.fn().mockReturnThis(),
        setPlaceholder: jest.fn().mockReturnThis(),
        setMinValues: jest.fn().mockReturnThis(),
        setMaxValues: jest.fn().mockReturnThis(),
      })),
      ModalBuilder: jest.fn().mockImplementation(() => ({
        setCustomId: jest.fn().mockReturnThis(),
        setTitle: jest.fn().mockReturnThis(),
        addComponents: jest.fn().mockReturnThis(),
      })),
      TextInputBuilder: jest.fn().mockImplementation(() => ({
        setCustomId: jest.fn().mockReturnThis(),
        setLabel: jest.fn().mockReturnThis(),
        setPlaceholder: jest.fn().mockReturnThis(),
        setStyle: jest.fn().mockReturnThis(),
        setMaxLength: jest.fn().mockReturnThis(),
        setRequired: jest.fn().mockReturnThis(),
      })),
      TextInputStyle: { Short: 1, Paragraph: 2 },
    }));

    // Mock database
    mockDb = {
      getSendConfig: jest.fn(),
      saveSendConfig: jest.fn(),
      recordQURLSend: jest.fn(),
      recordQURLSendBatch: jest.fn(),
      updateSendDMStatus: jest.fn(),
      getRecentSends: jest.fn(() => []),
      getSendResourceIds: jest.fn(() => []),
    };
    jest.mock('../src/database', () => mockDb);

    // Mock discord helper
    mockSendDM = jest.fn().mockResolvedValue(true);
    jest.mock('../src/discord', () => ({
      assignContributorRole: jest.fn(),
      notifyPRMerge: jest.fn(),
      notifyBadgeEarned: jest.fn(),
      postGoodFirstIssue: jest.fn(),
      postReleaseAnnouncement: jest.fn(),
      postStarMilestone: jest.fn(),
      postToGitHubFeed: jest.fn(),
      sendDM: mockSendDM,
      getVoiceChannelMembers: jest.fn(),
      getTextChannelMembers: jest.fn(),
    }));

    // Mock admin util
    jest.mock('../src/utils/admin', () => ({
      requireAdmin: jest.fn(() => true),
      isAdmin: jest.fn(() => false),
    }));

    // Mock qurl
    mockCreateOneTimeLink = jest.fn();
    jest.mock('../src/qurl', () => ({
      createOneTimeLink: mockCreateOneTimeLink,
      deleteLink: jest.fn(),
    }));

    // Mock connector
    mockMintLinks = jest.fn();
    mockDownloadAndUpload = jest.fn();
    mockReUploadBuffer = jest.fn();
    mockUploadJsonToConnector = jest.fn();
    jest.mock('../src/connector', () => ({
      uploadToConnector: jest.fn(),
      downloadAndUpload: mockDownloadAndUpload,
      reUploadBuffer: mockReUploadBuffer,
      mintLinks: mockMintLinks,
      uploadJsonToConnector: mockUploadJsonToConnector,
      isAllowedSourceUrl: (url) => typeof url === 'string' && url.startsWith('https://cdn.discordapp.com'),
    }));

    // Mock places
    jest.mock('../src/places', () => ({
      searchPlaces: jest.fn(),
    }));

    // Mock form-data
    jest.mock('form-data', () => {
      return jest.fn().mockImplementation(() => ({
        append: jest.fn(),
        getHeaders: jest.fn(() => ({ 'content-type': 'multipart/form-data; boundary=---' })),
      }));
    });

    const commands = require('../src/commands');
    handleAddRecipients = commands._test.handleAddRecipients;
  });

  // Helper to create a Discord-like users Collection (Map with filter/map support)
  function makeUsersCollection(users) {
    const map = new Map(users.map(u => [u.id, u]));
    map.filter = function (fn) {
      const filtered = new Map();
      for (const [k, v] of this) {
        if (fn(v, k, this)) filtered.set(k, v);
      }
      filtered.filter = map.filter.bind(filtered);
      filtered.map = map.map.bind(filtered);
      return filtered;
    };
    map.map = function (fn) {
      const arr = [];
      for (const [k, v] of this) {
        arr.push(fn(v, k, this));
      }
      return arr;
    };
    return map;
  }

  const mockOriginalInteraction = {
    user: { id: 'sender-1', username: 'TestSender' },
    channelId: 'ch-99',
  };

  it('returns error when send config is not found', async () => {
    mockDb.getSendConfig.mockReturnValue(undefined);

    const users = makeUsersCollection([
      { id: 'rcpt-1', bot: false, username: 'user1' },
    ]);

    const result = await handleAddRecipients('nonexistent-send', users, mockOriginalInteraction, 'test-api-key');
    expect(result.msg).toBe('Send configuration not found.');
  });

  it('returns error when all recipients are bots', async () => {
    mockDb.getSendConfig.mockReturnValue({
      resource_type: 'url',
      actual_url: 'https://example.com',
      expires_in: '24h',
    });

    const users = makeUsersCollection([
      { id: 'bot-1', bot: true, username: 'BotUser' },
      { id: 'bot-2', bot: true, username: 'AnotherBot' },
    ]);

    const result = await handleAddRecipients('send-1', users, mockOriginalInteraction, 'test-api-key');
    expect(result.msg).toBe('No valid recipients selected (bots and yourself are excluded).');
  });

  it('returns error when only recipient is the sender', async () => {
    mockDb.getSendConfig.mockReturnValue({
      resource_type: 'url',
      actual_url: 'https://example.com',
      expires_in: '24h',
    });

    const users = makeUsersCollection([
      { id: 'sender-1', bot: false, username: 'SenderSelf' },
    ]);

    const result = await handleAddRecipients('send-1', users, mockOriginalInteraction, 'test-api-key');
    expect(result.msg).toBe('No valid recipients selected (bots and yourself are excluded).');
  });

  it('returns error when all recipients are bots or sender', async () => {
    mockDb.getSendConfig.mockReturnValue({
      resource_type: 'url',
      actual_url: 'https://example.com',
      expires_in: '24h',
    });

    const users = makeUsersCollection([
      { id: 'sender-1', bot: false, username: 'SenderSelf' },
      { id: 'bot-1', bot: true, username: 'BotUser' },
    ]);

    const result = await handleAddRecipients('send-1', users, mockOriginalInteraction, 'test-api-key');
    expect(result.msg).toBe('No valid recipients selected (bots and yourself are excluded).');
  });

  it('file send: mints new links via connector and delivers DMs', async () => {
    mockDb.getSendConfig.mockReturnValue({
      resource_type: 'file',
      connector_resource_id: 'conn-res-42',
      actual_url: null,
      expires_in: '6h',
      personal_message: 'Here is the file',
      location_name: null,
      attachment_name: 'report.pdf',
      attachment_content_type: 'application/pdf',
      attachment_url: 'https://cdn.discordapp.com/attachments/1/2/report.pdf',
    });

    mockDownloadAndUpload.mockResolvedValue({
      resource_id: 'conn-res-43',
      fileBuffer: new ArrayBuffer(8),
    });

    mockMintLinks.mockResolvedValue([
      { qurl_link: 'https://q.test/mint-1' },
      { qurl_link: 'https://q.test/mint-2' },
    ]);

    mockSendDM.mockResolvedValue(true);

    const users = makeUsersCollection([
      { id: 'rcpt-1', bot: false, username: 'Alice' },
      { id: 'rcpt-2', bot: false, username: 'Bob' },
    ]);

    const result = await handleAddRecipients('send-file-1', users, mockOriginalInteraction, 'test-api-key');

    // Re-uploads a fresh resource from the stored attachment URL rather than
    // reusing the (possibly-drained) original connector_resource_id
    expect(mockDownloadAndUpload).toHaveBeenCalledWith(
      'https://cdn.discordapp.com/attachments/1/2/report.pdf',
      'report.pdf',
      'application/pdf',
      'test-api-key',
    );
    // mintLinks is called against the NEW resource (conn-res-43)
    expect(mockMintLinks).toHaveBeenCalledWith('conn-res-43', expect.any(String), 2, 'test-api-key');
    // createOneTimeLink should NOT have been called
    expect(mockCreateOneTimeLink).not.toHaveBeenCalled();
    // DMs should have been sent
    expect(mockSendDM).toHaveBeenCalledTimes(2);
    // DB should record the new sends
    expect(mockDb.recordQURLSendBatch).toHaveBeenCalledTimes(1);
    expect(mockDb.recordQURLSendBatch.mock.calls[0][0]).toHaveLength(2);
    expect(result.msg).toMatch(/Added 2 recipients/);
  });

  it('URL send: uploads location JSON and mints links', async () => {
    mockDb.getSendConfig.mockReturnValue({
      resource_type: 'url',
      connector_resource_id: null,
      actual_url: 'https://example.com/doc',
      expires_in: '24h',
      personal_message: null,
      location_name: null,
      attachment_name: null,
    });

    mockUploadJsonToConnector.mockResolvedValue({
      resource_id: 'res-loc-1',
      hash: 'loc-hash',
    });
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/otl-1' }]);
    mockSendDM.mockResolvedValue(true);

    const users = makeUsersCollection([
      { id: 'rcpt-1', bot: false, username: 'Charlie' },
    ]);

    const result = await handleAddRecipients('send-url-1', users, mockOriginalInteraction, 'test-api-key');

    expect(mockUploadJsonToConnector).toHaveBeenCalledWith(
      expect.objectContaining({ type: 'google-map', url: 'https://example.com/doc' }),
      'location.json', 'test-api-key',
    );
    expect(mockMintLinks).toHaveBeenCalledWith('res-loc-1', expect.any(String), 1, 'test-api-key');
    expect(mockSendDM).toHaveBeenCalledTimes(1);
    expect(mockDb.recordQURLSendBatch).toHaveBeenCalledTimes(1);
    expect(mockDb.recordQURLSendBatch.mock.calls[0][0]).toHaveLength(1);
    expect(result.msg).toMatch(/Added 1 recipient/);
  });

  it('maps send: uploads location JSON with location_name and mints links', async () => {
    mockDb.getSendConfig.mockReturnValue({
      resource_type: 'maps',
      connector_resource_id: null,
      actual_url: 'https://www.google.com/maps/place/Eiffel+Tower',
      expires_in: '1h',
      personal_message: null,
      location_name: 'Eiffel Tower',
      attachment_name: null,
    });

    mockUploadJsonToConnector.mockResolvedValue({
      resource_id: 'conn-loc-maps',
      hash: 'h-maps',
      success: true,
    });
    mockMintLinks.mockResolvedValue([{ qurl_link: 'https://q.test/maps-1' }]);
    mockSendDM.mockResolvedValue(true);

    const users = makeUsersCollection([
      { id: 'rcpt-1', bot: false, username: 'Dana' },
    ]);

    const result = await handleAddRecipients('send-maps-1', users, mockOriginalInteraction, 'test-api-key');

    // Should use uploadJsonToConnector with the location name
    expect(mockUploadJsonToConnector).toHaveBeenCalledWith(
      expect.objectContaining({ type: 'google-map', url: 'https://www.google.com/maps/place/Eiffel+Tower', name: 'Eiffel Tower' }),
      'location.json', 'test-api-key',
    );
    expect(mockMintLinks).toHaveBeenCalledWith('conn-loc-maps', expect.any(String), 1, 'test-api-key');
    expect(mockCreateOneTimeLink).not.toHaveBeenCalled();
    expect(result.msg).toMatch(/Added 1 recipient/);
  });

  it('reports partial DM failures', async () => {
    mockDb.getSendConfig.mockReturnValue({
      resource_type: 'url',
      connector_resource_id: null,
      actual_url: 'https://example.com',
      expires_in: '24h',
      personal_message: null,
      location_name: null,
      attachment_name: null,
    });

    mockUploadJsonToConnector.mockResolvedValue({ resource_id: 'conn-loc-partial', hash: 'hp', success: true });
    mockMintLinks.mockResolvedValue([
      { qurl_link: 'https://q.test/link1' },
      { qurl_link: 'https://q.test/link2' },
    ]);

    // First DM succeeds, second fails
    mockSendDM
      .mockResolvedValueOnce(true)
      .mockResolvedValueOnce(false);

    const users = makeUsersCollection([
      { id: 'rcpt-1', bot: false, username: 'Alice' },
      { id: 'rcpt-2', bot: false, username: 'Bob' },
    ]);

    const result = await handleAddRecipients('send-partial', users, mockOriginalInteraction, 'test-api-key');

    expect(result.msg).toMatch(/Added 1 recipient/);
    expect(result.msg).toMatch(/1 could not be reached/);
    expect(mockDb.updateSendDMStatus).toHaveBeenCalledWith('send-partial', 'rcpt-1', 'sent');
    expect(mockDb.updateSendDMStatus).toHaveBeenCalledWith('send-partial', 'rcpt-2', 'failed');
  });

  it('returns error when send config is incomplete (no connector_resource_id and no actual_url)', async () => {
    mockDb.getSendConfig.mockReturnValue({
      resource_type: 'url',
      connector_resource_id: null,
      actual_url: null,
      expires_in: '24h',
      personal_message: null,
      location_name: null,
      attachment_name: null,
    });

    const users = makeUsersCollection([
      { id: 'rcpt-1', bot: false, username: 'Eve' },
    ]);

    const result = await handleAddRecipients('send-broken', users, mockOriginalInteraction, 'test-api-key');
    expect(result.msg).toBe('Cannot add recipients \u2014 send configuration is incomplete.');
  });

  it('filters bots from mixed collection and sends only to real users', async () => {
    mockDb.getSendConfig.mockReturnValue({
      resource_type: 'url',
      connector_resource_id: null,
      actual_url: 'https://example.com',
      expires_in: '24h',
      personal_message: null,
      location_name: null,
      attachment_name: null,
    });

    mockUploadJsonToConnector.mockResolvedValue({ resource_id: 'conn-loc-mixed', hash: 'hm', success: true });
    mockMintLinks.mockResolvedValue([
      { qurl_link: 'https://q.test/link1' },
      { qurl_link: 'https://q.test/link2' },
    ]);

    mockSendDM.mockResolvedValue(true);

    const users = makeUsersCollection([
      { id: 'rcpt-1', bot: false, username: 'Alice' },
      { id: 'bot-1', bot: true, username: 'MusicBot' },
      { id: 'rcpt-2', bot: false, username: 'Bob' },
      { id: 'sender-1', bot: false, username: 'SenderSelf' },
    ]);

    const result = await handleAddRecipients('send-mixed', users, mockOriginalInteraction, 'test-api-key');

    // Only Alice and Bob should get DMs (bot and sender excluded)
    expect(mockSendDM).toHaveBeenCalledTimes(2);
    expect(mockUploadJsonToConnector).toHaveBeenCalledTimes(1);
    expect(mockMintLinks).toHaveBeenCalledWith('conn-loc-mixed', expect.any(String), 2, 'test-api-key');
    expect(result.msg).toMatch(/Added 2 recipients/);
  });

  it('returns failure message when link creation throws', async () => {
    mockDb.getSendConfig.mockReturnValue({
      resource_type: 'file',
      connector_resource_id: 'conn-res-1',
      actual_url: null,
      expires_in: '6h',
      personal_message: null,
      location_name: null,
      attachment_name: 'file.bin',
      attachment_content_type: 'application/octet-stream',
      attachment_url: 'https://cdn.discordapp.com/attachments/1/2/file.bin',
    });

    mockDownloadAndUpload.mockResolvedValue({ resource_id: 'new-res', fileBuffer: new ArrayBuffer(4) });
    mockMintLinks.mockRejectedValue(new Error('Connector down'));

    const users = makeUsersCollection([
      { id: 'rcpt-1', bot: false, username: 'Alice' },
    ]);

    const result = await handleAddRecipients('send-fail', users, mockOriginalInteraction, 'test-api-key');
    expect(result.msg).toMatch(/Failed to prepare links/);
  });

  it('file send: batches > 10 new recipients into multiple mintLinks calls', async () => {
    mockDb.getSendConfig.mockReturnValue({
      resource_type: 'file',
      connector_resource_id: 'conn-res-50',
      actual_url: null,
      expires_in: '24h',
      personal_message: null,
      location_name: null,
      attachment_name: 'big.pdf',
      attachment_content_type: 'application/pdf',
      attachment_url: 'https://cdn.discordapp.com/attachments/1/2/big.pdf',
    });

    // 12 recipients = 2 batches (10 + 2), each on a fresh resource
    const userList = [];
    for (let i = 0; i < 12; i++) {
      userList.push({ id: `rcpt-${i}`, bot: false, username: `User${i}` });
    }

    const fileBuffer = new ArrayBuffer(16);
    mockDownloadAndUpload.mockResolvedValue({ resource_id: 'new-res-A', fileBuffer });
    mockReUploadBuffer.mockResolvedValue({ resource_id: 'new-res-B' });

    const batch1Links = Array.from({ length: 10 }, (_, i) => ({ qurl_link: `https://q.test/r-${i}` }));
    const batch2Links = Array.from({ length: 2 }, (_, i) => ({ qurl_link: `https://q.test/r2-${i}` }));
    mockMintLinks
      .mockResolvedValueOnce(batch1Links)
      .mockResolvedValueOnce(batch2Links);

    mockSendDM.mockResolvedValue(true);

    const users = makeUsersCollection(userList);
    const result = await handleAddRecipients('send-batch', users, mockOriginalInteraction, 'test-api-key');

    expect(mockDownloadAndUpload).toHaveBeenCalledTimes(1);
    expect(mockReUploadBuffer).toHaveBeenCalledTimes(1);
    expect(mockMintLinks).toHaveBeenCalledTimes(2);
    expect(mockMintLinks).toHaveBeenCalledWith('new-res-A', expect.any(String), 10, 'test-api-key');
    expect(mockMintLinks).toHaveBeenCalledWith('new-res-B', expect.any(String), 2, 'test-api-key');
    expect(result.msg).toMatch(/Added 12 recipients/);
  });

  it('file send: addRecipients works for ≤ 10 new recipients', async () => {
    mockDb.getSendConfig.mockReturnValue({
      resource_type: 'file',
      connector_resource_id: 'conn-res-ok',
      actual_url: null,
      expires_in: '24h',
      personal_message: null,
      location_name: null,
      attachment_name: 'thing.pdf',
      attachment_content_type: 'application/pdf',
      attachment_url: 'https://cdn.discordapp.com/attachments/1/2/thing.pdf',
    });

    const userList = [];
    for (let i = 0; i < 8; i++) {
      userList.push({ id: `rcpt-${i}`, bot: false, username: `User${i}` });
    }

    mockDownloadAndUpload.mockResolvedValue({ resource_id: 'new-res-C', fileBuffer: new ArrayBuffer(8) });
    const links = Array.from({ length: 8 }, (_, i) => ({ qurl_link: `https://q.test/l-${i}` }));
    mockMintLinks.mockResolvedValueOnce(links);
    mockSendDM.mockResolvedValue(true);

    const users = makeUsersCollection(userList);
    const result = await handleAddRecipients('send-ok', users, mockOriginalInteraction, 'test-api-key');

    expect(mockDownloadAndUpload).toHaveBeenCalledTimes(1);
    expect(mockMintLinks).toHaveBeenCalledTimes(1);
    expect(mockMintLinks).toHaveBeenCalledWith('new-res-C', expect.any(String), 8, 'test-api-key');
    expect(mockSendDM).toHaveBeenCalledTimes(8);
    expect(result.msg).toMatch(/Added 8 recipients/);
  });

  it('URL send: returns failure when uploadJsonToConnector rejects', async () => {
    mockDb.getSendConfig.mockReturnValue({
      resource_type: 'url',
      connector_resource_id: null,
      actual_url: 'https://example.com/failing',
      expires_in: '24h',
      personal_message: null,
      location_name: null,
      attachment_name: null,
    });

    mockUploadJsonToConnector.mockRejectedValue(new Error('Connector upload failed'));

    const users = makeUsersCollection([
      { id: 'rcpt-1', bot: false, username: 'Alice' },
      { id: 'rcpt-2', bot: false, username: 'Bob' },
    ]);

    const result = await handleAddRecipients('send-allfail', users, mockOriginalInteraction, 'test-api-key');
    expect(result.msg).toBe('Failed to create links for new recipients.');
    expect(mockSendDM).not.toHaveBeenCalled();
  });
});

// =========================================================================
// 8. Additional isGoogleMapsURL tests (URL parsing edge cases)
// =========================================================================

describe('isGoogleMapsURL — additional URL parsing', () => {
  it('matches google.com.au/maps (two-part country TLD)', () => {
    expect(isGoogleMapsURL('https://www.google.com.au/maps/place/Sydney')).toBe(true);
    expect(isGoogleMapsURL('https://google.com.au/maps?q=melbourne')).toBe(true);
  });

  it('matches google.co.in/maps', () => {
    expect(isGoogleMapsURL('https://google.co.in/maps/place/TajMahal')).toBe(true);
  });

  it('matches maps.google.com.au', () => {
    expect(isGoogleMapsURL('https://maps.google.com.au/?q=sydney')).toBe(true);
  });

  it('matches maps.google.co.in', () => {
    expect(isGoogleMapsURL('https://maps.google.co.in/')).toBe(true);
  });

  it('rejects ftp://google.com/maps (protocol check)', () => {
    expect(isGoogleMapsURL('ftp://google.com/maps')).toBe(false);
    expect(isGoogleMapsURL('ftp://www.google.com/maps/place/test')).toBe(false);
  });

  it('rejects file:// protocol', () => {
    expect(isGoogleMapsURL('file://google.com/maps')).toBe(false);
  });

  it('rejects data: protocol', () => {
    expect(isGoogleMapsURL('data:text/html,google.com/maps')).toBe(false);
  });

  it('rejects google.com without /maps path', () => {
    expect(isGoogleMapsURL('https://www.google.com.au/search?q=maps')).toBe(false);
  });

  it('handles uppercase in hostname', () => {
    // URL constructor lowercases hostname, so this should still match
    expect(isGoogleMapsURL('https://WWW.GOOGLE.COM/maps/place/test')).toBe(true);
  });

  it('handles trailing slash on maps path', () => {
    expect(isGoogleMapsURL('https://google.com.au/maps/')).toBe(true);
  });

  it('handles google.de/maps (single-part TLD)', () => {
    expect(isGoogleMapsURL('https://google.de/maps/place/Berlin')).toBe(true);
  });
});
