const {
  createPkcePair,
  isPkceVerifier,
  pkceChallengeForVerifier,
} = require('../src/utils/oauth-pkce');

describe('utils/oauth-pkce', () => {
  it('computes the RFC 7636 S256 challenge vector', () => {
    const verifier = 'dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk';
    expect(pkceChallengeForVerifier(verifier)).toBe('E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM');
  });

  it('generates a verifier/challenge pair with a valid verifier shape', () => {
    const { codeVerifier, codeChallenge } = createPkcePair();
    expect(isPkceVerifier(codeVerifier)).toBe(true);
    expect(codeChallenge).toBe(pkceChallengeForVerifier(codeVerifier));
    expect(codeChallenge).not.toBe(codeVerifier);
  });

  it('rejects malformed verifiers before token exchange', () => {
    expect(isPkceVerifier('short')).toBe(false);
    expect(() => pkceChallengeForVerifier('short')).toThrow(/code_verifier/);
  });
});
