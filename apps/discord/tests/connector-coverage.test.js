
jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

const mockClient = {
  listAllResources: jest.fn(),
  createQurlForResource: jest.fn(),
  resolve: jest.fn(),
};
jest.mock('@layervai/qurl', () => ({
  QURLClient: jest.fn().mockImplementation(() => mockClient),
}));

function resetDetectSdkMocks() {
  mockClient.listAllResources.mockReset();
  mockClient.createQurlForResource.mockReset();
  mockClient.resolve.mockReset();
}

function mockListAllResources(resources) {
  mockClient.listAllResources.mockImplementation(async function* listAllResourcesMock() {
    for (const resource of resources) yield resource;
  });
}

function mockListAllResourcesOnce(resources) {
  mockClient.listAllResources.mockImplementationOnce(async function* listAllResourcesMock() {
    for (const resource of resources) yield resource;
  });
}

const originalFetch = globalThis.fetch;

describe('@layervai/qurl SDK contract — detect pagination', () => {
  it('exposes listAllResources as an async iterable on the pinned runtime package', () => {
    const { QURLClient: RealQURLClient } = jest.requireActual('@layervai/qurl');
    const client = new RealQURLClient({
      apiKey: 'test-key',
      baseUrl: 'https://qurl.invalid',
    });

    const iterator = client.listAllResources({ slug: 'detect-sandbox', limit: 100 });

    expect(typeof client.listAllResources).toBe('function');
    expect(iterator).toBeTruthy();
    expect(typeof iterator[Symbol.asyncIterator]).toBe('function');
  });
});

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
      audit: jest.fn(),
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

    it('rejects credential-in-URL that smuggles a different host', () => {
      expect(connector.isAllowedSourceUrl('https://cdn.discordapp.com@evil.com/file.png')).toBe(false);
    });

    it('rejects username/password even on an allowed host', () => {
      expect(connector.isAllowedSourceUrl('https://user:pass@cdn.discordapp.com/file.png')).toBe(false);
    });

    it('rejects a non-default port on an allowed host', () => {
      expect(connector.isAllowedSourceUrl('https://cdn.discordapp.com:9999/file.png')).toBe(false);
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
          headers: { get: jest.fn(() => '10') },
          arrayBuffer: async () => new ArrayBuffer(10),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ success: true, hash: 'h1', resource_id: 'r1' }),
        });

      await connector.uploadToConnector(
        'https://cdn.discordapp.com/file.png', 'file.png', 'image/png',
      );

      expect(globalThis.fetch).toHaveBeenCalledTimes(2);
      const uploadHeaders = globalThis.fetch.mock.calls[1][1].headers;
      expect(uploadHeaders['Authorization']).toBe('Bearer test-key-for-connector');
    });
  });

  describe('viewer_ttl_seconds field forwarding', () => {
    function captureUploadFormFields() {
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({ // CDN download (only used by file paths)
          ok: true,
          headers: { get: jest.fn(() => '5') },
          arrayBuffer: async () => new ArrayBuffer(5),
        })
        .mockResolvedValueOnce({ // connector /api/upload
          ok: true,
          json: async () => ({ success: true, hash: 'h1', resource_id: 'r1' }),
        })
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ success: true, hash: 'h2', resource_id: 'r2' }),
        });
      const originalAppend = globalThis.FormData.prototype.append;
      const appended = [];
      globalThis.FormData.prototype.append = function (...args) {
        appended.push({ name: args[0], valueType: typeof args[1], filename: args[2] });
        return originalAppend.apply(this, args);
      };
      const restore = () => { globalThis.FormData.prototype.append = originalAppend; };
      return { appended, restore };
    }

    it('uploadToConnector appends viewer_ttl_seconds when provided', async () => {
      const { appended, restore } = captureUploadFormFields();
      try {
        await connector.uploadToConnector('https://cdn.discordapp.com/x.png', 'x.png', 'image/png', undefined, 30);
      } finally { restore(); }
      expect(appended.find(f => f.name === 'viewer_ttl_seconds')).toMatchObject({ name: 'viewer_ttl_seconds' });
    });

    it('uploadToConnector omits viewer_ttl_seconds when null/undefined', async () => {
      const { appended, restore } = captureUploadFormFields();
      try {
        await connector.uploadToConnector('https://cdn.discordapp.com/x.png', 'x.png', 'image/png', undefined, null);
      } finally { restore(); }
      expect(appended.find(f => f.name === 'viewer_ttl_seconds')).toBeUndefined();
    });

    it('reUploadBuffer appends viewer_ttl_seconds when provided', async () => {
      globalThis.fetch = jest.fn().mockResolvedValueOnce({
        ok: true,
        json: async () => ({ success: true, hash: 'h', resource_id: 'r' }),
      });
      const originalAppend = globalThis.FormData.prototype.append;
      const appended = [];
      globalThis.FormData.prototype.append = function (...args) { appended.push({ name: args[0] }); return originalAppend.apply(this, args); };
      try {
        await connector.reUploadBuffer(Buffer.from('hi'), 'x.txt', 'text/plain', undefined, 0.5);
      } finally { globalThis.FormData.prototype.append = originalAppend; }
      expect(appended.find(f => f.name === 'viewer_ttl_seconds')).toBeDefined();
    });

    it('uploadJsonToConnector appends viewer_ttl_seconds when provided', async () => {
      globalThis.fetch = jest.fn().mockResolvedValueOnce({
        ok: true,
        json: async () => ({ success: true, hash: 'h', resource_id: 'r' }),
      });
      const originalAppend = globalThis.FormData.prototype.append;
      const appended = [];
      globalThis.FormData.prototype.append = function (...args) { appended.push({ name: args[0], value: args[1] }); return originalAppend.apply(this, args); };
      try {
        await connector.uploadJsonToConnector({ type: 'google-map' }, 'loc.json', undefined, 60);
      } finally { globalThis.FormData.prototype.append = originalAppend; }
      const ttlField = appended.find(f => f.name === 'viewer_ttl_seconds');
      expect(ttlField).toBeDefined();
      expect(ttlField.value).toBe('60');
    });

    describe('mintLinks — session_duration forwarding', () => {
      function captureMintBody() {
        let bodyJSON = null;
        globalThis.fetch = jest.fn(async (_url, opts) => {
          bodyJSON = JSON.parse(opts.body);
          return {
            ok: true,
            json: async () => ({ success: true, links: [{ qurl_id: 'q_1', qurl_link: 'https://q.test/l' }] }),
          };
        });
        return () => bodyJSON;
      }

      it('rejects a qURL access token used as a resource ID without echoing it', async () => {
        const logger = require('../src/logger');
        const accessToken = ['at', 'connector-sensitive-marker'].join('_');
        globalThis.fetch = jest.fn();

        const thrown = await connector.mintLinks(accessToken, {
          expiresAt: '2099-01-01T00:00:00Z',
          n: 1,
        }).catch(error => error);

        expect(globalThis.fetch).not.toHaveBeenCalled();
        expect(thrown.message).toBe('Invalid resource ID format');
        expect(thrown.message).not.toContain(accessToken);
        expect(JSON.stringify([
          logger.debug.mock.calls,
          logger.info.mock.calls,
          logger.warn.mock.calls,
          logger.error.mock.calls,
          logger.audit.mock.calls,
        ])).not.toContain(accessToken);
      });

      it('sends session_duration when selfDestructSeconds provided', async () => {
        const getBody = captureMintBody();
        await connector.mintLinks('r_xyz', { expiresAt: '2099-01-01T00:00:00Z', n: 1, selfDestructSeconds: 30 });
        expect(getBody().session_duration).toBe('30s');
      });

      it('clamps 0.5 (fileviewer preset) to "1s" — qurl-service MinSessionDuration floor', async () => {
        const getBody = captureMintBody();
        await connector.mintLinks('r_xyz', { expiresAt: '2099-01-01T00:00:00Z', n: 1, selfDestructSeconds: 0.5 });
        expect(getBody().session_duration).toBe('1s');
      });

      it('ceils fractional values >1 (defensive — presets are all integer ≥1)', async () => {
        const getBody = captureMintBody();
        await connector.mintLinks('r_xyz', { expiresAt: '2099-01-01T00:00:00Z', n: 1, selfDestructSeconds: 2.3 });
        expect(getBody().session_duration).toBe('3s');
      });

      it('omits session_duration when null', async () => {
        const getBody = captureMintBody();
        await connector.mintLinks('r_xyz', { expiresAt: '2099-01-01T00:00:00Z', n: 1, selfDestructSeconds: null });
        expect(getBody().session_duration).toBeUndefined();
      });

      it('omits session_duration when omitted (default param)', async () => {
        const getBody = captureMintBody();
        await connector.mintLinks('r_xyz', { expiresAt: '2099-01-01T00:00:00Z', n: 1 });
        expect(getBody().session_duration).toBeUndefined();
      });

      it('omits session_duration for non-finite / wrong-type / non-positive inputs', async () => {
        const cases = [NaN, Infinity, -Infinity, '30', '0.5', true, false, {}, [], 0, -1, -0.5];
        for (const v of cases) {
          const getBody = captureMintBody();
          // eslint-disable-next-line no-await-in-loop
          await connector.mintLinks('r_xyz', { expiresAt: '2099-01-01T00:00:00Z', n: 1, selfDestructSeconds: v });
          expect(getBody().session_duration).toBeUndefined();
        }
      });

      it('sends guild_id when guildId provided', async () => {
        const getBody = captureMintBody();
        await connector.mintLinks('r_xyz', { expiresAt: '2099-01-01T00:00:00Z', n: 1, guildId: 'guild-123' });
        expect(getBody().guild_id).toBe('guild-123');
      });

      it('omits guild_id when guildId is absent (default param)', async () => {
        const getBody = captureMintBody();
        await connector.mintLinks('r_xyz', { expiresAt: '2099-01-01T00:00:00Z', n: 1 });
        expect('guild_id' in getBody()).toBe(false);
      });

      it('omits guild_id for falsy guildId (empty string / null / undefined)', async () => {
        for (const v of ['', null, undefined]) {
          const getBody = captureMintBody();
          // eslint-disable-next-line no-await-in-loop
          await connector.mintLinks('r_xyz', { expiresAt: '2099-01-01T00:00:00Z', n: 1, guildId: v });
          expect('guild_id' in getBody()).toBe(false);
        }
      });
    });

    it('omits viewer_ttl_seconds for non-positive / non-finite / wrong-type input', async () => {
      const cases = [0, -1, NaN, Infinity, '30', null, undefined, {}];
      for (const v of cases) {
        globalThis.fetch = jest.fn().mockResolvedValueOnce({
          ok: true,
          json: async () => ({ success: true, hash: 'h', resource_id: 'r' }),
        });
        const originalAppend = globalThis.FormData.prototype.append;
        const appended = [];
        globalThis.FormData.prototype.append = function (...args) { appended.push({ name: args[0] }); return originalAppend.apply(this, args); };
        try {
          await connector.reUploadBuffer(Buffer.from('hi'), 'x.txt', 'text/plain', undefined, v);
        } finally { globalThis.FormData.prototype.append = originalAppend; }
        expect(appended.find(f => f.name === 'viewer_ttl_seconds')).toBeUndefined();
      }
    });
  });

  describe('throwConnectorError — quota_exceeded tagging', () => {
    it('tags quota_exceeded when error string contains "quota exceeded"', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: false,
        status: 502,
        text: async () => JSON.stringify({
          success: false,
          error: 'QURL API error (403): quota exceeded: token limit per QURL reached (12/10)',
          links: [],
        }),
      });

      try {
        await connector.mintLinks('res-1', { expiresAt: '2026-01-01T00:00:00Z', n: 1 });
        throw new Error('expected throw');
      } catch (e) {
        expect(e.message).toMatch(/Connector mint_link failed \(502\)/);
        expect(e.status).toBe(502);
        expect(e.apiCode).toBe('quota_exceeded');
        expect(e.apiDetail).toMatch(/token limit per QURL reached/);
      }
    });

    it('tags quota_exceeded for the "token limit per QURL" pattern', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: false,
        status: 502,
        text: async () => JSON.stringify({
          success: false,
          error: 'token limit per QURL reached (11/10)',
        }),
      });

      try {
        await connector.mintLinks('res-1', { expiresAt: '2026-01-01T00:00:00Z', n: 1 });
        throw new Error('expected throw');
      } catch (e) {
        expect(e.apiCode).toBe('quota_exceeded');
      }
    });

    it('leaves apiCode null for unknown errors (so callers fall through to generic)', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: false,
        status: 500,
        text: async () => JSON.stringify({
          success: false,
          error: 'Internal server error',
        }),
      });

      try {
        await connector.mintLinks('res-1', { expiresAt: '2026-01-01T00:00:00Z', n: 1 });
        throw new Error('expected throw');
      } catch (e) {
        expect(e.status).toBe(500);
        expect(e.apiCode).toBeNull();
      }
    });

    it('handles non-JSON error body without crashing', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: false,
        status: 503,
        text: async () => '<html>503 Service Unavailable</html>',
      });

      try {
        await connector.mintLinks('res-1', { expiresAt: '2026-01-01T00:00:00Z', n: 1 });
        throw new Error('expected throw');
      } catch (e) {
        expect(e.status).toBe(503);
        expect(e.apiCode).toBeNull();
      }
    });

    it('handles missing/unreadable body without crashing', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: false,
        status: 504,
        text: async () => { throw new Error('network read failed'); },
      });

      try {
        await connector.mintLinks('res-1', { expiresAt: '2026-01-01T00:00:00Z', n: 1 });
        throw new Error('expected throw');
      } catch (e) {
        expect(e.status).toBe(504);
        expect(e.apiCode).toBeNull();
      }
    });

    it('surfaces partial mint qurl_ids from non-2xx bodies without logging qurl_link tokens', async () => {
      const logger = require('../src/logger');
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: false,
        status: 502,
        text: async () => JSON.stringify({
          success: false,
          error: 'render failed after mint',
          links: [
            { qurl_id: 'q_partial_one', qurl_link: 'https://qurl.link/#at_secret_one' },
            { qurl_id: 'q_partial_two', qurl_link: 'https://qurl.link/#at_secret_two' },
          ],
        }),
      });

      try {
        await connector.mintLinks('res-1', { expiresAt: '2026-01-01T00:00:00Z', n: 2 });
        throw new Error('expected throw');
      } catch (e) {
        expect(e.status).toBe(502);
        expect(e.partialLinkCount).toBe(2);
        expect(e.partialQurlIds).toEqual(['q_partial_one', 'q_partial_two']);
      }

      expect(logger.warn).toHaveBeenCalledWith(
        'Connector mint_link returned partial links on non-2xx',
        expect.objectContaining({
          resource_id: 'res-1',
          status: 502,
          bodyLen: expect.any(Number),
          partial_link_count: 2,
          partial_qurl_ids: ['q_partial_one', 'q_partial_two'],
        }),
      );
      const serializedLogs = JSON.stringify([
        logger.warn.mock.calls,
        logger.debug.mock.calls,
      ]);
      expect(serializedLogs).not.toContain('at_secret');
      expect(serializedLogs).not.toContain('qurl.link');
    });

    it('ignores malformed partial mint links and uses the generic debug path', async () => {
      const logger = require('../src/logger');
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: false,
        status: 502,
        text: async () => JSON.stringify({
          success: false,
          error: 'render failed before usable qurl ids',
          links: [
            {},
            { qurl_id: '' },
            'not-an-object',
          ],
        }),
      });

      try {
        await connector.mintLinks('res-1', { expiresAt: '2026-01-01T00:00:00Z', n: 2 });
        throw new Error('expected throw');
      } catch (e) {
        expect(e.status).toBe(502);
        expect(e.partialLinkCount).toBeUndefined();
        expect(e.partialQurlIds).toBeUndefined();
      }

      expect(logger.warn).not.toHaveBeenCalledWith(
        'Connector mint_link returned partial links on non-2xx',
        expect.anything(),
      );
      expect(logger.debug).toHaveBeenCalledWith(
        'Connector mint_link error',
        expect.objectContaining({
          status: 502,
          bodyLen: expect.any(Number),
        }),
      );
    });
  });

  describe('mintLinks — null/missing links guard (line 96)', () => {
    it('throws when result.links is null', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        json: async () => ({ success: true, links: null }),
      });

      await expect(connector.mintLinks('res-1', { expiresAt: '2026-01-01T00:00:00Z', n: 1 }))
        .rejects.toThrow('Connector mint_link returned no links array');
    });

    it('throws when result.links is not an array (string)', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        json: async () => ({ success: true, links: 'not-array' }),
      });

      await expect(connector.mintLinks('res-1', { expiresAt: '2026-01-01T00:00:00Z', n: 1 }))
        .rejects.toThrow('Connector mint_link returned no links array');
    });

    it('throws when result.links is undefined', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        json: async () => ({ success: true }),
      });

      await expect(connector.mintLinks('res-1', { expiresAt: '2026-01-01T00:00:00Z', n: 1 }))
        .rejects.toThrow('Connector mint_link returned no links array');
    });
  });
});

describe('Connector client — no API key (requireApiKey guard)', () => {
  let connector;

  beforeEach(() => {
    jest.resetModules();
    jest.mock('../src/config', () => ({
      CONNECTOR_URL: 'https://connector.test.local',
      QURL_API_KEY: '', // empty — should throw
    }));
    jest.mock('../src/logger', () => ({
      info: jest.fn(),
      warn: jest.fn(),
      error: jest.fn(),
      debug: jest.fn(),
      audit: jest.fn(),
    }));
    connector = require('../src/connector');
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  it('throws when QURL_API_KEY is empty on uploadToConnector', async () => {
    await expect(connector.uploadToConnector(
      'https://cdn.discordapp.com/file.pdf', 'file.pdf', 'application/pdf',
    )).rejects.toThrow('QURL_API_KEY is not configured');
  });

  it('throws when QURL_API_KEY is empty on mintLinks', async () => {
    await expect(connector.mintLinks('res-1', { expiresAt: '2026-01-01T00:00:00Z', n: 1 }))
      .rejects.toThrow('QURL_API_KEY is not configured');
  });
});

describe('Connector client — MD5 hash truncation in upload logs', () => {
  let connector;
  let logger;

  const FULL_MD5 = '5d41402abc4b2a76b9719d911017c592';
  const MD5_PREFIX = '5d41402a';

  beforeEach(() => {
    jest.resetModules();
    resetDetectSdkMocks();
    jest.mock('../src/config', () => ({
      CONNECTOR_URL: 'https://connector.test.local',
      QURL_ENDPOINT: 'https://api.test.local',
      QURL_API_KEY: 'test-key',
      DETECT_TUNNEL_SLUG: 'detect-sandbox',
    }));
    jest.mock('../src/logger', () => ({
      info: jest.fn(),
      warn: jest.fn(),
      error: jest.fn(),
      debug: jest.fn(),
      audit: jest.fn(),
    }));
    connector = require('../src/connector');
    logger = require('../src/logger');
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  function assertNoFullHashLeaked() {
    for (const call of logger.info.mock.calls) {
      const meta = call[1] ?? {};
      expect(JSON.stringify(meta)).not.toContain(FULL_MD5);
      expect(meta).not.toHaveProperty('hash');
    }
  }

  it('uploadToConnector logs md5_prefix (8 chars), never the full hash', async () => {
    globalThis.fetch = jest.fn()
      .mockResolvedValueOnce({
        ok: true,
        headers: { get: jest.fn(() => '10') },
        arrayBuffer: async () => new ArrayBuffer(10),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ success: true, hash: FULL_MD5, resource_id: 'r1' }),
      });

    await connector.uploadToConnector(
      'https://cdn.discordapp.com/file.png', 'file.png', 'image/png',
    );

    expect(logger.info).toHaveBeenCalledWith('Uploaded to connector', {
      md5_prefix: MD5_PREFIX,
      resource_id: 'r1',
    });
    assertNoFullHashLeaked();
  });

  it('reUploadBuffer logs md5_prefix (8 chars), never the full hash', async () => {
    globalThis.fetch = jest.fn().mockResolvedValueOnce({
      ok: true,
      json: async () => ({ success: true, hash: FULL_MD5, resource_id: 'r2' }),
    });

    await connector.reUploadBuffer(Buffer.from('payload'), 'file.png', 'image/png');

    expect(logger.info).toHaveBeenCalledWith('Re-uploaded to connector (new resource)', {
      md5_prefix: MD5_PREFIX,
      resource_id: 'r2',
    });
    assertNoFullHashLeaked();
  });

  it('uploadJsonToConnector logs md5_prefix (8 chars), never the full hash', async () => {
    globalThis.fetch = jest.fn().mockResolvedValueOnce({
      ok: true,
      json: async () => ({ success: true, hash: FULL_MD5, resource_id: 'r3' }),
    });

    await connector.uploadJsonToConnector(
      { type: 'google-map', url: 'https://maps.app.goo.gl/x' },
      'location.json',
    );

    expect(logger.info).toHaveBeenCalledWith('Uploaded JSON to connector', {
      md5_prefix: MD5_PREFIX,
      resource_id: 'r3',
    });
    assertNoFullHashLeaked();
  });

  it('md5_prefix is undefined (not crash) when connector returns no hash', async () => {
    globalThis.fetch = jest.fn()
      .mockResolvedValueOnce({
        ok: true,
        headers: { get: jest.fn(() => '10') },
        arrayBuffer: async () => new ArrayBuffer(10),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ success: true, resource_id: 'r4' }),
      });

    await connector.uploadToConnector(
      'https://cdn.discordapp.com/file.png', 'file.png', 'image/png',
    );

    expect(logger.info).toHaveBeenCalledWith('Uploaded to connector', {
      md5_prefix: undefined,
      resource_id: 'r4',
    });
    assertNoFullHashLeaked();
  });

  it.each([
    ['null', null],
    ['number', 12345],
    ['object', { md5: 'embedded' }],
  ])('md5_prefix is undefined when connector returns hash as %s', async (_label, hashValue) => {
    globalThis.fetch = jest.fn()
      .mockResolvedValueOnce({
        ok: true,
        headers: { get: jest.fn(() => '10') },
        arrayBuffer: async () => new ArrayBuffer(10),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ success: true, hash: hashValue, resource_id: 'r5' }),
      });

    await connector.uploadToConnector(
      'https://cdn.discordapp.com/file.png', 'file.png', 'image/png',
    );

    expect(logger.info).toHaveBeenCalledWith('Uploaded to connector', {
      md5_prefix: undefined,
      resource_id: 'r5',
    });
    assertNoFullHashLeaked();
  });

  describe('detectWatermark — self-mint-then-POST tunnel contract', () => {
    const TUNNEL_HOST = 'r_abc12345678.qurl.site';
    const TUNNEL_SITE = `https://${TUNNEL_HOST}`;
    const OTHER_TUNNEL_SITE = 'https://r_other123456.qurl.site';
    const SANDBOX_TUNNEL_SITE = 'https://r_abc12345678.qurl.site.layerv.xyz';
    const STAGING_TUNNEL_SITE = 'https://r_abc12345678.qurl.site.layerv.ai';
    const TUNNEL_TARGET = `${TUNNEL_SITE}/api/detect`;
    const RESOURCE_ID = 'MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE2ifzReg5Fb3RadAQRn_oYpEYDKDXp0InOyQpO8Wo392Hmm92wvsORreNjzdi18er8WjAQzqP3KUgkYJxjO0ZpQ';
    const OTHER_RESOURCE_ID = 'MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEUdo9dFoITY7YjKpcsqAqirgvnBqmd4UqOI1rJoZr2vZfm5gY1gj-6ixqU6A4mUoic1tVyopTsrLI1RhI7V57CA';
    const REVOKED_RESOURCE_ID = 'MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEF7Eb1T4TJIa4uUdFdxiMl1ARZRlmvpts6WkeCY5RUN-DnrmyXbOVkKgsjRFxqhteXp-ybrf6j07zeJRjb7HFvQ';
    const MINT_LINK = 'https://qurl.link.layerv.xyz/#at_testtoken123';

    function captureDetect(jsonResponse, {
      ok = true,
      status = 200,
      qurlSite = TUNNEL_SITE,
      resolveResult = { target_url: '', resource_id: RESOURCE_ID },
      resources = [{ resource_id: RESOURCE_ID, status: 'active' }],
    } = {}) {
      mockListAllResources(resources);
      mockClient.createQurlForResource.mockResolvedValue({
        qurl_id: 'q_x',
        qurl_link: MINT_LINK,
        qurl_site: qurlSite,
      });
      mockClient.resolve.mockResolvedValue(resolveResult);
      let captured = null;
      globalThis.fetch = jest.fn(async (url, opts) => {
        captured = { url, opts };
        return {
          ok,
          status,
          json: async () => jsonResponse,
          text: async () => JSON.stringify(jsonResponse),
        };
      });
      return () => captured;
    }

    function freezeDetectClock(initialNow = 1_000_000) {
      let now = initialNow;
      const spy = jest.spyOn(Date, 'now').mockImplementation(() => now);
      return {
        advanceBy(ms) {
          now += ms;
        },
        restore() {
          spy.mockRestore();
        },
      };
    }

    const CREATE_QURL_FOR_RESOURCE_ALLOWED_KEYS = [
      'expires_in',
      'one_time_use',
      'max_sessions',
      'session_duration',
      'access_policy',
      'label',
    ];

    it('mints the detect qURL with no field outside CreateQurlForResourceRequest', async () => {
      captureDetect({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
      await connector.detectWatermark(Buffer.from('x'), { guildId: 'guild-9', apiKey: 'k-detect' });

      expect(mockClient.createQurlForResource).toHaveBeenCalledTimes(1);
      const [, body] = mockClient.createQurlForResource.mock.calls[0];
      const undeclared = Object.keys(body ?? {}).filter(
        (k) => !CREATE_QURL_FOR_RESOURCE_ALLOWED_KEYS.includes(k),
      );
      expect(undeclared).toEqual([]);
    });

    it('still POSTs to /api/detect once target_path is off the mint body', async () => {
      const get = captureDetect({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
      await connector.detectWatermark(Buffer.from('x'), { guildId: 'guild-9', apiKey: 'k-detect' });
      expect(get().url).toBe(`${TUNNEL_SITE}/api/detect`);
    });

    it('self-mints then POSTs to qurl_site with X-Guild-Id, Authorization, Content-Type and raw bytes', async () => {
      const get = captureDetect({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
      const bytes = Buffer.from('imagedata');
      await connector.detectWatermark(bytes, { guildId: 'guild-9', contentType: 'image/png', apiKey: 'k-detect' });
      expect(mockClient.listAllResources).toHaveBeenCalledWith({ slug: 'detect-sandbox', limit: 100 });
      expect(mockClient.createQurlForResource).toHaveBeenCalledWith(RESOURCE_ID, { expires_in: '5m' });
      expect(mockClient.resolve).toHaveBeenCalledWith({ access_token: 'at_testtoken123' });
      const { url, opts } = get();
      expect(url).toBe(TUNNEL_TARGET);
      expect(opts.method).toBe('POST');
      expect(opts.headers['X-Guild-Id']).toBe('guild-9');
      expect(opts.headers['Authorization']).toBe('Bearer k-detect');
      expect(opts.headers['Content-Type']).toBe('image/png');
      expect(opts.body).toBe(bytes);
    });

    it('accepts the sandbox qurl_site suffix and ignores an empty resolve target_url', async () => {
      const get = captureDetect(
        { detected: false, qurl_id: null, match_pct: null, confidence: 0 },
        { qurlSite: SANDBOX_TUNNEL_SITE, resolveResult: { target_url: '', resource_id: RESOURCE_ID } },
      );
      await connector.detectWatermark(Buffer.from('x'), { guildId: 'guild-9', apiKey: 'k-detect' });
      expect(get().url).toBe(`${SANDBOX_TUNNEL_SITE}/api/detect`);
    });

    it('accepts multiple routing labels under the sandbox qurl_site suffix', async () => {
      const site = 'https://edge.r_abc12345678.qurl.site.layerv.xyz';
      const get = captureDetect(
        { detected: false, qurl_id: null, match_pct: null, confidence: 0 },
        { qurlSite: site, resolveResult: { target_url: '', resource_id: RESOURCE_ID } },
      );

      await connector.detectWatermark(Buffer.from('x'), { guildId: 'guild-9', apiKey: 'k-detect' });

      expect(get().url).toBe(`${site}/api/detect`);
    });

    it('accepts the staging qurl_site suffix', async () => {
      const get = captureDetect(
        { detected: false, qurl_id: null, match_pct: null, confidence: 0 },
        { qurlSite: STAGING_TUNNEL_SITE },
      );
      await connector.detectWatermark(Buffer.from('x'), { guildId: 'guild-9', apiKey: 'k-detect' });
      expect(get().url).toBe(`${STAGING_TUNNEL_SITE}/api/detect`);
    });

    it('accepts the staging qurl_site suffix when QURL_ENDPOINT is the explicit staging API host', async () => {
      jest.resetModules();
      resetDetectSdkMocks();
      jest.doMock('../src/config', () => ({
        CONNECTOR_URL: 'https://connector.test.local',
        QURL_ENDPOINT: 'https://api.staging.layerv.ai',
        QURL_API_KEY: 'test-key',
        DETECT_TUNNEL_SLUG: 'detect-sandbox',
      }));
      const connectorStaging = require('../src/connector');
      const get = captureDetect(
        { detected: false, qurl_id: null, match_pct: null, confidence: 0 },
        { qurlSite: STAGING_TUNNEL_SITE },
      );

      await connectorStaging.detectWatermark(Buffer.from('x'), { guildId: 'guild-9', apiKey: 'k-detect' });
      expect(get().url).toBe(`${STAGING_TUNNEL_SITE}/api/detect`);
    });

    it.each([
      ['production', 'sandbox', 'https://api.layerv.ai', SANDBOX_TUNNEL_SITE],
      ['production', 'staging', 'https://api.layerv.ai', STAGING_TUNNEL_SITE],
      ['unknown', 'sandbox', 'https://api.future.layerv.ai', SANDBOX_TUNNEL_SITE],
      ['unknown', 'staging', 'https://api.future.layerv.ai', STAGING_TUNNEL_SITE],
      ['unlisted .local', 'sandbox', 'https://custom.local', SANDBOX_TUNNEL_SITE],
    ])('rejects the %s qURL endpoint with %s qurl_site suffix', async (_envLabel, _suffixLabel, endpoint, qurlSite) => {
      jest.resetModules();
      resetDetectSdkMocks();
      jest.doMock('../src/config', () => ({
        CONNECTOR_URL: 'https://connector.test.local',
        QURL_ENDPOINT: endpoint,
        QURL_API_KEY: 'test-key',
        DETECT_TUNNEL_SLUG: 'detect-sandbox',
      }));
      const connectorProd = require('../src/connector');
      const get = captureDetect(
        { detected: false, qurl_id: null, match_pct: null, confidence: 0 },
        { qurlSite },
      );

      await expect(
        connectorProd.detectWatermark(Buffer.from('x'), { guildId: 'guild-9', apiKey: 'k-detect' }),
      ).rejects.toThrow(/expected qURL tunnel domain/);
      expect(mockClient.resolve).not.toHaveBeenCalled();
      expect(get()).toBeNull();
    });

    it('accepts an r_-labelled qurl_site for an opaque public-key resource_id', async () => {
      const get = captureDetect(
        { detected: false, qurl_id: null, match_pct: null, confidence: 0 },
      );
      await connector.detectWatermark(Buffer.from('x'), { guildId: 'guild-9', apiKey: 'k-detect' });
      expect(get().url).toBe(TUNNEL_TARGET);
      expect(mockClient.createQurlForResource).toHaveBeenCalledWith(RESOURCE_ID, expect.any(Object));
    });

    it('host-pin compares complete target and returned qurl_site hosts after URL case normalization', () => {
      expect(() => connector.__testExports.assertPublicHttpsTarget(
        'https://R_ABC12345678.QURL.SITE/api/detect',
        TUNNEL_HOST,
      )).not.toThrow();
    });

    it('host-pin fails closed when the detect target hostname differs from the returned qurl_site', () => {
      expect(() => connector.__testExports.assertPublicHttpsTarget(
        'https://r_other123456.qurl.site/api/detect',
        TUNNEL_HOST,
      )).toThrow(/does not match the returned qurl_site/);
    });

    it.each([
      ['a plain non-r_ label', 'https://tenant-abc.qurl.site'],
      ['multiple non-empty routing labels', 'https://edge.r_abc12345678.qurl.site'],
    ])('host-pin accepts %s under an allowed suffix', (_label, qurlSite) => {
      expect(() => connector.__testExports.assertPublicHttpsTarget(
        `${qurlSite}/api/detect`,
        new URL(qurlSite).hostname,
      )).not.toThrow();
    });

    it('host-pin rejects a trailing-dot FQDN outside the exact suffix boundary', () => {
      const trailingDotSite = 'https://r_abc12345678.qurl.site.';
      expect(() => connector.__testExports.assertPublicHttpsTarget(
        `${trailingDotSite}/api/detect`,
        new URL(trailingDotSite).hostname,
      )).toThrow(/expected qURL tunnel domain/);
    });

    it.each([
      ['empty', 'https://.qurl.site'],
      ['empty interior', 'https://a..qurl.site'],
    ])('host-pin rejects an %s label under an allowed suffix', (_label, qurlSite) => {
      expect(() => connector.__testExports.assertPublicHttpsTarget(
        `${qurlSite}/api/detect`,
        new URL(qurlSite).hostname,
      )).toThrow(/expected qURL tunnel domain/);
    });

    it('host-pin rejects the bare allowed-suffix apex', () => {
      const suffixApex = 'https://qurl.site';
      expect(() => connector.__testExports.assertPublicHttpsTarget(
        `${suffixApex}/api/detect`,
        new URL(suffixApex).hostname,
      )).toThrow(/expected qURL tunnel domain/);
    });

    it('ignores a non-empty resolve target_url and still POSTs to qurl_site', async () => {
      const get = captureDetect(
        { detected: false, qurl_id: null, match_pct: null, confidence: 0 },
        { resolveResult: { target_url: 'https://evil.example.com/api/detect', resource_id: RESOURCE_ID } },
      );
      await connector.detectWatermark(Buffer.from('x'), { guildId: 'guild-9', apiKey: 'k-detect' });
      expect(get().url).toBe(TUNNEL_TARGET);
    });

    it('filters active resources client-side because status cannot be combined with slug server-side', async () => {
      captureDetect(
        { detected: false, qurl_id: null, match_pct: null, confidence: 0 },
        {
          resources: [
            { resource_id: REVOKED_RESOURCE_ID, slug: 'detect-sandbox', status: 'revoked' },
            { resource_id: RESOURCE_ID, slug: 'detect-sandbox', status: 'active' },
          ],
          resolveResult: { target_url: '', resource_id: RESOURCE_ID },
        },
      );
      await connector.detectWatermark(Buffer.from('x'), { guildId: 'guild-9', apiKey: 'k-detect' });
      expect(mockClient.listAllResources).toHaveBeenCalledWith({ slug: 'detect-sandbox', limit: 100 });
      expect(mockClient.createQurlForResource).toHaveBeenCalledWith(RESOURCE_ID, {
        expires_in: '5m',
      });
    });

    it('finds the active detect resource after many revoked rows via the SDK auto-paginator', async () => {
      const revokedRows = Array.from({ length: 150 }, (_, i) => ({
        resource_id: `${REVOKED_RESOURCE_ID.slice(0, -4)}${String(i).padStart(4, '0')}`,
        slug: 'detect-sandbox',
        status: 'revoked',
      }));
      captureDetect(
        { detected: false, qurl_id: null, match_pct: null, confidence: 0 },
        {
          resources: [
            ...revokedRows,
            { resource_id: RESOURCE_ID, slug: 'detect-sandbox', status: 'active' },
          ],
        },
      );

      await connector.detectWatermark(Buffer.from('x'), { guildId: 'guild-9', apiKey: 'k-detect' });

      expect(mockClient.listAllResources).toHaveBeenCalledWith({ slug: 'detect-sandbox', limit: 100 });
      expect(mockClient.createQurlForResource).toHaveBeenCalledWith(RESOURCE_ID, expect.any(Object));
    });

    it('throws when the slug resolves to multiple active resources', async () => {
      const get = captureDetect(
        { detected: false, qurl_id: null, match_pct: null, confidence: 0 },
        {
          resources: [
            { resource_id: RESOURCE_ID, slug: 'detect-sandbox', status: 'active' },
            { resource_id: OTHER_RESOURCE_ID, slug: 'detect-sandbox', status: 'active' },
          ],
        },
      );
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'guild-9', apiKey: 'k-detect' }),
      ).rejects.toThrow(/multiple active resources/);
      expect(mockClient.createQurlForResource).not.toHaveBeenCalled();
      expect(mockClient.resolve).not.toHaveBeenCalled();
      expect(get()).toBeNull();
      expect(logger.warn).toHaveBeenCalledWith(
        'Detect tunnel slug resolved to multiple active resources',
        expect.objectContaining({ slug: 'detect-sandbox', count: 2 }),
      );
    });

    it('backs off a multiple-active slug rejection, then re-resolves after the retry window', async () => {
      const clock = freezeDetectClock();
      try {
        const get = captureDetect({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
        mockListAllResourcesOnce([
          { resource_id: RESOURCE_ID, slug: 'detect-sandbox', status: 'active' },
          { resource_id: OTHER_RESOURCE_ID, slug: 'detect-sandbox', status: 'active' },
        ]);

        await expect(
          connector.detectWatermark(Buffer.from('x'), { guildId: 'guild-9', apiKey: 'k-detect' }),
        ).rejects.toThrow(/multiple active resources/);
        await expect(
          connector.detectWatermark(Buffer.from('y'), { guildId: 'guild-9', apiKey: 'k-detect' }),
        ).rejects.toThrow(/backing off/);
        expect(mockClient.listAllResources).toHaveBeenCalledTimes(1);
        expect(mockClient.createQurlForResource).not.toHaveBeenCalled();

        clock.advanceBy(30_001);
        await connector.detectWatermark(Buffer.from('z'), { guildId: 'guild-9', apiKey: 'k-detect' });

        expect(mockClient.listAllResources).toHaveBeenCalledTimes(2);
        expect(mockClient.createQurlForResource).toHaveBeenCalledTimes(1);
        expect(get().url).toBe(TUNNEL_TARGET);
      } finally {
        clock.restore();
      }
    });

    it('falls back to octet-stream content-type and global QURL_API_KEY for the POST Bearer', async () => {
      const get = captureDetect({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
      await connector.detectWatermark(Buffer.from('x'), { guildId: 'guild-9' });
      const { opts } = get();
      expect(opts.headers['Content-Type']).toBe('application/octet-stream');
      expect(opts.headers['Authorization']).toBe('Bearer test-key');
    });

    it('caches the resource_id — a second detect skips the listAllResources lookup but re-mints + re-resolves', async () => {
      captureDetect({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
      await connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' });
      await connector.detectWatermark(Buffer.from('y'), { guildId: 'g', apiKey: 'k' });
      expect(mockClient.listAllResources).toHaveBeenCalledTimes(1);     // cached after the first call
      expect(mockClient.createQurlForResource).toHaveBeenCalledTimes(2); // re-minted per call
      expect(mockClient.resolve).toHaveBeenCalledTimes(2);              // re-knocked per call
    });

    it('backs off after repeated mint failures, then re-resolves the slug after the retry window', async () => {
      const clock = freezeDetectClock();
      try {
        captureDetect({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
        mockClient.createQurlForResource
          .mockResolvedValueOnce({ qurl_id: 'q1', qurl_link: MINT_LINK, qurl_site: TUNNEL_SITE }) // caches id
          .mockRejectedValueOnce(new Error('resource not found'))        // clears cache; no backoff yet
          .mockRejectedValueOnce(new Error('resource still missing'))    // repeated failure arms backoff
          .mockResolvedValueOnce({ qurl_id: 'q3', qurl_link: MINT_LINK, qurl_site: TUNNEL_SITE }); // after re-resolve

        await connector.detectWatermark(Buffer.from('a'), { guildId: 'g', apiKey: 'k' });
        await expect(
          connector.detectWatermark(Buffer.from('b'), { guildId: 'g', apiKey: 'k' }),
        ).rejects.toThrow('resource not found');
        await expect(
          connector.detectWatermark(Buffer.from('c'), { guildId: 'g', apiKey: 'k' }),
        ).rejects.toThrow('resource still missing');
        await expect(
          connector.detectWatermark(Buffer.from('d'), { guildId: 'g', apiKey: 'k' }),
        ).rejects.toThrow(/backing off/);

        expect(mockClient.listAllResources).toHaveBeenCalledTimes(2);

        clock.advanceBy(30_001);
        await connector.detectWatermark(Buffer.from('e'), { guildId: 'g', apiKey: 'k' });

        expect(mockClient.listAllResources).toHaveBeenCalledTimes(3);
      } finally {
        clock.restore();
      }
    });

    it('does not treat two stale mint failures beyond the retry window as consecutive', async () => {
      const clock = freezeDetectClock();
      try {
        captureDetect({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
        mockClient.createQurlForResource
          .mockResolvedValueOnce({ qurl_id: 'q1', qurl_link: MINT_LINK, qurl_site: TUNNEL_SITE })
          .mockRejectedValueOnce(new Error('first stale miss'))
          .mockRejectedValueOnce(new Error('second stale miss'))
          .mockResolvedValueOnce({ qurl_id: 'q4', qurl_link: MINT_LINK, qurl_site: TUNNEL_SITE });

        await connector.detectWatermark(Buffer.from('a'), { guildId: 'g', apiKey: 'k' });
        await expect(
          connector.detectWatermark(Buffer.from('b'), { guildId: 'g', apiKey: 'k' }),
        ).rejects.toThrow('first stale miss');

        clock.advanceBy(30_001);
        await expect(
          connector.detectWatermark(Buffer.from('c'), { guildId: 'g', apiKey: 'k' }),
        ).rejects.toThrow('second stale miss');

        await connector.detectWatermark(Buffer.from('d'), { guildId: 'g', apiKey: 'k' });

        expect(mockClient.listAllResources).toHaveBeenCalledTimes(3);
        expect(mockClient.createQurlForResource).toHaveBeenCalledTimes(4);
      } finally {
        clock.restore();
      }
    });

    it('returns the normalized detect result on a detected match', async () => {
      captureDetect({ detected: true, qurl_id: 'q_match1', match_pct: 92, confidence: 0.98 });
      const res = await connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' });
      expect(res).toEqual({ detected: true, qurl_id: 'q_match1', match_pct: 92, confidence: 0.98 });
    });

    it('coerces a garbled/absent detected field to a hard boolean false', async () => {
      captureDetect({ qurl_id: 'q_x', match_pct: 50 });
      const res = await connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' });
      expect(res.detected).toBe(false);
      expect(res.confidence).toBe(0);
    });

    it('throws (with .status) on a non-ok response so the handler can ephemeral-error', async () => {
      captureDetect({ error: 'bad guild' }, { ok: false, status: 400 });
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toMatchObject({ status: 400 });
    });

    it('throws when no guildId is given (attribution is guild-scoped) BEFORE minting', async () => {
      const get = captureDetect({ detected: false });
      await expect(
        connector.detectWatermark(Buffer.from('x'), { apiKey: 'k' }),
      ).rejects.toThrow(/guild-scoped/);
      expect(mockClient.listAllResources).not.toHaveBeenCalled();
      expect(mockClient.createQurlForResource).not.toHaveBeenCalled();
      expect(mockClient.resolve).not.toHaveBeenCalled();
      expect(get()).toBeNull();
    });

    it('throws a clear configured-error when DETECT_TUNNEL_SLUG is unset, no mint, no POST', async () => {
      jest.resetModules();
      resetDetectSdkMocks();
      jest.doMock('../src/config', () => ({
        CONNECTOR_URL: 'https://connector.test.local',
        QURL_ENDPOINT: 'https://api.test.local',
        QURL_API_KEY: 'test-key',
      }));
      const connectorNoSlug = require('../src/connector');
      const fetchSpy = jest.fn();
      globalThis.fetch = fetchSpy;
      await expect(
        connectorNoSlug.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/DETECT_TUNNEL_SLUG is not configured/);
      expect(mockClient.listAllResources).not.toHaveBeenCalled();
      expect(mockClient.createQurlForResource).not.toHaveBeenCalled();
      expect(mockClient.resolve).not.toHaveBeenCalled();
      expect(fetchSpy).not.toHaveBeenCalled();
    });

    it('throws "resource not found" when the slug resolves to no active resource, and does NOT mint or POST', async () => {
      const get = captureDetect({ detected: false }, { resources: [] });
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/resource not found for slug/);
      expect(mockClient.listAllResources).toHaveBeenCalledTimes(1);
      expect(mockClient.listAllResources).toHaveBeenCalledWith({ slug: 'detect-sandbox', limit: 100 });
      expect(mockClient.createQurlForResource).not.toHaveBeenCalled();
      expect(mockClient.resolve).not.toHaveBeenCalled();
      expect(get()).toBeNull();
    });

    it('throws when the live resource shape has id but no resource_id', async () => {
      const get = captureDetect(
        { detected: false },
        { resources: [{ id: 'wrong-id', slug: 'detect-sandbox', status: 'active' }] },
      );
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/resource not found for slug/);
      expect(mockClient.createQurlForResource).not.toHaveBeenCalled();
      expect(mockClient.resolve).not.toHaveBeenCalled();
      expect(get()).toBeNull();
    });

    it('breadcrumbs a slug-lookup transport failure, allows one retry, and does NOT mint or POST on the failure', async () => {
      mockClient.listAllResources.mockImplementationOnce(() => {
        throw new Error('econnreset');
      });
      const fetchSpy = jest.fn();
      globalThis.fetch = fetchSpy;
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/econnreset/);
      expect(mockClient.createQurlForResource).not.toHaveBeenCalled();
      expect(fetchSpy).not.toHaveBeenCalled();
      expect(logger.warn).toHaveBeenCalledWith(
        'Detect tunnel slug lookup failed',
        expect.objectContaining({ error: 'econnreset' }),
      );

      const get = captureDetect({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
      await connector.detectWatermark(Buffer.from('y'), { guildId: 'g', apiKey: 'k' });
      expect(mockClient.listAllResources).toHaveBeenCalledTimes(2);
      expect(mockClient.createQurlForResource).toHaveBeenCalledTimes(1);
      expect(get().url).toBe(TUNNEL_TARGET);
    });

    it('backs off after repeated slug-lookup transport failures, then allows a fresh lookup after expiry', async () => {
      const clock = freezeDetectClock();
      try {
        mockClient.listAllResources
          .mockImplementationOnce(() => {
            throw new Error('first econnreset');
          })
          .mockImplementationOnce(() => {
            throw new Error('second econnreset');
          });
        const fetchSpy = jest.fn();
        globalThis.fetch = fetchSpy;

        await expect(
          connector.detectWatermark(Buffer.from('a'), { guildId: 'g', apiKey: 'k' }),
        ).rejects.toThrow(/first econnreset/);
        await expect(
          connector.detectWatermark(Buffer.from('b'), { guildId: 'g', apiKey: 'k' }),
        ).rejects.toThrow(/second econnreset/);
        await expect(
          connector.detectWatermark(Buffer.from('c'), { guildId: 'g', apiKey: 'k' }),
        ).rejects.toThrow(/backing off/);

        expect(mockClient.listAllResources).toHaveBeenCalledTimes(2);
        expect(mockClient.createQurlForResource).not.toHaveBeenCalled();
        expect(fetchSpy).not.toHaveBeenCalled();

        clock.advanceBy(30_001);
        const get = captureDetect({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
        await connector.detectWatermark(Buffer.from('d'), { guildId: 'g', apiKey: 'k' });
        expect(mockClient.listAllResources).toHaveBeenCalledTimes(3);
        expect(get().url).toBe(TUNNEL_TARGET);
      } finally {
        clock.restore();
      }
    });

    it('throws "mint did not return an access token" when the mint qurl_link lacks an at_ fragment, and does NOT POST', async () => {
      mockListAllResources([{ resource_id: RESOURCE_ID, status: 'active' }]);
      mockClient.createQurlForResource.mockResolvedValue({
        qurl_id: 'q_x',
        qurl_link: 'https://qurl.link.layerv.xyz/no-fragment',
        qurl_site: TUNNEL_SITE,
      });
      const fetchSpy = jest.fn();
      globalThis.fetch = fetchSpy;
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/mint did not return an access token/);
      expect(fetchSpy).not.toHaveBeenCalled();
      expect(mockClient.resolve).not.toHaveBeenCalled();
      expect(logger.warn).toHaveBeenCalledWith(
        'Detect tunnel mint failed',
        expect.objectContaining({ error: expect.stringMatching(/access token/) }),
      );
    });

    it('throws "invalid qurl_link" when the mint returns an unparseable qurl_link, and does NOT knock or POST', async () => {
      mockListAllResources([{ resource_id: RESOURCE_ID, status: 'active' }]);
      mockClient.createQurlForResource.mockResolvedValue({
        qurl_id: 'q_x',
        qurl_link: 'https://[',
        qurl_site: TUNNEL_SITE,
      });
      const fetchSpy = jest.fn();
      globalThis.fetch = fetchSpy;
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/invalid qurl_link/);
      expect(mockClient.resolve).not.toHaveBeenCalled();
      expect(fetchSpy).not.toHaveBeenCalled();
      expect(logger.warn).toHaveBeenCalledWith(
        'Detect tunnel mint failed',
        expect.objectContaining({ error: expect.stringMatching(/invalid qurl_link/) }),
      );
    });

    it('extracts only the at_ token from the mint fragment, stripping trailing params', async () => {
      captureDetect({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
      mockClient.createQurlForResource.mockResolvedValue({
        qurl_id: 'q_x',
        qurl_link: 'https://qurl.link.layerv.xyz/abc#at_tok123&utm=x',
        qurl_site: TUNNEL_SITE,
      });
      await connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' });
      expect(mockClient.resolve).toHaveBeenCalledWith({ access_token: 'at_tok123' });
    });

    it.each([
      ['later bare segment', 'https://qurl.link.layerv.xyz/abc#foo=bar&at_mixed789'],
      ['bad named key plus later bare token', 'https://qurl.link.layerv.xyz/abc#access_token=nope&at_real789'],
    ])('rejects a mint fragment with a %s instead of scanning arbitrary segments', async (_key, qurlLink) => {
      captureDetect({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
      mockClient.createQurlForResource.mockResolvedValue({
        qurl_id: 'q_x',
        qurl_link: qurlLink,
        qurl_site: TUNNEL_SITE,
      });
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/access token/);
      expect(mockClient.resolve).not.toHaveBeenCalled();
    });

    it('keeps the cached resource_id when the mint qurl_link shape is invalid', async () => {
      captureDetect({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
      mockClient.createQurlForResource
        .mockResolvedValueOnce({ qurl_id: 'q1', qurl_link: MINT_LINK, qurl_site: TUNNEL_SITE })
        .mockResolvedValueOnce({ qurl_id: 'q2', qurl_link: 'https://qurl.link.layerv.xyz/no-fragment', qurl_site: TUNNEL_SITE })
        .mockResolvedValueOnce({ qurl_id: 'q3', qurl_link: MINT_LINK, qurl_site: TUNNEL_SITE });

      await connector.detectWatermark(Buffer.from('a'), { guildId: 'g', apiKey: 'k' });
      await expect(
        connector.detectWatermark(Buffer.from('b'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/access token/);
      await connector.detectWatermark(Buffer.from('c'), { guildId: 'g', apiKey: 'k' });

      expect(mockClient.listAllResources).toHaveBeenCalledTimes(1);
      expect(mockClient.createQurlForResource).toHaveBeenCalledTimes(3);
      expect(mockClient.resolve).toHaveBeenCalledTimes(2);
    });

    it('redacts any at_ token from the resolve-failure breadcrumb', async () => {
      const get = captureDetect({ detected: false });
      mockClient.resolve.mockRejectedValue(new Error('knock failed for at_secretXYZ789: timeout'));
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow();
      const warn = logger.warn.mock.calls.find(
        (c) => c[0] === 'Detect tunnel resolve failed (knock/transport)',
      );
      expect(warn).toBeTruthy();
      expect(warn[1].error).not.toMatch(/at_secretXYZ789/);
      expect(warn[1].error).toContain('at_[REDACTED]');
      expect(get()).toBeNull(); // resolve failed → no POST
    });

    it('redacts any at_ token from the mint-failure breadcrumb', async () => {
      const get = captureDetect({ detected: false });
      mockClient.createQurlForResource.mockRejectedValue(new Error('mint rejected: at_leakedABC123 invalid'));
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow();
      const warn = logger.warn.mock.calls.find((c) => c[0] === 'Detect tunnel mint failed');
      expect(warn).toBeTruthy();
      expect(warn[1].error).not.toMatch(/at_leakedABC123/);
      expect(warn[1].error).toContain('at_[REDACTED]');
      expect(mockClient.resolve).not.toHaveBeenCalled(); // mint failed → no resolve
      expect(get()).toBeNull(); // and no POST
    });

    it('SSRF guard: a private/loopback minted qurl_site throws and NO knock or POST happens', async () => {
      const get = captureDetect({ detected: false }, { qurlSite: 'https://127.0.0.1' });
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/private\/internal/);
      expect(mockClient.resolve).not.toHaveBeenCalled();
      expect(get()).toBeNull();
      expect(logger.warn).toHaveBeenCalledWith(
        'Detect tunnel target rejected by SSRF guard',
        expect.objectContaining({
          error: expect.stringMatching(/private\/internal/),
          hostname: '127.0.0.1',
        }),
      );
    });

    it('SSRF guard: an IPv4-mapped IPv6 minted qurl_site is rejected as private, not by the host-pin', async () => {
      const get = captureDetect({ detected: false }, { qurlSite: 'https://[::ffff:169.254.169.254]' });
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/private\/internal/);
      expect(mockClient.resolve).not.toHaveBeenCalled();
      expect(get()).toBeNull();
      expect(logger.warn).toHaveBeenCalledWith(
        'Detect tunnel target rejected by SSRF guard',
        expect.objectContaining({ hostname: '[::ffff:a9fe:a9fe]' }),
      );
    });

    it('SSRF guard: a non-https minted qurl_site throws and NO knock or POST happens', async () => {
      const get = captureDetect({ detected: false }, { qurlSite: 'http://r_abc12345678.qurl.site' });
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/https:/);
      expect(mockClient.resolve).not.toHaveBeenCalled();
      expect(get()).toBeNull();
    });

    it('SSRF guard: a PUBLIC host with embedded userinfo throws and NO knock or POST happens', async () => {
      const get = captureDetect(
        { detected: false },
        { qurlSite: 'https://attacker@r_abc12345678.qurl.site' },
      );
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/userinfo/);
      expect(mockClient.resolve).not.toHaveBeenCalled();
      expect(get()).toBeNull();
    });

    it('SSRF guard: a PUBLIC non-qURL host throws and NO knock or POST happens', async () => {
      const get = captureDetect({ detected: false }, { qurlSite: 'https://evil.example.com' });
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/qURL tunnel domain/);
      expect(mockClient.resolve).not.toHaveBeenCalled();
      expect(get()).toBeNull();
      expect(logger.warn).toHaveBeenCalledWith(
        'Detect tunnel target rejected by SSRF guard',
        expect.objectContaining({
          error: expect.stringMatching(/qURL tunnel domain/),
          hostname: 'evil.example.com',
        }),
      );
    });

    it('host-pin rejects the look-alike suffix `evilqurl.site` (no dot separator)', async () => {
      const get = captureDetect({ detected: false }, { qurlSite: 'https://evilqurl.site' });
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/qURL tunnel domain/);
      expect(get()).toBeNull();
    });

    it('backs off when qurl_site host-pin repeatedly rejects, then retries mint without rewalking the slug', async () => {
      const clock = freezeDetectClock();
      try {
        captureDetect({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
        mockClient.createQurlForResource
          .mockResolvedValueOnce({ qurl_id: 'q1', qurl_link: MINT_LINK, qurl_site: TUNNEL_SITE })
          .mockResolvedValueOnce({ qurl_id: 'q2', qurl_link: MINT_LINK, qurl_site: 'https://r_abc12345678.evil.example.com' })
          .mockResolvedValueOnce({ qurl_id: 'q3', qurl_link: MINT_LINK, qurl_site: 'https://r_abc12345678.evil.example.com' })
          .mockResolvedValueOnce({ qurl_id: 'q3', qurl_link: MINT_LINK, qurl_site: TUNNEL_SITE });

        await connector.detectWatermark(Buffer.from('a'), { guildId: 'g', apiKey: 'k' });
        await expect(
          connector.detectWatermark(Buffer.from('b'), { guildId: 'g', apiKey: 'k' }),
        ).rejects.toThrow(/qURL tunnel domain/);
        await expect(
          connector.detectWatermark(Buffer.from('c'), { guildId: 'g', apiKey: 'k' }),
        ).rejects.toThrow(/qURL tunnel domain/);
        await expect(
          connector.detectWatermark(Buffer.from('d'), { guildId: 'g', apiKey: 'k' }),
        ).rejects.toThrow(/backing off/);

        expect(mockClient.listAllResources).toHaveBeenCalledTimes(1);
        expect(mockClient.resolve).toHaveBeenCalledTimes(1);

        clock.advanceBy(30_001);
        await connector.detectWatermark(Buffer.from('e'), { guildId: 'g', apiKey: 'k' });

        expect(mockClient.listAllResources).toHaveBeenCalledTimes(1);
        expect(mockClient.resolve).toHaveBeenCalledTimes(2);
      } finally {
        clock.restore();
      }
    });

    it('throws when qurl_site includes path state instead of silently dropping it', async () => {
      const get = captureDetect({ detected: false }, { qurlSite: `${TUNNEL_SITE}/base/path?x=1#frag` });
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/host-only/);
      expect(mockClient.resolve).not.toHaveBeenCalled();
      expect(get()).toBeNull();
      expect(logger.warn).toHaveBeenCalledWith(
        'Detect tunnel mint returned an invalid qurl_site',
        expect.objectContaining({ error: expect.stringMatching(/host-only/) }),
      );
    });

    it('requires config.QURL_API_KEY for the SDK Bearer even when a per-call apiKey is given (no mint, no POST)', async () => {
      jest.resetModules();
      resetDetectSdkMocks();
      jest.doMock('../src/config', () => ({
        CONNECTOR_URL: 'https://connector.test.local',
        QURL_ENDPOINT: 'https://api.test.local',
        DETECT_TUNNEL_SLUG: 'detect-sandbox',
      }));
      const connectorNoKey = require('../src/connector');
      const fetchSpy = jest.fn();
      globalThis.fetch = fetchSpy;
      await expect(
        connectorNoKey.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k-detect' }),
      ).rejects.toThrow(/QURL_API_KEY is not configured/);
      expect(mockClient.listAllResources).not.toHaveBeenCalled();
      expect(mockClient.createQurlForResource).not.toHaveBeenCalled();
      expect(mockClient.resolve).not.toHaveBeenCalled();
      expect(fetchSpy).not.toHaveBeenCalled();
    });

    it('throws "unparseable qurl_site" when the mint response has no qurl_site, and does NOT knock or POST', async () => {
      mockListAllResources([{ resource_id: RESOURCE_ID, status: 'active' }]);
      mockClient.createQurlForResource.mockResolvedValue({ qurl_id: 'q_x', qurl_link: MINT_LINK });
      const fetchSpy = jest.fn();
      globalThis.fetch = fetchSpy;
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/unparseable qurl_site/);
      expect(mockClient.resolve).not.toHaveBeenCalled();
      expect(fetchSpy).not.toHaveBeenCalled();
      expect(logger.warn).toHaveBeenCalledWith(
        'Detect tunnel mint returned an invalid qurl_site',
        expect.objectContaining({ error: expect.stringMatching(/unparseable qurl_site/) }),
      );
      const warn = logger.warn.mock.calls.find(
        ([msg]) => msg === 'Detect tunnel mint returned an invalid qurl_site',
      );
      expect(warn[1].hostname).toBeUndefined();
    });

    it('rejects an allowlisted mint host when resolve identifies a different resource, then backs off', async () => {
      const clock = freezeDetectClock();
      try {
        captureDetect(
          { detected: false },
          {
            qurlSite: OTHER_TUNNEL_SITE,
            resolveResult: { target_url: '', resource_id: OTHER_RESOURCE_ID },
          },
        );
        mockClient.resolve
          .mockResolvedValueOnce({ target_url: '', resource_id: OTHER_RESOURCE_ID })
          .mockResolvedValueOnce({ target_url: '', resource_id: OTHER_RESOURCE_ID })
          .mockResolvedValueOnce({ target_url: '', resource_id: RESOURCE_ID });
        await expect(
          connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
        ).rejects.toThrow(/mismatched resource_id/);
        expect(globalThis.fetch).not.toHaveBeenCalled();
        expect(logger.warn).toHaveBeenCalledWith(
          'Detect tunnel resolve returned mismatched resource_id',
          expect.objectContaining({ expected_resource_id: RESOURCE_ID, actual_resource_id: OTHER_RESOURCE_ID }),
        );
        await expect(
          connector.detectWatermark(Buffer.from('y'), { guildId: 'g', apiKey: 'k' }),
        ).rejects.toThrow(/mismatched resource_id/);
        await expect(
          connector.detectWatermark(Buffer.from('z'), { guildId: 'g', apiKey: 'k' }),
        ).rejects.toThrow(/backing off/);

        expect(mockClient.listAllResources).toHaveBeenCalledTimes(2);
        expect(mockClient.resolve).toHaveBeenCalledTimes(2);

        clock.advanceBy(30_001);
        await connector.detectWatermark(Buffer.from('w'), { guildId: 'g', apiKey: 'k' });

        expect(mockClient.listAllResources).toHaveBeenCalledTimes(3);
        expect(mockClient.resolve).toHaveBeenCalledTimes(3);
      } finally {
        clock.restore();
      }
    });

    it('rejects a resolve resource_id that differs only by case', async () => {
      const caseChangedResourceId = RESOURCE_ID.replace(/[a-z]/i, (character) => (
        character === character.toLowerCase()
          ? character.toUpperCase()
          : character.toLowerCase()
      ));
      expect(caseChangedResourceId).not.toBe(RESOURCE_ID);
      expect(caseChangedResourceId.toLowerCase()).toBe(RESOURCE_ID.toLowerCase());
      captureDetect(
        { detected: false },
        { resolveResult: { target_url: '', resource_id: caseChangedResourceId } },
      );

      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/mismatched resource_id/);
      expect(globalThis.fetch).not.toHaveBeenCalled();
    });

    it.each([
      ['omits', { target_url: '' }],
      ['returns an empty', { target_url: '', resource_id: '' }],
    ])('fails closed when resolve %s resource_id and never sends image or API key', async (_label, resolveResult) => {
      const get = captureDetect(
        { detected: false, qurl_id: null, match_pct: null, confidence: 0 },
        { qurlSite: OTHER_TUNNEL_SITE, resolveResult },
      );

      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/omitted resource_id/);

      expect(mockClient.resolve).toHaveBeenCalledTimes(1);
      expect(get()).toBeNull();
      expect(logger.warn).toHaveBeenCalledWith(
        'Detect tunnel resolve rejected missing resource_id',
        { expected_resource_id: RESOURCE_ID },
      );

      await expect(
        connector.detectWatermark(Buffer.from('y'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/backing off/);
      expect(mockClient.createQurlForResource).toHaveBeenCalledTimes(1);
      expect(mockClient.resolve).toHaveBeenCalledTimes(1);
      expect(get()).toBeNull();
    });

    it('propagates a resolve() failure (knock/transport) and does NOT POST', async () => {
      mockListAllResources([{ resource_id: RESOURCE_ID, status: 'active' }]);
      mockClient.createQurlForResource.mockResolvedValue({ qurl_id: 'q_x', qurl_link: MINT_LINK, qurl_site: TUNNEL_SITE });
      mockClient.resolve.mockRejectedValue(new Error('resolve transport failure'));
      const fetchSpy = jest.fn();
      globalThis.fetch = fetchSpy;
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/resolve transport failure/);
      expect(fetchSpy).not.toHaveBeenCalled();
      expect(logger.warn).toHaveBeenCalledWith(
        'Detect tunnel resolve failed (knock/transport)',
        expect.objectContaining({ error: 'resolve transport failure' }),
      );
    });

    it('propagates a mint failure (transport) and does NOT resolve or POST', async () => {
      mockListAllResources([{ resource_id: RESOURCE_ID, status: 'active' }]);
      mockClient.createQurlForResource.mockRejectedValue(new Error('mint transport failure'));
      const fetchSpy = jest.fn();
      globalThis.fetch = fetchSpy;
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/mint transport failure/);
      expect(fetchSpy).not.toHaveBeenCalled();
      expect(mockClient.resolve).not.toHaveBeenCalled();
      expect(logger.warn).toHaveBeenCalledWith(
        'Detect tunnel mint failed',
        expect.objectContaining({ error: 'mint transport failure' }),
      );
    });
  });
});

describe('detectTunnelHostSuffixesForEndpoint — env-extendable non-prod allowlist', () => {
  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  it('grants the extra suffix when QURL_ENDPOINT matches an extra non-prod endpoint host', () => {
    jest.resetModules();
    jest.doMock('../src/config', () => ({
      CONNECTOR_URL: 'https://connector.test.local',
      QURL_ENDPOINT: 'https://api.sandbox.example',
      QURL_API_KEY: 'test-key',
      DETECT_EXTRA_NON_PROD_QURL_ENDPOINT_HOSTS: ['api.sandbox.example'],
      DETECT_EXTRA_NON_PROD_HOST_SUFFIXES: ['.tunnel.sandbox.example'],
    }));
    const { detectTunnelHostSuffixesForEndpoint } = require('../src/connector');
    expect(detectTunnelHostSuffixesForEndpoint('https://api.sandbox.example'))
      .toContain('.tunnel.sandbox.example');
  });

  it('does NOT grant the extra suffix for an endpoint absent from the extra allowlist (fail closed)', () => {
    jest.resetModules();
    jest.doMock('../src/config', () => ({
      CONNECTOR_URL: 'https://connector.test.local',
      QURL_ENDPOINT: 'https://api.layerv.ai',
      QURL_API_KEY: 'test-key',
      DETECT_EXTRA_NON_PROD_QURL_ENDPOINT_HOSTS: ['api.sandbox.example'],
      DETECT_EXTRA_NON_PROD_HOST_SUFFIXES: ['.tunnel.sandbox.example'],
    }));
    const { detectTunnelHostSuffixesForEndpoint } = require('../src/connector');
    expect(detectTunnelHostSuffixesForEndpoint('https://api.layerv.ai')).toEqual(['.qurl.site']);
  });

  it('still returns only the production suffix for an unknown endpoint when the extra vars are unset (no behavior change)', () => {
    jest.resetModules();
    jest.doMock('../src/config', () => ({
      CONNECTOR_URL: 'https://connector.test.local',
      QURL_ENDPOINT: 'https://api.layerv.ai',
      QURL_API_KEY: 'test-key',
    }));
    const { detectTunnelHostSuffixesForEndpoint } = require('../src/connector');
    expect(detectTunnelHostSuffixesForEndpoint('https://api.layerv.ai')).toEqual(['.qurl.site']);
  });

  it('still grants the built-in non-prod suffixes for a built-in endpoint host when the extra vars are unset', () => {
    jest.resetModules();
    jest.doMock('../src/config', () => ({
      CONNECTOR_URL: 'https://connector.test.local',
      QURL_ENDPOINT: 'https://api.staging.layerv.ai',
      QURL_API_KEY: 'test-key',
    }));
    const { detectTunnelHostSuffixesForEndpoint } = require('../src/connector');
    expect(detectTunnelHostSuffixesForEndpoint('https://api.staging.layerv.ai'))
      .toEqual(['.qurl.site', '.qurl.site.layerv.xyz', '.qurl.site.layerv.ai']);
  });
});
