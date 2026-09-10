const mockClient = { listAllResources: jest.fn(), createQurlForResource: jest.fn() };
const mockOpen = jest.fn();
jest.mock('@layervai/qurl', () => ({ QURLClient: jest.fn(() => mockClient) }));
jest.mock('@layervai/qurl/node', () => ({ createPortalOpener: (...args) => mockOpen(...args) }));
jest.mock('../src/config', () => ({
  QURL_ENDPOINT: 'https://api.layerv.ai', QURL_API_KEY: 'bot-key', DETECT_TUNNEL_SLUG: 'detect-test',
}));
jest.mock('../src/logger', () => ({ warn: jest.fn(), info: jest.fn(), debug: jest.fn(), error: jest.fn() }));
const bindingId = 'eib_12345678901';
const otherBinding = 'eib_12345678902';
const origin = 'https://detect-test.qurl.site';
const path = `/api/detect/${bindingId}`;
const target = origin + path;
const qurl = 'https://qurl.link/#qv2t1.test-credential';
// Existing SDK 0.6 resource identity contract.
const resourceId = 'MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE2ifzReg5Fb3RadAQRn_oYpEYDKDXp0InOyQpO8Wo392Hmm92wvsORreNjzdi18er8WjAQzqP3KUgkYJxjO0ZpQ';
let detect, opener, send;
const result = { detected: false, qurl_id: null, match_pct: null, confidence: 0 };
beforeEach(() => {
  jest.resetModules();
  jest.clearAllMocks();
  mockClient.listAllResources.mockImplementation(async function* () {
    yield { resource_id: resourceId, status: 'active' };
  });
  mockClient.createQurlForResource.mockReset().mockResolvedValue({
    resource_id: resourceId, qurl_link: qurl, qurl_site: origin, target_path: path,
  });
  send = jest.fn().mockResolvedValue({ ok: true, json: async () => result });
  opener = {
    start: jest.fn().mockResolvedValue(), close: jest.fn().mockResolvedValue(),
    fetch: jest.fn(async (build) => send(build(new URL(target)))),
  };
  mockOpen.mockReset().mockReturnValue(opener);
  detect = require('../src/connector').detectWatermark;
});
it('mints the exact binding path and sends no API key or guild header', async () => {
  await expect(detect(Buffer.from('image'), { bindingId, contentType: 'image/png' })).resolves.toEqual(result);
  expect(mockClient.createQurlForResource).toHaveBeenCalledWith(resourceId, { expires_in: '5m', target_path: path });
  expect(mockOpen).toHaveBeenCalledWith({ qurl });
  expect(send).toHaveBeenCalledWith(expect.objectContaining({ method: 'POST', headers: { 'Content-Type': 'image/png' } }));
  expect(opener.fetch).toHaveBeenCalledWith(expect.any(Function), { redirects: 'error' });
  expect(opener.close).toHaveBeenCalledTimes(1);
});
it.each([undefined, '', 'guild-9', 'eib_short', `${bindingId}/other`, `${bindingId}?x=1`])('rejects invalid binding %s before mint', async (bad) => {
  await expect(detect(Buffer.from('x'), { bindingId: bad })).rejects.toThrow(/binding/);
  expect(mockClient.listAllResources).not.toHaveBeenCalled();
});
it.each([
  target, `${origin}/api/detect`, `${origin}/api/detect/${otherBinding}`, `${target}?x=1`, `${target}#fragment`,
  `https://localhost${path}`, `https://127.0.0.1${path}`, `http://detect-test.qurl.site${path}`,
  `https://evil.example${path}`, `https://user:secret@detect-test.qurl.site${path}`,
])('rejects an untrusted minted target %s before opening', async (site) => {
  mockClient.createQurlForResource.mockResolvedValue({ resource_id: resourceId, qurl_link: qurl, qurl_site: site, target_path: path });
  await expect(detect(Buffer.from('x'), { bindingId })).rejects.toThrow();
  expect(mockOpen).not.toHaveBeenCalled();
  expect(send).not.toHaveBeenCalled();
});
it('rejects a signed ACK for another binding before sending bytes', async () => {
  opener.fetch.mockImplementation(async build => send(build(new URL(`${origin}/api/detect/${otherBinding}`))));
  await expect(detect(Buffer.from('x'), { bindingId })).rejects.toThrow(/native target/);
  expect(send).not.toHaveBeenCalled();
  expect(opener.close).toHaveBeenCalledTimes(1);
});
it.each(['https://qurl.link/#at_legacy', '', undefined])('rejects unsigned or missing credentials', async link => {
  mockClient.createQurlForResource.mockResolvedValue({ resource_id: resourceId, qurl_link: link, qurl_site: origin, target_path: path });
  await expect(detect(Buffer.from('x'), { bindingId })).rejects.toThrow(/signed native/);
  expect(send).not.toHaveBeenCalled();
});
it('rejects mismatched resource identity', async () => {
  mockClient.createQurlForResource.mockResolvedValue({ resource_id: 'other', qurl_link: qurl, qurl_site: origin, target_path: path });
  await expect(detect(Buffer.from('x'), { bindingId })).rejects.toThrow(/resource_id/);
  expect(mockOpen).not.toHaveBeenCalled();
});
it('closes after native open failure and redacts credentials', async () => {
  opener.start.mockRejectedValue(new Error(`failed ${qurl}`));
  await expect(detect(Buffer.from('x'), { bindingId })).rejects.not.toThrow('test-credential');
  expect(opener.close).toHaveBeenCalledTimes(1);
  expect(send).not.toHaveBeenCalled();
});
it('closes after POST failure and preserves status', async () => {
  send.mockResolvedValue({ ok: false, status: 503, text: async () => 'unavailable' });
  await expect(detect(Buffer.from('x'), { bindingId })).rejects.toMatchObject({ status: 503 });
  expect(opener.close).toHaveBeenCalledTimes(1);
});
it('keeps attribution identity from a successful response', async () => {
  send.mockResolvedValue({ ok: true, json: async () => ({ detected: true, qurl_id: 'q_known', confidence: 1, match_pct: 100 }) });
  await expect(detect(Buffer.from('x'), { bindingId })).resolves.toMatchObject({ detected: true, qurl_id: 'q_known' });
});
it('caches only resource identity and mints anew for each binding', async () => {
  await detect(Buffer.from('x'), { bindingId });
  const otherTarget = `${origin}/api/detect/${otherBinding}`;
  mockClient.createQurlForResource.mockResolvedValue({ resource_id: resourceId, qurl_link: qurl, qurl_site: origin, target_path: `/api/detect/${otherBinding}` });
  opener.fetch.mockImplementation(async build => send(build(new URL(otherTarget))));
  await detect(Buffer.from('x'), { bindingId: otherBinding });
  expect(mockClient.listAllResources).toHaveBeenCalledTimes(1);
  expect(mockClient.createQurlForResource).toHaveBeenLastCalledWith(resourceId, { expires_in: '5m', target_path: `/api/detect/${otherBinding}` });
});
it.each([[[]], [[{ status: 'active', resource_id: resourceId }, { status: 'active', resource_id: resourceId }]]])('rejects absent or ambiguous resources', async resources => {
  mockClient.listAllResources.mockImplementation(async function* () { yield* resources; });
  await expect(detect(Buffer.from('x'), { bindingId })).rejects.toThrow(/resource/);
  expect(mockClient.createQurlForResource).not.toHaveBeenCalled();
});
