import { describe, expect, it } from 'vitest';
import { TeamsBot, helpMessage } from '../src/bot.js';
import { parseCommand } from '../src/parser.js';
import { HttpQurlClient } from '../src/qurl-client.js';
import { TeamsDataStore, type DynamoRequest } from '../src/teams-data.js';

// Canonical public-key/CRID pair verified with the service's CRID codec.
const CRID = 'aho4gla7hppidc664lhtpy3vq3fif5tn3m2esan3tbgtnhwafvqn6fblhemq';
const PUBLIC_KEY = 'MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEN4yvBX3yjAvYl9qagkStIWB1ie2gp_LF2Jy0w5AdxXefsTNLn9nrOlA4umKRiIQeGfvad9OFVoWa3PAIxcy4qg';
const activity = { type: 'message', id: 'activity-1', from: { aadObjectId: 'admin', id: 'delivery' } };

function fixture(options: { legacy?: boolean; backfill?: boolean; detailStatus?: number; failFirstPurge?: boolean } = {}) {
  const requests: Request[] = [];
  const aliases = new Map<string, string>([['docs', PUBLIC_KEY]]);
  const allowed = new Set([PUBLIC_KEY]);
  const bindings: unknown[][] = [];
  const purges: string[] = [];
  let revoked = false;
  const resource = () => ({
    resource_id: PUBLIC_KEY, ...(!options.legacy && !options.backfill ? { crid: CRID } : {}),
    type: 'url', description: 'Internal docs', status: revoked ? 'revoked' : 'active',
  });
  const qurl = new HttpQurlClient({
    endpoint: 'https://api.example.test', apiKey: 'synthetic-account-key',
    fetch: async (input, init) => {
      const request = new Request(input, init);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (request.method === 'GET' && path === '/v1/resources') {
        return Response.json({ data: [resource()], meta: { has_more: false } });
      }
      if (request.method === 'GET' && path === `/v1/resources/${CRID}`) {
        return options.detailStatus
          ? Response.json({ error: { code: 'resource_not_found' } }, { status: options.detailStatus })
          : Response.json({ data: { resource: resource(), qurls: [] } });
      }
      if (request.method === 'POST' && path.endsWith('/qurls')) {
        return Response.json({ data: { resource_id: PUBLIC_KEY, crid: CRID, qurl_link: 'https://qurl.example/one' } }, { status: 201 });
      }
      if (request.method === 'DELETE') { revoked = true; return new Response(null, { status: 204 }); }
      if (request.method === 'PATCH') return new Response(null, { status: 204 });
      throw new Error(`Unexpected request: ${request.method} ${path}`);
    },
  });
  const data = {
    checkAdmin: async () => ({ isAdmin: true }),
    lookupScopeAlias: async (_tenant: string, _scope: string, alias: string) => aliases.get(alias),
    allowedResourceIds: async () => allowed,
    bindScopeAlias: async (...args: [string, string, string, string, string?]) => {
      bindings.push(args); aliases.set(args[2], args[3]);
    },
    exposeResource: async (_tenant: string, _scope: string, key: string) => { allowed.add(key); },
    purgeResourceFromTenant: async (_tenant: string, key: string) => {
      purges.push(key); aliases.clear(); allowed.clear();
      if (options.failFirstPurge && purges.length === 1) throw new Error('other channel cleanup unavailable');
    },
  } as unknown as TeamsDataStore;
  const bot = new TeamsBot({ qurl, data, messages: {} as never, qurlEndpoint: 'https://api.example.test' });
  const execute = (command: string) => bot.execute(activity, 'tenant', 'channel', true, parseCommand(command));
  return { bot, execute, requests, aliases, allowed, bindings, purges };
}

describe('CRIDs at the Teams command boundary', () => {
  it('shows the CRID from the API and mints through the existing channel grant', async () => {
    const { execute, requests } = fixture();
    const list = await execute('list');
    expect(list).toContain(`$${CRID}`);
    expect(list).not.toContain(PUBLIC_KEY);
    requests.length = 0;
    await expect(execute(`get $${CRID}`)).resolves.toContain('https://qurl.example/one');
    expect(requests).toHaveLength(2);
    expect(requests[0]?.url).toBe(`https://api.example.test/v1/resources/${CRID}`);
    expect(requests.at(-1)?.url).toBe(`https://api.example.test/v1/resources/${PUBLIC_KEY}/qurls`);
  });

  it('does not turn a known CRID into access from a different channel', async () => {
    const { execute, allowed, requests } = fixture();
    allowed.clear();
    await expect(execute(`get $${CRID}`)).rejects.toThrow('not found');
    expect(requests.every(request => request.method === 'GET')).toBe(true);
  });

  it.each(['get', 'revoke'])('keeps a channel alias ahead of a CRID-shaped token for %s', async verb => {
    const { execute, aliases, allowed, requests } = fixture();
    aliases.set(CRID, 'alias-target'); allowed.add('alias-target');
    await execute(`${verb} $${CRID}`);
    const mutation = requests.find(request => request.method === (verb === 'get' ? 'POST' : 'DELETE'));
    expect(new URL(mutation!.url).pathname).toBe(`/v1/resources/alias-target${verb === 'get' ? '/qurls' : ''}`);
    expect(requests.some(request => request.method === 'GET')).toBe(false);
  });

  it.each([
    ['set-alias $reports', ''],
    ['set-display-name', ' Friendly name'],
    ['unset-display-name', ''],
    ['protect-url', ' as:$reports'],
  ])('resolves a CRID for %s while keeping stored keys stable', async (prefix, suffix) => {
    const { execute, requests, bindings } = fixture();
    const reply = await execute(`${prefix} $${CRID}${suffix}`);
    expect(reply).toContain(`$${CRID}`);
    expect(reply).not.toContain(PUBLIC_KEY);
    if (prefix.includes('display-name')) {
      const request = requests.at(-1)!;
      expect(request.method).toBe('PATCH');
      expect(new URL(request.url).pathname).toBe(`/v1/resources/${PUBLIC_KEY}`);
      expect(await request.json()).toEqual({ description: suffix ? 'Friendly name' : '' });
    } else {
      expect(bindings).toEqual([['tenant', 'channel', 'reports', PUBLIC_KEY, CRID]]);
    }
  });

  it('uses a returned CRID as a removable default alias for an existing URL', async () => {
    const { execute, bindings } = fixture();
    await execute(`protect-url $${CRID}`);
    expect(bindings).toEqual([['tenant', 'channel', CRID, PUBLIC_KEY, CRID]]);
    expect(parseCommand(`unset-alias $${CRID}`).resource).toBe(CRID);
  });

  it.each([false, true])('retries partial revoke cleanup by CRID after discovery and aliases lose the resource (backfill=%s)', async backfill => {
    const { execute, requests, purges } = fixture({ failFirstPurge: true, backfill });
    await expect(execute(`revoke $${CRID}`)).rejects.toThrow('other channel cleanup unavailable');
    await expect(execute('list')).resolves.toContain('No protected resources');
    const reply = await execute(`revoke $${CRID}`);
    expect(reply).toContain(`$${CRID}`);
    expect(reply).not.toContain('cleared');
    expect(purges).toEqual([PUBLIC_KEY, PUBLIC_KEY]);
    expect(requests.filter(request => request.method === 'DELETE').map(request => new URL(request.url).pathname))
      .toEqual([`/v1/resources/${PUBLIC_KEY}`, `/v1/resources/${PUBLIC_KEY}`]);
  });

  it.each([404, 410])('does not claim local cleanup when a CRID can no longer map to its key (%s)', async detailStatus => {
    const { execute, purges, requests } = fixture({ detailStatus });
    await expect(execute(`revoke $${CRID}`)).rejects.toThrow('channel alias');
    expect(purges).toEqual([]);
    expect(requests.every(request => request.method === 'GET')).toBe(true);
  });

  it.each([404, 410])('hides unavailable CRIDs without scanning the catalogue (%s)', async detailStatus => {
    const { execute, requests } = fixture({ detailStatus });
    await expect(execute(`get $${CRID}`)).rejects.toThrow('Resource not found.');
    expect(requests).toHaveLength(1);
  });

  it('does not mint a revoked CRID returned by detail', async () => {
    const { execute, allowed, requests } = fixture();
    await execute(`revoke $${CRID}`);
    allowed.add(PUBLIC_KEY);
    requests.length = 0;
    await expect(execute(`get $${CRID}`)).rejects.toThrow('Resource not found.');
    expect(requests).toHaveLength(1);
    expect(requests[0]?.method).toBe('GET');
  });

  it('keeps older resources without CRIDs usable by their public keys', async () => {
    const { execute } = fixture({ legacy: true });
    await expect(execute('list')).resolves.toContain(PUBLIC_KEY);
    await expect(execute(`get $${PUBLIC_KEY}`)).resolves.toContain('https://qurl.example/one');
  });

  it('describes the supported CRID command arguments', () => {
    const help = helpMessage();
    expect(help).toContain('$<crid|alias>');
    expect(help).not.toMatch(/resource[ -]+ids?/i);
    expect(help).toContain('User commands (channels only, except setup):');
  });

  it('keeps the CRID alongside an alias without changing authorization or index keys', async () => {
    const writes: DynamoRequest[] = [];
    const store = new TeamsDataStore({
      client: { async send<T>(request: DynamoRequest): Promise<T> {
        writes.push(request);
        return (request.operation === 'query' ? { Items: [writes[0]!.input.Item] } : {}) as T;
      } },
      tenantPrincipalsTable: 'principals', channelPoliciesTable: 'policies',
      personalConversationsTable: 'conversations', tenantCredentialsTable: 'credentials',
    });
    await store.bindScopeAlias('tenant', 'channel', 'docs', PUBLIC_KEY, CRID);
    expect(writes[0]?.input.Item).toMatchObject({
      resource_id: PUBLIC_KEY, crid: CRID, tenant_resource_key: `tenant#${PUBLIC_KEY}`,
    });
    expect(await store.allowedResourceIds('tenant', 'channel')).toEqual(new Set([PUBLIC_KEY]));
    const entries = await store.scopeAliases('tenant', 'channel');
    expect(entries).toEqual([{ scopeId: 'channel', alias: 'docs', resourceId: PUBLIC_KEY, crid: CRID }]);
    const bot = new TeamsBot({ data: store, messages: {} as never, qurlEndpoint: 'https://api.example.test' });
    await expect(bot.aliases('tenant', 'channel')).resolves.toContain(`$${CRID}`);
  });
});
