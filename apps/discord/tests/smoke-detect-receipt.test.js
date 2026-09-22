jest.mock('@layervai/qurl', () => ({ QURLClient: class {} }));
jest.mock('node:child_process', () => ({ spawnSync: jest.fn() }));
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { installMintReceipt } = require('../scripts/smoke-detect');

test('captures exact detector child before returning mint, without capability or native session claims', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'detect-receipt-'));
  process.env.QURL_OWNERSHIP_RECEIPTS = path.join(dir, 'owned.jsonl');
  process.env.QURL_OWNERSHIP_VERIFIER = '/private/verifier';
  const minted = { crid: 'crid-owned', resource_id: 'r_owned', qurl_id: 'q_detector',
    expires_at: '2030-01-01T00:00:00Z', qurl_link: 'secret-capability' };
  const identity = { agent_public_key: 'YWdlbnQ=', resource_public_key_b64: 'cmVzb3VyY2U=', cell_public_key_b64: 'Y2VsbA==' };
  spawnSync.mockReturnValue({ status: 0, stdout: JSON.stringify(identity) });
  class Client {
    async createQurlForResource() { return minted; }
    async revokeResourceQurl() { throw new Error('must not revoke on success'); }
  }
  const restore = installMintReceipt(Client, 'owner');
  try {
    expect(await new Client().createQurlForResource('crid-owned')).toBe(minted);
    const text = fs.readFileSync(process.env.QURL_OWNERSHIP_RECEIPTS, 'utf8');
    const receipt = JSON.parse(text);
    expect(receipt).toMatchObject({ owner_id: 'owner', resource_id: 'r_owned', qurl_id: 'q_detector',
      purpose: 'discord_detect_smoke_detector_child', public_identity: identity });
    expect(text).not.toMatch(/secret-capability|session_id|detected/);
    expect(restore.captured).toBe(1);
    expect(fs.statSync(process.env.QURL_OWNERSHIP_RECEIPTS).mode & 0o777).toBe(0o600);
  } finally { restore(); fs.rmSync(dir, { recursive: true, force: true }); }
});

test.each([['write', 0], ['verification', 1]])('%s failure prevents opening and revokes only exact returned child', async (_label, status) => {
  process.env.QURL_OWNERSHIP_RECEIPTS = '/missing-parent/owned.jsonl';
  spawnSync.mockReturnValue({ status, stdout: JSON.stringify({ agent_public_key: 'a', resource_public_key_b64: 'b', cell_public_key_b64: 'c' }) });
  const revoke = jest.fn();
  class Client {
    async createQurlForResource() { return { crid: 'crid-owned', resource_id: 'r_owned', qurl_id: 'q_detector', expires_at: '2030-01-01T00:00:00Z', qurl_link: 'secret' }; }
    async revokeResourceQurl(...args) { revoke(...args); }
  }
  const original = Client.prototype.createQurlForResource;
  const restore = installMintReceipt(Client, 'owner');
  const open = jest.fn();
  try {
    await expect(new Client().createQurlForResource('crid-owned').then(open)).rejects.toThrow('could not be persisted');
    expect(open).not.toHaveBeenCalled();
    expect(revoke).toHaveBeenCalledWith('crid-owned', 'q_detector');
    expect(restore.captured).toBe(0);
  } finally { restore(); }
  expect(Client.prototype.createQurlForResource).toBe(original);
});
