import { randomBytes } from 'node:crypto';
import { base64UrlEncode, sha256Hex } from './encoding.js';
import { OAuthCoreError } from './errors.js';
import type {
  Clock,
  MintedOAuthState,
  OAuthStatePersistence,
  OAuthTransaction,
  SetupMode,
  StoredOAuthState,
} from './interfaces.js';
import { systemClock } from './interfaces.js';
import { generateOidcNonce, isOidcNonce } from './nonce.js';
import { generatePkcePair, isPkceVerifier } from './pkce.js';
import type { RandomBytes } from './pkce.js';

export const OAUTH_STATE_TTL_SECONDS = 5 * 60;
const STATE_HANDLE_PATTERN = /^[A-Za-z0-9_-]{43}$/;
const STATE_KEY_PATTERN = /^[0-9a-f]{64}$/;
const DELIVERY_ID_PATTERN = /^29:[A-Za-z0-9._~-]{1,253}$/;
// Entra identifiers are UUID-shaped, but their generator's RFC version and
// variant nibbles are not part of this security contract.
const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
const INITIAL_SETUP_MODE: SetupMode = 'bind';

export interface MintOAuthStateInput {
  readonly teamsTenantId: string;
  readonly actorAadObjectId: string;
  readonly actorDeliveryId: string;
  readonly setupEmail: string;
  readonly setupMode: SetupMode;
}

export interface OAuthStateManagerOptions {
  readonly persistence: OAuthStatePersistence;
  readonly clock?: Clock;
  readonly randomBytes?: RandomBytes;
}

export function normalizeEmail(email: string): string {
  const normalized = email.trim().toLowerCase();
  if (normalized.length > 254 || !EMAIL_PATTERN.test(normalized)) {
    throw new OAuthCoreError('INVALID_INPUT', 'Setup email is invalid.');
  }
  return normalized;
}

export function isOpaqueStateHandle(value: unknown): value is string {
  if (typeof value !== 'string' || !STATE_HANDLE_PATTERN.test(value)) {
    return false;
  }
  try {
    return Buffer.from(value, 'base64url').length === 32;
  } catch {
    return false;
  }
}

export function stateLookupKey(handle: string): string {
  if (!isOpaqueStateHandle(handle)) {
    throw new OAuthCoreError('INVALID_STATE', 'OAuth state is invalid.');
  }
  return sha256Hex(handle);
}

function assertIdentifier(value: string, label: string): void {
  if (!UUID_PATTERN.test(value)) {
    throw new OAuthCoreError('INVALID_INPUT', `${label} must be a canonical UUID.`);
  }
}

function normalizeMintInput(input: MintOAuthStateInput): Omit<OAuthTransaction, 'pkceVerifier' | 'oidcNonce' | 'expiresAtEpochSeconds'> {
  assertIdentifier(input.teamsTenantId, 'Teams tenant ID');
  assertIdentifier(input.actorAadObjectId, 'Actor AAD object ID');
  if (!DELIVERY_ID_PATTERN.test(input.actorDeliveryId)) {
    throw new OAuthCoreError('INVALID_INPUT', 'Actor delivery ID is invalid.');
  }
  if (input.setupMode !== INITIAL_SETUP_MODE) {
    throw new OAuthCoreError('INVALID_INPUT', 'Setup mode is invalid.');
  }
  return {
    teamsTenantId: input.teamsTenantId.toLowerCase(),
    actorAadObjectId: input.actorAadObjectId.toLowerCase(),
    actorDeliveryId: input.actorDeliveryId,
    setupEmail: normalizeEmail(input.setupEmail),
    setupMode: input.setupMode,
  };
}

function transactionFromStored(state: StoredOAuthState, expectedStateKey: string): OAuthTransaction {
  if (!STATE_KEY_PATTERN.test(state.stateKey) || state.stateKey !== expectedStateKey) {
    throw new OAuthCoreError('STATE_STORE_FAILED', 'OAuth state storage returned an invalid record.');
  }
  const normalized = normalizeMintInput(state);
  if (normalized.setupEmail !== state.setupEmail || !isPkceVerifier(state.pkceVerifier) || !isOidcNonce(state.oidcNonce)) {
    throw new OAuthCoreError('STATE_STORE_FAILED', 'OAuth state storage returned an invalid record.');
  }
  if (!Number.isSafeInteger(state.expiresAtEpochSeconds)) {
    throw new OAuthCoreError('STATE_STORE_FAILED', 'OAuth state storage returned an invalid record.');
  }
  return {
    ...normalized,
    pkceVerifier: state.pkceVerifier,
    oidcNonce: state.oidcNonce,
    expiresAtEpochSeconds: state.expiresAtEpochSeconds,
  };
}

export class OAuthStateManager {
  readonly #persistence: OAuthStatePersistence;
  readonly #clock: Clock;
  readonly #randomBytes: RandomBytes;

  constructor(options: OAuthStateManagerOptions) {
    this.#persistence = options.persistence;
    this.#clock = options.clock ?? systemClock;
    this.#randomBytes = options.randomBytes ?? randomBytes;
  }

  async mint(input: MintOAuthStateInput): Promise<MintedOAuthState> {
    const now = this.#now();
    const identity = normalizeMintInput(input);
    const handle = base64UrlEncode(this.#randomBytes(32));
    if (!isOpaqueStateHandle(handle)) {
      throw new OAuthCoreError('INVALID_INPUT', 'Random source returned an invalid OAuth state handle.');
    }
    const pkce = generatePkcePair(this.#randomBytes);
    const transaction: OAuthTransaction = {
      ...identity,
      pkceVerifier: pkce.verifier,
      oidcNonce: generateOidcNonce(this.#randomBytes),
      expiresAtEpochSeconds: now + OAUTH_STATE_TTL_SECONDS,
    };
    const stored: StoredOAuthState = {
      stateKey: stateLookupKey(handle),
      ...transaction,
    };

    let created;
    try {
      created = await this.#persistence.conditionalCreate(stored);
    } catch {
      throw new OAuthCoreError('STATE_STORE_FAILED', 'OAuth state could not be stored.', { retryable: true });
    }
    if (created.status !== 'created') {
      throw new OAuthCoreError('STATE_COLLISION', 'OAuth state could not be created.');
    }
    return { handle, transaction };
  }

  async consume(handle: string): Promise<OAuthTransaction> {
    const stateKey = stateLookupKey(handle);
    const now = this.#now();
    let consumed;
    try {
      consumed = await this.#persistence.conditionalConsume(stateKey, now);
    } catch {
      throw new OAuthCoreError('STATE_STORE_FAILED', 'OAuth state could not be consumed.', { retryable: true });
    }
    if (consumed.status === 'missing') {
      throw new OAuthCoreError('STATE_NOT_FOUND', 'OAuth state is invalid or already consumed.');
    }
    if (consumed.status === 'expired') {
      throw new OAuthCoreError('STATE_EXPIRED', 'OAuth state has expired.');
    }

    const transaction = transactionFromStored(consumed.state, stateKey);
    if (now >= transaction.expiresAtEpochSeconds) {
      throw new OAuthCoreError('STATE_EXPIRED', 'OAuth state has expired.');
    }
    if (transaction.expiresAtEpochSeconds > now + OAUTH_STATE_TTL_SECONDS) {
      throw new OAuthCoreError('STATE_STORE_FAILED', 'OAuth state storage returned an invalid expiry.');
    }
    return transaction;
  }

  #now(): number {
    const now = this.#clock.now();
    if (!Number.isSafeInteger(now) || now < 0) {
      throw new OAuthCoreError('INVALID_INPUT', 'Clock returned an invalid time.');
    }
    return now;
  }
}
