import { randomUUID } from 'node:crypto';
import { isChannelAlias } from './alias.js';
import type { TeamsActivity } from './activity.js';
import { deriveScope, normalizeActivityText } from './activity.js';
import type { TeamsMessagePoster } from './connector.js';
import { idempotencyKey } from './qurl-client.js';
import type { QurlApiKey, QurlClient, QurlResource } from './qurl-client.js';
import { parseCommand } from './parser.js';
import type { TeamsCommand } from './parser.js';
import { ScopeAliasConflictError, TenantOwnerAlreadyAdminError, TenantOwnerRemovalError, type TeamsDataStore } from './teams-data.js';
import type { TeamsSetupLinkBuilder } from './setup-link.js';
import { normalizeTunnelEnvironment, renderTunnelInstallMessage, validateTunnelSlug } from './tunnel.js';
import { isUserFacingError, UserFacingError } from './user-facing-error.js';
import type { Logger } from './interfaces.js';

const ADMIN_COMMANDS = new Set([
  'admins', 'add', 'remove', 'uninstall', 'protect-url', 'protect-connector',
  'set-alias', 'unset-alias', 'set-display-name', 'unset-display-name', 'revoke',
]);

export interface TeamsBotOptions {
  readonly qurl?: QurlClient;
  readonly qurlForTenant?: QurlClientFactory;
  readonly data: TeamsDataStore;
  readonly messages: TeamsMessagePoster;
  readonly setup?: TeamsSetupLinkBuilder;
  readonly connectorImage?: string;
  readonly logger?: Logger;
  readonly feedback?: (input: { readonly tenantId: string; readonly actorId: string; readonly message: string }) => Promise<void>;
}

export interface QurlClientFactory {
  forTenant(tenantId: string): Promise<QurlClient>;
}

export class TeamsBot {
  readonly #options: TeamsBotOptions;
  constructor(options: TeamsBotOptions) { this.#options = options; }

  async #qurl(tenantId: string): Promise<QurlClient> {
    if (this.#options.qurlForTenant) return this.#options.qurlForTenant.forTenant(tenantId);
    if (this.#options.qurl) return this.#options.qurl;
    throw new Error('qURL client is not configured');
  }

  async #bindAlias(tenantId: string, scopeId: string, alias: string, resourceId: string): Promise<void> {
    const existing = await this.#options.data.lookupScopeAlias(tenantId, scopeId, alias);
    if (existing !== undefined && existing !== resourceId) {
      throw new UserFacingError(`Alias \`$${alias}\` is already in use in this channel.`);
    }
    try {
      await this.#options.data.bindScopeAlias(tenantId, scopeId, alias, resourceId);
    } catch (error) {
      if (error instanceof ScopeAliasConflictError) {
        throw new UserFacingError(`Alias \`$${alias}\` is already in use in this channel.`);
      }
      throw error;
    }
  }

  #activityIdempotencyField(activity: TeamsActivity): string {
    const activityId = activity.id?.trim();
    // Bot Framework activities normally have an id. If an unexpected SDK
    // payload lacks one, avoid coalescing distinct user actions by structure.
    return activityId || `missing-activity-id:${randomUUID()}`;
  }

  async handleActivity(activity: TeamsActivity, signal?: AbortSignal, reply?: (text: string) => Promise<void>): Promise<void> {
    if ((activity.type ?? '').toLowerCase() !== 'message') return;
    // ConversationUpdate can be emitted before the user's identity is
    // available (and some clients do not emit it for an existing chat). Keep
    // the DM reference fresh from authenticated personal messages as well.
    try { await this.captureConversation(activity); }
    catch (error) { this.#options.logger?.warn('Teams conversation capture failed', { error }); }
    let response: string;
    try {
      const scope = deriveScope(activity);
      const command = parseCommand(normalizeActivityText(activity));
      response = await this.execute(activity, scope.tenantId, scope.scopeId, scope.channel, command, signal);
    } catch (error) {
      if (isUserFacingError(error)) response = error.message;
      else {
        this.#options.logger?.error('Teams command failed', { error });
        response = 'The qURL command could not be completed. Check the command syntax and try again.';
      }
    }
    if (response) {
      try {
        if (reply) await reply(response);
        else await this.#options.messages.reply(activity, response, signal);
      } catch (error) {
        // Delivery is outside command execution: a malformed activity (for
        // example, one without serviceUrl or conversation.id) must not turn
        // the SDK message handler into an unhandled rejection.
        this.#options.logger?.error('Teams message delivery failed', { error });
      }
    }
  }

  async execute(activity: TeamsActivity, tenantId: string, scopeId: string, channel: boolean, command: TeamsCommand, signal?: AbortSignal): Promise<string> {
    if (command.verb === 'help') return helpMessage();
    // Durable principal and personal-conversation rows are keyed by the
    // stable Entra object id. The delivery id remains available for Teams
    // replies and idempotency keys, but must not be used as the DDB identity.
    const actorId = (activity.from?.aadObjectId?.trim() ?? '').toLowerCase();
    if (!tenantId) throw new Error('Teams tenant id is required');
    if (!actorId) throw new Error('Teams actor AAD object id is required');
    if (command.verb === 'setup') {
      if (!this.#options.setup || !command.email) throw new Error('Teams OAuth setup is not configured');
      const deliveryId = activity.from?.id?.trim() ?? '';
      if (!deliveryId) throw new Error('Teams actor delivery id is required');
      const link = await this.#options.setup.build(tenantId, actorId, deliveryId, command.email, command.setupMode ?? 'bind');
      return `Open this qURL setup link in your browser:\n${link.url.toString()}`;
    }
    if (command.verb === 'feedback') {
      // The production runtime does not wire a feedback handler, so the verb is
      // no longer advertised in help. Someone who types it anyway gets a plain
      // answer rather than a generic failure plus an error-level log entry.
      if (!this.#options.feedback) throw new UserFacingError('Feedback is not enabled for this qURL installation.');
      await this.#options.feedback({ tenantId, actorId, message: command.text ?? '' }); return 'Thanks. The qURL team received your feedback.';
    }
    if (ADMIN_COMMANDS.has(command.verb)) {
      const admin = await this.#options.data.checkAdmin(tenantId, actorId);
      if (!admin.isAdmin) throw new UserFacingError('This command is limited to the tenant owner and qURL admins.');
    }
    if (command.verb === 'admins') { const admins = await this.#options.data.listAdmins(tenantId); return `Tenant owner: ${admins.ownerId}\nAdmins: ${admins.adminIds.length ? admins.adminIds.join(', ') : 'none'}`; }
    const mentionedAadObjectId = command.userId === undefined ? undefined : activity.entities?.find(entity => entity.mentioned?.id === command.userId)?.mentioned?.aadObjectId?.trim().toLowerCase();
    if (command.verb === 'add' || command.verb === 'remove') {
      if (!mentionedAadObjectId) throw new UserFacingError('The Teams user mention has no AAD object id');
      try {
        if (command.verb === 'add') await this.#options.data.addAdmin(tenantId, mentionedAadObjectId);
        else await this.#options.data.removeAdmin(tenantId, mentionedAadObjectId);
      } catch (error) {
        if (error instanceof TenantOwnerAlreadyAdminError) throw new UserFacingError('The tenant owner already has qURL admin access.');
        if (error instanceof TenantOwnerRemovalError) throw new UserFacingError('The tenant owner cannot be removed.');
        throw error;
      }
      return `${command.verb === 'add' ? 'Added' : 'Removed'} Teams user \`${command.userId}\` ${command.verb === 'add' ? 'as a qURL admin' : 'from qURL admins'} for this tenant.`;
    }
    if (command.verb === 'uninstall') {
      const credential = await this.#options.data.tenantCredential(tenantId);
      let upstreamRevocationPending = credential !== undefined && credential.keyId === undefined;
      if (credential?.keyId) {
        try {
          const qurl = await this.#qurl(tenantId);
          await qurl.revokeApiKey(credential.keyId, signal);
        } catch {
          // Local teardown must remain recoverable even when qURL is
          // unavailable. An operator can revoke the upstream key later.
          upstreamRevocationPending = true;
        }
      }
      await this.#options.data.deleteWorkspace(tenantId);
      return upstreamRevocationPending
        ? 'Disconnected qURL from this Teams tenant. Upstream API-key revocation may require operator follow-up.'
        : 'Disconnected qURL from this Teams tenant. If a later reinstall reports a retained upstream binding, contact your qURL operator for cleanup.';
    }
    if (!channel) throw new UserFacingError('This command is available only in Teams channels, not direct or group chats.');
    // Channel-policy-only verbs are answered before the tenant qURL client is
    // built (a DynamoDB read plus a KMS decrypt) and before the resource list
    // is paged; neither reads anything from qURL.
    if (command.verb === 'aliases') return this.aliases(tenantId, scopeId);
    if (command.verb === 'unset-alias') {
      const alias = command.resource ?? '';
      // Reply on what actually happened: an admin who mistypes an alias must
      // not be told channel access was removed when no row was touched.
      const removed = await this.#options.data.unbindScopeAlias(tenantId, scopeId, alias);
      return removed
        ? `Removed alias \`$${alias}\` from this channel. The resource remains protected.`
        : `No alias \`$${alias}\` is bound in this channel.`;
    }
    const qurl = await this.#qurl(tenantId);
    if (command.verb === 'protect-connector') return this.protectConnector(qurl, activity, tenantId, scopeId, command, signal);
    const resources = await this.resources(qurl, signal);
    if (command.verb === 'list') return this.list(tenantId, scopeId, resources);
    if (command.verb === 'get') return this.get(qurl, activity, tenantId, scopeId, resources, command, signal);
    if (command.verb === 'protect-url') return this.protectUrl(qurl, activity, tenantId, scopeId, resources, command, signal);
    if (command.verb === 'set-alias') return this.setAlias(qurl, tenantId, scopeId, resources, command, signal);
    if (command.verb === 'set-display-name' || command.verb === 'unset-display-name') {
      const setting = command.verb === 'set-display-name';
      const resource = this.resolve(resources, command.resource ?? '');
      await qurl.updateResource(resource.resourceId, setting ? command.text ?? '' : '', signal);
      return `${setting ? 'Updated' : 'Reset'} display name for \`$${resource.resourceId}\`.`;
    }
    if (command.verb === 'revoke') {
      const resource = this.resolve(resources, command.resource ?? '');
      await qurl.deleteResource(resource.resourceId, signal);
      await this.#options.data.purgeResourceFromTenant(tenantId, resource.resourceId);
      return `Revoked resource \`$${resource.resourceId}\`.`;
    }
    throw new UserFacingError('Unsupported qURL command.');
  }

  async captureConversation(activity: TeamsActivity): Promise<void> {
    if (activity.conversation?.conversationType !== 'personal') return;
    const tenantId = deriveScope(activity).tenantId;
    const actorAadObjectId = activity.from?.aadObjectId?.trim().toLowerCase() ?? '';
    if (!tenantId || !actorAadObjectId || !activity.serviceUrl || !activity.conversation.id) return;
    await this.#options.data.savePersonalConversationRef(tenantId, actorAadObjectId, {
      serviceUrl: activity.serviceUrl,
      conversationId: activity.conversation.id,
    });
  }

  async aliases(tenantId: string, scopeId: string): Promise<string> {
    const entries = await this.#options.data.scopeAliases(tenantId, scopeId);
    if (!entries.length) return 'No aliases are configured in this channel.';
    return `Aliases in this channel:\n${entries.map(entry => `- \`$${entry.alias}\` -> \`$${entry.resourceId}\``).join('\n')}`;
  }

  async resources(qurl: QurlClient, signal?: AbortSignal): Promise<QurlResource[]> {
    const result: QurlResource[] = [];
    const seenCursors = new Set<string>();
    let cursor: string | undefined;
    for (let pageCount = 0; ; pageCount += 1) {
      if (pageCount >= 1_000) throw new Error('qURL resource pagination exceeded the safety limit');
      const page = await qurl.listResources(signal, cursor);
      result.push(...page.resources.filter(resource => resource.status !== 'revoked'));
      if (!page.nextCursor) {
        if (page.hasMore === true) throw new Error('qURL resource pagination is invalid');
        return result;
      }
      // A repeated cursor would page forever against a misbehaving upstream.
      if (seenCursors.has(page.nextCursor)) throw new Error('qURL resource pagination is invalid');
      seenCursors.add(page.nextCursor);
      cursor = page.nextCursor;
    }
  }

  async list(tenantId: string, scopeId: string, resources: readonly QurlResource[]): Promise<string> {
    const allowed = await this.#options.data.allowedResourceIds(tenantId, scopeId);
    const visible = resources.filter(resource => allowed.has(resource.resourceId));
    if (!visible.length) return 'No protected resources are available in this channel yet.';
    const rows = visible.map(resource =>
      `- \`$${resource.resourceId}\`  ${resource.description ?? resource.slug ?? resource.targetUrl ?? resource.resourceId}`);
    return `Protected resources in this channel:\n${rows.join('\n')}`;
  }

  resolve(resources: readonly QurlResource[], token: string): QurlResource {
    const matches = resources.filter(resource =>
      resource.resourceId === token || resource.slug === token || resource.alias === token);
    if (matches.length !== 1) {
      throw new UserFacingError(matches.length ? 'Resource token is ambiguous' : `Resource not found: ${token}`);
    }
    const resource = matches[0];
    if (!resource) throw new UserFacingError('Resource not found');
    return resource;
  }

  async get(qurl: QurlClient, activity: TeamsActivity, tenantId: string, scopeId: string, resources: readonly QurlResource[], command: TeamsCommand, signal?: AbortSignal): Promise<string> {
    const token = command.resource ?? '';
    const [allowed, localAliasResourceId] = await Promise.all([
      this.#options.data.allowedResourceIds(tenantId, scopeId),
      this.#options.data.lookupScopeAlias(tenantId, scopeId, token),
    ]);
    // Only resources exposed in this channel are resolvable, whether the user
    // typed a channel alias or the resource id itself.
    const visible = resources.filter(item => allowed.has(item.resourceId));
    const resource = this.resolve(visible, localAliasResourceId ?? token);
    const wantsDm = command.flags.dm?.toLowerCase() === 'true';
    const dmActor = activity.from?.aadObjectId?.trim().toLowerCase() ?? '';
    const ref = wantsDm && dmActor ? await this.#options.data.personalConversationRef(tenantId, dmActor) : undefined;
    if (wantsDm && !ref) throw new UserFacingError('Open a personal chat with the bot before using dm:true.');
    const output = await qurl.create({
      resourceId: resource.resourceId,
      expiresIn: '1m',
      oneTimeUse: true,
      maxSessions: 1,
      sessionDuration: '1h',
      idempotencyKey: idempotencyKey(tenantId, scopeId, activity.from?.id ?? '', resource.resourceId, this.#activityIdempotencyField(activity)),
      ...(command.flags.reason ? { label: command.flags.reason } : {}),
    }, signal);
    if (ref) {
      await this.#options.messages.sendText(ref.serviceUrl, ref.conversationId, `qURL for \`$${token}\`: ${output.qurlLink}`, signal);
      return 'Sent the one-time qURL to your personal Teams chat.';
    }
    return `qURL for \`$${token}\`: ${output.qurlLink}`;
  }

  async protectUrl(qurl: QurlClient, activity: TeamsActivity, tenantId: string, scopeId: string, resources: readonly QurlResource[], command: TeamsCommand, signal?: AbortSignal): Promise<string> {
    const value = command.args[0] ?? '';
    const creating = value.toLowerCase().startsWith('url:');
    const resource = creating
      ? await qurl.createResource({
        targetUrl: value.slice(4),
        type: 'url',
        idempotencyKey: idempotencyKey(tenantId, scopeId, activity.from?.id ?? '', value, this.#activityIdempotencyField(activity)),
      }, signal)
      : this.resolve(resources, value.replace(/^\$/, ''));
    if (!creating && resource.type !== 'url') throw new UserFacingError('Only URL resources can be protected with protect-url');
    const resolvedAlias = command.flags.as ?? this.#channelAliasFor(resource);
    await this.#bindAlias(tenantId, scopeId, resolvedAlias, resource.resourceId);
    await this.#options.data.exposeResource(tenantId, scopeId, resource.resourceId);
    return `URL resource \`$${resource.resourceId}\` is now available in this channel.`;
  }

  /**
   * Pick a channel alias for a resource the caller did not name with `as:`.
   *
   * Only `as:` has been through the channel-alias grammar. The upstream alias,
   * slug, and resource id are qURL-side identifiers under no such constraint,
   * and `set-alias`/`unset-alias` parse their argument through that same
   * grammar — so binding one that fails it strands the alias in this channel
   * with no way to rename or remove it short of revoking the resource.
   */
  #channelAliasFor(resource: QurlResource): string {
    const candidate = [resource.alias, resource.slug, resource.resourceId]
      .find((value): value is string => value !== undefined && isChannelAlias(value));
    if (!candidate) {
      throw new UserFacingError('This resource has no channel-safe alias. Re-run `protect-url` with `as:$alias`.');
    }
    return candidate;
  }

  async setAlias(_qurl: QurlClient, tenantId: string, scopeId: string, resources: readonly QurlResource[], command: TeamsCommand, _signal?: AbortSignal): Promise<string> {
    const resource = this.resolve(resources, command.target ?? '');
    const alias = command.alias ?? '';
    await this.#bindAlias(tenantId, scopeId, alias, resource.resourceId);
    await this.#options.data.exposeResource(tenantId, scopeId, resource.resourceId);
    return `Alias \`$${alias}\` now points to \`$${resource.resourceId}\` in this channel.`;
  }

  async protectConnector(qurl: QurlClient, activity: TeamsActivity, tenantId: string, scopeId: string, command: TeamsCommand, signal?: AbortSignal): Promise<string> {
    const slug = command.resource ?? command.args[0] ?? '';
    validateTunnelSlug(slug);
    // A connector id may be 64 characters; a channel alias may be 63. Without
    // alias:, the id becomes the alias, so the wider grammar would otherwise
    // strand an unremovable row here exactly as #channelAliasFor describes --
    // and it would do so after the resource, alias, and exposure writes, since
    // renderTunnelInstallMessage only rejects it at the very end.
    const alias = command.flags.alias ?? slug;
    if (!isChannelAlias(alias)) {
      throw new UserFacingError('This connector id is not a usable channel alias. Re-run with `alias:$alias`.');
    }
    const ref = await this.#options.data.personalConversationRef(tenantId, activity.from?.aadObjectId?.trim().toLowerCase() ?? '');
    if (!ref) throw new UserFacingError('Open a personal chat with the bot before protecting a connector.');
    const resources = await this.resources(qurl, signal);
    const operationKey = [tenantId, scopeId, activity.from?.id ?? '', slug, this.#activityIdempotencyField(activity)];
    const resource = resources.find(item => item.type === 'tunnel' && item.slug === slug)
      ?? await qurl.createResource({ type: 'tunnel', slug, findOrCreate: true, idempotencyKey: idempotencyKey(...operationKey, 'resource') }, signal);
    await this.#bindAlias(tenantId, scopeId, alias, resource.resourceId);
    await this.#options.data.exposeResource(tenantId, scopeId, resource.resourceId);
    let token: QurlApiKey | undefined;
    try {
      token = await qurl.createEnrollmentToken(slug, idempotencyKey(...operationKey, 'enrollment'), signal);
      const installText = renderTunnelInstallMessage({ slug, alias, environment: normalizeTunnelEnvironment(command.flags.env ?? 'docker'), port: Number(command.flags.port ?? '8080'), image: this.#options.connectorImage ?? '', bootstrapKey: token.apiKey });
      await this.#options.messages.sendText(ref.serviceUrl, ref.conversationId, `Connector \`${slug}\` bootstrap instructions:\n${installText}`, signal);
    } catch (error) {
      if (token) {
        try { await qurl.revokeApiKey(token.keyId, signal); } catch { /* preserve the failure without leaking the bootstrap key */ }
      }
      // Resource and alias changes may predate this request or be concurrently updated. The one-time credential is the only newly-created secret and is revoked above.
      throw error;
    }
    return `Protected connector \`$${resource.resourceId}\` and sent the bootstrap instructions to your personal Teams chat.`;
  }
}

export function helpMessage(): string {
  return [
    'qURL for Teams',
    '',
    'User commands:',
    '- `setup <email>`',
    '- `get $<id|alias> [dm:true] [reason:"..."]`',
    '- `list`',
    '- `aliases`',
    '',
    'Admin commands:',
    '- `protect-url url:https://internal.example.com as:$docs`',
    '- `protect-connector <id>`',
    '- `set-alias $alias $resource-id`',
    '- `unset-alias $alias`',
    '- `set-display-name $resource-id Friendly name`',
    '- `unset-display-name $resource-id`',
    '- `revoke $resource-id`',
    '- `add @user` / `remove @user` / `admins`',
    '- `uninstall`',
  ].join('\n');
}
