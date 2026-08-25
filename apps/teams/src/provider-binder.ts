import { sha256Hex } from './encoding.js';
import { decodeUtf8, readBoundedBody } from './http.js';
import type { FetchLike, ProviderBinder, ProviderBindingRequest, ProviderBindingResult } from './interfaces.js';
import type { TeamsDataStore } from './teams-data.js';

const BINDING_PATH = 'v1/external-identity-bindings';
const BODY_LIMIT = 16 * 1024;

export interface HttpProviderBinderOptions {
  readonly endpoint: string;
  readonly data: TeamsDataStore;
  readonly fetch?: FetchLike;
  readonly timeoutMs?: number;
}

interface BindingResponse {
  readonly api_key?: { readonly plaintext?: unknown; readonly key_id?: unknown; readonly key_prefix?: unknown };
  readonly data?: { readonly api_key?: { readonly plaintext?: unknown; readonly key_id?: unknown; readonly key_prefix?: unknown } };
}

function bindingConflict(error: unknown): boolean {
  return error instanceof Error && error.name === 'ConditionalCheckFailedException';
}

function parseBinding(body: Uint8Array): { readonly apiKey: string; readonly keyId: string; readonly keyPrefix?: string } {
  let parsed: unknown;
  try { parsed = JSON.parse(decodeUtf8(body)); } catch { throw new Error('qURL binding response is invalid'); }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('qURL binding response is invalid');
  const response = parsed as BindingResponse;
  const key = response.api_key ?? response.data?.api_key;
  if (!key || typeof key.plaintext !== 'string' || !key.plaintext || typeof key.key_id !== 'string' || !key.key_id) throw new Error('qURL binding response has no complete credential');
  return {
    apiKey: key.plaintext,
    keyId: key.key_id,
    ...(typeof key.key_prefix === 'string' && key.key_prefix ? { keyPrefix: key.key_prefix } : {}),
  };
}

/** Provisions qURL's tenant binding and stores the returned tenant credential. */
export class HttpProviderBinder implements ProviderBinder {
  readonly #endpoint: URL;
  readonly #data: TeamsDataStore;
  readonly #fetch: FetchLike;
  readonly #timeoutMs: number;

  constructor(options: HttpProviderBinderOptions) {
    this.#endpoint = new URL(options.endpoint.endsWith('/') ? options.endpoint : `${options.endpoint}/`);
    if (this.#endpoint.protocol !== 'https:' || this.#endpoint.username || this.#endpoint.password || this.#endpoint.pathname !== '/' || this.#endpoint.search || this.#endpoint.hash) throw new Error('qURL endpoint must be an HTTPS origin');
    this.#data = options.data;
    this.#fetch = options.fetch ?? fetch;
    this.#timeoutMs = options.timeoutMs ?? 15_000;
    if (!Number.isSafeInteger(this.#timeoutMs) || this.#timeoutMs < 1) throw new Error('qURL binding timeout is invalid');
  }

  async bind(request: ProviderBindingRequest): Promise<ProviderBindingResult> {
    const existing = await this.#data.checkAdmin(request.teamsTenantId, request.actorAadObjectId);
    if (existing.ownerId !== undefined && existing.ownerId !== request.actorAadObjectId) return { status: 'conflict', reason: 'tenant_bound_to_another_account' };
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), this.#timeoutMs);
    let response: Response;
    try {
      response = await this.#fetch(new URL(BINDING_PATH, this.#endpoint), {
        method: 'POST',
        headers: {
          Accept: 'application/json',
          'Content-Type': 'application/json',
          Authorization: `Bearer ${request.accessToken}`,
          'Idempotency-Key': `teams-tenant-binding-v1-${sha256Hex(request.teamsTenantId)}`,
        },
        body: JSON.stringify({ provider: 'teams', external_id: request.teamsTenantId, display_name: `Teams tenant ${request.teamsTenantId}` }),
        redirect: 'error',
        signal: controller.signal,
      });
    } catch (error) {
      clearTimeout(timeout);
      throw new Error('qURL tenant binding request failed', { cause: error });
    }
    let body: Uint8Array;
    try {
      body = await readBoundedBody(response, BODY_LIMIT, 'TOKEN_RESPONSE_TOO_LARGE');
    } finally {
      clearTimeout(timeout);
    }
    if (response.status === 409) {
      const result = await this.existingBindingResult(request);
      if (result.status === 'already_bound' && !(await this.#data.tenantCredential(request.teamsTenantId))) {
        throw new Error('qURL tenant binding exists but its credential is unavailable');
      }
      return result;
    }
    if (response.status === 401 || response.status === 403) return { status: 'conflict', reason: 'actor_not_authorized' };
    if (!response.ok) throw new Error('qURL tenant binding request was rejected');
    const credential = parseBinding(body);
    try {
      await this.#data.bindWorkspace({ tenantId: request.teamsTenantId, ownerId: request.actorAadObjectId }, request.actorAadObjectId);
    } catch (error) {
      if (!bindingConflict(error)) throw error;
      const current = await this.#data.checkAdmin(request.teamsTenantId, request.actorAadObjectId);
      if (current.ownerId !== request.actorAadObjectId) return { status: 'conflict', reason: 'tenant_bound_to_another_account' };
      await this.#data.saveTenantCredential(request.teamsTenantId, credential);
      return { status: 'already_bound' };
    }
    await this.#data.saveTenantCredential(request.teamsTenantId, credential);
    return credential.keyId === undefined
      ? { status: 'bound' }
      : { status: 'bound', bindingReference: credential.keyId };
  }

  async existingBindingResult(request: ProviderBindingRequest): Promise<ProviderBindingResult> {
    const current = await this.#data.checkAdmin(request.teamsTenantId, request.actorAadObjectId);
    if (current.ownerId === request.actorAadObjectId) return { status: 'already_bound' };
    return { status: 'conflict', reason: 'tenant_bound_to_another_account' };
  }
}
