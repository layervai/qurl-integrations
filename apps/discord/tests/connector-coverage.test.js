/**
 * Additional connector.js tests for 90%+ coverage.
 * Covers: isAllowedSourceUrl catch block (line 15), connectorAuthHeaders with key (line 26),
 * uploadToConnector SSRF rejection (line 38), mintLinks null links (line 96).
 */

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
}));

const originalFetch = globalThis.fetch;

describe('Connector client — coverage boost', () => {
  let connector;

  beforeEach(() => {
    jest.resetModules();
    jest.mock('../src/config', () => ({
      CONNECTOR_URL: 'https://connector.test.local',
      QURL_API_KEY: 'test-key-for-connector',
    }));
    jest.mock('../src/logger', () => ({
      info: jest.fn(),
      warn: jest.fn(),
      error: jest.fn(),
      debug: jest.fn(),
    }));
    connector = require('../src/connector');
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  describe('isAllowedSourceUrl', () => {
    it('returns false for invalid URL string (catch block — line 15)', () => {
      expect(connector.isAllowedSourceUrl('not-a-url-at-all')).toBe(false);
    });

    it('returns false for empty string', () => {
      expect(connector.isAllowedSourceUrl('')).toBe(false);
    });

    it('returns false for non-https protocol', () => {
      expect(connector.isAllowedSourceUrl('http://cdn.discordapp.com/file.png')).toBe(false);
    });

    it('returns false for non-Discord host', () => {
      expect(connector.isAllowedSourceUrl('https://evil.com/file.png')).toBe(false);
    });

    it('returns true for cdn.discordapp.com', () => {
      expect(connector.isAllowedSourceUrl('https://cdn.discordapp.com/path/file.png')).toBe(true);
    });

    it('returns true for media.discordapp.net', () => {
      expect(connector.isAllowedSourceUrl('https://media.discordapp.net/path/file.png')).toBe(true);
    });
  });

  describe('uploadToConnector — SSRF rejection (line 38)', () => {
    it('throws for non-Discord CDN source URL', async () => {
      await expect(connector.uploadToConnector('https://evil.com/malicious.bin', 'f.bin', 'image/png'))
        .rejects.toThrow('Source URL is not a valid Discord CDN URL');
    });

    it('throws for invalid URL string', async () => {
      await expect(connector.uploadToConnector('garbage', 'f.bin', 'image/png'))
        .rejects.toThrow('Source URL is not a valid Discord CDN URL');
    });
  });

  describe('uploadToConnector — auth headers and arrayBuffer (line 26)', () => {
    it('includes Authorization header in upload when QURL_API_KEY is set', async () => {
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true,
          arrayBuffer: async () => new ArrayBuffer(10),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ success: true, hash: 'h1', resource_id: 'r1' }),
        });

      await connector.uploadToConnector(
        'https://cdn.discordapp.com/file.png', 'file.png', 'image/png',
      );

      // The second fetch call (upload to connector) should include auth header
      expect(globalThis.fetch).toHaveBeenCalledTimes(2);
      const uploadHeaders = globalThis.fetch.mock.calls[1][1].headers;
      expect(uploadHeaders['Authorization']).toBe('Bearer test-key-for-connector');
    });
  });

  describe('mintLinks — null/missing links guard (line 96)', () => {
    it('throws when result.links is null', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        json: async () => ({ success: true, links: null }),
      });

      await expect(connector.mintLinks('res-1', '2026-01-01T00:00:00Z', 1))
        .rejects.toThrow('Connector mint_link returned no links array');
    });

    it('throws when result.links is not an array (string)', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        json: async () => ({ success: true, links: 'not-array' }),
      });

      await expect(connector.mintLinks('res-1', '2026-01-01T00:00:00Z', 1))
        .rejects.toThrow('Connector mint_link returned no links array');
    });

    it('throws when result.links is undefined', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        json: async () => ({ success: true }),
      });

      await expect(connector.mintLinks('res-1', '2026-01-01T00:00:00Z', 1))
        .rejects.toThrow('Connector mint_link returned no links array');
    });
  });
});

describe('Connector client — no API key (empty connectorAuthHeaders)', () => {
  let connector;

  beforeEach(() => {
    jest.resetModules();
    jest.mock('../src/config', () => ({
      CONNECTOR_URL: 'https://connector.test.local',
      QURL_API_KEY: '', // empty — should not include Authorization
    }));
    jest.mock('../src/logger', () => ({
      info: jest.fn(),
      warn: jest.fn(),
      error: jest.fn(),
      debug: jest.fn(),
    }));
    connector = require('../src/connector');
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  it('throws when QURL_API_KEY is empty', async () => {
    globalThis.fetch = jest.fn();

    await expect(connector.uploadToConnector(
      'https://cdn.discordapp.com/file.pdf', 'file.pdf', 'application/pdf',
    )).rejects.toThrow('QURL_API_KEY is not configured');

    // No fetch calls should have been made
    expect(globalThis.fetch).not.toHaveBeenCalled();
  });
});
