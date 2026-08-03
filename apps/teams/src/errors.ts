export type OAuthCoreErrorCode =
  | 'INVALID_INPUT'
  | 'INVALID_STATE'
  | 'STATE_COLLISION'
  | 'STATE_NOT_FOUND'
  | 'STATE_EXPIRED'
  | 'STATE_STORE_FAILED'
  | 'COOKIE_MISMATCH'
  | 'PKCE_MISMATCH'
  | 'TOKEN_TIMEOUT'
  | 'TOKEN_RESPONSE_TOO_LARGE'
  | 'TOKEN_ENDPOINT_REJECTED'
  | 'TOKEN_INVALID_RESPONSE'
  | 'JWKS_TIMEOUT'
  | 'JWKS_RESPONSE_TOO_LARGE'
  | 'JWKS_UNAVAILABLE'
  | 'ID_TOKEN_INVALID'
  | 'ID_TOKEN_EXPIRED'
  | 'ID_TOKEN_NONCE_MISMATCH'
  | 'ID_TOKEN_SUBJECT_MISSING'
  | 'ID_TOKEN_EMAIL_MISSING'
  | 'ID_TOKEN_EMAIL_UNVERIFIED'
  | 'ID_TOKEN_EMAIL_MISMATCH'
  | 'BINDING_FAILED';

interface OAuthCoreErrorOptions {
  readonly retryable?: boolean;
  readonly safeDetails?: Readonly<Record<string, string | number | boolean>>;
}

/**
 * A stable, secret-free failure that route wiring may map to browser copy.
 * Raw upstream bodies and caught errors must never be attached as a cause.
 */
export class OAuthCoreError extends Error {
  readonly code: OAuthCoreErrorCode;
  readonly retryable: boolean;
  readonly safeDetails: Readonly<Record<string, string | number | boolean>>;

  constructor(code: OAuthCoreErrorCode, message: string, options: OAuthCoreErrorOptions = {}) {
    super(message);
    this.name = 'OAuthCoreError';
    this.code = code;
    this.retryable = options.retryable ?? false;
    this.safeDetails = options.safeDetails ?? {};
  }
}

export function isOAuthCoreError(error: unknown): error is OAuthCoreError {
  return error instanceof OAuthCoreError;
}

