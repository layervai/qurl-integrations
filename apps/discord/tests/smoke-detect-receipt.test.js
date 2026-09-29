jest.mock('@layervai/qurl', () => ({ QURLClient: class {} }));
jest.mock('node:child_process', () => ({ spawnSync: jest.fn() }));
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const originalEnv = { ...process.env };
beforeEach(() => { jest.clearAllMocks(); });
afterEach(() => { process.env = { ...originalEnv }; });
const { installMintReceipt } = require('../scripts/smoke-detect');

test('captures exact detector child before returning mint, without capability or native session claims', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'detect-receipt-'));
  process.env.QURL_OWNERSHIP_RECEIPTS = path.join(dir, 'owned.jsonl');
  process.env.QURL_OWNERSHIP_VERIFIER = '/private/verifier';
  process.env.QURL_PUBLIC_CONFIG_URL = 'https://qurl.link.layerv.xyz/';
  process.env.QURL_API_KEY = 'private-api-key';
  const { CRID_RESOURCE_ID: crid } = require('./helpers/qurl-fixtures');
  const minted = { crid, resource_id: 'r_owned', qurl_id: 'q_detector',
    expires_at: '2030-01-01T00:00:00Z', qurl_link: 'secret-capability' };
  const identity = { agent_public_key: 'YWdlbnQ=', resource_public_key_b64: 'cmVzb3VyY2U=', cell_public_key_b64: 'Y2VsbA==', cell_id: 'cell', signed_jti: 'jti', signed_expiry_unix: '1893456000' };
  spawnSync.mockReturnValue({ status: 0, stdout: JSON.stringify({ ...identity, private_key: 'secret-private-key' }) });
  const { QURLClient: Client } = jest.requireActual('@layervai/qurl');
  const fetch = jest.fn(async () => new Response(JSON.stringify({ data: minted }), {
    status: 200, headers: { 'Content-Type': 'application/json' },
  }));
  const client = new Client({ baseUrl: 'https://api.example', apiKey: 'test-key', fetch });
  const restore = installMintReceipt(Client, 'owner');
  try {
    expect(await client.createQurlForResource(crid)).toEqual(minted);
    expect(fetch).toHaveBeenCalledTimes(1);
    const text = fs.readFileSync(process.env.QURL_OWNERSHIP_RECEIPTS, 'utf8');
    const receipt = JSON.parse(text);
    expect(receipt).toMatchObject({ owner_id: 'owner', resource_id: 'r_owned', qurl_id: 'q_detector',
      purpose: 'discord_detect_smoke_detector_child', crid, public_identity: identity });
    expect(text).not.toMatch(/secret-capability|secret-private-key|private_key|session_id|detected/);
    expect(restore.captured).toBe(1);
    expect(spawnSync.mock.calls[0][2].env).toEqual({ QURL_PUBLIC_CONFIG_URL: process.env.QURL_PUBLIC_CONFIG_URL });
    expect(fs.statSync(process.env.QURL_OWNERSHIP_RECEIPTS).mode & 0o777).toBe(0o600);
  } finally { restore(); fs.rmSync(dir, { recursive: true, force: true }); }
});

test.each([['write', 0], ['verification', 1]])('%s failure prevents opening and revokes only exact returned child', async (_label, status) => {
  process.env.QURL_OWNERSHIP_RECEIPTS = '/missing-parent/owned.jsonl';
  spawnSync.mockReturnValue({ status, stdout: JSON.stringify({ agent_public_key: 'a', resource_public_key_b64: 'b', cell_public_key_b64: 'c', cell_id: 'cell', signed_jti: 'jti', signed_expiry_unix: '1893456000' }) });
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


test.each([undefined, 'at_secret', 'bad/id'])('invalid child %s is never revoked', async qurl_id => {
  const log = jest.spyOn(console, 'error').mockImplementation(() => {});
  const revoke = jest.fn();
  class Client {
    async createQurlForResource() { return { crid: 'crid-owned', resource_id: 'r_owned', qurl_id }; }
    async revokeResourceQurl(...args) { revoke(...args); }
  }
  const restore = installMintReceipt(Client, 'owner');
  try {
    await expect(new Client().createQurlForResource('crid-owned')).rejects.toThrow('could not be persisted');
    expect(revoke).not.toHaveBeenCalled();
    expect(log).toHaveBeenCalledWith('Detector child cleanup required', {
      event: 'detector_child_cleanup_required', owner_id: 'owner', resource_id: 'r_owned', qurl_id: null, crid: 'crid-owned',
    });
  } finally { restore(); log.mockRestore(); }
});

test('failed child revoke retains only structured safe cleanup identity', async () => {
  const log = jest.spyOn(console, 'error').mockImplementation(() => {});
  spawnSync.mockReturnValue({ status: 1 });
  class Client {
    async createQurlForResource() { return { crid: 'crid-owned', resource_id: 'r_owned', qurl_id: 'q_detector', expires_at: '2030-01-01T00:00:00Z', qurl_link: 'at_capability' }; }
    async revokeResourceQurl() { throw new Error('at_dependency-secret'); }
  }
  const restore = installMintReceipt(Client, 'owner');
  try {
    await expect(new Client().createQurlForResource('crid-owned')).rejects.toThrow('could not be persisted');
    expect(log).toHaveBeenCalledWith('Detector child cleanup required', {
      event: 'detector_child_cleanup_required', owner_id: 'owner', resource_id: 'r_owned', qurl_id: 'q_detector', crid: 'crid-owned',
    });
    expect(JSON.stringify(log.mock.calls)).not.toContain('at_');
  } finally { restore(); log.mockRestore(); }
});


test('real SDK exposes the interception and exact-child revoke methods', () => {
  const { QURLClient } = jest.requireActual('@layervai/qurl');
  expect(typeof QURLClient.prototype.createQurlForResource).toBe('function');
  expect(typeof QURLClient.prototype.revokeResourceQurl).toBe('function');
  expect(() => installMintReceipt(class {}, 'owner')).toThrow('interception point missing');
});


test.each(['http://api.layerv.xyz', 'https://untrusted.example', 'https://user:pass@api.layerv.xyz'])('preflight rejects %s before fetching owner identity', async endpoint => {
  process.env.DETECT_SMOKE_GUILD_ID = '1491271325791293611';
  for (const key of ['QURL_OWNERSHIP_VERIFIER', 'QURL_OWNERSHIP_RECEIPTS', 'QURL_PUBLIC_CONFIG_URL', 'QURL_ENDPOINT', 'QURL_API_KEY']) process.env[key] = 'configured';
  const argv = process.argv;
  process.argv = argv.slice(0, 2);
  const config = require('../src/config');
  const original = config.QURL_ENDPOINT;
  config.QURL_ENDPOINT = endpoint;
  const fetch = jest.spyOn(global, 'fetch');
  try {
    await expect(require('../scripts/smoke-detect').main()).rejects.toThrow('untrusted ownership API endpoint');
    expect(fetch).not.toHaveBeenCalled();
  } finally { process.argv = argv; config.QURL_ENDPOINT = original; fetch.mockRestore(); }
});
