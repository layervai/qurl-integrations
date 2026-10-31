process.env.AUTH0_DOMAIN = 'layerv-test.auth0.com';
process.env.AUTH0_CLIENT_ID = 'test-client-id';
const mockJwks = jest.fn();
jest.mock('jose', () => ({
  ...jest.requireActual('jose'),
  createRemoteJWKSet: () => mockJwks,
}));
const { generateKeyPair, SignJWT } = require('jose');
const { verifyAuth0IdToken } = require('../src/utils/auth0-jwks');
let privateKey;
const now = Math.floor(Date.now() / 1000);
const binding = { nonce: 'flow-nonce', startedAt: now - 10 };

beforeAll(async () => {
  const keys = await generateKeyPair('RS256');
  privateKey = keys.privateKey;
  mockJwks.mockResolvedValue(keys.publicKey);
});

async function tokenWithout(missing) {
  const claims = {
    sub: 'auth0|abc', nonce: binding.nonce, auth_time: now, iat: now, exp: now + 60,
    iss: 'https://layerv-test.auth0.com/', aud: 'test-client-id',
  };
  if (missing) delete claims[missing];
  return new SignJWT(claims).setProtectedHeader({ alg: 'RS256' }).sign(privateKey);
}

test('verifies a real signed setup ID token', async () => {
  expect((await verifyAuth0IdToken(await tokenWithout(), binding)).ok).toBe(true);
});

test.each(['sub', 'exp', 'iat', 'nonce', 'auth_time'])(
  'rejects a signed token missing the required %s claim', async (claim) => {
    expect((await verifyAuth0IdToken(await tokenWithout(claim), binding)).ok).toBe(false);
  },
);
