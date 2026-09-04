const { withFreshEnv } = require('./helpers/fresh-config');

const AUTH0_ENV = {
  AUTH0_DOMAIN: 'layerv-test.auth0.com',
  AUTH0_CLIENT_ID: 'test-client-id',
  AUTH0_CLIENT_SECRET: 'test-client-secret',
  AUTH0_AUDIENCE: 'https://api.layerv.test',
};

function captureServerLogs(connection, auth0Env = AUTH0_ENV) {
  let calls;
  withFreshEnv({
    ...auth0Env,
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
      calls = {
        info: [...logger.info.mock.calls],
        warn: [...logger.warn.mock.calls],
      };
    } finally {
      stopIntervals?.();
      jest.dontMock('../src/logger');
      jest.dontMock('../src/discord');
      jest.dontMock('../src/store');
    }
  });
  return calls;
}

describe('server Auth0 connection policy log', () => {
  it('names the pinned connection in the message and metadata', () => {
    expect(captureServerLogs('email').info).toContainEqual([
      'qURL OAuth authorize redirects pin Auth0 connection "email"; the Auth0 application must enable it.',
      { event: 'qurl_oauth_auth0_connection_policy', connection: 'email' },
    ]);
  });

  it('makes the unpinned deployment state explicit', () => {
    expect(captureServerLogs(undefined).warn).toContainEqual([
      'qURL OAuth authorize redirects send no connection pin (AUTH0_EMAIL_CONNECTION unset); upstream identity-provider sessions may still select an account until #1365.',
      { event: 'qurl_oauth_auth0_connection_policy', connection: null },
    ]);
  });

  it('reports a configured connection that is inactive without the Auth0 app settings', () => {
    expect(captureServerLogs('email', {
      AUTH0_DOMAIN: undefined,
      AUTH0_CLIENT_ID: undefined,
      AUTH0_CLIENT_SECRET: undefined,
      AUTH0_AUDIENCE: undefined,
    }).info).toContainEqual([
      'AUTH0_EMAIL_CONNECTION="email" is set but inactive because qURL OAuth AUTH0_* settings are incomplete.',
      { event: 'qurl_oauth_auth0_connection_policy', connection: 'email' },
    ]);
  });

  it('emits a policy event when both OAuth and the connection are unconfigured', () => {
    expect(captureServerLogs(undefined, {
      AUTH0_DOMAIN: undefined,
      AUTH0_CLIENT_ID: undefined,
      AUTH0_CLIENT_SECRET: undefined,
      AUTH0_AUDIENCE: undefined,
    }).info).toContainEqual([
      'AUTH0_EMAIL_CONNECTION is unset and inactive because qURL OAuth AUTH0_* settings are incomplete.',
      { event: 'qurl_oauth_auth0_connection_policy', connection: null },
    ]);
  });

  it('does not leak its module mocks to later tests', () => {
    captureServerLogs('email');
    expect(jest.isMockFunction(require('../src/logger').info)).toBe(false);
    expect(jest.isMockFunction(require('../src/discord').sendDM)).toBe(false);
  });
});
