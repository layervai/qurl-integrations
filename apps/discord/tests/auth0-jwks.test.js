// Tests for src/utils/auth0-jwks.js — the JWKS-cached id_token verifier
// that replaced "decode the base64 payload, trust the TLS chain" in
// qurl-oauth.js. The verifier is load-bearing for the success-page
// binding readout's qURL-account-email line: a forged email there
// would defeat the confused-deputy mitigation, so the verifier must
// reject anything that isn't signature-, issuer-, audience-, and
// expiry-valid against Auth0's published JWKS.
//
// We can't reach Auth0's HTTPS endpoint from a unit test (no creds, no
// network in CI), so jose is mocked at the module boundary and we
// exercise verifyAuth0IdToken's wrapper logic only.

process.env.AUTH0_DOMAIN = 'layerv-test.auth0.com';
process.env.AUTH0_CLIENT_ID = 'test-client-id';

jest.mock('jose', () => ({
  createRemoteJWKSet: jest.fn(() => 'fake-jwks-fn'),
  jwtVerify: jest.fn(),
}));

const jose = require('jose');
const { verifyAuth0IdToken } = require('../src/utils/auth0-jwks');

beforeEach(() => {
  jest.clearAllMocks();
});

describe('verifyAuth0IdToken', () => {
  it('returns no_token for a missing or non-string token', async () => {
    expect(await verifyAuth0IdToken(undefined)).toEqual({ ok: false, reason: 'no_token' });
    expect(await verifyAuth0IdToken('')).toEqual({ ok: false, reason: 'no_token' });
    expect(await verifyAuth0IdToken(123)).toEqual({ ok: false, reason: 'no_token' });
    expect(jose.jwtVerify).not.toHaveBeenCalled();
  });

  it('returns ok with the payload when jose.jwtVerify resolves', async () => {
    jose.jwtVerify.mockResolvedValueOnce({
      payload: { email: 'alice@layerv.test', sub: 'auth0|abc' },
      protectedHeader: { alg: 'RS256' },
    });
    const res = await verifyAuth0IdToken('valid.jwt.sig');
    expect(res.ok).toBe(true);
    expect(res.payload.email).toBe('alice@layerv.test');
    // Issuer and audience MUST be enforced — pin them so a future
    // refactor doesn't quietly drop the validation.
    const verifyArgs = jose.jwtVerify.mock.calls[0][2];
    expect(verifyArgs.issuer).toBe('https://layerv-test.auth0.com/');
    expect(verifyArgs.audience).toBe('test-client-id');
  });

  it('returns coarse-grained reason on signature/issuer/audience/expiry failures', async () => {
    const err = new Error('signature verification failed');
    err.code = 'ERR_JWS_SIGNATURE_VERIFICATION_FAILED';
    jose.jwtVerify.mockRejectedValueOnce(err);
    const res = await verifyAuth0IdToken('tampered.jwt.sig');
    expect(res.ok).toBe(false);
    expect(res.reason).toBe('ERR_JWS_SIGNATURE_VERIFICATION_FAILED');
  });

  it('returns verify_failed when jose throws without a code', async () => {
    jose.jwtVerify.mockRejectedValueOnce(new Error('mystery'));
    const res = await verifyAuth0IdToken('some.jwt.sig');
    expect(res).toEqual({ ok: false, reason: 'verify_failed' });
  });
});

describe('verifyAuth0IdToken — Auth0 not configured', () => {
  it('returns auth0_not_configured when AUTH0_DOMAIN is unset (load-time guard)', async () => {
    // The module reads AUTH0_DOMAIN via config; clearing it forces the
    // verifier into its short-circuit path. Use jest.isolateModules so
    // config.js is re-required against the cleared env (caching is per-
    // worker and we'd otherwise see the stale value).
    const saved = process.env.AUTH0_DOMAIN;
    delete process.env.AUTH0_DOMAIN;
    try {
      await jest.isolateModulesAsync(async () => {
        jest.doMock('jose', () => ({
          createRemoteJWKSet: jest.fn(() => 'fake-jwks-fn'),
          jwtVerify: jest.fn(),
        }));
        // eslint-disable-next-line global-require
        const { verifyAuth0IdToken: fresh } = require('../src/utils/auth0-jwks');
        const res = await fresh('any.jwt.sig');
        expect(res).toEqual({ ok: false, reason: 'auth0_not_configured' });
      });
    } finally {
      process.env.AUTH0_DOMAIN = saved;
    }
  });
});
