import { describe, expect, it } from 'vitest';
import { deriveScope, normalizeActivityText, toTeamsActivity } from '../src/activity.js';
import { TeamsBot } from '../src/bot.js';
import { parseCommand, tokenize } from '../src/parser.js';
import type { QurlClient } from '../src/qurl-client.js';
import type { TeamsDataStore } from '../src/teams-data.js';
import { TenantOwnerAlreadyAdminError, TenantOwnerRemovalError } from '../src/teams-data.js';
import { renderTunnelInstallMessage, validateTunnelImageRef, validateTunnelSlug } from '../src/tunnel.js';

describe('Teams bot primitives', () => {
  it('parses the bind-only setup command', () => {
    expect(parseCommand('qurl setup Admin@Example.com')).toMatchObject({ verb: 'setup', email: 'Admin@Example.com', setupMode: 'bind' });
    expect(() => parseCommand('setup user@example.com --rotate')).toThrow();
  });

  it('parses get flags and quoted values', () => {
    expect(parseCommand('get $docs dm:true reason:"private docs"')).toMatchObject({ verb: 'get', resource: 'docs', flags: { dm: 'true', reason: 'private docs' } });
    expect(() => parseCommand('unset-alias $Docs')).toThrow('invalid alias');
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

  it('resolves malformed mention metadata without overlapping valid mentions', () => {
    expect(normalizeActivityText({
      text: '<at>qURL</at> list <at>other</at>',
      recipient: { id: 'bot' },
      entities: [
        { type: 'mention', text: '<at>other</at>', mentioned: { id: 'other' } },
        { type: 'mention', text: '<at>qURL</at>', offset: 0, length: 13, mentioned: { id: 'bot' } },
      ],
    })).toBe('list <@other>');
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

  it('rejects contradictory tenant identities before deriving a persistence scope', () => {
    expect(() => deriveScope({
      channelData: { tenant: { id: 'tenant-a' } },
      conversation: { tenantId: 'tenant-b' },
    })).toThrow('tenant identities do not match');
  });

  it('explains that resource commands are unavailable in group chats', async () => {
    const replies: string[] = [];
    const bot = new TeamsBot({
      qurl: {} as QurlClient,
      data: { checkAdmin: async () => ({ isAdmin: false }) } as unknown as TeamsDataStore,
      messages: {} as never,
    });
    await bot.handleActivity({
      type: 'message', text: 'list', from: { aadObjectId: 'actor' },
      channelData: { tenant: { id: 'tenant' } },
      conversation: { id: 'group', conversationType: 'groupChat' },
    }, undefined, async text => { replies.push(text); });
    expect(replies).toEqual(['This command is available only in Teams channels, not direct or group chats.']);
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

  it('rejects every mutating admin command for non-admins', async () => {
    const commands = [
      'protect-url url:https://example.com as:$docs',
      'protect-connector prod',
      'revoke $resource-1',
      'set-alias $docs $resource-1',
      'unset-alias $docs',
      'set-display-name $resource-1 Friendly name',
      'unset-display-name $resource-1',
      'add <@victim>',
      'remove <@victim>',
      'uninstall',
    ];
    const bot = new TeamsBot({
      qurl: {} as QurlClient,
      data: { checkAdmin: async () => ({ isAdmin: false }) } as unknown as TeamsDataStore,
      messages: {} as never,
    });

    for (const input of commands) {
      await expect(bot.execute(
        { type: 'message', from: { aadObjectId: 'actor' } },
        'tenant-1', 'channel-1', true, parseCommand(input),
      ), input).rejects.toThrow('limited to the tenant owner');
    }
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

  it('refuses to bind a channel alias the alias commands could never parse back', async () => {
    // protect-url without as: falls back to qURL-side identifiers, which are
    // under no channel-alias constraint. Binding one strands it: set-alias and
    // unset-alias both reject it, so only revoking the resource clears it.
    const bound: string[] = [];
    const data = {
      checkAdmin: async () => ({ isAdmin: true }),
      lookupScopeAlias: async () => undefined,
      bindScopeAlias: async (_tenantId: string, _scopeId: string, alias: string) => { bound.push(alias); },
      exposeResource: async () => undefined,
    } as unknown as TeamsDataStore;
    const activity = { type: 'message', id: 'activity-1', from: { aadObjectId: 'actor-1', id: 'delivery-1' } };

    for (const resource of [
      { resourceId: '7f3a-report-svc', type: 'url' },
      { resourceId: 'RES_01', type: 'url', slug: 'Payroll_API' },
      { resourceId: 'trailing-', type: 'url', alias: 'UPPER' },
    ]) {
      const bot = new TeamsBot({
        qurl: { listResources: async () => ({ resources: [resource] }) } as unknown as QurlClient,
        data,
        messages: {} as never,
      });
      await expect(bot.execute(activity, 'tenant-1', 'channel-1', true,
        parseCommand(`protect-url $${resource.resourceId}`)), resource.resourceId)
        .rejects.toThrow('no channel-safe alias');
    }
    expect(bound).toEqual([]);
  });

  it('falls back to the first upstream identifier that is a usable channel alias', async () => {
    const bound: string[] = [];
    const bot = new TeamsBot({
      qurl: { listResources: async () => ({ resources: [{ resourceId: 'RES_01', type: 'url', slug: 'payroll-api' }] }) } as unknown as QurlClient,
      data: {
        checkAdmin: async () => ({ isAdmin: true }),
        lookupScopeAlias: async () => undefined,
        bindScopeAlias: async (_tenantId: string, _scopeId: string, alias: string) => { bound.push(alias); },
        exposeResource: async () => undefined,
      } as unknown as TeamsDataStore,
      messages: {} as never,
    });
    await bot.execute({ type: 'message', id: 'activity-1', from: { aadObjectId: 'actor-1', id: 'delivery-1' } },
      'tenant-1', 'channel-1', true, parseCommand('protect-url $RES_01'));
    // The unusable resourceId is skipped for the slug, and the bound alias
    // round-trips through the alias commands.
    expect(bound).toEqual(['payroll-api']);
    expect(parseCommand('unset-alias $payroll-api')).toMatchObject({ verb: 'unset-alias', resource: 'payroll-api' });
  });

  it('answers unset-alias from channel policy alone, without any qURL call', async () => {
    let qurlCalls = 0;
    let tenantClientBuilds = 0;
    const bot = new TeamsBot({
      qurlForTenant: {
        forTenant: async () => {
          tenantClientBuilds += 1;
          return { listResources: async () => { qurlCalls += 1; return { resources: [] }; } } as unknown as QurlClient;
        },
      },
      data: {
        checkAdmin: async () => ({ isAdmin: true }),
        unbindScopeAlias: async () => undefined,
      } as unknown as TeamsDataStore,
      messages: {} as never,
    });
    await expect(bot.execute({ type: 'message', id: 'activity-1', from: { aadObjectId: 'actor-1', id: 'delivery-1' } },
      'tenant-1', 'channel-1', true, parseCommand('unset-alias $docs')))
      .resolves.toContain('Removed alias `$docs`');
    // Building the tenant client costs a DynamoDB read plus a KMS decrypt, and
    // resources() pages the whole catalogue. unset-alias needs neither.
    expect(tenantClientBuilds).toBe(0);
    expect(qurlCalls).toBe(0);
  });

  it('renders ECS and Kubernetes connector instructions', () => {
    const base = { slug: 'prod', alias: 'prod', port: 8080, image: 'registry.example/qurl:1', bootstrapKey: 'key' };
    expect(renderTunnelInstallMessage({ ...base, environment: 'ecs-fargate' })).toContain('ECS/Fargate task-definition fields');
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
    expect(() => parseCommand(`feedback ${'x'.repeat(2_001)}`)).toThrow('feedback message is too long');
    expect(() => parseCommand('add not-a-mention')).toThrow('Teams user mention');
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

  it('uses distinct idempotency keys when unexpected activities lack IDs', async () => {
    const keys: string[] = [];
    const bot = new TeamsBot({
      qurl: {
        listResources: async () => ({ resources: [{ resourceId: 'resource-1' }] }),
        create: async (input: { readonly idempotencyKey?: string }) => {
          keys.push(input.idempotencyKey ?? '');
          return { resourceId: 'resource-1', qurlLink: 'https://qurl.example/one' };
        },
      } as unknown as QurlClient,
      data: {
        allowedResourceIds: async () => new Set(['resource-1']),
        lookupScopeAlias: async () => undefined,
        personalConversationRef: async () => undefined,
      } as unknown as TeamsDataStore,
      messages: {} as never,
    });
    const activity = { type: 'message', from: { id: 'delivery', aadObjectId: 'actor' } };
    await bot.execute(activity, 'tenant-1', 'channel-1', true, parseCommand('get $resource-1'));
    await bot.execute(activity, 'tenant-1', 'channel-1', true, parseCommand('get $resource-1'));
    expect(keys).toHaveLength(2);
    expect(keys[0]).not.toBe(keys[1]);
  });

  it('returns clear owner-management errors to administrators', async () => {
    const base = { type: 'message' as const, from: { aadObjectId: 'admin' }, channelData: { tenant: { id: 'tenant' }, channel: { id: 'channel' } }, conversation: { id: 'conversation', conversationType: 'channel' } };
    const ownerBot = new TeamsBot({
      qurl: {} as QurlClient,
      data: { checkAdmin: async () => ({ isAdmin: true }), addAdmin: async () => { throw new TenantOwnerAlreadyAdminError(); } } as unknown as TeamsDataStore,
      messages: {} as never,
    });
    const removalBot = new TeamsBot({
      qurl: {} as QurlClient,
      data: { checkAdmin: async () => ({ isAdmin: true }), removeAdmin: async () => { throw new TenantOwnerRemovalError(); } } as unknown as TeamsDataStore,
      messages: {} as never,
    });
    const mention = [{ type: 'mention', mentioned: { id: 'owner', aadObjectId: 'owner-aad' } }];
    const replies: string[] = [];
    await ownerBot.handleActivity({ ...base, text: 'add <@owner>', entities: mention }, undefined, async text => { replies.push(text); });
    await removalBot.handleActivity({ ...base, text: 'remove <@owner>', entities: mention }, undefined, async text => { replies.push(text); });
    expect(replies).toEqual([
      'The tenant owner already has qURL admin access.',
      'The tenant owner cannot be removed.',
    ]);
  });

  it('filters list and get results to resources exposed in the channel', async () => {
    const resources = [
      { resourceId: 'visible', description: 'Visible resource' },
      { resourceId: 'hidden', description: 'Hidden resource' },
    ];
    const bot = new TeamsBot({
      qurl: { listResources: async () => ({ resources }) } as unknown as QurlClient,
      data: {
        checkAdmin: async () => { throw new Error('checkAdmin must not run for read commands'); },
        allowedResourceIds: async () => new Set(['visible']),
        lookupScopeAlias: async () => undefined,
      } as unknown as TeamsDataStore,
      messages: {} as never,
    });
    const activity = { type: 'message', from: { aadObjectId: 'actor' } };

    await expect(bot.execute(activity, 'tenant-1', 'channel-1', true, parseCommand('list'))).resolves.toMatch(/Visible resource/);
    await expect(bot.execute(activity, 'tenant-1', 'channel-1', true, parseCommand('get $hidden'))).rejects.toThrow('Resource not found');
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

  it('rejects a resource pagination cursor cycle', async () => {
    const bot = new TeamsBot({ qurl: {} as QurlClient, data: {} as TeamsDataStore, messages: {} as never });
    const qurl = { listResources: async () => ({ resources: [], nextCursor: 'loop' }) } as unknown as QurlClient;
    await expect(bot.resources(qurl)).rejects.toThrow('pagination is invalid');
  });

  it('rejects has_more without a continuation cursor', async () => {
    const bot = new TeamsBot({ qurl: {} as QurlClient, data: {} as TeamsDataStore, messages: {} as never });
    const qurl = { listResources: async () => ({ resources: [], hasMore: true }) } as unknown as QurlClient;
    await expect(bot.resources(qurl)).rejects.toThrow('pagination is invalid');
  });

  it('enforces the resource pagination safety cap', async () => {
    let calls = 0;
    const bot = new TeamsBot({ qurl: {} as QurlClient, data: {} as TeamsDataStore, messages: {} as never });
    const qurl = {
      listResources: async (_signal?: AbortSignal, cursor?: string) => {
        calls += 1;
        return { resources: [], nextCursor: String(Number(cursor ?? '0') + 1) };
      },
    } as unknown as QurlClient;
    await expect(bot.resources(qurl)).rejects.toThrow('exceeded the safety limit');
    expect(calls).toBe(1_000);
  });

  it('delivers dm:true qURLs to the actor personal conversation', async () => {
    let sent: { readonly serviceUrl: string; readonly conversationId: string; readonly text: string } | undefined;
    const bot = new TeamsBot({
      qurl: {
        listResources: async () => ({ resources: [{ resourceId: 'resource-1' }] }),
        create: async () => ({ resourceId: 'resource-1', qurlLink: 'https://qurl.example/one' }),
      } as unknown as QurlClient,
      data: {
        checkAdmin: async () => ({ isAdmin: false }),
        allowedResourceIds: async () => new Set(['resource-1']),
        lookupScopeAlias: async () => undefined,
        personalConversationRef: async () => ({ serviceUrl: 'https://smba.trafficmanager.net', conversationId: 'personal-conversation' }),
      } as unknown as TeamsDataStore,
      messages: {
        sendText: async (serviceUrl: string, conversationId: string, text: string) => { sent = { serviceUrl, conversationId, text }; },
      } as never,
    });

    await expect(bot.execute(
      { type: 'message', id: 'activity-1', from: { id: 'delivery', aadObjectId: 'actor' } },
      'tenant-1', 'channel-1', true, parseCommand('get $resource-1 dm:true'),
    )).resolves.toBe('Sent the one-time qURL to your personal Teams chat.');
    expect(sent).toEqual({ serviceUrl: 'https://smba.trafficmanager.net', conversationId: 'personal-conversation', text: 'qURL for `$resource-1`: https://qurl.example/one' });
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

  it('revokes a connector enrollment key when bootstrap delivery fails', async () => {
    const revoked: string[] = [];
    const bot = new TeamsBot({
      qurl: {
        listResources: async () => ({ resources: [] }),
        createResource: async () => ({ resourceId: 'connector-1', type: 'tunnel', slug: 'prod' }),
        createEnrollmentToken: async () => ({ keyId: 'key-1', apiKey: 'bootstrap-secret' }),
        revokeApiKey: async (keyId: string) => { revoked.push(keyId); },
      } as unknown as QurlClient,
      data: {
        checkAdmin: async () => ({ isAdmin: true }),
        personalConversationRef: async () => ({ serviceUrl: 'https://smba.trafficmanager.net/teams', conversationId: 'conversation' }),
        lookupScopeAlias: async () => undefined,
        bindScopeAlias: async () => undefined,
        exposeResource: async () => undefined,
      } as unknown as TeamsDataStore,
      messages: { sendText: async () => { throw new Error('delivery failed'); } } as never,
      connectorImage: 'registry.example/qurl:1',
    });

    await expect(bot.execute(
      { type: 'message', id: 'activity-1', from: { id: 'delivery', aadObjectId: 'actor' } },
      'tenant-1', 'channel-1', true, parseCommand('protect-connector prod'),
    )).rejects.toThrow('delivery failed');
    expect(revoked).toEqual(['key-1']);
  });

  it('revokes a connector enrollment key when install rendering fails', async () => {
    const revoked: string[] = [];
    const bot = new TeamsBot({
      qurl: {
        listResources: async () => ({ resources: [] }),
        createResource: async () => ({ resourceId: 'connector-1', type: 'tunnel', slug: 'prod' }),
        createEnrollmentToken: async () => ({ keyId: 'key-1', apiKey: 'bootstrap-secret' }),
        revokeApiKey: async (keyId: string) => { revoked.push(keyId); },
      } as unknown as QurlClient,
      data: {
        checkAdmin: async () => ({ isAdmin: true }),
        personalConversationRef: async () => ({ serviceUrl: 'https://smba.trafficmanager.net/teams', conversationId: 'conversation' }),
        lookupScopeAlias: async () => undefined,
        bindScopeAlias: async () => undefined,
        exposeResource: async () => undefined,
      } as unknown as TeamsDataStore,
      messages: { sendText: async () => { throw new Error('delivery should not run'); } } as never,
      connectorImage: 'invalid image',
    });

    await expect(bot.execute(
      { type: 'message', id: 'activity-1', from: { id: 'delivery', aadObjectId: 'actor' } },
      'tenant-1', 'channel-1', true, parseCommand('protect-connector prod'),
    )).rejects.toThrow('invalid connector image reference');
    expect(revoked).toEqual(['key-1']);
  });

  it('does not fetch the resource catalog twice for protect-connector', async () => {
    let listCalls = 0;
    const bot = new TeamsBot({
      qurl: {
        listResources: async () => { listCalls += 1; return { resources: [{ resourceId: 'connector-1', type: 'tunnel', slug: 'prod' }] }; },
        createEnrollmentToken: async () => ({ keyId: 'key-1', apiKey: 'bootstrap' }),
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
    await bot.execute({ type: 'message', id: 'activity-1', from: { id: 'delivery', aadObjectId: 'actor' } }, 'tenant-1', 'channel-1', true, parseCommand('protect-connector prod'));
    expect(listCalls).toBe(1);
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

  it('contains final delivery failures instead of rejecting the activity handler', async () => {
    const errors: string[] = [];
    const bot = new TeamsBot({
      qurl: {} as QurlClient,
      data: {} as TeamsDataStore,
      messages: { reply: async () => { throw new TypeError('Invalid URL'); } } as never,
      logger: { debug: () => undefined, info: () => undefined, warn: () => undefined, error: message => { errors.push(message); } },
    });
    await expect(bot.handleActivity({
      type: 'message', text: 'help', from: { aadObjectId: 'actor' },
    })).resolves.toBeUndefined();
    expect(errors).toEqual(['Teams message delivery failed']);
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

  it('always removes local data when uninstalling a damaged or unavailable binding', async () => {
    let deleted = false;
    const bot = new TeamsBot({
      qurl: { revokeApiKey: async () => { throw new Error('qURL unavailable'); } } as unknown as QurlClient,
      data: {
        checkAdmin: async () => ({ isAdmin: true }),
        tenantCredential: async () => ({ apiKey: 'secret' }),
        deleteWorkspace: async () => { deleted = true; },
      } as unknown as TeamsDataStore,
      messages: {} as never,
    });
    await expect(bot.execute({ type: 'message', from: { aadObjectId: 'admin' } }, 'tenant-1', 'personal', false, parseCommand('uninstall'))).resolves.toContain('operator follow-up');
    expect(deleted).toBe(true);

    deleted = false;
    const unavailableBot = new TeamsBot({
      qurl: { revokeApiKey: async () => { throw new Error('qURL unavailable'); } } as unknown as QurlClient,
      data: {
        checkAdmin: async () => ({ isAdmin: true }),
        tenantCredential: async () => ({ apiKey: 'secret', keyId: 'key-1' }),
        deleteWorkspace: async () => { deleted = true; },
      } as unknown as TeamsDataStore,
      messages: {} as never,
    });
    await expect(unavailableBot.execute({ type: 'message', from: { aadObjectId: 'admin' } }, 'tenant-1', 'personal', false, parseCommand('uninstall'))).resolves.toContain('operator follow-up');
    expect(deleted).toBe(true);
  });
});
