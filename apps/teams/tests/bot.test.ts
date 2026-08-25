import { describe, expect, it } from 'vitest';
import { deriveScope, normalizeActivityText, toTeamsActivity } from '../src/activity.js';
import { TeamsBot } from '../src/bot.js';
import { parseCommand, tokenize } from '../src/parser.js';
import type { QurlClient } from '../src/qurl-client.js';
import type { TeamsDataStore } from '../src/teams-data.js';
import { renderTunnelInstallMessage, validateTunnelImageRef, validateTunnelSlug } from '../src/tunnel.js';

describe('Teams bot primitives', () => {
  it('parses the bind-only setup command', () => {
    expect(parseCommand('qurl setup Admin@Example.com')).toMatchObject({ verb: 'setup', email: 'Admin@Example.com', setupMode: 'bind' });
    expect(() => parseCommand('setup user@example.com --rotate')).toThrow();
  });

  it('parses get flags and quoted values', () => {
    expect(parseCommand('get $docs dm:true reason:"private docs"')).toMatchObject({ verb: 'get', resource: 'docs', flags: { dm: 'true', reason: 'private docs' } });
  });

  it('removes quote delimiters without corrupting mid-token values', () => {
    expect(tokenize('get $docs reason:"private docs"')).toEqual(['get', '$docs', 'reason:private docs']);
    expect(tokenize('set-display-name $docs "Internal docs"')).toEqual(['set-display-name', '$docs', 'Internal docs']);
  });

  it('removes a bot mention and derives a channel tenant scope', () => {
    const activity = { type: 'message', text: '<at>qURL</at> list', recipient: { id: 'bot' }, entities: [{ type: 'mention', text: '<at>qURL</at>', mentioned: { id: 'bot' } }], channelData: { tenant: { id: 'Tenant' }, channel: { id: 'channel' } }, conversation: { id: 'conversation', conversationType: 'channel' } };
    expect(normalizeActivityText(activity)).toBe('list');
    expect(deriveScope(activity)).toEqual({ tenantId: 'tenant', scopeId: 'channel', channel: true });
  });

  it('normalizes repeated mentions using entity offsets', () => {
    expect(normalizeActivityText({
      text: '<at>qURL</at> list <at>qURL</at>',
      recipient: { id: 'bot' },
      entities: [
        { type: 'mention', text: '<at>qURL</at>', offset: 0, length: 13, mentioned: { id: 'bot' } },
        { type: 'mention', text: '<at>qURL</at>', offset: 19, length: 13, mentioned: { id: 'bot' } },
      ],
    })).toBe('list');
  });

  it('maps only supported inbound activity fields at the SDK boundary', () => {
    expect(toTeamsActivity({
      type: 'message',
      text: 'list',
      from: { id: 'actor', aadObjectId: 'aad' },
      conversation: { id: 'conversation', conversationType: 'channel' },
      unexpectedSecret: 'must not cross the boundary',
    })).toEqual({
      type: 'message',
      text: 'list',
      from: { id: 'actor', aadObjectId: 'aad' },
      conversation: { id: 'conversation', conversationType: 'channel' },
    });
  });

  it('does not treat group chats as channel policy scopes', () => {
    expect(deriveScope({ conversation: { id: 'group', conversationType: 'groupChat' }, channelData: { tenant: { id: 'Tenant' } } })).toEqual({
      tenantId: 'tenant', scopeId: 'personal', channel: false,
    });
  });

  it('quotes connector bootstrap secrets in the rendered command', () => {
    const message = renderTunnelInstallMessage({ slug: 'prod', alias: 'prod', environment: 'docker', port: 8080, image: 'registry.example/qurl:1', bootstrapKey: "key'with-space" });
    expect(message).toContain("'key'\"'\"'with-space'");
  });

  it('rejects unsafe connector image references and renders Compose configuration', () => {
    expect(() => validateTunnelImageRef('registry.example/qurl:1;rm -rf /')).toThrow('invalid connector image reference');
    expect(renderTunnelInstallMessage({ slug: 'prod', alias: 'prod', environment: 'compose', port: 8080, image: 'registry.example/qurl:1', bootstrapKey: 'bootstrap' })).toContain('QURL_BOOTSTRAP_KEY: "bootstrap"');
  });

  it('enforces the documented 3-64 character connector ID boundary', () => {
    expect(() => validateTunnelSlug('ab')).toThrow('3-64');
    expect(() => validateTunnelSlug('abc')).not.toThrow();
    expect(() => validateTunnelSlug(`a${'b'.repeat(62)}c`)).not.toThrow();
    expect(() => validateTunnelSlug(`a${'b'.repeat(63)}c`)).toThrow('3-64');
  });

  it('renders safe authorization errors to the Teams user', async () => {
    const replies: string[] = [];
    const bot = new TeamsBot({
      qurl: {} as QurlClient,
      data: { checkAdmin: async () => ({ isAdmin: false }) } as unknown as TeamsDataStore,
      messages: {} as never,
    });
    await bot.handleActivity({
      type: 'message', text: 'admins', from: { aadObjectId: 'actor', id: 'delivery' },
      channelData: { tenant: { id: 'tenant' }, channel: { id: 'channel' } },
      conversation: { id: 'conversation', conversationType: 'channel' },
    }, undefined, async text => { replies.push(text); });
    expect(replies).toEqual(['This command is limited to the tenant owner and qURL admins.']);
  });

  it('keeps unexpected failures generic', async () => {
    const replies: string[] = [];
    const bot = new TeamsBot({
      qurl: {} as QurlClient,
      data: { checkAdmin: async () => { throw new Error('upstream secret detail'); } } as unknown as TeamsDataStore,
      messages: {} as never,
    });
    await bot.handleActivity({
      type: 'message', text: 'list', from: { aadObjectId: 'actor', id: 'delivery' },
      channelData: { tenant: { id: 'tenant' }, channel: { id: 'channel' } },
      conversation: { id: 'conversation', conversationType: 'channel' },
    }, undefined, async text => { replies.push(text); });
    expect(replies).toEqual(['The qURL command could not be completed. Check the command syntax and try again.']);
  });

  it('renders ECS and Kubernetes connector instructions', () => {
    const base = { slug: 'prod', alias: 'prod', port: 8080, image: 'registry.example/qurl:1', bootstrapKey: 'key' };
    expect(renderTunnelInstallMessage({ ...base, environment: 'ecs-fargate' })).toContain('ECS/Fargate environment');
    expect(renderTunnelInstallMessage({ ...base, environment: 'kubernetes' })).toContain('kind: Deployment');
  });

  it('parses connector deployment options instead of silently dropping them', () => {
    expect(parseCommand('protect-connector prod env:compose port:9090 alias:$web')).toMatchObject({
      verb: 'protect-connector',
      resource: 'prod',
      flags: { env: 'compose', port: '9090', alias: 'web' },
    });
    expect(() => parseCommand('protect-connector prod port:0')).toThrow();
    expect(() => parseCommand('protect-connector prod env:unknown')).toThrow();
  });

  it('requires an alias when creating a URL resource', () => {
    expect(() => parseCommand('protect-url url:https://example.com')).toThrow();
    expect(parseCommand('protect-url url:https://example.com as:$docs')).toMatchObject({
      verb: 'protect-url', flags: { as: 'docs' },
    });
  });

  it('rejects unterminated quotes and invalid boolean get flags', () => {
    expect(() => parseCommand('get $docs reason:"missing')).toThrow();
    expect(() => parseCommand('get $docs dm:yes')).toThrow();
  });

  it('resolves a channel-local alias when minting a qURL', async () => {
    let createdResourceId = '';
    const qurl = {
      listResources: async () => ({ resources: [{ resourceId: 'resource-1' }] }),
      create: async (input: { readonly resourceId?: string }) => {
        createdResourceId = input.resourceId ?? '';
        return { resourceId: createdResourceId, qurlLink: 'https://qurl.example/one' };
      },
    } as unknown as QurlClient;
    const data = {
      checkAdmin: async () => ({ isAdmin: false }),
      allowedResourceIds: async () => new Set(['resource-1']),
      lookupScopeAlias: async () => 'resource-1',
      personalConversationRef: async () => undefined,
    } as unknown as TeamsDataStore;
    const bot = new TeamsBot({ qurl, data, messages: {} as never });

    await expect(bot.execute(
      { type: 'message', id: 'activity-1', from: { aadObjectId: 'actor-1', id: 'delivery-1' } },
      'tenant-1', 'channel-1', true, parseCommand('get $docs'),
    )).resolves.toContain('https://qurl.example/one');
    expect(createdResourceId).toBe('resource-1');
  });

  it('follows a next cursor even when has_more is omitted', async () => {
    const cursors: Array<string | undefined> = [];
    const bot = new TeamsBot({ qurl: {} as QurlClient, data: {} as TeamsDataStore, messages: {} as never });
    const qurl = {
      listResources: async (_signal?: AbortSignal, cursor?: string) => {
        cursors.push(cursor);
        return cursor === undefined
          ? { resources: [{ resourceId: 'resource-1' }], nextCursor: 'next' }
          : { resources: [{ resourceId: 'resource-2' }] };
      },
    } as unknown as QurlClient;
    await expect(bot.resources(qurl)).resolves.toEqual([{ resourceId: 'resource-1' }, { resourceId: 'resource-2' }]);
    expect(cursors).toEqual([undefined, 'next']);
  });

  it('uses distinct idempotency keys for connector resources and enrollment tokens', async () => {
    const keys: string[] = [];
    const bot = new TeamsBot({
      qurl: {
        listResources: async () => ({ resources: [] }),
        createResource: async (input: { readonly idempotencyKey?: string }) => { keys.push(input.idempotencyKey ?? ''); return { resourceId: 'connector-1', type: 'tunnel', slug: 'prod' }; },
        createEnrollmentToken: async (_slug: string, key: string) => { keys.push(key); return { keyId: 'key-1', apiKey: 'bootstrap' }; },
      } as unknown as QurlClient,
      data: {
        checkAdmin: async () => ({ isAdmin: true }),
        personalConversationRef: async () => ({ serviceUrl: 'https://smba.trafficmanager.net/teams', conversationId: 'conversation' }),
        lookupScopeAlias: async () => undefined,
        bindScopeAlias: async () => undefined,
        exposeResource: async () => undefined,
      } as unknown as TeamsDataStore,
      messages: { sendText: async () => undefined } as never,
      connectorImage: 'registry.example/qurl:1',
    });
    await expect(bot.execute(
      { type: 'message', id: 'activity-1', from: { id: 'delivery', aadObjectId: 'actor' } },
      'tenant-1', 'channel-1', true, parseCommand('protect-connector prod'),
    )).resolves.toContain('sent the bootstrap instructions');
    expect(keys).toHaveLength(2);
    expect(keys[0]).not.toBe(keys[1]);
  });

  it('continues commands when personal-conversation capture fails', async () => {
    const replies: string[] = [];
    const bot = new TeamsBot({
      qurl: {} as QurlClient,
      data: { savePersonalConversationRef: async () => { throw new Error('temporary DDB failure'); } } as unknown as TeamsDataStore,
      messages: {} as never,
    });
    await bot.handleActivity({ type: 'message', text: 'help', from: { aadObjectId: 'actor' }, serviceUrl: 'https://smba.trafficmanager.net', conversation: { id: 'personal', conversationType: 'personal' }, channelData: { tenant: { id: 'tenant' } } }, undefined, async text => { replies.push(text); });
    expect(replies[0]).toContain('qURL for Teams');
  });

  it('does not silently reassign an existing alias with set-alias', async () => {
    const bot = new TeamsBot({
      qurl: {
        listResources: async () => ({ resources: [{ resourceId: 'resource-1' }] }),
      } as unknown as QurlClient,
      data: {
        checkAdmin: async () => ({ isAdmin: true }),
        lookupScopeAlias: async () => 'other-resource',
      } as unknown as TeamsDataStore,
      messages: {} as never,
    });
    await expect(bot.execute(
      { type: 'message', from: { aadObjectId: 'admin' } },
      'tenant-1', 'channel-1', true, parseCommand('set-alias $docs $resource-1'),
    )).rejects.toThrow('Alias `$docs` is already in use in this channel.');
  });

  it('explains that unsetting an alias does not unprotect its resource', async () => {
    let unbound = '';
    const bot = new TeamsBot({
      qurl: { listResources: async () => ({ resources: [{ resourceId: 'resource-1' }] }) } as unknown as QurlClient,
      data: {
        checkAdmin: async () => ({ isAdmin: true }),
        unbindScopeAlias: async (_tenantId: string, _scopeId: string, alias: string) => { unbound = alias; },
      } as unknown as TeamsDataStore,
      messages: {} as never,
    });
    await expect(bot.execute(
      { type: 'message', from: { aadObjectId: 'admin' } },
      'tenant-1', 'channel-1', true, parseCommand('unset-alias $docs'),
    )).resolves.toContain('resource remains protected');
    expect(unbound).toBe('docs');
  });
});
