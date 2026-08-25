import { describe, expect, it } from 'vitest';
import { HttpProviderBinder } from '../src/provider-binder.js';
import type { TeamsDataStore } from '../src/teams-data.js';

const request = {
  teamsTenantId: 'tenant', actorAadObjectId: 'actor', actorDeliveryId: 'delivery', setupMode: 'bind' as const,
  providerSubject: 'subject', providerEmail: 'admin@example.com', accessToken: 'access-token',
};

function response(status: number, body = '{}'): Response {
  return new Response(body, { status, headers: { 'Content-Type': 'application/json' } });
}

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
    const binder = new HttpProviderBinder({ endpoint: 'https://qurl.example', data, fetch: async () => response(409) });
    await expect(binder.bind(request)).resolves.toEqual({ status: 'conflict', reason: 'tenant_bound_to_another_account' });
  });

  it('fails closed when an existing qURL binding has no recoverable credential', async () => {
    const data = {
      checkAdmin: async () => ({ isAdmin: true, ownerId: 'actor' }),
      tenantCredential: async () => undefined,
    } as unknown as TeamsDataStore;
    const binder = new HttpProviderBinder({ endpoint: 'https://qurl.example', data, fetch: async () => response(409) });
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
      fetch: async () => response(201, JSON.stringify({ data: { api_key: { plaintext: 'api-key', key_id: 'key-1', key_prefix: 'qurl_' } } })),
    });
    await expect(binder.bind(request)).resolves.toEqual({ status: 'bound', bindingReference: 'key-1' });
    expect(bound).toBe(true);
    expect(saved).toEqual({ apiKey: 'api-key', keyId: 'key-1', keyPrefix: 'qurl_' });
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
