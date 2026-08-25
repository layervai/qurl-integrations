import { describe, expect, it } from 'vitest';
import { TeamsDataStore, type DynamoClient, type DynamoRequest } from '../src/teams-data.js';
import type { CredentialCipher } from '../src/credential-cipher.js';

class RecordingDynamo implements DynamoClient {
  readonly requests: DynamoRequest[] = [];
  async send<T>(request: DynamoRequest): Promise<T> {
    this.requests.push(request);
    return {} as T;
  }
}

class PagedDynamo implements DynamoClient {
  readonly requests: DynamoRequest[] = [];
  async send<T>(request: DynamoRequest): Promise<T> {
    this.requests.push(request);
    const page = this.requests.length === 1
      ? { Items: [{ scope_id: 'channel', item_type: 'resource', resource_id: 'one' }], LastEvaluatedKey: { teams_tenant_id: 'tenant', policy_key: 'next' } }
      : { Items: [{ scope_id: 'channel', item_type: 'resource', resource_id: 'two' }] };
    return page as T;
  }
}

describe('Teams DynamoDB data paths', () => {
  it('encrypts tenant credentials through the injected CMK adapter', async () => {
    const client = new RecordingDynamo();
    const cipher: CredentialCipher = {
      encrypt: async (tenantId, value) => `${tenantId}:encrypted:${value}`,
      decrypt: async (_tenantId, value) => value.replace(/^tenant:encrypted:/, ''),
    };
    await new TeamsDataStore({ client, credentialCipher: cipher, tenantPrincipalsTable: 'principals', channelPoliciesTable: 'policy', personalConversationsTable: 'conversations', tenantCredentialsTable: 'credentials' }).saveTenantCredential('tenant', { apiKey: 'secret' });
    expect((client.requests[0]?.input.ExpressionAttributeValues as Record<string, unknown>)[':apiKey']).toBe('tenant:encrypted:secret');
  });

  it('writes a normalized alias policy row', async () => {
    const client = new RecordingDynamo();
    await new TeamsDataStore({ client, tenantPrincipalsTable: 'principals', channelPoliciesTable: 'policy', personalConversationsTable: 'conversations', tenantCredentialsTable: 'credentials' }).bindScopeAlias('tenant', 'channel', 'docs', 'resource');
    expect(client.requests).toHaveLength(1);
    expect(client.requests[0]?.operation).toBe('put');
    expect(client.requests[0]?.input.TableName).toBe('policy');
  });

  it('keeps alias binding idempotent for the same resource', async () => {
    const client: DynamoClient = {
      async send<T>(request: DynamoRequest): Promise<T> {
        if (request.operation === 'put') {
          const error = new Error('conditional failure');
          error.name = 'ConditionalCheckFailedException';
          throw error;
        }
        return { Item: { resource_id: 'resource' } } as T;
      },
    };
    await expect(new TeamsDataStore({ client, tenantPrincipalsTable: 'principals', channelPoliciesTable: 'policy', personalConversationsTable: 'conversations', tenantCredentialsTable: 'credentials' }).bindScopeAlias('tenant', 'channel', 'docs', 'resource')).resolves.toBeUndefined();
  });

  it('rejects alias hijacking when a conditional bind finds another resource', async () => {
    const client: DynamoClient = {
      async send<T>(request: DynamoRequest): Promise<T> {
        if (request.operation === 'put') {
          const error = new Error('conditional failure');
          error.name = 'ConditionalCheckFailedException';
          throw error;
        }
        return { Item: { resource_id: 'other-resource' } } as T;
      },
    };
    await expect(new TeamsDataStore({ client, tenantPrincipalsTable: 'principals', channelPoliciesTable: 'policy', personalConversationsTable: 'conversations', tenantCredentialsTable: 'credentials' }).bindScopeAlias('tenant', 'channel', 'docs', 'resource')).rejects.toThrow('Scope alias is already bound');
  });

  it('writes personal conversations to the normalized table', async () => {
    const client = new RecordingDynamo();
    await new TeamsDataStore({ client, tenantPrincipalsTable: 'principals', channelPoliciesTable: 'policy', personalConversationsTable: 'conversations', tenantCredentialsTable: 'credentials' }).savePersonalConversationRef('tenant', 'actor', { serviceUrl: 'https://smba.trafficmanager.net', conversationId: 'conversation' });
    expect(client.requests).toHaveLength(1);
    expect(client.requests[0]?.operation).toBe('put');
    expect(client.requests[0]?.input.TableName).toBe('conversations');
  });

  it('purges normalized resource rows without an empty expression-name map', async () => {
    const client = new RecordingDynamo();
    await new TeamsDataStore({ client, tenantPrincipalsTable: 'principals', channelPoliciesTable: 'policy', personalConversationsTable: 'conversations', tenantCredentialsTable: 'credentials' }).purgeResourceFromScope('tenant', 'channel', 'resource');
    expect(client.requests[0]?.operation).toBe('delete');
    expect(client.requests[0]?.input).not.toHaveProperty('ExpressionAttributeNames');
  });

  it('continues tenant queries across DynamoDB pages', async () => {
    const client = new PagedDynamo();
    const store = new TeamsDataStore({ client, tenantPrincipalsTable: 'principals', channelPoliciesTable: 'policy', personalConversationsTable: 'conversations', tenantCredentialsTable: 'credentials' });
    await expect(store.allowedResourceIds('tenant', 'channel')).resolves.toEqual(new Set(['one', 'two']));
    expect(client.requests[1]?.input).toHaveProperty('ExclusiveStartKey');
  });
});
