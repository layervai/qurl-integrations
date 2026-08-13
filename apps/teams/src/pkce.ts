import { createHash, randomBytes } from 'node:crypto';
import { OAuthCoreError } from './errors.js';
import { base64UrlEncode, constantTimeEqual } from './encoding.js';

const PKCE_VERIFIER_PATTERN = /^[A-Za-z0-9._~-]{43,128}$/;
const PKCE_CHALLENGE_PATTERN = /^[A-Za-z0-9_-]{43}$/;

export type RandomBytes = (size: number) => Uint8Array;

export interface PkcePair {
  readonly verifier: string;
  readonly challenge: string;
  readonly method: 'S256';
}

export function isPkceVerifier(value: unknown): value is string {
  return typeof value === 'string' && PKCE_VERIFIER_PATTERN.test(value);
}

export function pkceChallengeForVerifier(verifier: string): string {
  if (!isPkceVerifier(verifier)) {
    throw new OAuthCoreError('INVALID_INPUT', 'PKCE verifier has an invalid shape.');
  }
  return createHash('sha256').update(verifier, 'utf8').digest('base64url');
}

export function generatePkcePair(random: RandomBytes = randomBytes): PkcePair {
  const verifier = base64UrlEncode(random(32));
  if (!isPkceVerifier(verifier)) {
    throw new OAuthCoreError('INVALID_INPUT', 'Random source returned an invalid PKCE value.');
  }
  return { verifier, challenge: pkceChallengeForVerifier(verifier), method: 'S256' };
}

export function verifyPkceChallenge(verifier: string, expectedChallenge: string): boolean {
  if (!isPkceVerifier(verifier) || !PKCE_CHALLENGE_PATTERN.test(expectedChallenge)) {
    return false;
  }
  return constantTimeEqual(pkceChallengeForVerifier(verifier), expectedChallenge);
}

export function assertPkceChallenge(verifier: string, expectedChallenge: string): void {
  if (!verifyPkceChallenge(verifier, expectedChallenge)) {
    throw new OAuthCoreError('PKCE_MISMATCH', 'PKCE verifier did not match the authorization request.');
  }
}
