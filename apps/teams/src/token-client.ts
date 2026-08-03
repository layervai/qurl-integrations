import { OAuthCoreError, isOAuthCoreError } from './errors.js';
import { decodeUtf8, readBoundedBody, withStrictTimeout } from './http.js';
import type {
  ConfidentialTokenClient,
  EphemeralTokenSet,
  FetchLike,
  Logger,
  TokenExchangeRequest,
} from './interfaces.js';
import { nullLogger } from './interfaces.js';
import { RedactingLogger } from './logger.js';
import { isOidcNonce } from './nonce.js';
import { isPkceVerifier } from './pkce.js';
import { isOpaqueStateHandle, normalizeEmail } from './state.js';

export const AUTHORIZATION_SCOPES = Object.freeze([
  'openid',
  'email',
  'qurl:read',
  'qurl:write',
] as const);
export const AUTHORIZATION_SCOPE = AUTHORIZATION_SCOPES.join(' ');

const DEFAULT_TOKEN_TIMEOUT_MS = 15_000;
const DEFAULT_TOKEN_BODY_LIMIT_BYTES = 8 * 1_024;
const PKCE_CHALLENGE_PATTERN = /^[A-Za-z0-9_-]{43}$/;

export interface ConfidentialTokenClientOptions {
  readonly issuer: string;
  readonly clientId: string;
  readonly clientSecret: string;
  readonly clientSecretFallback?: string;
  readonly audience: string;
  readonly redirectUri: string;
  readonly fetch: FetchLike;
  readonly logger?: Logger;
  readonly timeoutMs?: number;
  readonly responseBodyLimitBytes?: number;
}

interface OAuthErrorBody {
  readonly error?: unknown;
}

interface TokenBody {
  readonly access_token?: unknown;
  readonly id_token?: unknown;
}

function requireHttpsUrl(value: string, label: string): URL {
  let parsed: URL;
  try {
    parsed = new URL(value);
  } catch {
    throw new OAuthCoreError('INVALID_INPUT', `${label} must be a valid URL.`);
  }
  if (parsed.protocol !== 'https:' || parsed.username !== '' || parsed.password !== '') {
    throw new OAuthCoreError('INVALID_INPUT', `${label} must be an HTTPS URL without credentials.`);
  }
  return parsed;
}

function parseJsonObject(body: Uint8Array): Record<string, unknown> | undefined {
  try {
    const parsed: unknown = JSON.parse(decodeUtf8(body));
    if (parsed !== null && typeof parsed === 'object' && !Array.isArray(parsed)) {
      return parsed as Record<string, unknown>;
    }
  } catch {
    return undefined;
  }
  return undefined;
}

function endpointFor(issuer: URL, path: string): URL {
  const normalized = new URL(issuer.toString());
  normalized.pathname = path;
  normalized.search = '';
  normalized.hash = '';
  return normalized;
}

function safeOAuthError(value: string | undefined): string {
  return value !== undefined && /^[A-Za-z][A-Za-z0-9_]{0,63}$/.test(value)
    ? value
    : 'unknown_oauth_error';
}

export function createConfidentialTokenClient(options: ConfidentialTokenClientOptions): ConfidentialTokenClient {
  const issuer = requireHttpsUrl(options.issuer, 'Issuer');
  const redirectUri = requireHttpsUrl(options.redirectUri, 'Redirect URI');
  if (issuer.pathname !== '/' || issuer.search !== '' || issuer.hash !== '' || redirectUri.hash !== '') {
    throw new OAuthCoreError('INVALID_INPUT', 'OAuth endpoint URLs have an invalid shape.');
  }
  if (!options.clientId || !options.clientSecret || !options.audience) {
    throw new OAuthCoreError('INVALID_INPUT', 'OAuth client configuration is incomplete.');
  }
  if (options.clientSecretFallback === options.clientSecret) {
    throw new OAuthCoreError('INVALID_INPUT', 'Fallback client secret must differ from the current secret.');
  }
  const timeoutMs = options.timeoutMs ?? DEFAULT_TOKEN_TIMEOUT_MS;
  const bodyLimit = options.responseBodyLimitBytes ?? DEFAULT_TOKEN_BODY_LIMIT_BYTES;
  const logger = new RedactingLogger(
    options.logger ?? nullLogger,
    [options.clientSecret, options.clientSecretFallback ?? ''],
  );
  const authorizeUrl = endpointFor(issuer, '/authorize');
  const tokenUrl = endpointFor(issuer, '/oauth/token');

  async function attempt(
    request: TokenExchangeRequest,
    clientSecret: string,
    signal: AbortSignal,
  ): Promise<{ readonly result?: EphemeralTokenSet; readonly oauthError?: string; readonly status?: number }> {
    const body = new URLSearchParams({
      grant_type: 'authorization_code',
      client_id: options.clientId,
      client_secret: clientSecret,
      code: request.code,
      code_verifier: request.codeVerifier,
      redirect_uri: redirectUri.toString(),
    });

    let response: Response;
    try {
      response = await options.fetch(tokenUrl, {
        method: 'POST',
        headers: {
          Accept: 'application/json',
          'Content-Type': 'application/x-www-form-urlencoded',
        },
        body,
        redirect: 'error',
        signal,
      });
    } catch {
      throw new OAuthCoreError('TOKEN_ENDPOINT_REJECTED', 'OAuth token endpoint could not be reached.', {
        retryable: true,
      });
    }

    const responseBody = await readBoundedBody(response, bodyLimit, 'TOKEN_RESPONSE_TOO_LARGE');
    const parsed = parseJsonObject(responseBody);
    if (!response.ok) {
      const oauthBody = parsed as OAuthErrorBody | undefined;
      const oauthError = typeof oauthBody?.error === 'string' ? oauthBody.error : undefined;
      return {
        ...(oauthError === undefined ? {} : { oauthError }),
        status: response.status,
      };
    }
    const tokenBody = parsed as TokenBody | undefined;
    if (typeof tokenBody?.access_token !== 'string' || tokenBody.access_token === ''
      || typeof tokenBody.id_token !== 'string' || tokenBody.id_token === '') {
      throw new OAuthCoreError('TOKEN_INVALID_RESPONSE', 'OAuth token response was incomplete.');
    }
    return { result: { accessToken: tokenBody.access_token, idToken: tokenBody.id_token } };
  }

  return {
    createAuthorizationUrl(input): URL {
      if (!isOpaqueStateHandle(input.state)
        || !PKCE_CHALLENGE_PATTERN.test(input.codeChallenge)
        || !isOidcNonce(input.nonce)) {
        throw new OAuthCoreError('INVALID_INPUT', 'Authorization request is invalid.');
      }
      const url = new URL(authorizeUrl);
      url.searchParams.set('response_type', 'code');
      url.searchParams.set('client_id', options.clientId);
      url.searchParams.set('redirect_uri', redirectUri.toString());
      url.searchParams.set('audience', options.audience);
      url.searchParams.set('scope', AUTHORIZATION_SCOPE);
      url.searchParams.set('state', input.state);
      url.searchParams.set('code_challenge', input.codeChallenge);
      url.searchParams.set('code_challenge_method', 'S256');
      url.searchParams.set('nonce', input.nonce);
      if (input.loginHint !== undefined) {
        url.searchParams.set('login_hint', normalizeEmail(input.loginHint));
      }
      return url;
    },

    async exchangeAuthorizationCode(request): Promise<EphemeralTokenSet> {
      if (!request.code || request.code.length > 4_096 || !isPkceVerifier(request.codeVerifier)) {
        throw new OAuthCoreError('INVALID_INPUT', 'Authorization code exchange input is invalid.');
      }
      try {
        return await withStrictTimeout(timeoutMs, 'TOKEN_TIMEOUT', async (signal) => {
          const primary = await attempt(request, options.clientSecret, signal);
          if (primary.result !== undefined) {
            return primary.result;
          }
          const fallback = options.clientSecretFallback;
          if (primary.oauthError === 'invalid_client' && fallback) {
            logger.warn('OAuth client authentication failed; retrying with the configured fallback.', {
              errorCode: 'invalid_client',
              status: primary.status ?? 0,
            });
            if (signal.aborted) {
              throw new OAuthCoreError('TOKEN_TIMEOUT', 'OAuth provider request timed out.', { retryable: true });
            }
            const rotated = await attempt(request, fallback, signal);
            if (rotated.result !== undefined) {
              return rotated.result;
            }
            throw new OAuthCoreError('TOKEN_ENDPOINT_REJECTED', 'OAuth token endpoint rejected client authentication.', {
              safeDetails: {
                errorCode: safeOAuthError(rotated.oauthError),
                status: rotated.status ?? 0,
              },
            });
          }
          throw new OAuthCoreError('TOKEN_ENDPOINT_REJECTED', 'OAuth token endpoint rejected the authorization code.', {
            safeDetails: {
              errorCode: safeOAuthError(primary.oauthError),
              status: primary.status ?? 0,
            },
          });
        });
      } catch (error) {
        if (isOAuthCoreError(error)) {
          throw error;
        }
        throw new OAuthCoreError('TOKEN_ENDPOINT_REJECTED', 'OAuth token exchange failed.', { retryable: true });
      }
    },
  };
}
