
process.env.AUTH0_DOMAIN = 'layerv-test.auth0.com';
process.env.AUTH0_CLIENT_ID = 'test-client-id';

jest.mock('jose', () => ({
  createRemoteJWKSet: jest.fn(() => 'fake-jwks-fn'),
  jwtVerify: jest.fn(),
}));

const EXPECTED_NONCE = 'a'.repeat(32);
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
      payload: { email: 'alice@layerv.test', sub: 'auth0|abc', nonce: EXPECTED_NONCE },
      protectedHeader: { alg: 'RS256' },
    });
    const res = await verifyAuth0IdToken('valid.jwt.sig', EXPECTED_NONCE);
    expect(res.ok).toBe(true);
    expect(res.payload.email).toBe('alice@layerv.test');
    const verifyArgs = jose.jwtVerify.mock.calls[0][2];
    expect(verifyArgs.issuer).toBe('https://layerv-test.auth0.com/');
    expect(verifyArgs.audience).toBe('test-client-id');
  });

  it.each([undefined, null, 42, '', 'b'.repeat(32)])('rejects a verified token with a missing or mismatched nonce %s', async nonce => {
    jose.jwtVerify.mockResolvedValueOnce({ payload: { nonce, email: 'wrong@example.test' } });
    expect(await verifyAuth0IdToken('valid.jwt.sig', EXPECTED_NONCE)).toEqual({ ok: false, reason: 'nonce_mismatch' });
  });

  it('returns coarse-grained reason on signature/issuer/audience/expiry failures', async () => {
    const err = new Error('signature verification failed');
    err.code = 'ERR_JWS_SIGNATURE_VERIFICATION_FAILED';
    jose.jwtVerify.mockRejectedValueOnce(err);
    const res = await verifyAuth0IdToken('tampered.jwt.sig', EXPECTED_NONCE);
    expect(res.ok).toBe(false);
    expect(res.reason).toBe('ERR_JWS_SIGNATURE_VERIFICATION_FAILED');
  });

  it('returns verify_failed when jose throws without a code', async () => {
    jose.jwtVerify.mockRejectedValueOnce(new Error('mystery'));
    const res = await verifyAuth0IdToken('some.jwt.sig', EXPECTED_NONCE);
    expect(res).toEqual({ ok: false, reason: 'verify_failed' });
  });
});

describe('verifyAuth0IdToken — Auth0 not configured', () => {
  it('returns auth0_not_configured when AUTH0_DOMAIN is unset (load-time guard)', async () => {
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
