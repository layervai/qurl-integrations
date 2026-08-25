import { sha256Hex } from './encoding.js';

export interface QurlResource {
  readonly resourceId: string;
  readonly type?: string;
  readonly slug?: string;
  readonly alias?: string;
  readonly description?: string;
  readonly targetUrl?: string;
  readonly status?: string;
}

export interface QurlPage { readonly resources: readonly QurlResource[]; readonly nextCursor?: string; readonly hasMore?: boolean; }
export interface QurlCreateInput {
  readonly resourceId?: string;
  readonly targetUrl?: string;
  readonly label?: string;
  readonly expiresIn?: string;
  readonly oneTimeUse?: boolean;
  readonly maxSessions?: number;
  readonly sessionDuration?: string;
  readonly idempotencyKey?: string;
}
export interface QurlCreateOutput { readonly resourceId: string; readonly qurlLink: string; readonly expiresAt?: string; }
export interface QurlApiKey { readonly keyId: string; readonly apiKey: string; }
export interface QurlClient {
  listResources(signal?: AbortSignal, cursor?: string): Promise<QurlPage>;
  create(input: QurlCreateInput, signal?: AbortSignal): Promise<QurlCreateOutput>;
  createResource(input: { readonly targetUrl?: string; readonly type: string; readonly slug?: string; readonly findOrCreate?: boolean; readonly idempotencyKey?: string }, signal?: AbortSignal): Promise<QurlResource>;
  updateResource(resourceId: string, description: string, signal?: AbortSignal): Promise<void>;
  deleteResource(resourceId: string, signal?: AbortSignal): Promise<void>;
  createEnrollmentToken(slug: string, idempotencyKey: string, signal?: AbortSignal): Promise<QurlApiKey>;
  revokeApiKey(keyId: string, signal?: AbortSignal): Promise<void>;
}
export interface QurlClientOptions { readonly endpoint: string; readonly apiKey: string; readonly fetch?: typeof fetch; readonly userAgent?: string; }
const QURL_REQUEST_TIMEOUT_MS = 15_000;
const QURL_RESPONSE_LIMIT_BYTES = 1_048_576;
type JsonObject = Record<string, unknown>;

function object(value: unknown, label: string): JsonObject {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error(`${label} response is invalid`);
  return value as JsonObject;
}
function requiredString(value: unknown, label: string): string {
  if (typeof value !== 'string' || value === '') throw new Error(`${label} response is invalid`);
  return value;
}
function optionalString(value: unknown): string | undefined { return typeof value === 'string' && value !== '' ? value : undefined; }
function responseData(value: unknown, label: string): JsonObject { const envelope = object(value, label); return object(envelope.data ?? envelope, label); }
function resourceFromWire(value: unknown): QurlResource {
  const resource = object(value, 'qURL resource');
  const type = optionalString(resource.type); const slug = optionalString(resource.slug); const alias = optionalString(resource.alias);
  const description = optionalString(resource.description); const targetUrl = optionalString(resource.target_url); const status = optionalString(resource.status);
  return { resourceId: requiredString(resource.resource_id, 'qURL resource'), ...(type ? { type } : {}), ...(slug ? { slug } : {}), ...(alias ? { alias } : {}), ...(description ? { description } : {}), ...(targetUrl ? { targetUrl } : {}), ...(status ? { status } : {}) };
}
function createOutputFromWire(value: unknown): QurlCreateOutput {
  const output = responseData(value, 'qURL create'); const expiresAt = optionalString(output.expires_at);
  return { resourceId: requiredString(output.resource_id, 'qURL create'), qurlLink: requiredString(output.qurl_link, 'qURL create'), ...(expiresAt ? { expiresAt } : {}) };
}
function apiKeyFromWire(value: unknown): QurlApiKey {
  const key = responseData(value, 'qURL API key');
  return { keyId: requiredString(key.key_id, 'qURL API key'), apiKey: requiredString(key.api_key, 'qURL API key') };
}
function pageFromWire(value: unknown): QurlPage {
  if (Array.isArray(value)) return { resources: value.map(resourceFromWire) };
  const envelope = object(value, 'qURL resource list'); const rows = envelope.data ?? envelope.resources;
  if (!Array.isArray(rows)) throw new Error('qURL resource list response is invalid');
  const meta = envelope.meta === undefined ? undefined : object(envelope.meta, 'qURL resource list metadata');
  const nextCursor = typeof meta?.next_cursor === 'string' && meta.next_cursor !== '' ? meta.next_cursor : undefined;
  const hasMore = typeof meta?.has_more === 'boolean' ? meta.has_more : undefined;
  return { resources: rows.map(resourceFromWire), ...(nextCursor ? { nextCursor } : {}), ...(hasMore === undefined ? {} : { hasMore }) };
}
function requestBody(input: QurlCreateInput): JsonObject {
  return { ...(input.label ? { label: input.label } : {}), ...(input.expiresIn ? { expires_in: input.expiresIn } : {}), ...(input.oneTimeUse === undefined ? {} : { one_time_use: input.oneTimeUse }), ...(input.maxSessions === undefined ? {} : { max_sessions: input.maxSessions }), ...(input.sessionDuration ? { session_duration: input.sessionDuration } : {}) };
}

export class HttpQurlClient implements QurlClient {
  readonly #endpoint: URL; readonly #apiKey: string; readonly #fetch: typeof fetch; readonly #userAgent: string;
  constructor(options: QurlClientOptions) {
    this.#endpoint = new URL(options.endpoint.endsWith('/') ? options.endpoint : `${options.endpoint}/`);
    if (this.#endpoint.protocol !== 'https:' || this.#endpoint.username || this.#endpoint.password || this.#endpoint.pathname !== '/' || this.#endpoint.search || this.#endpoint.hash) throw new Error('qURL endpoint must be an HTTPS origin without credentials');
    if (!options.apiKey.trim()) throw new Error('qURL API key is required');
    this.#apiKey = options.apiKey; this.#fetch = options.fetch ?? fetch; this.#userAgent = options.userAgent ?? 'qurl-teams/unknown';
  }
  async listResources(signal?: AbortSignal, cursor?: string): Promise<QurlPage> {
    const url = new URL('v1/resources', this.#endpoint); if (cursor) url.searchParams.set('cursor', cursor);
    return pageFromWire(await this.#request(url, signal ? { signal } : {}));
  }
  async create(input: QurlCreateInput, signal?: AbortSignal): Promise<QurlCreateOutput> {
    if (input.resourceId && input.targetUrl) throw new Error('qURL create target and resource are mutually exclusive');
    if (!input.resourceId && !input.targetUrl) throw new Error('qURL create requires a target or resource');
    const body = requestBody(input); const url = input.resourceId ? new URL(`v1/resources/${encodeURIComponent(input.resourceId)}/qurls`, this.#endpoint) : new URL('v1/qurls', this.#endpoint);
    if (input.targetUrl) body.target_url = input.targetUrl;
    return createOutputFromWire(await this.#request(url, { method: 'POST', body: JSON.stringify(body), ...(signal ? { signal } : {}), ...(input.idempotencyKey ? { idempotencyKey: input.idempotencyKey } : {}) }));
  }
  async createResource(input: { readonly targetUrl?: string; readonly type: string; readonly slug?: string; readonly findOrCreate?: boolean; readonly idempotencyKey?: string }, signal?: AbortSignal): Promise<QurlResource> {
    const { idempotencyKey: requestKey, ...resource } = input;
    const body = { ...(resource.targetUrl ? { target_url: resource.targetUrl } : {}), type: resource.type, ...(resource.slug ? { slug: resource.slug } : {}), ...(resource.findOrCreate === undefined ? {} : { find_or_create: resource.findOrCreate }) };
    return resourceFromWire(responseData(await this.#request(new URL('v1/resources', this.#endpoint), { method: 'POST', body: JSON.stringify(body), ...(signal ? { signal } : {}), ...(requestKey ? { idempotencyKey: requestKey } : {}) }), 'qURL resource'));
  }
  async updateResource(resourceId: string, description: string, signal?: AbortSignal): Promise<void> { await this.#request(new URL(`v1/resources/${encodeURIComponent(resourceId)}`, this.#endpoint), { method: 'PATCH', body: JSON.stringify({ description }), ...(signal ? { signal } : {}) }); }
  async deleteResource(resourceId: string, signal?: AbortSignal): Promise<void> {
    await this.#request(new URL(`v1/resources/${encodeURIComponent(resourceId)}`, this.#endpoint), {
      method: 'DELETE',
      ignoreNotFound: true,
      ...(signal ? { signal } : {}),
    });
  }
  async createEnrollmentToken(slug: string, idempotencyKey: string, signal?: AbortSignal): Promise<QurlApiKey> {
    return apiKeyFromWire(await this.#request(new URL('v1/api-keys', this.#endpoint), { method: 'POST', body: JSON.stringify({ name: `Teams connector ${slug}`, kind: 'enrollment_token', target: 'connector', claims: [{ type: 'connector', id: slug }], expires_in: '15m' }), ...(signal ? { signal } : {}), idempotencyKey }));
  }
  async revokeApiKey(keyId: string, signal?: AbortSignal): Promise<void> {
    await this.#request(new URL(`v1/api-keys/${encodeURIComponent(keyId)}`, this.#endpoint), {
      method: 'DELETE',
      ignoreNotFound: true,
      ...(signal ? { signal } : {}),
    });
  }
  async #request(url: URL, options: { readonly method?: string; readonly body?: string; readonly signal?: AbortSignal; readonly idempotencyKey?: string; readonly ignoreNotFound?: boolean }): Promise<unknown> {
    if (options.signal?.aborted) throw new Error('qURL request timed out or was cancelled');
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), QURL_REQUEST_TIMEOUT_MS);
    const abort = () => controller.abort();
    options.signal?.addEventListener('abort', abort, { once: true });
    let response: Response;
    try {
      response = await this.#fetch(url, { method: options.method ?? 'GET', headers: { Accept: 'application/json', ...(options.body === undefined ? {} : { 'Content-Type': 'application/json' }), Authorization: `Bearer ${this.#apiKey}`, 'User-Agent': this.#userAgent, ...(options.idempotencyKey ? { 'Idempotency-Key': options.idempotencyKey } : {}) }, ...(options.body === undefined ? {} : { body: options.body }), signal: controller.signal, redirect: 'error' });
      const text = await boundedText(response);
      if (!response.ok && !(options.ignoreNotFound && response.status === 404)) throw new Error(`qURL request failed (${response.status})`); if (!text) return undefined;
      try { return JSON.parse(text) as unknown; } catch { throw new Error('qURL response is invalid JSON'); }
    } catch (error) {
      if (controller.signal.aborted) throw new Error('qURL request timed out or was cancelled', { cause: error });
      throw error;
    } finally {
      clearTimeout(timeout);
      options.signal?.removeEventListener('abort', abort);
    }
  }
}

async function boundedText(response: Response): Promise<string> {
  if (!response.body) return '';
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    while (true) {
      const chunk = await reader.read();
      if (chunk.done) break;
      total += chunk.value.byteLength;
      if (total > QURL_RESPONSE_LIMIT_BYTES) {
        await reader.cancel().catch(() => undefined);
        throw new Error('qURL response exceeded the configured size limit');
      }
      chunks.push(chunk.value);
    }
  } finally {
    reader.releaseLock();
  }
  const body = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) { body.set(chunk, offset); offset += chunk.byteLength; }
  return new TextDecoder('utf-8', { fatal: true }).decode(body);
}

export function idempotencyKey(...fields: string[]): string { return sha256Hex(fields.map(value => `${value.length}:${value}`).join('|')); }
