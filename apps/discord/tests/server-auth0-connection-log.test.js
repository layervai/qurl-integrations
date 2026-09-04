const AUTH0_ENV = {
  AUTH0_DOMAIN: 'layerv-test.auth0.com',
  AUTH0_CLIENT_ID: 'test-client-id',
  AUTH0_CLIENT_SECRET: 'test-client-secret',
  AUTH0_AUDIENCE: 'https://api.layerv.test',
};

function captureServerLogs(connection) {
  const previous = {};
  for (const [key, value] of Object.entries({
    ...AUTH0_ENV,
    AUTH0_EMAIL_CONNECTION: connection,
  })) {
    previous[key] = process.env[key];
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }

  let calls;
  let stopIntervals;
  try {
    jest.isolateModules(() => {
      jest.doMock('../src/logger', () => ({
        info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(), audit: jest.fn(),
      }));
      jest.doMock('../src/discord', () => ({ sendDM: jest.fn() }));
      jest.doMock('../src/store', () => ({
        getStats: jest.fn(), healthCheck: jest.fn(),
      }));
      const logger = require('../src/logger');
      ({ stopIntervals } = require('../src/server'));
      calls = logger.info.mock.calls;
    });
  } finally {
    stopIntervals?.();
    jest.dontMock('../src/logger');
    jest.dontMock('../src/discord');
    jest.dontMock('../src/store');
    for (const [key, value] of Object.entries(previous)) {
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
  }
  return calls;
}

describe('server Auth0 connection policy log', () => {
  it('names the pinned connection in the message and metadata', () => {
    expect(captureServerLogs('email')).toContainEqual([
      'qURL OAuth authorize redirects pin Auth0 connection "email"; the Auth0 application must enable it.',
      { connection: 'email' },
    ]);
  });

  it('makes the unpinned deployment state explicit', () => {
    expect(captureServerLogs(undefined)).toContainEqual([
      'qURL OAuth authorize redirects send no connection pin (AUTH0_EMAIL_CONNECTION unset); upstream identity-provider sessions may still select an account until #1365.',
      { connection: null },
    ]);
  });
});
