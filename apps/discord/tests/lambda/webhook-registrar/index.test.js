
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

const lambdaModule = require('../../../lambda/webhook-registrar/index');
const { handler } = lambdaModule;
const { _resetSsmClientCacheForTests, getSsmClient } = lambdaModule._internals;

beforeEach(() => {
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

  it.each([
    ['apiEndpoint', 'http://insecure.example'],
    ['apiEndpoint', 'ws://wrong-scheme.example'],
    ['bridgeUrl', 'http://insecure.example/webhooks/qurl'],
  ])('throws when %s doesn\'t start with https:// (got %s)', async (key, badValue) => {
    await expect(handler({ ...BASE_EVENT, [key]: badValue }, CONTEXT)).rejects.toThrow(/must start with https:\/\//);
  });

  it.each([
    'Discord Bot view counter (env=prod)', // capital B
    'discord bot view counter (env=prod)', // lowercase d
    'Discord bot - view counter (env=prod)', // dash instead of contiguous
    'Some unrelated description (env=prod)',
    'Discord bot view counterX (env=prod)', // no boundary
  ])('throws on description prefix drift (%s) — orphan-sweep matcher invariant', async (badDescription) => {
    await expect(handler({ ...BASE_EVENT, description: badDescription }, CONTEXT))
      .rejects.toThrow(/description must start with "Discord bot view counter \("/);
  });

  it('KEEPS the sweep enabled when QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP=0 (footgun guard, normalize-then-test)', async () => {
    const oldEnv = process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP;
    process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP = '0';
    try {
      ssmMock
        .on(GetParameterCommand, { Name: '/test/QURL_API_KEY' })
        .resolves({ Parameter: { Value: 'lv_test_key' } })
        .on(GetParameterCommand, { Name: '/test/QURL_WEBHOOK_SECRET' })
        .rejects(Object.assign(new Error('not found'), { name: 'ParameterNotFound' }))
        .on(PutParameterCommand)
        .resolves({});
      let deleted = false;
      mockQurlService({
        'GET /v1/webhooks': () => ({ body: { data: [
          {
            webhook_id: 'wh_orphan',
            url: 'https://oldhost.example/webhooks/qurl',
            description: 'Discord bot view counter (region=us-east-2, env=sandbox-old)',
            events: ['qurl.accessed', 'qurl.expired'],
            failure_count: 1475,
            last_delivery_success: false,
          },
        ] } }),
        'DELETE /v1/webhooks/wh_orphan': () => { deleted = true; return { status: 204, body: '' }; },
        'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
      });
      await handler({ ...BASE_EVENT }, CONTEXT);
      expect(deleted).toBe(true); // sweep ran — `=0` does NOT disable
    } finally {
      if (oldEnv === undefined) delete process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP;
      else process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP = oldEnv;
    }
  });

  it('disables the orphan sweep when QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP is set (hard guard for active-active)', async () => {
    const oldEnv = process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP;
    process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP = '1';
    const warnSpy = jest.spyOn(console, 'warn').mockImplementation(() => {});
    try {
      ssmMock
        .on(GetParameterCommand, { Name: '/test/QURL_API_KEY' })
        .resolves({ Parameter: { Value: 'lv_test_key' } })
        .on(GetParameterCommand, { Name: '/test/QURL_WEBHOOK_SECRET' })
        .rejects(Object.assign(new Error('not found'), { name: 'ParameterNotFound' }))
        .on(PutParameterCommand)
        .resolves({});
      let deleted = false;
      mockQurlService({
        'GET /v1/webhooks': () => ({ body: { data: [
          {
            webhook_id: 'wh_orphan',
            url: 'https://oldhost.example/webhooks/qurl',
            description: 'Discord bot view counter (region=us-east-2, env=sandbox-old)',
            events: ['qurl.accessed', 'qurl.expired'],
            failure_count: 1475,
            last_delivery_success: false,
          },
        ] } }),
        'DELETE /v1/webhooks/wh_orphan': () => { deleted = true; return { status: 204, body: '' }; },
        'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_new', secret: 'whsec_new' } } }),
      });
      const result = await handler({ ...BASE_EVENT }, CONTEXT);
      expect(deleted).toBe(false); // hard guard wins despite confirmed-orphan-shaped row
      expect(result.action).toBe('created');
      expect(warnSpy).toHaveBeenCalledWith(expect.stringContaining('URL-migration orphan sweep DISABLED via env override'));
    } finally {
      warnSpy.mockRestore();
      if (oldEnv === undefined) delete process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP;
      else process.env.QURL_WEBHOOK_REGISTRAR_DISABLE_URL_MIGRATION_SWEEP = oldEnv;
    }
  });

  it('accepts description that exactly equals the prefix (boundary case)', async () => {
    ssmMock
      .on(GetParameterCommand, { Name: '/test/QURL_API_KEY' })
      .resolves({ Parameter: { Value: 'lv_test_key' } })
      .on(GetParameterCommand, { Name: '/test/QURL_WEBHOOK_SECRET' })
      .rejects(Object.assign(new Error('not found'), { name: 'ParameterNotFound' }))
      .on(PutParameterCommand)
      .resolves({});
    mockQurlService({
      'GET /v1/webhooks': () => ({ body: { data: [] } }),
      'POST /v1/webhooks': () => ({ status: 201, body: { data: { webhook_id: 'wh_ok', secret: 'whsec_x' } } }),
    });
    const result = await handler({ ...BASE_EVENT, description: 'Discord bot view counter' }, CONTEXT);
    expect(result.action).toBe('created');
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
        events: ['qurl.accessed', 'qurl.expired'],
      } } }),
    });
    
    const result = await handler(BASE_EVENT, CONTEXT);
    expect(result).toEqual({ webhookId: 'wh_lambda_created', action: 'created' });
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
        events: ['qurl.accessed', 'qurl.expired'],
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

describe('webhook-registrar Lambda — secret never echoes in handler response (all action paths)', () => {
  it('does not return secret on the rotated action', async () => {
    ssmMock
      .on(GetParameterCommand, { Name: '/test/QURL_API_KEY' })
      .resolves({ Parameter: { Value: 'lv_test_key' } })
      .on(GetParameterCommand, { Name: '/test/QURL_WEBHOOK_SECRET' })
      .rejects(Object.assign(new Error('not found'), { name: 'ParameterNotFound' }))
      .on(PutParameterCommand)
      .resolves({});
    mockQurlService({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing', url: BASE_EVENT.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'],
      }] } }),
      'POST /v1/webhooks/wh_existing/secret': () => ({
        body: { data: { webhook_id: 'wh_existing', secret: 'whsec_secret_to_hide' } },
      }),
    });
    const result = await handler(BASE_EVENT, CONTEXT);
    expect(result.action).toBe('rotated');
    expect(result).not.toHaveProperty('secret');
    expect(JSON.stringify(result)).not.toContain('whsec_secret_to_hide');
  });

  it('does not return secret on the reused action', async () => {
    ssmMock
      .on(GetParameterCommand, { Name: '/test/QURL_API_KEY' })
      .resolves({ Parameter: { Value: 'lv_test_key' } })
      .on(GetParameterCommand, { Name: '/test/QURL_WEBHOOK_SECRET' })
      .resolves({ Parameter: { Value: 'whsec_already_known_secret' } });
    mockQurlService({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing', url: BASE_EVENT.bridgeUrl, events: ['qurl.accessed', 'qurl.expired'],
      }] } }),
    });
    const result = await handler(BASE_EVENT, CONTEXT);
    expect(result.action).toBe('reused');
    expect(result).not.toHaveProperty('secret');
    expect(JSON.stringify(result)).not.toContain('whsec_already_known_secret');
  });
});

describe('webhook-registrar Lambda — bootstrap rotate (existing sub, SSM empty)', () => {
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
        events: ['qurl.accessed', 'qurl.expired'],
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

  it('throws when SSM PutParameter fails on the persist call (strict-persist; Terraform deploy must fail fast)', async () => {
    ssmMock
      .on(GetParameterCommand, { Name: '/test/QURL_API_KEY' })
      .resolves({ Parameter: { Value: 'lv_test_key' } })
      .on(GetParameterCommand, { Name: '/test/QURL_WEBHOOK_SECRET' })
      .rejects(Object.assign(new Error('not found'), { name: 'ParameterNotFound' }))
      .on(PutParameterCommand)
      .rejects(Object.assign(new Error('Rate exceeded'), { name: 'ThrottlingException' }));
    mockQurlService({
      'GET /v1/webhooks': () => ({ body: { data: [] } }),
      'POST /v1/webhooks': () => ({ status: 201, body: { data: {
        webhook_id: 'wh_orphaned', secret: 'whsec_lost_to_ssm',
      } } }),
    });
    await expect(handler(BASE_EVENT, CONTEXT)).rejects.toThrow(/Rate exceeded|ThrottlingException/);
  });

  it('does NOT call PutParameter on the reuse path (steady-state re-invocations are no-op for SSM)', async () => {
    ssmMock
      .on(GetParameterCommand, { Name: '/test/QURL_API_KEY' })
      .resolves({ Parameter: { Value: 'lv_test_key' } })
      .on(GetParameterCommand, { Name: '/test/QURL_WEBHOOK_SECRET' })
      .resolves({ Parameter: { Value: 'whsec_existing' } })
      .on(PutParameterCommand)
      .resolves({});
    mockQurlService({
      'GET /v1/webhooks': () => ({ body: { data: [{
        webhook_id: 'wh_existing',
        url: BASE_EVENT.bridgeUrl,
        events: ['qurl.accessed', 'qurl.expired'],
      }] } }),
    });
    const result = await handler(BASE_EVENT, CONTEXT);
    expect(result.action).toBe('reused');
    expect(ssmMock.commandCalls(PutParameterCommand)).toHaveLength(0);
  });
});

describe('webhook-registrar Lambda — getSsmClient cache', () => {
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
    const c = getSsmClient('us-east-2');
    expect(c).not.toBe(a); // a fresh instance — the cache only holds the LATEST region
  });
});
