import { describe, expect, it } from 'vitest';
import { OAuthCoreError } from '../src/errors.js';
import type { FetchLike, LogContext, Logger } from '../src/interfaces.js';
import { RedactingLogger } from '../src/logger.js';
import { createConfidentialTokenClient } from '../src/token-client.js';

const PRIMARY_SECRET = 'synthetic-primary-client-secret';
const FALLBACK_SECRET = 'synthetic-fallback-client-secret';
const STATE = Buffer.alloc(32, 10).toString('base64url');
const VERIFIER = Buffer.alloc(32, 11).toString('base64url');
const CHALLENGE = Buffer.alloc(32, 12).toString('base64url');
const NONCE = Buffer.alloc(32, 13).toString('base64url');

function tokenClient(fetch: FetchLike, overrides: {
  readonly logger?: Logger;
  readonly fallback?: string;
  readonly timeoutMs?: number;
  readonly bodyLimit?: number;
} = {}) {
  return createConfidentialTokenClient({
    issuer: 'https://auth.example.com/',
    clientId: 'synthetic-teams-client',
    clientSecret: PRIMARY_SECRET,
    ...(overrides.fallback === undefined ? {} : { clientSecretFallback: overrides.fallback }),
    audience: 'https://api.example.com/',
    redirectUri: 'https://teams-bot.example.com/oauth/qurl/callback',
    fetch,
    ...(overrides.logger === undefined ? {} : { logger: overrides.logger }),
    ...(overrides.timeoutMs === undefined ? {} : { timeoutMs: overrides.timeoutMs }),
    ...(overrides.bodyLimit === undefined ? {} : { responseBodyLimitBytes: overrides.bodyLimit }),
  });
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function expectCode(error: unknown, code: OAuthCoreError['code']): boolean {
  return error instanceof OAuthCoreError && error.code === code;
}

describe('confidential token client', () => {
  it('builds an authorization request with exactly the approved scopes and no offline access', () => {
    const client = tokenClient(async () => jsonResponse({}));
    const url = client.createAuthorizationUrl({
      state: STATE,
      codeChallenge: CHALLENGE,
      nonce: NONCE,
      loginHint: 'Admin@Example.com',
    });

    expect(url.origin).toBe('https://auth.example.com');
    expect(url.pathname).toBe('/authorize');
    expect(url.searchParams.get('scope')).toBe('openid email qurl:read qurl:write');
    expect(url.searchParams.get('scope')?.split(' ')).toEqual([
      'openid', 'email', 'qurl:read', 'qurl:write',
    ]);
    expect(url.search).not.toContain('offline_access');
    expect(url.searchParams.get('code_challenge_method')).toBe('S256');
    expect(url.searchParams.get('login_hint')).toBe('admin@example.com');
  });

  it('sends the client secret and transaction PKCE verifier in a form-encoded exchange', async () => {
    let submitted: URLSearchParams | undefined;
    const client = tokenClient(async (input, init) => {
      expect(new URL(input).toString()).toBe('https://auth.example.com/oauth/token');
      expect(init?.method).toBe('POST');
      submitted = new URLSearchParams(String(init?.body));
      return jsonResponse({
        access_token: 'synthetic-access-token',
        id_token: 'synthetic-id-token',
        refresh_token: 'synthetic-refresh-token-that-must-be-ignored',
      });
    });

    const result = await client.exchangeAuthorizationCode({
      code: 'synthetic-authorization-code',
      codeVerifier: VERIFIER,
    });
    expect(submitted?.get('client_secret')).toBe(PRIMARY_SECRET);
    expect(submitted?.get('code_verifier')).toBe(VERIFIER);
    expect(submitted?.get('grant_type')).toBe('authorization_code');
    expect(result).toEqual({
      accessToken: 'synthetic-access-token',
      idToken: 'synthetic-id-token',
    });
    expect(result).not.toHaveProperty('refreshToken');
  });

  it('uses the fallback secret once and only after an exact invalid_client response', async () => {
    const submittedSecrets: string[] = [];
    const client = tokenClient(async (_input, init) => {
      const form = new URLSearchParams(String(init?.body));
      submittedSecrets.push(form.get('client_secret') ?? '');
      if (submittedSecrets.length === 1) {
        return jsonResponse({ error: 'invalid_client', error_description: PRIMARY_SECRET }, 401);
      }
      return jsonResponse({ access_token: 'synthetic-access-token', id_token: 'synthetic-id-token' });
    }, { fallback: FALLBACK_SECRET });

    await expect(client.exchangeAuthorizationCode({
      code: 'synthetic-authorization-code',
      codeVerifier: VERIFIER,
    })).resolves.toMatchObject({ accessToken: 'synthetic-access-token' });
    expect(submittedSecrets).toEqual([PRIMARY_SECRET, FALLBACK_SECRET]);
  });

  it.each([
    ['access_denied', 400],
    ['invalid_grant', 400],
    ['server_error', 503],
  ])('does not use the fallback for %s', async (oauthError, status) => {
    let calls = 0;
    const client = tokenClient(async () => {
      calls += 1;
      return jsonResponse({ error: oauthError }, status);
    }, { fallback: FALLBACK_SECRET });

    await expect(client.exchangeAuthorizationCode({
      code: 'synthetic-authorization-code',
      codeVerifier: VERIFIER,
    })).rejects.toSatisfy((error: unknown) => expectCode(error, 'TOKEN_ENDPOINT_REJECTED'));
    expect(calls).toBe(1);
  });

  it('does not use the fallback for network failures', async () => {
    let calls = 0;
    const client = tokenClient(async () => {
      calls += 1;
      throw new Error(`network echoed ${PRIMARY_SECRET}`);
    }, { fallback: FALLBACK_SECRET });

    await expect(client.exchangeAuthorizationCode({
      code: 'synthetic-authorization-code',
      codeVerifier: VERIFIER,
    })).rejects.toSatisfy((error: unknown) => expectCode(error, 'TOKEN_ENDPOINT_REJECTED'));
    expect(calls).toBe(1);
  });

  it('bounds token response bodies and does not inspect a truncated invalid_client body', async () => {
    let calls = 0;
    const client = tokenClient(async () => {
      calls += 1;
      return new Response(JSON.stringify({ error: 'invalid_client', padding: 'x'.repeat(256) }), { status: 401 });
    }, { fallback: FALLBACK_SECRET, bodyLimit: 64 });

    await expect(client.exchangeAuthorizationCode({
      code: 'synthetic-authorization-code',
      codeVerifier: VERIFIER,
    })).rejects.toSatisfy((error: unknown) => expectCode(error, 'TOKEN_RESPONSE_TOO_LARGE'));
    expect(calls).toBe(1);
  });

  it('enforces one strict timeout across the exchange', async () => {
    const client = tokenClient(
      () => new Promise<Response>(() => undefined),
      { fallback: FALLBACK_SECRET, timeoutMs: 10 },
    );
    await expect(client.exchangeAuthorizationCode({
      code: 'synthetic-authorization-code',
      codeVerifier: VERIFIER,
    })).rejects.toSatisfy((error: unknown) => expectCode(error, 'TOKEN_TIMEOUT'));
  });

  it('keeps secrets and upstream response text out of logs and thrown errors', async () => {
    const logs: Array<{ message: string; context?: LogContext }> = [];
    const sink: Logger = {
      debug: (message, context) => logs.push({ message, ...(context === undefined ? {} : { context }) }),
      info: (message, context) => logs.push({ message, ...(context === undefined ? {} : { context }) }),
      warn: (message, context) => logs.push({ message, ...(context === undefined ? {} : { context }) }),
      error: (message, context) => logs.push({ message, ...(context === undefined ? {} : { context }) }),
    };
    const logger = new RedactingLogger(sink, [PRIMARY_SECRET, FALLBACK_SECRET]);
    const upstreamEcho = `echo-${PRIMARY_SECRET}-${FALLBACK_SECRET}`;
    const client = tokenClient(async () => jsonResponse({
      error: 'invalid_client',
      error_description: upstreamEcho,
    }, 401), { fallback: FALLBACK_SECRET, logger });

    let thrown: unknown;
    try {
      await client.exchangeAuthorizationCode({
        code: 'synthetic-authorization-code',
        codeVerifier: VERIFIER,
      });
    } catch (error) {
      thrown = error;
    }
    const rendered = `${JSON.stringify(logs)} ${String(thrown)}`;
    expect(rendered).not.toContain(PRIMARY_SECRET);
    expect(rendered).not.toContain(FALLBACK_SECRET);
    expect(rendered).not.toContain(upstreamEcho);
    expect(logs).toHaveLength(1);
  });

  it('sanitizes an unexpected OAuth error value before attaching safe details', async () => {
    const client = tokenClient(async () => jsonResponse({ error: PRIMARY_SECRET }, 400));
    let thrown: unknown;
    try {
      await client.exchangeAuthorizationCode({
        code: 'synthetic-authorization-code',
        codeVerifier: VERIFIER,
      });
    } catch (error) {
      thrown = error;
    }
    expect(thrown).toBeInstanceOf(OAuthCoreError);
    expect(JSON.stringify(thrown)).not.toContain(PRIMARY_SECRET);
    expect((thrown as OAuthCoreError).safeDetails.errorCode).toBe('unknown_oauth_error');
  });
});
