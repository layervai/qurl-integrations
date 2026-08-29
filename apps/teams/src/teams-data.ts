import {
  DeleteCommand,
  GetCommand,
  PutCommand,
  QueryCommand,
  UpdateCommand,
  type DeleteCommandInput,
  type GetCommandInput,
  type PutCommandInput,
  type QueryCommandInput,
  type UpdateCommandInput,
} from '@aws-sdk/lib-dynamodb';
import type { DynamoDBDocumentClient } from '@aws-sdk/lib-dynamodb';
import type { CredentialCipher } from './credential-cipher.js';

export class TenantOwnerRemovalError extends Error {
  constructor() {
    super('tenant owner cannot be removed');
    this.name = 'TenantOwnerRemovalError';
  }
}

export class TenantOwnerAlreadyAdminError extends Error {
  constructor() {
    super('tenant owner already has admin access');
    this.name = 'TenantOwnerAlreadyAdminError';
  }
}

export interface DynamoRequest {
  readonly operation: 'get' | 'put' | 'update' | 'delete' | 'query';
  readonly input: Record<string, unknown>;
}

export interface DynamoClient {
  send<T = Record<string, unknown>>(request: DynamoRequest): Promise<T>;
}

export function createDynamoClient(client: DynamoDBDocumentClient): DynamoClient {
  return {
    async send<T>(request: DynamoRequest): Promise<T> {
      switch (request.operation) {
        case 'get': return await client.send(new GetCommand(request.input as GetCommandInput)) as T;
        case 'put': return await client.send(new PutCommand(request.input as PutCommandInput)) as T;
        case 'update': return await client.send(new UpdateCommand(request.input as UpdateCommandInput)) as T;
        case 'delete': return await client.send(new DeleteCommand(request.input as DeleteCommandInput)) as T;
        case 'query': return await client.send(new QueryCommand(request.input as QueryCommandInput)) as T;
      }
      throw new Error(`Unsupported DynamoDB operation: ${request.operation}`);
    },
  };
}

export interface PersonalConversationRef { readonly serviceUrl: string; readonly conversationId: string; }
export interface PolicyEntry { readonly scopeId: string; readonly alias: string; readonly resourceId: string; }
export interface WorkspaceMapping { readonly tenantId: string; readonly ownerId: string; readonly createdAt?: string; }
export interface TenantCredential { readonly apiKey: string; readonly keyId?: string; readonly keyPrefix?: string; readonly updatedAt?: string; }

export interface TeamsDataStoreOptions {
  readonly client: DynamoClient;
  readonly tenantPrincipalsTable: string;
  readonly channelPoliciesTable: string;
  readonly personalConversationsTable: string;
  // TODO(upstream-contract): this externally managed table intentionally uses
  // tenant_id, unlike the Teams-scoped tables that use teams_tenant_id.
  readonly tenantCredentialsTable: string;
  readonly credentialCipher?: CredentialCipher;
  readonly now?: () => Date;
}

const tenantKey = 'teams_tenant_id';
const principalKey = 'principal_key';
const policyKey = 'policy_key';
const ownerPrincipal = 'owner';

function principalDdbKey(tenantId: string, value: string): Record<string, string> {
  return { [tenantKey]: tenantId, [principalKey]: value };
}
function policyDdbKey(tenantId: string, value: string): Record<string, string> {
  return { [tenantKey]: tenantId, [policyKey]: value };
}
function personalDdbKey(tenantId: string, actorAadObjectId: string): Record<string, string> {
  return { [tenantKey]: tenantId, actor_aad_object_id: actorAadObjectId };
}
function nowIso(now: () => Date): string { return now().toISOString(); }
function assertPresent(...values: string[]): void {
  if (values.some(value => !value.trim())) throw new Error('required Teams data field is missing');
}
function isConditionalCheckFailed(error: unknown): boolean {
  return error instanceof Error && error.name === 'ConditionalCheckFailedException';
}

export class ScopeAliasConflictError extends Error {
  readonly alias: string;
  readonly existingResourceId: string | undefined;

  constructor(alias: string, existingResourceId?: string) {
    super(`Scope alias is already bound: ${alias}`);
    this.name = 'ScopeAliasConflictError';
    this.alias = alias;
    this.existingResourceId = existingResourceId;
  }
}
function asString(value: unknown): string | undefined {
  return typeof value === 'string' && value !== '' ? value : undefined;
}
function keyPart(value: string): string { return encodeURIComponent(value); }
function resourcePolicyKey(scopeId: string, resourceId: string): string {
  return `scope#${keyPart(scopeId)}#resource#${keyPart(resourceId)}`;
}
function aliasPolicyKey(scopeId: string, alias: string): string {
  return `scope#${keyPart(scopeId)}#alias#${keyPart(alias)}`;
}
function scopePolicyPrefix(scopeId: string): string {
  return `scope#${keyPart(scopeId)}#`;
}
// The channel-policies GSI provisioned by modules/qurl-teams-ddb. Its keys are
// the two encoded attributes #policyItem materializes on every row, and its
// KEYS_ONLY projection returns the base-table keys needed to delete a row.
const resourceScopesIndex = 'resource_scopes';
function resourceIndexKey(tenantId: string, resourceId: string): string {
  return `${keyPart(tenantId)}#${keyPart(resourceId)}`;
}
function scopeIndexKey(scopeId: string, itemType: 'resource' | 'alias', value: string): string {
  return `${keyPart(scopeId)}#${itemType}#${keyPart(value)}`;
}

export class TeamsDataStore {
  readonly #client: DynamoClient;
  readonly #tenantPrincipalsTable: string;
  readonly #channelPoliciesTable: string;
  readonly #personalConversationsTable: string;
  readonly #tenantCredentialsTable: string;
  readonly #credentialCipher: CredentialCipher | undefined;
  readonly #now: () => Date;

  constructor(options: TeamsDataStoreOptions) {
    this.#client = options.client;
    this.#tenantPrincipalsTable = options.tenantPrincipalsTable;
    this.#channelPoliciesTable = options.channelPoliciesTable;
    this.#personalConversationsTable = options.personalConversationsTable;
    this.#tenantCredentialsTable = options.tenantCredentialsTable;
    this.#credentialCipher = options.credentialCipher;
    this.#now = options.now ?? (() => new Date());
  }

  async bindWorkspace(mapping: WorkspaceMapping, seedAdmin: string): Promise<void> {
    assertPresent(mapping.tenantId, mapping.ownerId, seedAdmin);
    await this.#client.send({
      operation: 'put',
      input: {
        TableName: this.#tenantPrincipalsTable,
        Item: {
          ...principalDdbKey(mapping.tenantId, ownerPrincipal),
          principal_type: 'owner',
          actor_aad_object_id: mapping.ownerId,
          created_at: mapping.createdAt ?? nowIso(this.#now),
          updated_at: nowIso(this.#now),
        },
        ConditionExpression: `attribute_not_exists(${tenantKey}) AND attribute_not_exists(${principalKey})`,
      },
    });
    if (seedAdmin !== mapping.ownerId) await this.addAdmin(mapping.tenantId, seedAdmin);
  }

  async checkAdmin(tenantId: string, actorId: string): Promise<{ readonly isAdmin: boolean; readonly ownerId?: string }> {
    assertPresent(tenantId, actorId);
    const [owner, admin] = await Promise.all([
      this.#client.send<{ readonly Item?: Record<string, unknown> }>({ operation: 'get', input: { TableName: this.#tenantPrincipalsTable, Key: principalDdbKey(tenantId, ownerPrincipal), ConsistentRead: true } }),
      this.#client.send<{ readonly Item?: Record<string, unknown> }>({ operation: 'get', input: { TableName: this.#tenantPrincipalsTable, Key: principalDdbKey(tenantId, `admin#${keyPart(actorId)}`), ConsistentRead: true } }),
    ]);
    const ownerId = asString(owner.Item?.actor_aad_object_id);
    return { isAdmin: ownerId === actorId || admin.Item?.principal_type === 'admin', ...(ownerId ? { ownerId } : {}) };
  }

  async listAdmins(tenantId: string): Promise<{ readonly ownerId: string; readonly adminIds: readonly string[] }> {
    assertPresent(tenantId);
    const items = await this.#queryTenant(this.#tenantPrincipalsTable, tenantId);
    const owner = items.find(item => item.principal_type === 'owner');
    const ownerId = asString(owner?.actor_aad_object_id);
    if (!ownerId) throw new Error('workspace not bound');
    const adminIds = items.filter(item => item.principal_type === 'admin')
      .map(item => asString(item.actor_aad_object_id)).filter((id): id is string => id !== undefined).sort();
    return { ownerId, adminIds };
  }

  async addAdmin(tenantId: string, actorId: string): Promise<void> {
    assertPresent(tenantId, actorId);
    const owner = await this.#client.send<{ readonly Item?: Record<string, unknown> }>({ operation: 'get', input: { TableName: this.#tenantPrincipalsTable, Key: principalDdbKey(tenantId, ownerPrincipal), ConsistentRead: true } });
    if (asString(owner.Item?.actor_aad_object_id) === actorId) throw new TenantOwnerAlreadyAdminError();
    try {
      await this.#client.send({ operation: 'put', input: {
        TableName: this.#tenantPrincipalsTable,
        Item: { ...principalDdbKey(tenantId, `admin#${keyPart(actorId)}`), principal_type: 'admin', actor_aad_object_id: actorId, created_at: nowIso(this.#now), updated_at: nowIso(this.#now) },
        ConditionExpression: `attribute_not_exists(${principalKey})`,
      } });
    } catch (error) {
      if (!isConditionalCheckFailed(error)) throw error;
    }
  }

  async removeAdmin(tenantId: string, actorId: string): Promise<void> {
    assertPresent(tenantId, actorId);
    const owner = await this.#client.send<{ readonly Item?: Record<string, unknown> }>({ operation: 'get', input: { TableName: this.#tenantPrincipalsTable, Key: principalDdbKey(tenantId, ownerPrincipal), ConsistentRead: true } });
    if (asString(owner.Item?.actor_aad_object_id) === actorId) throw new TenantOwnerRemovalError();
    try {
      await this.#client.send({ operation: 'delete', input: { TableName: this.#tenantPrincipalsTable, Key: principalDdbKey(tenantId, `admin#${keyPart(actorId)}`), ConditionExpression: `attribute_exists(${principalKey})` } });
    } catch (error) {
      if (!isConditionalCheckFailed(error)) throw error;
    }
  }

  async deleteWorkspace(tenantId: string): Promise<void> {
    assertPresent(tenantId);
    // DynamoDB has no cross-table transaction for this cleanup. Deletes are
    // deliberately idempotent so an interrupted uninstall can be retried.
    for (const [table, keyName] of [[this.#tenantPrincipalsTable, principalKey], [this.#channelPoliciesTable, policyKey]] as const) {
      const items = await this.#queryTenant(table, tenantId);
      for (const item of items) {
        const value = asString(item[keyName]);
        if (value) await this.#delete(table, keyName === principalKey ? principalDdbKey(tenantId, value) : policyDdbKey(tenantId, value));
      }
    }
    const conversations = await this.#queryTenant(this.#personalConversationsTable, tenantId);
    for (const item of conversations) {
      const actorId = asString(item.actor_aad_object_id);
      if (actorId) await this.#delete(this.#personalConversationsTable, personalDdbKey(tenantId, actorId));
    }
    await this.#delete(this.#tenantCredentialsTable, { tenant_id: tenantId });
  }

  async purgeResourceFromTenant(tenantId: string, resourceId: string): Promise<void> {
    assertPresent(tenantId, resourceId);
    // The resource_scopes hash key already narrows to exactly this tenant and
    // resource, so every returned row is one to delete -- no tenant-wide read
    // and no client-side filter. A GSI cannot be read consistently, so a row
    // written moments earlier may not be indexed yet; as above, this cleanup
    // is best effort and safe to rerun after a partial failure.
    const items = await this.#queryAll({
      TableName: this.#channelPoliciesTable,
      IndexName: resourceScopesIndex,
      KeyConditionExpression: 'tenant_resource_key = :resourceKey',
      ExpressionAttributeValues: { ':resourceKey': resourceIndexKey(tenantId, resourceId) },
    });
    for (const item of items) {
      const policyKeyValue = asString(item.policy_key);
      if (policyKeyValue) await this.#delete(this.#channelPoliciesTable, policyDdbKey(tenantId, policyKeyValue));
    }
  }

  async saveTenantCredential(tenantId: string, credential: TenantCredential): Promise<void> {
    assertPresent(tenantId, credential.apiKey);
    const storedApiKey = this.#credentialCipher
      ? await this.#credentialCipher.encrypt(tenantId, credential.apiKey)
      : credential.apiKey;
    const setExpressions = [
      'qurl_api_key = :apiKey',
      'updated_at = :now',
      ...(credential.keyId !== undefined ? ['qurl_key_id = :keyId'] : []),
      ...(credential.keyPrefix !== undefined ? ['qurl_key_prefix = :keyPrefix'] : []),
    ];
    const removeExpressions = [
      ...(credential.keyId === undefined ? ['qurl_key_id'] : []),
      ...(credential.keyPrefix === undefined ? ['qurl_key_prefix'] : []),
    ];
    await this.#client.send({ operation: 'update', input: {
      TableName: this.#tenantCredentialsTable,
      Key: { tenant_id: tenantId },
      UpdateExpression: `SET ${setExpressions.join(', ')}${removeExpressions.length ? ` REMOVE ${removeExpressions.join(', ')}` : ''}`,
      ExpressionAttributeValues: { ':apiKey': storedApiKey, ':now': credential.updatedAt ?? nowIso(this.#now), ...(credential.keyId !== undefined ? { ':keyId': credential.keyId } : {}), ...(credential.keyPrefix !== undefined ? { ':keyPrefix': credential.keyPrefix } : {}) },
    } });
  }

  async tenantCredential(tenantId: string): Promise<TenantCredential | undefined> {
    assertPresent(tenantId);
    const output = await this.#client.send<{ readonly Item?: Record<string, unknown> }>({ operation: 'get', input: { TableName: this.#tenantCredentialsTable, Key: { tenant_id: tenantId }, ConsistentRead: true } });
    const apiKey = asString(output.Item?.qurl_api_key);
    if (!apiKey) return undefined;
    const decryptedApiKey = this.#credentialCipher
      ? await this.#credentialCipher.decrypt(tenantId, apiKey)
      : apiKey;
    const keyId = asString(output.Item?.qurl_key_id);
    const keyPrefix = asString(output.Item?.qurl_key_prefix);
    return { apiKey: decryptedApiKey, ...(keyId ? { keyId } : {}), ...(keyPrefix ? { keyPrefix } : {}) };
  }

  async savePersonalConversationRef(tenantId: string, actorAadObjectId: string, ref: PersonalConversationRef): Promise<void> {
    assertPresent(tenantId, actorAadObjectId, ref.serviceUrl, ref.conversationId);
    await this.#client.send({ operation: 'put', input: { TableName: this.#personalConversationsTable, Item: { ...personalDdbKey(tenantId, actorAadObjectId), service_url: ref.serviceUrl, conversation_id: ref.conversationId, updated_at: nowIso(this.#now) } } });
  }

  async personalConversationRef(tenantId: string, actorAadObjectId: string): Promise<PersonalConversationRef | undefined> {
    assertPresent(tenantId, actorAadObjectId);
    const output = await this.#client.send<{ readonly Item?: Record<string, unknown> }>({ operation: 'get', input: { TableName: this.#personalConversationsTable, Key: personalDdbKey(tenantId, actorAadObjectId), ConsistentRead: true } });
    const serviceUrl = asString(output.Item?.service_url);
    const conversationId = asString(output.Item?.conversation_id);
    return serviceUrl && conversationId ? { serviceUrl, conversationId } : undefined;
  }

  async allowedResourceIds(tenantId: string, scopeId: string): Promise<ReadonlySet<string>> {
    assertPresent(tenantId, scopeId);
    const items = await this.#queryTenant(this.#channelPoliciesTable, tenantId, { sortKeyName: policyKey, sortKeyPrefix: scopePolicyPrefix(scopeId) });
    return new Set(items.filter(item => item.item_type === 'resource' || item.item_type === 'alias')
      .map(item => asString(item.resource_id)).filter((id): id is string => id !== undefined));
  }

  async bindScopeAlias(tenantId: string, scopeId: string, alias: string, resourceId: string): Promise<void> {
    assertPresent(tenantId, scopeId, alias, resourceId);
    try {
      await this.#client.send({ operation: 'put', input: { TableName: this.#channelPoliciesTable, Item: this.#policyItem(tenantId, scopeId, 'alias', alias, resourceId), ConditionExpression: `attribute_not_exists(${tenantKey}) AND attribute_not_exists(${policyKey})` } });
    } catch (error) {
      if (!isConditionalCheckFailed(error)) throw error;
      // A retry of the same operation is safe, but an existing alias must
      // never be silently reassigned to a different resource.
      const existingResourceId = await this.lookupScopeAlias(tenantId, scopeId, alias);
      if (existingResourceId === resourceId) return;
      throw new ScopeAliasConflictError(alias, existingResourceId);
    }
  }

  async unbindScopeAlias(tenantId: string, scopeId: string, alias: string): Promise<void> {
    assertPresent(tenantId, scopeId, alias);
    try {
      await this.#client.send({ operation: 'delete', input: { TableName: this.#channelPoliciesTable, Key: policyDdbKey(tenantId, aliasPolicyKey(scopeId, alias)), ConditionExpression: `attribute_exists(${policyKey})` } });
    } catch (error) {
      if (!isConditionalCheckFailed(error)) throw error;
    }
  }

  async lookupScopeAlias(tenantId: string, scopeId: string, alias: string): Promise<string | undefined> {
    assertPresent(tenantId, scopeId, alias);
    const output = await this.#client.send<{ readonly Item?: Record<string, unknown> }>({ operation: 'get', input: { TableName: this.#channelPoliciesTable, Key: policyDdbKey(tenantId, aliasPolicyKey(scopeId, alias)), ConsistentRead: true } });
    return asString(output.Item?.resource_id);
  }

  async scopeAliases(tenantId: string, scopeId: string): Promise<readonly PolicyEntry[]> {
    assertPresent(tenantId, scopeId);
    const items = await this.#queryTenant(this.#channelPoliciesTable, tenantId, { sortKeyName: policyKey, sortKeyPrefix: scopePolicyPrefix(scopeId) });
    return items.filter(item => item.item_type === 'alias')
      .map(item => ({ scopeId, alias: asString(item.alias) ?? '', resourceId: asString(item.resource_id) ?? '' }))
      .filter(item => item.alias !== '' && item.resourceId !== '').sort((a, b) => a.alias.localeCompare(b.alias));
  }

  async exposeResource(tenantId: string, scopeId: string, resourceId: string): Promise<void> {
    assertPresent(tenantId, scopeId, resourceId);
    try {
      await this.#client.send({ operation: 'put', input: { TableName: this.#channelPoliciesTable, Item: this.#policyItem(tenantId, scopeId, 'resource', resourceId, resourceId), ConditionExpression: `attribute_not_exists(${tenantKey}) AND attribute_not_exists(${policyKey})` } });
    } catch (error) {
      if (!isConditionalCheckFailed(error)) throw error;
    }
  }

  async purgeResourceFromScope(tenantId: string, scopeId: string, resourceId: string, aliases: readonly string[] = []): Promise<void> {
    assertPresent(tenantId, scopeId, resourceId);
    await this.#delete(this.#channelPoliciesTable, policyDdbKey(tenantId, resourcePolicyKey(scopeId, resourceId)));
    for (const alias of aliases) {
      assertPresent(alias);
      await this.#delete(this.#channelPoliciesTable, policyDdbKey(tenantId, aliasPolicyKey(scopeId, alias)));
    }
  }

  #policyItem(tenantId: string, scopeId: string, itemType: 'resource' | 'alias', value: string, resourceId: string): Record<string, unknown> {
    return {
      teams_tenant_id: tenantId,
      policy_key: itemType === 'resource' ? resourcePolicyKey(scopeId, resourceId) : aliasPolicyKey(scopeId, value),
      scope_id: scopeId,
      item_type: itemType,
      resource_id: resourceId,
      tenant_resource_key: resourceIndexKey(tenantId, resourceId),
      scope_item_type_key: scopeIndexKey(scopeId, itemType, value),
      ...(itemType === 'alias' ? { alias: value } : {}),
      updated_at: nowIso(this.#now),
    };
  }

  #queryTenant(tableName: string, tenantId: string, sortKey?: { readonly sortKeyName: string; readonly sortKeyPrefix: string }): Promise<readonly Record<string, unknown>[]> {
    return this.#queryAll({
      TableName: tableName,
      KeyConditionExpression: `${tenantKey} = :tenant${sortKey === undefined ? '' : ` AND begins_with(${sortKey.sortKeyName}, :policyPrefix)`}`,
      ExpressionAttributeValues: { ':tenant': tenantId, ...(sortKey === undefined ? {} : { ':policyPrefix': sortKey.sortKeyPrefix }) },
    });
  }

  async #queryAll(input: Record<string, unknown>): Promise<readonly Record<string, unknown>[]> {
    const items: Record<string, unknown>[] = [];
    let exclusiveStartKey: Record<string, unknown> | undefined;
    do {
      const output = await this.#client.send<{
        readonly Items?: readonly Record<string, unknown>[];
        readonly LastEvaluatedKey?: Record<string, unknown>;
      }>({ operation: 'query', input: { ...input, ...(exclusiveStartKey ? { ExclusiveStartKey: exclusiveStartKey } : {}) } });
      items.push(...(output.Items ?? []));
      exclusiveStartKey = output.LastEvaluatedKey;
    } while (exclusiveStartKey && Object.keys(exclusiveStartKey).length > 0);
    return items;
  }

  // Unconditional: a delete of an absent key succeeds in DynamoDB, which is
  // what makes the teardown paths above safe to rerun after a partial failure.
  async #delete(tableName: string, key: Record<string, string>): Promise<void> {
    await this.#client.send({ operation: 'delete', input: { TableName: tableName, Key: key } });
  }
}
