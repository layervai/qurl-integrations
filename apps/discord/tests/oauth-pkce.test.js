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
    expect(isPkceVerifier('a'.repeat(129))).toBe(false);
    expect(() => pkceChallengeForVerifier('short')).toThrow(/code_verifier/);
    expect(() => pkceChallengeForVerifier('a'.repeat(129))).toThrow(/code_verifier/);
  });

  it('accepts verifier length boundaries from RFC 7636', () => {
    const minLengthVerifier = 'a'.repeat(43);
    const maxLengthVerifier = 'b'.repeat(128);

    expect(isPkceVerifier(minLengthVerifier)).toBe(true);
    expect(isPkceVerifier(maxLengthVerifier)).toBe(true);
    expect(pkceChallengeForVerifier(minLengthVerifier)).toHaveLength(43);
    expect(pkceChallengeForVerifier(maxLengthVerifier)).toHaveLength(43);
  });
});
