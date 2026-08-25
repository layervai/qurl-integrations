import { describe, expect, it } from 'vitest';
import { DynamoOAuthStatePersistence } from '../src/oauth-state-store.js';
import type { DynamoClient, DynamoRequest } from '../src/teams-data.js';

class RecordingDynamo implements DynamoClient {
  readonly requests: DynamoRequest[] = [];
  async send<T>(request: DynamoRequest): Promise<T> {
    this.requests.push(request);
    if (request.operation === 'delete') {
      return { Attributes: {
        state_handle_hash: 'a'.repeat(64), teams_tenant_id: '00000000-0000-4000-8000-000000000001',
        actor_aad_object_id: '00000000-0000-4000-8000-000000000002', actor_delivery_id: '29:delivery',
        setup_email: 'admin@example.com', setup_mode: 'bind', pkce_verifier: 'a'.repeat(43),
        oidc_nonce: 'b'.repeat(43), expires_at: 2_000_000_100,
      } } as T;
    }
    return {} as T;
  }
}

describe('Teams runtime adapters', () => {
  it('uses conditional put and atomic old-value delete for OAuth state', async () => {
    const client = new RecordingDynamo();
    const store = new DynamoOAuthStatePersistence({ client, tableName: 'oauth-state' });
    await expect(store.conditionalCreate({
      stateKey: 'a'.repeat(64), teamsTenantId: '00000000-0000-4000-8000-000000000001',
      actorAadObjectId: '00000000-0000-4000-8000-000000000002', actorDeliveryId: '29:delivery',
      setupEmail: 'admin@example.com', setupMode: 'bind', pkceVerifier: 'a'.repeat(43),
      oidcNonce: 'b'.repeat(43), expiresAtEpochSeconds: 2_000_000_100,
    })).resolves.toEqual({ status: 'created' });
    await expect(store.conditionalConsume('a'.repeat(64), 2_000_000_000)).resolves.toMatchObject({ status: 'consumed' });
    expect(client.requests[0]?.input.ConditionExpression).toBe('attribute_not_exists(state_handle_hash)');
    expect(client.requests[1]?.input.ConditionExpression).toContain('expires_at > :now');
    expect(client.requests[1]?.input.ReturnValues).toBe('ALL_OLD');
    await store.read('a'.repeat(64));
    expect(client.requests[2]?.input.ConsistentRead).toBe(true);
  });

  it('reports expired rows from conditional consume failures', async () => {
    const client: DynamoClient = {
      async send<T>(request: DynamoRequest): Promise<T> {
        if (request.operation === 'delete') {
          const error = new Error('conditional failure');
          error.name = 'ConditionalCheckFailedException';
          Object.assign(error, { Item: {
            state_handle_hash: 'a'.repeat(64), teams_tenant_id: '00000000-0000-4000-8000-000000000001',
            actor_aad_object_id: '00000000-0000-4000-8000-000000000002', actor_delivery_id: '29:delivery',
            setup_email: 'admin@example.com', setup_mode: 'bind', pkce_verifier: 'a'.repeat(43),
            oidc_nonce: 'b'.repeat(43), expires_at: 1_000,
          } });
          throw error;
        }
        return {} as T;
      },
    };
    const store = new DynamoOAuthStatePersistence({ client, tableName: 'oauth-state' });
    await expect(store.conditionalConsume('a'.repeat(64), 2_000)).resolves.toEqual({ status: 'expired' });
  });

});
