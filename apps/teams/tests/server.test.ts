import type { ServerResponse } from 'node:http';
import { Readable } from 'node:stream';
import type { Application, Request } from 'express';
import express from 'express';
import type { App } from '@microsoft/teams.apps';
import { describe, expect, it } from 'vitest';
import type { OAuthCallbackCore } from '../src/callback.js';
import type { ConfidentialTokenClient } from '../src/interfaces.js';
import { createProductionTeamsConfig, createTeamsServer, httpsIssuer, httpsOrigin, installOAuthRoutes, isMainModule } from '../src/server.js';
import type { OAuthStateManager } from '../src/state.js';

const OPAQUE_STATE = Buffer.alloc(32, 1).toString('base64url');

type RouteHandler = (request: Request, response: ServerResponse & {
  status: (status: number) => { type: (value: string) => { set: (key: string, value: string) => { send: (value: unknown) => void } } };
  set: (key: string, value: string) => { redirect: (status: number, url: string) => void };
}) => Promise<void> | void;

function routes(input: {
  readonly authorizationRequest?: () => Promise<{ readonly codeChallenge: string; readonly nonce: string; readonly loginHint: string }>;
  readonly complete?: () => Promise<{ readonly binding: { readonly status: 'bound' } | { readonly status: 'conflict'; readonly reason?: string } }>;
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
});

describe('Teams production URL configuration', () => {
  it('only treats the exact server module as the executable entrypoint', () => {
    expect(isMainModule('/srv/server.js', 'file:///srv/server.js')).toBe(true);
    expect(isMainModule('/srv/server.js', 'file:///srv/consumer.js')).toBe(false);
  });

  it('accepts HTTPS origins and preserves the OIDC issuer trailing slash', () => {
    expect(httpsOrigin('https://teams.example', 'TEAMS_BASE_URL')).toBe('https://teams.example');
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
