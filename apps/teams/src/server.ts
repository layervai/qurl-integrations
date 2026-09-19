#!/usr/bin/env node

import { Console } from 'node:console';
import { realpathSync } from 'node:fs';
import { createServer, type Server, type ServerResponse } from 'node:http';
import { pathToFileURL } from 'node:url';
import express from 'express';
import type { Application, Request, ErrorRequestHandler } from 'express';
import { App, ExpressAdapter } from '@microsoft/teams.apps';
import { DynamoDBClient } from '@aws-sdk/client-dynamodb';
import { DynamoDBDocumentClient } from '@aws-sdk/lib-dynamodb';
import { OAuthCallbackCore } from './callback.js';
import { isOAuthCoreError, OAuthCoreError } from './errors.js';
import { TeamsSetupLinkBuilder } from './setup-link.js';
import { createOAuthStateCookie, clearOAuthStateCookie, OAUTH_STATE_COOKIE_NAME } from './cookies.js';
import { createConfidentialTokenClient } from './token-client.js';
import { createIdTokenVerifier } from './id-token-verifier.js';
import { OAuthStateManager } from './state.js';
import { DynamoOAuthStatePersistence } from './oauth-state-store.js';
import { HttpProviderBinder } from './provider-binder.js';
import { HttpQurlClient } from './qurl-client.js';
import { createDynamoClient, TeamsDataStore, type TenantCredential } from './teams-data.js';
import { KmsCredentialCipher } from './credential-cipher.js';
import { TeamsBot } from './bot.js';
import { TeamsSdkMessagePoster, validateTeamsServiceUrl } from './teams-sdk.js';
import { validateTunnelHub, validateTunnelImageRef, type TunnelHub } from './tunnel.js';
import { UserFacingError } from './user-facing-error.js';
import type { ConfidentialTokenClient, FetchLike } from './interfaces.js';
import type { Logger } from './interfaces.js';
import { jsonConsoleSink, RedactingLogger } from './logger.js';
import { toTeamsActivity } from './activity.js';

const DEFAULT_MAX_BODY_BYTES = 1_048_576;
const ACTIVITY_TIMEOUT_MS = 30_000;
// ponytail: match Slack's 50-slot process pool; split by tenant if load requires it.
const MAX_ACTIVE_MESSAGES = 50;
// TODO(upstream-contract): qurl-webhook-runtime gives the task 30 seconds
// after SIGTERM. Leave time for exit without shortening active work signals.
const SHUTDOWN_TIMEOUT_MS = 25_000;
const DEFAULT_HOST = '127.0.0.1';
const runtimeConsole = new Console({ stdout: process.stdout, stderr: process.stderr });

export interface TeamsServerOptions {
  readonly baseUrl: string;
  readonly app: App;
  readonly expressApp: Application;
  readonly tokenClient: ConfidentialTokenClient;
  readonly callback: OAuthCallbackCore;
  readonly state: OAuthStateManager;
  readonly maxBodyBytes?: number;
  readonly logger?: Logger;
}

function escapeHtml(value: string): string {
  return value.replace(/[&<>"']/g, character => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[character] ?? character));
}

function html(response: ServerResponse, status: number, title: string, message: string): void {
  response.writeHead(status, {
    'Content-Type': 'text/html; charset=utf-8',
    'Cache-Control': 'no-store',
    'Content-Security-Policy': "default-src 'none'; frame-ancestors 'none'; base-uri 'none'",
    'Referrer-Policy': 'no-referrer',
    'X-Content-Type-Options': 'nosniff',
    'X-Frame-Options': 'DENY',
  });
  response.end(`<!doctype html><meta charset="utf-8"><meta name="robots" content="noindex"><title>${escapeHtml(title)}</title><main><h1>${escapeHtml(title)}</h1><p>${escapeHtml(message)}</p></main>`);
}

function setCookie(response: ServerResponse, cookie: { readonly name: string; readonly value: string; readonly path: string; readonly maxAgeSeconds: number; readonly secure: boolean; readonly httpOnly: boolean; readonly sameSite: string }): void {
  const attributes = [`Path=${cookie.path}`, `Max-Age=${cookie.maxAgeSeconds}`, ...(cookie.httpOnly ? ['HttpOnly'] : []), ...(cookie.secure ? ['Secure'] : []), `SameSite=${cookie.sameSite}`];
  response.setHeader('Set-Cookie', `${cookie.name}=${cookie.value}; ${attributes.join('; ')}`);
}

function readCookie(request: Request, name: string): string | undefined {
  const header = request.header('cookie');
  if (!header) return undefined;
  for (const entry of header.split(';')) {
    const separator = entry.indexOf('=');
    if (separator >= 0 && entry.slice(0, separator).trim() === name) return entry.slice(separator + 1).trim();
  }
  return undefined;
}

function logOAuthFailure(logger: Logger | undefined, message: string, error: unknown): void {
  // Stale links, replays and browser cookie mismatches are routine rejections.
  const expected = isOAuthCoreError(error) && ['INVALID_STATE', 'STATE_NOT_FOUND', 'STATE_EXPIRED', 'COOKIE_MISMATCH'].includes(error.code);
  logger?.[expected ? 'warn' : 'error'](message, { error });
}

export function installOAuthRoutes(options: TeamsServerOptions): void {
  const { expressApp } = options;
  expressApp.get('/health', (_request, response) => {
    response.status(200).type('application/json').set('Cache-Control', 'no-store').send({ ok: true });
  });

  expressApp.get('/oauth/qurl/start', async (request, response) => {
    try {
      const state = request.query.state;
      if (typeof state !== 'string') throw new OAuthCoreError('INVALID_STATE', 'Invalid setup link.');
      const authorizationRequest = await options.state.authorizationRequest(state);
      const authorization = options.tokenClient.createAuthorizationUrl({
        state,
        codeChallenge: authorizationRequest.codeChallenge,
        nonce: authorizationRequest.nonce,
        loginHint: authorizationRequest.loginHint,
      });
      const cookie = createOAuthStateCookie(state);
      setCookie(response, cookie);
      response.set('Cache-Control', 'no-store').redirect(302, authorization.toString());
    } catch (error) {
      logOAuthFailure(options.logger, 'Teams OAuth start failed', error);
      setCookie(response, clearOAuthStateCookie());
      html(response, 400, 'qURL setup link invalid', 'The qURL setup link is invalid or expired. Return to Teams and run setup again.');
    }
  });

  expressApp.get('/oauth/qurl/callback', async (request, response) => {
    let status = 400;
    let title = 'qURL setup failed';
    let message = 'The qURL setup could not be completed. Return to Teams and run setup again.';
    try {
      const state = request.query.state;
      const code = request.query.code;
      if (request.query.error || typeof state !== 'string' || typeof code !== 'string') {
        title = 'qURL setup incomplete';
        message = 'The qURL setup was cancelled or the callback was incomplete.';
      } else {
        const cookieState = readCookie(request, OAUTH_STATE_COOKIE_NAME);
        // OAuthCallbackCore rejects an absent cookie before it consumes state.
        const completion = await options.callback.complete({ state, code, ...(cookieState === undefined ? {} : { cookieState }) });
        if (completion.binding.status === 'conflict') {
          status = completion.binding.reason === 'actor_not_authorized' ? 403 : 409;
          title = 'qURL setup blocked';
          message = completion.binding.reason === 'upstream_binding_cleanup_required'
            ? 'A previous qURL installation still has an upstream tenant binding. Ask your qURL operator to remove that binding before reinstalling.'
            : completion.binding.reason === 'actor_not_authorized'
            ? 'qURL could not authorize this setup. Ask your qURL operator to check the account permissions and Teams application configuration, then run setup again.'
            : 'This Teams tenant is already connected to another qURL account.';
        } else {
          status = 200;
          title = 'qURL connected';
          message = 'qURL is connected to this Microsoft Teams tenant. You can close this tab and return to Teams.';
        }
      }
    } catch (error) {
      // Do not expose OAuth codes, tokens, state, or upstream error details.
      logOAuthFailure(options.logger, 'Teams OAuth callback failed', error);
    }
    setCookie(response, clearOAuthStateCookie());
    html(response, status, title, message);
  });
}

/** Official Teams SDK owns POST /api/messages, Activity routing, JWT
 * validation, and Connector responses. qURL OAuth remains separate. */
export async function createTeamsServer(options: TeamsServerOptions): Promise<Server> {
  // ExpressAdapter also installs express.json() for its message route and
  // consumes req.body. Parsing first is supported by Express and preserves
  // qURL's intentional 1 MiB Activity limit for cards and attachments. This
  // is the effective /api/messages ceiling, including ahead of the SDK parser.
  // TODO(upstream-contract): verify this parser-ordering contract on every
  // @microsoft/teams.apps upgrade; the route-level SDK parser must continue
  // respecting an already parsed body.
  options.expressApp.disable('x-powered-by');
  options.expressApp.use(express.json({ limit: options.maxBodyBytes ?? DEFAULT_MAX_BODY_BYTES }));
  installOAuthRoutes(options);
  await options.app.initialize();
  const handleError: ErrorRequestHandler = (error: unknown, _request, response, next) => {
    if (response.headersSent) { next(error); return; }
    const status = error && typeof error === 'object' && 'status' in error ? error.status : undefined;
    const code = typeof status === 'number' && Number.isInteger(status) && status >= 400 && status < 600 ? status : 500;
    if (code >= 500) options.logger?.error('Teams HTTP request failed', { error });
    response.sendStatus(code);
  };
  options.expressApp.use(handleError);
  return createServer(options.expressApp);
}

export interface TeamsProductionConfig {
  readonly server: Server;
  readonly app: App;
  readonly port: number;
  readonly host: string;
  readonly shutdown: () => Promise<boolean>;
}

/**
 * True when this module is the process entrypoint.
 *
 * `realpath` matters: launched through the `qurl-teams` bin symlink
 * (`node_modules/.bin/qurl-teams`, `npx qurl-teams`), `process.argv[1]` keeps
 * the SYMLINK path while the ESM loader resolves `import.meta.url` to the
 * realpath of `dist/server.js`. Comparing them raw returns false, nothing
 * calls `listen()`, the event loop drains, and the process exits 0 with no
 * output -- a container healthcheck sees a clean exit, not a crash loop.
 */
export function isMainModule(entrypoint: string | undefined, moduleUrl: string): boolean {
  if (entrypoint === undefined) return false;
  let resolved = entrypoint;
  try {
    resolved = realpathSync(entrypoint);
  } catch {
    // Entrypoint may not exist on disk (bundled/virtual). Fall back to the
    // raw path rather than refusing to start.
  }
  return pathToFileURL(resolved).href === moduleUrl;
}

function env(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) throw new Error(`${name} is required`);
  return value;
}

function optionalEnv(name: string): string {
  return process.env[name]?.trim() ?? '';
}

/**
 * The NHP Hub triple is all-or-nothing. A partial triple is a deployment
 * error, not a degraded mode: rendering two of three values produces an
 * install that fails at knock time with nothing pointing back at the cause.
 */
function connectorHubFromEnv(): TunnelHub | undefined {
  const host = optionalEnv('QURL_CONNECTOR_HUB_HOST');
  const port = optionalEnv('QURL_CONNECTOR_HUB_PORT');
  const serverPublicKeyB64 = optionalEnv('QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64');
  const set = [host, port, serverPublicKeyB64].filter(value => value !== '');
  if (set.length === 0) return undefined;
  if (set.length !== 3) throw new Error('QURL_CONNECTOR_HUB_HOST, QURL_CONNECTOR_HUB_PORT, and QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64 must be set together');
  const hub = { host, port, serverPublicKeyB64 };
  validateTunnelHub(hub);
  return hub;
}

export function httpsOrigin(value: string, name: string): string {
  const url = new URL(value.includes('://') ? value : `https://${value}`);
  // qURL and the public Teams callback may be served on a non-default HTTPS
  // port. Bot Framework service URLs are stricter and are validated separately.
  if (url.protocol !== 'https:' || url.username || url.password || url.pathname !== '/' || url.search || url.hash) throw new Error(`${name} must be an HTTPS origin`);
  return url.toString().replace(/\/$/, '');
}

export function httpsIssuer(value: string, name: string): string {
  const url = new URL(value.includes('://') ? value : `https://${value}`);
  if (url.protocol !== 'https:' || url.username || url.password || url.pathname !== '/' || url.search || url.hash) throw new Error(`${name} must be an HTTPS issuer`);
  // OIDC issuer identifiers are exact strings; Auth0 includes the root slash
  // in both discovery metadata and the ID-token iss claim.
  return url.toString();
}

class TenantQurlClientFactory {
  readonly #data: TeamsDataStore;
  readonly #endpoint: string;
  readonly #logger: Logger;
  constructor(data: TeamsDataStore, endpoint: string, logger: Logger) { this.#data = data; this.#endpoint = endpoint; this.#logger = logger; }
  async forTenant(tenantId: string, existingCredential?: TenantCredential): Promise<HttpQurlClient> {
    const credential = existingCredential ?? await this.#data.tenantCredential(tenantId).catch((error: unknown) => {
      this.#logger.error('Tenant credentials could not be read', { tenantId, error });
      throw new UserFacingError('The saved qURL credentials could not be read. Please try again in a moment. If this keeps happening, ask your qURL operator to restore credential access or complete recovery.');
    });
    // Deliberately per-activity: a ConsistentRead GetItem plus a KMS Decrypt on
    // every command. No cache, so `uninstall` and credential rotation take
    // effect on the very next message everywhere, with no invalidation path to
    // get wrong. If the interactive latency or the KMS bill becomes the
    // constraint, a short-TTL cache invalidated on saveTenantCredential and
    // deleteWorkspace is the upgrade -- see qurl-integrations #1446.
    if (!credential) throw new UserFacingError('This Teams tenant is not connected to qURL yet. Run `qurl setup <your-email>` in a personal chat with the bot.');
    return new HttpQurlClient({ endpoint: this.#endpoint, apiKey: credential.apiKey, userAgent: 'qurl-teams/1' });
  }
}

export async function createProductionTeamsConfig(): Promise<TeamsProductionConfig> {
  const baseUrl = httpsOrigin(env('TEAMS_BASE_URL'), 'TEAMS_BASE_URL');
  const qurlEndpoint = httpsOrigin(env('QURL_ENDPOINT'), 'QURL_ENDPOINT');
  const auth0Audience = env('AUTH0_AUDIENCE');
  const expectedAudience = optionalEnv('AUTH0_EXPECTED_AUDIENCE');
  if (expectedAudience && auth0Audience !== expectedAudience) throw new Error('AUTH0_AUDIENCE must match AUTH0_EXPECTED_AUDIENCE');
  const region = env('AWS_REGION');
  const appId = env('TEAMS_APP_ID');
  const appPassword = env('TEAMS_APP_PASSWORD');
  const botTenantId = env('BOT_TENANT_ID').toLowerCase();
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(botTenantId)) throw new Error('BOT_TENANT_ID must be the bot registration tenant UUID');
  const connectorImage = env('QURL_IMAGE');
  validateTunnelImageRef(connectorImage);
  const connectorHub = connectorHubFromEnv();
  const ddb = createDynamoClient(DynamoDBDocumentClient.from(new DynamoDBClient({ region }), { marshallOptions: { removeUndefinedValues: true } }));
  const data = new TeamsDataStore({
    client: ddb,
    tenantPrincipalsTable: env('QURL_TEAMS_TENANT_PRINCIPALS_TABLE'),
    channelPoliciesTable: env('QURL_TEAMS_CHANNEL_POLICIES_TABLE'),
    personalConversationsTable: env('QURL_TEAMS_PERSONAL_CONVERSATIONS_TABLE'),
    tenantCredentialsTable: env('QURL_TEAMS_TENANT_CREDENTIALS_TABLE'),
    credentialCipher: new KmsCredentialCipher({ keyId: env('QURL_TEAMS_TENANT_CREDENTIALS_KMS_KEY_ARN'), region }),
  });
  const oauthState = new OAuthStateManager({ persistence: new DynamoOAuthStatePersistence({ client: ddb, tableName: env('OAUTH_STATE_TABLE') }) });
  const auth0Issuer = httpsIssuer(env('AUTH0_DOMAIN'), 'AUTH0_DOMAIN');
  const tokenClient = createConfidentialTokenClient({
    issuer: auth0Issuer,
    clientId: env('AUTH0_CLIENT_ID'),
    clientSecret: env('AUTH0_CLIENT_SECRET'),
    // Trimmed like every other config value: an all-whitespace parameter is
    // not a usable rotation secret and must not read as "configured".
    ...(optionalEnv('AUTH0_CLIENT_SECRET_FALLBACK') ? { clientSecretFallback: optionalEnv('AUTH0_CLIENT_SECRET_FALLBACK') } : {}),
    audience: auth0Audience,
    redirectUri: `${baseUrl}/oauth/qurl/callback`,
    fetch: fetch as FetchLike,
  });
  const verifier = createIdTokenVerifier({ issuer: auth0Issuer, audience: env('AUTH0_CLIENT_ID'), fetch: fetch as FetchLike });
  // JSON lines are required by the CloudWatch application alarm filters.
  const logger = new RedactingLogger(jsonConsoleSink(runtimeConsole));
  const binder = new HttpProviderBinder({ endpoint: qurlEndpoint, data, logger });
  const callback = new OAuthCallbackCore({ state: oauthState, tokenClient, idTokenVerifier: verifier, providerBinder: binder, logger });
  const expressApp = express();
  const configuredServiceUrl = process.env.TEAMS_SERVICE_URL?.trim();
  const serviceUrl = configuredServiceUrl ? validateTeamsServiceUrl(configuredServiceUrl) : undefined;
  const app = new App({
    clientId: appId,
    clientSecret: appPassword,
    tenantId: botTenantId,
    httpServerAdapter: new ExpressAdapter(expressApp),
    messagingEndpoint: '/api/messages',
    ...(serviceUrl === undefined ? {} : { serviceUrl }),
    // Keep mention normalization in the existing qURL Activity adapter.
    activity: { mentions: { stripText: false } },
  });
  const bot = new TeamsBot({
    qurlForTenant: new TenantQurlClientFactory(data, qurlEndpoint, logger),
    data,
    messages: new TeamsSdkMessagePoster(app),
    connectorImage,
    qurlEndpoint,
    validateServiceUrl: validateTeamsServiceUrl,
    ...(connectorHub ? { connectorHub } : {}),
    setup: new TeamsSetupLinkBuilder({ state: oauthState, tokenClient, setupBaseUrl: baseUrl }),
    logger,
  });
  const activeActivities = new Set<Promise<void>>();
  // TODO(upstream-contract): the SDK propagates this status after authentication.
  // next() enters the message handler synchronously, and its HTTP response is
  // written after that handler returns. Admission and shutdown depend on this.
  // Reject excess work before acknowledging it or starting any bot side effect.
  app.use(({ activity, next }) => activity.type.toLowerCase() === 'message' && activeActivities.size >= MAX_ACTIVE_MESSAGES
    ? { status: 503 }
    : next());
  app.on('message', ({ activity }) => {
    const normalized = toTeamsActivity(activity);
    if (normalized) {
      const controller = new AbortController();
      const timeout = setTimeout(() => controller.abort(), ACTIVITY_TIMEOUT_MS);
      // TODO(upstream-contract): Teams retries activities held past 15 seconds.
      // Acknowledge after SDK authentication, then complete in this service.
      // The message adapter preserves the conversation/thread. Final replies
      // use its own HTTP timeout independently of this work signal.
      const work = bot.handleActivity(normalized, controller.signal)
        .catch(error => { logger.error('Teams activity handling failed', { error }); })
        .finally(() => { clearTimeout(timeout); activeActivities.delete(work); });
      activeActivities.add(work);
    }
  });
  app.on('activity', async ({ activity }) => {
    const normalized = toTeamsActivity(activity);
    if (normalized?.type === 'conversationUpdate') {
      try { await bot.captureConversation(normalized); }
      catch (error) { logger.warn('Teams conversation capture failed', { error }); }
    }
  });
  const server = await createTeamsServer({ baseUrl, app, expressApp, tokenClient, callback, state: oauthState, logger });
  const host = process.env.HOST?.trim() || DEFAULT_HOST;
  const port = Number(process.env.PORT?.trim() || '3000');
  if (!Number.isInteger(port) || port < 1 || port > 65_535) throw new Error('PORT is invalid');
  let shutdownPromise: Promise<boolean> | undefined;
  const shutdown = (): Promise<boolean> => {
    shutdownPromise ??= (async () => {
      let timeout: ReturnType<typeof setTimeout> | undefined;
      try {
        // App.stop() cannot close an externally managed Express server.
        // Finish admitted HTTP requests first, so every acknowledged activity
        // is in the set before taking the drain snapshot. Do not abort work:
        // uncertain sharing restarts still need readback and compensation.
        const drained = await Promise.race([
          new Promise<void>((resolve, reject) => {
            server.close(error => { if (error) reject(error); else resolve(); });
          }).then(async () => { await Promise.allSettled(activeActivities); return true; }),
          new Promise<boolean>(resolve => { timeout = setTimeout(() => resolve(false), SHUTDOWN_TIMEOUT_MS); }),
        ]);
        if (!drained) {
          logger.warn('Teams shutdown drain timed out', { activeActivities: activeActivities.size });
          server.closeAllConnections();
        }
        return drained;
      } catch (error) {
        logger.error('Teams shutdown failed', { error });
        server.closeAllConnections();
        return false;
      } finally {
        clearTimeout(timeout);
      }
    })();
    return shutdownPromise;
  };
  return { server, app, port, host, shutdown };
}

if (isMainModule(process.argv[1], import.meta.url)) {
  const runtime = await createProductionTeamsConfig();
  const shutdown = (): void => { void runtime.shutdown().then(drained => { process.exit(drained ? 0 : 1); }); };
  process.on('SIGTERM', shutdown);
  process.on('SIGINT', shutdown);
  runtime.server.listen(runtime.port, runtime.host, () => { process.stdout.write(`Teams bot listening on ${runtime.host}:${runtime.port}\n`); });
}
