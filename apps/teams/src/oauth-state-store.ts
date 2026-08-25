import type {
  ConditionalConsumeResult,
  ConditionalCreateResult,
  OAuthStatePersistence,
  StoredOAuthState,
} from './interfaces.js';
import type { DynamoClient } from './teams-data.js';

export interface DynamoOAuthStatePersistenceOptions {
  readonly client: DynamoClient;
  readonly tableName: string;
}

function conditionalFailure(error: unknown): boolean {
  return error instanceof Error && error.name === 'ConditionalCheckFailedException';
}

function storedItem(state: StoredOAuthState): Record<string, unknown> {
  return {
    state_handle_hash: state.stateKey,
    teams_tenant_id: state.teamsTenantId,
    actor_aad_object_id: state.actorAadObjectId,
    actor_delivery_id: state.actorDeliveryId,
    setup_email: state.setupEmail,
    setup_mode: state.setupMode,
    pkce_verifier: state.pkceVerifier,
    oidc_nonce: state.oidcNonce,
    expires_at: state.expiresAtEpochSeconds,
  };
}

function stateFromItem(item: Record<string, unknown>): StoredOAuthState | undefined {
  if (typeof item.state_handle_hash !== 'string' || typeof item.teams_tenant_id !== 'string'
    || typeof item.actor_aad_object_id !== 'string' || typeof item.actor_delivery_id !== 'string'
    || typeof item.setup_email !== 'string' || item.setup_mode !== 'bind'
    || typeof item.pkce_verifier !== 'string' || typeof item.oidc_nonce !== 'string'
    || typeof item.expires_at !== 'number') {
    return undefined;
  }
  return {
    stateKey: item.state_handle_hash,
    teamsTenantId: item.teams_tenant_id,
    actorAadObjectId: item.actor_aad_object_id,
    actorDeliveryId: item.actor_delivery_id,
    setupEmail: item.setup_email,
    setupMode: 'bind',
    pkceVerifier: item.pkce_verifier,
    oidcNonce: item.oidc_nonce,
    expiresAtEpochSeconds: item.expires_at,
  };
}

function oldItem(error: unknown): Record<string, unknown> | undefined {
  if (!error || typeof error !== 'object') return undefined;
  const item = (error as { readonly Item?: unknown }).Item;
  return item !== null && typeof item === 'object' && !Array.isArray(item)
    ? item as Record<string, unknown>
    : undefined;
}

/** DynamoDB implementation of the one-shot OAuth state contract. */
export class DynamoOAuthStatePersistence implements OAuthStatePersistence {
  readonly #client: DynamoClient;
  readonly #tableName: string;

  constructor(options: DynamoOAuthStatePersistenceOptions) {
    if (!options.tableName.trim()) throw new Error('OAuth state table name is required');
    this.#client = options.client;
    this.#tableName = options.tableName;
  }

  async conditionalCreate(state: StoredOAuthState): Promise<ConditionalCreateResult> {
    try {
      await this.#client.send({
        operation: 'put',
        input: {
          TableName: this.#tableName,
          Item: storedItem(state),
          ConditionExpression: 'attribute_not_exists(state_handle_hash)',
        },
      });
      return { status: 'created' };
    } catch (error) {
      if (conditionalFailure(error)) return { status: 'conflict' };
      throw error;
    }
  }

  async read(stateKey: string): Promise<StoredOAuthState | undefined> {
    const result = await this.#client.send<{ readonly Item?: Record<string, unknown> }>({
      operation: 'get',
      input: {
        TableName: this.#tableName,
        Key: { state_handle_hash: stateKey },
        ConsistentRead: true,
      },
    });
    if (!result.Item) return undefined;
    const stored = stateFromItem(result.Item);
    if (!stored) throw new Error('DynamoDB state read returned an invalid row');
    return stored;
  }

  async conditionalConsume(stateKey: string, nowEpochSeconds: number): Promise<ConditionalConsumeResult> {
    try {
      const result = await this.#client.send<{ readonly Attributes?: Record<string, unknown> }>({
        operation: 'delete',
        input: {
          TableName: this.#tableName,
          Key: { state_handle_hash: stateKey },
          ConditionExpression: 'attribute_exists(state_handle_hash) AND expires_at > :now',
          ExpressionAttributeValues: { ':now': nowEpochSeconds },
          // The old row lets us distinguish an expired transaction from a
          // missing/already-consumed transaction without a racy read first.
          ReturnValues: 'ALL_OLD',
          ReturnValuesOnConditionCheckFailure: 'ALL_OLD',
        },
      });
      const stored = result.Attributes ? stateFromItem(result.Attributes) : undefined;
      if (!stored) throw new Error('DynamoDB state consume returned an invalid row');
      return { status: 'consumed', state: stored };
    } catch (error) {
      if (!conditionalFailure(error)) throw error;
      const item = oldItem(error);
      const stored = item ? stateFromItem(item) : undefined;
      if (stored && stored.expiresAtEpochSeconds <= nowEpochSeconds) return { status: 'expired' };
      return { status: 'missing' };
    }
  }
}
