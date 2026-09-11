import { describe, expect, it } from 'vitest';
import { HttpQurlClient } from '../src/qurl-client.js';

describe('qURL HTTP adapter', () => {
  it('rejects a qURL endpoint that is not an origin', () => {
    expect(() => new HttpQurlClient({ endpoint: 'https://api.example.test/prefix', apiKey: 'secret' })).toThrow();
  });

  it('uses the resource-scoped mint endpoint and decodes the API envelope', async () => {
    const requests: Request[] = [];
    const client = new HttpQurlClient({
      endpoint: 'https://api.example.test',
      apiKey: 'secret',
      fetch: async (input, init) => {
        requests.push(new Request(input, init));
        return new Response(JSON.stringify({ data: { resource_id: 'r_1', qurl_link: 'https://qurl.example/one' } }), { status: 201 });
      },
    });

    await expect(client.create({ resourceId: 'r_1', oneTimeUse: true, idempotencyKey: 'create-key' })).resolves.toMatchObject({ resourceId: 'r_1' });
    expect(requests[0]?.url).toBe('https://api.example.test/v1/resources/r_1/qurls');
    expect(requests[0]?.headers.get('Idempotency-Key')).toBe('create-key');
    expect(JSON.parse(await requests[0]!.text())).toEqual({ one_time_use: true });
  });

  it('maps snake_case resource lists and pagination metadata', async () => {
    const client = new HttpQurlClient({
      endpoint: 'https://api.example.test',
      apiKey: 'secret',
      fetch: async () => new Response(JSON.stringify({
        data: [{ resource_id: 'r_1', target_url: 'https://internal.example', status: 'active' }],
        meta: { has_more: true, next_cursor: 'next' },
      })),
    });

    await expect(client.listResources()).resolves.toEqual({
      resources: [{ resourceId: 'r_1', targetUrl: 'https://internal.example', status: 'active' }],
      hasMore: true,
      nextCursor: 'next',
    });
  });

  it('preserves a continuation cursor when the server omits has_more', async () => {
    const client = new HttpQurlClient({
      endpoint: 'https://api.example.test',
      apiKey: 'secret',
      fetch: async () => new Response(JSON.stringify({
        data: [{ resource_id: 'r_1' }],
        meta: { next_cursor: 'next' },
      })),
    });
    await expect(client.listResources()).resolves.toEqual({ resources: [{ resourceId: 'r_1' }], nextCursor: 'next' });
  });

  it('sends kind-first connector enrollment credentials', async () => {
    let body = '';
    const client = new HttpQurlClient({
      endpoint: 'https://api.example.test',
      apiKey: 'secret',
      fetch: async (_input, init) => {
        body = String(init?.body ?? '');
        return new Response(JSON.stringify({ data: { key_id: 'key_1', api_key: 'bootstrap', kind: 'enrollment_token', target: 'agent', claims: [{ type: 'connector', id: 'prod' }] } }), { status: 201 });
      },
    });

    await expect(client.createEnrollmentToken('prod', 'idempotency')).resolves.toEqual({ keyId: 'key_1', apiKey: 'bootstrap' });
    expect(JSON.parse(body)).toMatchObject({ kind: 'enrollment_token', target: 'agent', claims: [{ type: 'connector', id: 'prod' }] });
  });

  it.each([
    { kind: undefined }, { kind: 'api_key' }, { target: undefined }, { target: 'connector' },
    { claims: undefined }, { claims: [] }, { claims: [{ type: 'connector', id: 'other' }] },
    { claims: [{ type: 'resource', id: 'prod' }] },
    { claims: [{ type: 'connector', id: 'prod' }, { type: 'connector', id: 'other' }] },
  ])('revokes enrollment credentials whose authority is not confirmed: %j', async override => {
    const methods: string[] = [];
    const controller = new AbortController();
    const client = new HttpQurlClient({
      endpoint: 'https://api.example.test', apiKey: 'secret',
      fetch: async (input, init) => {
        const request = new Request(input, init);
        methods.push(`${request.method} ${new URL(request.url).pathname}`);
        if (request.method === 'DELETE') return new Response(null, { status: 204 });
        // Even if the activity expires just after minting, cleanup must run.
        controller.abort();
        return new Response(JSON.stringify({ data: {
          key_id: 'key_1', api_key: 'must-never-be-delivered', kind: 'enrollment_token',
          target: 'agent', claims: [{ type: 'connector', id: 'prod' }], ...override,
        } }), { status: 201 });
      },
    });
    await expect(client.createEnrollmentToken('prod', 'attempt', controller.signal)).rejects.toThrow('enrollment credential');
    expect(methods).toEqual(['POST /v1/api-keys', 'DELETE /v1/api-keys/key_1']);
  });

  it('names got-vs-want so a stale qurl-service is diagnosable', async () => {
    const client = new HttpQurlClient({
      endpoint: 'https://api.example.test', apiKey: 'secret',
      // A qurl-service predating the kind-first API omits target and claims.
      fetch: async () => new Response(JSON.stringify({ data: { key_id: 'key_1', api_key: 'plaintext-secret', kind: 'enrollment_token' } }), { status: 201 }),
    });
    const failure = await client.createEnrollmentToken('prod', 'attempt').catch((error: Error) => error);
    expect(failure).toBeInstanceOf(Error);
    const message = (failure as Error).message;
    expect(message).toContain('may predate the kind-first API');
    expect(message).toContain('want "agent"');
    expect(message).toContain('want 1 of type "connector"');
    // The plaintext key must never reach an error string.
    expect(message).not.toContain('plaintext-secret');
  });

  it('treats repeated resource and API-key revocation as successful', async () => {
    const requests: string[] = [];
    const client = new HttpQurlClient({
      endpoint: 'https://api.example.test',
      apiKey: 'secret',
      fetch: async input => {
        requests.push((input instanceof Request ? new URL(input.url) : new URL(input)).pathname);
        return new Response(null, { status: 404 });
      },
    });

    await expect(client.deleteResource('resource')).resolves.toBeUndefined();
    await expect(client.revokeApiKey('key')).resolves.toBeUndefined();
    expect(requests).toEqual(['/v1/resources/resource', '/v1/api-keys/key']);
  });

  it('does not start a request with an already-aborted signal', async () => {
    let calls = 0;
    const client = new HttpQurlClient({
      endpoint: 'https://api.example.test',
      apiKey: 'secret',
      fetch: async () => {
        calls += 1;
        return new Response('{}');
      },
    });
    const controller = new AbortController();
    controller.abort();

    await expect(client.listResources(controller.signal)).rejects.toThrow('timed out or was cancelled');
    expect(calls).toBe(0);
  });

  it('maps a non-UTF-8 response body to a stable adapter error', async () => {
    const client = new HttpQurlClient({
      endpoint: 'https://api.example.test',
      apiKey: 'secret',
      fetch: async () => new Response(new Uint8Array([0xff])),
    });
    await expect(client.listResources()).rejects.toThrow('qURL response is invalid UTF-8');
  });
});
