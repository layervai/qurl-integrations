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

    await expect(client.create({ resourceId: 'r_1', oneTimeUse: true })).resolves.toMatchObject({ resourceId: 'r_1' });
    expect(requests[0]?.url).toBe('https://api.example.test/v1/resources/r_1/qurls');
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
        return new Response(JSON.stringify({ data: { key_id: 'key_1', api_key: 'bootstrap' } }), { status: 201 });
      },
    });

    await expect(client.createEnrollmentToken('prod', 'idempotency')).resolves.toEqual({ keyId: 'key_1', apiKey: 'bootstrap' });
    expect(JSON.parse(body)).toMatchObject({ kind: 'enrollment_token', target: 'connector', claims: [{ type: 'connector', id: 'prod' }] });
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
});
