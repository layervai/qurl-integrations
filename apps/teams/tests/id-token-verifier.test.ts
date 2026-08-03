import { exportJWK, generateKeyPair, SignJWT } from 'jose';
import type { JWK } from 'jose';
import { beforeAll, describe, expect, it } from 'vitest';
import { OAuthCoreError } from '../src/errors.js';
import { createIdTokenVerifier, parseJwks } from '../src/id-token-verifier.js';
import type { FetchLike } from '../src/interfaces.js';
import { TEST_NOW, fixedClock } from './helpers.js';

const ISSUER = 'https://auth.example.com/';
const AUDIENCE = 'synthetic-teams-client';
const NONCE = Buffer.alloc(32, 14).toString('base64url');
const KEY_ID = 'synthetic-signing-key';

let privateKey: CryptoKey;
let publicJwk: JWK;

beforeAll(async () => {
  const pair = await generateKeyPair('RS256');
  privateKey = pair.privateKey;
  publicJwk = {
    ...await exportJWK(pair.publicKey),
    kid: KEY_ID,
    alg: 'RS256',
    use: 'sig',
  };
});

async function signToken(overrides: {
  readonly nonce?: string;
  readonly email?: string;
  readonly emailVerified?: boolean;
  readonly subject?: string;
  readonly issuer?: string;
  readonly audience?: string;
  readonly expiresAt?: number;
  readonly includeEmail?: boolean;
  readonly includeSubject?: boolean;
  readonly signingKey?: CryptoKey;
  readonly keyId?: string;
} = {}): Promise<string> {
  const payload: Record<string, unknown> = {
    nonce: overrides.nonce ?? NONCE,
    email_verified: overrides.emailVerified ?? true,
  };
  if (overrides.includeEmail !== false) {
    payload.email = overrides.email ?? 'admin@example.com';
  }
  let token = new SignJWT(payload)
    .setProtectedHeader({ alg: 'RS256', kid: overrides.keyId ?? KEY_ID, typ: 'JWT' })
    .setIssuer(overrides.issuer ?? ISSUER)
    .setAudience(overrides.audience ?? AUDIENCE)
    .setIssuedAt(TEST_NOW)
    .setExpirationTime(overrides.expiresAt ?? TEST_NOW + 60);
  if (overrides.includeSubject !== false) {
    token = token.setSubject(overrides.subject ?? 'provider|synthetic-user');
  }
  return token.sign(overrides.signingKey ?? privateKey);
}

function jwksResponse(keys: readonly unknown[] = [publicJwk]): Response {
  return new Response(JSON.stringify({ keys }), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  });
}

function expectCode(error: unknown, code: OAuthCoreError['code']): boolean {
  return error instanceof OAuthCoreError && error.code === code;
}

describe('ID-token verification', () => {
  it('rejects a pathed issuer before any JWKS request', () => {
    let fetches = 0;
    expect(() => createIdTokenVerifier({
      issuer: 'https://auth.example.com/tenant/',
      audience: AUDIENCE,
      fetch: async () => {
        fetches += 1;
        return jwksResponse();
      },
    })).toThrowError(expect.objectContaining({ code: 'INVALID_INPUT' }));
    expect(fetches).toBe(0);
  });

  it('verifies signature, issuer, audience, expiry, nonce, subject, and email in one cached step', async () => {
    let fetches = 0;
    const fetch: FetchLike = async (input) => {
      fetches += 1;
      expect(new URL(input).toString()).toBe('https://auth.example.com/.well-known/jwks.json');
      return jwksResponse();
    };
    const verifier = createIdTokenVerifier({
      issuer: ISSUER,
      audience: AUDIENCE,
      fetch,
      clock: fixedClock(),
    });

    await expect(verifier.verify(await signToken(), {
      nonce: NONCE,
      normalizedEmail: 'admin@example.com',
    })).resolves.toEqual({
      subject: 'provider|synthetic-user',
      email: 'admin@example.com',
    });
    await expect(verifier.verify(await signToken({ subject: 'provider|synthetic-user-2' }), {
      nonce: NONCE,
      normalizedEmail: 'admin@example.com',
    })).resolves.toMatchObject({ subject: 'provider|synthetic-user-2' });
    expect(fetches).toBe(1);
  });

  it('accepts an RSA signing JWK without the optional alg member', async () => {
    const jwkWithoutAlg: JWK = { ...publicJwk };
    delete jwkWithoutAlg.alg;
    const verifier = createIdTokenVerifier({
      issuer: ISSUER,
      audience: AUDIENCE,
      fetch: async () => jwksResponse([jwkWithoutAlg]),
      clock: fixedClock(),
    });

    await expect(verifier.verify(await signToken(), {
      nonce: NONCE,
      normalizedEmail: 'admin@example.com',
    })).resolves.toEqual({
      subject: 'provider|synthetic-user',
      email: 'admin@example.com',
    });
  });

  it.each([
    ['a non-RSA key', () => [{ ...publicJwk, kty: 'EC' }]],
    ['duplicate key IDs', () => [{ ...publicJwk }, { ...publicJwk }]],
    ['an empty key set', () => []],
  ] as const)('rejects %s at the JWKS parsing boundary', (_description, keys) => {
    const body = new TextEncoder().encode(JSON.stringify({ keys: keys() }));
    let thrown: unknown;
    try {
      parseJwks(body);
    } catch (error) {
      thrown = error;
    }
    expect(expectCode(thrown, 'JWKS_UNAVAILABLE')).toBe(true);
  });

  it('maps invalid UTF-8 at the JWKS boundary to a retryable JWKS error', () => {
    let thrown: unknown;
    try {
      parseJwks(Uint8Array.of(0xff));
    } catch (error) {
      thrown = error;
    }
    expect(expectCode(thrown, 'JWKS_UNAVAILABLE')).toBe(true);
    expect((thrown as OAuthCoreError).retryable).toBe(true);
  });

  it('fails closed on nonce mismatch', async () => {
    const verifier = createIdTokenVerifier({
      issuer: ISSUER,
      audience: AUDIENCE,
      fetch: async () => jwksResponse(),
      clock: fixedClock(),
    });
    const wrongNonce = Buffer.alloc(32, 15).toString('base64url');
    await expect(verifier.verify(await signToken({ nonce: wrongNonce }), {
      nonce: NONCE,
      normalizedEmail: 'admin@example.com',
    })).rejects.toSatisfy((error: unknown) => expectCode(error, 'ID_TOKEN_NONCE_MISMATCH'));
  });

  it('returns a stable typed failure on verified-email mismatch', async () => {
    const verifier = createIdTokenVerifier({
      issuer: ISSUER,
      audience: AUDIENCE,
      fetch: async () => jwksResponse(),
      clock: fixedClock(),
    });
    await expect(verifier.verify(await signToken({ email: 'other@example.com' }), {
      nonce: NONCE,
      normalizedEmail: 'admin@example.com',
    })).rejects.toSatisfy((error: unknown) => expectCode(error, 'ID_TOKEN_EMAIL_MISMATCH'));
  });

  it('requires email_verified to be the boolean true', async () => {
    const verifier = createIdTokenVerifier({
      issuer: ISSUER,
      audience: AUDIENCE,
      fetch: async () => jwksResponse(),
      clock: fixedClock(),
    });
    await expect(verifier.verify(await signToken({ emailVerified: false }), {
      nonce: NONCE,
      normalizedEmail: 'admin@example.com',
    })).rejects.toSatisfy((error: unknown) => expectCode(error, 'ID_TOKEN_EMAIL_UNVERIFIED'));
  });

  it('requires subject and email claims after cryptographic verification', async () => {
    const verifier = createIdTokenVerifier({
      issuer: ISSUER,
      audience: AUDIENCE,
      fetch: async () => jwksResponse(),
      clock: fixedClock(),
    });
    await expect(verifier.verify(await signToken({ includeSubject: false }), {
      nonce: NONCE,
      normalizedEmail: 'admin@example.com',
    })).rejects.toSatisfy((error: unknown) => expectCode(error, 'ID_TOKEN_SUBJECT_MISSING'));
    await expect(verifier.verify(await signToken({ includeEmail: false }), {
      nonce: NONCE,
      normalizedEmail: 'admin@example.com',
    })).rejects.toSatisfy((error: unknown) => expectCode(error, 'ID_TOKEN_EMAIL_MISSING'));
  });

  it('rejects expired and wrong-issuer tokens after signature verification', async () => {
    const verifier = createIdTokenVerifier({
      issuer: ISSUER,
      audience: AUDIENCE,
      fetch: async () => jwksResponse(),
      clock: fixedClock(),
    });
    await expect(verifier.verify(await signToken({ expiresAt: TEST_NOW - 1 }), {
      nonce: NONCE,
      normalizedEmail: 'admin@example.com',
    })).rejects.toSatisfy((error: unknown) => expectCode(error, 'ID_TOKEN_EXPIRED'));
    await expect(verifier.verify(await signToken({ issuer: 'https://other-auth.example.com/' }), {
      nonce: NONCE,
      normalizedEmail: 'admin@example.com',
    })).rejects.toSatisfy((error: unknown) => expectCode(error, 'ID_TOKEN_INVALID'));
    await expect(verifier.verify(await signToken({ audience: 'other-synthetic-client' }), {
      nonce: NONCE,
      normalizedEmail: 'admin@example.com',
    })).rejects.toSatisfy((error: unknown) => expectCode(error, 'ID_TOKEN_INVALID'));
  });

  it('refreshes a live cache once when the token uses a rotated signing key', async () => {
    const rotatedPair = await generateKeyPair('RS256');
    const rotatedJwk: JWK = {
      ...await exportJWK(rotatedPair.publicKey),
      kid: 'synthetic-rotated-key',
      alg: 'RS256',
      use: 'sig',
    };
    let fetches = 0;
    const verifier = createIdTokenVerifier({
      issuer: ISSUER,
      audience: AUDIENCE,
      fetch: async () => {
        fetches += 1;
        return new Response(JSON.stringify({ keys: [fetches === 1 ? publicJwk : rotatedJwk] }));
      },
      clock: fixedClock(),
    });
    await verifier.verify(await signToken(), {
      nonce: NONCE,
      normalizedEmail: 'admin@example.com',
    });
    await expect(verifier.verify(await signToken({
      signingKey: rotatedPair.privateKey,
      keyId: 'synthetic-rotated-key',
      subject: 'provider|synthetic-rotated-user',
    }), {
      nonce: NONCE,
      normalizedEmail: 'admin@example.com',
    })).resolves.toMatchObject({ subject: 'provider|synthetic-rotated-user' });
    expect(fetches).toBe(2);
  });

  it('bounds JWKS response bodies', async () => {
    const verifier = createIdTokenVerifier({
      issuer: ISSUER,
      audience: AUDIENCE,
      fetch: async () => new Response(JSON.stringify({ keys: [], padding: 'x'.repeat(256) })),
      clock: fixedClock(),
      responseBodyLimitBytes: 64,
    });
    await expect(verifier.verify(await signToken(), {
      nonce: NONCE,
      normalizedEmail: 'admin@example.com',
    })).rejects.toSatisfy((error: unknown) => expectCode(error, 'JWKS_RESPONSE_TOO_LARGE'));
  });

  it('enforces the JWKS fetch timeout with a stable retryable code', async () => {
    const verifier = createIdTokenVerifier({
      issuer: ISSUER,
      audience: AUDIENCE,
      fetch: () => new Promise<Response>(() => undefined),
      clock: fixedClock(),
      timeoutMs: 10,
    });
    let thrown: unknown;
    try {
      await verifier.verify(await signToken(), {
        nonce: NONCE,
        normalizedEmail: 'admin@example.com',
      });
    } catch (error) {
      thrown = error;
    }
    expect(expectCode(thrown, 'JWKS_TIMEOUT')).toBe(true);
    expect((thrown as OAuthCoreError).retryable).toBe(true);
  });
});
