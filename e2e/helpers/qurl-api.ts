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
): Promise<{ resource_id: string; qurl_link: string }> {
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
    if (data.resource_id && data.qurl_link) {
      return { resource_id: data.resource_id, qurl_link: data.qurl_link };
    }

    const errStr = typeof data.error === 'string' ? data.error : '';
    if (/429|rate.limit|too many requests/i.test(errStr)) {
      await new Promise((r) => setTimeout(r, baseDelayMs * (i + 1)));
      continue;
    }
    throw new Error(`Upload returned no resource_id/qurl_link: ${JSON.stringify(data)}`);
  }
  throw new Error(`uploadFile: still rate-limited after ${maxAttempts} attempts`);
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

/** Get link status */
export async function getLinkStatus(
  mintUrl: string,
  apiKey: string,
  resourceId: string,
): Promise<{ use_count: number; status: string; expires_at: string }> {
  const res = await fetch(`${mintUrl}/${resourceId}/status`, {
    headers: { Authorization: `Bearer ${apiKey}` },
  });
  if (!res.ok) throw new Error(`Status check failed: ${res.status}`);
  return res.json() as any;
}
