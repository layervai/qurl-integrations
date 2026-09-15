import { describe, expect, it, vi } from 'vitest';
import { HttpQurlClient, QurlHttpError } from '../src/qurl-client.js';

// Matching public fixtures from qurl-conformance's resource_key_qv2_v01 vector.
const RESOURCE_KEY = 'MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEcOtuxu2qhc3gt1E7BiEU0CLqEDlXDwzZq0JnESgMAwERX6y_XXF5Cn5SKITWIZQmUhCZ0pHHlVn7SmFUTAnTGQ';
const RESOURCE_CRID = 'ae4jqpd7eaoslq7jinmjv4yikgzmcxgpjfsuobiniqnko32lpw743ivbeyha';

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

  it('retains the non-secret key ID when rejected enrollment authority cannot be revoked', async () => {
    const client = new HttpQurlClient({
      endpoint: 'https://api.example.test', apiKey: 'secret',
      fetch: async (_input, init) => init?.method === 'DELETE'
        ? new Response(null, { status: 403 })
        : new Response(JSON.stringify({ data: { key_id: 'key_cleanup', api_key: 'plaintext-secret', kind: 'enrollment_token' } }), { status: 201 }),
    });
    const error = await client.createEnrollmentToken('prod', 'attempt').catch((error: Error) => error);
    expect(error).toBeInstanceOf(Error);
    expect((error as Error).message).toContain('key_cleanup');
    expect((error as Error).message).not.toContain('plaintext-secret');
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

  it('accepts API not-found envelopes but rejects an HTML routing failure during revocation', async () => {
    for (const method of ['deleteResource', 'revokeApiKey'] as const) {
      const client = (body: string, contentType: string): HttpQurlClient => new HttpQurlClient({
        endpoint: 'https://api.example.test', apiKey: 'secret',
        fetch: async () => new Response(body, { status: 404, headers: { 'Content-Type': contentType } }),
      });
      await expect(client(JSON.stringify({ error: { code: 'not_found' } }), 'application/problem+json')[method]('id')).resolves.toBeUndefined();
      // An intermediary's HTML 404 does not establish that the upstream key
      // or resource is absent. Reject so callers retain their recovery rows.
      await expect(client('<html>private routing failure</html>', 'text/html')[method]('id')).rejects.toThrow('qURL response is invalid JSON');
    }
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
    await expect(client.getResource('public-crid', controller.signal)).rejects.toThrow('timed out or was cancelled');
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

  it.each([RESOURCE_CRID, RESOURCE_KEY])('reads resource details by %s while keeping its canonical key and CRID distinct', async id => {
    const requests: Request[] = [];
    const client = new HttpQurlClient({
      endpoint: 'https://api.example.test', apiKey: 'synthetic-account-key', userAgent: 'qurl-teams/1',
      fetch: async (input, init) => {
        requests.push(new Request(input, init));
        return new Response(JSON.stringify({ data: { resource: {
          resource_id: RESOURCE_KEY, crid: RESOURCE_CRID, type: 'url',
          target_url: 'https://protected.example.test', status: 'active',
        } } }));
      },
    });
    expect(typeof client.getResource).toBe('function');
    await expect(client.getResource(id)).resolves.toEqual({
      resourceId: RESOURCE_KEY, crid: RESOURCE_CRID, type: 'url',
      targetUrl: 'https://protected.example.test', status: 'active',
    });
    expect(requests[0]?.method).toBe('GET');
    expect(requests[0]?.url).toBe(`https://api.example.test/v1/resources/${id}`);
    expect(requests[0]?.headers.get('Authorization')).toBe('Bearer synthetic-account-key');
    expect(requests[0]?.headers.get('User-Agent')).toBe('qurl-teams/1');
  });

  it('accepts a service-resolved CRID during backfill before the resource row stores its CRID', async () => {
    const client = new HttpQurlClient({ endpoint: 'https://api.example.test', apiKey: 'secret', fetch: async () => new Response(JSON.stringify({ data: { resource: { resource_id: RESOURCE_KEY, type: 'url', status: 'revoked' } } })) });
    await expect(client.getResource(RESOURCE_CRID)).resolves.toEqual({ resourceId: RESOURCE_KEY, type: 'url', status: 'revoked' });
  });

  it.each([
    { requested: RESOURCE_CRID, returned: { resource_id: RESOURCE_KEY, crid: 'another-crid' } },
    { requested: RESOURCE_CRID, returned: { resource_id: RESOURCE_KEY, crid: '' } },
    { requested: RESOURCE_KEY, returned: { resource_id: 'another-key' } },
    { requested: 'not-a-crid', returned: { resource_id: RESOURCE_KEY } },
  ])('does not weaken the resource identity guard for $requested with $returned', async ({ requested, returned }) => {
    const client = new HttpQurlClient({ endpoint: 'https://api.example.test', apiKey: 'secret', fetch: async () => new Response(JSON.stringify({ data: { resource: returned } })) });
    await expect(client.getResource(requested)).rejects.toThrow('qURL resource identity does not match the request');
  });

  it.each([{ data: {} }, { data: { resource: { crid: 'public-crid' } } }])('rejects an incomplete resource detail envelope: %j', async body => {
    const client = new HttpQurlClient({ endpoint: 'https://api.example.test', apiKey: 'secret', fetch: async () => new Response(JSON.stringify(body)) });
    expect(typeof client.getResource).toBe('function');
    await expect(client.getResource('public-crid')).rejects.toThrow('qURL resource response is invalid');
  });

  it('preserves only safe service error metadata for a throttled request', async () => {
    const client = new HttpQurlClient({
      endpoint: 'https://api.example.test', apiKey: 'synthetic-account-key',
      fetch: async () => new Response(JSON.stringify({
        error: { code: 'rate_limited', title: 'private upstream title', detail: 'private upstream detail' },
        meta: { request_id: 'private upstream metadata' },
      }), { status: 429, headers: { 'Retry-After': '30' } }),
    });
    const failure: unknown = await client.listResources().catch((error: unknown) => error);
    expect(failure).toBeInstanceOf(QurlHttpError);
    expect(failure).toMatchObject({ status: 429, code: 'rate_limited', retryAfterSeconds: 30 });
    expect(String(failure)).toBe('QurlHttpError: qURL request failed (429)');
    expect(JSON.stringify(failure)).not.toContain('private upstream');
    expect(JSON.stringify(failure)).not.toContain('synthetic-account-key');
  });

  it.each([
    { status: 403, body: { error: { code: 'connector_disabled' } }, retry: '30', code: 'connector_disabled' },
    { status: 403, body: { error: { code: 'quota_exceeded' } }, retry: '', code: 'quota_exceeded' },
    { status: 500, body: { error: { code: 'upstream\nprivate-detail' } }, retry: '', code: undefined },
    { status: 500, body: { error: { code: 'x'.repeat(65) } }, retry: '', code: undefined },
    { status: 500, body: { error: { code: { private: 'detail' } } }, retry: '', code: undefined },
  ])('keeps status $status and sanitizes error code without accepting non-rate retry metadata', async ({ status, body, retry, code }) => {
    const client = new HttpQurlClient({ endpoint: 'https://api.example.test', apiKey: 'secret', fetch: async () => new Response(JSON.stringify(body), { status, headers: { 'Retry-After': retry } }) });
    const failure: unknown = await client.listResources().catch((error: unknown) => error);
    expect(failure).toBeInstanceOf(QurlHttpError);
    expect(failure).toMatchObject({ status });
    expect((failure as QurlHttpError).code).toBe(code);
    expect((failure as QurlHttpError).retryAfterSeconds).toBeUndefined();
    expect(String(failure)).not.toContain('private');
  });

  it.each(['', '-1', '1.5', '1e3', 'Wed, 21 Oct 2015 07:28:00 GMT', '9007199254740992'])('ignores unsupported Retry-After seconds: %s', async retry => {
    const client = new HttpQurlClient({ endpoint: 'https://api.example.test', apiKey: 'secret', fetch: async () => new Response('private non-JSON error', { status: 429, headers: { 'Retry-After': retry } }) });
    const failure: unknown = await client.listResources().catch((error: unknown) => error);
    expect(failure).toBeInstanceOf(QurlHttpError);
    expect(failure).toMatchObject({ status: 429 });
    expect((failure as QurlHttpError).retryAfterSeconds).toBeUndefined();
    expect(String(failure)).not.toContain('private');
  });

  it.each(['fetch', 'body'])('bounds an in-flight %s with the existing request deadline', async phase => {
    vi.useFakeTimers();
    let aborted = false;
    const client = new HttpQurlClient({
      endpoint: 'https://api.example.test', apiKey: 'secret',
      fetch: async (_input, init) => {
        const signal = init?.signal;
        if (phase === 'fetch') return await new Promise<Response>((_resolve, reject) => {
          signal?.addEventListener('abort', () => { aborted = true; reject(new Error('synthetic timeout')); }, { once: true });
        });
        return new Response(new ReadableStream<Uint8Array>({
          start(controller) {
            signal?.addEventListener('abort', () => { aborted = true; controller.error(new Error('synthetic timeout')); }, { once: true });
          },
        }));
      },
    });
    try {
      const rejected = expect(client.listResources()).rejects.toThrow('qURL request timed out or was cancelled');
      await vi.advanceTimersByTimeAsync(15_000);
      await rejected;
      expect(aborted).toBe(true);
    } finally { vi.useRealTimers(); }
  });

  it('rejects an oversized resource response before decoding it', async () => {
    const client = new HttpQurlClient({ endpoint: 'https://api.example.test', apiKey: 'secret', fetch: async () => new Response(' '.repeat(1_048_577)) });
    await expect(client.listResources()).rejects.toThrow('qURL response exceeded the configured size limit');
  });
});
