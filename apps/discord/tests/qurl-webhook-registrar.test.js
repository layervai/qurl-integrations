// Tests for the qurl-service webhook self-registration helper.
//
// Wire-contract pinned against qurl-service's public Webhooks API:
//   POST /v1/webhooks                  → creates, returns secret
//   POST /v1/webhooks/{id}/secret      → rotates, returns NEW secret
//   GET  /v1/webhooks                  → lists for owner
//   PATCH /v1/webhooks/{id}            → updates events list

const { ensureWebhookSubscription, buildSsmPersistSecret, _internals } = require('../src/qurl-webhook-registrar');

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

describe('ensureWebhookSubscription — cold bootstrap (no existing sub + no real initialSecret) → creates', () => {
  // The first-deploy-of-a-fresh-environment path. `initialSecret` is
  // either unset (env never had QURL_WEBHOOK_SECRET) or an empty
  // string (SSM parameter not yet populated). Either way, action='created'.
  it.each([
    ['initialSecret undefined', undefined],
    ['initialSecret empty string', ''],
  ])('creates a fresh subscription when no existing matches the bridge URL — %s', async (_label, initialSecret) => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [] } }),
      'POST /v1/webhooks': () => ({ status: 201, body: { data: {
        webhook_id: 'wh_cold_bootstrap',
        secret: 'whsec_fresh',
        url: BASE_OPTS.bridgeUrl,
        events: ['qurl.accessed', 'qurl.expired'],
      } } }),
    });
    const result = await ensureWebhookSubscription({ ...BASE_OPTS, initialSecret });
    expect(result.action).toBe('created');
    expect(result.webhookId).toBe('wh_cold_bootstrap');
    expect(result.secret).toBe('whsec_fresh');
  });
});

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
          events: ['qurl.accessed', 'qurl.expired'],
        } } };
      },
    });
    const result = await ensureWebhookSubscription(BASE_OPTS);
    expect(createBody).toEqual({
      url: BASE_OPTS.bridgeUrl,
      events: ['qurl.accessed', 'qurl.expired'],
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
        events: ['qurl.accessed', 'qurl.expired'],
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

  it('also rotates when initialSecret is the empty string (SSM param not yet populated)', async () => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'],
      }] } }),
      'POST /v1/webhooks/wh_existing/secret': () => ({
        body: { data: { webhook_id: 'wh_existing', secret: 'whsec_post_bootstrap' } },
      }),
    });
    const result = await ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: '' });
    expect(result.action).toBe('rotated');
    expect(result.secret).toBe('whsec_post_bootstrap');
  });

  it('patches events to the target set if the existing list is missing any target event', async () => {
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
    expect(patchedEvents).toEqual(['qurl.accessed', 'qurl.expired']);
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
        webhook_id: 'wh_existing', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'],
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

  it('does NOT PATCH when events already match the target set (no-drift positive case)', async () => {
    let patched = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'],
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
        events: ['qurl.accessed', 'qurl.expired'],
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
        data: [{ webhook_id: 'wh_other', url: 'https://other.example/foo', events: ['qurl.accessed', 'qurl.expired'] }],
        meta: { next_cursor: 'page2', has_more: true },
      } }),
      'GET /v1/webhooks?cursor=page2&limit=100': () => ({ body: {
        data: [{ webhook_id: 'wh_match', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'] }],
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
        data: [{ webhook_id: 'wh_other', url: 'https://other.example/foo', events: ['qurl.accessed', 'qurl.expired'] }],
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

describe('ensureWebhookSubscription — set-based reconcileEvents (latent bug fix)', () => {
  // The pre-fix reconcile short-circuited on `events.includes('qurl.accessed')`,
  // which would leave an `accessed`-only subscription in place even after
  // the target set grew to include `expired`. These tests pin the
  // strict-set-equality semantics so a future event-list addition can
  // never silently under-cover via the inclusion-check pattern again.
  it('PATCHes when accessed is present but expired is missing (the original latent-bug shape)', async () => {
    let patchedEvents = null;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed'],
      }] } }),
      'PATCH /v1/webhooks/wh_existing': (opts) => {
        patchedEvents = JSON.parse(opts.body).events;
        return { body: { data: { webhook_id: 'wh_existing' } } };
      },
    });
    await ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'whsec_known' });
    expect(patchedEvents).toEqual(['qurl.accessed', 'qurl.expired']);
  });

  it('PATCHes when expired is present but accessed is missing (symmetric case)', async () => {
    let patchedEvents = null;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing', url: BASE_OPTS.bridgeUrl, events: ['qurl.expired'],
      }] } }),
      'PATCH /v1/webhooks/wh_existing': (opts) => {
        patchedEvents = JSON.parse(opts.body).events;
        return { body: { data: { webhook_id: 'wh_existing' } } };
      },
    });
    await ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'whsec_known' });
    expect(patchedEvents).toEqual(['qurl.accessed', 'qurl.expired']);
  });

  it('PATCHes when an extra non-target event is present (set equality, not subset)', async () => {
    // Set equality drops drift extras — a stale event from a removed
    // target stays subscribed otherwise. Trade-off accepted: if a
    // future peer ever co-subscribes a third event on the same sub,
    // this would drop it. There is no co-subscriber today (the bot
    // owns this subscription, owner_id-scoped).
    let patchedEvents = null;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing', url: BASE_OPTS.bridgeUrl,
        events: ['qurl.accessed', 'qurl.expired', 'qurl.stale_event'],
      }] } }),
      'PATCH /v1/webhooks/wh_existing': (opts) => {
        patchedEvents = JSON.parse(opts.body).events;
        return { body: { data: { webhook_id: 'wh_existing' } } };
      },
    });
    await ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'whsec_known' });
    expect(patchedEvents).toEqual(['qurl.accessed', 'qurl.expired']);
  });

  it('does NOT PATCH when target events are present in different order (order-independent)', async () => {
    let patched = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing', url: BASE_OPTS.bridgeUrl,
        events: ['qurl.expired', 'qurl.accessed'], // reversed
      }] } }),
      'PATCH /v1/webhooks/wh_existing': () => { patched = true; return { body: { data: {} } }; },
    });
    await ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'whsec_known' });
    expect(patched).toBe(false);
  });

  it('PATCHes when events field is a string instead of an array (treats non-array as missing)', async () => {
    // Defensive against a future contract drift where qurl-service
    // returns events: "qurl.accessed,qurl.expired" — the pre-fix
    // .includes() would have matched via string-contains and
    // silently skipped the PATCH despite drift.
    let patchedEvents = null;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing', url: BASE_OPTS.bridgeUrl,
        events: 'qurl.accessed,qurl.expired',
      }] } }),
      'PATCH /v1/webhooks/wh_existing': (opts) => {
        patchedEvents = JSON.parse(opts.body).events;
        return { body: { data: { webhook_id: 'wh_existing' } } };
      },
    });
    await ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'whsec_known' });
    expect(patchedEvents).toEqual(['qurl.accessed', 'qurl.expired']);
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
        webhook_id: 'wh_existing', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'],
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

  it('logs at WARN when persistSecret throws AccessDeniedException (expected IAM-missing path)', async () => {
    const warnSpy = jest.spyOn(console, 'warn').mockImplementation(() => {});
    const errorSpy = jest.spyOn(console, 'error').mockImplementation(() => {});
    try {
      mockFetchResponses({
        'GET /v1/webhooks': () => ({ body: { data: [] } }),
        'POST /v1/webhooks': () => ({ status: 201, body: { data: {
          webhook_id: 'wh', secret: 'whsec_',
        } } }),
      });
      const accessDenied = new Error('User is not authorized to perform: ssm:PutParameter');
      accessDenied.name = 'AccessDeniedException';
      await ensureWebhookSubscription({
        ...BASE_OPTS,
        persistSecret: async () => { throw accessDenied; },
      });
      expect(warnSpy).toHaveBeenCalledWith(expect.stringContaining('qURL webhook secret persistence failed'));
      // Critical: NOT error-level. AccessDenied is the "expected
      // failure mode" — alarm-tier-distinction documented in runbook.
      expect(errorSpy).not.toHaveBeenCalledWith(expect.stringContaining('qURL webhook secret persistence failed'));
    } finally {
      warnSpy.mockRestore();
      errorSpy.mockRestore();
    }
  });

  it('logs at ERROR when persistSecret throws anything OTHER than AccessDeniedException (unexpected — alarm-worthy)', async () => {
    const warnSpy = jest.spyOn(console, 'warn').mockImplementation(() => {});
    const errorSpy = jest.spyOn(console, 'error').mockImplementation(() => {});
    try {
      mockFetchResponses({
        'GET /v1/webhooks': () => ({ body: { data: [] } }),
        'POST /v1/webhooks': () => ({ status: 201, body: { data: {
          webhook_id: 'wh', secret: 'whsec_',
        } } }),
      });
      const throttle = new Error('Rate exceeded');
      throttle.name = 'ThrottlingException';
      await ensureWebhookSubscription({
        ...BASE_OPTS,
        persistSecret: async () => { throw throttle; },
      });
      expect(errorSpy).toHaveBeenCalledWith(expect.stringContaining('qURL webhook secret persistence failed'));
      expect(warnSpy).not.toHaveBeenCalledWith(expect.stringContaining('qURL webhook secret persistence failed'));
    } finally {
      warnSpy.mockRestore();
      errorSpy.mockRestore();
    }
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
    expect(body.events).toEqual(['qurl.accessed', 'qurl.expired']);
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
        { webhook_id: 'wh_b', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'], created_at: '2026-05-19T12:00:00Z' },
        { webhook_id: 'wh_a', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'], created_at: '2026-05-19T10:00:00Z' }, // older — survivor
        { webhook_id: 'wh_c', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'], created_at: '2026-05-19T14:00:00Z' },
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
        { webhook_id: 'wh_zzz', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'] },
        { webhook_id: 'wh_aaa', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'] }, // lex-first — survivor
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
        { webhook_id: 'wh_a', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'] },
        { webhook_id: 'wh_b', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'] },
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
        { webhook_id: 'wh_a', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'], created_at: '2026-05-19T10:00:00Z' }, // survivor
        { webhook_id: 'wh_b', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'], created_at: '2026-05-19T11:00:00Z' },
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
        { webhook_id: 'wh_a', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'] },
        { webhook_id: 'wh_b', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'] },
      ] } }),
      'DELETE /v1/webhooks/wh_b': () => ({ status: 500, body: { error: 'oops' } }),
    });
    await expect(ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'whsec_known' })).rejects.toThrow(/500/);
  });

  it('rotation does NOT fire when a non-404 DELETE rejects (Promise.all sequencing)', async () => {
    // The rotate-after-dedupe path runs `await Promise.all(...DELETEs)`
    // before `rotateSecret`. A rejection there must short-circuit the
    // rotate so we don't ship a rotated secret while siblings might
    // still be in-flight or partially failed. Pin the invariant.
    let rotateHit = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        { webhook_id: 'wh_a', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'] },
        { webhook_id: 'wh_b', url: BASE_OPTS.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'] },
      ] } }),
      'DELETE /v1/webhooks/wh_b': () => ({ status: 500, body: { error: 'oops' } }),
      'POST /v1/webhooks/wh_a/secret': () => {
        rotateHit = true;
        return { body: { data: { webhook_id: 'wh_a', secret: 'whsec_should_not_happen' } } };
      },
    });
    await expect(ensureWebhookSubscription({ ...BASE_OPTS, initialSecret: 'whsec_known' })).rejects.toThrow(/500/);
    expect(rotateHit).toBe(false);
  });
});

// Real-config setter tests removed: webhook-registrar Lambda is the
// sole writer of QURL_WEBHOOK_SECRET (via SSM). The bot reads it
// once at boot from env and never mutates it in-process, so there
// is no setter seam to pin.

describe('ensureWebhookSubscription — multi-subscription scan', () => {
  it('matches the correct sub when several non-matching ones share the page', async () => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        { webhook_id: 'wh_other_1', url: 'https://elsewhere.example/hook1', events: ['qurl.accessed', 'qurl.expired'] },
        { webhook_id: 'wh_match',   url: BASE_OPTS.bridgeUrl,              events: ['qurl.accessed', 'qurl.expired'] },
        { webhook_id: 'wh_other_2', url: 'https://elsewhere.example/hook2', events: ['qurl.accessed', 'qurl.expired'] },
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
    // would never get a usable secret on this boot — receiver stays
    // unconfigured and 503s every webhook. Asserting the rotated
    // secret made it out implicitly proves rotation ran (and
    // succeeded) despite the PATCH 500. Doesn't pin call order, so
    // a future refactor that makes rotation+PATCH independent still
    // passes.
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
  it('fail-closes at depth cap with [TRUNCATED] (deeply-wrapped secret cannot survive)', () => {
    // Build a body with a `secret` field deeper than REDACT_MAX_DEPTH (8).
    let nested = { secret: 'whsec_deeply_nested' };
    for (let i = 0; i < 10; i++) nested = { wrap: nested };
    const out = redactSecret(nested);
    // Walk to the truncation point and check the subtree was replaced
    // by '[TRUNCATED]' instead of the original {secret: ...} subtree.
    const json = JSON.stringify(out);
    expect(json).not.toContain('whsec_deeply_nested');
    expect(json).toContain('[TRUNCATED]');
  });
});

describe('buildSsmPersistSecret — abortSignal placement (regression guard for cr round-8)', () => {
  it('passes abortSignal on send\'s SECOND arg, not on the Command constructor (constructor would silently drop it)', async () => {
    // The bug round-8 caught: putting {abortSignal: ...} as the second
    // arg to `new PutParameterCommand({...}, ...)` is silently dropped
    // because the Command takes only `input`. Has to land on
    // `client.send(cmd, { abortSignal })`.
    let sendCalls = [];
    const fakeSsmClient = { send: jest.fn(async (cmd, opts) => { sendCalls.push({ cmd, opts }); }) };
    class FakePutParameterCommand {
      constructor(input) { this.input = input; }
    }
    const persist = buildSsmPersistSecret({
      ssmClient: fakeSsmClient,
      paramName: '/test/QURL_WEBHOOK_SECRET',
      PutParameterCommand: FakePutParameterCommand,
    });
    await persist('whsec_new_value');
    expect(sendCalls).toHaveLength(1);
    expect(sendCalls[0].cmd.input).toEqual({
      Name: '/test/QURL_WEBHOOK_SECRET',
      Type: 'SecureString',
      Value: 'whsec_new_value',
      Overwrite: true,
    });
    // Critical assertion — abortSignal lives on the send-call options,
    // NOT swallowed by the Command constructor.
    expect(sendCalls[0].opts).toEqual({ abortSignal: expect.any(AbortSignal) });
    expect(sendCalls[0].opts.abortSignal.aborted).toBe(false);
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
  it('prefers the row with a timestamp over the row without (mixed case)', () => {
    // Asymmetric responses (one row has created_at, the other doesn't)
    // should resolve to the timestamped row regardless of input order.
    const winner1 = pickSurvivor([
      { webhook_id: 'wh_zzz', created_at: '2026-05-19T10:00:00Z' },
      { webhook_id: 'wh_aaa' /* no timestamp */ },
    ]);
    expect(winner1.webhook_id).toBe('wh_zzz');
    const winner2 = pickSurvivor([
      { webhook_id: 'wh_aaa' /* no timestamp */ },
      { webhook_id: 'wh_zzz', created_at: '2026-05-19T10:00:00Z' },
    ]);
    expect(winner2.webhook_id).toBe('wh_zzz');
  });
});

describe('ensureWebhookSubscription — return-shape pin (Lambda persists then bot reads from SSM)', () => {
  // Lambda flow: ensureWebhookSubscription returns a secret →
  // persistSecret callback writes it to SSM → bot reads it from env
  // at next deploy. This test pins the return-shape contract that the
  // Lambda relies on: the secret in result.secret is exactly what the
  // bot will end up verifying webhooks against.
  it('the secret the registrar returns matches the value the persistSecret callback receives', async () => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [] } }),
      'POST /v1/webhooks': () => ({ status: 201, body: { data: {
        webhook_id: 'wh_seam', secret: 'whsec_new_active',
      } } }),
    });
    const persisted = [];
    const result = await ensureWebhookSubscription({
      ...BASE_OPTS,
      persistSecret: async (s) => { persisted.push(s); },
    });
    expect(result.secret).toBe('whsec_new_active');
    expect(persisted).toEqual(['whsec_new_active']);
  });
});

describe('ensureWebhookSubscription — ownerId return field (per-guild receiver routing)', () => {
  // guild-webhook-link.js consumes result.ownerId to populate the
  // in-process secret cache. If qurl-service drops `owner_id` from a
  // response shape, every BYOK guild's first link rolls back with
  // OWNER_MISSING — pin the field across all three branches so the
  // upstream contract regression fails loudly here.
  it('forwards owner_id from a POST /v1/webhooks created response', async () => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [] } }),
      'POST /v1/webhooks': () => ({ status: 201, body: { data: {
        webhook_id: 'wh_new', secret: 'whsec_x', owner_id: 'auth0|created',
      } } }),
    });
    const result = await ensureWebhookSubscription({ ...BASE_OPTS });
    expect(result.action).toBe('created');
    expect(result.ownerId).toBe('auth0|created');
  });

  it('forwards owner_id from the existing-sub rotate path', async () => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing',
        url: BASE_OPTS.bridgeUrl,
        events: ['qurl.accessed', 'qurl.expired'],
        owner_id: 'auth0|existing',
      }] } }),
      'POST /v1/webhooks/wh_existing/secret': () => ({ status: 200, body: { data: {
        webhook_id: 'wh_existing', secret: 'whsec_rotated',
      } } }),
    });
    const result = await ensureWebhookSubscription({ ...BASE_OPTS });
    expect(result.action).toBe('rotated');
    expect(result.ownerId).toBe('auth0|existing');
  });

  it('forwards owner_id from the reuse path (initialSecret + existing sub)', async () => {
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_reused',
        url: BASE_OPTS.bridgeUrl,
        events: ['qurl.accessed', 'qurl.expired'],
        owner_id: 'auth0|reused',
      }] } }),
    });
    const result = await ensureWebhookSubscription({
      ...BASE_OPTS, initialSecret: 'whsec_already_known',
    });
    expect(result.action).toBe('reused');
    expect(result.ownerId).toBe('auth0|reused');
  });

  it('leaks undefined when a future contract drift drops owner_id (caller must guard)', async () => {
    // The guild-webhook-link OWNER_MISSING rollback catches this.
    // Pinning the leakage here makes a contract regression LOUD.
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [] } }),
      'POST /v1/webhooks': () => ({ status: 201, body: { data: {
        webhook_id: 'wh_no_owner', secret: 'whsec_y', // no owner_id
      } } }),
    });
    const result = await ensureWebhookSubscription({ ...BASE_OPTS });
    expect(result.ownerId).toBeUndefined();
    expect(result.secret).toBe('whsec_y');
    expect(result.webhookId).toBe('wh_no_owner');
  });
});

describe('ensureWebhookSubscription — URL-migration orphan cleanup (cross-host sweep)', () => {
  // Symptom motivating this block: sandbox `base_url` rename from
  // `discord.layerv.xyz` → `discord.connector.layerv.xyz` left an
  // orphan sub (wh_s6wOhbKLPYSk--Jv) alive forever. qurl-service
  // kept delivering to the old host (DNS still resolved to the same
  // ALB) and every delivery failed sig-verification at the bot.
  //
  // The sweep runs BEFORE the find/reuse/rotate/create branching (so a
  // transient orphan-DELETE 5xx on one boot is retried on every
  // subsequent boot, not just the next create-fresh one — that "retry"
  // window would otherwise close the moment the new sub is created).
  // Cross-host detection uses host inequality; description-prefix +
  // boundary anchoring keeps sibling-service subs (e.g. qurl-s3-connector)
  // out of scope even though they share owner_id with the bot under
  // today's bot-API-key-shared model (see project_qurl_api_key_blast_radius).
  // A liveness gate (last_delivery_success === false) prevents the sweep
  // from cannibalizing a healthy cross-host sibling (e.g. an active-
  // active multi-region deploy sharing a QURL_API_KEY).
  const NEW_URL = 'https://discord.connector.layerv.xyz/webhooks/qurl';
  const OLD_URL = 'https://discord.layerv.xyz/webhooks/qurl';
  const BOT_DESC = 'Discord bot view counter (region=us-east-2, env=sandbox)';
  const BOT_OPTS = { ...BASE_OPTS, bridgeUrl: NEW_URL, description: BOT_DESC };

  // Default orphan shape used across the cases below — matches all
  // sweep criteria (cross-host, same path, description-prefix, dead).
  function deadOrphan(overrides = {}) {
    return {
      webhook_id: 'wh_orphan',
      url: OLD_URL,
      description: BOT_DESC,
      events: ['qurl.accessed', 'qurl.expired'],
      failure_count: 1475,
      last_delivery_success: false,
      ...overrides,
    };
  }

  it('deletes a cross-host orphan with matching description + dead liveness before creating fresh at the new URL', async () => {
    let orphanDeleted = false;
    let createCalled = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [deadOrphan()] } }),
      'DELETE /v1/webhooks/wh_orphan': () => { orphanDeleted = true; return { status: 204, body: '' }; },
      'POST /v1/webhooks': () => {
        createCalled = true;
        return { status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } };
      },
    });
    const result = await ensureWebhookSubscription(BOT_OPTS);
    expect(orphanDeleted).toBe(true);
    expect(createCalled).toBe(true);
    expect(result.action).toBe('created');
    expect(result.webhookId).toBe('wh_new');
  });

  it('does NOT delete a cross-host sub with a DIFFERENT description (sibling-service safety)', async () => {
    // The bot's QURL_API_KEY today provisions BOTH the view-counter sub
    // (this bot) AND sibling-service subs (e.g. qurl-s3-connector's
    // `resource.closed` subscription). They share owner_id. The
    // description-prefix filter is the load-bearing safety that keeps
    // the orphan sweep from deleting them.
    let connectorDeleted = false;
    let createCalled = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        {
          webhook_id: 'wh_connector',
          url: 'https://s3-connector.layerv.xyz/webhooks/qurl', // different host, same path
          description: 'qurl-s3-connector resource.closed subscription',
          events: ['qurl.accessed'],
          failure_count: 0,
          last_delivery_success: true,
        },
      ] } }),
      'DELETE /v1/webhooks/wh_connector': () => { connectorDeleted = true; return { status: 204, body: '' }; },
      'POST /v1/webhooks': () => {
        createCalled = true;
        return { status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } };
      },
    });
    const result = await ensureWebhookSubscription(BOT_OPTS);
    expect(connectorDeleted).toBe(false); // critical: connector sub untouched
    expect(createCalled).toBe(true);
    expect(result.action).toBe('created');
  });

  it('sweeps a stale cross-host orphan even when the reuse path runs at the new URL (retry-on-next-boot)', async () => {
    // The sweep runs BEFORE branching, so a transient DELETE 5xx on a
    // previous create-fresh boot still gets retried on every subsequent
    // boot via this very path. Without the hoist, that retry window
    // would close the moment the new sub is created.
    let orphanDeleted = false;
    let secretEndpointHit = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        // Match at the new URL (with matching description) — reuse path.
        {
          webhook_id: 'wh_current',
          url: NEW_URL,
          description: BOT_DESC,
          events: ['qurl.accessed', 'qurl.expired'],
          owner_id: 'auth0|bot',
        },
        // Cross-host orphan from a previous rename — still dead and
        // never cleaned up. Sweep must still pick it up here.
        deadOrphan({ webhook_id: 'wh_stale_orphan' }),
      ] } }),
      'DELETE /v1/webhooks/wh_stale_orphan': () => { orphanDeleted = true; return { status: 204, body: '' }; },
      'POST /v1/webhooks/wh_current/secret': () => {
        secretEndpointHit = true;
        return { body: { data: { webhook_id: 'wh_current', secret: 'whsec_rot' } } };
      },
    });
    const result = await ensureWebhookSubscription(BOT_OPTS);
    expect(orphanDeleted).toBe(true); // sweep ran despite existing sub at new URL
    expect(secretEndpointHit).toBe(true); // normal rotate path also ran
    expect(result.webhookId).toBe('wh_current');
  });

  it('does NOT delete a HEALTHY cross-host sub (liveness gate protects siblings whose last delivery succeeded)', async () => {
    // Active-active multi-region under a shared QURL_API_KEY is the
    // hypothetical motivating scenario, but the gate only protects
    // siblings whose LAST delivery succeeded (or hasn't happened yet).
    // It does NOT protect a sibling in a sustained outage — see the
    // long comment above buildUrlMigrationOrphanFilter for why these
    // signals fundamentally can't distinguish that case.
    // Today's deployment is single-host so this is purely defensive.
    let deleted = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        {
          webhook_id: 'wh_other_region',
          url: 'https://discord.eu-central-1.layerv.xyz/webhooks/qurl', // different host, same path, same description prefix
          description: 'Discord bot view counter (region=eu-central-1, env=sandbox)',
          events: ['qurl.accessed', 'qurl.expired'],
          failure_count: 0,
          last_delivery_success: true, // healthy
        },
      ] } }),
      'DELETE /v1/webhooks/wh_other_region': () => { deleted = true; return { status: 204, body: '' }; },
      'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
    });
    const result = await ensureWebhookSubscription(BOT_OPTS);
    expect(deleted).toBe(false); // critical: healthy active-active sibling untouched
    expect(result.action).toBe('created');
  });

  it.each([
    ['null', null],
    ['undefined', undefined],
    ['missing entirely (field omitted)', '__MISSING__'],
  ])('does NOT delete a cross-host sub with last_delivery_success=%s (presumed-alive)', async (_label, livenessValue) => {
    // A brand-new sub with no deliveries yet typically reports null/undefined.
    // Strict `=== false` gate treats that as presumed-alive — better to miss
    // an orphan than to false-positive delete a freshly-created sibling.
    let deleted = false;
    const sub = {
      webhook_id: 'wh_no_deliveries_yet',
      url: 'https://otherhost.example/webhooks/qurl',
      description: BOT_DESC,
      events: ['qurl.accessed'],
      failure_count: 0,
    };
    if (livenessValue !== '__MISSING__') sub.last_delivery_success = livenessValue;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [sub] } }),
      'DELETE /v1/webhooks/wh_no_deliveries_yet': () => { deleted = true; return { status: 204, body: '' }; },
      'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
    });
    await ensureWebhookSubscription(BOT_OPTS);
    expect(deleted).toBe(false);
  });

  it('does NOT delete a description that matches the prefix without the " (" boundary (no over-match)', async () => {
    // The boundary anchor turns the prefix into a full-segment match.
    // Without it, `startsWith("Discord bot view counter")` would over-
    // match a sibling like `Discord bot view counter-archiver (...)`
    // or `Discord bot view counterX`.
    let deleted = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        {
          webhook_id: 'wh_overmatch',
          url: OLD_URL,
          description: 'Discord bot view counterX (env=sandbox)',
          events: ['qurl.accessed'],
          failure_count: 100,
          last_delivery_success: false,
        },
      ] } }),
      'DELETE /v1/webhooks/wh_overmatch': () => { deleted = true; return { status: 204, body: '' }; },
      'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
    });
    await ensureWebhookSubscription(BOT_OPTS);
    expect(deleted).toBe(false);
  });

  it('continues with create when a DELETE fails (5xx) so the bot still registers; next boot retries the orphan', async () => {
    // Load-bearing failure semantics: blocking the create on a stale-
    // orphan DELETE failure would leave the bot UN-registered AND
    // orphaned — strictly worse than the orphan-only state we started
    // in. Log + continue + create. The hoisted sweep means the next
    // boot ALSO retries the orphan delete (not gated on create-fresh).
    const errorSpy = jest.spyOn(console, 'error').mockImplementation(() => {});
    let createCalled = false;
    try {
      mockFetchResponses({
        'GET /v1/webhooks': () => ({ body: { data: [
          deadOrphan({ webhook_id: 'wh_orphan_5xx' }),
        ] } }),
        'DELETE /v1/webhooks/wh_orphan_5xx': () => ({ status: 503, body: { error: 'transient' } }),
        'POST /v1/webhooks': () => {
          createCalled = true;
          return { status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } };
        },
      });
      const result = await ensureWebhookSubscription(BOT_OPTS);
      expect(createCalled).toBe(true);
      expect(result.action).toBe('created');
      expect(result.webhookId).toBe('wh_new');
      expect(errorSpy).toHaveBeenCalledWith(expect.stringContaining('URL-migration orphan delete failed'));
    } finally {
      errorSpy.mockRestore();
    }
  });

  it('retries a previously-failed orphan DELETE on the FOLLOWING boot (sweep is not gated on create-fresh)', async () => {
    // Pins the hoisted-cleanup invariant directly: after a successful
    // create on boot 1 (orphan DELETE 5xx-swallowed), boot 2 finds
    // BOTH the just-created sub AND the still-alive orphan. Sweep
    // re-attempts the orphan DELETE here — closing the recurrence
    // window the cr-bot flagged.
    let orphanDeletedOnBoot2 = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        // Boot 2's listing: new sub from boot 1 + orphan still alive.
        { webhook_id: 'wh_new', url: NEW_URL, description: BOT_DESC, events: ['qurl.accessed', 'qurl.expired'], owner_id: 'auth0|bot' },
        deadOrphan({ webhook_id: 'wh_orphan_retry' }),
      ] } }),
      'DELETE /v1/webhooks/wh_orphan_retry': () => { orphanDeletedOnBoot2 = true; return { status: 204, body: '' }; },
      // Boot 2 takes reuse path (initialSecret + existing match) — no
      // secret rotate, but events PATCH might run if drift, which it
      // doesn't here.
    });
    const result = await ensureWebhookSubscription({ ...BOT_OPTS, initialSecret: 'whsec_known' });
    expect(orphanDeletedOnBoot2).toBe(true);
    expect(result.action).toBe('reused');
  });

  it('treats DELETE 404 as success AND distinguishes the log line (concurrent cleanup by another invocation)', async () => {
    // 404 propagates through deleteSubscription as a no-throw. The
    // log line is the "already-absent" variant so the runbook-grep
    // on `URL-migration orphan deleted` doesn't get false-attributed
    // to this invocation when another beat us to the DELETE.
    const logSpy = jest.spyOn(console, 'log').mockImplementation(() => {});
    try {
      mockFetchResponses({
        'GET /v1/webhooks': () => ({ body: { data: [
          deadOrphan({ webhook_id: 'wh_already_gone' }),
        ] } }),
        'DELETE /v1/webhooks/wh_already_gone': () => ({ status: 404, body: { error: 'not found' } }),
        'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
      });
      const result = await ensureWebhookSubscription(BOT_OPTS);
      expect(result.action).toBe('created');
      expect(result.webhookId).toBe('wh_new');
      const lines = logSpy.mock.calls.map(c => c[0]).filter(l => typeof l === 'string');
      const absentLine = lines.find(l => l.includes('URL-migration orphan already absent'));
      const deletedLine = lines.find(l => l.includes('URL-migration orphan deleted'));
      expect(absentLine).toBeDefined();
      expect(deletedLine).toBeUndefined(); // critical: no false attribution
    } finally {
      logSpy.mockRestore();
    }
  });

  it('does NOT classify same-hostname different-port as cross-host (port-insensitive comparison)', async () => {
    // A port-flip rename (`:8080` → `:8443`, or implicit-443 vs
    // explicit-:443) at the same hostname is NOT a URL-migration:
    // the new port still resolves to the same backend, no orphan
    // accrues. Pin that `urlHost` uses `hostname` (port-excluded)
    // not `host` (port-included). Without this, a sub at
    // `https://discord.layerv.xyz:443/webhooks/qurl` could be
    // classified as cross-host against the unparametrized
    // `https://discord.layerv.xyz/webhooks/qurl` and falsely swept.
    let deleted = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        {
          webhook_id: 'wh_same_host_diff_port',
          // Same hostname as NEW_URL (`discord.connector.layerv.xyz`),
          // but with an explicit `:8443`.
          url: 'https://discord.connector.layerv.xyz:8443/webhooks/qurl',
          description: BOT_DESC,
          events: ['qurl.accessed'],
          failure_count: 9999,
          last_delivery_success: false,
        },
      ] } }),
      'DELETE /v1/webhooks/wh_same_host_diff_port': () => { deleted = true; return { status: 204, body: '' }; },
      'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
    });
    await ensureWebhookSubscription(BOT_OPTS);
    expect(deleted).toBe(false);
  });

  it('does NOT touch same-host subs at a DIFFERENT path (path filter)', async () => {
    // A bot rev that ever served `/webhooks/qurl/v2` (hypothetical)
    // could collide here. Pin that the path filter excludes any sub
    // whose pathname differs from the new bridge URL's pathname.
    let deleted = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        {
          webhook_id: 'wh_other_path',
          url: 'https://example.test/webhooks/something-else',
          description: BOT_DESC, // same prefix
          events: ['qurl.accessed'],
          failure_count: 100,
          last_delivery_success: false,
        },
      ] } }),
      'DELETE /v1/webhooks/wh_other_path': () => { deleted = true; return { status: 204, body: '' }; },
      'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
    });
    const result = await ensureWebhookSubscription(BOT_OPTS);
    expect(deleted).toBe(false);
    expect(result.action).toBe('created');
  });

  it('logs each deletion at INFO with old_url + webhook_id + description + failure_count + last_delivery_success', async () => {
    // The runbook-grep contract for "what got cleaned up" — pin the
    // field set so a future log-shape regression surfaces here. The
    // logger emits `[ts] INFO: <msg> <json-of-meta>` via console.log,
    // so we capture console.log and parse the meta JSON tail.
    const logSpy = jest.spyOn(console, 'log').mockImplementation(() => {});
    try {
      mockFetchResponses({
        'GET /v1/webhooks': () => ({ body: { data: [
          deadOrphan({ webhook_id: 'wh_orphan_log', failure_count: 99 }),
        ] } }),
        'DELETE /v1/webhooks/wh_orphan_log': () => ({ status: 204, body: '' }),
        'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
      });
      await ensureWebhookSubscription(BOT_OPTS);
      const orphanLine = logSpy.mock.calls
        .map(c => c[0])
        .find(line => typeof line === 'string' && line.includes('URL-migration orphan deleted'));
      expect(orphanLine).toBeDefined();
      // Pull the JSON meta off the end of the formatted line and verify
      // every required field is present with the expected value.
      const jsonStart = orphanLine.indexOf('{');
      const meta = JSON.parse(orphanLine.slice(jsonStart));
      expect(meta).toMatchObject({
        old_url: OLD_URL,
        webhook_id: 'wh_orphan_log',
        description: BOT_DESC,
        failure_count: 99,
        last_delivery_success: false,
      });
    } finally {
      logSpy.mockRestore();
    }
  });

  it('paginates through orphan candidates (cursor walk on the sweep list, same as findExistingSubscriptions)', async () => {
    // Sweep uses the same list endpoint; if the bot's owner_id has many
    // subs, the orphan could be on page 2+. Pin the cursor walk.
    let deletedIds = [];
    mockFetchResponses({
      'GET /v1/webhooks?limit=100': () => ({ body: {
        data: [{ webhook_id: 'wh_unrelated', url: 'https://other.example/hook', description: 'other service', events: [] }],
        meta: { next_cursor: 'page2', has_more: true },
      } }),
      'GET /v1/webhooks?cursor=page2&limit=100': () => ({ body: {
        data: [deadOrphan({ webhook_id: 'wh_orphan_paged' })],
        meta: { next_cursor: '', has_more: false },
      } }),
      'DELETE /v1/webhooks/wh_orphan_paged': () => { deletedIds.push('wh_orphan_paged'); return { status: 204, body: '' }; },
      'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
    });
    const result = await ensureWebhookSubscription(BOT_OPTS);
    expect(deletedIds).toEqual(['wh_orphan_paged']);
    expect(result.action).toBe('created');
  });

  it('deletes MULTIPLE cross-host orphans (multi-rename history) before create', async () => {
    // Two sequential renames in the past — both leave orphans. Sweep
    // handles both in one pass.
    const deletedIds = [];
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        deadOrphan({ webhook_id: 'wh_orphan_a', url: 'https://oldhost-a.example/webhooks/qurl' }),
        deadOrphan({ webhook_id: 'wh_orphan_b', url: 'https://oldhost-b.example/webhooks/qurl' }),
      ] } }),
      'DELETE /v1/webhooks/wh_orphan_a': () => { deletedIds.push('wh_orphan_a'); return { status: 204, body: '' }; },
      'DELETE /v1/webhooks/wh_orphan_b': () => { deletedIds.push('wh_orphan_b'); return { status: 204, body: '' }; },
      'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
    });
    const result = await ensureWebhookSubscription(BOT_OPTS);
    expect(deletedIds.sort()).toEqual(['wh_orphan_a', 'wh_orphan_b']);
    expect(result.action).toBe('created');
  });

  it('a single DELETE 5xx does not prevent the OTHER orphan from being deleted in the same sweep', async () => {
    // Per-orphan failure isolation — one bad apple shouldn't shadow the
    // rest. Sequential loop with per-iteration catch is the design.
    const errorSpy = jest.spyOn(console, 'error').mockImplementation(() => {});
    const deletedIds = [];
    try {
      mockFetchResponses({
        'GET /v1/webhooks': () => ({ body: { data: [
          deadOrphan({ webhook_id: 'wh_5xx', url: 'https://oldhost-a.example/webhooks/qurl' }),
          deadOrphan({ webhook_id: 'wh_ok',  url: 'https://oldhost-b.example/webhooks/qurl' }),
        ] } }),
        'DELETE /v1/webhooks/wh_5xx': () => ({ status: 503, body: { error: 'transient' } }),
        'DELETE /v1/webhooks/wh_ok':  () => { deletedIds.push('wh_ok');  return { status: 204, body: '' }; },
        'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
      });
      const result = await ensureWebhookSubscription(BOT_OPTS);
      expect(deletedIds).toEqual(['wh_ok']);
      expect(result.action).toBe('created');
    } finally {
      errorSpy.mockRestore();
    }
  });

  it('does NOT delete a cross-host sub with last_delivery_success=false but failure_count below the transient-failure floor', async () => {
    // The compound liveness gate (last_delivery_success === false AND
    // failure_count >= URL_MIGRATION_ORPHAN_MIN_FAILURES) tolerates a
    // single transient delivery failure on an otherwise-healthy
    // sibling. Without the floor, a network blip on one delivery would
    // flip last_delivery_success to false and a peer reboot in that
    // window would DELETE the live sub.
    let deleted = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        {
          webhook_id: 'wh_transient',
          url: 'https://discord.eu-central-1.layerv.xyz/webhooks/qurl',
          description: BOT_DESC,
          events: ['qurl.accessed', 'qurl.expired'],
          failure_count: 1, // below MIN_FAILURES (3)
          last_delivery_success: false,
        },
      ] } }),
      'DELETE /v1/webhooks/wh_transient': () => { deleted = true; return { status: 204, body: '' }; },
      'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
    });
    await ensureWebhookSubscription(BOT_OPTS);
    expect(deleted).toBe(false);
  });

  it('does NOT delete when failure_count is negative (fails closed)', async () => {
    // JSON.stringify maps NaN/Infinity to null, so those can't reach
    // the predicate through a normal qurl-service JSON response. We
    // still pin the predicate-level invariant against NaN/Infinity in
    // the _internals unit tests below (where we can hand-construct the
    // exact value); here we cover the only end-to-end shape that the
    // wire could deliver and still slip past `< MIN_FAILURES` — a
    // negative number (typeof 'number', not non-finite).
    let deleted = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        {
          webhook_id: 'wh_negative',
          url: OLD_URL,
          description: BOT_DESC,
          events: ['qurl.accessed', 'qurl.expired'],
          last_delivery_success: false,
          failure_count: -1,
        },
      ] } }),
      'DELETE /v1/webhooks/wh_negative': () => { deleted = true; return { status: 204, body: '' }; },
      'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
    });
    await ensureWebhookSubscription(BOT_OPTS);
    expect(deleted).toBe(false);
  });

  it('does NOT delete when failure_count is missing or non-numeric (fails closed on schema drift)', async () => {
    let deleted = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        {
          webhook_id: 'wh_no_count',
          url: OLD_URL,
          description: BOT_DESC,
          events: ['qurl.accessed', 'qurl.expired'],
          last_delivery_success: false,
          // failure_count omitted — qurl-service contract drift
        },
      ] } }),
      'DELETE /v1/webhooks/wh_no_count': () => { deleted = true; return { status: 204, body: '' }; },
      'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
    });
    await ensureWebhookSubscription(BOT_OPTS);
    expect(deleted).toBe(false);
  });

  it('logs the liveness-gated near-miss count for CloudWatch observability', async () => {
    // Pin the observability seam: when a host+path+description matches
    // but the liveness gate held the row back, surface the count so an
    // operator can grep CloudWatch instead of inspecting subs by hand.
    const logSpy = jest.spyOn(console, 'log').mockImplementation(() => {});
    try {
      mockFetchResponses({
        'GET /v1/webhooks': () => ({ body: { data: [
          // Two near-miss rows (healthy, transient) — neither gets deleted.
          {
            webhook_id: 'wh_healthy',
            url: 'https://discord.eu-central-1.layerv.xyz/webhooks/qurl',
            description: BOT_DESC,
            events: ['qurl.accessed'],
            failure_count: 0,
            last_delivery_success: true,
          },
          {
            webhook_id: 'wh_transient',
            url: 'https://discord.eu-west-1.layerv.xyz/webhooks/qurl',
            description: BOT_DESC,
            events: ['qurl.accessed'],
            failure_count: 1,
            last_delivery_success: false,
          },
        ] } }),
        'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
      });
      await ensureWebhookSubscription(BOT_OPTS);
      const nearMissLine = logSpy.mock.calls
        .map(c => c[0])
        .find(line => typeof line === 'string' && line.includes('liveness-gated near-misses'));
      expect(nearMissLine).toBeDefined();
      const meta = JSON.parse(nearMissLine.slice(nearMissLine.indexOf('{')));
      expect(meta).toMatchObject({
        near_miss_count: 2,
        orphan_delete_attempts: 0,
        url: NEW_URL,
      });
    } finally {
      logSpy.mockRestore();
    }
  });

  it('does NOT emit the near-miss log when there are zero candidates (steady state, no noise)', async () => {
    // Avoid log spam on every healthy boot — only emit when we actually
    // saw a candidate-but-not-orphan row.
    const logSpy = jest.spyOn(console, 'log').mockImplementation(() => {});
    try {
      mockFetchResponses({
        'GET /v1/webhooks': () => ({ body: { data: [
          // Sibling-service sub — doesn't match description-prefix, so
          // not a near-miss either.
          { webhook_id: 'wh_connector', url: 'https://s3-connector.example/webhooks/qurl', description: 'qurl-s3-connector ...', events: [], failure_count: 0, last_delivery_success: true },
        ] } }),
        'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
      });
      await ensureWebhookSubscription(BOT_OPTS);
      const nearMissLine = logSpy.mock.calls
        .map(c => c[0])
        .find(line => typeof line === 'string' && line.includes('liveness-gated near-misses'));
      expect(nearMissLine).toBeUndefined();
    } finally {
      logSpy.mockRestore();
    }
  });

  it('skips a row with a malformed URL gracefully (urlHost/urlPathname return null)', async () => {
    // Defensive: a future qurl-service contract drift that lets a junk
    // URL through (or a manual sub created with an unparseable URL)
    // must not crash the sweep — it just skips that row.
    let createCalled = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        { webhook_id: 'wh_junk_url', url: 'not://a valid url with spaces', description: BOT_DESC, events: [], failure_count: 999, last_delivery_success: false },
      ] } }),
      'POST /v1/webhooks': () => { createCalled = true; return { status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }; },
    });
    await ensureWebhookSubscription(BOT_OPTS);
    expect(createCalled).toBe(true);
  });

  it('skips the sweep entirely when urlMigrationSweepEnabled=false (hard guard for active-active rollout)', async () => {
    // Hard guard for the cannibalization risk tracked in #827. When the
    // flag is false, no row is classified as orphan or near-miss; the
    // matches + dedupe path still runs normally.
    let deleted = false;
    let nearMissLogged = false;
    const logSpy = jest.spyOn(console, 'log').mockImplementation((line) => {
      if (typeof line === 'string' && line.includes('liveness-gated near-misses')) {
        nearMissLogged = true;
      }
    });
    try {
      mockFetchResponses({
        'GET /v1/webhooks': () => ({ body: { data: [
          // What would be a confirmed orphan with sweep enabled.
          deadOrphan({ webhook_id: 'wh_would_orphan' }),
          // What would be a near-miss with sweep enabled.
          { webhook_id: 'wh_would_near_miss', url: OLD_URL, description: BOT_DESC, events: ['qurl.accessed'], failure_count: 0, last_delivery_success: true },
        ] } }),
        'DELETE /v1/webhooks/wh_would_orphan': () => { deleted = true; return { status: 204, body: '' }; },
        'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
      });
      const result = await ensureWebhookSubscription({ ...BOT_OPTS, urlMigrationSweepEnabled: false });
      expect(deleted).toBe(false); // hard-guard wins
      expect(nearMissLogged).toBe(false); // no observability noise when disabled
      expect(result.action).toBe('created');
    } finally {
      logSpy.mockRestore();
    }
  });

  it('runs the sweep normally when urlMigrationSweepEnabled defaults to true (no opt set)', async () => {
    // Sanity-check: omitting the opt entirely preserves the existing
    // single-host behavior (sweep runs).
    let deleted = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [deadOrphan()] } }),
      'DELETE /v1/webhooks/wh_orphan': () => { deleted = true; return { status: 204, body: '' }; },
      'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
    });
    await ensureWebhookSubscription(BOT_OPTS); // no urlMigrationSweepEnabled key
    expect(deleted).toBe(true);
  });

  it('skips the sweep when description is empty (cannot derive a safe prefix)', async () => {
    // Defensive: an empty description would derive an empty prefix,
    // which would match LITERALLY every sub via startsWith(''). The
    // sweep must short-circuit and never delete in that case.
    let deleted = false;
    mockFetchResponses({
      'GET /v1/webhooks': () => ({ body: { data: [
        { webhook_id: 'wh_anything', url: OLD_URL, description: 'literally anything', events: [], failure_count: 1, last_delivery_success: false },
      ] } }),
      'DELETE /v1/webhooks/wh_anything': () => { deleted = true; return { status: 204, body: '' }; },
      'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
    });
    await ensureWebhookSubscription({ ...BOT_OPTS, description: '' });
    expect(deleted).toBe(false);
  });
});

describe('isTruthyEnvFlag — env-var normalization (kill-switch semantics)', () => {
  const { isTruthyEnvFlag } = require('../src/qurl-webhook-registrar');
  it.each([
    ['1', true],
    ['true', true],
    ['TRUE', true],
    ['True', true],
    ['yes', true],
    ['YES', true],
    ['on', true],
    [' on ', true], // whitespace tolerated
  ])('treats %s as truthy', (value, expected) => {
    expect(isTruthyEnvFlag(value)).toBe(expected);
  });
  it.each([
    ['0', false],
    ['false', false],
    ['FALSE', false],
    ['no', false],
    ['off', false],
    ['', false],
    ['arbitrary-string', false],
    [undefined, false],
    [null, false],
    [42, false], // non-string
  ])('treats %s as falsy (no any-non-empty-string footgun)', (value, expected) => {
    expect(isTruthyEnvFlag(value)).toBe(expected);
  });
});

describe('buildUrlMigrationOrphanFilter — predicate factory edge cases', () => {
  const { buildUrlMigrationOrphanFilter, ORPHAN_FILTER_RESULTS, URL_MIGRATION_ORPHAN_MIN_FAILURES } = _internals;

  it('returns null when descriptionPrefix is empty (sweep disabled)', () => {
    expect(buildUrlMigrationOrphanFilter({ bridgeUrl: 'https://bot.example/webhooks/qurl', descriptionPrefix: '' })).toBeNull();
  });

  it('returns null when bridgeUrl is unparseable (sweep disabled, no false-positive deletes possible)', () => {
    expect(buildUrlMigrationOrphanFilter({ bridgeUrl: 'not://a real url', descriptionPrefix: 'X' })).toBeNull();
  });

  it('classifies a healthy cross-host candidate as NEAR_MISS_LIVENESS (not MATCH)', () => {
    const f = buildUrlMigrationOrphanFilter({
      bridgeUrl: 'https://bot.test.example/webhooks/qurl',
      descriptionPrefix: 'X',
    });
    expect(f({
      url: 'https://other.test.example/webhooks/qurl',
      description: 'X (env=prod)',
      failure_count: 0,
      last_delivery_success: true,
    })).toBe(ORPHAN_FILTER_RESULTS.NEAR_MISS_LIVENESS);
  });

  it('classifies a transient-failure cross-host candidate as NEAR_MISS_LIVENESS (failure_count under threshold)', () => {
    const f = buildUrlMigrationOrphanFilter({
      bridgeUrl: 'https://bot.test.example/webhooks/qurl',
      descriptionPrefix: 'X',
    });
    expect(f({
      url: 'https://other.test.example/webhooks/qurl',
      description: 'X (env=prod)',
      failure_count: URL_MIGRATION_ORPHAN_MIN_FAILURES - 1, // just under
      last_delivery_success: false,
    })).toBe(ORPHAN_FILTER_RESULTS.NEAR_MISS_LIVENESS);
  });

  it('classifies a confirmed orphan (cross-host + matching desc + dead + many failures) as MATCH', () => {
    const f = buildUrlMigrationOrphanFilter({
      bridgeUrl: 'https://bot.test.example/webhooks/qurl',
      descriptionPrefix: 'X',
    });
    expect(f({
      url: 'https://other.test.example/webhooks/qurl',
      description: 'X (env=prod)',
      failure_count: URL_MIGRATION_ORPHAN_MIN_FAILURES,
      last_delivery_success: false,
    })).toBe(ORPHAN_FILTER_RESULTS.MATCH);
  });

  it('classifies a same-host candidate as NO_MATCH regardless of liveness', () => {
    const f = buildUrlMigrationOrphanFilter({
      bridgeUrl: 'https://bot.test.example/webhooks/qurl',
      descriptionPrefix: 'X',
    });
    expect(f({
      url: 'https://bot.test.example/webhooks/qurl',
      description: 'X (env=prod)',
      failure_count: 9999,
      last_delivery_success: false,
    })).toBe(ORPHAN_FILTER_RESULTS.NO_MATCH);
  });

  it.each([
    ['NaN', NaN],
    ['Infinity', Infinity],
    ['-Infinity', -Infinity],
  ])('classifies a candidate with non-finite failure_count (%s) as NEAR_MISS_LIVENESS (fail-closed against latent fail-open)', (_label, failureCount) => {
    // JSON.stringify maps these to null at the wire boundary so a
    // normal qurl-service response can't deliver them, but a
    // non-JSON-parsed path (raw Node fetch with a non-JSON SDK, a
    // future contract migration, an in-process injection bug) could.
    // `Number.isFinite` rejects all three uniformly, matching the
    // stated fail-closed invariant. Without it, NaN/Infinity would
    // slip through `typeof === 'number'` AND the `< 3` comparison
    // (NaN < 3 is false; Infinity < 3 is false) → classify as MATCH
    // → DELETE. This unit test makes the predicate-level guarantee
    // wire-independent.
    const f = buildUrlMigrationOrphanFilter({
      bridgeUrl: 'https://bot.test.example/webhooks/qurl',
      descriptionPrefix: 'X',
    });
    expect(f({
      url: 'https://other.test.example/webhooks/qurl',
      description: 'X (env=prod)',
      failure_count: failureCount,
      last_delivery_success: false,
    })).toBe(ORPHAN_FILTER_RESULTS.NEAR_MISS_LIVENESS);
  });

  it('classifies a sibling-service candidate (non-matching description prefix) as NO_MATCH', () => {
    const f = buildUrlMigrationOrphanFilter({
      bridgeUrl: 'https://bot.test.example/webhooks/qurl',
      descriptionPrefix: 'X',
    });
    expect(f({
      url: 'https://other.test.example/webhooks/qurl',
      description: 'qurl-s3-connector resource.closed subscription',
      failure_count: 9999,
      last_delivery_success: false,
    })).toBe(ORPHAN_FILTER_RESULTS.NO_MATCH);
  });
});

describe('deriveDescriptionPrefix — internal helper (orphan-sweep safety net)', () => {
  const { deriveDescriptionPrefix } = _internals;
  it('returns the prefix up to the first " ("', () => {
    expect(deriveDescriptionPrefix('Discord bot view counter (region=us-east-2, env=sandbox)'))
      .toBe('Discord bot view counter');
    expect(deriveDescriptionPrefix('Discord bot view counter (guild=123, via=oauth)'))
      .toBe('Discord bot view counter');
  });
  it('returns the whole string when no " (" is present', () => {
    expect(deriveDescriptionPrefix('just a flat description')).toBe('just a flat description');
  });
  it('returns "" for empty / non-string inputs', () => {
    expect(deriveDescriptionPrefix('')).toBe('');
    expect(deriveDescriptionPrefix(undefined)).toBe('');
    expect(deriveDescriptionPrefix(null)).toBe('');
    expect(deriveDescriptionPrefix(42)).toBe('');
  });
});

// Pinned to keep webhook-subscriptions.js::discoverDefaultOwnerId
// from breaking silently if a future registrar refactor changes
// callQurlService's signature. The external caller relies on
// (method, path, apiEndpoint, apiKey) + response = parsed-JSON body.
describe('callQurlService — exported contract', () => {
  const { callQurlService } = require('../src/qurl-webhook-registrar');

  beforeEach(() => { global.fetch = jest.fn(); });
  afterAll(() => { global.fetch = ORIGINAL_FETCH; });

  it('is exported as a top-level function (not _internals)', () => {
    expect(typeof callQurlService).toBe('function');
  });

  it('returns the JSON-parsed body on 2xx', async () => {
    global.fetch = jest.fn(async () => ({
      ok: true, status: 200, text: async () => JSON.stringify({ data: [{ id: 'x' }] }),
    }));
    const out = await callQurlService({
      method: 'GET', path: '/v1/webhooks', apiEndpoint: 'https://q.example', apiKey: 'k',
    });
    expect(out).toEqual({ data: [{ id: 'x' }] });
  });

  it('forwards Authorization: Bearer <apiKey>', async () => {
    global.fetch = jest.fn(async () => ({
      ok: true, status: 200, text: async () => '{}',
    }));
    await callQurlService({
      method: 'GET', path: '/v1/webhooks', apiEndpoint: 'https://q.example', apiKey: 'secret-k',
    });
    const opts = global.fetch.mock.calls[0][1];
    expect(opts.headers.Authorization).toBe('Bearer secret-k');
  });

  it('throws on non-2xx with the QurlServiceError shape (op + status)', async () => {
    global.fetch = jest.fn(async () => ({
      ok: false, status: 503, text: async () => '',
    }));
    await expect(callQurlService({
      method: 'GET', path: '/v1/webhooks', apiEndpoint: 'https://q.example', apiKey: 'k',
    })).rejects.toThrow(/returned 503/);
  });
});
