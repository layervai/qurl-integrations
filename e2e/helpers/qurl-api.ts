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

/** Mint a one-time qURL link for a resource */
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
  const res = await fetch(mintUrl, {
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
  const json = await res.json() as any;
  const d = json.data ?? json;
  return {
    resource_id: d.resource_id,
    qurl_link: d.qurl_link ?? d.link ?? d.url,
    qurl_id: d.qurl_id ?? d.id,
  };
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
  use_count: number;
  status: string;
  expires_at: string;
}

/** Get link status */
export async function getLinkStatus(
  mintUrl: string,
  apiKey: string,
  resourceId: string,
): Promise<LinkStatus> {
  const res = await fetch(`${mintUrl}/${resourceId}/status`, {
    headers: { Authorization: `Bearer ${apiKey}` },
  });
  if (!res.ok) throw new Error(`Status check failed: ${res.status}`);
  return res.json() as any;
}

/** Like getLinkStatus, but the 404 shape — "resource fully consumed or
 * revoked" — resolves to null instead of throwing. Any OTHER failure
 * (auth, network, 5xx) still throws, so tests can assert the valid
 * post-consumption shapes without swallowing unrelated errors (the
 * try/catch-around-expect anti-pattern this replaces). */
export async function getLinkStatusOrNull(
  mintUrl: string,
  apiKey: string,
  resourceId: string,
): Promise<LinkStatus | null> {
  try {
    return await getLinkStatus(mintUrl, apiKey, resourceId);
  } catch (e) {
    if (/\b404\b|\bnot found\b/i.test((e as Error).message)) return null;
    throw e;
  }
}
