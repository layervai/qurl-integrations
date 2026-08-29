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

class CleanupDynamo implements DynamoClient {
  readonly requests: DynamoRequest[] = [];
  async send<T>(request: DynamoRequest): Promise<T> {
    this.requests.push(request);
    if (request.operation !== 'query') return {} as T;
    const table = request.input.TableName;
    const paged = request.input.ExclusiveStartKey !== undefined;
    if (table === 'principals') return { Items: [{ principal_key: 'owner' }, { principal_key: 'admin%2Factor' }] } as T;
    if (table === 'policy') {
      return paged
        ? { Items: [{ policy_key: 'scope%23channel%23alias%23docs', resource_id: 'resource-2' }] } as T
        : { Items: [{ policy_key: 'scope%23channel%23resource%23resource-1', resource_id: 'resource-1' }], LastEvaluatedKey: { teams_tenant_id: 'tenant', policy_key: 'next' } } as T;
    }
    if (table === 'conversations') return { Items: [{ actor_aad_object_id: 'actor' }] } as T;
    return {} as T;
  }
}

// Models the resource_scopes GSI from modules/qurl-teams-ddb: the hash key
// selects the rows server-side, and the KEYS_ONLY projection returns only the
// base-table and index keys -- notably no resource_id to filter on.
class PurgeDynamo implements DynamoClient {
  readonly requests: DynamoRequest[] = [];
  async send<T>(request: DynamoRequest): Promise<T> {
    this.requests.push(request);
    if (request.operation !== 'query') return {} as T;
    const values = request.input.ExpressionAttributeValues as Record<string, string> | undefined;
    if (request.input.IndexName !== 'resource_scopes' || values?.[':resourceKey'] !== 'tenant#target') {
      return { Items: [] } as T;
    }
    const row = (policyKey: string, scopeItemTypeKey: string) => ({
      teams_tenant_id: 'tenant', policy_key: policyKey,
      tenant_resource_key: 'tenant#target', scope_item_type_key: scopeItemTypeKey,
    });
    return request.input.ExclusiveStartKey === undefined
      ? { Items: [row('scope%23one%23resource%23target', 'one#resource#target')], LastEvaluatedKey: { teams_tenant_id: 'tenant', policy_key: 'next' } } as T
      : { Items: [row('scope%23two%23alias%23target', 'two#alias#target')] } as T;
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
    expect(client.requests[0]?.input.KeyConditionExpression).toContain('begins_with(policy_key, :policyPrefix)');
    expect((client.requests[0]?.input.ExpressionAttributeValues as Record<string, unknown>)[':policyPrefix']).toBe('scope#channel#');
  });

  it('deletes all workspace rows across tables and query pages', async () => {
    const client = new CleanupDynamo();
    const store = new TeamsDataStore({ client, tenantPrincipalsTable: 'principals', channelPoliciesTable: 'policy', personalConversationsTable: 'conversations', tenantCredentialsTable: 'credentials' });
    await store.deleteWorkspace('tenant');
    const deletes = client.requests.filter(request => request.operation === 'delete');
    expect(client.requests.filter(request => request.operation === 'query')).toHaveLength(4);
    expect(deletes).toHaveLength(6);
    expect(deletes.map(request => request.input.TableName)).toEqual(['principals', 'principals', 'policy', 'policy', 'conversations', 'credentials']);
  });

  it('purges resource policies through the resource_scopes index across pages', async () => {
    const client = new PurgeDynamo();
    const store = new TeamsDataStore({ client, tenantPrincipalsTable: 'principals', channelPoliciesTable: 'policy', personalConversationsTable: 'conversations', tenantCredentialsTable: 'credentials' });
    await store.purgeResourceFromTenant('tenant', 'target');

    // Every read goes to the GSI: revoke must never scan the tenant partition.
    const queries = client.requests.filter(request => request.operation === 'query');
    expect(queries).toHaveLength(2);
    for (const query of queries) {
      expect(query.input.IndexName).toBe('resource_scopes');
      expect(query.input.KeyConditionExpression).toBe('tenant_resource_key = :resourceKey');
      expect((query.input.ExpressionAttributeValues as Record<string, string>)[':resourceKey']).toBe('tenant#target');
    }

    const deletes = client.requests.filter(request => request.operation === 'delete');
    expect(deletes).toHaveLength(2);
    expect(deletes.map(request => (request.input.Key as Record<string, string>).policy_key)).toEqual([
      'scope%23one%23resource%23target',
      'scope%23two%23alias%23target',
    ]);
    // Deletes address the base table, not the index.
    expect(deletes.map(request => request.input.TableName)).toEqual(['policy', 'policy']);
  });
});
