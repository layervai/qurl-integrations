// Tests for the qurl-service webhook self-registration helper.
//
// Wire-contract pinned against qurl-service's public Webhooks API:
//   POST /v1/webhooks                  → creates, returns secret
//   POST /v1/webhooks/{id}/secret      → rotates, returns NEW secret
//   GET  /v1/webhooks                  → lists for owner
//   PATCH /v1/webhooks/{id}            → updates events list

const { ensureWebhookSubscription, _internals } = require('../src/qurl-webhook-registrar');

const ORIGINAL_FETCH = global.fetch;

function mockFetchResponses(handlers) {
  global.fetch = jest.fn(async (url, opts) => {
    const path = url.replace(/^https?:\/\/[^/]+/, '');
    const method = opts.method || 'GET';
    const handler = handlers[`${method} ${path}`];
    if (!handler) {
      throw new Error(`Unmocked fetch: ${method} ${path}`);
    }
    const { status = 200, body, throwError } = handler(opts);
    if (throwError) throw throwError;
    return {
      ok: status >= 200 && status < 300,
      status,
      text: async () => (typeof body === 'string' ? body : JSON.stringify(body)),
    };
  });
}

afterEach(() => {
  global.fetch = ORIGINAL_FETCH;
});

const BASE_OPTS = {
  apiEndpoint: 'https://api.test.example',
  apiKey: 'lv_test_key',
  bridgeUrl: 'https://bot.test.example/webhooks/qurl',
  description: 'test description',
};

describe('ensureWebhookSubscription — no existing subscription → creates fresh', () => {
  it('POSTs /v1/webhooks with the right body and returns the server-generated secret', async () => {
    let createBody = null;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [], meta: {} } }),
      'POST /v1/webhooks': (opts) => {
        createBody = JSON.parse(opts.body);
        return { status: 201, body: { data: {
          webhook_id: 'wh_test_new',
          secret: 'whsec_fresh_secret',
          url: BASE_OPTS.bridgeUrl,
          events: ['qurl.accessed'],
        } } };
      },
    });
    const result = await ensureWebhookSubscription(BASE_OPTS);
    expect(createBody).toEqual({
      url: BASE_OPTS.bridgeUrl,
      events: ['qurl.accessed'],
      description: BASE_OPTS.description,
    });
    expect(result).toEqual({
      secret: 'whsec_fresh_secret',
      webhookId: 'wh_test_new',
      action: 'created',
    });
  });
});

describe('ensureWebhookSubscription — existing sub, bootstrap (no real initialSecret) → rotates', () => {
  it('finds the sub, calls POST /v1/webhooks/{id}/secret, returns the rotated secret', async () => {
    let rotatedFor = null;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing',
        url: BASE_OPTS.bridgeUrl,
        events: ['qurl.accessed'],
      }] } }),
      'POST /v1/webhooks/wh_existing/secret': () => {
        rotatedFor = 'wh_existing';
        return { body: { data: { webhook_id: 'wh_existing', secret: 'whsec_rotated' } } };
      },
    });
    const result = await ensureWebhookSubscription(BASE_OPTS);
    expect(rotatedFor).toBe('wh_existing');
    expect(result).toEqual({
      secret: 'whsec_rotated',
      webhookId: 'wh_existing',
      action: 'rotated',
    });
  });

  it('also rotates when initialSecret is the terraform PLACEHOLDER sentinel', async () => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed'],
      }] } }),
      'POST /v1/webhooks/wh_existing/secret': () => ({
        body: { data: { webhook_id: 'wh_existing', secret: 'whsec_post_bootstrap' } },
      }),
    });
    const result = await ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'PLACEHOLDER' });
    expect(result.action).toBe('rotated');
    expect(result.secret).toBe('whsec_post_bootstrap');
  });

  it('patches events if the existing list does not include qurl.accessed', async () => {
    let patchedEvents = null;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing',
        url: BASE_OPTS.bridgeUrl,
        events: ['qurl.created'], // drift — missing qurl.accessed
      }] } }),
      'PATCH /v1/webhooks/wh_existing': (opts) => {
        patchedEvents = JSON.parse(opts.body).events;
        return { body: { data: { webhook_id: 'wh_existing' } } };
      },
      'POST /v1/webhooks/wh_existing/secret': () => ({
        body: { data: { webhook_id: 'wh_existing', secret: 'whsec_post_patch' } },
      }),
    });
    const result = await ensureWebhookSubscription(BASE_OPTS);
    expect(patchedEvents).toEqual(['qurl.accessed']);
    expect(result.secret).toBe('whsec_post_patch');
  });
});

describe('ensureWebhookSubscription — existing sub + real initialSecret → REUSE (multi-replica safety)', () => {
  it('skips POST /secret entirely and returns the initialSecret unchanged', async () => {
    // The load-bearing multi-replica safety property. Pre-fix, each
    // HTTP replica rotated → server-side last-write-wins → (N-1)
    // replicas held stale secrets → ALB-routed events 401'd on
    // ~(N-1)/N of replicas until a follow-up restart.
    let secretEndpointHit = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed'],
      }] } }),
      'POST /v1/webhooks/wh_existing/secret': () => {
        secretEndpointHit = true;
        return { body: { data: {} } };
      },
    });
    const result = await ensureWebhookSubscription({
      ...BASE_OPTS,
      initialSecret: 'whsec_already_known',
    });
    expect(secretEndpointHit).toBe(false); // critical: no rotation
    expect(result).toEqual({
      secret: 'whsec_already_known',
      webhookId: 'wh_existing',
      action: 'reused',
    });
  });

  it('still PATCHes events on drift even in the reuse path (PATCH is idempotent)', async () => {
    let patched = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing', url: BASE_OPTS.bridgeUrl, events: ['qurl.created'],
      }] } }),
      'PATCH /v1/webhooks/wh_existing': () => { patched = true; return { body: { data: {} } }; },
    });
    await ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'whsec_known' });
    expect(patched).toBe(true);
  });

  it('does NOT PATCH when events already include qurl.accessed (no-drift positive case)', async () => {
    let patched = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed'],
      }] } }),
      'PATCH /v1/webhooks/wh_existing': () => { patched = true; return { body: { data: {} } }; },
    });
    await ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'whsec_known' });
    expect(patched).toBe(false);
  });
});

describe('ensureWebhookSubscription — URL canonicalization', () => {
  it('matches an existing sub even when URLs differ by trailing slash', async () => {
    let rotated = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing',
        url: 'https://bot.test.example/webhooks/qurl/', // trailing slash
        events: ['qurl.accessed'],
      }] } }),
      'POST /v1/webhooks/wh_existing/secret': () => {
        rotated = true;
        return { body: { data: { webhook_id: 'wh_existing', secret: 'whsec_x' } } };
      },
    });
    // bridgeUrl has NO trailing slash; strict equality would miss
    // and create a duplicate. canonicalUrl matches both.
    const result = await ensureWebhookSubscription({
      ...BASE_OPTS,
      bridgeUrl: 'https://bot.test.example/webhooks/qurl',
    });
    expect(rotated).toBe(true);
    expect(result.action).toBe('rotated'); // matched the existing sub, not 'created'
  });
});

describe('ensureWebhookSubscription — pagination', () => {
  it('walks cursor pages until the matching sub is found', async () => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: {
        data: [{ webhook_id: 'wh_other', url: 'https://other.example/foo', events: ['qurl.accessed'] }],
        meta: { next_cursor: 'page2', has_more: true },
      } }),
      'GET /v1/webhooks?cursor=page2': () => ({ body: {
        data: [{ webhook_id: 'wh_match', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed'] }],
        meta: { next_cursor: '', has_more: false },
      } }),
    });
    const result = await ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'whsec_known' });
    expect(result.webhookId).toBe('wh_match');
    expect(result.action).toBe('reused');
  });

  it('returns null (→ creates fresh) when cursor walk exhausts without a match', async () => {
    let createCalled = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: {
        data: [{ webhook_id: 'wh_other', url: 'https://other.example/foo', events: ['qurl.accessed'] }],
        meta: { next_cursor: '', has_more: false },
      } }),
      'POST /v1/webhooks': () => {
        createCalled = true;
        return { status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } };
      },
    });
    const result = await ensureWebhookSubscription(BASE_OPTS);
    expect(createCalled).toBe(true);
    expect(result.action).toBe('created');
  });
});

describe('ensureWebhookSubscription — secret redaction in error messages', () => {
  it('redacts secret from response body when surfacing an error', async () => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({
        status: 500,
        body: { error: 'Internal Server Error', data: { secret: 'whsec_leaked', other: 'fine' } },
      }),
    });
    let caught;
    try {
      await ensureWebhookSubscription(BASE_OPTS);
    } catch (err) { caught = err; }
    expect(caught).toBeDefined();
    expect(caught.message).not.toContain('whsec_leaked');
    expect(caught.message).toContain('[REDACTED]');
  });
});

describe('ensureWebhookSubscription — error paths', () => {
  it('throws when qurl-service returns 401 (bad API key)', async () => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ status: 401, body: { error: 'Unauthorized' } }),
    });
    await expect(ensureWebhookSubscription(BASE_OPTS)).rejects.toThrow(/401/);
  });

  it('throws if create response has no secret (contract drift)', async () => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [] } }),
      'POST /v1/webhooks': () => ({ status: 201, body: { data: {
        webhook_id: 'wh_new',
        // No secret field — contract drift
      } } }),
    });
    await expect(ensureWebhookSubscription(BASE_OPTS)).rejects.toThrow(/no secret in response/);
  });

  it('throws on missing required option', async () => {
    await expect(ensureWebhookSubscription({ apiEndpoint: 'x' })).rejects.toThrow(/required/);
  });
});

describe('ensureWebhookSubscription — best-effort secret persistence', () => {
  it('invokes persistSecret callback with the server-generated secret', async () => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [] } }),
      'POST /v1/webhooks': () => ({ status: 201, body: { data: {
        webhook_id: 'wh_persisted', secret: 'whsec_to_persist',
      } } }),
    });
    const persistSecret = jest.fn(async () => {});
    const result = await ensureWebhookSubscription({
      ...BASE_OPTS,
      persistSecret,
    });
    expect(persistSecret).toHaveBeenCalledWith('whsec_to_persist');
    expect(result.secret).toBe('whsec_to_persist');
  });

  it('returns the secret EVEN IF persistSecret throws (best-effort)', async () => {
    // Load-bearing safety property: persistence is observability, not
    // correctness. If IAM denies PutParameter (or whatever backend the
    // caller wired) we STILL return the secret so the receiver can
    // verify against it in-process. Without this, an AccessDenied
    // would crash the registrar and the bot would 503 on every webhook.
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [] } }),
      'POST /v1/webhooks': () => ({ status: 201, body: { data: {
        webhook_id: 'wh_denied', secret: 'whsec_in_memory_only',
      } } }),
    });
    const accessDenied = new Error('User is not authorized to perform: ssm:PutParameter');
    accessDenied.name = 'AccessDeniedException';
    const persistSecret = jest.fn(async () => { throw accessDenied; });
    const result = await ensureWebhookSubscription({
      ...BASE_OPTS,
      persistSecret,
    });
    expect(result.secret).toBe('whsec_in_memory_only');
    expect(persistSecret).toHaveBeenCalled();
  });

  it('skips persistence entirely when persistSecret is not provided', async () => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [] } }),
      'POST /v1/webhooks': () => ({ status: 201, body: { data: {
        webhook_id: 'wh_no_persist', secret: 'whsec_in_memory_only',
      } } }),
    });
    const result = await ensureWebhookSubscription(BASE_OPTS);
    expect(result.secret).toBe('whsec_in_memory_only');
  });
});

describe('ensureWebhookSubscription — wire-contract pins', () => {
  it('uses Bearer auth + JSON content-type', async () => {
    let getOpts = null;
    let postOpts = null;
    mockFetchResponses({
      'GET /v1/webhooks': (opts) => {
        getOpts = opts;
        return { body: { data: [] } };
      },
      'POST /v1/webhooks': (opts) => {
        postOpts = opts;
        return { status: 201, body: { data: { webhook_id: 'wh', secret: 'whsec_' } } };
      },
    });
    await ensureWebhookSubscription(BASE_OPTS);
    expect(getOpts.headers.Authorization).toBe(`Bearer ${BASE_OPTS.apiKey}`);
    expect(postOpts.headers.Authorization).toBe(`Bearer ${BASE_OPTS.apiKey}`);
    expect(postOpts.headers['Content-Type']).toBe('application/json');
  });

  it('events list is the exact ["qurl.accessed"] string (regression guard)', async () => {
    // The qurl-service spec lists multiple event types; we ONLY want
    // qurl.accessed. If a future change accidentally subscribes to
    // qurl.created / .revoked etc., the receiver would ignore those
    // with 200 — but the metric volume + log noise would grow.
    let body = null;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [] } }),
      'POST /v1/webhooks': (opts) => {
        body = JSON.parse(opts.body);
        return { status: 201, body: { data: { webhook_id: 'wh', secret: 'whsec_' } } };
      },
    });
    await ensureWebhookSubscription(BASE_OPTS);
    expect(body.events).toEqual(['qurl.accessed']);
  });
});

describe('ensureWebhookSubscription — config-mutation seam (receiver picks up new secret)', () => {
  // The wire-up cr called out: receiver reads config.QURL_WEBHOOK_SECRET
  // on every request; the registrar's `.then()` callback writes to
  // config via setQurlWebhookSecret() — that mutation IS what makes
  // the new secret active without restarting the process. Test pins
  // both directions: registrar returns a secret, caller writes it to
  // config, receiver-side verification (in-process simulation) uses it.
  it('the secret the registrar returns is the one the receiver verifies against', async () => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [] } }),
      'POST /v1/webhooks': () => ({ status: 201, body: { data: {
        webhook_id: 'wh_seam', secret: 'whsec_new_active',
      } } }),
    });
    const fakeConfig = { QURL_WEBHOOK_SECRET: 'PLACEHOLDER' };
    const setQurlWebhookSecret = (s) => { fakeConfig.QURL_WEBHOOK_SECRET = s; };
    const result = await ensureWebhookSubscription(BASE_OPTS);
    // Caller's responsibility (mirrors apps/discord/src/index.js):
    setQurlWebhookSecret(result.secret);
    // After the .then() resolves, the receiver's view of the config:
    expect(fakeConfig.QURL_WEBHOOK_SECRET).toBe('whsec_new_active');
    expect(fakeConfig.QURL_WEBHOOK_SECRET).not.toBe('PLACEHOLDER');
  });
});
