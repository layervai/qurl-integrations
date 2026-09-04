const { withFreshEnv } = require('./helpers/fresh-config');

const AUTH0_ENV = {
  AUTH0_DOMAIN: 'layerv-test.auth0.com',
  AUTH0_CLIENT_ID: 'test-client-id',
  AUTH0_CLIENT_SECRET: 'test-client-secret',
  AUTH0_AUDIENCE: 'https://api.layerv.test',
};

function captureServerLogs(connection) {
  let calls;
  withFreshEnv({
    ...AUTH0_ENV,
    AUTH0_EMAIL_CONNECTION: connection,
  }, () => {
    jest.doMock('../src/logger', () => ({
      info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(), audit: jest.fn(),
    }));
    jest.doMock('../src/discord', () => ({ sendDM: jest.fn() }));
    jest.doMock('../src/store', () => ({
      getStats: jest.fn(), healthCheck: jest.fn(),
    }));
    let stopIntervals;
    try {
      const logger = require('../src/logger');
      ({ stopIntervals } = require('../src/server'));
      calls = [...logger.info.mock.calls];
    } finally {
      stopIntervals?.();
    }
  });
  return calls;
}

describe('server Auth0 connection policy log', () => {
  it('names the pinned connection in the message and metadata', () => {
    expect(captureServerLogs('email')).toContainEqual([
      'qURL OAuth authorize redirects pin Auth0 connection "email"; the Auth0 application must enable it.',
      { event: 'qurl_oauth_auth0_connection_policy', connection: 'email' },
    ]);
  });

  it('makes the unpinned deployment state explicit', () => {
    expect(captureServerLogs(undefined)).toContainEqual([
      'qURL OAuth authorize redirects send no connection pin (AUTH0_EMAIL_CONNECTION unset); upstream identity-provider sessions may still select an account until #1365.',
      { event: 'qurl_oauth_auth0_connection_policy', connection: null },
    ]);
  });
});
