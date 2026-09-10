
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

    it('revokes partial non-2xx mints before rethrowing the original mint error', async () => {
      const logger = require('../src/logger');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
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
        })
        .mockResolvedValueOnce({
          ok: true,
          status: 200,
          json: async () => ({
            success: true,
            results: [
              { qurl_id: 'q_partial_two', status: 'revoked' },
              { qurl_id: 'q_partial_one', status: 'already_gone' },
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
        expect(e.partialCleanupConfirmed).toBe(true);
      }

      expect(globalThis.fetch).toHaveBeenCalledTimes(2);
      expect(globalThis.fetch.mock.calls[1][0]).toBe('https://connector.test.local/api/revoke_links');
      expect(JSON.parse(globalThis.fetch.mock.calls[1][1].body)).toEqual({
        resource_id: 'res-1',
        qurl_ids: ['q_partial_one', 'q_partial_two'],
      });
      expect(logger.error).not.toHaveBeenCalledWith(
        'Connector partial mint cleanup failed', expect.anything(),
      );

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
      expect(serializedLogs).not.toContain('res-1');
      expect(serializedLogs).not.toContain('at_secret');
      expect(serializedLogs).not.toContain('qurl.link');
    });

    it('retains the original mint error and logs when partial cleanup fails', async () => {
      const logger = require('../src/logger');
      const cleanupError = Object.assign(new Error('cleanup network failure'), { name: 'TypeError' });
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: false,
          status: 502,
          text: async () => JSON.stringify({
            success: false,
            error: 'render failed after mint',
            links: [{ qurl_id: 'q_partial_one' }],
          }),
        })
        .mockRejectedValueOnce(cleanupError);

      await expect(connector.mintLinks(
        'res-1', { expiresAt: '2026-01-01T00:00:00Z', n: 1 },
      )).rejects.toMatchObject({
        message: 'Connector mint_link failed (502)',
        status: 502,
        partialQurlIds: ['q_partial_one'],
      });

      expect(logger.error).toHaveBeenCalledWith(
        'Connector partial mint cleanup failed',
        expect.objectContaining({
          resource_ref: expect.stringMatching(/^sha256:/),
          partial_link_count: 1,
          cleanup_error_name: 'TypeError',
        }),
      );
    });

    it('allows the render-at-mint server budget plus transport slack', async () => {
      const signal = new AbortController().signal;
      const timeoutSpy = jest.spyOn(AbortSignal, 'timeout').mockReturnValue(signal);
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({ success: true, links: [] }),
      });

      try {
        await connector.mintLinks('res-1', {
          expiresAt: '2026-01-01T00:00:00Z', n: 1,
        });
        expect(timeoutSpy).toHaveBeenCalledWith(65_000);
      } finally {
        timeoutSpy.mockRestore();
      }
    });

    it('records malformed partial mint links as unidentified for parent cleanup', async () => {
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
        expect(e.partialLinkCount).toBe(3);
        expect(e.partialQurlIds).toEqual([]);
        expect(e.partialUnidentifiedQurlCount).toBe(3);
        expect(e.partialCleanupConfirmed).toBe(false);
      }

      expect(globalThis.fetch).toHaveBeenCalledTimes(1);
      expect(logger.warn).toHaveBeenCalledWith(
        'Connector mint_link returned partial links on non-2xx',
        expect.objectContaining({
          status: 502,
          partial_link_count: 3,
          partial_qurl_ids: [],
          unidentified_qurl_count: 3,
        }),
      );
    });

    it('cleans every identifiable child from a mixed malformed partial response', async () => {
      const logger = require('../src/logger');
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: false,
          status: 502,
          text: async () => JSON.stringify({
            success: false,
            error: 'render failed after mixed results',
            links: [
              { qurl_id: 'q_partial_valid' },
              { qurl_id: '' },
              { qurl_id: 'bad/id' },
              { qurl_id: `q_${'a'.repeat(200)}` },
              {},
            ],
          }),
        })
        .mockResolvedValueOnce({
          ok: true,
          status: 200,
          json: async () => ({
            success: true,
            results: [{ qurl_id: 'q_partial_valid', status: 'revoked' }],
          }),
        });

      await expect(connector.mintLinks(
        'res-1', { expiresAt: '2026-01-01T00:00:00Z', n: 5 },
      )).rejects.toMatchObject({
        partialQurlIds: ['q_partial_valid'],
        partialUnidentifiedQurlCount: 4,
        partialCleanupConfirmed: true,
      });

      expect(JSON.parse(globalThis.fetch.mock.calls[1][1].body)).toEqual({
        resource_id: 'res-1',
        qurl_ids: ['q_partial_valid'],
      });
      expect(logger.warn).toHaveBeenCalledWith(
        'Connector mint_link returned partial links on non-2xx',
        expect.objectContaining({
          partial_qurl_ids: ['q_partial_valid'],
          unidentified_qurl_count: 4,
        }),
      );
      expect(JSON.stringify(logger.warn.mock.calls)).not.toContain('bad/id');
    });
  });

  describe('revokeMintedLinks — fail-closed response contract', () => {
    it('does not call the connector when no token ids were recorded', async () => {
      globalThis.fetch = jest.fn();

      await expect(connector.revokeMintedLinks('res-1', [], 'guild-key'))
        .resolves.toBe(true);

      expect(globalThis.fetch).not.toHaveBeenCalled();
    });

    it('sends the caller credential and accepts one outcome per requested token', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({
          success: true,
          results: [
            { qurl_id: 'q_one', status: 'revoked' },
            { qurl_id: 'q_two', status: 'already_gone' },
          ],
        }),
      });

      await expect(connector.revokeMintedLinks(
        'res-1', ['q_one', 'q_two'], 'guild-key',
      )).resolves.toBe(true);

      expect(globalThis.fetch).toHaveBeenCalledWith(
        'https://connector.test.local/api/revoke_links',
        expect.objectContaining({
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            Authorization: 'Bearer guild-key',
          },
          body: JSON.stringify({
            resource_id: 'res-1',
            qurl_ids: ['q_one', 'q_two'],
          }),
        }),
      );
      const logger = require('../src/logger');
      expect(logger.info).toHaveBeenCalledWith(
        'Confirmed connector-managed link revoke',
        { resource_ref: expect.stringMatching(/^sha256:/), count: 2 },
      );
    });

    it('accepts exact id-keyed coverage when results are reordered', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({
          success: true,
          results: [
            { qurl_id: 'q_two', status: 'already_gone' },
            { qurl_id: 'q_one', status: 'revoked' },
          ],
        }),
      });

      await expect(connector.revokeMintedLinks(
        'res-1', ['q_one', 'q_two'], 'guild-key',
      )).resolves.toBe(true);
    });

    it('accepts a typed ordinary-token proof from the connector', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({
          success: true,
          results: [{ qurl_id: 'q_one', status: 'not_connector_managed' }],
        }),
      });

      await expect(connector.revokeMintedLinks('res-1', ['q_one'], 'guild-key'))
        .resolves.toBe(true);
    });

    it('chunks more than ten unique ids to the connector batch cap', async () => {
      const ids = Array.from({ length: 11 }, (_, i) => `q_${i + 1}`);
      globalThis.fetch = jest.fn().mockImplementation(async (_url, options) => {
        const requested = JSON.parse(options.body).qurl_ids;
        return {
          ok: true,
          status: 200,
          json: async () => ({
            success: true,
            results: requested.map(qurlId => ({ qurl_id: qurlId, status: 'revoked' })),
          }),
        };
      });

      await expect(connector.revokeMintedLinks('res-1', ids, 'guild-key'))
        .resolves.toBe(true);

      expect(globalThis.fetch).toHaveBeenCalledTimes(2);
      expect(globalThis.fetch.mock.calls.map(([, options]) => (
        JSON.parse(options.body).qurl_ids
      ))).toEqual([ids.slice(0, 10), ids.slice(10)]);
    });

    it('keeps a maximum-size request below the connector body cap', async () => {
      const { MAX_RESOURCE_ID_LENGTH } = require('../src/utils/resource-id');
      const { MAX_QURL_ID_LENGTH } = require('../src/utils/qurl-id');
      const {
        REVOKE_LINKS_MAX_IDS,
        REVOKE_REQUEST_MAX_BYTES,
      } = connector.__testExports;
      const resourceId = `r_${'a'.repeat(MAX_RESOURCE_ID_LENGTH - 2)}`;
      const ids = Array.from({ length: REVOKE_LINKS_MAX_IDS }, (_, i) => (
        `q_${'a'.repeat(MAX_QURL_ID_LENGTH - 4)}${i.toString().padStart(2, '0')}`
      ));
      globalThis.fetch = jest.fn().mockImplementation(async (_url, options) => {
        const requested = JSON.parse(options.body).qurl_ids;
        return {
          ok: true,
          status: 200,
          json: async () => ({
            success: true,
            results: requested.map(qurlId => ({ qurl_id: qurlId, status: 'revoked' })),
          }),
        };
      });

      await expect(connector.revokeMintedLinks(resourceId, ids, 'guild-key'))
        .resolves.toBe(true);

      expect(Buffer.byteLength(globalThis.fetch.mock.calls[0][1].body, 'utf8'))
        .toBeLessThanOrEqual(REVOKE_REQUEST_MAX_BYTES);
    });

    it('requires every chunk to confirm before reporting a large revoke complete', async () => {
      const ids = Array.from({ length: 11 }, (_, i) => `q_${i + 1}`);
      globalThis.fetch = jest.fn()
        .mockResolvedValueOnce({
          ok: true,
          status: 200,
          json: async () => ({
            success: true,
            results: ids.slice(0, 10).map(qurlId => ({ qurl_id: qurlId, status: 'revoked' })),
          }),
        })
        .mockResolvedValueOnce({
          ok: true,
          status: 200,
          json: async () => ({ success: true, results: [] }),
        });

      await expect(connector.revokeMintedLinks('res-1', ids, 'guild-key'))
        .rejects.toMatchObject({ unresolvedCount: 1 });

      expect(globalThis.fetch).toHaveBeenCalledTimes(2);
    });

    it('deduplicates requested token ids before calling the connector', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({
          success: true,
          results: [{ qurl_id: 'q_one', status: 'revoked' }],
        }),
      });

      await expect(connector.revokeMintedLinks(
        'res-1', [' q_one ', 'q_one'], 'guild-key',
      )).resolves.toBe(true);

      expect(JSON.parse(globalThis.fetch.mock.calls[0][1].body).qurl_ids).toEqual(['q_one']);
    });

    it.each([42, {}, '   ', 'bad/id', 'at_bearer_like'])('rejects invalid token id %p before fetch', async (qurlId) => {
      globalThis.fetch = jest.fn();

      await expect(connector.revokeMintedLinks('res-1', [qurlId], 'guild-key'))
        .rejects.toThrow('Invalid connector revoke token identity');

      expect(globalThis.fetch).not.toHaveBeenCalled();
    });

    it('rejects an overlong token id before fetch', async () => {
      globalThis.fetch = jest.fn();

      await expect(connector.revokeMintedLinks(
        'res-1', [`q_${'a'.repeat(200)}`], 'guild-key',
      )).rejects.toThrow('Invalid connector revoke token identity');

      expect(globalThis.fetch).not.toHaveBeenCalled();
    });

    it.each([undefined, null, '', 'bad/id', 'at_bearer_like'])(
      'rejects invalid resource id %p before fetch',
      async (resourceId) => {
        globalThis.fetch = jest.fn();

        await expect(connector.revokeMintedLinks(resourceId, ['q_one'], 'guild-key'))
          .rejects.toThrow('Invalid resource ID format');

        expect(globalThis.fetch).not.toHaveBeenCalled();
      },
    );

    it.each([undefined, null, 'q_one'])('rejects invalid token list %p before fetch', async (qurlIds) => {
      globalThis.fetch = jest.fn();

      await expect(connector.revokeMintedLinks('res-1', qurlIds, 'guild-key'))
        .rejects.toThrow('Invalid connector revoke token list');

      expect(globalThis.fetch).not.toHaveBeenCalled();
    });

    it.each([
      new TypeError('fetch failed'),
      Object.assign(new Error('The operation was aborted due to timeout'), { name: 'TimeoutError' }),
    ])('propagates %s so the send remains retryable', async (error) => {
      globalThis.fetch = jest.fn().mockRejectedValue(error);

      await expect(connector.revokeMintedLinks('res-1', ['q_one'], 'guild-key'))
        .rejects.toBe(error);
    });

    it('allows the bounded revoke handler deadline plus transport slack', async () => {
      const signal = new AbortController().signal;
      const timeoutSpy = jest.spyOn(AbortSignal, 'timeout').mockReturnValue(signal);
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({
          success: true,
          results: [{ qurl_id: 'q_one', status: 'revoked' }],
        }),
      });

      try {
        await connector.revokeMintedLinks('res-1', ['q_one'], 'guild-key');
        expect(timeoutSpy).toHaveBeenCalledWith(65_000);
      } finally {
        timeoutSpy.mockRestore();
      }
    });

    it.each([404, 410, 503])('fails closed on connector HTTP %i', async (status) => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: false,
        status,
        text: async () => JSON.stringify({
          success: false,
          code: status === 503 ? 'revoke_not_available' : 'not_found',
          results: [],
        }),
      });

      await expect(connector.revokeMintedLinks('res-1', ['q_one'], 'guild-key'))
        .rejects.toMatchObject({
          message: `Connector revoke_links failed (${status})`,
          status,
        });
    });

    it('rejects a success response that omits a requested outcome', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({
          success: true,
          results: [{ qurl_id: 'q_one', status: 'revoked' }],
        }),
      });

      await expect(connector.revokeMintedLinks(
        'res-1', ['q_one', 'q_two'], 'guild-key',
      )).rejects.toMatchObject({ unresolvedCount: 1 });
    });

    it('rejects a success response whose outcome belongs to a different token', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({
          success: true,
          results: [{ qurl_id: 'q_other', status: 'revoked' }],
        }),
      });

      await expect(connector.revokeMintedLinks('res-1', ['q_one'], 'guild-key'))
        .rejects.toMatchObject({ unresolvedCount: 1 });
    });

    it('rejects a success response with an extra outcome', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({
          success: true,
          results: [
            { qurl_id: 'q_one', status: 'revoked' },
            { qurl_id: 'q_extra', status: 'already_gone' },
          ],
        }),
      });

      await expect(connector.revokeMintedLinks('res-1', ['q_one'], 'guild-key'))
        .rejects.toMatchObject({ unresolvedCount: 1 });
    });

    it('rejects a success response that repeats one outcome and omits another', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({
          success: true,
          results: [
            { qurl_id: 'q_one', status: 'revoked' },
            { qurl_id: 'q_one', status: 'already_gone' },
          ],
        }),
      });

      await expect(connector.revokeMintedLinks(
        'res-1', ['q_one', 'q_two'], 'guild-key',
      )).rejects.toMatchObject({ unresolvedCount: 1 });
    });

    it('rejects success false even when every per-token status looks successful', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({
          success: false,
          results: [{ qurl_id: 'q_one', status: 'revoked' }],
        }),
      });

      await expect(connector.revokeMintedLinks('res-1', ['q_one'], 'guild-key'))
        .rejects.toMatchObject({ unresolvedCount: 1 });
    });

    it.each([undefined, null, 'not-an-array', {}])(
      'rejects success true with malformed results %p',
      async (results) => {
        globalThis.fetch = jest.fn().mockResolvedValue({
          ok: true,
          status: 200,
          json: async () => ({ success: true, results }),
        });

        await expect(connector.revokeMintedLinks('res-1', ['q_one'], 'guild-key'))
          .rejects.toMatchObject({ unresolvedCount: 1 });
      },
    );

    it.each(['refused', 'failed', 'unknown'])(
      'rejects the per-token %s status',
      async (status) => {
        globalThis.fetch = jest.fn().mockResolvedValue({
          ok: true,
          status: 200,
          json: async () => ({
            success: true,
            results: [{ qurl_id: 'q_one', status }],
          }),
        });

        await expect(connector.revokeMintedLinks('res-1', ['q_one'], 'guild-key'))
          .rejects.toMatchObject({ unresolvedCount: 1 });
      },
    );

    it('reports malformed JSON distinctly from a confirmed revoke failure', async () => {
      globalThis.fetch = jest.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => { throw new SyntaxError('bad JSON'); },
      });

      await expect(connector.revokeMintedLinks('res-1', ['q_one'], 'guild-key'))
        .rejects.toThrow('Connector revoke_links returned invalid JSON');
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
