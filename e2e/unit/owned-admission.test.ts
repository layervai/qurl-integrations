import { mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { recordAdmissionAttempt, checkOwnershipConfig } from '../helpers/tunnelView';

jest.mock('node:child_process', () => ({ spawnSync: jest.fn() }));

test.each(['', 'cell0'])('receipt retains optional cell ID %j and rejects invalid identity', async (cellID) => {
  const dir = mkdtempSync(join(tmpdir(), 'owned-admission-'));
  const old = { ...process.env };
  const oldFetch = global.fetch;
  try {
    const path = join(dir, 'receipt.jsonl');
    const verifier = join(dir, 'verifier');
    writeFileSync(verifier, '', { mode: 0o700 });
    process.env.QURL_OWNERSHIP_VERIFIER = verifier;
    process.env.QURL_PUBLIC_CONFIG_URL = 'https://qurl.link.layerv.xyz/';
    process.env.QURL_OWNERSHIP_RECEIPTS = path;
    process.env.MINT_API_URL = 'https://api.example/v1/qurls';
    process.env.QURL_API_KEY = 'private-api-key';
    global.fetch = jest.fn().mockImplementation(() => Promise.resolve(new Response(JSON.stringify({ data: { owner_id: 'owner' } }))));
    const identity = { agent_public_key: 'public-agent', resource_public_key_b64: 'public-resource', cell_public_key_b64: 'public-cell', cell_id: cellID, signed_jti: 'independent-signed-jti', signed_expiry_unix: '1789862400' };
    const child = { resource_id: 'source', qurl_id: 'child', expires_at: '2026-09-20T00:00:00Z' };
    (spawnSync as jest.Mock).mockReturnValue({ status: 0, stdout: JSON.stringify(identity) });
    await checkOwnershipConfig();
    await recordAdmissionAttempt('https://qurl.link/#private-secret', child, 30_000);
    expect(spawnSync).toHaveBeenLastCalledWith(verifier, [], expect.objectContaining({ input: 'https://qurl.link/#private-secret' }));
    const raw = readFileSync(path, 'utf8');
    expect(raw).not.toContain('private-secret');
    expect(JSON.parse(raw)).toMatchObject({ owner_id: 'owner', resource_id: 'source', qurl_id: 'child', child_binding: 'pending_independent_readback', public_identity: identity });
    expect(statSync(path).mode & 0o777).toBe(0o600);
    (spawnSync as jest.Mock).mockReturnValue({ status: 0, stdout: JSON.stringify({ ...identity, cell_public_key_b64: '' }) });
    await expect(recordAdmissionAttempt('private-secret', child, 30_000)).rejects.toThrow('verified public identity is incomplete');
    expect(readFileSync(path, 'utf8')).toBe(raw);
    (spawnSync as jest.Mock).mockReturnValue({ status: 1, stderr: 'private-secret' });
    await expect(recordAdmissionAttempt('private-secret', child, 30_000)).rejects.toThrow(/^signed ownership verification failed$/);
    expect(readFileSync(path, 'utf8')).toBe(raw);
    const reads = (global.fetch as jest.Mock).mock.calls.length;
    await expect(checkOwnershipConfig()).rejects.toThrow('public ownership configuration failed');
    expect((global.fetch as jest.Mock).mock.calls).toHaveLength(reads);
  } finally {
    process.env = old;
    global.fetch = oldFetch;
    rmSync(dir, { recursive: true, force: true });
  }
});
