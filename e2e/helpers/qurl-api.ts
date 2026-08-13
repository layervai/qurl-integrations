/**
 * qURL service + connector HTTP API client for E2E testing.
 * Drives file upload, link minting, link access, and revocation.
 */

import * as fs from 'fs';
import * as path from 'path';

import { fetchWithTransientRetry } from './http';

export interface MintResult {
  resource_id: string;
  qurl_link: string;
  qurl_id: string;
}

export interface LinkAccessResult {
  status: number;
  finalUrl: string;
  ok: boolean;
  body?: string;
}

/** Upload a file to the connector and get back its resource_id + qurl_link.
 * Accepts a path (read from disk) OR in-memory bytes — the connector's upload is
 * content-addressed, so a test fixture buffer works the same as a file.
 *
 * `viewerTtlSeconds` forwards as `session_duration` (the field #283 pins).
 * Handles the connector's documented 200-with-error-body convention: a transient
 * 429 (HTTP 200 + `error` string + no `resource_id`) is retried with backoff; any
 * other missing-`resource_id` body throws WITH the connector's `error` string so a
 * real regression is legible, not a bare "expected undefined to be truthy".
 *
 * The HTTP call goes through fetchWithTransientRetry (bounded retry on transient
 * connector-stack statuses; see its header + qurl-integrations-infra#1085) —
 * distinct from the app-level 429 (HTTP 200 + `error`) loop below. */
export async function uploadFile(
  uploadUrl: string,
  file: string | { bytes: Uint8Array; filename: string; mime?: string },
  apiKey: string,
  opts?: { viewerTtlSeconds?: number },
): Promise<{ resource_id: string; qurl_link?: string }> {
  const fileBuffer: Uint8Array = typeof file === 'string' ? fs.readFileSync(file) : file.bytes;
  const fileName = typeof file === 'string' ? path.basename(file) : file.filename;
  const mime = typeof file === 'string' ? undefined : file.mime;
  const maxAttempts = 4;
  const baseDelayMs = 1500;

  for (let i = 0; i < maxAttempts; i++) {
    const formData = new FormData();
    formData.append('file', new Blob([fileBuffer as BlobPart], mime ? { type: mime } : undefined), fileName);
    if (opts?.viewerTtlSeconds !== undefined) {
      formData.append('viewer_ttl_seconds', String(opts.viewerTtlSeconds));
    }

    const res = await fetchWithTransientRetry(`${uploadUrl}/upload`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${apiKey}` },
      body: formData,
    });

    if (!res.ok) throw new Error(`Upload failed: ${res.status} ${await res.text()}`);
    const data = (await res.json()) as { resource_id?: string; qurl_link?: string; error?: string };
    // Success signal is `resource_id` alone — the connector's documented marker
    // that the upload AND the mint succeeded. A mint failure returns HTTP 200 +
    // success:true + an `error` string + NO resource_id (the 2026-05-13 shape),
    // which falls through to the throw below. Deliberately do NOT also gate on
    // `qurl_link`: the sibling viewer-ttl smoke asserts only resource_id, so a
    // link requirement here would couple that test to a resource_id-without-link
    // response it never contracts on. Callers needing the link assert it
    // themselves (file-revoke).
    if (data.resource_id) {
      return { resource_id: data.resource_id, qurl_link: data.qurl_link };
    }

    const errStr = typeof data.error === 'string' ? data.error : '';
    if (/429|rate.limit|too many requests/i.test(errStr)) {
      await new Promise((r) => setTimeout(r, baseDelayMs * (i + 1)));
      continue;
    }
    throw new Error(`Upload returned no resource_id: ${JSON.stringify(data)}`);
  }
  throw new Error(`uploadFile: still rate-limited after ${maxAttempts} attempts`);
}

/** Mint a render-at-mint per-recipient VIEW link for an already-uploaded
 * resource, via the connector's `POST /api/mint_link/:resource_id`.
 *
 * This is the recipient path the fileviewer tunnel actually serves: in
 * render-at-mint mode the connector bakes a per-recipient `views/<mint-id>`
 * object against the fileviewer tunnel and returns a one-time `qurl.link` whose
 * NHP knock resolves to `r_<tunnel>.qurl.site/views/<mint-id>` (200). The
 * `qurl_link` returned by `/upload` is a DIFFERENT thing — the connector's
 * "never-shared per-upload qURL" (handler.go: it targets `/resources/<md5>`, so
 * its knock 404s) — so a tunnel view test MUST mint here, not reuse the upload
 * link. Distinct from `mintLink` below: that hits the qurl-service mint API
 * (`MINT_API_URL`); this hits the CONNECTOR (`uploadUrl` = the `/api` base).
 *
 * `expiresAt` is REQUIRED by render-at-mint (RFC3339; the connector clamps to
 * its max-expiry cap rather than rejecting an over-cap value).
 *
 * Sends `n: 1` (mint exactly one link → read `links[0]`). The POST goes through
 * `fetchWithTransientRetry`, which is safe here despite being non-idempotent:
 * per the mint handler a 503 comes ONLY from the pre-bake "render-at-mint not
 * ready" guard (so a retry can't double-bake a `views/<mint-id>` object),
 * post-bake failures surface as 502 (which the helper does NOT retry for a
 * POST), and a rate-limited mint is HTTP 429 (which it backs off on) — so,
 * unlike `uploadFile`, no separate 200+`error` rate-limit loop is needed.
 *
 * `oneTimeUse` defaults true — single-view, matching `viewViaQurlLink`'s one
 * knock+navigation per call. */
export async function mintConnectorView(
  uploadUrl: string,
  resourceId: string,
  apiKey: string,
  opts: { expiresAt: string; oneTimeUse?: boolean },
): Promise<{ qurl_link: string }> {
  const res = await fetchWithTransientRetry(`${uploadUrl}/mint_link/${encodeURIComponent(resourceId)}`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${apiKey}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ n: 1, expires_at: opts.expiresAt, one_time_use: opts.oneTimeUse ?? true }),
  });
  if (!res.ok) throw new Error(`mint_link failed: ${res.status} ${await res.text()}`);
  const data = (await res.json()) as {
    error?: string;
    links?: Array<{ qurl_id: string; qurl_link: string; expires_at: string }>;
  };
  // Gate on the link itself, not a `success` flag: a non-200 already threw above
  // (`!res.ok`), and a 200 always carries `links[]`, so a present `qurl_link` is
  // the authoritative signal (mirrors uploadFile keying on `resource_id`).
  const link = data.links?.[0];
  if (!link?.qurl_link) {
    throw new Error(`mint_link returned no link: ${JSON.stringify(data)}`);
  }
  // Return only the field this helper guarantees (and the caller uses); the
  // response also carries qurl_id/expires_at, but only qurl_link is guard-checked.
  return { qurl_link: link.qurl_link };
}

/** Mint a one-time qURL link for a resource.
 * This non-idempotent POST deliberately uses one bare fetch: after an
 * ambiguous transport failure the helper cannot know whether a link was
 * created, so replaying the request could mint an unintended extra qURL. */
export async function mintLink(
  mintUrl: string,
  apiKey: string,
  opts: {
    target_url?: string;
    resource_id?: string;
    expires_in?: string;
    description?: string;
    max_uses?: number;
  },
): Promise<MintResult> {
  const res = await fetch(stripTrailingSlashes(mintUrl), {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${apiKey}`,
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({
      target_url: opts.target_url,
      resource_id: opts.resource_id,
      expires_in: opts.expires_in ?? '1h',
      description: opts.description ?? 'E2E test link',
      max_uses: opts.max_uses ?? 1,
    }),
  });

  if (!res.ok) throw new Error(`Mint failed: ${res.status} ${await res.text()}`);
  const body = await res.json() as unknown;
  const result = parseBareOrEnveloped(body, parseMintResult);
  if (!result) throw new Error('Mint returned an invalid response shape');
  return result;
}

/** Access a qURL link and return the HTTP result */
export async function accessLink(url: string): Promise<LinkAccessResult> {
  const res = await fetch(url, { redirect: 'follow' });
  return {
    status: res.status,
    finalUrl: res.url,
    ok: res.ok,
    body: await res.text().catch(() => undefined),
  };
}

/** Access a qURL link without following redirects */
export async function accessLinkNoRedirect(url: string): Promise<LinkAccessResult> {
  const res = await fetch(url, { redirect: 'manual' });
  return {
    status: res.status,
    finalUrl: res.headers.get('location') ?? res.url,
    ok: res.status >= 200 && res.status < 400,
  };
}

/** Revoke a qURL link by resource_id (revokes entire resource) */
export async function revokeLink(
  baseUrl: string,
  apiKey: string,
  resourceId: string,
): Promise<boolean> {
  // API: DELETE /v1/resources/{resource_id}
  const parsed = new URL(baseUrl);
  parsed.pathname = `/v1/resources/${encodeURIComponent(resourceId)}`;
  const url = parsed.toString();
  const res = await fetch(url, {
    method: 'DELETE',
    headers: { Authorization: `Bearer ${apiKey}` },
  });
  return res.ok;
}

export interface LinkStatus {
  qurl_id: string;
  use_count: number;
  status: string;
  expires_at?: string;
}

export interface ResourceStatus {
  resource_id: string;
  status: string;
  expires_at?: string;
  qurls?: LinkStatus[];
}

interface PollOptions {
  timeoutMs?: number;
  intervalMs?: number;
}

function isJsonObject(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function stripTrailingSlashes(url: string): string {
  return url.replace(/\/+$/, '');
}

function unwrapDataEnvelope(value: unknown): unknown {
  return isJsonObject(value) && 'data' in value ? value.data : value;
}

/** Prefer a complete bare response recognized by `parse`; otherwise parse the
 * API's `data` envelope. This preserves future unrelated top-level data fields.
 * A parser throw intentionally aborts: only an unrecognized (`null`) top-level
 * shape may fall through to the envelope. */
function parseBareOrEnveloped<T>(
  value: unknown,
  parse: (candidate: unknown) => T | null,
): T | null {
  return parse(value) ?? parse(unwrapDataEnvelope(value));
}

function parseMintResult(value: unknown): MintResult | null {
  if (
    !isJsonObject(value) ||
    typeof value.resource_id !== 'string' ||
    value.resource_id.length === 0
  ) {
    return null;
  }
  const qurlLink = value.qurl_link ?? value.link ?? value.url;
  const qurlId = value.qurl_id ?? value.id;
  if (typeof qurlLink !== 'string' || typeof qurlId !== 'string') return null;
  return { resource_id: value.resource_id, qurl_link: qurlLink, qurl_id: qurlId };
}

function parseLinkStatus(value: unknown, resourceId: string, index: number): LinkStatus {
  if (
    !isJsonObject(value) ||
    typeof value.qurl_id !== 'string' ||
    typeof value.use_count !== 'number' ||
    typeof value.status !== 'string' ||
    (value.expires_at !== undefined && typeof value.expires_at !== 'string')
  ) {
    throw new Error(
      `qURL lookup returned an invalid token status shape for resource ${resourceId} at qurls[${index}]`,
    );
  }
  return {
    qurl_id: value.qurl_id,
    use_count: value.use_count,
    status: value.status,
    ...(value.expires_at === undefined ? {} : { expires_at: value.expires_at }),
  };
}

function parseResourceStatus(value: unknown, id: string): ResourceStatus | null {
  if (
    !isJsonObject(value) ||
    typeof value.resource_id !== 'string' ||
    typeof value.status !== 'string'
  ) {
    return null;
  }
  if (value.expires_at !== undefined && typeof value.expires_at !== 'string') {
    throw new Error(`qURL lookup returned an invalid expires_at for resource ${id}`);
  }
  if (value.qurls !== undefined && !Array.isArray(value.qurls)) {
    throw new Error(`qURL lookup returned an invalid qURL preview for resource ${id}`);
  }

  const resourceId = value.resource_id;
  // Deliberately validate every returned summary even for resource-lifecycle
  // callers: a malformed management response must red the E2E gate rather than
  // let a revoke assertion pass while silently ignoring preview corruption.
  const qurls = value.qurls?.map((qurl, index) => (
    parseLinkStatus(qurl, resourceId, index)
  ));
  return {
    resource_id: resourceId,
    status: value.status,
    ...(value.expires_at === undefined ? {} : { expires_at: value.expires_at }),
    ...(qurls === undefined ? {} : { qurls }),
  };
}

/** A typed HTTP failure from the qURL management lookup. */
class StatusCheckError extends Error {
  constructor(readonly status: number) {
    super(`qURL lookup failed: ${status}`);
    this.name = 'StatusCheckError';
  }
}

/** The parent resource is readable, but its bounded qURL preview has not made
 * the requested token visible. Direct reads fail closed on this condition;
 * only bounded read-after-write polling may retry it. */
class TokenSummaryNotVisibleError extends Error {
  constructor(qurlId: string, resourceId: string) {
    super(
      `qURL lookup for ${qurlId} returned resource ${resourceId} without the requested token summary`,
    );
    this.name = 'TokenSummaryNotVisibleError';
  }
}

/** Read the documented resource-centric management endpoint. It accepts
 * either a public resource ID or a q_ display ID and returns the parent
 * resource. There is no `/status` sub-route: per-qURL status lives in the
 * response's `qurls` summaries. */
async function getQurlResource(
  managementUrl: string,
  apiKey: string,
  id: string,
): Promise<ResourceStatus> {
  const base = stripTrailingSlashes(managementUrl);
  const res = await fetchWithTransientRetry(`${base}/${encodeURIComponent(id)}`, {
    headers: { Authorization: `Bearer ${apiKey}` },
  });
  if (!res.ok) throw new StatusCheckError(res.status);

  const body = await res.json() as unknown;
  const resource = parseBareOrEnveloped(body, (value) => parseResourceStatus(value, id));
  if (!resource) throw new Error(`qURL lookup returned an invalid resource shape for ${id}`);
  return resource;
}

/** Get one qURL token's status from its parent resource response. The id must
 * be a qurl_id; use getResourceStatus for an opaque public resource_id.
 * TODO(upstream-contract): layervai/qurl-service#1233 tracks the bounded qURL
 * detail-preview and terminal-row retention contract.
 * The E2E fixtures nonce their target URLs, so each resource has a small qURL
 * set and the requested token must be present in the bounded detail preview.
 * Missing it is an ambiguous/invalid response, not a synthetic 404 pass. */
export async function getLinkStatus(
  managementUrl: string,
  apiKey: string,
  qurlId: string,
): Promise<LinkStatus> {
  // TODO(upstream-contract): layervai/qurl-service#1233 tracks whether `q_`
  // remains the stable discriminator between qurl_id and opaque resource_id.
  if (!qurlId.startsWith('q_')) {
    throw new TypeError('qURL token lookup requires a qurl_id; use getResourceStatus for resource IDs');
  }
  const resource = await getQurlResource(managementUrl, apiKey, qurlId);
  const status = resource.qurls?.find((qurl) => qurl.qurl_id === qurlId);
  if (!status) {
    throw new TokenSummaryNotVisibleError(qurlId, resource.resource_id);
  }
  return status;
}

/** Get resource-level lifecycle status by its opaque public resource ID. */
export async function getResourceStatus(
  managementUrl: string,
  apiKey: string,
  resourceId: string,
): Promise<ResourceStatus> {
  const resource = await getQurlResource(managementUrl, apiKey, resourceId);
  // Resource-level callers pass the canonical public resource_id, never a qURL
  // display ID; require the management response to echo that encoding exactly.
  if (resource.resource_id !== resourceId) {
    throw new Error(
      `qURL lookup for resource ${resourceId} returned mismatched resource ${resource.resource_id}`,
    );
  }
  return resource;
}

/** Map an actual management-API 404 to null. Any other HTTP, network, or
 * response-shape failure still throws. */
async function readStatusOrNull<T>(read: () => Promise<T>): Promise<T | null> {
  try {
    return await read();
  } catch (e) {
    if (e instanceof StatusCheckError && e.status === 404) return null;
    throw e;
  }
}

/** Turn a nullable lookup into an actionable canary failure. */
export function assertStatusVisible<T>(
  status: T | null,
  idDescription: string,
): asserts status is T {
  if (status === null) {
    throw new Error(
      `status canary: ${idDescription} did not become visible at GET /v1/qurls/{id} within the poll window`,
    );
  }
}

/** Bounded read-after-write poll shared by token and resource canaries. */
async function pollStatus<T>(
  read: () => Promise<T | null>,
  predicate: (status: T | null) => boolean,
  { timeoutMs = 5_000, intervalMs = 1_000 }: PollOptions = {},
): Promise<T | null> {
  const deadline = Date.now() + timeoutMs;
  let last = await read();
  while (!predicate(last) && Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, intervalMs));
    last = await read();
  }
  return last;
}

export async function pollLinkStatus(
  managementUrl: string,
  apiKey: string,
  qurlId: string,
  predicate: (status: LinkStatus | null) => boolean,
  options?: PollOptions,
): Promise<LinkStatus | null> {
  const readAfterWrite = async (): Promise<LinkStatus | null> => {
    try {
      return await readStatusOrNull(() => getLinkStatus(managementUrl, apiKey, qurlId));
    } catch (e) {
      // A freshly minted parent resource can become readable before its token
      // reaches the bounded preview. Retry that one explicit visibility lag;
      // invalid shapes, auth failures, and all other errors still abort.
      if (e instanceof TokenSummaryNotVisibleError) return null;
      throw e;
    }
  };
  return pollStatus(readAfterWrite, predicate, options);
}

export async function pollResourceStatus(
  managementUrl: string,
  apiKey: string,
  resourceId: string,
  predicate: (status: ResourceStatus | null) => boolean,
  options?: PollOptions,
): Promise<ResourceStatus | null> {
  return pollStatus(
    () => readStatusOrNull(() => getResourceStatus(managementUrl, apiKey, resourceId)),
    predicate,
    options,
  );
}
