const http = require('node:http');

const mockConfig = {
  QURL_API_KEY: 'test-guild-key',
  QURL_ENDPOINT: '',
  CONNECTOR_URL: '',
  QURL_LINK_DOMAIN: 'qurl.site',
  PRIVATE_UPLOAD_QURL: null,
};
jest.mock('../src/config', () => mockConfig);
jest.mock('../src/logger', () => ({ info: jest.fn(), warn: jest.fn(), error: jest.fn(), debug: jest.fn(), audit: jest.fn() }));

const { revokeMintedLinks } = require('../src/connector');
const { redeemDelegatedBatch } = require('../src/private-upload');
const { PUBLIC_KEY_RESOURCE_ID: source, CRID_RESOURCE_ID: crid } = require('./helpers/qurl-fixtures');
const oldChild = 'q_00000000001';
const newChild = 'q_00000000002';
const delegatedChild = 'q_00000000003';
const batchId = `dqb_${'f'.repeat(22)}`;
let server;
let requests;
let children;
let rejectRevoke;
let parentMismatch;

beforeAll(async () => {
  server = http.createServer(async (req, res) => {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString()) : undefined;
    requests.push({ method: req.method, path: req.url, body, key: req.headers['idempotency-key'] });
    const json = (status, data, headers = {}) => {
      res.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'private, no-store', ...headers });
      res.end(JSON.stringify(data));
    };
    if (req.url === '/api/revoke_links') {
      return json(200, { success: true, results: body.qurl_ids.map(qurl_id => ({ qurl_id, status: 'not_connector_managed' })) });
    }
    if (req.method === 'GET' && req.url === `/v1/qurls/${newChild}`) {
      return json(200, { data: { resource_id: parentMismatch ? 'foreign-parent' : source, crid, target_url: 'https://file.example/item', status: 'active', qurls: [] } });
    }
    if (req.method === 'DELETE' && [
      `/v1/resources/${crid}/qurls/${newChild}`, `/v1/delegated-qurls/${delegatedChild}`,
    ].includes(req.url)) {
      if (rejectRevoke) return json(503, { error: { status: 503, code: 'service_unavailable', title: 'Unavailable' } });
      children.delete(req.url.endsWith(delegatedChild) ? delegatedChild : newChild);
      res.writeHead(204).end();
      return;
    }
    if (req.method === 'POST' && req.url === '/v1/delegated-qurl-batches') {
      return json(202, {
        meta: { request_id: 'test-request', timestamp: '2026-09-10T00:00:00Z' },
        data: { batch_id: batchId, status: 'queued', item_count: 1, submitted_at: '2026-09-10T00:00:00Z' },
      }, { Location: `${mockConfig.QURL_ENDPOINT}/v1/delegated-qurl-batches/${batchId}`, 'Retry-After': '2' });
    }
    json(500, { error: { status: 500, code: 'unexpected_test_request', title: 'Unexpected request' } });
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  mockConfig.QURL_ENDPOINT = `http://127.0.0.1:${server.address().port}`;
  mockConfig.CONNECTOR_URL = mockConfig.QURL_ENDPOINT;
});
afterAll(() => new Promise(resolve => server.close(resolve)));
beforeEach(() => {
  requests = [];
  children = new Set([oldChild, newChild, delegatedChild]);
  rejectRevoke = false;
  parentMismatch = false;
  mockConfig.PRIVATE_UPLOAD_QURL = null;
});

test('failed Add Recipients cleanup and repeat revoke preserve the earlier child and shared parent', async () => {
  // Turning private upload on must not change how a historical public row is revoked.
  mockConfig.PRIVATE_UPLOAD_QURL = 'qurl://private-upload';
  await revokeMintedLinks(source, [newChild], 'test-guild-key');
  await revokeMintedLinks(source, [newChild], 'test-guild-key');
  expect(children.has(newChild)).toBe(false);
  expect(children.has(oldChild)).toBe(true);
  expect(requests.filter(request => request.method === 'DELETE').map(request => request.path))
    .toEqual(Array(2).fill(`/v1/resources/${crid}/qurls/${newChild}`));
});

test('a failed ordinary child revoke is not reported as cleanup success', async () => {
  rejectRevoke = true;
  await expect(revokeMintedLinks(source, [newChild], 'test-guild-key')).rejects.toThrow(/503/);
  expect(children.has(newChild)).toBe(true);
  expect(children.has(oldChild)).toBe(true);
});

test('a child from a different parent cannot redirect cleanup', async () => {
  parentMismatch = true;
  await expect(revokeMintedLinks(source, [newChild], 'test-guild-key')).rejects.toThrow(/does not match/);
  expect(requests.some(request => request.method === 'DELETE')).toBe(false);
});

test('private rows still use delegated revoke after the private flag is removed', async () => {
  await revokeMintedLinks(delegatedChild, [delegatedChild], 'test-guild-key');
  expect(children.has(delegatedChild)).toBe(false);
  expect(children.has(oldChild)).toBe(true);
  expect(requests.map(request => request.path)).toEqual([`/v1/delegated-qurls/${delegatedChild}`]);
});

test('a queued batch beyond the interaction deadline remains unknown and is not resubmitted', async () => {
  const expiry = '2027-01-01T00:00:00Z';
  const error = await redeemDelegatedBatch({ mint_capability: 'qmc1.test', authority_expires_at: expiry }, {
    credential: { apiKey: 'test-guild-key', keyId: 'key_A1b2C3d4E5f6' },
    grants: [{ one_time_use: true }], deadlineMs: Date.now() + 1000,
  }).catch(err => err);
  expect(error).toMatchObject({ batchOutcomeUnknown: true, batchId, unknownBatchExpiresAt: expiry, partialLinkCount: 0 });
  expect(requests.map(request => request.method)).toEqual(['POST']);
});
