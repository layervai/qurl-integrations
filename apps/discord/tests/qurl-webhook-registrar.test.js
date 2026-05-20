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
    // Try exact match first (so tests targeting a specific query string
    // like `?cursor=page2` still work), then fall back to pathname-only
    // so tests don't have to enumerate every `?limit=100` / `?limit=100&cursor=...`
    // variation that the registrar adds for defensive reasons.
    const pathnameOnly = path.split('?')[0];
    const handler = handlers[`${method} ${path}`] || handlers[`${method} ${pathnameOnly}`];
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

  it('returns reused successfully even when the events PATCH fails (transient 5xx must not flip the boot log)', async () => {
    // Receiver is already correct via initialSecret. A transient PATCH
    // 5xx shouldn't make ensureWebhookSubscription reject — the boot
    // log would then say "self-registration failed" while the bot is
    // actually healthy. Catch + log + return reused.
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing', url: BASE_OPTS.bridgeUrl, events: ['qurl.created'], // drift
      }] } }),
      'PATCH /v1/webhooks/wh_existing': () => ({ status: 500, body: { error: 'transient' } }),
    });
    const result = await ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'whsec_known' });
    expect(result.secret).toBe('whsec_known');
    expect(result.action).toBe('reused');
    expect(result.webhookId).toBe('wh_existing');
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
    // Test keys spell the exact path the registrar produces so the
    // exact-match path-with-query takes precedence over the
    // pathname-only fallback (which would otherwise return the
    // first-page handler for every call → cursor walk would loop).
    mockFetchResponses({
      'GET /v1/webhooks?limit=100': () => ({ body: {
        data: [{ webhook_id: 'wh_other', url: 'https://other.example/foo', events: ['qurl.accessed'] }],
        meta: { next_cursor: 'page2', has_more: true },
      } }),
      'GET /v1/webhooks?cursor=page2&limit=100': () => ({ body: {
        data: [{ webhook_id: 'wh_match', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed'] }],
        meta: { next_cursor: '', has_more: false },
      } }),
    });
    const result = await ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'whsec_known' });
    expect(result.webhookId).toBe('wh_match');
    expect(result.action).toBe('reused');
  });

  it('returns [] (→ creates fresh) when cursor walk exhausts without any match', async () => {
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
    await expect(ensureWebhookSubscription(BASE_OPTS)).rejects.toThrow(/contract drift/);
  });

  it('throws if create response has no data envelope (contract drift)', async () => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [] } }),
      'POST /v1/webhooks': () => ({ status: 201, body: { /* no data */ } }),
    });
    await expect(ensureWebhookSubscription(BASE_OPTS)).rejects.toThrow(/contract drift/);
  });

  it('throws if rotate response has no secret (contract drift)', async () => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed'],
      }] } }),
      'POST /v1/webhooks/wh_existing/secret': () => ({ body: { data: { webhook_id: 'wh_existing' /* no secret */ } } }),
    });
    await expect(ensureWebhookSubscription(BASE_OPTS)).rejects.toThrow(/rotateSecret.*contract drift/);
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

describe('ensureWebhookSubscription — description length defense', () => {
  // Slice lives at the wire boundary (createSubscription) so future
  // callers don't have to remember the 200-char cap. Defense against
  // a hypothetical future qurl-service-side length-cap 4xx that
  // would otherwise infinite-loop on retry-create.
  it('clips description to 200 chars at the wire boundary regardless of caller input', async () => {
    let sentDescription = null;
    const longDescription = 'x'.repeat(500);
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [] } }),
      'POST /v1/webhooks': (opts) => {
        sentDescription = JSON.parse(opts.body).description;
        return { status: 201, body: { data: { webhook_id: 'wh_clipped', secret: 'whsec_' } } };
      },
    });
    await ensureWebhookSubscription({ ...BASE_OPTS, description: longDescription });
    expect(sentDescription).toHaveLength(200);
    expect(sentDescription).toBe('x'.repeat(200));
  });

  it('treats non-string description as empty', async () => {
    let sentDescription = null;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [] } }),
      'POST /v1/webhooks': (opts) => {
        sentDescription = JSON.parse(opts.body).description;
        return { status: 201, body: { data: { webhook_id: 'wh', secret: 'whsec_' } } };
      },
    });
    await ensureWebhookSubscription({ ...BASE_OPTS, description: undefined });
    expect(sentDescription).toBe('');
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

describe('ensureWebhookSubscription — duplicate-subscription recovery', () => {
  // Cold-bootstrap with N replicas + empty SSM creates N duplicate subs
  // (each replica POSTs concurrently). This path tests RECOVERY on the
  // next boot: pick deterministic survivor, DELETE others, continue.
  it('deletes duplicates and keeps oldest-by-created_at survivor (force-rotates the survivor)', async () => {
    const deletedIds = [];
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        { webhook_id: 'wh_b', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed'], created_at: '2026-05-19T12:00:00Z' },
        { webhook_id: 'wh_a', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed'], created_at: '2026-05-19T10:00:00Z' }, // older — survivor
        { webhook_id: 'wh_c', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed'], created_at: '2026-05-19T14:00:00Z' },
      ] } }),
      'DELETE /v1/webhooks/wh_b': () => { deletedIds.push('wh_b'); return { status: 204, body: '' }; },
      'DELETE /v1/webhooks/wh_c': () => { deletedIds.push('wh_c'); return { status: 204, body: '' }; },
      'POST /v1/webhooks/wh_a/secret': () => ({ body: { data: { webhook_id: 'wh_a', secret: 'whsec_rot' } } }),
    });
    const result = await ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'whsec_known' });
    expect(result.webhookId).toBe('wh_a');
    expect(result.action).toBe('rotated'); // dedupe always force-rotates
    expect(deletedIds.sort()).toEqual(['wh_b', 'wh_c']);
  });

  it('falls back to lexicographic webhook_id when created_at is absent (no replica-identity coupling)', async () => {
    const deletedIds = [];
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        { webhook_id: 'wh_zzz', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed'] },
        { webhook_id: 'wh_aaa', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed'] }, // lex-first — survivor
      ] } }),
      'DELETE /v1/webhooks/wh_zzz': () => { deletedIds.push('wh_zzz'); return { status: 204, body: '' }; },
      'POST /v1/webhooks/wh_aaa/secret': () => ({ body: { data: { webhook_id: 'wh_aaa', secret: 'whsec_rot' } } }),
    });
    const result = await ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'whsec_known' });
    expect(result.webhookId).toBe('wh_aaa');
    expect(deletedIds).toEqual(['wh_zzz']);
  });

  it('treats DELETE 404 as success (concurrent dedupe race)', async () => {
    // Two replicas independently picking the same survivor + DELETEing
    // the same losers means the second DELETE on each loser hits 404.
    // The dedupe path must NOT crash on this.
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        { webhook_id: 'wh_a', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed'] },
        { webhook_id: 'wh_b', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed'] },
      ] } }),
      'DELETE /v1/webhooks/wh_b': () => ({ status: 404, body: { error: 'not found' } }),
      'POST /v1/webhooks/wh_a/secret': () => ({ body: { data: { webhook_id: 'wh_a', secret: 'whsec_rot' } } }),
    });
    const result = await ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'whsec_known' });
    expect(result.webhookId).toBe('wh_a');
    expect(result.action).toBe('rotated');
  });

  it('force-rotates the survivor even when initialSecret is real (closes SSM↔survivor mismatch)', async () => {
    // Cold-bootstrap created N subs each with distinct server-generated
    // secrets. The SSM-persisted secret (last-write-wins) almost
    // certainly belongs to a replica whose sub we just DELETEd. If we
    // took the REUSE path with initialSecret, the receiver would 401
    // every inbound forever (survivor's secret is unknown). Force-
    // rotate produces a known-good secret tied to the survivor.
    let rotated = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        { webhook_id: 'wh_a', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed'], created_at: '2026-05-19T10:00:00Z' }, // survivor
        { webhook_id: 'wh_b', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed'], created_at: '2026-05-19T11:00:00Z' },
      ] } }),
      'DELETE /v1/webhooks/wh_b': () => ({ status: 204, body: '' }),
      'POST /v1/webhooks/wh_a/secret': () => {
        rotated = true;
        return { body: { data: { webhook_id: 'wh_a', secret: 'whsec_post_dedupe' } } };
      },
    });
    const result = await ensureWebhookSubscription({
      ...BASE_OPTS,
      initialSecret: 'whsec_was_in_ssm_but_for_wh_b',
    });
    expect(rotated).toBe(true);
    expect(result.webhookId).toBe('wh_a');
    expect(result.action).toBe('rotated'); // NOT 'reused' — dedupe forces rotate
    expect(result.secret).toBe('whsec_post_dedupe');
    expect(result.secret).not.toBe('whsec_was_in_ssm_but_for_wh_b');
  });

  it('propagates non-404 DELETE errors so a real failure surfaces', async () => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        { webhook_id: 'wh_a', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed'] },
        { webhook_id: 'wh_b', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed'] },
      ] } }),
      'DELETE /v1/webhooks/wh_b': () => ({ status: 500, body: { error: 'oops' } }),
    });
    await expect(ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'whsec_known' })).rejects.toThrow(/500/);
  });
});

describe('ensureWebhookSubscription — real config integration (setQurlWebhookSecret)', () => {
  // The fakeConfig-based mutation-seam test (above) keeps the unit
  // hermetic but pins a synthesized shape. This test exercises the
  // REAL `setQurlWebhookSecret` from src/config.js so a future rename
  // or signature change is caught loudly here instead of at boot.
  it('writing through the real config setter updates the canonical export', () => {
    const config = require('../src/config');
    const prev = config.QURL_WEBHOOK_SECRET;
    try {
      config.setQurlWebhookSecret('whsec_integration_test');
      expect(config.QURL_WEBHOOK_SECRET).toBe('whsec_integration_test');
    } finally {
      config.setQurlWebhookSecret(prev);
    }
  });

  it('works even when destructured-and-called (closure-captures module.exports, not `this`)', () => {
    const { setQurlWebhookSecret } = require('../src/config');
    const config = require('../src/config');
    const prev = config.QURL_WEBHOOK_SECRET;
    try {
      // Pre-fix shorthand `setQurlWebhookSecret(secret) { this.X = secret }`
      // would silently TypeError here because `this` is undefined in
      // strict-mode standalone calls.
      setQurlWebhookSecret('whsec_destructured');
      expect(config.QURL_WEBHOOK_SECRET).toBe('whsec_destructured');
    } finally {
      setQurlWebhookSecret(prev);
    }
  });
});

describe('ensureWebhookSubscription — multi-subscription scan', () => {
  it('matches the correct sub when several non-matching ones share the page', async () => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        { webhook_id: 'wh_other_1', url: 'https://elsewhere.example/hook1', events: ['qurl.accessed'] },
        { webhook_id: 'wh_match',   url: BASE_OPTS.bridgeUrl,              events: ['qurl.accessed'] },
        { webhook_id: 'wh_other_2', url: 'https://elsewhere.example/hook2', events: ['qurl.accessed'] },
      ] } }),
    });
    const result = await ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'whsec_known' });
    expect(result.webhookId).toBe('wh_match');
    expect(result.action).toBe('reused');
  });
});

describe('ensureWebhookSubscription — rotation survives PATCH failure', () => {
  it('returns the rotated secret even when the events PATCH fails', async () => {
    // Behavior pin: if PATCH ran before rotate and threw, the bot
    // would never get a usable secret on this boot — receiver stays on
    // PLACEHOLDER, 503s forever. Asserting the rotated secret made it
    // out implicitly proves rotation ran (and succeeded) despite the
    // PATCH 500. Doesn't pin call order, so a future refactor that
    // makes rotation+PATCH independent still passes.
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing', url: BASE_OPTS.bridgeUrl, events: ['qurl.created'], // drift
      }] } }),
      'POST /v1/webhooks/wh_existing/secret': () => ({
        body: { data: { webhook_id: 'wh_existing', secret: 'whsec_rotated_ok' } },
      }),
      'PATCH /v1/webhooks/wh_existing': () => ({ status: 500, body: { error: 'transient' } }),
    });
    const result = await ensureWebhookSubscription(BASE_OPTS);
    expect(result.secret).toBe('whsec_rotated_ok');
    expect(result.action).toBe('rotated');
  });
});

describe('ensureWebhookSubscription — events drift edge cases', () => {
  it('triggers PATCH when existing.events is undefined (regression guard for Array.isArray(undefined))', async () => {
    let patched = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing', url: BASE_OPTS.bridgeUrl, // no events field at all
      }] } }),
      'PATCH /v1/webhooks/wh_existing': () => { patched = true; return { body: { data: {} } }; },
    });
    await ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'whsec_known' });
    expect(patched).toBe(true);
  });
});

describe('ensureWebhookSubscription — pagination cap', () => {
  it('throws when the 50-page cap is hit (refuses to fall through to create-fresh)', async () => {
    // Silently returning null + creating-fresh would compound duplicates
    // on every restart with a stuck cursor.
    global.fetch = jest.fn(async () => ({
      ok: true, status: 200,
      text: async () => JSON.stringify({
        data: [{ webhook_id: 'wh_other', url: 'https://other.example', events: [] }],
        meta: { next_cursor: 'never-ends', has_more: true },
      }),
    }));
    await expect(ensureWebhookSubscription(BASE_OPTS)).rejects.toThrow(/pagination cap/i);
  });
});

describe('ensureWebhookSubscription — fetch timeout', () => {
  it('surfaces AbortError when qurl-service hangs past the 10s deadline', async () => {
    // The registrar relies on AbortSignal.timeout(10_000) — verify the
    // surface is an error, not a hung promise. Replaces the awaited
    // fetch with one that throws an AbortError synchronously to avoid
    // real-time waits in tests.
    const abortErr = new Error('The operation was aborted');
    abortErr.name = 'AbortError';
    global.fetch = jest.fn(async () => { throw abortErr; });
    await expect(ensureWebhookSubscription(BASE_OPTS)).rejects.toThrow(/aborted/i);
  });
  it('attaches `op` to pre-response fetch errors so oncall greps catch network failures too', async () => {
    // callQurlService used to surface only resp-status errors with op;
    // pre-response failures (AbortError, DNS, TLS) lacked the op field,
    // so a "filter logs by op=GET /v1/webhooks" search silently missed
    // network errors. Now op is attached at the catch site.
    const abortErr = new Error('The operation was aborted');
    abortErr.name = 'AbortError';
    global.fetch = jest.fn(async () => { throw abortErr; });
    let caught;
    try {
      await ensureWebhookSubscription(BASE_OPTS);
    } catch (err) { caught = err; }
    expect(caught).toBeDefined();
    expect(caught.op).toBe('GET /v1/webhooks?limit=100');
  });
});

describe('redactSecret — recursive scrubbing', () => {
  const { redactSecret } = _internals;
  it('scrubs nested secret fields at any depth', () => {
    const out = redactSecret({ data: { webhook: { secret: 'whsec_leaked', other: 'fine' } } });
    expect(out.data.webhook.secret).toBe('[REDACTED]');
    expect(out.data.webhook.other).toBe('fine');
  });
  it('scrubs secret-shaped key variants (webhook_secret, signing_secret, client_secret)', () => {
    const out = redactSecret({
      webhook_secret: 'whsec_a',
      signing_secret: 'whsec_b',
      client_secret: 'whsec_c',
      regular_field: 'fine',
    });
    expect(out.webhook_secret).toBe('[REDACTED]');
    expect(out.signing_secret).toBe('[REDACTED]');
    expect(out.client_secret).toBe('[REDACTED]');
    expect(out.regular_field).toBe('fine');
  });
  it('scrubs secrets inside arrays of objects', () => {
    const out = redactSecret({ data: [{ secret: 'whsec_one' }, { secret: 'whsec_two' }] });
    expect(out.data[0].secret).toBe('[REDACTED]');
    expect(out.data[1].secret).toBe('[REDACTED]');
  });
  it('leaves non-secret leaf values intact', () => {
    const out = redactSecret({ a: 1, b: 'x', c: null, d: false });
    expect(out).toEqual({ a: 1, b: 'x', c: null, d: false });
  });
});

describe('pickSurvivor — deterministic across replicas', () => {
  const { pickSurvivor } = _internals;
  it('returns null on empty input', () => {
    expect(pickSurvivor([])).toBeNull();
  });
  it('returns the single match when only one', () => {
    expect(pickSurvivor([{ webhook_id: 'x' }])).toEqual({ webhook_id: 'x' });
  });
  it('picks oldest created_at when both present', () => {
    const winner = pickSurvivor([
      { webhook_id: 'wh_z', created_at: '2026-05-19T12:00:00Z' },
      { webhook_id: 'wh_a', created_at: '2026-05-19T10:00:00Z' },
    ]);
    expect(winner.webhook_id).toBe('wh_a');
  });
  it('falls back to lex webhook_id when created_at is missing', () => {
    const winner = pickSurvivor([
      { webhook_id: 'wh_zzz' },
      { webhook_id: 'wh_aaa' },
    ]);
    expect(winner.webhook_id).toBe('wh_aaa');
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
