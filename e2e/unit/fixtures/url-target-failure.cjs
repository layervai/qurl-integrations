const { appendFileSync, writeFileSync } = require('node:fs');
const { resolve } = require('node:path');
const root = resolve(__dirname, '../..');
const mode = process.env.QURL_TEST_FAILURE;
let target;
let posts = 0;
let reads = 0;
jest.mock(resolve(root, 'helpers/tunnelView'), () => ({
  checkOwnershipConfig: async () => appendFileSync(process.env.QURL_OWNERSHIP_RECEIPTS, '', { mode: 0o600 }),
  recordAdmissionAttempt: async () => { throw new Error('No admission allowed in failure fixture'); },
}));
jest.mock(resolve(root, 'helpers/cleanup'), () => ({ trackedQurlResources: () => ({
  track() {}, trackChild() {},
  async revokeAll() { if (mode === 'cleanup') throw new Error('Fixture cleanup failed'); },
}) }));
jest.mock('playwright', () => ({ chromium: { async launch() {
  if (mode === 'browser') throw new Error('Fixture browser unavailable');
  return { async close() {} };
} } }));
global.fetch = async (url, options) => {
  if (url === 'https://api.layerv.xyz/v1/qurls' && options.method === 'POST') {
    posts++;
    target = JSON.parse(options.body).target_url;
    if (mode === 'mint') return new Response('not JSON', { status: 502 });
    return Response.json({ resource_id: 'owned-fixture', qurl_id: 'q_fixture', expires_at: '2030-01-01T00:00:00Z', qurl_link: 'invalid' }, { status: 201 });
  }
  if (url === 'https://api.layerv.xyz/v1/qurls/q_fixture' && !options.method) {
    reads++;
    if (mode === 'readback') return new Response('not JSON', { status: 502 });
    return Response.json({ resource_id: 'owned-fixture', target_url: target,
      qurls: [{ qurl_id: 'q_fixture', one_time_use: true, max_sessions: 1, use_count: 0, status: 'active' }] });
  }
  throw new Error('Unexpected network operation in failure fixture');
};
afterAll(() => writeFileSync(process.env.QURL_TEST_NETWORK, JSON.stringify({ posts, reads }), { mode: 0o600 }));
