import { describe, expect, it } from 'vitest';
import type { CredentialCipher } from '../src/credential-cipher.js';
import { HttpProviderBinder } from '../src/provider-binder.js';
import { TeamsDataStore, type DynamoClient, type DynamoRequest } from '../src/teams-data.js';

const request = {
  teamsTenantId: 'tenant', actorAadObjectId: 'owner', actorDeliveryId: 'delivery', setupMode: 'bind' as const,
  providerSubject: 'auth0|account', providerEmail: 'owner@example.com', accessToken: 'access-token',
};
const credential = { apiKey: 'api-key', keyId: 'key-1', keyPrefix: 'qurl_', bindingId: 'binding-1' };

// A narrow transport seam for composing the real binder and datastore. It
// returns the captured owner Put row and a credential readback fixture; the
// conditional-conflict response is scripted, not a DynamoDB condition evaluator.
// Native marshalling/CAS behavior is covered by the separate DynamoDB Local proof.
class BindingTransport implements DynamoClient {
  readonly writes: DynamoRequest[] = [];
  owner: Record<string, unknown> | undefined;
  savedValues: Record<string, unknown> | undefined;
  rejectOwnerPut = false;

  async send<T>(operation: DynamoRequest): Promise<T> {
    const { input } = operation;
    if (operation.operation === 'get' && input.TableName === 'principals') return { Item: this.owner } as T;
    if (operation.operation === 'get' && input.TableName === 'credentials') {
      return { Item: this.savedValues ? {
        qurl_api_key: this.savedValues[':apiKey'], qurl_key_id: this.savedValues[':keyId'],
        qurl_binding_id: this.savedValues[':bindingId'], qurl_key_prefix: this.savedValues[':keyPrefix'],
      } : undefined } as T;
    }
    this.writes.push(operation);
    if (operation.operation === 'put' && input.TableName === 'principals') {
      if (this.rejectOwnerPut) {
        const error = new Error('scripted existing-owner conflict');
        error.name = 'ConditionalCheckFailedException';
        throw error;
      }
      this.owner = structuredClone(input.Item as Record<string, unknown>);
      return {} as T;
    }
    if (operation.operation === 'update' && input.TableName === 'credentials') {
      this.savedValues = structuredClone(input.ExpressionAttributeValues as Record<string, unknown>);
      return {} as T;
    }
    throw new Error(`Unexpected binding operation: ${operation.operation}`);
  }
}

function fixture(transport: BindingTransport, cipher?: CredentialCipher) {
  const upstream: { path: string; authorization: string | null; idempotencyKey: string | null }[] = [];
  const data = new TeamsDataStore({ client: transport, tenantPrincipalsTable: 'principals', channelPoliciesTable: 'policies',
    personalConversationsTable: 'conversations', tenantCredentialsTable: 'credentials', ...(cipher ? { credentialCipher: cipher } : {}) });
  const binder = new HttpProviderBinder({ endpoint: 'https://qurl.example', data, fetch: async (input, init) => {
    const path = new URL(input).pathname;
    const headers = new Headers(init?.headers);
    upstream.push({ path, authorization: headers.get('Authorization'), idempotencyKey: headers.get('Idempotency-Key') });
    const body = path === '/v1/me'
      ? { data: { owner_id: request.providerSubject, auth_type: 'api_key', api_key: { key_id: credential.keyId } } }
      : { binding_id: credential.bindingId, api_key: { plaintext: credential.apiKey, key_id: credential.keyId, key_prefix: credential.keyPrefix } };
    return new Response(JSON.stringify(body), { status: path === '/v1/me' ? 200 : 201 });
  } });
  return { binder, data, upstream };
}

describe('binding to administrator lifecycle', () => {
  it('makes the successful installer the owner administrator through the real datastore', async () => {
    const transport = new BindingTransport();
    const { binder, data, upstream } = fixture(transport);
    await expect(binder.bind(request)).resolves.toEqual({ status: 'bound', bindingReference: credential.bindingId });
    await expect(data.checkAdmin('tenant', 'owner')).resolves.toEqual({ isAdmin: true, ownerId: 'owner', installationId: expect.any(String) });
    await expect(data.checkAdmin('tenant', 'other')).resolves.toMatchObject({ isAdmin: false, ownerId: 'owner' });
    await expect(data.tenantCredential('tenant')).resolves.toEqual(credential);
    expect(upstream.map(call => [call.path, call.authorization])).toEqual([
      ['/v1/external-identity-bindings', 'Bearer access-token'], ['/v1/me', 'Bearer api-key'],
    ]);
  });

  it.each([false, true])('refuses a non-owner rebind before upstream calls (existing admin=%s)', async isAdmin => {
    const transport = new BindingTransport();
    const { binder, data, upstream } = fixture(transport);
    await binder.bind(request);
    transport.owner = { ...transport.owner, admin_aad_object_ids: new Set(isAdmin ? ['other'] : []) };
    const originalOwner = structuredClone(transport.owner);
    const writesBefore = transport.writes.length;
    upstream.length = 0;
    await expect(binder.bind({ ...request, actorAadObjectId: 'other' })).resolves.toEqual({ status: 'conflict', reason: 'tenant_bound_to_another_account' });
    expect(upstream).toEqual([]);
    expect(transport.writes).toHaveLength(writesBefore);
    expect(transport.owner).toEqual(originalOwner);
    await expect(data.checkAdmin('tenant', 'owner')).resolves.toMatchObject({ isAdmin: true, ownerId: 'owner', installationId: originalOwner.installation_id });
    await expect(data.checkAdmin('tenant', 'other')).resolves.toMatchObject({ isAdmin, ownerId: 'owner', installationId: originalOwner.installation_id });
    await expect(data.tenantCredential('tenant')).resolves.toEqual(credential);
  });

  it('recovers credential persistence while retaining the original owner, administrators, and installation', async () => {
    const transport = new BindingTransport();
    let encryptions = 0;
    const cipher: CredentialCipher = {
      encrypt: async (_tenantId, value) => { if (++encryptions === 1) throw new Error('temporary encryption failure'); return `encrypted:${value}`; },
      decrypt: async (_tenantId, value) => value.slice('encrypted:'.length),
    };
    const { binder, data, upstream } = fixture(transport, cipher);
    await expect(binder.bind(request)).rejects.toThrow('temporary encryption failure');
    await expect(data.checkAdmin('tenant', 'owner')).resolves.toMatchObject({ isAdmin: true, ownerId: 'owner' });
    await expect(data.tenantCredential('tenant')).resolves.toBeUndefined();
    // Represent an administrator already present when the owner retries. The
    // test does not evaluate String Set updates or simulate their atomicity.
    transport.owner = { ...transport.owner, admin_aad_object_ids: new Set(['existing-admin']) };
    const originalOwner = structuredClone(transport.owner);
    transport.rejectOwnerPut = true;

    await expect(binder.bind({ ...request, accessToken: 'fresh-access-token' })).resolves.toEqual({ status: 'already_bound', bindingReference: credential.bindingId });
    expect(transport.owner).toEqual(originalOwner);
    await expect(data.checkAdmin('tenant', 'owner')).resolves.toMatchObject({ isAdmin: true, installationId: originalOwner.installation_id });
    await expect(data.checkAdmin('tenant', 'existing-admin')).resolves.toMatchObject({ isAdmin: true, ownerId: 'owner', installationId: originalOwner.installation_id });
    await expect(data.checkAdmin('tenant', 'other')).resolves.toMatchObject({ isAdmin: false });
    await expect(data.tenantCredential('tenant')).resolves.toEqual(credential);
    expect(transport.savedValues?.[':apiKey']).toBe('encrypted:api-key');
    const bindingCalls = upstream.filter(call => call.path === '/v1/external-identity-bindings');
    expect(bindingCalls).toHaveLength(2);
    expect(bindingCalls[0]?.idempotencyKey).toBeTruthy();
    expect(bindingCalls[1]?.idempotencyKey).toBe(bindingCalls[0]?.idempotencyKey);
    expect(bindingCalls[1]?.authorization).toBe('Bearer fresh-access-token');
    expect(upstream.filter(call => call.path === '/v1/me')).toHaveLength(2);
  });
});
