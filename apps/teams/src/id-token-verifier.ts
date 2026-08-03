import {
  createLocalJWKSet,
  decodeProtectedHeader,
  errors as joseErrors,
  jwtVerify,
} from 'jose';
import type { JSONWebKeySet, JWTVerifyGetKey } from 'jose';
import { OAuthCoreError, isOAuthCoreError } from './errors.js';
import { decodeUtf8, readBoundedBody, withStrictTimeout } from './http.js';
import type { Clock, FetchLike, IdTokenVerifier, Logger, VerifiedIdentity } from './interfaces.js';
import { nullLogger, systemClock } from './interfaces.js';
import { RedactingLogger } from './logger.js';
import { verifyOidcNonce } from './nonce.js';
import { normalizeEmail } from './state.js';

const DEFAULT_JWKS_TIMEOUT_MS = 5_000;
const DEFAULT_JWKS_BODY_LIMIT_BYTES = 64 * 1_024;
const DEFAULT_JWKS_CACHE_SECONDS = 5 * 60;
const MAX_JWKS_KEYS = 16;
const MAX_ID_TOKEN_BYTES = 16 * 1_024;

export interface IdTokenVerifierOptions {
  readonly issuer: string;
  /** Auth0 client ID used as the ID-token `aud`, not the qURL API audience. */
  readonly audience: string;
  readonly fetch: FetchLike;
  readonly clock?: Clock;
  readonly logger?: Logger;
  readonly timeoutMs?: number;
  readonly responseBodyLimitBytes?: number;
  readonly cacheSeconds?: number;
}

interface CachedJwks {
  readonly resolver: JWTVerifyGetKey;
  readonly expiresAtEpochSeconds: number;
}

function validateIssuer(value: string): URL {
  let issuer: URL;
  try {
    issuer = new URL(value);
  } catch {
    throw new OAuthCoreError('INVALID_INPUT', 'ID-token issuer must be a valid URL.');
  }
  if (issuer.protocol !== 'https:' || issuer.username !== '' || issuer.password !== ''
    || issuer.pathname !== '/' || issuer.search || issuer.hash) {
    throw new OAuthCoreError('INVALID_INPUT', 'ID-token issuer must be a root HTTPS URL without credentials.');
  }
  return issuer;
}

export function parseJwks(body: Uint8Array): JSONWebKeySet {
  let parsed: unknown;
  try {
    parsed = JSON.parse(decodeUtf8(body));
  } catch {
    throw new OAuthCoreError('JWKS_UNAVAILABLE', 'JWKS response was not valid JSON.', { retryable: true });
  }
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
    throw new OAuthCoreError('JWKS_UNAVAILABLE', 'JWKS response was invalid.', { retryable: true });
  }
  const keys = (parsed as { readonly keys?: unknown }).keys;
  if (!Array.isArray(keys) || keys.length < 1 || keys.length > MAX_JWKS_KEYS) {
    throw new OAuthCoreError('JWKS_UNAVAILABLE', 'JWKS response contained an invalid key set.', { retryable: true });
  }
  const keyIds = new Set<string>();
  for (const key of keys) {
    if (key === null || typeof key !== 'object' || Array.isArray(key)) {
      throw new OAuthCoreError('JWKS_UNAVAILABLE', 'JWKS response contained an invalid key.', { retryable: true });
    }
    const candidate = key as Record<string, unknown>;
    // RFC 7517 makes JWK `alg` optional. A missing value is safe here because
    // the JWT protected header is pinned to RS256 and jwtVerify also allowlists
    // only RS256; present metadata is rejected when it contradicts those pins.
    if (candidate.kty !== 'RSA'
      || (candidate.alg !== undefined && candidate.alg !== 'RS256')
      || typeof candidate.kid !== 'string' || candidate.kid === ''
      || typeof candidate.n !== 'string' || candidate.n === ''
      || typeof candidate.e !== 'string' || candidate.e === ''
      || (candidate.use !== undefined && candidate.use !== 'sig')
      || keyIds.has(candidate.kid)) {
      throw new OAuthCoreError('JWKS_UNAVAILABLE', 'JWKS response contained an unsupported key.', { retryable: true });
    }
    keyIds.add(candidate.kid);
  }
  return { keys } as JSONWebKeySet;
}

export function createIdTokenVerifier(options: IdTokenVerifierOptions): IdTokenVerifier {
  const issuerUrl = validateIssuer(options.issuer);
  if (!options.audience) {
    throw new OAuthCoreError('INVALID_INPUT', 'ID-token audience is required.');
  }
  const clock = options.clock ?? systemClock;
  const logger = new RedactingLogger(options.logger ?? nullLogger);
  const timeoutMs = options.timeoutMs ?? DEFAULT_JWKS_TIMEOUT_MS;
  const bodyLimit = options.responseBodyLimitBytes ?? DEFAULT_JWKS_BODY_LIMIT_BYTES;
  const cacheSeconds = options.cacheSeconds ?? DEFAULT_JWKS_CACHE_SECONDS;
  if (!Number.isSafeInteger(cacheSeconds) || cacheSeconds < 1) {
    throw new OAuthCoreError('INVALID_INPUT', 'JWKS cache duration is invalid.');
  }
  const jwksUrl = new URL('.well-known/jwks.json', issuerUrl);
  let cache: CachedJwks | undefined;
  let inFlightRefresh: Promise<CachedJwks> | undefined;

  function now(): number {
    const value = clock.now();
    if (!Number.isSafeInteger(value) || value < 0) {
      throw new OAuthCoreError('INVALID_INPUT', 'Clock returned an invalid time.');
    }
    return value;
  }

  async function refresh(): Promise<CachedJwks> {
    if (inFlightRefresh !== undefined) {
      return inFlightRefresh;
    }
    inFlightRefresh = withStrictTimeout(timeoutMs, 'JWKS_TIMEOUT', async (signal) => {
      let response: Response;
      try {
        response = await options.fetch(jwksUrl, {
          method: 'GET',
          headers: { Accept: 'application/json' },
          redirect: 'error',
          signal,
        });
      } catch {
        throw new OAuthCoreError('JWKS_UNAVAILABLE', 'JWKS endpoint could not be reached.', { retryable: true });
      }
      const body = await readBoundedBody(response, bodyLimit, 'JWKS_RESPONSE_TOO_LARGE');
      if (!response.ok) {
        throw new OAuthCoreError('JWKS_UNAVAILABLE', 'JWKS endpoint rejected the request.', {
          retryable: true,
          safeDetails: { status: response.status },
        });
      }
      const refreshed = {
        resolver: createLocalJWKSet(parseJwks(body)),
        expiresAtEpochSeconds: now() + cacheSeconds,
      };
      cache = refreshed;
      return refreshed;
    }).finally(() => {
      inFlightRefresh = undefined;
    });
    return inFlightRefresh;
  }

  async function getCachedResolver(): Promise<{ readonly resolver: JWTVerifyGetKey; readonly fromCache: boolean }> {
    if (cache !== undefined && now() < cache.expiresAtEpochSeconds) {
      return { resolver: cache.resolver, fromCache: true };
    }
    const refreshed = await refresh();
    return { resolver: refreshed.resolver, fromCache: false };
  }

  async function verifyCryptographically(idToken: string): Promise<Record<string, unknown>> {
    if (!idToken || Buffer.byteLength(idToken, 'utf8') > MAX_ID_TOKEN_BYTES) {
      throw new OAuthCoreError('ID_TOKEN_INVALID', 'ID token is invalid.');
    }
    let header;
    try {
      header = decodeProtectedHeader(idToken);
    } catch {
      throw new OAuthCoreError('ID_TOKEN_INVALID', 'ID token is invalid.');
    }
    if (typeof header.kid !== 'string' || header.kid === '' || header.alg !== 'RS256') {
      throw new OAuthCoreError('ID_TOKEN_INVALID', 'ID token header is invalid.');
    }

    const cached = await getCachedResolver();
    const verifyWith = async (resolver: JWTVerifyGetKey): Promise<Record<string, unknown>> => {
      const result = await jwtVerify(idToken, resolver, {
        algorithms: ['RS256'],
        issuer: issuerUrl.toString(),
        audience: options.audience,
        // Match the shipped Discord/Auth0 boundary: production clock drift is
        // expected to be negligible, so temporal claims get no grace period.
        clockTolerance: 0,
        currentDate: new Date(now() * 1_000),
        requiredClaims: ['exp'],
      });
      return result.payload;
    };

    try {
      return await verifyWith(cached.resolver);
    } catch (error) {
      if (cached.fromCache && error instanceof joseErrors.JWKSNoMatchingKey) {
        logger.info('Cached JWKS did not contain the signing key; refreshing once.');
        const refreshed = await refresh();
        return verifyWith(refreshed.resolver);
      }
      throw error;
    }
  }

  return {
    async verify(idToken, expected): Promise<VerifiedIdentity> {
      let payload: Record<string, unknown>;
      try {
        payload = await verifyCryptographically(idToken);
      } catch (error) {
        if (isOAuthCoreError(error)) {
          throw error;
        }
        if (error instanceof joseErrors.JWTExpired) {
          throw new OAuthCoreError('ID_TOKEN_EXPIRED', 'ID token has expired.');
        }
        throw new OAuthCoreError('ID_TOKEN_INVALID', 'ID token verification failed.');
      }

      if (typeof payload.nonce !== 'string' || !verifyOidcNonce(payload.nonce, expected.nonce)) {
        throw new OAuthCoreError('ID_TOKEN_NONCE_MISMATCH', 'ID token nonce did not match the setup transaction.');
      }
      if (typeof payload.sub !== 'string' || payload.sub === '') {
        throw new OAuthCoreError('ID_TOKEN_SUBJECT_MISSING', 'ID token subject is missing.');
      }
      if (typeof payload.email !== 'string' || payload.email === '') {
        throw new OAuthCoreError('ID_TOKEN_EMAIL_MISSING', 'ID token email is missing.');
      }
      if (payload.email_verified !== true) {
        throw new OAuthCoreError('ID_TOKEN_EMAIL_UNVERIFIED', 'ID token email is not verified.');
      }

      let email: string;
      let expectedEmail: string;
      try {
        email = normalizeEmail(payload.email);
        expectedEmail = normalizeEmail(expected.normalizedEmail);
      } catch {
        throw new OAuthCoreError('ID_TOKEN_EMAIL_MISMATCH', 'ID token email did not match the setup transaction.');
      }
      if (email !== expectedEmail) {
        throw new OAuthCoreError('ID_TOKEN_EMAIL_MISMATCH', 'ID token email did not match the setup transaction.');
      }
      return { subject: payload.sub, email };
    },
  };
}
