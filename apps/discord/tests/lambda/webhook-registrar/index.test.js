// Tests for the webhook-registrar Lambda handler.
//
// Strategy: mock @aws-sdk/client-ssm + global.fetch (qurl-service
// calls), then invoke the handler directly. Asserts the Lambda's
// orchestration contract — input validation, SSM secret read, ensure
// → persist → return shape — without booting AWS.

const { mockClient } = require('aws-sdk-client-mock');
const {
  SSMClient,
  GetParameterCommand,
  PutParameterCommand,
} = require('@aws-sdk/client-ssm');

const ORIGINAL_FETCH = global.fetch;
const ssmMock = mockClient(SSMClient);

beforeEach(() => {
  ssmMock.reset();
});

afterEach(() => {
  global.fetch = ORIGINAL_FETCH;
});

const BASE_EVENT = {
  apiEndpoint: 'https://api.test.example',
  bridgeUrl: 'https://bot.test.example/webhooks/qurl',
  description: 'Discord bot view counter (region=us-east-2)',
  ssmParamName: '/test/QURL_WEBHOOK_SECRET',
  ssmRegion: 'us-east-2',
  apiKeySsmParamName: '/test/QURL_API_KEY',
};

const CONTEXT = { awsRequestId: 'req-1' };

function mockQurlService(handlers) {
  global.fetch = jest.fn(async (url, opts) => {
    const path = url.replace(/^https?:\/\/[^/]+/, '');
    const method = opts.method || 'GET';
    const pathnameOnly = path.split('?')[0];
    const handler = handlers[`${method} ${path}`] || handlers[`${method} ${pathnameOnly}`];
    if (!handler) throw new Error(`Unmocked fetch: ${method} ${path}`);
    const { status = 200, body } = handler(opts);
    return {
      ok: status >= 200 && status < 300,
      status,
      text: async () => (typeof body === 'string' ? body : JSON.stringify(body)),
    };
  });
}

// Lambda module is required once — `aws-sdk-client-mock` patches the
// SSMClient class globally, so the cached client inside the handler
// still routes through the per-test `ssmMock.reset()`. Re-requiring
// the module would create a fresh SDK class that the mock doesn't
// cover, breaking the dynamic-import path.
const lambdaModule = require('../../../lambda/webhook-registrar/index');
const { handler } = lambdaModule;
const { _resetSsmClientCacheForTests, getSsmClient } = lambdaModule._internals;

beforeEach(() => {
  // Reset the module-level SSM client cache between tests so a region
  // change in one test doesn't leak into the next, and so the
  // ssmMock-patched class is what the handler resolves on first use.
  _resetSsmClientCacheForTests();
});

describe('webhook-registrar Lambda — input validation', () => {
  it.each([
    'apiEndpoint',
    'bridgeUrl',
    'description',
    'ssmParamName',
    'ssmRegion',
    'apiKeySsmParamName',
  ])('throws when required field %s is missing', async (key) => {
    
    const event = { ...BASE_EVENT };
    delete event[key];
    await expect(handler(event, CONTEXT)).rejects.toThrow(new RegExp(`missing.*${key}`));
  });

  it.each([null, undefined, '', 42, {}])('throws on non-string value for required field (%s)', async (badValue) => {
    
    await expect(handler({ ...BASE_EVENT, ssmParamName: badValue }, CONTEXT)).rejects.toThrow(/missing.*ssmParamName/);
  });
});

describe('webhook-registrar Lambda — cold bootstrap (no existing sub, no SSM secret)', () => {
  it('reads API key from SSM, creates subscription, persists returned secret, returns metadata', async () => {
    ssmMock
      .on(GetParameterCommand, { Name: '/test/QURL_API_KEY' })
      .resolves({ Parameter: { Value: 'lv_test_key' } })
      .on(GetParameterCommand, { Name: '/test/QURL_WEBHOOK_SECRET' })
      .rejects(Object.assign(new Error('not found'), { name: 'ParameterNotFound' }))
      .on(PutParameterCommand)
      .resolves({});
    mockQurlService({
      'GET /v1/webhooks': () => ({ body: { data: [] } }),
      'POST /v1/webhooks': () => ({ status: 201, body: { data: {
        webhook_id: 'wh_lambda_created',
        secret: 'whsec_from_lambda',
        url: BASE_EVENT.bridgeUrl,
        events: ['qurl.accessed'],
      } } }),
    });
    
    const result = await handler(BASE_EVENT, CONTEXT);
    expect(result).toEqual({ webhookId: 'wh_lambda_created', action: 'created' });
    // Secret persisted via SSM, NOT echoed in the response (avoids
    // leaking through Terraform's invocation log).
    expect(result.secret).toBeUndefined();
    const putCalls = ssmMock.commandCalls(PutParameterCommand);
    expect(putCalls).toHaveLength(1);
    expect(putCalls[0].args[0].input).toEqual(expect.objectContaining({
      Name: '/test/QURL_WEBHOOK_SECRET',
      Type: 'SecureString',
      Value: 'whsec_from_lambda',
      Overwrite: true,
    }));
  });
});

describe('webhook-registrar Lambda — steady-state (existing sub + SSM secret present)', () => {
  it('reuses the existing subscription without rotating', async () => {
    ssmMock
      .on(GetParameterCommand, { Name: '/test/QURL_API_KEY' })
      .resolves({ Parameter: { Value: 'lv_test_key' } })
      .on(GetParameterCommand, { Name: '/test/QURL_WEBHOOK_SECRET' })
      .resolves({ Parameter: { Value: 'whsec_existing' } })
      .on(PutParameterCommand)
      .resolves({});
    let rotateHit = false;
    mockQurlService({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing',
        url: BASE_EVENT.bridgeUrl,
        events: ['qurl.accessed'],
      }] } }),
      'POST /v1/webhooks/wh_existing/secret': () => {
        rotateHit = true;
        return { body: { data: { webhook_id: 'wh_existing', secret: 'whsec_rotated' } } };
      },
    });
    
    const result = await handler(BASE_EVENT, CONTEXT);
    expect(result).toEqual({ webhookId: 'wh_existing', action: 'reused' });
    expect(rotateHit).toBe(false); // critical: no rotate, single-source-of-truth secret stays
  });
});

describe('webhook-registrar Lambda — bootstrap rotate (existing sub, SSM empty)', () => {
  // Path that fires when an operator-or-prior-deploy created the
  // subscription out-of-band but the Lambda hasn't populated the
  // SSM secret yet. The Lambda rotates the secret to a known value
  // and persists it. Common during a manual-recovery → automated-
  // takeover transition.
  it('rotates the existing sub when SSM returns empty/missing', async () => {
    ssmMock
      .on(GetParameterCommand, { Name: '/test/QURL_API_KEY' })
      .resolves({ Parameter: { Value: 'lv_test_key' } })
      .on(GetParameterCommand, { Name: '/test/QURL_WEBHOOK_SECRET' })
      .resolves({ Parameter: { Value: '' } })
      .on(PutParameterCommand)
      .resolves({});
    let rotateHit = false;
    mockQurlService({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing',
        url: BASE_EVENT.bridgeUrl,
        events: ['qurl.accessed'],
      }] } }),
      'POST /v1/webhooks/wh_existing/secret': () => {
        rotateHit = true;
        return { body: { data: { webhook_id: 'wh_existing', secret: 'whsec_rotated_by_lambda' } } };
      },
    });
    const result = await handler(BASE_EVENT, CONTEXT);
    expect(rotateHit).toBe(true);
    expect(result).toEqual({ webhookId: 'wh_existing', action: 'rotated' });
    const putCalls = ssmMock.commandCalls(PutParameterCommand);
    expect(putCalls[0].args[0].input.Value).toBe('whsec_rotated_by_lambda');
  });
});

describe('webhook-registrar Lambda — bridgeUrl normalization', () => {
  it('strips a trailing slash on bridgeUrl before sending to qurl-service', async () => {
    ssmMock
      .on(GetParameterCommand, { Name: '/test/QURL_API_KEY' })
      .resolves({ Parameter: { Value: 'lv_test_key' } })
      .on(GetParameterCommand, { Name: '/test/QURL_WEBHOOK_SECRET' })
      .rejects(Object.assign(new Error('not found'), { name: 'ParameterNotFound' }))
      .on(PutParameterCommand)
      .resolves({});
    let createBody = null;
    mockQurlService({
      'GET /v1/webhooks': () => ({ body: { data: [] } }),
      'POST /v1/webhooks': (opts) => {
        createBody = JSON.parse(opts.body);
        return { status: 201, body: { data: { webhook_id: 'wh', secret: 'whsec_' } } };
      },
    });
    await handler({ ...BASE_EVENT, bridgeUrl: 'https://bot.test.example/webhooks/qurl/' }, CONTEXT);
    // No trailing slash on the wire. canonicalUrl in the registrar
    // normalizes trailing-slash drift BETWEEN matched subs but not
    // an internal `//` from a stale `BASE_URL=https://bot/`.
    expect(createBody.url).toBe('https://bot.test.example/webhooks/qurl');
  });
});

describe('webhook-registrar Lambda — failure surfacing', () => {
  it('throws when SSM API key is missing — ParameterNotFound (lambda fails → Terraform fails the deploy)', async () => {
    ssmMock
      .on(GetParameterCommand, { Name: '/test/QURL_API_KEY' })
      .rejects(Object.assign(new Error('not found'), { name: 'ParameterNotFound' }));
    await expect(handler(BASE_EVENT, CONTEXT)).rejects.toThrow(/SSM parameter.*returned empty or missing/);
  });

  it('throws the same error when SSM API key value is present but empty', async () => {
    // SSM returning Value: '' (operator put-parameter --value "") is a
    // distinct cold-fail mode from ParameterNotFound — but both indicate
    // unusable API key state. The same error fires for grep parity.
    ssmMock
      .on(GetParameterCommand, { Name: '/test/QURL_API_KEY' })
      .resolves({ Parameter: { Value: '' } });
    await expect(handler(BASE_EVENT, CONTEXT)).rejects.toThrow(/SSM parameter.*returned empty or missing/);
  });

  it('propagates qurl-service errors so Terraform deploy fails fast', async () => {
    ssmMock
      .on(GetParameterCommand, { Name: '/test/QURL_API_KEY' })
      .resolves({ Parameter: { Value: 'lv_test_key' } })
      .on(GetParameterCommand, { Name: '/test/QURL_WEBHOOK_SECRET' })
      .rejects(Object.assign(new Error('not found'), { name: 'ParameterNotFound' }));
    mockQurlService({
      'GET /v1/webhooks': () => ({ status: 401, body: { error: 'Unauthorized' } }),
    });
    
    await expect(handler(BASE_EVENT, CONTEXT)).rejects.toThrow(/401/);
  });
});

describe('webhook-registrar Lambda — getSsmClient cache', () => {
  // The cache key (region) is tracked in a closure — NOT read back
  // off `client.config.region` which in AWS SDK v3 is a Provider<string>
  // (function returning Promise). Comparing the Promise to a region
  // string would always be truthy → cache never invalidates AND
  // forcing this helper async would propagate up. These tests pin
  // the closure-based behavior.
  it('warm invocation returns the same client instance for the same region', () => {
    _resetSsmClientCacheForTests();
    const a = getSsmClient('us-east-2');
    const b = getSsmClient('us-east-2');
    expect(b).toBe(a);
  });

  it('returns a new client instance when the region changes', () => {
    _resetSsmClientCacheForTests();
    const a = getSsmClient('us-east-2');
    const b = getSsmClient('us-west-2');
    expect(b).not.toBe(a);
    // ...and reverts again on the original region (no permanent cache).
    const c = getSsmClient('us-east-2');
    expect(c).not.toBe(a); // a fresh instance — the cache only holds the LATEST region
  });
});
