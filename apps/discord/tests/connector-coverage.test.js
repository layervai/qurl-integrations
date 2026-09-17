
jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

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
          resource_ref: expect.stringMatching(/^sha256:/),
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

    it('counts malformed partial mint links without revoking or surfacing them as ids', async () => {
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

      expect(globalThis.fetch).toHaveBeenCalledTimes(1);
      expect(logger.warn).toHaveBeenCalledWith(
        'Connector mint_link returned partial links on non-2xx',
        expect.objectContaining({ partial_qurl_ids: [], unidentified_qurl_count: 3 }),
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

describe('revokeMintedLinks — #1551 fail-closed contract', () => {
  let connector;
  let logger;
  let revokeOrdinaryLinks;

  const okJson = (body) => ({ ok: true, status: 200, json: async () => body });
  const revoked = (...ids) => okJson({
    success: true,
    results: ids.map(qurl_id => ({ qurl_id, status: 'revoked' })),
  });

  beforeEach(() => {
    jest.resetModules();
    jest.doMock('../src/config', () => ({
      CONNECTOR_URL: 'https://connector.test.local',
      QURL_API_KEY: 'test-key-for-connector',
    }));
    jest.doMock('../src/qurl', () => ({
      ...jest.requireActual('../src/qurl'),
      revokeOrdinaryLinks: jest.fn().mockResolvedValue(undefined),
    }));
    connector = require('../src/connector');
    logger = require('../src/logger');
    ({ revokeOrdinaryLinks } = require('../src/qurl'));
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  it('sends the caller credential and accepts reordered exact coverage', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue(okJson({
      success: true,
      results: [
        { qurl_id: 'q_two', status: 'already_gone' },
        { qurl_id: 'q_one', status: 'revoked' },
      ],
    }));

    await connector.revokeMintedLinks('res-1', ['q_one', ' q_two ', 'q_one'], 'guild-key');

    expect(globalThis.fetch).toHaveBeenCalledWith(
      'https://connector.test.local/api/revoke_links',
      expect.objectContaining({
        method: 'POST',
        redirect: 'error',
        headers: { 'Content-Type': 'application/json', Authorization: 'Bearer guild-key' },
        body: JSON.stringify({ resource_id: 'res-1', qurl_ids: ['q_one', 'q_two'] }),
      }),
    );
    expect(revokeOrdinaryLinks).not.toHaveBeenCalled();
    expect(logger.info).toHaveBeenCalledWith('Confirmed minted link revoke', {
      resource_ref: expect.stringMatching(/^sha256:/), count: 2, route_absent: false,
    });
  });

  it('skips the network and the confirmation log when no token ids were recorded', async () => {
    globalThis.fetch = jest.fn();
    await connector.revokeMintedLinks('res-1', [], 'guild-key');
    expect(globalThis.fetch).not.toHaveBeenCalled();
    expect(revokeOrdinaryLinks).not.toHaveBeenCalled();
    expect(logger.info).not.toHaveBeenCalled();
  });

  it('falls back to the bot credential when no guild key is supplied', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue(revoked('q_one'));
    await connector.revokeMintedLinks('res-1', ['q_one']);
    expect(globalThis.fetch.mock.calls[0][1].headers.Authorization).toBe('Bearer test-key-for-connector');
  });

  it('confirms a successful multi-chunk revoke', async () => {
    const ids = Array.from({ length: 11 }, (_, i) => `q_${i + 1}`);
    globalThis.fetch = jest.fn()
      .mockResolvedValueOnce(revoked(...ids.slice(0, 10)))
      .mockResolvedValueOnce(revoked(ids[10]));

    await connector.revokeMintedLinks('res-1', ids, 'guild-key');

    expect(globalThis.fetch).toHaveBeenCalledTimes(2);
    expect(logger.info).toHaveBeenCalledWith('Confirmed minted link revoke', {
      resource_ref: expect.stringMatching(/^sha256:/), count: 11, route_absent: false,
    });
  });

  it('does not re-probe an absent route for later chunks', async () => {
    const ids = Array.from({ length: 11 }, (_, i) => `q_${i + 1}`);
    globalThis.fetch = jest.fn().mockResolvedValue({ ok: false, status: 404 });

    await connector.revokeMintedLinks('res-1', ids, 'guild-key');

    expect(globalThis.fetch).toHaveBeenCalledTimes(1);
    expect(revokeOrdinaryLinks.mock.calls).toEqual([
      ['res-1', ids.slice(0, 10), 'guild-key'],
      ['res-1', ids.slice(10), 'guild-key'],
    ]);
    expect(logger.info).toHaveBeenCalledWith('Confirmed minted link revoke', expect.objectContaining({
      route_absent: true,
    }));
  });

  it('revokes not_connector_managed children through the SDK, never the parent', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue(okJson({
      success: true,
      results: [
        { qurl_id: 'q_one', status: 'not_connector_managed' },
        { qurl_id: 'q_two', status: 'revoked' },
      ],
    }));

    await connector.revokeMintedLinks('res-1', ['q_one', 'q_two'], 'guild-key');

    expect(revokeOrdinaryLinks).toHaveBeenCalledWith('res-1', ['q_one'], 'guild-key');
  });

  it('falls back to exact SDK child revoke when the default-off route is not registered', async () => {
    const cancel = jest.fn();
    globalThis.fetch = jest.fn().mockResolvedValue({ ok: false, status: 404, body: { cancel } });

    await connector.revokeMintedLinks('res-1', ['q_one'], 'guild-key');

    expect(cancel).toHaveBeenCalled();
    expect(revokeOrdinaryLinks).toHaveBeenCalledWith('res-1', ['q_one'], 'guild-key');
  });

  it('still falls back when discarding the 404 body fails', async () => {
    const cancel = jest.fn().mockRejectedValue(new TypeError('stream locked'));
    globalThis.fetch = jest.fn().mockResolvedValue({ ok: false, status: 404, body: { cancel } });

    await connector.revokeMintedLinks('res-1', ['q_one'], 'guild-key');

    expect(revokeOrdinaryLinks).toHaveBeenCalledWith('res-1', ['q_one'], 'guild-key');
  });

  it('propagates an SDK fallback failure so a watermarked child stays retryable', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue({ ok: false, status: 404 });
    revokeOrdinaryLinks.mockRejectedValueOnce(new Error('qURL API request failed (404)'));

    await expect(connector.revokeMintedLinks('res-1', ['q_one'], 'guild-key'))
      .rejects.toThrow('qURL API request failed (404)');
  });

  it('chunks to ten ids and requires every chunk to confirm', async () => {
    const ids = Array.from({ length: 11 }, (_, i) => `q_${i + 1}`);
    globalThis.fetch = jest.fn()
      .mockResolvedValueOnce(revoked(...ids.slice(0, 10)))
      .mockResolvedValueOnce(okJson({ success: true, results: [] }));

    await expect(connector.revokeMintedLinks('res-1', ids, 'guild-key'))
      .rejects.toThrow('Connector revoke_links did not confirm every requested link');

    expect(globalThis.fetch.mock.calls.map(([, opts]) => JSON.parse(opts.body).qurl_ids))
      .toEqual([ids.slice(0, 10), ids.slice(10)]);
  });

  it('bounds each request by the handler deadline plus transport slack', async () => {
    const timeoutSpy = jest.spyOn(AbortSignal, 'timeout');
    globalThis.fetch = jest.fn().mockResolvedValue(revoked('q_one'));
    try {
      await connector.revokeMintedLinks('res-1', ['q_one'], 'guild-key');
      expect(timeoutSpy).toHaveBeenCalledWith(65_000);
    } finally {
      timeoutSpy.mockRestore();
    }
  });

  it.each([
    [401, {}],
    [403, {}],
    [413, {}],
    [429, { code: 'capacity' }],
    [502, { results: [{ qurl_id: 'q_one', status: 'failed' }] }],
    [503, { code: 'revoke_not_available' }],
  ])('fails closed on connector HTTP %i', async (status, body) => {
    globalThis.fetch = jest.fn().mockResolvedValue({
      ok: false,
      status,
      headers: new Headers({ 'Retry-After': '1' }),
      text: async () => JSON.stringify({ success: false, results: [], ...body }),
    });

    await expect(connector.revokeMintedLinks('res-1', ['q_one'], 'guild-key'))
      .rejects.toMatchObject({ message: `Connector revoke_links failed (${status})`, status });
    expect(revokeOrdinaryLinks).not.toHaveBeenCalled();
  });

  it.each([
    ['an omitted outcome', { success: true, results: [{ qurl_id: 'q_one', status: 'revoked' }] }],
    ['a foreign outcome', { success: true, results: [{ qurl_id: 'q_one', status: 'revoked' }, { qurl_id: 'q_other', status: 'revoked' }] }],
    ['a duplicate outcome', { success: true, results: [{ qurl_id: 'q_one', status: 'revoked' }, { qurl_id: 'q_one', status: 'revoked' }] }],
    ['an extra outcome', { success: true, results: [{ qurl_id: 'q_one', status: 'revoked' }, { qurl_id: 'q_two', status: 'revoked' }, { qurl_id: 'q_x', status: 'revoked' }] }],
    ['malformed results', { success: true, results: {} }],
    ['an unknown status', { success: true, results: [{ qurl_id: 'q_one', status: 'revoked' }, { qurl_id: 'q_two', status: 'refused' }] }],
  ])('rejects a 200 with %s', async (_label, body) => {
    globalThis.fetch = jest.fn().mockResolvedValue(okJson(body));

    await expect(connector.revokeMintedLinks('res-1', ['q_one', 'q_two'], 'guild-key'))
      .rejects.toThrow('Connector revoke_links did not confirm every requested link');
    expect(revokeOrdinaryLinks).not.toHaveBeenCalled();
  });

  it('reports success false distinctly even when every outcome looks terminal', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue(okJson({
      success: false, results: [{ qurl_id: 'q_one', status: 'revoked' }],
    }));
    await expect(connector.revokeMintedLinks('res-1', ['q_one'], 'guild-key'))
      .rejects.toThrow('Connector revoke_links returned success: false');
  });

  it('hands not_connector_managed children from a later chunk to the SDK', async () => {
    const ids = Array.from({ length: 11 }, (_, i) => `q_${i + 1}`);
    globalThis.fetch = jest.fn()
      .mockResolvedValueOnce(revoked(...ids.slice(0, 10)))
      .mockResolvedValueOnce(okJson({ success: true, results: [{ qurl_id: ids[10], status: 'not_connector_managed' }] }));

    await connector.revokeMintedLinks('res-1', ids, 'guild-key');

    expect(revokeOrdinaryLinks.mock.calls).toEqual([['res-1', [ids[10]], 'guild-key']]);
  });

  it('rejects the whole call when a later chunk fails after an earlier chunk confirmed', async () => {
    const ids = Array.from({ length: 11 }, (_, i) => `q_${i + 1}`);
    globalThis.fetch = jest.fn()
      .mockResolvedValueOnce(revoked(...ids.slice(0, 10)))
      .mockResolvedValueOnce(okJson({ success: true, results: [{ qurl_id: ids[10], status: 'not_connector_managed' }] }));
    revokeOrdinaryLinks.mockRejectedValueOnce(new Error('qURL API DELETE failed (503)'));

    await expect(connector.revokeMintedLinks('res-1', ids, 'guild-key')).rejects.toThrow('failed (503)');
    expect(logger.info).not.toHaveBeenCalledWith('Confirmed minted link revoke', expect.anything());
  });

  it('rejects malformed JSON', async () => {
    globalThis.fetch = jest.fn().mockResolvedValue({
      ok: true, status: 200, json: async () => { throw new SyntaxError('bad'); },
    });
    await expect(connector.revokeMintedLinks('res-1', ['q_one'], 'guild-key'))
      .rejects.toThrow('Connector revoke_links returned invalid JSON');
  });

  it.each([42, '   ', 'bad/id', 'at_bearer_like', `q_${'a'.repeat(63)}`])(
    'rejects invalid token id %p before fetch',
    async (qurlId) => {
      globalThis.fetch = jest.fn();
      await expect(connector.revokeMintedLinks('res-1', [qurlId], 'guild-key'))
        .rejects.toThrow('Invalid connector revoke token identity');
      expect(globalThis.fetch).not.toHaveBeenCalled();
    },
  );

  it('rejects a non-array token list before fetch', async () => {
    globalThis.fetch = jest.fn();
    await expect(connector.revokeMintedLinks('res-1', 'q_one', 'guild-key'))
      .rejects.toThrow('Invalid connector revoke token list');
    expect(globalThis.fetch).not.toHaveBeenCalled();
  });

  it('revokes id-only partial mint children before rethrowing the mint error', async () => {
    globalThis.fetch = jest.fn()
      .mockResolvedValueOnce({
        ok: false,
        status: 502,
        text: async () => JSON.stringify({
          success: false,
          links: [{ qurl_id: 'q_partial_one', qurl_link: 'https://qurl.link/#at_secret' }, { qurl_id: 'q_partial_two' }],
        }),
      })
      .mockResolvedValueOnce(revoked('q_partial_one', 'q_partial_two'));

    await expect(connector.mintLinks('res-1', { expiresAt: '2026-01-01T00:00:00Z', n: 2 }))
      .rejects.toMatchObject({ status: 502, partialQurlIds: ['q_partial_one', 'q_partial_two'] });
    expect(JSON.parse(globalThis.fetch.mock.calls[1][1].body)).toEqual({
      resource_id: 'res-1', qurl_ids: ['q_partial_one', 'q_partial_two'],
    });
    expect(logger.error).not.toHaveBeenCalled();
  });

  it('counts partial children whose id cannot be revoked in the warning', async () => {
    globalThis.fetch = jest.fn().mockResolvedValueOnce({
      ok: false,
      status: 502,
      text: async () => JSON.stringify({ success: false, links: [{ qurl_id: `r_${'a'.repeat(80)}` }] }),
    });

    await expect(connector.mintLinks('res-1', { expiresAt: '2026-01-01T00:00:00Z', n: 1 }))
      .rejects.toThrow('Connector mint_link failed (502)');
    expect(globalThis.fetch).toHaveBeenCalledTimes(1);
    expect(logger.warn).toHaveBeenCalledWith('Connector mint_link returned partial links on non-2xx', expect.objectContaining({
      partial_qurl_ids: [], unidentified_qurl_count: 1,
    }));
  });

  it('keeps the original mint error and logs when partial cleanup fails', async () => {
    globalThis.fetch = jest.fn()
      .mockResolvedValueOnce({
        ok: false,
        status: 502,
        text: async () => JSON.stringify({ success: false, links: [{ qurl_id: 'q_partial_one' }] }),
      })
      .mockRejectedValueOnce(new TypeError('fetch failed'));

    await expect(connector.mintLinks('res-1', { expiresAt: '2026-01-01T00:00:00Z', n: 1 }))
      .rejects.toThrow('Connector mint_link failed (502)');
    expect(logger.error).toHaveBeenCalledWith('Connector partial mint cleanup failed', expect.objectContaining({
      resource_ref: expect.stringMatching(/^sha256:/),
      partial_link_count: 1,
      error: 'fetch failed',
    }));
    expect(JSON.stringify(logger.error.mock.calls)).not.toContain('res-1');
  });
});
