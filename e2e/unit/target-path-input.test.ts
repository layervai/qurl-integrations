import { existsSync, mkdtempSync, readFileSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { runTargetPathInput } from '../helpers/targetPathInput';
const apiOrigin = 'https://api.example.layerv.xyz';
const parent = 'ownedParentResource0123456789';
const snapshot = (path: string) => JSON.parse(readFileSync(path, 'utf8').trimEnd().split('\n').pop()!);
const bearer = 'private-bearer-canary';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

for (const mode of ['success', 'unknown-post', 'wrong-path', 'wrong-parent', 'delete-fails', 'not-revoked', 'wrong-type', 'bad-rejection', 'oversize-response', 'missing-child']) {
 test(mode, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'target-path-input-'));
  const path = join(dir, 'journal.json');
  const children = new Map<string, { path?: string; revoked: boolean }>();
  let posts = 0; let deletes = 0;
  const fetchMock = jest.spyOn(global, 'fetch').mockImplementation(async (input, init) => {
   const url = new URL(String(input));
   expect(url.origin).toBe('https://api.example.layerv.xyz');
   if (init?.method === 'POST') {
    const saved = snapshot(path);
    expect(saved.pending).not.toBeNull();
    expect(statSync(path).mode & 0o777).toBe(0o600);
    posts++;
    if (mode === 'unknown-post') throw new Error('private transport payload');
    const body = JSON.parse(String(init.body));
    expect(body.label).toBe(saved.pending.request_id);
    expect(body.label).toMatch(/^targetpath-[0-9a-f-]{36}$/);
    const bad = ['/objects/%61', '/objects/../owned-input-probe', '/objects//owned-input-probe'].includes(body.target_path);
    if (bad) return json({ error: { code: mode === 'bad-rejection' ? 'wrong' : 'invalid_target_path' } }, 400);
    if (mode === 'oversize-response') return new Response('x'.repeat(1_048_577), { status: 201 });
    if (mode === 'missing-child') return json({ data: {} }, 201);
    const id = `q_child${children.size + 1}`;
    children.set(id, { path: body.target_path, revoked: false });
    return json({ data: { qurl_id: id, qurl_link: 'secret-capability-canary' } }, 201);
   }
   if (init?.method === 'DELETE') {
    expect(url.pathname).toMatch(new RegExp(`^/v1/resources/${parent}/qurls/q_child[1-6]$`));
    deletes++;
    if (mode === 'delete-fails') return json({}, 403);
    const child = children.get(url.pathname.split('/').pop()!)!;
    child.revoked = true;
    return new Response(null, { status: 204 });
   }
   const id = url.pathname.split('/').pop()!;
   const entry = children.get(id);
   if (entry) expect(snapshot(path).children.some((child: { id: string }) => child.id === id)).toBe(true);
   return json({ data: { resource_id: mode === 'wrong-parent' && entry ? 'wrong-parent' : parent,
    status: 'active', type: mode === 'wrong-type' ? 'url' : 'tunnel',
    qurls: entry ? [{ qurl_id: id, use_count: 0, status: entry.revoked && mode !== 'not-revoked' ? 'revoked' : 'active',
     ...(entry.path === undefined ? {} : { target_path: mode === 'wrong-path' ? '/wrong' : entry.path }) }] : [] } });
  });
  try {
   if (mode === 'success') await runTargetPathInput("q_seed", bearer, path, apiOrigin);
   else await expect(runTargetPathInput("q_seed", bearer, path, apiOrigin)).rejects.toThrow('retain lock and private journal');
   const saved = snapshot(path);
   expect(readFileSync(path, 'utf8')).not.toMatch(/private-bearer-canary|secret-capability-canary|private transport payload/);
   if (mode === 'success') {
    expect(posts).toBe(12); expect(deletes).toBe(6);
    expect(saved.children).toHaveLength(6); expect(saved.results).toHaveLength(12);
    const snapshots = readFileSync(path, 'utf8').trimEnd().split('\n').map((line) => JSON.parse(line));
    expect(snapshots.length).toBeGreaterThan(30);
    expect(snapshots.some((row) => row.children.length === 1)).toBe(true);
    expect(saved.status).toBe('INPUT_MATRIX_AND_CHILD_CLEANUP_COMPLETE');
   } else {
    expect(saved.status).toBe('FAILED_CLOSED_RETAIN_LOCK_AND_JOURNAL');
    if (mode === 'unknown-post') { expect(posts).toBe(1); expect(saved.pending).not.toBeNull(); }
    if (mode === 'wrong-type') expect(posts).toBe(0);
   }
  } finally { fetchMock.mockRestore(); rmSync(dir, { recursive: true, force: true }); }
 });
}

for (const apiOrigin of ['', 'http://api.example.com', 'https://user:secret@api.example.com', 'https://api.example.com/path', 'https://api.example.com?x=1', 'https://api.example.com/#x']) {
 test(`rejects an unsafe API origin before creating the journal: ${new URL(apiOrigin || 'http://example.com').protocol}`, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'target-path-origin-'));
  const path = join(dir, 'journal.json');
  const fetchMock = jest.spyOn(global, 'fetch');
  try {
   await expect(runTargetPathInput('q_seed', bearer, path, apiOrigin)).rejects.toThrow('Explicit HTTPS API origin required');
   expect(fetchMock).not.toHaveBeenCalled();
   expect(existsSync(path)).toBe(false);
  } finally { fetchMock.mockRestore(); rmSync(dir, { recursive: true, force: true }); }
 });
}
