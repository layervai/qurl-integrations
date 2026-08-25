import type { ServerResponse } from 'node:http';
import type { Application, Request } from 'express';
import { describe, expect, it } from 'vitest';
import type { OAuthCallbackCore } from '../src/callback.js';
import type { ConfidentialTokenClient } from '../src/interfaces.js';
import { installOAuthRoutes } from '../src/server.js';
import type { OAuthStateManager } from '../src/state.js';

const OPAQUE_STATE = Buffer.alloc(32, 1).toString('base64url');

type RouteHandler = (request: Request, response: ServerResponse & {
  status: (status: number) => { type: (value: string) => { set: (key: string, value: string) => { send: (value: unknown) => void } } };
  set: (key: string, value: string) => { redirect: (status: number, url: string) => void };
}) => Promise<void> | void;

function routes(input: {
  readonly authorizationRequest?: () => Promise<{ readonly codeChallenge: string; readonly nonce: string; readonly loginHint: string }>;
  readonly complete?: () => Promise<{ readonly binding: { readonly status: 'bound' | 'conflict' } }>;
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
});
