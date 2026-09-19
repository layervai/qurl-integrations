import { TeamsDataStore } from '../src/teams-data.js';
import { HttpQurlClient } from '../src/qurl-client.js';
import { mkdtempSync, realpathSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import type { ServerResponse } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { Readable } from 'node:stream';
import type { Application, Request } from 'express';
import express from 'express';
import type { App } from '@microsoft/teams.apps';
import { DynamoDBDocumentClient } from '@aws-sdk/lib-dynamodb';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { TeamsBot } from '../src/bot.js';
import type { OAuthCallbackCore } from '../src/callback.js';
import type { ConfidentialTokenClient, Logger } from '../src/interfaces.js';
import { OAuthCoreError } from '../src/errors.js';
import { createProductionTeamsConfig, createTeamsServer, httpsIssuer, httpsOrigin, installOAuthRoutes, isMainModule } from '../src/server.js';
import type { OAuthStateManager } from '../src/state.js';
import { TeamsSdkMessagePoster } from '../src/teams-sdk.js';

const OPAQUE_STATE = Buffer.alloc(32, 1).toString('base64url');

type RouteHandler = (request: Request, response: ServerResponse & {
  status: (status: number) => { type: (value: string) => { set: (key: string, value: string) => { send: (value: unknown) => void } } };
  set: (key: string, value: string) => { redirect: (status: number, url: string) => void };
}) => Promise<void> | void;

function routes(input: {
  readonly authorizationRequest?: () => Promise<{ readonly codeChallenge: string; readonly nonce: string; readonly loginHint: string }>;
  readonly complete?: () => Promise<{ readonly binding: { readonly status: 'bound' } | { readonly status: 'conflict'; readonly reason?: string } }>;
  readonly logger?: Logger;
} = {}): Map<string, RouteHandler> {
  const registered = new Map<string, RouteHandler>();
  const app = {
    get: (path: string, handler: RouteHandler) => { registered.set(path, handler); },
  } as unknown as Application;
  const tokenClient: ConfidentialTokenClient = {
    createAuthorizationUrl: request => new URL(`https://auth.example/authorize?state=${request.state}&challenge=${request.codeChallenge}&nonce=${request.nonce}&hint=${request.loginHint ?? ''}`),
    exchangeAuthorizationCode: async () => ({ accessToken: '', idToken: '' }),
  };
  installOAuthRoutes({
    baseUrl: 'https://teams.example',
    expressApp: app,
    app: {} as never,
    tokenClient,
    state: { authorizationRequest: input.authorizationRequest ?? (async () => ({ codeChallenge: 'server-challenge', nonce: 'server-nonce', loginHint: 'admin@example.com' })) } as unknown as OAuthStateManager,
    callback: { complete: input.complete ?? (async () => ({ binding: { status: 'bound' } })) } as unknown as OAuthCallbackCore,
    ...(input.logger ? { logger: input.logger } : {}),
  });
  return registered;
}

async function invoke(handler: RouteHandler, query: Record<string, string>, cookie?: string): Promise<{ readonly status?: number; readonly headers: Map<string, string>; readonly body: string; readonly redirect?: string }> {
  const headers = new Map<string, string>();
  let status: number | undefined;
  let body = '';
  let redirect: string | undefined;
  const response = {
    setHeader: (name: string, value: string) => { headers.set(name, value); },
    writeHead: (value: number, values: Record<string, string>) => { status = value; Object.entries(values).forEach(([name, header]) => headers.set(name, header)); },
    end: (value: string) => { body = value; },
    status: (value: number) => ({ type: () => ({ set: () => ({ send: (payload: unknown) => { status = value; body = JSON.stringify(payload); } }) }) }),
    set: (name: string, value: string) => ({ redirect: (redirectStatus: number, url: string) => { headers.set(name, value); status = redirectStatus; redirect = url; } }),
  };
  await handler({ query, header: (name: string) => name === 'cookie' ? cookie : undefined } as unknown as Request, response as unknown as ServerResponse & never);
  return { ...(status === undefined ? {} : { status }), headers, body, ...(redirect ? { redirect } : {}) };
}

describe('Teams OAuth routes', () => {
  it.each(['start', 'callback'])('logs ordinary %s rejections as warnings while preserving dependency errors', async route => {
    const cases = [
      [new OAuthCoreError('INVALID_STATE', 'Invalid state.'), 'warn'],
      [new OAuthCoreError('STATE_NOT_FOUND', 'Already consumed.'), 'warn'],
      [new OAuthCoreError('STATE_EXPIRED', 'Expired state.'), 'warn'],
      [new OAuthCoreError('COOKIE_MISMATCH', 'Cookie mismatch.'), 'warn'],
      [new OAuthCoreError('STATE_STORE_FAILED', 'State store failed.'), 'error'],
      [new OAuthCoreError('TOKEN_TIMEOUT', 'Token exchange timed out.'), 'error'],
      [new OAuthCoreError('BINDING_FAILED', 'Binding failed.'), 'error'],
      [new OAuthCoreError('ID_TOKEN_NONCE_MISMATCH', 'Nonce mismatch.'), 'error'],
      [new Error('unexpected private upstream detail'), 'error'],
    ] as const;
    for (const [error, level] of cases) {
      const logger = { debug: vi.fn(), info: vi.fn(), warn: vi.fn(), error: vi.fn() };
      const reject = async (): Promise<never> => { throw error; };
      const handler = routes({ authorizationRequest: reject, complete: reject, logger }).get(`/oauth/qurl/${route}`);
      if (!handler) throw new Error('OAuth route was not registered');
      const response = await invoke(handler, { state: OPAQUE_STATE, code: 'private-code' }, `qurl_teams_oauth_state=${OPAQUE_STATE}`);
      expect(logger[level]).toHaveBeenCalledExactlyOnceWith(`Teams OAuth ${route} failed`, { error });
      expect(logger[level === 'warn' ? 'error' : 'warn']).not.toHaveBeenCalled();
      expect(response.status).toBe(400);
      expect(response.headers.get('Set-Cookie')).toContain('Max-Age=0');
      expect(response.body).not.toContain(error.message);
      expect(response.body).not.toContain('private-code');
      expect(response.body).not.toContain(OPAQUE_STATE);
    }
  });

  it('treats a missing setup-link state as an ordinary rejection', async () => {
    const logger = { debug: vi.fn(), info: vi.fn(), warn: vi.fn(), error: vi.fn() };
    const handler = routes({ logger }).get('/oauth/qurl/start');
    if (!handler) throw new Error('start route was not registered');
    const response = await invoke(handler, {});
    expect(response.status).toBe(400);
    expect(logger.warn).toHaveBeenCalledOnce();
    expect(logger.error).not.toHaveBeenCalled();
    expect(response.headers.get('Set-Cookie')).toContain('Max-Age=0');
  });

  it('sets the CSRF cookie and uses server-side authorization parameters', async () => {
    const handler = routes().get('/oauth/qurl/start');
    if (!handler) throw new Error('start route was not registered');
    const response = await invoke(handler, { state: OPAQUE_STATE, code_challenge: 'attacker', nonce: 'attacker', login_hint: 'attacker@example.com' });
    expect(response.status).toBe(302);
    const location = new URL(response.redirect ?? '');
    expect(location.searchParams.get('challenge')).toBe('server-challenge');
    expect(location.searchParams.get('nonce')).toBe('server-nonce');
    expect(location.searchParams.get('hint')).toBe('admin@example.com');
    expect(response.headers.get('Set-Cookie')).toContain(`qurl_teams_oauth_state=${OPAQUE_STATE}`);
  });

  it('clears the cookie and rejects an invalid or expired setup link', async () => {
    const handler = routes({ authorizationRequest: async () => { throw new Error('state unavailable'); } }).get('/oauth/qurl/start');
    if (!handler) throw new Error('start route was not registered');
    const response = await invoke(handler, { state: OPAQUE_STATE });
    expect(response.status).toBe(400);
    expect(response.body).toContain('qURL setup link invalid');
    expect(response.headers.get('Set-Cookie')).toContain('Max-Age=0');
  });

  it('maps an existing conflicting binding to 409 and clears the CSRF cookie', async () => {
    const handler = routes({ complete: async () => ({ binding: { status: 'conflict' } }) }).get('/oauth/qurl/callback');
    if (!handler) throw new Error('callback route was not registered');
    const response = await invoke(handler, { state: OPAQUE_STATE, code: 'code' }, `qurl_teams_oauth_state=${OPAQUE_STATE}`);
    expect(response.status).toBe(409);
    expect(response.body).toContain('already connected to another qURL account');
    expect(response.headers.get('Set-Cookie')).toContain('Max-Age=0');
  });

  it('explains how to recover a retained upstream binding after uninstall', async () => {
    const handler = routes({ complete: async () => ({ binding: { status: 'conflict', reason: 'upstream_binding_cleanup_required' } }) }).get('/oauth/qurl/callback');
    if (!handler) throw new Error('callback route was not registered');
    const response = await invoke(handler, { state: OPAQUE_STATE, code: 'code' }, `qurl_teams_oauth_state=${OPAQUE_STATE}`);
    expect(response.status).toBe(409);
    expect(response.body).toContain('Ask your qURL operator to remove that binding before reinstalling');
  });

  it('identifies authorization failure without claiming a different account owns the tenant', async () => {
    const handler = routes({ complete: async () => ({ binding: { status: 'conflict', reason: 'actor_not_authorized' } }) }).get('/oauth/qurl/callback');
    if (!handler) throw new Error('callback route was not registered');
    const response = await invoke(handler, { state: OPAQUE_STATE, code: 'code' }, `qurl_teams_oauth_state=${OPAQUE_STATE}`);
    expect(response.status).toBe(403);
    expect(response.body).toContain('could not authorize this setup');
    expect(response.body).not.toContain('another qURL account');
    expect(response.headers.get('Set-Cookie')).toContain('Max-Age=0');
  });
});

describe('Teams production URL configuration', () => {
  it('only treats the exact server module as the executable entrypoint', () => {
    expect(isMainModule('/srv/server.js', 'file:///srv/server.js')).toBe(true);
    expect(isMainModule('/srv/server.js', 'file:///srv/consumer.js')).toBe(false);
  });

  it('starts when launched through the bin symlink the package advertises', () => {
    // package.json declares `bin: { "qurl-teams": "dist/server.js" }`. Launched
    // that way, process.argv[1] is the symlink while import.meta.url is the
    // realpath. Comparing raw made the process exit 0 doing nothing, which a
    // container healthcheck reads as a clean exit rather than a crash loop.
    const dir = mkdtempSync(join(tmpdir(), 'qurl-teams-bin-'));
    const real = join(dir, 'server.js');
    const link = join(dir, 'qurl-teams');
    writeFileSync(real, '');
    symlinkSync(real, link);
    try {
      expect(isMainModule(link, pathToFileURL(realpathSync(real)).href)).toBe(true);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('falls back to the raw path when the entrypoint is not on disk', () => {
    expect(isMainModule('/virtual/server.js', 'file:///virtual/server.js')).toBe(true);
  });

  it('accepts HTTPS origins and preserves the OIDC issuer trailing slash', () => {
    expect(httpsOrigin('https://teams.example', 'TEAMS_BASE_URL')).toBe('https://teams.example');
    expect(httpsOrigin('https://Teams.Example', 'TEAMS_BASE_URL')).toBe('https://teams.example');
    expect(httpsOrigin('https://teams.example:443/', 'TEAMS_BASE_URL')).toBe('https://teams.example');
    expect(httpsOrigin('https://teams.example:8443', 'QURL_ENDPOINT')).toBe('https://teams.example:8443');
    expect(httpsIssuer('https://tenant.auth0.com', 'AUTH0_DOMAIN')).toBe('https://tenant.auth0.com/');
  });

  it('rejects credentials, paths, queries, fragments, and non-HTTPS URLs', () => {
    for (const value of [
      'http://teams.example',
      'https://user:pass@teams.example',
      'https://teams.example/path',
      'https://teams.example/?redirect=bad',
      'https://teams.example/#fragment',
    ]) {
      expect(() => httpsOrigin(value, 'TEAMS_BASE_URL')).toThrow('must be an HTTPS origin');
    }
    expect(() => httpsIssuer('https://tenant.auth0.com/path', 'AUTH0_DOMAIN')).toThrow('must be an HTTPS issuer');
  });

  it('fails before constructing production dependencies when a required env value is invalid', async () => {
    const previous = process.env.TEAMS_BASE_URL;
    process.env.TEAMS_BASE_URL = 'http://teams.example';
    try {
      await expect(createProductionTeamsConfig()).rejects.toThrow('TEAMS_BASE_URL must be an HTTPS origin');
    } finally {
      if (previous === undefined) delete process.env.TEAMS_BASE_URL;
      else process.env.TEAMS_BASE_URL = previous;
    }
  });
});

describe('Teams HTTP body limits', () => {
  it('installs the body limit before the SDK messaging route parser', async () => {
    const expressApp = express();
    const app = {
      initialize: async () => {
        expressApp.post('/api/messages', express.json(), (_request, response) => { response.sendStatus(200); });
      },
    } as unknown as App;
    const server = await createTeamsServer({
      baseUrl: 'https://teams.example', expressApp, app,
      tokenClient: {} as never, callback: {} as never, state: {} as never, maxBodyBytes: 1_024,
    });
    const router = (expressApp as unknown as { readonly router?: { readonly stack?: readonly { readonly name?: string; readonly route?: { readonly path?: string; readonly stack?: readonly { readonly name?: string }[] } }[] } }).router;
    const stack = router?.stack ?? [];
    const bodyParserIndex = stack.findIndex(layer => layer.name === 'jsonParser');
    const messageRouteIndex = stack.findIndex(layer => layer.route?.path === '/api/messages');
    expect(bodyParserIndex).toBeGreaterThanOrEqual(0);
    expect(messageRouteIndex).toBeGreaterThan(bodyParserIndex);
    expect(stack[messageRouteIndex]?.route?.stack?.[0]?.name).toBe('jsonParser');
    expect(server).toBeDefined();
  });

  it('does not advertise the Express version to callers', async () => {
    const expressApp = express();
    const app = { initialize: async () => undefined } as unknown as App;
    await createTeamsServer({
      baseUrl: 'https://teams.example', expressApp, app,
      tokenClient: {} as never, callback: {} as never, state: {} as never,
    });
    expect(expressApp.get('x-powered-by')).toBe(false);
  });

  it('enforces the configured body limit on the Teams message route', async () => {
    const expressApp = express();
    const app = {
      initialize: async () => {
        expressApp.post('/api/messages', (_request, response) => { response.sendStatus(200); });
      },
    } as unknown as App;
    const server = await createTeamsServer({
      baseUrl: 'https://teams.example', expressApp, app,
      tokenClient: {} as never, callback: {} as never, state: {} as never, maxBodyBytes: 128,
    });
    const router = (expressApp as unknown as { readonly router?: { readonly stack?: readonly { readonly name?: string; readonly handle?: (request: unknown, response: unknown, next: (error?: unknown) => void) => void }[] } }).router;
    const parser = router?.stack?.find(layer => layer.name === 'jsonParser')?.handle;
    if (!parser) throw new Error('body-limit parser was not registered');
    const body = Buffer.from(JSON.stringify({ padding: 'x'.repeat(256) }));
    const request = Object.assign(Readable.from([body]), {
      headers: { 'content-type': 'application/json', 'content-length': String(body.byteLength) }, method: 'POST', url: '/api/messages',
    });
    const error = await new Promise<unknown>(resolve => parser(request, {}, resolve));
    expect(error).toMatchObject({ status: 413, type: 'entity.too.large' });
    expect(server).toBeDefined();
  });
});

describe('Teams production message handling', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllEnvs();
    vi.useRealTimers();
  });

  function configureEnvironment(): void {
    const values = {
      TEAMS_BASE_URL: 'https://teams.example.com', QURL_ENDPOINT: 'https://qurl.example.com', AWS_REGION: 'us-east-1',
      TEAMS_APP_ID: 'a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d', TEAMS_APP_PASSWORD: 'synthetic-bot-secret',
      BOT_TENANT_ID: '22222222-2222-4222-8222-222222222222',
      QURL_IMAGE: `ghcr.io/layervai/qurl@sha256:${'a'.repeat(64)}`,
      QURL_TEAMS_TENANT_PRINCIPALS_TABLE: 'principals', QURL_TEAMS_CHANNEL_POLICIES_TABLE: 'policies',
      QURL_TEAMS_PERSONAL_CONVERSATIONS_TABLE: 'conversations', QURL_TEAMS_TENANT_CREDENTIALS_TABLE: 'credentials',
      QURL_TEAMS_TENANT_CREDENTIALS_KMS_KEY_ARN: 'synthetic-kms-key', OAUTH_STATE_TABLE: 'oauth-state',
      AUTH0_DOMAIN: 'https://auth.example.com', AUTH0_CLIENT_ID: 'synthetic-oauth-client',
      AUTH0_CLIENT_SECRET: 'synthetic-oauth-secret', AUTH0_AUDIENCE: 'https://qurl.example.com',
      QURL_CONNECTOR_HUB_HOST: '', QURL_CONNECTOR_HUB_PORT: '', QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64: '',
      AUTH0_CLIENT_SECRET_FALLBACK: '', AUTH0_EXPECTED_AUDIENCE: '', TEAMS_SERVICE_URL: '', HOST: '', PORT: '',
    };
    for (const [name, value] of Object.entries(values)) vi.stubEnv(name, value);
  }

  const activity = {
    type: 'message', id: 'activity-1', channelId: 'msteams', text: 'qurl help',
    serviceUrl: 'https://smba.trafficmanager.net/amer/',
    from: { id: '29:actor', aadObjectId: '33333333-3333-4333-8333-333333333333' },
    recipient: { id: '28:bot' },
    conversation: { id: '19:channel;messageid=activity-1', conversationType: 'channel' },
    channelData: { tenant: { id: '44444444-4444-4444-8444-444444444444' }, channel: { id: '19:channel' } },
  };

  it.each(['', 'common', 'botframework.com', 'invalid-tenant'])('rejects an invalid registration tenant %j before startup', async tenantId => {
    configureEnvironment();
    vi.stubEnv('BOT_TENANT_ID', tenantId);
    await expect(createProductionTeamsConfig().then(() => undefined)).rejects.toThrow('BOT_TENANT_ID');
  });

  it('rejects an Auth0 audience that differs from the deployment expectation before startup', async () => {
    configureEnvironment();
    vi.stubEnv('AUTH0_EXPECTED_AUDIENCE', 'https://other-api.example.com');
    await expect(createProductionTeamsConfig().then(() => undefined)).rejects.toThrow('AUTH0_AUDIENCE must match AUTH0_EXPECTED_AUDIENCE');
  });

  it('accepts a matching trimmed audience expectation independently of the API URL', async () => {
    configureEnvironment();
    vi.stubEnv('AUTH0_AUDIENCE', '  custom-resource-server  ');
    vi.stubEnv('AUTH0_EXPECTED_AUDIENCE', '\tcustom-resource-server\n');
    await expect(createProductionTeamsConfig()).resolves.toHaveProperty('server');
  });

  it.each([undefined, '', ' \t '])('preserves custom Auth0 audiences when the expectation is %j', async expected => {
    configureEnvironment();
    vi.stubEnv('AUTH0_AUDIENCE', 'custom-resource-server');
    vi.stubEnv('AUTH0_EXPECTED_AUDIENCE', expected);
    await expect(createProductionTeamsConfig()).resolves.toHaveProperty('server');
  });

  it('passes the bot registration tenant explicitly, independently of customer tenant IDs', async () => {
    configureEnvironment();
    vi.stubEnv('BOT_TENANT_ID', '  ABCDEFAB-1234-4234-8234-ABCDEFABCDEF  ');
    vi.stubEnv('TENANT_ID', '55555555-5555-4555-8555-555555555555');
    const runtime = await createProductionTeamsConfig();
    expect(runtime.app.credentials?.tenantId).toBe('abcdefab-1234-4234-8234-abcdefabcdef');
  });

  it('explains unreadable stored credentials without modifying tenant recovery state', async () => {
    configureEnvironment();
    // Keep the production data read and ciphertext validation real. Replace
    // only DynamoDB and final delivery; malformed saved ciphertext needs no KMS call.
    const send = vi.spyOn(DynamoDBDocumentClient.prototype, 'send').mockResolvedValue({
      Item: { qurl_api_key: 'kms:v2:invalid', qurl_key_id: 'key_123456789abc', qurl_binding_id: 'eib_123456789ab' },
    } as never);
    const responses: string[] = [];
    vi.spyOn(TeamsSdkMessagePoster.prototype, 'reply').mockImplementation(async (_activity, text) => { responses.push(text); });
    const errors = vi.spyOn(process.stderr, 'write').mockImplementation(() => true);
    const runtime = await createProductionTeamsConfig();

    await runtime.app.onActivity({ body: { ...activity, text: 'qurl list' }, token: { serviceUrl: activity.serviceUrl } } as never);
    await vi.waitFor(() => expect(responses).toHaveLength(1));
    expect(responses[0]).toContain('saved qURL credentials could not be read');
    expect(responses[0]).toContain('ask your qURL operator');
    expect(responses[0]).not.toMatch(/command syntax|kms:v2/);
    expect(send).toHaveBeenCalledTimes(1);
    expect(send.mock.calls[0]?.[0]).toMatchObject({ input: {
      TableName: 'credentials', Key: { tenant_id: activity.channelData.tenant.id }, ConsistentRead: true,
    } });
    expect(errors).toHaveBeenCalledTimes(1);
    expect(JSON.parse(String(errors.mock.calls[0]?.[0]))).toMatchObject({
      level: 'ERROR', error: expect.stringContaining('tenant credential ciphertext is malformed'),
    });
  });

  it('reuses the credential read for uninstall through the production factory', async () => {
    configureEnvironment();
    const credential = { apiKey: 'synthetic-key', keyId: 'key-id' };
    const read = vi.spyOn(TeamsDataStore.prototype, 'tenantCredential').mockResolvedValueOnce(credential)
      .mockRejectedValue(new Error('second credential read must not happen'));
    vi.spyOn(TeamsDataStore.prototype, 'checkAdmin').mockResolvedValue({ isAdmin: true, ownerId: 'owner', installationId: 'install' });
    const remove = vi.spyOn(TeamsDataStore.prototype, 'deleteWorkspace').mockResolvedValue();
    const revoke = vi.spyOn(HttpQurlClient.prototype, 'revokeApiKey').mockResolvedValue();
    const replies: string[] = [];
    vi.spyOn(TeamsSdkMessagePoster.prototype, 'reply').mockImplementation(async (_activity, text) => { replies.push(text); });
    const runtime = await createProductionTeamsConfig();
    await runtime.app.onActivity({ body: { ...activity, text: 'qurl uninstall' }, token: { serviceUrl: activity.serviceUrl } } as never);
    await vi.waitFor(() => expect(replies).toHaveLength(1));
    expect(replies[0]).toContain('Disconnected qURL');
    expect(read).toHaveBeenCalledTimes(1);
    expect(revoke).toHaveBeenCalledWith('key-id', expect.any(AbortSignal));
    expect(remove).toHaveBeenCalledWith(activity.channelData.tenant.id, 'install', expect.any(AbortSignal));
  });

  it('bounds concurrent message work and rejects overload until accepted final replies finish', async () => {
    configureEnvironment();
    vi.useFakeTimers();
    let finishCommands = (): void => undefined;
    let finishReplies = (): void => undefined;
    const commandsDone = new Promise<void>(resolve => { finishCommands = resolve; });
    const repliesDone = new Promise<void>(resolve => { finishReplies = resolve; });
    const execute = vi.spyOn(TeamsBot.prototype, 'execute').mockImplementation(async () => {
      await commandsDone;
      return 'qURL operation completed';
    });
    const reply = vi.spyOn(TeamsSdkMessagePoster.prototype, 'reply').mockImplementation(async () => { await repliesDone; });
    const runtime = await createProductionTeamsConfig();
    const dispatch = (id: string) => runtime.app.onActivity({ body: { ...activity, id }, token: { serviceUrl: activity.serviceUrl } } as never);
    try {
      // Exercise the actual SDK middleware/router with a concurrent burst.
      const responses = await Promise.all(Array.from({ length: 55 }, (_, i) => dispatch(`burst-${i}`)));
      expect(responses.filter(response => response.status === 200)).toHaveLength(50);
      expect(responses.filter(response => response.status === 503)).toHaveLength(5);
      expect(execute).toHaveBeenCalledTimes(50);
      expect(reply).not.toHaveBeenCalled();
      expect(await dispatch('retry-after-capacity')).toMatchObject({ status: 503 });
      finishCommands();
      await vi.advanceTimersByTimeAsync(0);
      expect(reply).toHaveBeenCalledTimes(50);
      expect(await dispatch('retry-after-capacity')).toMatchObject({ status: 503 });
      finishReplies();
      await vi.advanceTimersByTimeAsync(0);
      expect(await dispatch('retry-after-capacity')).toMatchObject({ status: 200 });
      await vi.advanceTimersByTimeAsync(0);
      expect(execute).toHaveBeenCalledTimes(51);
      expect(execute.mock.calls.filter(([value]) => value.id === 'retry-after-capacity')).toHaveLength(1);
      expect(reply).toHaveBeenCalledTimes(51);
    } finally {
      finishCommands();
      finishReplies();
      await vi.advanceTimersByTimeAsync(0);
    }
  });

  it('stops HTTP acceptance and drains detached command work through its final reply', async () => {
    configureEnvironment();
    vi.useFakeTimers();
    let finishCommand = (): void => undefined;
    let finishReply = (): void => undefined;
    const commandDone = new Promise<void>(resolve => { finishCommand = resolve; });
    const replyDone = new Promise<void>(resolve => { finishReply = resolve; });
    let workSignal: AbortSignal | undefined;
    vi.spyOn(TeamsBot.prototype, 'execute').mockImplementation(async (_activity, _tenant, _scope, _channel, _command, signal) => {
      workSignal = signal;
      await commandDone;
      return 'qURL operation completed';
    });
    const reply = vi.spyOn(TeamsSdkMessagePoster.prototype, 'reply').mockImplementation(async () => { await replyDone; });
    const runtime = await createProductionTeamsConfig();
    await new Promise<void>(resolve => { runtime.server.listen(0, '127.0.0.1', resolve); });
    try {
      await runtime.app.onActivity({ body: activity, token: { serviceUrl: activity.serviceUrl } } as never);
      await vi.advanceTimersByTimeAsync(0);
      const shutdown = runtime.shutdown();
      expect(runtime.shutdown()).toBe(shutdown);
      let drained = false;
      void shutdown.then(() => { drained = true; });
      await vi.advanceTimersByTimeAsync(0);
      expect(runtime.server.listening).toBe(false);
      expect(drained).toBe(false);
      expect(workSignal?.aborted).toBe(false);
      finishCommand();
      await vi.advanceTimersByTimeAsync(0);
      expect(reply).toHaveBeenCalledWith(expect.objectContaining({ id: activity.id, conversation: activity.conversation }), 'qURL operation completed');
      expect(drained).toBe(false);
      finishReply();
      await expect(shutdown).resolves.toBe(true);
      expect(workSignal?.aborted).toBe(false);
    } finally {
      finishCommand();
      finishReply();
      runtime.server.closeAllConnections();
      runtime.server.close();
    }
  });

  it('bounds shutdown inside the ECS stop budget without claiming stuck work drained', async () => {
    configureEnvironment();
    vi.useFakeTimers();
    let finishCommand = (): void => undefined;
    const commandDone = new Promise<void>(resolve => { finishCommand = resolve; });
    vi.spyOn(TeamsBot.prototype, 'execute').mockImplementation(async () => { await commandDone; return 'late result'; });
    vi.spyOn(TeamsSdkMessagePoster.prototype, 'reply').mockResolvedValue();
    const warnings = vi.spyOn(process.stderr, 'write').mockImplementation(() => true);
    const runtime = await createProductionTeamsConfig();
    await new Promise<void>(resolve => { runtime.server.listen(0, '127.0.0.1', resolve); });
    try {
      await runtime.app.onActivity({ body: activity, token: { serviceUrl: activity.serviceUrl } } as never);
      await vi.advanceTimersByTimeAsync(0);
      const shutdown = runtime.shutdown();
      let settled = false;
      void shutdown.then(() => { settled = true; });
      await vi.advanceTimersByTimeAsync(24_999);
      expect(settled).toBe(false);
      await vi.advanceTimersByTimeAsync(1);
      await expect(shutdown).resolves.toBe(false);
      expect(JSON.parse(String(warnings.mock.calls[0]?.[0]))).toMatchObject({
        level: 'WARN', message: 'Teams shutdown drain timed out', activeActivities: 1,
      });
    } finally {
      finishCommand();
      await vi.advanceTimersByTimeAsync(0);
      runtime.server.closeAllConnections();
      runtime.server.close();
    }
  });

  it('acknowledges a slow command before completion and gives its real reply an independent HTTP deadline', async () => {
    configureEnvironment();
    vi.useFakeTimers();
    let workSignal: AbortSignal | undefined;
    vi.spyOn(TeamsBot.prototype, 'execute').mockImplementation(async (_activity, _tenantId, _scopeId, _channel, _command, signal) => {
      workSignal = signal;
      await new Promise(resolve => setTimeout(resolve, 20_000));
      return 'qURL result';
    });
    const runtime = await createProductionTeamsConfig();
    // Only the two network boundaries are replaced. The production route,
    // TeamsBot reply selection, SDK client, and request options stay real.
    const client = runtime.app.api.http as unknown as {
      token?: () => Promise<undefined>;
      http: { defaults: { adapter: (config: { url: string; data: string; timeout: number; signal?: AbortSignal }) => Promise<never> } };
    };
    client.token = async () => undefined;
    let sent: { readonly url: string; readonly body: Record<string, unknown>; readonly timeout: number; readonly signal?: AbortSignal } | undefined;
    let timedOut = false;
    client.http.defaults.adapter = async config => {
      sent = { url: config.url, body: JSON.parse(config.data) as Record<string, unknown>, timeout: config.timeout, ...(config.signal ? { signal: config.signal } : {}) };
      return new Promise((_resolve, reject) => {
        setTimeout(() => {
          timedOut = true;
          reject(new Error('synthetic delivery timeout'));
        }, config.timeout);
      });
    };
    // handleActivity logs a failed send; keep that expected test error local.
    vi.spyOn(process.stderr, 'write').mockImplementation(() => true);
    let acknowledged = false;
    const response = runtime.app.onActivity({ body: activity, token: { serviceUrl: activity.serviceUrl } } as never)
      .then(value => { acknowledged = true; return value; });
    await vi.advanceTimersByTimeAsync(1);
    expect(acknowledged).toBe(true);
    await expect(response).resolves.toMatchObject({ status: 200 });
    expect(sent).toBeUndefined();
    await vi.advanceTimersByTimeAsync(19_999);
    expect(sent).toMatchObject({
      url: 'https://smba.trafficmanager.net/amer/v3/conversations/19%3Achannel%3Bmessageid%3Dactivity-1/activities',
      body: { type: 'message', text: 'qURL result', replyToId: 'activity-1' }, timeout: 15_000,
    });
    expect(sent?.signal).toBeUndefined();
    expect(workSignal?.aborted).toBe(false);
    await vi.advanceTimersByTimeAsync(10_000);
    expect(workSignal?.aborted).toBe(true);
    expect(timedOut).toBe(false);
    await vi.advanceTimersByTimeAsync(5_000);
    expect(timedOut).toBe(true);
  });
});
