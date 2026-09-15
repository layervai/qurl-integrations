import type { OAuthCoreError } from './errors.js';

/** Epoch seconds. Implementations should return an integer. */
export interface Clock {
  now(): number;
}

export const systemClock: Clock = {
  now: () => Math.floor(Date.now() / 1_000),
};

export type FetchLike = (input: string | URL, init?: RequestInit) => Promise<Response>;

export interface LogContext {
  readonly [key: string]: unknown;
}

export interface Logger {
  debug(message: string, context?: LogContext): void;
  info(message: string, context?: LogContext): void;
  warn(message: string, context?: LogContext): void;
  error(message: string, context?: LogContext): void;
}

export const nullLogger: Logger = {
  debug: () => undefined,
  info: () => undefined,
  warn: () => undefined,
  error: () => undefined,
};

// The initial Teams release supports binding only. Additional setup modes must
// arrive with the future #910/#956 adapters as their own reviewed change.
export type SetupMode = 'bind';

export interface StoredOAuthState {
  /** Lowercase hexadecimal SHA-256 of the raw browser state handle. */
  readonly stateKey: string;
  readonly teamsTenantId: string;
  readonly actorAadObjectId: string;
  readonly actorDeliveryId: string;
  readonly setupEmail: string;
  readonly setupMode: SetupMode;
  readonly pkceVerifier: string;
  readonly oidcNonce: string;
  readonly expiresAtEpochSeconds: number;
}

export type ConditionalCreateResult =
  | { readonly status: 'created' }
  | { readonly status: 'conflict' };

export type ConditionalConsumeResult =
  | { readonly status: 'consumed'; readonly state: StoredOAuthState }
  | { readonly status: 'missing' }
  | { readonly status: 'expired' };

/**
 * Persistence contract for a dedicated ephemeral state store.
 *
 * `conditionalCreate` must create only when stateKey is absent.
 * `conditionalConsume` must atomically return-and-remove exactly one unexpired
 * row, using the supplied application time rather than relying on TTL cleanup.
 * Implementations must never accept or reconstruct the raw state handle.
 */
export interface OAuthStatePersistence {
  conditionalCreate(state: StoredOAuthState): Promise<ConditionalCreateResult>;
  conditionalConsume(
    stateKey: string,
    nowEpochSeconds: number,
  ): Promise<ConditionalConsumeResult>;
}

export type OAuthTransaction = Omit<StoredOAuthState, 'stateKey'>;

export interface MintedOAuthState {
  /** The only value that may enter the browser/front channel. */
  readonly handle: string;
  /** Returned for the immediate authorization redirect; do not persist it. */
  readonly transaction: OAuthTransaction;
}

export interface OAuthStateConsumer {
  consume(handle: string): Promise<OAuthTransaction>;
}

export interface TokenExchangeRequest {
  readonly code: string;
  readonly codeVerifier: string;
}

export interface EphemeralTokenSet {
  readonly accessToken: string;
  readonly idToken: string;
}

export interface ConfidentialTokenClient {
  createAuthorizationUrl(input: {
    readonly state: string;
    readonly codeChallenge: string;
    readonly nonce: string;
    readonly loginHint?: string;
  }): URL;
  exchangeAuthorizationCode(request: TokenExchangeRequest): Promise<EphemeralTokenSet>;
}

export interface VerifiedIdentity {
  readonly subject: string;
  readonly email: string;
}

export interface IdTokenVerifier {
  verify(
    idToken: string,
    expected: { readonly nonce: string; readonly normalizedEmail: string },
  ): Promise<VerifiedIdentity>;
}

export interface ProviderBindingRequest {
  readonly teamsTenantId: string;
  readonly actorAadObjectId: string;
  readonly actorDeliveryId: string;
  readonly setupMode: SetupMode;
  readonly providerSubject: string;
  readonly providerEmail: string;
  /** Ephemeral bearer credential. Implementations must not retain or log it. */
  readonly accessToken: string;
}

export type ProviderBindingConflictReason =
  | 'tenant_bound_to_another_account'
  | 'actor_not_authorized';

export type ProviderBindingResult =
  | { readonly status: 'bound'; readonly bindingReference?: string }
  | { readonly status: 'already_bound'; readonly bindingReference?: string }
  | { readonly status: 'conflict'; readonly reason: ProviderBindingConflictReason };

/**
 * S04b's qurl-typescript adapter must make this operation atomic per tenant.
 * It must return a conflict instead of overwriting a different first binder.
 */
export interface ProviderBinder {
  bind(request: ProviderBindingRequest): Promise<ProviderBindingResult>;
}

export interface CallbackCompletion {
  readonly teamsTenantId: string;
  readonly actorAadObjectId: string;
  readonly actorDeliveryId: string;
  readonly setupMode: SetupMode;
  readonly identity: VerifiedIdentity;
  readonly binding: ProviderBindingResult;
}

export interface CallbackFailureLogContext extends LogContext {
  readonly errorCode: OAuthCoreError['code'];
}
