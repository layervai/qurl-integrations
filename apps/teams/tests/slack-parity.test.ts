import { describe, expect, it } from 'vitest';
import { TeamsBot } from '../src/bot.js';
import type { TeamsActivity } from '../src/activity.js';
import type { FetchLike, LogContext } from '../src/interfaces.js';
import { nullLogger } from '../src/interfaces.js';
import { RedactingLogger } from '../src/logger.js';
import { HttpQurlClient } from '../src/qurl-client.js';
import type { TeamsDataStore } from '../src/teams-data.js';

const endpoint = 'https://api.example.test';
const accountKey = 'synthetic-account-key';
const canonicalKey = 'synthetic-canonical-public-key';
const crid = 'synthetic-crid';
const link = 'https://qurl.example.test/one';
const personal = { serviceUrl: 'https://smba.trafficmanager.net/teams', conversationId: 'personal-actor' };
const image = `ghcr.io/layervai/qurl@sha256:${'a'.repeat(64)}`;
const urlResource = { resource_id: canonicalKey, crid, type: 'url', status: 'active', target_url: 'https://protected.example.test' };
const connectorResource = { resource_id: canonicalKey, crid, type: 'tunnel', status: 'active', slug: 'prod', connector_routing_id: 'synthetic-routing', knock_resource_id: 'synthetic-knock' };

function json(body: unknown, status = 200, headers?: HeadersInit): Response {
  return new Response(JSON.stringify(body), { status, ...(headers ? { headers } : {}) });
}

function activity(text: string, id = 'activity-one'): TeamsActivity {
  return {
    type: 'message', id, text, serviceUrl: personal.serviceUrl,
    from: { id: 'delivery-actor', aadObjectId: 'actor' },
    conversation: { id: 'thread', conversationType: 'channel', tenantId: 'tenant' },
    channelData: { tenant: { id: 'tenant' }, channel: { id: 'channel' } },
  };
}

function mintScenario(mint: FetchLike = async () => json({ data: { resource_id: canonicalKey, qurl_link: link } }, 201)) {
  const requests: Request[] = [];
  const replies: string[] = [];
  const dms: Array<{ serviceUrl: string; conversationId: string; text: string }> = [];
  const errors: Array<{ message: string; context?: LogContext }> = [];
  const qurl = new HttpQurlClient({ endpoint, apiKey: accountKey, userAgent: 'qurl-teams/1', fetch: async (input, init) => {
    const request = new Request(input, init);
    requests.push(request);
    if (request.method === 'POST' && new URL(request.url).pathname.endsWith('/qurls')) return mint(input, init);
    if (request.method === 'GET' && new URL(request.url).pathname === `/v1/resources/${canonicalKey}`) return json({ data: { resource: urlResource } });
    throw new Error(`Unexpected synthetic API request: ${request.method} ${new URL(request.url).pathname}`);
  } });
  const bot = new TeamsBot({
    qurl, qurlEndpoint: endpoint,
    data: {
      allowedResourceIds: async () => new Set([canonicalKey]),
      lookupScopeAlias: async (_tenant: string, _scope: string, alias: string) => alias === 'docs' ? canonicalKey : undefined,
      personalConversationRef: async () => personal,
    } as unknown as TeamsDataStore,
    messages: {
      reply: async (_activity, text) => { replies.push(text); },
      sendText: async (serviceUrl, conversationId, text) => { dms.push({ serviceUrl, conversationId, text }); },
    },
    logger: new RedactingLogger({ ...nullLogger, error: (message, context) => { errors.push({ message, ...(context ? { context } : {}) }); } }, [accountKey]),
  });
  return { bot, requests, replies, dms, errors };
}

// Only external HTTP, private delivery, and persisted channel policy are
// simulated. The command, HTTP client, request bodies, and compensation run.
function connectorScenario(options: {
  restartFailure?: number | 'network' | 'cancelled';
  reconciliation?: 'advanced' | 'unchanged' | 'off' | 'unavailable';
  failSecondDm?: boolean;
} = {}) {
  const requests: Request[] = [];
  const replies: string[] = [];
  const deliveredDms: string[] = [];
  const errors: Array<{ message: string; context?: LogContext }> = [];
  const aliases = new Map<string, string>();
  const allowed = new Set<string>();
  const controller = new AbortController();
  let resourceExists = options.restartFailure !== undefined;
  let sharingReads = 0;
  let enrollmentCount = 0;
  let dmCalls = 0;
  let sharing = { crid, desired_state: resourceExists ? 'on' : 'off', serving_epoch: 7 };
  const qurl = new HttpQurlClient({ endpoint, apiKey: accountKey, userAgent: 'qurl-teams/1', fetch: async (input, init) => {
    const request = new Request(input, init);
    requests.push(request);
    const path = new URL(request.url).pathname;
    if (request.method === 'GET' && path === '/v1/me') return json({ data: { owner_id: 'auth0|owner', auth_type: 'api_key', api_key: { key_id: 'account-key' } } });
    if (request.method === 'GET' && path === '/v1/resources') return json({ data: resourceExists ? [connectorResource] : [] });
    if (request.method === 'GET' && path === `/v1/resources/${canonicalKey}`) return json({ data: { resource: connectorResource } });
    if (request.method === 'POST' && path === '/v1/resources') { resourceExists = true; return json({ data: connectorResource }, 201); }
    if (request.method === 'GET' && path.endsWith('/sharing')) {
      sharingReads += 1;
      if (sharingReads > 1 && options.reconciliation === 'unavailable') return json({ error: { code: 'internal_error' } }, 503);
      return json({ data: sharing });
    }
    if (request.method === 'POST' && path.endsWith('/sharing/restart')) {
      if (options.restartFailure !== undefined) {
        if (options.reconciliation === 'advanced') sharing = { crid, desired_state: 'on', serving_epoch: 8 };
        if (options.reconciliation === 'off') sharing = { crid, desired_state: 'off', serving_epoch: 8 };
        if (options.restartFailure === 'cancelled') controller.abort();
        if (typeof options.restartFailure === 'string') throw new Error('synthetic uncertain restart');
        return json({ error: { code: 'restart_failed' } }, options.restartFailure);
      }
      sharing = { crid, desired_state: 'on', serving_epoch: sharing.serving_epoch + 1 };
      return json({ data: sharing });
    }
    if (request.method === 'PUT' && path.endsWith('/sharing')) { sharing = { ...sharing, desired_state: 'off' }; return json({ data: sharing }); }
    if (request.method === 'POST' && path === '/v1/api-keys') {
      enrollmentCount += 1;
      return json({ data: { key_id: `enrollment-${enrollmentCount}`, api_key: `synthetic-bootstrap-${enrollmentCount}`, kind: 'enrollment_token', target: 'agent', claims: [{ type: 'connector', id: 'prod' }] } }, 201);
    }
    if (request.method === 'DELETE' && path.startsWith('/v1/api-keys/')) return new Response(null, { status: 204 });
    throw new Error(`Unexpected synthetic API request: ${request.method} ${path}`);
  } });
  const bot = new TeamsBot({
    qurl, qurlEndpoint: endpoint, connectorImage: image,
    data: {
      checkAdmin: async () => ({ isAdmin: true, ownerId: 'actor', installationId: 'installation' }),
      personalConversationRef: async () => personal,
      lookupScopeAlias: async (_tenant: string, _scope: string, alias: string) => aliases.get(alias),
      bindScopeAlias: async (_tenant: string, _scope: string, alias: string, resourceId: string) => { aliases.set(alias, resourceId); },
      exposeResource: async (_tenant: string, _scope: string, resourceId: string) => { allowed.add(resourceId); },
      allowedResourceIds: async () => allowed,
    } as unknown as TeamsDataStore,
    messages: {
      reply: async (_activity, text) => { replies.push(text); },
      sendText: async (serviceUrl, conversationId, text) => {
        expect({ serviceUrl, conversationId }).toEqual(personal);
        dmCalls += 1;
        if (options.failSecondDm && dmCalls === 2) { controller.abort(); throw new Error('synthetic private delivery failure'); }
        deliveredDms.push(text);
      },
    },
    logger: new RedactingLogger({ ...nullLogger, error: (message, context) => { errors.push({ message, ...(context ? { context } : {}) }); } }, [accountKey, 'synthetic-bootstrap-1', 'synthetic-bootstrap-2']),
  });
  return { bot, requests, replies, deliveredDms, errors, controller };
}

describe('Slack behavior parity through the Teams bot and HTTP adapter', () => {
  it.each([false, true])('sends the complete resource mint policy and explains link lifetime (dm=%s)', async dm => {
    const test = mintScenario();
    const command = `get $docs reason:"incident review"${dm ? ' dm:true' : ''}`;
    await test.bot.handleActivity(activity(command));
    await test.bot.handleActivity(activity(command));
    await test.bot.handleActivity(activity(command, 'activity-two'));
    const minted = test.requests.filter(request => request.method === 'POST');
    expect(minted).toHaveLength(3);
    for (const request of minted) {
      expect(request.url).toBe(`${endpoint}/v1/resources/${canonicalKey}/qurls`);
      expect(request.headers.get('Authorization')).toBe(`Bearer ${accountKey}`);
      expect(request.headers.get('User-Agent')).toBe('qurl-teams/1');
      expect(request.headers.get('Content-Type')).toBe('application/json');
      expect(await request.json()).toEqual({ label: 'incident review', expires_in: '1m', one_time_use: true, max_sessions: 1, session_duration: '1h' });
    }
    expect(minted[0]?.headers.get('Idempotency-Key')).toMatch(/^[a-f0-9]{64}$/);
    expect(minted[1]?.headers.get('Idempotency-Key')).toBe(minted[0]?.headers.get('Idempotency-Key'));
    expect(minted[2]?.headers.get('Idempotency-Key')).not.toBe(minted[0]?.headers.get('Idempotency-Key'));
    const delivered = dm ? test.dms.map(message => message.text) : test.replies;
    expect(delivered).toHaveLength(3);
    for (const text of delivered) {
      expect(text).toContain(link);
      expect(text).toContain('one-time use');
      expect(text).toContain('1-minute lifetime');
    }
    if (dm) { expect(test.replies.join('\n')).not.toContain(link); expect(test.dms[0]).toMatchObject(personal); }
    expect(test.errors).toEqual([]);
  });

  it.each([
    { status: 403, code: 'connector_disabled', advice: /not (yet )?enabled|disabled/i },
    { status: 403, code: 'api_key_limit', advice: /limit|cannot create another/i },
    { status: 403, code: 'quota_exceeded', advice: /limit|cannot create another/i },
    { status: 429, code: 'rate_limited', advice: /30.*second|30s/i },
    { status: 500, code: 'internal_error', advice: /reach qURL|unavailable/i },
    { status: 503, code: 'internal_error', advice: /reach qURL|unavailable/i },
  ])('gives service-specific mint guidance without ERROR for $status/$code', async ({ status, code, advice }) => {
    const test = mintScenario(async () => json({ error: { code, detail: 'private upstream diagnostic' } }, status, { 'Retry-After': '30' }));
    await test.bot.handleActivity(activity('get $docs'));
    expect(test.replies).toHaveLength(1);
    expect(test.replies[0]).toMatch(advice);
    if (code === 'connector_disabled') {
      expect(test.replies[0]).toMatch(/environment/i);
      expect(test.replies[0]).toMatch(/operator|support/i);
      expect(test.replies[0]).not.toMatch(/enable sharing/i);
    }
    expect(test.replies[0]).not.toMatch(/syntax|private upstream/i);
    expect(test.errors).toEqual([]);
    expect(test.requests.filter(request => request.method === 'POST')).toHaveLength(1);
  });

  it('gives retry guidance for an actual HTTP transport failure without exposing its private detail', async () => {
    const test = mintScenario(async () => { throw new TypeError(`synthetic network failure for Bearer ${accountKey}`); });
    await test.bot.handleActivity(activity('get $docs'));
    expect(test.replies[0]).toMatch(/reach qURL|unavailable/i);
    expect(test.replies[0]).toMatch(/try again|retry/i);
    expect(test.replies[0]).not.toContain(accountKey);
    expect(test.errors).toEqual([]);
    expect(test.requests.filter(request => request.method === 'POST')).toHaveLength(1);
  });

  it.each([
    { status: 400, code: 'revoked' },
    { status: 401, code: 'insufficient_scope' },
    { status: 403, code: 'insufficient_scope' },
    { status: 404, code: 'resource_not_found' },
    { status: 410, code: 'resource_tombstoned' },
  ])('keeps $status/$code mint failures generic and visible to operators without blaming syntax', async ({ status, code }) => {
    const test = mintScenario(async () => json({ error: { code, detail: 'private upstream diagnostic' } }, status));
    await test.bot.handleActivity(activity('get $docs'));
    expect(test.errors).toHaveLength(1);
    expect(test.errors[0]?.context?.error).toMatchObject({ name: 'QurlHttpError', message: `qURL request failed (${status})` });
    expect(test.replies).toEqual(['The qURL command could not be completed. Please try again or contact your qURL operator.']);
    expect(test.requests).toHaveLength(1);
    expect(test.requests[0]?.method).toBe('POST');
    expect(test.dms).toEqual([]);
  });

  it('revokes after the second private send fails and a fresh attempt delivers a fresh enrollment token', async () => {
    const test = connectorScenario({ failSecondDm: true });
    await test.bot.handleActivity(activity('protect-connector prod'), test.controller.signal);
    const cleanup = test.requests.filter(request => request.method === 'DELETE' || request.method === 'PUT');
    expect(cleanup.map(request => `${request.method} ${new URL(request.url).pathname}`)).toEqual([
      'DELETE /v1/api-keys/enrollment-1', `PUT /v1/resources/${canonicalKey}/sharing`,
    ]);
    expect(cleanup.every(request => !request.signal.aborted)).toBe(true);
    expect(test.deliveredDms).toHaveLength(1);
    expect(test.deliveredDms[0]).toContain('install instructions');
    expect(test.deliveredDms[0]).not.toContain('synthetic-bootstrap-1');
    await test.bot.handleActivity(activity('protect-connector prod', 'fresh-attempt'));
    const minted = test.requests.filter(request => request.method === 'POST' && new URL(request.url).pathname === '/v1/api-keys');
    expect(minted).toHaveLength(2);
    expect(minted[0]?.headers.get('Idempotency-Key')).toMatch(/^[a-f0-9]{64}$/);
    expect(minted[1]?.headers.get('Idempotency-Key')).not.toBe(minted[0]?.headers.get('Idempotency-Key'));
    expect(test.requests.filter(request => request.method === 'POST' && new URL(request.url).pathname === '/v1/resources')).toHaveLength(1);
    expect(test.deliveredDms).toHaveLength(3);
    expect(test.deliveredDms[2]).toContain('synthetic-bootstrap-2');
    expect(test.deliveredDms.join('\n')).not.toContain('synthetic-bootstrap-1');
    expect(test.replies.join('\n')).not.toContain('synthetic-bootstrap');
    expect(JSON.stringify(test.errors)).not.toContain('synthetic-bootstrap');
  });

  it.each(['network', 429, 503] as const)('adopts an advanced on epoch after an ambiguous %s restart without replaying POST', async restartFailure => {
    const test = connectorScenario({ restartFailure, reconciliation: 'advanced' });
    await test.bot.handleActivity(activity('protect-connector prod'), test.controller.signal);
    expect(test.requests.filter(request => request.method === 'POST' && new URL(request.url).pathname.endsWith('/sharing/restart'))).toHaveLength(1);
    expect(test.requests.filter(request => request.method === 'GET' && new URL(request.url).pathname.endsWith('/sharing'))).toHaveLength(2);
    expect(test.requests.filter(request => request.method === 'POST' && new URL(request.url).pathname === '/v1/api-keys')).toHaveLength(1);
    expect(test.deliveredDms).toHaveLength(2);
    expect(test.deliveredDms[0]).toContain('serving_epoch: 8');
    expect(test.errors).toEqual([]);
  });

  it.each(['unchanged', 'off', 'unavailable'] as const)('does not mint when restart readback is %s', async reconciliation => {
    const test = connectorScenario({ restartFailure: 503, reconciliation });
    await test.bot.handleActivity(activity('protect-connector prod'), test.controller.signal);
    expect(test.requests.filter(request => request.method === 'POST' && new URL(request.url).pathname.endsWith('/sharing/restart'))).toHaveLength(1);
    expect(test.requests.filter(request => request.method === 'GET' && new URL(request.url).pathname.endsWith('/sharing'))).toHaveLength(2);
    expect(test.requests.filter(request => request.method === 'POST' && new URL(request.url).pathname === '/v1/api-keys')).toHaveLength(0);
    expect(test.deliveredDms).toEqual([]);
  });

  it.each([403, 'cancelled'] as const)('does not reconcile a deterministic or cancelled %s restart', async restartFailure => {
    const test = connectorScenario({ restartFailure, reconciliation: 'advanced' });
    await test.bot.handleActivity(activity('protect-connector prod'), test.controller.signal);
    expect(test.requests.filter(request => request.method === 'POST' && new URL(request.url).pathname.endsWith('/sharing/restart'))).toHaveLength(1);
    expect(test.requests.filter(request => request.method === 'GET' && new URL(request.url).pathname.endsWith('/sharing'))).toHaveLength(1);
    expect(test.requests.filter(request => request.method === 'POST' && new URL(request.url).pathname === '/v1/api-keys')).toHaveLength(0);
    expect(test.deliveredDms).toEqual([]);
  });
});
