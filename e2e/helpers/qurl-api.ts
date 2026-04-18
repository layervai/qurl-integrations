/**
 * QURL service + connector HTTP API client for E2E testing.
 * Drives file upload, link minting, link access, and revocation.
 */

import * as fs from 'fs';
import * as path from 'path';

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

/** Upload a file to the connector and get a resource_id */
export async function uploadFile(
  uploadUrl: string,
  filePath: string,
  apiKey: string,
): Promise<{ resource_id: string; hash: string }> {
  const fileBuffer = fs.readFileSync(filePath);
  const fileName = path.basename(filePath);

  const formData = new FormData();
  formData.append('file', new Blob([fileBuffer]), fileName);

  const res = await fetch(`${uploadUrl}/upload`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${apiKey}` },
    body: formData,
  });

  if (!res.ok) throw new Error(`Upload failed: ${res.status} ${await res.text()}`);
  return res.json() as Promise<{ resource_id: string; hash: string }>;
}

/** Mint a one-time QURL link for a resource */
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

/** Access a QURL link and return the HTTP result */
export async function accessLink(url: string): Promise<LinkAccessResult> {
  const res = await fetch(url, { redirect: 'follow' });
  return {
    status: res.status,
    finalUrl: res.url,
    ok: res.ok,
    body: await res.text().catch(() => undefined),
  };
}

/** Access a QURL link without following redirects */
export async function accessLinkNoRedirect(url: string): Promise<LinkAccessResult> {
  const res = await fetch(url, { redirect: 'manual' });
  return {
    status: res.status,
    finalUrl: res.headers.get('location') ?? res.url,
    ok: res.status >= 200 && res.status < 400,
  };
}

/** Revoke a QURL link by resource_id (revokes entire resource) */
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
