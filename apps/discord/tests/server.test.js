const crypto = require('crypto');

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
      expect(db.getStats).not.toHaveBeenCalled();
    });

    it('returns 503 when db.healthCheck throws', async () => {
      db.healthCheck.mockImplementationOnce(() => { throw new Error('db unreachable'); });
      const res = await request(app).get('/health');
      expect(res.status).toBe(503);
      expect(res.body.status).toBe('unhealthy');
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
      let last;
      for (let i = 0; i < 31; i++) {
        last = await request(app).get('/metrics').set('Authorization', 'Bearer secret-token');
      }
      expect(last.status).toBe(429);
      expect(last.body.error).toBe('Rate limit exceeded');
    });
  });
});

describe('server.js — /webhooks rawBody middleware invariant', () => {
  const fs = require('fs');
  const path = require('path');
  const serverSource = fs.readFileSync(
    path.join(__dirname, '..', 'src', 'server.js'),
    'utf8',
  );
  it('mounts rawBodyJson at /webhooks with a 1mb cap', () => {
    expect(serverSource).toMatch(/rawBodyJson\s*=\s*express\.json\(\{[\s\S]*?limit:\s*['"]1mb['"][\s\S]*?\}\)/);
    expect(serverSource).toMatch(/app\.use\(\s*['"]\/webhooks['"]\s*,\s*rawBodyJson\s*\)/);
  });
  it('rawBodyJson populates req.rawBody (HMAC source-of-truth)', () => {
    expect(serverSource).toMatch(/verify\s*:[\s\S]{0,200}?req\.rawBody\s*=\s*buf/);
  });
  it('no second body parser is mounted on /webhooks (would widen the pre-HMAC parse surface)', () => {
    expect(serverSource).not.toMatch(/app\.use\(\s*['"]\/webhooks['"]\s*,\s*express\.(urlencoded|text|raw)/);
    expect(serverSource).not.toMatch(/app\.use\(\s*['"]\/webhooks['"]\s*,\s*express\.json\b/);
  });
});
