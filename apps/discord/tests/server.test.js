const crypto = require('crypto');

// Mock dependencies before requiring modules
jest.mock('../src/discord', () => ({
  sendDM: jest.fn(),
}));

jest.mock('../src/store', () => ({
  getStats: jest.fn(() => ({
    configuredGuilds: 5,
    totalSends: 10,
  })),
  healthCheck: jest.fn(() => ({ ok: true })),
}));

// GUILD_ID must be a valid Discord snowflake to put the bot in
// single-guild mode.
process.env.GUILD_ID = '123456789012345678';
process.env.BASE_URL = 'http://localhost:3000';

const request = require('supertest');
const { app } = require('../src/server');
const db = require('../src/store');

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
});

// ─── /webhooks rawBody parser invariant ────────────────────────────
//
// The qurl-webhook receiver does a pre-HMAC JSON.parse of req.rawBody
// to extract owner_id for secret routing. The SECURITY invariant
// (called out in qurl-webhook.js's verifyAndResolve comment) is that
// the 1mb cap on rawBodyJson middleware bounds the pre-trust window.
// A refactor that re-orders middleware or bumps the limit without
// re-reading that warning would silently widen the attack surface;
// this pin-test fails loudly when the cap regresses.
describe('server.js — /webhooks rawBody middleware invariant', () => {
  const fs = require('fs');
  const path = require('path');
  const serverSource = fs.readFileSync(
    path.join(__dirname, '..', 'src', 'server.js'),
    'utf8',
  );
  // Structural pin: the 1mb cap on /webhooks rawBodyJson is the pre-
  // HMAC parse safety boundary documented in qurl-webhook.js. The
  // behavioral counterpart (recordQurlView NOT called for a >1mb
  // payload) lives in qurl-webhook.test.js where the store mock has
  // the right shape; this regex pin catches whitespace-tolerant
  // refactors that bump the limit constant.
  it('mounts rawBodyJson at /webhooks with a 1mb cap', () => {
    expect(serverSource).toMatch(/rawBodyJson\s*=\s*express\.json\(\{[\s\S]*?limit:\s*['"]1mb['"][\s\S]*?\}\)/);
    expect(serverSource).toMatch(/app\.use\(\s*['"]\/webhooks['"]\s*,\s*rawBodyJson\s*\)/);
  });
  it('rawBodyJson populates req.rawBody (HMAC source-of-truth)', () => {
    // Tolerant of variable-name and whitespace refactors (Prettier-
    // style reformat won't break it): match the verify-callback shape
    // by anchoring on `verify:` + `req.rawBody = buf` rather than the
    // exact parenthesization of the arrow-fn parameters.
    expect(serverSource).toMatch(/verify\s*:[\s\S]{0,200}?req\.rawBody\s*=\s*buf/);
  });
  it('no second body parser is mounted on /webhooks (would widen the pre-HMAC parse surface)', () => {
    // A middleware re-order that adds e.g. `express.urlencoded` or
    // `express.text` at /webhooks would let a different parser run
    // before the receiver's owner_id extraction. The /webhooks path
    // legitimately has multiple `app.use` mounts (rawBodyJson +
    // qurlWebhookRouter), so target PARSERS specifically by name
    // rather than by mount-count.
    expect(serverSource).not.toMatch(/app\.use\(\s*['"]\/webhooks['"]\s*,\s*express\.(urlencoded|text|raw)/);
    // And no bare express.json() at /webhooks either — only the
    // audited rawBodyJson (which is also express.json(), but
    // configured with the 1mb cap and the verify-callback) should
    // mount as a parser on this path.
    expect(serverSource).not.toMatch(/app\.use\(\s*['"]\/webhooks['"]\s*,\s*express\.json\b/);
  });
});
