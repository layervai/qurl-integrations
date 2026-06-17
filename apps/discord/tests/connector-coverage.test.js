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
  audit: jest.fn(),
}));

// Mock `qurlFetch` so connector.detectWatermark's self-mint-then-resolve tunnel
// flow can be driven without real /v1 round-trips. `mockQurlFetch` is a shared
// jest.fn the detect tests configure per case (routes GET /resources, POST
// /resources/{id}/qurls, POST /resolve by (method, path); see captureDetect).
// The `mock`-prefix lets the factory reference it past jest's hoist. Keep the
// REAL `isPrivateHost` — the host-pin SSRF cases need its IP-literal parsing.
// Only resolveDetectTarget() calls qurlFetch, so the upload / mint describes
// never touch this — they don't reach the detect path (they hit globalThis.fetch
// directly, and the real qurlFetch is never invoked there).
const mockQurlFetch = jest.fn();
jest.mock('../src/qurl', () => ({
  ...jest.requireActual('../src/qurl'),
  qurlFetch: mockQurlFetch,
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

    // Adversarial SSRF-bypass inputs.
    it('rejects credential-in-URL that smuggles a different host', () => {
      // https://cdn.discordapp.com@evil.com/file.png parses to hostname evil.com
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

      // The second fetch call (upload to connector) should include auth header
      expect(globalThis.fetch).toHaveBeenCalledTimes(2);
      const uploadHeaders = globalThis.fetch.mock.calls[1][1].headers;
      expect(uploadHeaders['Authorization']).toBe('Bearer test-key-for-connector');
    });
  });

  describe('viewer_ttl_seconds field forwarding', () => {
    // Each upload entry-point must thread viewerTtlSeconds onto the
    // multipart body when the caller passes a positive number, and
    // omit the field entirely otherwise. Pinning all four paths so a
    // future entry-point that forgets `appendViewerTtl` gets caught.
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
        // Second fetch call for the no-CDN paths (re-upload, JSON) lands here.
        .mockResolvedValueOnce({
          ok: true,
          json: async () => ({ success: true, hash: 'h2', resource_id: 'r2' }),
        });
      // Hook the FormData append so we can see exactly what the helper sent.
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

    // mintLinks forwards selfDestructSeconds as session_duration so
    // every minted token on a self-destruct send gets an L7 session
    // window matching the fileviewer's client-side blank. Sibling of
    // the viewer_ttl_seconds tests above — same value, different
    // wire-field (mint_link request JSON, not upload form).
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

      // Defensive: a future caller passing NaN, ±Infinity, a numeric
      // string, a boolean, or an object shouldn't put garbage on the
      // wire ("NaNs", "Infinitys", etc.) and turn a recoverable input
      // mistake into a confusing 400 from qurl-service's
      // validateSessionDuration. `Number.isFinite(x) && x > 0` is the
      // load-bearing predicate. Mirrors the sibling viewer_ttl_seconds
      // defensive-input test below (same idiom, same belt-and-
      // suspenders justification: the confirm-card dropdown is the
      // contract, but mintLinks is exported).
      it('omits session_duration for non-finite / wrong-type / non-positive inputs', async () => {
        const cases = [NaN, Infinity, -Infinity, '30', '0.5', true, false, {}, [], 0, -1, -0.5];
        for (const v of cases) {
          const getBody = captureMintBody();
          // eslint-disable-next-line no-await-in-loop
          await connector.mintLinks('r_xyz', { expiresAt: '2099-01-01T00:00:00Z', n: 1, selfDestructSeconds: v });
          expect(getBody().session_duration).toBeUndefined();
        }
      });

      // guild_id forwarding (#1101): mintLinks attaches the minting guild
      // so the connector can guild-scope a future watermark-attribution
      // lookup. Optional/back-compat — omitted when not provided. Same
      // harness as session_duration above (mint_link request JSON body).
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
      // Defensive: an upstream caller passing 0, NaN, or a string by
      // mistake shouldn't cause the field to land on the form. The
      // confirm-card self-destruct dropdown is the contract (only the
      // 7 preset numeric values reach this layer); the append helper
      // is belt-and-suspenders.
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
    // The connector wraps upstream qURL API errors as
    //   { success: false, error: "QURL API error (403): quota exceeded: token limit per QURL reached (12/10)", links: [] }
    // throwConnectorError must surface this as Error.apiCode = 'quota_exceeded'
    // so the send-pipeline catch block can show a specific user-facing
    // message instead of a generic "Failed to create links. Please try
    // again." (which is unhelpful — the user needs to re-upload, not retry).
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

// Guard the truncation invariant from md5Prefix(): the bot must never log the
// full hash. These tests pin all three upload paths to the helper so a future
// caller can't quietly revert to `hash: result.hash`. See md5Prefix() in
// connector.js for the "why."
describe('Connector client — MD5 hash truncation in upload logs', () => {
  let connector;
  let logger;

  // 32-char hex string. The first 8 chars are what the bot is allowed to log.
  const FULL_MD5 = '5d41402abc4b2a76b9719d911017c592';
  const MD5_PREFIX = '5d41402a';

  beforeEach(() => {
    jest.resetModules();
    mockQurlFetch.mockReset();
    jest.mock('../src/config', () => ({
      CONNECTOR_URL: 'https://connector.test.local',
      QURL_ENDPOINT: 'https://api.test.local',
      QURL_API_KEY: 'test-key',
      // resolveDetectTarget() reads this; the detect tests below drive the
      // mint+resolve via mockQurlFetch and exercise both the configured and
      // unset-slug paths.
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

  // Pins the `typeof hash === 'string'` guard against future schema drift
  // where the connector returns a non-string non-undefined value (number,
  // null, Buffer, ...). The guard must short-circuit to undefined; without
  // it, `?.slice` on a Buffer would have produced a usable byte slice.
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

  // detectWatermark — the bot side of #1101, now over the qURL reverse-tunnel
  // via an EPHEMERAL self-mint per call. The public connector /api/detect path
  // is gone: detectWatermark first self-mints a fresh qURL to the detect tunnel
  // resource (resolve slug → resource_id, mint → at_ token, resolve → NHP knock
  // for our IP), SSRF-guards the target, then POSTs the raw image bytes with the
  // X-Guild-Id scope header; parses {detected, qurl_id, match_pct, confidence}.
  // The handler-side guild filter + cooldown live in commands.js (tested in
  // qurl-send-map.test.js); these pin the multi-leg wire contract this client
  // owns. The mint+resolve legs run through mockQurlFetch; the image POST runs
  // through globalThis.fetch — so "no POST happened" = globalThis.fetch not
  // called (distinct from the mint/resolve qurlFetch legs).
  describe('detectWatermark — self-mint-then-POST tunnel contract', () => {
    // A known-good public https tunnel target the resolve leg hands back — the
    // real qURL reverse-tunnel host form `r_<id>.qurl.site` (qurl-service
    // resourceIDPattern), which the assertPublicHttpsTarget host-pin allows.
    const TUNNEL_TARGET = 'https://r_abc12345678.qurl.site/api/detect';
    // The resource_id the GET /resources?slug lookup resolves to.
    const RESOURCE_ID = 'r_abc12345678';
    // The mint's qurl_link: the at_ access token rides in the fragment.
    const MINT_LINK = 'https://qurl.link.layerv.xyz/#at_testtoken123';

    // Route the three qurlFetch legs by (method, path) and capture the image
    // POST (globalThis.fetch). Returns a getter for the captured POST {url,
    // opts}. Defaults the /resolve target to TUNNEL_TARGET; pass `target` to
    // exercise the SSRF guard. `resourcesList` overrides the GET /resources
    // shape (for the not-found / alt-shape / id-fallback cases).
    function captureDetect(jsonResponse, {
      ok = true,
      status = 200,
      target = TUNNEL_TARGET,
      resourcesList = [{ id: RESOURCE_ID }],
    } = {}) {
      mockQurlFetch.mockImplementation(async (method, path, body) => {
        if (method === 'GET' && path.startsWith('/resources?slug=')) {
          return resourcesList;
        }
        if (method === 'POST' && /^\/resources\/.+\/qurls$/.test(path)) {
          return { qurl_id: 'q_x', qurl_link: MINT_LINK };
        }
        if (method === 'POST' && path === '/resolve') {
          return { target_url: target };
        }
        throw new Error(`unexpected qurlFetch ${method} ${path} ${JSON.stringify(body)}`);
      });
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

    it('self-mints then POSTs to the resolved target with X-Guild-Id, Authorization, Content-Type and raw bytes', async () => {
      const get = captureDetect({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
      const bytes = Buffer.from('imagedata');
      await connector.detectWatermark(bytes, { guildId: 'guild-9', contentType: 'image/png', apiKey: 'k-detect' });
      // The three qurlFetch legs fire in order: resolve slug → resource_id, mint
      // a fresh ephemeral qURL on it, resolve that (the NHP knock for our IP).
      expect(mockQurlFetch).toHaveBeenNthCalledWith(1, 'GET', '/resources?slug=detect-sandbox');
      expect(mockQurlFetch).toHaveBeenNthCalledWith(2, 'POST', `/resources/${RESOURCE_ID}/qurls`, { target_path: '/api/detect' });
      expect(mockQurlFetch).toHaveBeenNthCalledWith(3, 'POST', '/resolve', { access_token: 'at_testtoken123' });
      const { url, opts } = get();
      expect(url).toBe(TUNNEL_TARGET);
      expect(opts.method).toBe('POST');
      expect(opts.headers['X-Guild-Id']).toBe('guild-9');
      // Per-call apiKey threads into the POST Bearer (NOT the qurlFetch Bearer).
      expect(opts.headers['Authorization']).toBe('Bearer k-detect');
      expect(opts.headers['Content-Type']).toBe('image/png');
      expect(opts.body).toBe(bytes);
    });

    it('falls back to octet-stream content-type and global QURL_API_KEY for the POST Bearer', async () => {
      const get = captureDetect({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
      await connector.detectWatermark(Buffer.from('x'), { guildId: 'guild-9' });
      const { opts } = get();
      expect(opts.headers['Content-Type']).toBe('application/octet-stream');
      // This describe block's config mock sets QURL_API_KEY: 'test-key'
      // (line ~493); the fallback resolves to it when no apiKey is passed.
      expect(opts.headers['Authorization']).toBe('Bearer test-key');
    });

    it('caches the resource_id — a second detect skips the GET /resources lookup but re-mints + re-resolves', async () => {
      // _detectResourceId is module-level cached (stable, non-secret), so the
      // slug→resource_id GET happens ONCE; the ephemeral mint + the resolve
      // knock still run per call (fresh short-lived token + fresh IP knock).
      captureDetect({ detected: false, qurl_id: null, match_pct: null, confidence: 0 });
      await connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' });
      await connector.detectWatermark(Buffer.from('y'), { guildId: 'g', apiKey: 'k' });
      const getCalls = mockQurlFetch.mock.calls.filter(([m, p]) => m === 'GET' && p.startsWith('/resources?slug='));
      const mintCalls = mockQurlFetch.mock.calls.filter(([m, p]) => m === 'POST' && /\/qurls$/.test(p));
      const resolveCalls = mockQurlFetch.mock.calls.filter(([m, p]) => m === 'POST' && p === '/resolve');
      expect(getCalls).toHaveLength(1);  // cached after the first call
      expect(mintCalls).toHaveLength(2); // re-minted per call
      expect(resolveCalls).toHaveLength(2); // re-knocked per call
    });

    it('returns the normalized detect result on a detected match', async () => {
      captureDetect({ detected: true, qurl_id: 'q_match1', match_pct: 92, confidence: 0.98 });
      const res = await connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' });
      expect(res).toEqual({ detected: true, qurl_id: 'q_match1', match_pct: 92, confidence: 0.98 });
    });

    it('coerces a garbled/absent detected field to a hard boolean false', async () => {
      // A connector response missing `detected` (or sending a truthy
      // non-boolean) must read as "no attribution", never as truthy.
      captureDetect({ qurl_id: 'q_x', match_pct: 50 });
      const res = await connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' });
      expect(res.detected).toBe(false);
      // Non-number match_pct / missing confidence normalize to null / 0.
      expect(res.confidence).toBe(0);
    });

    it('throws (with .status) on a non-ok response so the handler can ephemeral-error', async () => {
      captureDetect({ error: 'bad guild' }, { ok: false, status: 400 });
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toMatchObject({ status: 400 });
    });

    it('throws when no guildId is given (attribution is guild-scoped) BEFORE minting', async () => {
      // Ordering guard: the guildId check must run before resolveDetectTarget,
      // so no mint and no resolve (the NHP knock) is ever issued for a malformed
      // call.
      const get = captureDetect({ detected: false });
      await expect(
        connector.detectWatermark(Buffer.from('x'), { apiKey: 'k' }),
      ).rejects.toThrow(/guild-scoped/);
      expect(mockQurlFetch).not.toHaveBeenCalled();
      expect(get()).toBeNull();
    });

    it('throws a clear configured-error when DETECT_TUNNEL_SLUG is unset, no mint, no POST', async () => {
      // Re-require connector under a config mock with DETECT_TUNNEL_SLUG unset.
      jest.resetModules();
      mockQurlFetch.mockReset();
      jest.doMock('../src/config', () => ({
        CONNECTOR_URL: 'https://connector.test.local',
        QURL_ENDPOINT: 'https://api.test.local',
        QURL_API_KEY: 'test-key',
        // DETECT_TUNNEL_SLUG intentionally absent.
      }));
      const connectorNoSlug = require('../src/connector');
      const fetchSpy = jest.fn();
      globalThis.fetch = fetchSpy;
      await expect(
        connectorNoSlug.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/DETECT_TUNNEL_SLUG is not configured/);
      // Without the slug, neither the mint legs nor the POST are attempted.
      expect(mockQurlFetch).not.toHaveBeenCalled();
      expect(fetchSpy).not.toHaveBeenCalled();
    });

    it('throws "resource not found" when the slug resolves to no resource, and does NOT mint or POST', async () => {
      // An empty /resources list (or a list whose [0] has no id) must hit the
      // clean throw, not a TypeError — and never mint/POST.
      const get = captureDetect({ detected: false }, { resourcesList: [] });
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/resource not found for slug/);
      // Only the GET ran; no mint, no resolve.
      expect(mockQurlFetch).toHaveBeenCalledTimes(1);
      expect(mockQurlFetch).toHaveBeenCalledWith('GET', '/resources?slug=detect-sandbox');
      expect(get()).toBeNull();
    });

    it('accepts the {resources:[...]} envelope shape and the resource_id id-fallback', async () => {
      // The GET shape may be the bare array OR `{resources:[...]}`, and the id
      // field may be `id` OR `resource_id`. Exercise both alt branches at once.
      const get = captureDetect(
        { detected: false, qurl_id: null, match_pct: null, confidence: 0 },
        { resourcesList: { resources: [{ resource_id: RESOURCE_ID }] } },
      );
      await connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' });
      // The mint targets the resource_id pulled from the envelope+fallback shape.
      expect(mockQurlFetch).toHaveBeenNthCalledWith(2, 'POST', `/resources/${RESOURCE_ID}/qurls`, { target_path: '/api/detect' });
      expect(get().url).toBe(TUNNEL_TARGET);
    });

    it('throws "mint did not return an access token" when the mint qurl_link lacks an at_ fragment, and does NOT POST', async () => {
      // A mint response whose qurl_link has no `#at_…` fragment must hit the
      // clean throw (breadcrumbed), never POST, and never call /resolve.
      mockQurlFetch.mockImplementation(async (method, path) => {
        if (method === 'GET' && path.startsWith('/resources?slug=')) return [{ id: RESOURCE_ID }];
        if (method === 'POST' && /\/qurls$/.test(path)) return { qurl_id: 'q_x', qurl_link: 'https://qurl.link.layerv.xyz/no-fragment' };
        throw new Error(`unexpected qurlFetch ${method} ${path}`);
      });
      const fetchSpy = jest.fn();
      globalThis.fetch = fetchSpy;
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/mint did not return an access token/);
      expect(fetchSpy).not.toHaveBeenCalled();
      // No /resolve attempted once the mint yields no usable token.
      expect(mockQurlFetch.mock.calls.some(([m, p]) => m === 'POST' && p === '/resolve')).toBe(false);
      // Breadcrumb: a mint failure is logged (message only — never token/URL).
      expect(logger.warn).toHaveBeenCalledWith(
        'Detect tunnel mint failed',
        expect.objectContaining({ error: expect.stringMatching(/access token/) }),
      );
    });

    it('SSRF guard: a private/loopback resolved target_url throws and NO POST happens', async () => {
      const get = captureDetect({ detected: false }, { target: 'https://127.0.0.1/api/detect' });
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/private\/internal/);
      // The mint+resolve legs ran (knock issued), but the SSRF guard rejected
      // before the POST.
      expect(mockQurlFetch.mock.calls.some(([m, p]) => m === 'POST' && p === '/resolve')).toBe(true);
      expect(get()).toBeNull();
      // Breadcrumb: a rejected target is logged (message only, never the URL).
      expect(logger.warn).toHaveBeenCalledWith(
        'Detect tunnel target rejected by SSRF guard',
        expect.objectContaining({ error: expect.stringMatching(/private\/internal/) }),
      );
    });

    it('SSRF guard: a non-https resolved target_url throws and NO POST happens', async () => {
      const get = captureDetect({ detected: false }, { target: 'http://r_abc12345678.qurl.site/api/detect' });
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/https:/);
      expect(get()).toBeNull();
    });

    it('SSRF guard: a PUBLIC host with embedded userinfo throws and NO POST happens', async () => {
      // Pins the userinfo branch INDEPENDENTLY of the other guards: the host is
      // an OTHERWISE-VALID public qurl.site tunnel host, so neither isPrivateHost
      // nor the qurl.site host-pin fires and the scheme is https — only the
      // userinfo check can reject this `https://good@valid-host/` confusion form.
      const get = captureDetect(
        { detected: false },
        { target: 'https://attacker@r_abc12345678.qurl.site/api/detect' },
      );
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/userinfo/);
      // The resolve leg ran (knock), but the userinfo guard rejected before the POST.
      expect(mockQurlFetch.mock.calls.some(([m, p]) => m === 'POST' && p === '/resolve')).toBe(true);
      expect(get()).toBeNull();
    });

    it('SSRF guard: a PUBLIC non-qURL host (not under qurl.site) throws and NO POST happens', async () => {
      // Host-pin: even a perfectly public, non-private https host is rejected
      // unless it's under the qURL tunnel domain (qurl.site) — so a compromised
      // or spoofed resolve() can't redirect the image bytes + Bearer to an
      // attacker endpoint. isPrivateHost would NOT fire on a public host; only
      // the host-pin catches this.
      const get = captureDetect({ detected: false }, { target: 'https://evil.example.com/api/detect' });
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/qurl\.site/);
      expect(mockQurlFetch.mock.calls.some(([m, p]) => m === 'POST' && p === '/resolve')).toBe(true);
      expect(get()).toBeNull();
      // And it's logged via the SSRF-rejection breadcrumb (message only).
      expect(logger.warn).toHaveBeenCalledWith(
        'Detect tunnel target rejected by SSRF guard',
        expect.objectContaining({ error: expect.stringMatching(/qurl\.site/) }),
      );
    });

    it('host-pin rejects the look-alike suffix `evilqurl.site` (no dot separator)', async () => {
      // Guards the endsWith boundary: `evilqurl.site` must NOT satisfy the
      // `.qurl.site` suffix (no dot separator), so it's rejected like any other
      // non-qURL host. (The valid `*.qurl.site` subdomain form — the only shape a
      // real tunnel host `r_<id>.qurl.site` takes — is covered by the happy-path
      // tests above via TUNNEL_TARGET.)
      const get = captureDetect({ detected: false }, { target: 'https://evilqurl.site/api/detect' });
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/qurl\.site/);
      expect(get()).toBeNull();
    });

    it('requires config.QURL_API_KEY for the qurlFetch Bearer even when a per-call apiKey is given (no mint, no POST)', async () => {
      // The mint+resolve legs always authenticate with the global QURL_API_KEY
      // (qurlFetch's default Bearer), so a set per-call apiKey can't substitute
      // for it. With QURL_API_KEY unset we must fail fast with the clean
      // configured-error BEFORE any mint or POST.
      jest.resetModules();
      mockQurlFetch.mockReset();
      jest.doMock('../src/config', () => ({
        CONNECTOR_URL: 'https://connector.test.local',
        QURL_ENDPOINT: 'https://api.test.local',
        // QURL_API_KEY intentionally absent.
        DETECT_TUNNEL_SLUG: 'detect-sandbox',
      }));
      const connectorNoKey = require('../src/connector');
      const fetchSpy = jest.fn();
      globalThis.fetch = fetchSpy;
      await expect(
        connectorNoKey.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k-detect' }),
      ).rejects.toThrow(/QURL_API_KEY is not configured/);
      // A per-call apiKey can't stand in for the qurlFetch Bearer: no mint, no POST.
      expect(mockQurlFetch).not.toHaveBeenCalled();
      expect(fetchSpy).not.toHaveBeenCalled();
    });

    it('throws "unparseable target URL" when resolve() returns no target_url, and does NOT POST', async () => {
      // A /resolve success envelope missing target_url (e.g. {} or another shape):
      // assertPublicHttpsTarget(undefined) → new URL(undefined) throws → caught →
      // the graceful "unparseable target URL". Pins that a future shape change
      // (or a different destructure) can't silently POST to undefined.
      mockQurlFetch.mockImplementation(async (method, path) => {
        if (method === 'GET' && path.startsWith('/resources?slug=')) return [{ id: RESOURCE_ID }];
        if (method === 'POST' && /\/qurls$/.test(path)) return { qurl_id: 'q_x', qurl_link: MINT_LINK };
        if (method === 'POST' && path === '/resolve') return {}; // no target_url
        throw new Error(`unexpected qurlFetch ${method} ${path}`);
      });
      const fetchSpy = jest.fn();
      globalThis.fetch = fetchSpy;
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/unparseable target URL/);
      expect(fetchSpy).not.toHaveBeenCalled();
    });

    it('propagates a resolve() failure (knock/transport) and does NOT POST', async () => {
      // A /resolve rejection — the knock or transport failing after qurlFetch's
      // own retries — propagates to the handler (intended); crucially NO POST is
      // attempted, so a failed knock never leaks an un-knocked request.
      mockQurlFetch.mockImplementation(async (method, path) => {
        if (method === 'GET' && path.startsWith('/resources?slug=')) return [{ id: RESOURCE_ID }];
        if (method === 'POST' && /\/qurls$/.test(path)) return { qurl_id: 'q_x', qurl_link: MINT_LINK };
        if (method === 'POST' && path === '/resolve') throw new Error('resolve transport failure');
        throw new Error(`unexpected qurlFetch ${method} ${path}`);
      });
      const fetchSpy = jest.fn();
      globalThis.fetch = fetchSpy;
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/resolve transport failure/);
      expect(fetchSpy).not.toHaveBeenCalled();
      // Breadcrumb: a failed knock/transport is logged distinctly from a
      // rejected target, so activation failures are diagnosable.
      expect(logger.warn).toHaveBeenCalledWith(
        'Detect tunnel resolve failed (knock/transport)',
        expect.objectContaining({ error: 'resolve transport failure' }),
      );
    });

    it('propagates a mint failure (transport) and does NOT resolve or POST', async () => {
      // A mint qurlFetch rejection (the POST /resources/{id}/qurls leg failing
      // after retries) is breadcrumbed via 'Detect tunnel mint failed' and
      // rethrown — never reaching /resolve or the image POST.
      mockQurlFetch.mockImplementation(async (method, path) => {
        if (method === 'GET' && path.startsWith('/resources?slug=')) return [{ id: RESOURCE_ID }];
        if (method === 'POST' && /\/qurls$/.test(path)) throw new Error('mint transport failure');
        throw new Error(`unexpected qurlFetch ${method} ${path}`);
      });
      const fetchSpy = jest.fn();
      globalThis.fetch = fetchSpy;
      await expect(
        connector.detectWatermark(Buffer.from('x'), { guildId: 'g', apiKey: 'k' }),
      ).rejects.toThrow(/mint transport failure/);
      expect(fetchSpy).not.toHaveBeenCalled();
      expect(mockQurlFetch.mock.calls.some(([m, p]) => m === 'POST' && p === '/resolve')).toBe(false);
      expect(logger.warn).toHaveBeenCalledWith(
        'Detect tunnel mint failed',
        expect.objectContaining({ error: 'mint transport failure' }),
      );
    });
  });
});
