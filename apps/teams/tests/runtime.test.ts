import { afterEach, describe, expect, it, vi } from 'vitest';
import { DynamoOAuthStatePersistence } from '../src/oauth-state-store.js';
import { createProductionTeamsConfig } from '../src/server.js';
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
  afterEach(() => { vi.unstubAllEnvs(); });

  it('mounts the real SDK on the external listener with authentication and the body limit intact', async () => {
    const environment = {
      TEAMS_BASE_URL: 'https://teams.example.com', QURL_ENDPOINT: 'https://qurl.example.com', AWS_REGION: 'us-east-1',
      TEAMS_APP_ID: 'a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d', TEAMS_APP_PASSWORD: 'synthetic-bot-secret',
      BOT_TENANT_ID: 'b1c2d3e4-f5a6-4b7c-8d9e-0f1a2b3c4d5e',
      QURL_IMAGE: `ghcr.io/layervai/qurl@sha256:${'a'.repeat(64)}`,
      QURL_TEAMS_TENANT_PRINCIPALS_TABLE: 'principals', QURL_TEAMS_CHANNEL_POLICIES_TABLE: 'policies',
      QURL_TEAMS_PERSONAL_CONVERSATIONS_TABLE: 'conversations', QURL_TEAMS_TENANT_CREDENTIALS_TABLE: 'credentials',
      QURL_TEAMS_TENANT_CREDENTIALS_KMS_KEY_ARN: 'synthetic-key', OAUTH_STATE_TABLE: 'oauth-state',
      AUTH0_DOMAIN: 'https://auth.example.com/', AUTH0_CLIENT_ID: 'synthetic-client',
      AUTH0_CLIENT_SECRET: 'synthetic-secret', AUTH0_AUDIENCE: 'https://qurl.example.com',
      QURL_CONNECTOR_HUB_HOST: '', QURL_CONNECTOR_HUB_PORT: '', QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64: '',
      HOST: '127.0.0.1', PORT: '3000', NODE_ENV: 'production', AWS_EC2_METADATA_DISABLED: 'true',
    };
    for (const [key, value] of Object.entries(environment)) vi.stubEnv(key, value);
    const { server } = await createProductionTeamsConfig();
    try {
      await new Promise<void>((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
      const address = server.address();
      if (!address || typeof address === 'string') throw new Error('Expected a local TCP listener');
      const origin = `http://127.0.0.1:${address.port}`;
      const health = await fetch(`${origin}/health`);
      expect(health.status).toBe(200);
      expect(await health.json()).toEqual({ ok: true });
      const unauthorized = await fetch(`${origin}/api/messages`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ type: 'message' }),
      });
      expect(unauthorized.status).toBe(401);
      const oversized = await fetch(`${origin}/api/messages`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ text: 'x'.repeat(1_048_576) }),
      });
      expect(oversized.status).toBe(413);
    } finally {
      server.closeAllConnections();
      await new Promise<void>((resolve, reject) => { server.close(error => { if (error) reject(error); else resolve(); }); });
    }
  });

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
          // The raw service shape the document client actually throws: its
          // unmarshalling middleware runs on a resolved output only, so the
          // item on an exception is never converted to plain values.
          Object.assign(error, { Item: {
            state_handle_hash: { S: 'a'.repeat(64) }, teams_tenant_id: { S: '00000000-0000-4000-8000-000000000001' },
            actor_aad_object_id: { S: '00000000-0000-4000-8000-000000000002' }, actor_delivery_id: { S: '29:delivery' },
            setup_email: { S: 'admin@example.com' }, setup_mode: { S: 'bind' }, pkce_verifier: { S: 'a'.repeat(43) },
            oidc_nonce: { S: 'b'.repeat(43) }, expires_at: { N: '1000' },
          } });
          throw error;
        }
        return {} as T;
      },
    };
    const store = new DynamoOAuthStatePersistence({ client, tableName: 'oauth-state' });
    await expect(store.conditionalConsume('a'.repeat(64), 2_000)).resolves.toEqual({ status: 'expired' });
  });

  it('reports missing when the conditional failure carries no usable expiry', async () => {
    const client: DynamoClient = {
      async send<T>(request: DynamoRequest): Promise<T> {
        if (request.operation === 'delete') {
          const error = new Error('conditional failure');
          error.name = 'ConditionalCheckFailedException';
          throw error;
        }
        return {} as T;
      },
    };
    const store = new DynamoOAuthStatePersistence({ client, tableName: 'oauth-state' });
    await expect(store.conditionalConsume('a'.repeat(64), 2_000)).resolves.toEqual({ status: 'missing' });
  });

});
