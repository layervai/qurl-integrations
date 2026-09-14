import { randomUUID } from 'node:crypto';
import { isChannelAlias } from './alias.js';
import type { TeamsActivity } from './activity.js';
import { deriveScope, normalizeActivityText } from './activity.js';
import type { TeamsMessagePoster } from './connector.js';
import { idempotencyKey, QurlHttpError } from './qurl-client.js';
import type { QurlApiKey, QurlClient, QurlResource } from './qurl-client.js';
import { parseCommand } from './parser.js';
import type { TeamsCommand } from './parser.js';
import { ScopeAliasConflictError, TenantOwnerAlreadyAdminError, TenantOwnerRemovalError, type TeamsDataStore } from './teams-data.js';
import type { TeamsSetupLinkBuilder } from './setup-link.js';
import { normalizeTunnelEnvironment, renderTunnelBootstrapSecretMessage, renderTunnelInstallMessage, validateTunnelSlug, type TunnelHub } from './tunnel.js';
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
  /** qURL API origin rendered into Connector installs as QURL_ENDPOINT. */
  readonly qurlEndpoint: string;
  /** NHP Hub triple. Absent until the environment's Hub identity is seeded. */
  readonly connectorHub?: TunnelHub;
  /** Outbound serviceUrl allowlist, applied when capturing a conversation. */
  readonly validateServiceUrl?: (serviceUrl: string) => unknown;
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

  async #bindAlias(tenantId: string, scopeId: string, alias: string, resource: QurlResource): Promise<void> {
    const resourceId = resource.resourceId;
    const existing = await this.#options.data.lookupScopeAlias(tenantId, scopeId, alias);
    if (existing !== undefined && existing !== resourceId) {
      throw new UserFacingError(`Alias \`$${alias}\` is already in use in this channel.`);
    }
    try {
      await this.#options.data.bindScopeAlias(tenantId, scopeId, alias, resourceId, resource.crid);
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
        response = signal?.aborted
          ? 'The qURL command timed out. Some changes may have completed; check the result before retrying.'
          : 'The qURL command could not be completed. Check the command syntax and try again.';
      }
    }
    if (response) {
      try {
        if (reply) await reply(response);
        // Final delivery has the adapter's own HTTP deadline, as Slack's
        // follow-up does. An expired work signal must not suppress the result.
        else await this.#options.messages.reply(activity, response);
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
    // Genuine faults, not user error -- but "check the command syntax" sends
    // the user hunting for a typo they cannot fix. Name the real condition.
    if (!tenantId) throw new UserFacingError('This activity did not carry a Teams tenant id, so qURL cannot scope the request. Please report this if it repeats.');
    if (!actorId) throw new UserFacingError('This activity did not carry your Teams directory id, so qURL cannot identify you. Please report this if it repeats.');
    if (command.verb === 'setup') {
      if (!this.#options.setup || !command.email) throw new Error('Teams OAuth setup is not configured');
      const { ownerId } = await this.#options.data.checkAdmin(tenantId, actorId);
      if (ownerId !== undefined && ownerId !== actorId) {
        throw new UserFacingError('This Teams tenant is already connected to qURL. Ask the person who connected it to re-run `qurl setup`.');
      }
      const deliveryId = activity.from?.id?.trim() ?? '';
      if (!deliveryId) throw new Error('Teams actor delivery id is required');
      const link = await this.#options.setup.build(tenantId, actorId, deliveryId, command.email, command.setupMode ?? 'bind');
      // The setup URL carries the opaque one-shot state handle, which the rest
      // of this flow treats as secret (httpOnly/Secure/SameSite cookie,
      // five-minute TTL, constant-time compare). Replying in place would post
      // it into persistent channel history -- readable by every member and by
      // any export/eDiscovery path -- so it goes to the personal chat only.
      const ref = await this.#options.data.personalConversationRef(tenantId, actorId);
      if (!ref) throw new UserFacingError('Open a personal chat with the bot, then run `qurl setup` again. The setup link is a one-time secret and is never posted in a channel.');
      await this.#options.messages.sendText(ref.serviceUrl, ref.conversationId, `Open this qURL setup link in your browser:\n${link.url.toString()}`);
      return 'Sent your one-time qURL setup link to our personal chat. It is not posted here because it is a one-time secret.';
    }
    if (command.verb === 'feedback') {
      // The production runtime does not wire a feedback handler, so the verb is
      // no longer advertised in help. Someone who types it anyway gets a plain
      // answer rather than a generic failure plus an error-level log entry.
      if (!this.#options.feedback) throw new UserFacingError('Feedback is not enabled for this qURL installation.');
      await this.#options.feedback({ tenantId, actorId, message: command.text ?? '' }); return 'Thanks. The qURL team received your feedback.';
    }
    const admin = ADMIN_COMMANDS.has(command.verb) ? await this.#options.data.checkAdmin(tenantId, actorId) : undefined;
    if (admin && !admin.isAdmin) throw new UserFacingError('This command is limited to the tenant owner and qURL admins.');
    if (command.verb === 'admins') { const admins = await this.#options.data.listAdmins(tenantId); return `Tenant owner: ${admins.ownerId}\nAdmins: ${admins.adminIds.length ? admins.adminIds.join(', ') : 'none'}`; }
    if (command.verb === 'add' || command.verb === 'remove') {
      if (!admin?.installationId) throw new Error('workspace installation is unavailable');
      const mention = activity.entities?.find(entity => entity.type === 'mention' && entity.mentioned?.id === command.userId)?.mentioned;
      const mentionedAadObjectId = (mention?.aadObjectId
        || (mention?.id ? await this.#options.messages.resolveMemberAadObjectId?.(activity, mention.id, signal) : undefined))?.trim().toLowerCase();
      if (!mentionedAadObjectId) throw new UserFacingError('This Teams member could not be resolved to a directory identity. Select a user mention and try again.');
      try {
        if (command.verb === 'add') await this.#options.data.addAdmin(tenantId, mentionedAadObjectId, admin.installationId);
        else await this.#options.data.removeAdmin(tenantId, mentionedAadObjectId, admin.installationId);
      } catch (error) {
        if (error instanceof TenantOwnerAlreadyAdminError) throw new UserFacingError('The tenant owner already has qURL admin access.');
        if (error instanceof TenantOwnerRemovalError) throw new UserFacingError('The tenant owner cannot be removed.');
        throw error;
      }
      return `Teams user \`${mentionedAadObjectId}\` ${command.verb === 'add' ? 'has' : 'does not have'} qURL admin access for this tenant.`;
    }
    if (command.verb === 'uninstall') {
      if (!admin?.installationId) throw new Error('workspace installation is unavailable');
      const credential = await this.#options.data.tenantCredential(tenantId);
      let upstreamRevocationPending = credential !== undefined && credential.keyId === undefined;
      if (credential?.keyId) {
        try {
          const qurl = await this.#qurl(tenantId);
          await qurl.revokeApiKey(credential.keyId, signal);
        } catch (error) {
          if (!(error instanceof QurlHttpError) || (error.status !== 401 && error.status !== 403)) throw error;
          this.#options.logger?.warn('Tenant credential requires upstream cleanup after local disconnect', { tenantId, keyId: credential.keyId, error });
          upstreamRevocationPending = true;
        }
      }
      await this.#options.data.deleteWorkspace(tenantId, admin.installationId, signal);
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
    if (command.verb === 'get') return this.get(qurl, activity, tenantId, scopeId, command, signal);
    if (command.verb === 'revoke') {
      const token = command.resource ?? '';
      const aliasResourceId = await this.#options.data.lookupScopeAlias(tenantId, scopeId, token);
      // TODO(upstream-contract): qurl-service api/openapi.yaml ResourceId accepts
      // public keys and 47/60-character CRIDs; the API verifies their semantics.
      const publicKeyShape = /^[A-Za-z0-9_-]{107,214}$/.test(token) && token.length % 4 !== 1;
      let resource: QurlResource;
      if (aliasResourceId) resource = { resourceId: aliasResourceId };
      else if (/^([a-z2-7]{47}|[a-z2-7]{60})$/.test(token)) {
        try {
          // Detail reads include ordinarily revoked resources. Recover the
          // stored public key even after a partial purge loses this alias.
          resource = await qurl.getResource(token, signal);
        } catch (error) {
          if (error instanceof QurlHttpError && (error.status === 404 || error.status === 410)) {
            throw new UserFacingError('This CRID is unavailable to this account. To retry local cleanup, use a retained channel alias or contact your qURL operator.');
          }
          throw error;
        }
      } else if (publicKeyShape || (await this.#options.data.allowedResourceIds(tenantId, scopeId)).has(token)) {
        resource = { resourceId: token };
      } else resource = this.resolve(await this.resources(qurl, signal), token);
      const resourceId = resource.resourceId;
      try {
        await qurl.deleteResource(resourceId, signal);
      } catch (error) {
        if (!(error instanceof QurlHttpError) || (error.status !== 404 && error.status !== 410)) throw error;
      }
      await this.#options.data.purgeResourceFromTenant(tenantId, resourceId, signal);
      return `Resource \`$${resource.crid ?? token}\` is revoked or already unavailable to this account.`;
    }
    const resources = await this.resources(qurl, signal);
    if (command.verb === 'list') return this.list(tenantId, scopeId, resources);
    if (command.verb === 'protect-url') return this.protectUrl(qurl, activity, tenantId, scopeId, resources, command, signal);
    if (command.verb === 'set-alias') return this.setAlias(qurl, tenantId, scopeId, resources, command, signal);
    if (command.verb === 'set-display-name' || command.verb === 'unset-display-name') {
      const setting = command.verb === 'set-display-name';
      const resource = await this.resolveInScope(tenantId, scopeId, resources, command.resource ?? '');
      await qurl.updateResource(resource.resourceId, setting ? command.text ?? '' : '', signal);
      return `${setting ? 'Updated' : 'Reset'} display name for \`$${resource.crid ?? resource.resourceId}\`.`;
    }
    throw new UserFacingError('Unsupported qURL command.');
  }

  async captureConversation(activity: TeamsActivity): Promise<void> {
    if (activity.conversation?.conversationType !== 'personal') return;
    const tenantId = deriveScope(activity).tenantId;
    const actorAadObjectId = activity.from?.aadObjectId?.trim().toLowerCase() ?? '';
    if (!tenantId || !actorAadObjectId || !activity.serviceUrl || !activity.conversation.id) return;
    // Validate on the way IN, not just at send time. The SDK authenticates the
    // inbound activity, so this is not an SSRF gate -- it keeps a row whose
    // serviceUrl the outbound validator will reject from being stored at all,
    // where it would silently break setup, dm:true and protect-connector
    // later with a generic error and stay in the table.
    try {
      this.#options.validateServiceUrl?.(activity.serviceUrl);
    } catch {
      this.#options.logger?.warn?.('teams: refusing to store a personal conversation with an unusable serviceUrl', { tenantId });
      return;
    }
    await this.#options.data.savePersonalConversationRef(tenantId, actorAadObjectId, {
      serviceUrl: activity.serviceUrl,
      conversationId: activity.conversation.id,
    });
  }

  async aliases(tenantId: string, scopeId: string): Promise<string> {
    const entries = await this.#options.data.scopeAliases(tenantId, scopeId);
    if (!entries.length) return 'No aliases are configured in this channel.';
    return `Aliases in this channel:\n${entries.map(entry => `- \`$${entry.alias}\` -> \`$${entry.crid ?? entry.resourceId}\``).join('\n')}`;
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
      `- \`$${resource.crid ?? resource.resourceId}\`  ${resource.description ?? resource.slug ?? resource.targetUrl ?? resource.crid ?? resource.resourceId}`);
    return `Protected resources in this channel:\n${rows.join('\n')}`;
  }

  resolve(resources: readonly QurlResource[], token: string): QurlResource {
    const matches = resources.filter(resource =>
      resource.crid === token || resource.resourceId === token || resource.slug === token || resource.alias === token);
    if (matches.length !== 1) {
      throw new UserFacingError(matches.length ? 'Resource token is ambiguous' : `Resource not found: ${token}`);
    }
    const resource = matches[0];
    if (!resource) throw new UserFacingError('Resource not found');
    return resource;
  }

  async resolveInScope(tenantId: string, scopeId: string, resources: readonly QurlResource[], token: string): Promise<QurlResource> {
    const resourceId = await this.#options.data.lookupScopeAlias(tenantId, scopeId, token);
    return this.resolve(resources, resourceId ?? token);
  }

  async get(qurl: QurlClient, activity: TeamsActivity, tenantId: string, scopeId: string, command: TeamsCommand, signal?: AbortSignal): Promise<string> {
    const token = command.resource ?? '';
    const [allowed, aliasResourceId] = await Promise.all([
      this.#options.data.allowedResourceIds(tenantId, scopeId),
      this.#options.data.lookupScopeAlias(tenantId, scopeId, token),
    ]);
    // A bound channel alias already names the resource, as in Slack. The API
    // validates its live status at mint; only unbound tokens need discovery.
    const resourceId = aliasResourceId ?? this.resolve((await this.resources(qurl, signal)).filter(item => allowed.has(item.resourceId)), token).resourceId;
    if (!allowed.has(resourceId)) throw new UserFacingError(`Resource not found: ${token}`);
    const wantsDm = command.flags.dm === 'true';
    const dmActor = activity.from?.aadObjectId?.trim().toLowerCase() ?? '';
    const ref = wantsDm && dmActor ? await this.#options.data.personalConversationRef(tenantId, dmActor) : undefined;
    if (wantsDm && !ref) throw new UserFacingError('Open a personal chat with the bot before using dm:true.');
    const output = await qurl.create({
      resourceId,
      expiresIn: '1m',
      oneTimeUse: true,
      maxSessions: 1,
      sessionDuration: '1h',
      idempotencyKey: idempotencyKey(tenantId, scopeId, activity.from?.id ?? '', resourceId, this.#activityIdempotencyField(activity)),
      ...(command.flags.reason ? { label: command.flags.reason } : {}),
    }, signal).catch((error: unknown) => {
      if (signal?.aborted || isUserFacingError(error)) throw error;
      if (error instanceof QurlHttpError) {
        if (error.status === 429) {
          throw new UserFacingError(error.retryAfterSeconds
            ? `qURL is rate limited. Try again in ${error.retryAfterSeconds} seconds.`
            : 'qURL is rate limited. Please try again shortly.');
        }
        if (error.status === 403 && error.code === 'connector_disabled') {
          throw new UserFacingError('Connector access is not enabled in this environment. Contact your qURL operator.');
        }
        if (error.status === 403 && (error.code === 'api_key_limit' || error.code === 'quota_exceeded')) {
          this.#options.logger?.info('qURL mint rejected by an account limit', { code: error.code });
          throw new UserFacingError('This qURL account has reached a limit and cannot create another link. Ask the account owner to review its limits.');
        }
        if (error.status < 500 || error.status >= 600) throw error;
      }
      this.#options.logger?.warn('qURL mint request failed', { error });
      throw new UserFacingError('The qURL service is temporarily unavailable. Please try again.');
    });
    const message = `qURL for \`$${token}\` (one-time use; 1-minute lifetime): ${output.qurlLink}`;
    if (ref) {
      await this.#options.messages.sendText(ref.serviceUrl, ref.conversationId, message, signal);
      return 'Sent the one-time qURL to your personal Teams chat.';
    }
    return message;
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
      : await this.resolveInScope(tenantId, scopeId, resources, value.replace(/^\$/, ''));
    if (!creating && resource.type !== 'url') throw new UserFacingError('Only URL resources can be protected with protect-url');
    const resolvedAlias = command.flags.as ?? this.#channelAliasFor(resource);
    await this.#bindAlias(tenantId, scopeId, resolvedAlias, resource);
    await this.#options.data.exposeResource(tenantId, scopeId, resource.resourceId);
    return `URL resource \`$${resource.crid ?? resource.resourceId}\` is now available in this channel.`;
  }

  /**
   * Pick a channel alias for a resource the caller did not name with `as:`.
   *
   * Only `as:` has been through the channel-alias grammar. The upstream alias,
   * slug, CRID, and public key are qURL-side identifiers under no such constraint,
   * and `set-alias`/`unset-alias` parse their argument through that same
   * grammar — so binding one that fails it strands the alias in this channel
   * with no way to rename or remove it short of revoking the resource.
   */
  #channelAliasFor(resource: QurlResource): string {
    const candidate = [resource.alias, resource.slug, resource.crid, resource.resourceId]
      .find((value): value is string => value !== undefined && isChannelAlias(value));
    if (!candidate) {
      throw new UserFacingError('This resource has no channel-safe alias. Re-run `protect-url` with `as:$alias`.');
    }
    return candidate;
  }

  async setAlias(_qurl: QurlClient, tenantId: string, scopeId: string, resources: readonly QurlResource[], command: TeamsCommand, _signal?: AbortSignal): Promise<string> {
    const resource = await this.resolveInScope(tenantId, scopeId, resources, command.target ?? '');
    const alias = command.alias ?? '';
    await this.#bindAlias(tenantId, scopeId, alias, resource);
    await this.#options.data.exposeResource(tenantId, scopeId, resource.resourceId);
    return `Alias \`$${alias}\` now points to \`$${resource.crid ?? resource.resourceId}\` in this channel.`;
  }

  async protectConnector(qurl: QurlClient, activity: TeamsActivity, tenantId: string, scopeId: string, command: TeamsCommand, signal?: AbortSignal): Promise<string> {
    const slug = command.resource ?? command.args[0] ?? '';
    validateTunnelSlug(slug);
    // A connector id may be 64 characters; a channel alias may be 63. Without
    // alias:, the id becomes the alias, so the wider grammar would otherwise
    // strand an unremovable row exactly as #channelAliasFor describes -- and it
    // would do so after the resource, alias, and exposure writes below. Checked
    // here so the request fails before any of them.
    const alias = command.flags.alias ?? slug;
    if (!isChannelAlias(alias)) {
      throw new UserFacingError('This connector id is not a usable channel alias. Re-run with `alias:$alias`.');
    }
    const ref = await this.#options.data.personalConversationRef(tenantId, activity.from?.aadObjectId?.trim().toLowerCase() ?? '');
    if (!ref) throw new UserFacingError('Open a personal chat with the bot before protecting a connector.');
    // Resolved before any remote mutation so an identity outage aborts without
    // leaving sharing state behind. Only an API-key principal names the account
    // owner; a delegated credential would name the calling user instead.
    const identity = await qurl.me(signal);
    if (!identity.isApiKeyPrincipal) throw new UserFacingError('This qURL credential cannot provision a connector. Re-run `qurl setup`.');
    const ownerId = identity.ownerId;
    const resources = await this.resources(qurl, signal);
    const operationKey = [tenantId, scopeId, activity.from?.id ?? '', slug, this.#activityIdempotencyField(activity)];
    const resource = resources.find(item => item.type === 'tunnel' && item.slug === slug)
      ?? await qurl.createResource({ type: 'tunnel', slug, findOrCreate: true, idempotencyKey: idempotencyKey(...operationKey, 'resource') }, signal);
    // Fail before the alias/exposure writes: a tunnel resource without full
    // routing metadata cannot produce a config the daemon will accept, and
    // there is nothing to clean up at this point.
    if (!resource.connectorRoutingId || !resource.knockResourceId) {
      throw new UserFacingError('qURL returned incomplete connector routing metadata. No enrollment token was minted; please retry.');
    }
    // Bind before exposing, same as protectUrl. Without this the `alias:` flag
    // parses and validates and then does nothing: `get $alias`, `aliases` and
    // `unset-alias` would all report the alias does not exist.
    await this.#bindAlias(tenantId, scopeId, alias, resource);
    await this.#options.data.exposeResource(tenantId, scopeId, resource.resourceId);
    // Sharing must be restarted before the config is rendered: `serving_epoch`
    // and the CRID both come from that response, and a daemon handed a stale
    // epoch is refused. apps/slack does the same in the same order.
    const previousSharing = await qurl.getSharing(resource.resourceId, signal);
    let publicToken: string;
    let token: QurlApiKey | undefined;
    try {
      const restarted = await qurl.restartSharing(resource.resourceId, signal).catch(async (error: unknown) => {
        if (signal?.aborted || (error instanceof QurlHttpError && error.status < 500 && error.status !== 429)) throw error;
        // Like Slack, send this non-idempotent POST once. A lost response may
        // still have advanced sharing; only an authoritative newer on-state
        // permits enrollment. Never replay the POST to resolve uncertainty.
        const current = await qurl.getSharing(resource.resourceId, signal);
        if (current.desiredState === 'on' && current.servingEpoch > previousSharing.servingEpoch) return current;
        throw new Error('qURL sharing restart is uncertain; authoritative state did not advance', { cause: error });
      });
      publicToken = restarted.crid;
      const installArgs = {
        slug,
        alias,
        environment: normalizeTunnelEnvironment(command.flags.env ?? 'docker'),
        port: Number(command.flags.port ?? '8080'),
        ...(command.flags.service ? { service: command.flags.service } : {}),
        image: this.#options.connectorImage ?? '',
        endpoint: this.#options.qurlEndpoint,
        ownerId,
        crid: restarted.crid,
        resourceId: resource.resourceId,
        connectorRoutingId: resource.connectorRoutingId,
        knockResourceId: resource.knockResourceId,
        servingEpoch: restarted.servingEpoch,
        ...(this.#options.connectorHub ? { hub: this.#options.connectorHub } : {}),
      };
      // Render before minting so a bad contract fails without creating a secret.
      const installText = renderTunnelInstallMessage(installArgs);
      token = await qurl.createEnrollmentToken(slug, idempotencyKey(...operationKey, 'enrollment'), signal);
      // Two messages, both to the personal chat: the install block, then the
      // one-time token on its own. The install block never contains the token
      // (it prompts for it), so only the second message carries a secret and
      // only it has to be deleted afterwards. Secret last, so a delivery
      // failure cannot leave the token sitting in chat without instructions.
      const secretText = renderTunnelBootstrapSecretMessage(slug, token.apiKey);
      await this.#options.messages.sendText(ref.serviceUrl, ref.conversationId, `Connector \`${slug}\` install instructions:\n${installText}`, signal);
      await this.#options.messages.sendText(ref.serviceUrl, ref.conversationId, secretText, signal);
    } catch (error) {
      // HTTP cleanup keeps its own deadline after the activity is cancelled.
      if (token) {
        try { await qurl.revokeApiKey(token.keyId); }
        catch (cleanupError) { this.#options.logger?.warn('Connector enrollment credential requires operator cleanup', { tenantId, keyId: token.keyId, error: cleanupError }); }
      }
      // Resource and alias changes may predate this request or be concurrently updated.
      // A previously-off Connector owns the `on` transition this request made,
      // so compensate it back off. A previously-on one is left alone: its live
      // daemon reacquires the rotated epoch, and turning it off would be an
      // outage for a device this failure never touched.
      if (previousSharing.desiredState !== 'on') {
        try { await qurl.stopSharing(resource.resourceId); }
        catch (cleanupError) { this.#options.logger?.warn('Connector sharing rollback requires operator cleanup', { tenantId, resourceId: resource.resourceId, error: cleanupError }); }
      }
      throw error;
    }
    return `Protected connector \`$${publicToken}\` and sent the bootstrap instructions to your personal Teams chat.`;
  }
}

export function helpMessage(): string {
  return [
    'qURL for Teams',
    '',
    'User commands:',
    '- `setup <email>`',
    '- `get $<crid|alias> [dm:true] [reason:"..."]`',
    '- `list`',
    '- `aliases`',
    '',
    'Admin commands:',
    '- `protect-url url:https://internal.example.com as:$docs`',
    '- `protect-connector <id> [env:docker|compose|ecs-fargate|kubernetes] [port:8080] [service:web] [alias:$name]`',
    '- `set-alias $alias $crid`',
    '- `unset-alias $alias`',
    '- `set-display-name $crid Friendly name`',
    '- `unset-display-name $crid`',
    '- `revoke $crid`',
    '- `add @user` / `remove @user` / `admins`',
    '- `uninstall`',
  ].join('\n');
}
