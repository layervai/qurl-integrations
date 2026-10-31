const request = require('supertest');
const { withFreshEnv } = require('./helpers/fresh-config');

function loadServer(env) {
  let server;
  withFreshEnv(env, () => {
    jest.doMock('../src/logger', () => ({ info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(), audit: jest.fn() }));
    jest.doMock('../src/discord', () => ({ sendDM: jest.fn() }));
    jest.doMock('../src/store', () => ({ getStats: jest.fn(), healthCheck: jest.fn() }));
    try {
      server = require('../src/server');
    } finally {
      server?.stopIntervals();
      jest.dontMock('../src/logger');
      jest.dontMock('../src/discord');
      jest.dontMock('../src/store');
    }
  });
  return server;
}

it.each(['true', 'false', '-1', '1oops', '1.5', '1e2', 'Infinity', 'NaN', '9007199254740992'])(
  'refuses invalid TRUST_PROXY=%s at module load', value => {
    expect(() => loadServer({ TRUST_PROXY: value })).toThrow(/TRUST_PROXY must be/);
  },
);

it.each([
  [undefined, 'development', 0], [undefined, 'production', 1],
  ['', 'production', 1], ['0', 'production', 0], ['1', 'staging', 1], ['2', 'development', 2],
])('sets numeric hops for %s in %s', (value, mode, expected) => {
  expect(loadServer({ TRUST_PROXY: value, NODE_ENV: mode }).app.get('trust proxy')).toBe(expected);
});

it('ignores spoofed prefixes before the ALB-appended client address', async () => {
  const { app } = loadServer({ TRUST_PROXY: '1' });
  app.get('/__proxy-test', (req, res) => res.json({ ip: req.ip }));
  for (const spoofed of ['198.51.100.10', '198.51.100.99']) {
    const response = await request(app).get('/__proxy-test').set('X-Forwarded-For', `${spoofed}, 203.0.113.20`);
    expect(response.body.ip).toBe('203.0.113.20');
  }
});

it('ignores forwarded addresses on a direct connection', async () => {
  const { app } = loadServer({ TRUST_PROXY: '0' });
  app.get('/__proxy-test', (req, res) => res.json({ ip: req.ip }));
  const response = await request(app).get('/__proxy-test').set('X-Forwarded-For', '198.51.100.10');
  expect(response.body.ip).toMatch(/127\.0\.0\.1|::1/);
});

it('refuses startup after an all-hops override without opening a listener', () => {
  const { app, startServer } = loadServer({ TRUST_PROXY: '1' });
  const listen = jest.spyOn(app, 'listen').mockReturnValue({});
  app.set('trust proxy', true);
  expect(startServer).toThrow(/TRUST_PROXY changed/);
  expect(listen).not.toHaveBeenCalled();
});
