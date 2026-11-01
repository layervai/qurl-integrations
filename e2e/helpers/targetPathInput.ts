import { closeSync, fsyncSync, openSync, writeSync } from 'node:fs';
import { randomUUID } from 'node:crypto';
import { getLinkStatus, getQurlResource, getResourceStatus, revokeChild } from './qurl-api';
const API = 'https://api.layerv.xyz';
const MANAGEMENT = `${API}/v1/qurls`;
const cases = [undefined, '/objects/owned-input-probe', '/?x=%2e/%2f', '/objects/%61', '/objects/../owned-input-probe', '/objects//owned-input-probe'];
/** Caller provides the existing owned parent and coordinates the live Sandbox lock. */
export async function runTargetPathInput(seedChild: string, bearer: string, journalPath: string): Promise<void> {
  if (!/^q_[A-Za-z0-9]+$/.test(seedChild) || !bearer || !journalPath.startsWith('/')) throw new Error('Explicit normal owned child, bearer, absolute journal required');
  const fd = openSync(journalPath, 'ax', 0o600);
  const journal: { seed_child: string; parent: string; status: string; pending: { route: number; index: number; request_id: string } | null;
    children: Array<{ id: string; route: number; index: number; revoked: boolean }>;
    results: Array<{ route: number; index: number; status: number; request_id: string }>;
  } = { seed_child: seedChild, parent: '', status: 'STARTED', pending: null, children: [], results: [] };
  const save = () => {
    const bytes = Buffer.from(JSON.stringify(journal) + '\n');
    let offset = 0;
    while (offset < bytes.length) offset += writeSync(fd, bytes, offset, bytes.length - offset);
    fsyncSync(fd);
  };
  let failed = false;
  try {
    save();
    const original = await getQurlResource(MANAGEMENT, bearer, seedChild, AbortSignal.timeout(10_000));
    const parent = original.resource_id;
    if (!/^[A-Za-z0-9_-]{20,256}$/.test(parent)) throw new Error('Canonical parent missing');
    journal.parent = parent; save();
    const ownedParent = await getResourceStatus(`${API}/v1/resources`, bearer, parent, AbortSignal.timeout(10_000));
    if (ownedParent.status !== 'active' || ownedParent.type !== 'tunnel') throw new Error('Owned parent is not active');
    for (const route of [0, 1]) for (const [index, path] of cases.entries()) {
      const requestID = `targetpath-${randomUUID()}`;
      journal.pending = { route, index, request_id: requestID }; save();
      const url = route === 0 ? `${API}/v1/qurls/${encodeURIComponent(parent)}/mint_link` : `${API}/v1/resources/${encodeURIComponent(parent)}/qurls`;
      const response = await fetch(url, { method: 'POST', redirect: 'error', signal: AbortSignal.timeout(10_000),
        headers: { Authorization: `Bearer ${bearer}`, 'Content-Type': 'application/json', 'X-Request-ID': requestID },
        body: JSON.stringify({ expires_in: '5m', session_duration: '30s', one_time_use: false, label: requestID, ...(path === undefined ? {} : { target_path: path }) }) });
      const reader = response.body?.getReader();
      if (!reader) throw new Error('Mint response body absent');
      const decoder = new TextDecoder();
      let raw = ''; let bytes = 0;
      try {
        for (;;) {
          const part = await reader.read();
          if (part.done) break;
          bytes += part.value.byteLength;
          if (bytes > 1_048_576) { await reader.cancel(); throw new Error('Mint response exceeds bound'); }
          raw += decoder.decode(part.value, { stream: true });
        }
        raw += decoder.decode();
      } finally { reader.releaseLock(); }
      const body = JSON.parse(raw) as { data?: { qurl_id?: unknown }; error?: { code?: unknown } };
      const id = body.data?.qurl_id;
      if (typeof id === 'string' && /^q_[A-Za-z0-9]+$/.test(id)) {
        if (journal.children.some((child) => child.id === id)) throw new Error('Duplicate child identity');
        journal.children.push({ id, route, index, revoked: false }); save();
      }
      if (index < 3) {
        if (response.status !== 201 || typeof id !== 'string' || !journal.children.some((child) => child.id === id)) throw new Error('Successful mint identity unconfirmed');
        const resource = await getQurlResource(MANAGEMENT, bearer, id, AbortSignal.timeout(10_000));
        if (resource.resource_id !== parent) throw new Error('Minted child parent mismatch');
        const detail = await getLinkStatus(MANAGEMENT, bearer, id, AbortSignal.timeout(10_000));
        if ((detail.target_path ?? '') !== (path ?? '') || detail.status !== 'active') throw new Error('Stored target path differs');
      } else if (response.status !== 400 || body.error?.code !== 'invalid_target_path' || id !== undefined) throw new Error('Required rejection absent');
      journal.results.push({ route, index, status: response.status, request_id: requestID }); journal.pending = null; save();
    }
  } catch { failed = true; }
  finally {
    for (const child of journal.children) {
      try {
        const resource = await getQurlResource(MANAGEMENT, bearer, child.id, AbortSignal.timeout(10_000));
        if (resource.resource_id !== journal.parent) throw new Error('Cleanup parent mismatch');
        await revokeChild(MANAGEMENT, bearer, child.id, AbortSignal.timeout(10_000));
        if ((await getLinkStatus(MANAGEMENT, bearer, child.id, AbortSignal.timeout(10_000))).status !== 'revoked') throw new Error('Revoked readback absent');
        child.revoked = true; save();
      } catch { failed = true; }
    }
    journal.status = !failed && journal.pending === null && journal.results.length === 12 && journal.children.length === 6 && journal.children.every((child) => child.revoked)
      ? 'INPUT_MATRIX_AND_CHILD_CLEANUP_COMPLETE' : 'FAILED_CLOSED_RETAIN_LOCK_AND_JOURNAL';
    try { save(); } finally { closeSync(fd); }
  }
  if (journal.status !== 'INPUT_MATRIX_AND_CHILD_CLEANUP_COMPLETE') throw new Error('Input probe unresolved; retain lock and private journal');
}
