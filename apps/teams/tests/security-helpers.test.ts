import { describe, expect, it } from 'vitest';
import {
  clearOAuthStateCookie,
  createOAuthStateCookie,
  verifyDoubleSubmitCookie,
} from '../src/cookies.js';
import { OAuthCoreError } from '../src/errors.js';
import { generateOidcNonce, verifyOidcNonce } from '../src/nonce.js';
import { assertPkceChallenge, generatePkcePair, verifyPkceChallenge } from '../src/pkce.js';
import { deterministicRandom } from './helpers.js';

function expectCode(error: unknown, code: OAuthCoreError['code']): boolean {
  return error instanceof OAuthCoreError && error.code === code;
}

describe('PKCE and OIDC helpers', () => {
  it('generates and verifies PKCE S256 while rejecting a verifier mismatch', () => {
    const first = generatePkcePair(deterministicRandom());
    const second = generatePkcePair((size) => new Uint8Array(size).fill(9));

    expect(first.method).toBe('S256');
    expect(verifyPkceChallenge(first.verifier, first.challenge)).toBe(true);
    expect(verifyPkceChallenge(second.verifier, first.challenge)).toBe(false);
    expect(() => assertPkceChallenge(second.verifier, first.challenge)).toThrowError(OAuthCoreError);
    try {
      assertPkceChallenge(second.verifier, first.challenge);
    } catch (error) {
      expect(expectCode(error, 'PKCE_MISMATCH')).toBe(true);
    }
  });

  it('generates a 256-bit OIDC nonce and compares it in constant time', () => {
    const nonce = generateOidcNonce(deterministicRandom());
    const other = generateOidcNonce((size) => new Uint8Array(size).fill(7));
    expect(nonce).toHaveLength(43);
    expect(verifyOidcNonce(nonce, nonce)).toBe(true);
    expect(verifyOidcNonce(other, nonce)).toBe(false);
  });

  it('maps an invalid nonce random source to the stable OAuth error contract', () => {
    let thrown: unknown;
    try {
      generateOidcNonce(() => new Uint8Array());
    } catch (error) {
      thrown = error;
    }
    expect(expectCode(thrown, 'INVALID_INPUT')).toBe(true);
  });
});

describe('double-submit cookie contract', () => {
  const state = Buffer.alloc(32, 5).toString('base64url');
  const otherState = Buffer.alloc(32, 6).toString('base64url');

  it('fixes Secure, HttpOnly, SameSite=Lax, path, and five-minute lifetime', () => {
    expect(createOAuthStateCookie(state)).toEqual({
      name: 'qurl_teams_oauth_state',
      value: state,
      secure: true,
      httpOnly: true,
      sameSite: 'Lax',
      path: '/oauth/qurl',
      maxAgeSeconds: 300,
    });
    expect(clearOAuthStateCookie()).toMatchObject({ value: '', maxAgeSeconds: 0, secure: true });
  });

  it('fails closed when the cookie is absent or mismatched', () => {
    expect(() => verifyDoubleSubmitCookie(undefined, state)).toThrowError(OAuthCoreError);
    expect(() => verifyDoubleSubmitCookie(otherState, state)).toThrowError(OAuthCoreError);
    expect(() => verifyDoubleSubmitCookie(state, state)).not.toThrow();
    try {
      verifyDoubleSubmitCookie(otherState, state);
    } catch (error) {
      expect(expectCode(error, 'COOKIE_MISMATCH')).toBe(true);
    }
  });
});
