import { describe, expect, it } from 'vitest';
import { HttpProviderBinder } from '../src/provider-binder.js';
import { sha256Hex } from '../src/encoding.js';
import { nullLogger } from '../src/interfaces.js';
import type { TeamsDataStore } from '../src/teams-data.js';

const request = {
  teamsTenantId: 'tenant', actorAadObjectId: 'actor', actorDeliveryId: 'delivery', setupMode: 'bind' as const,
  providerSubject: 'subject', providerEmail: 'admin@example.com', accessToken: 'access-token',
};

function response(status: number, body = '{}'): Response {
  return new Response(body, { status, headers: { 'Content-Type': 'application/json' } });
}

const bindingBody = JSON.stringify({ binding_id: 'binding-1', api_key: { plaintext: 'api-key', key_id: 'key-1', key_prefix: 'qurl_' } });
const identityBody = JSON.stringify({ data: { owner_id: 'subject', auth_type: 'api_key', api_key: { key_id: 'key-1' } } });
const conflictBody = JSON.stringify({ error: { code: 'already_exists' } });

describe('HttpProviderBinder', () => {
  it('maps upstream authorization failures to a safe conflict', async () => {
    const data = { checkAdmin: async () => ({ isAdmin: false }) } as unknown as TeamsDataStore;
    for (const status of [401, 403]) {
      const binder = new HttpProviderBinder({ endpoint: 'https://qurl.example', data, fetch: async () => response(status) });
      await expect(binder.bind(request)).resolves.toEqual({ status: 'conflict', reason: 'actor_not_authorized' });
    }
  });

  it('reports a conflicting 409 binding without storing a credential', async () => {
    const data = {
      checkAdmin: async () => ({ isAdmin: false, ownerId: 'another-actor' }),
      tenantCredential: async () => undefined,
    } as unknown as TeamsDataStore;
    const binder = new HttpProviderBinder({ endpoint: 'https://qurl.example', data, fetch: async () => response(409, conflictBody) });
    await expect(binder.bind(request)).resolves.toEqual({ status: 'conflict', reason: 'tenant_bound_to_another_account' });
  });

  it('fails closed with a distinct recovery state after local uninstall removes the owner', async () => {
    const data = {
      checkAdmin: async () => ({ isAdmin: false }),
      tenantCredential: async () => undefined,
    } as unknown as TeamsDataStore;
    const binder = new HttpProviderBinder({ endpoint: 'https://qurl.example', data, fetch: async () => response(409, conflictBody) });
    await expect(binder.bind(request)).resolves.toEqual({ status: 'conflict', reason: 'upstream_binding_cleanup_required' });
  });

  it('fails closed when an existing qURL binding has no recoverable credential', async () => {
    const data = {
      checkAdmin: async () => ({ isAdmin: true, ownerId: 'actor' }),
      tenantCredential: async () => undefined,
    } as unknown as TeamsDataStore;
    const binder = new HttpProviderBinder({ endpoint: 'https://qurl.example', data, fetch: async () => response(409, conflictBody) });
    await expect(binder.bind(request)).rejects.toThrow('credential is unavailable');
  });

  it('stores and returns a newly provisioned binding credential', async () => {
    let bound = false;
    let saved: unknown;
    const data = {
      checkAdmin: async () => ({ isAdmin: false }),
      bindWorkspace: async () => { bound = true; },
      saveTenantCredential: async (_tenantId: string, credential: unknown) => { saved = credential; },
    } as unknown as TeamsDataStore;
    const binder = new HttpProviderBinder({
      endpoint: 'https://qurl.example',
      data,
      fetch: async input => new URL(input).pathname === '/v1/me' ? response(200, identityBody) : response(201, bindingBody),
    });
    await expect(binder.bind(request)).resolves.toEqual({ status: 'bound', bindingReference: 'binding-1' });
    expect(bound).toBe(true);
    expect(saved).toEqual({ apiKey: 'api-key', keyId: 'key-1', keyPrefix: 'qurl_', bindingId: 'binding-1' });
  });

  it('recovers a failed local save on a fresh setup using the same tenant binding replay', async () => {
    let ownerId: string | undefined;
    let saves = 0;
    const keys: string[] = [];
    const logs: unknown[] = [];
    const data = {
      checkAdmin: async () => ({ isAdmin: !!ownerId, ownerId }),
      bindWorkspace: async () => {
        if (ownerId) { const error = new Error('already bound'); error.name = 'ConditionalCheckFailedException'; throw error; }
        ownerId = 'actor';
      },
      saveTenantCredential: async () => { if (++saves === 1) throw new Error('temporary KMS failure api-key'); },
    } as unknown as TeamsDataStore;
    const binder = new HttpProviderBinder({
      endpoint: 'https://qurl.example', data,
      logger: { ...nullLogger, error: (message, context) => logs.push({ message, context }) },
      fetch: async (input, init) => {
        if (new URL(input).pathname === '/v1/me') return response(200, identityBody);
        keys.push(new Headers(init?.headers).get('Idempotency-Key') ?? '');
        return response(201, bindingBody);
      },
    });
    await expect(binder.bind(request)).rejects.toThrow('temporary KMS failure');
    await expect(binder.bind({ ...request, accessToken: 'new-access-token' })).resolves.toMatchObject({ status: 'already_bound' });
    expect(keys).toEqual(Array(2).fill(`teams-tenant-binding-v1-${sha256Hex('tenant')}`));
    expect(saves).toBe(2);
    expect(JSON.stringify(logs)).toContain('binding-1');
    expect(JSON.stringify(logs)).toContain('key-1');
    expect(JSON.stringify(logs)).not.toContain('api-key');
    expect(JSON.stringify(logs)).not.toContain('access-token');
  });

  it('does not accept an unrelated 409 as an existing binding', async () => {
    const data = {
      checkAdmin: async () => ({ isAdmin: true, ownerId: 'actor' }),
      tenantCredential: async () => ({ apiKey: 'api-key' }),
    } as unknown as TeamsDataStore;
    const binder = new HttpProviderBinder({ endpoint: 'https://qurl.example', data, fetch: async () => response(409, JSON.stringify({ error: { code: 'idempotency_conflict' } })) });
    await expect(binder.bind(request)).rejects.toThrow('rejected');
  });

  it.each([201, 409])('validates ownership and credential liveness before accepting status %s', async status => {
    for (const me of [
      { status: 401, body: '{}' },
      { status: 200, body: JSON.stringify({ data: { owner_id: 'another-owner', auth_type: 'api_key', api_key: {} } }) },
      { status: 200, body: JSON.stringify({ data: { owner_id: 'subject', auth_type: 'jwt' } }) },
    ]) {
      let saved = false;
      const data = {
        checkAdmin: async () => ({ isAdmin: true, ownerId: 'actor' }),
        tenantCredential: async () => ({ apiKey: 'api-key', keyId: 'key-1', bindingId: 'binding-1' }),
        bindWorkspace: async () => undefined,
        saveTenantCredential: async () => { saved = true; },
      } as unknown as TeamsDataStore;
      const binder = new HttpProviderBinder({ endpoint: 'https://qurl.example', data, fetch: async input => new URL(input).pathname === '/v1/me'
        ? response(me.status, me.body) : response(status, status === 409 ? conflictBody : bindingBody) });
      await expect(binder.bind(request)).rejects.toThrow();
      expect(saved).toBe(false);
    }
  });

  it('reuses a stored credential only for an already-existing binding and the verified qURL owner', async () => {
    let validated = false;
    const data = {
      checkAdmin: async () => ({ isAdmin: true, ownerId: 'actor' }),
      tenantCredential: async () => ({ apiKey: 'api-key', keyId: 'key-1', bindingId: 'binding-1' }),
    } as unknown as TeamsDataStore;
    const binder = new HttpProviderBinder({ endpoint: 'https://qurl.example', data, fetch: async (input, init) => {
      if (new URL(input).pathname !== '/v1/me') return response(409, conflictBody);
      expect(new Headers(init?.headers).get('Authorization')).toBe('Bearer api-key');
      expect(new Headers(init?.headers).get('User-Agent')).toBe('qurl-teams/1');
      validated = true;
      return response(200, identityBody);
    } });
    await expect(binder.bind(request)).resolves.toEqual({ status: 'already_bound', bindingReference: 'binding-1' });
    expect(validated).toBe(true);
  });

  it('normalizes a timeout while reading the binding response body', async () => {
    let aborted = false;
    const binder = new HttpProviderBinder({
      endpoint: 'https://qurl.example',
      data: { checkAdmin: async () => ({ isAdmin: false }) } as unknown as TeamsDataStore,
      timeoutMs: 5,
      fetch: async (_input, init) => new Response(new ReadableStream<Uint8Array>({
        start(controller) {
          init?.signal?.addEventListener('abort', () => {
            aborted = true;
            controller.error(new Error('aborted'));
          }, { once: true });
        },
      }), { status: 201 }),
    });

    await expect(binder.bind(request)).rejects.toThrow('timed out or was cancelled');
    expect(aborted).toBe(true);
  });
});
