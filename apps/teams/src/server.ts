#!/usr/bin/env node

import { Console } from 'node:console';
import { createServer, type Server, type ServerResponse } from 'node:http';
import { pathToFileURL } from 'node:url';
import express from 'express';
import type { Application, Request } from 'express';
import { App, ExpressAdapter } from '@microsoft/teams.apps';
import { DynamoDBClient } from '@aws-sdk/client-dynamodb';
import { DynamoDBDocumentClient } from '@aws-sdk/lib-dynamodb';
import { OAuthCallbackCore } from './callback.js';
import { TeamsSetupLinkBuilder } from './setup-link.js';
import { createOAuthStateCookie, clearOAuthStateCookie, OAUTH_STATE_COOKIE_NAME } from './cookies.js';
import { createConfidentialTokenClient } from './token-client.js';
import { createIdTokenVerifier } from './id-token-verifier.js';
import { OAuthStateManager } from './state.js';
import { DynamoOAuthStatePersistence } from './oauth-state-store.js';
import { HttpProviderBinder } from './provider-binder.js';
import { HttpQurlClient } from './qurl-client.js';
import { createDynamoClient, TeamsDataStore } from './teams-data.js';
import { KmsCredentialCipher } from './credential-cipher.js';
import { TeamsBot } from './bot.js';
import { TeamsSdkMessagePoster, validateTeamsServiceUrl } from './teams-sdk.js';
import { validateTunnelImageRef } from './tunnel.js';
import type { ConfidentialTokenClient, FetchLike } from './interfaces.js';
import type { Logger } from './interfaces.js';
import { RedactingLogger } from './logger.js';
import { toTeamsActivity } from './activity.js';

const DEFAULT_MAX_BODY_BYTES = 1_048_576;
const ACTIVITY_TIMEOUT_MS = 30_000;
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

export function installOAuthRoutes(options: TeamsServerOptions): void {
  const { expressApp } = options;
  expressApp.get('/health', (_request, response) => {
    response.status(200).type('application/json').set('Cache-Control', 'no-store').send({ ok: true });
  });

  expressApp.get('/oauth/qurl/start', async (request, response) => {
    try {
      const state = request.query.state;
      if (typeof state !== 'string') throw new Error('invalid setup link');
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
      options.logger?.error('Teams OAuth start failed', { error });
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
          status = 409;
          title = 'qURL setup blocked';
          message = 'This Teams tenant is already connected to another qURL account.';
        } else {
          status = 200;
          title = 'qURL connected';
          message = 'qURL is connected to this Microsoft Teams tenant. You can close this tab and return to Teams.';
        }
      }
    } catch (error) {
      // Do not expose OAuth codes, tokens, state, or upstream error details.
      options.logger?.error('Teams OAuth callback failed', { error });
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
  options.expressApp.use(express.json({ limit: options.maxBodyBytes ?? DEFAULT_MAX_BODY_BYTES }));
  installOAuthRoutes(options);
  await options.app.initialize();
  return createServer(options.expressApp);
}

export interface TeamsProductionConfig {
  readonly server: Server;
  readonly app: App;
  readonly port: number;
  readonly host: string;
}

export function isMainModule(entrypoint: string | undefined, moduleUrl: string): boolean {
  return entrypoint !== undefined && pathToFileURL(entrypoint).href === moduleUrl;
}

function env(name: string): string {
  const value = process.env[name]?.trim();
  if (!value) throw new Error(`${name} is required`);
  return value;
}

export function httpsOrigin(value: string, name: string): string {
  const url = new URL(value.includes('://') ? value : `https://${value}`);
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
  constructor(data: TeamsDataStore, endpoint: string) { this.#data = data; this.#endpoint = endpoint; }
  async forTenant(tenantId: string): Promise<HttpQurlClient> {
    const credential = await this.#data.tenantCredential(tenantId);
    if (!credential) throw new Error('Teams tenant is not connected to qURL');
    return new HttpQurlClient({ endpoint: this.#endpoint, apiKey: credential.apiKey, userAgent: 'qurl-teams/1' });
  }
}

export async function createProductionTeamsConfig(): Promise<TeamsProductionConfig> {
  const baseUrl = httpsOrigin(env('TEAMS_BASE_URL'), 'TEAMS_BASE_URL');
  const qurlEndpoint = httpsOrigin(env('QURL_ENDPOINT'), 'QURL_ENDPOINT');
  const region = env('AWS_REGION');
  const appId = env('TEAMS_APP_ID');
  const appPassword = env('TEAMS_APP_PASSWORD');
  const connectorImage = env('QURL_CONNECTOR_IMAGE');
  validateTunnelImageRef(connectorImage);
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
    ...(process.env.AUTH0_CLIENT_SECRET_FALLBACK ? { clientSecretFallback: process.env.AUTH0_CLIENT_SECRET_FALLBACK } : {}),
    audience: env('AUTH0_AUDIENCE'),
    redirectUri: `${baseUrl}/oauth/qurl/callback`,
    fetch: fetch as FetchLike,
  });
  const verifier = createIdTokenVerifier({ issuer: auth0Issuer, audience: env('AUTH0_CLIENT_ID'), fetch: fetch as FetchLike });
  const binder = new HttpProviderBinder({ endpoint: qurlEndpoint, data });
  const callback = new OAuthCallbackCore({ state: oauthState, tokenClient, idTokenVerifier: verifier, providerBinder: binder });
  const expressApp = express();
  const configuredServiceUrl = process.env.TEAMS_SERVICE_URL?.trim();
  const serviceUrl = configuredServiceUrl ? validateTeamsServiceUrl(configuredServiceUrl) : undefined;
  const app = new App({
    clientId: appId,
    clientSecret: appPassword,
    httpServerAdapter: new ExpressAdapter(expressApp),
    messagingEndpoint: '/api/messages',
    ...(serviceUrl === undefined ? {} : { serviceUrl }),
    // Keep mention normalization in the existing qURL Activity adapter.
    activity: { mentions: { stripText: false } },
  });
  const logger = new RedactingLogger({
    debug: (message, context) => runtimeConsole.debug(message, context),
    info: (message, context) => runtimeConsole.info(message, context),
    warn: (message, context) => runtimeConsole.warn(message, context),
    error: (message, context) => runtimeConsole.error(message, context),
  });
  const bot = new TeamsBot({
    qurlForTenant: new TenantQurlClientFactory(data, qurlEndpoint),
    data,
    messages: new TeamsSdkMessagePoster(app),
    connectorImage,
    setup: new TeamsSetupLinkBuilder({ state: oauthState, tokenClient, setupBaseUrl: baseUrl }),
    logger,
  });
  app.on('message', async ({ activity, reply }) => {
    const normalized = toTeamsActivity(activity);
    if (normalized) {
      const controller = new AbortController();
      const timeout = setTimeout(() => controller.abort(), ACTIVITY_TIMEOUT_MS);
      // The Teams SDK replies against its authenticated inbound conversation
      // reference. validateTeamsServiceUrl intentionally guards only qURL's
      // DM and other out-of-band Connector sends in TeamsSdkMessagePoster.
      try { await bot.handleActivity(normalized, controller.signal, async text => { await reply(text); }); }
      finally { clearTimeout(timeout); }
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
  return { server, app, port, host };
}

if (isMainModule(process.argv[1], import.meta.url)) {
  const runtime = await createProductionTeamsConfig();
  runtime.server.listen(runtime.port, runtime.host, () => { process.stdout.write(`Teams bot listening on ${runtime.host}:${runtime.port}\n`); });
}
