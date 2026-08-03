import { randomBytes } from 'node:crypto';
import { base64UrlEncode, constantTimeEqual } from './encoding.js';
import { OAuthCoreError } from './errors.js';
import type { RandomBytes } from './pkce.js';

const OIDC_NONCE_PATTERN = /^[A-Za-z0-9_-]{43}$/;

export function isOidcNonce(value: unknown): value is string {
  return typeof value === 'string' && OIDC_NONCE_PATTERN.test(value);
}

export function generateOidcNonce(random: RandomBytes = randomBytes): string {
  const nonce = base64UrlEncode(random(32));
  if (!isOidcNonce(nonce)) {
    throw new OAuthCoreError('INVALID_INPUT', 'Random source returned an invalid OIDC nonce.');
  }
  return nonce;
}

export function verifyOidcNonce(actual: string, expected: string): boolean {
  return isOidcNonce(actual) && isOidcNonce(expected) && constantTimeEqual(actual, expected);
}
