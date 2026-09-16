const mockClient = { listAllResources: jest.fn(), createQurlForResource: jest.fn() };
const mockOpen = jest.fn();
jest.mock('@layervai/qurl', () => ({ QURLClient: jest.fn(() => mockClient) }));
jest.mock('@layervai/qurl/node', () => ({ createPortalOpener: (...args) => mockOpen(...args) }));
jest.mock('../src/config', () => ({
  QURL_ENDPOINT: 'https://api.layerv.ai', QURL_API_KEY: 'bot-key', DETECT_TUNNEL_SLUG: 'detect-test',
}));
jest.mock('../src/logger', () => ({ warn: jest.fn(), info: jest.fn(), debug: jest.fn(), error: jest.fn() }));
const guildId = '1491271325791293611';
const otherGuild = '1539333142207528980';
const origin = 'https://detect-test.qurl.site';
const path = `/api/detect/discord/${guildId}`;
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
it('mints the exact guild path and sends no API key or guild header', async () => {
  await expect(detect(Buffer.from('image'), { guildId, contentType: 'image/png' })).resolves.toEqual(result);
  expect(mockClient.createQurlForResource).toHaveBeenCalledWith(resourceId, { expires_in: '5m', session_duration: '5m', target_path: path });
  expect(mockOpen).toHaveBeenCalledWith({ qurl });
  expect(send).toHaveBeenCalledWith(expect.objectContaining({ method: 'POST', headers: { 'Content-Type': 'image/png' } }));
  expect(opener.fetch).toHaveBeenCalledWith(expect.any(Function), { redirects: 'error' });
  expect(opener.close).toHaveBeenCalledTimes(1);
});
it.each([undefined, '', 'guild-9', 'eib_short', `${guildId}/other`, `${guildId}?x=1`])('rejects invalid guild %s before mint', async (bad) => {
  await expect(detect(Buffer.from('x'), { guildId: bad })).rejects.toThrow(/guild/);
  expect(mockClient.listAllResources).not.toHaveBeenCalled();
});
it.each([
  target, `${origin}/api/detect`, `${origin}/api/detect/discord/${otherGuild}`, `${target}?x=1`, `${target}#fragment`,
  'https://localhost', 'https://127.0.0.1', 'http://detect-test.qurl.site',
  'https://evil.example', 'https://user:secret@detect-test.qurl.site',
])('rejects an untrusted minted target %s before opening', async (site) => {
  mockClient.createQurlForResource.mockResolvedValue({ resource_id: resourceId, qurl_link: qurl, qurl_site: site, target_path: path });
  await expect(detect(Buffer.from('x'), { guildId })).rejects.toThrow();
  expect(mockOpen).not.toHaveBeenCalled();
  expect(send).not.toHaveBeenCalled();
});
it('rejects a signed ACK for another guild before sending bytes', async () => {
  opener.fetch.mockImplementation(async build => send(build(new URL(`${origin}/api/detect/discord/${otherGuild}`))));
  await expect(detect(Buffer.from('x'), { guildId })).rejects.toThrow(/native target/);
  expect(send).not.toHaveBeenCalled();
  expect(opener.close).toHaveBeenCalledTimes(1);
});
it.each(['https://qurl.link/#at_legacy', '', undefined])('rejects unsigned or missing credentials', async link => {
  mockClient.createQurlForResource.mockResolvedValue({ resource_id: resourceId, qurl_link: link, qurl_site: origin, target_path: path });
  await expect(detect(Buffer.from('x'), { guildId })).rejects.toThrow(/signed native/);
  expect(send).not.toHaveBeenCalled();
});
it('rejects mismatched resource identity', async () => {
  mockClient.createQurlForResource.mockResolvedValue({ resource_id: 'other', qurl_link: qurl, qurl_site: origin, target_path: path });
  await expect(detect(Buffer.from('x'), { guildId })).rejects.toThrow(/resource_id/);
  expect(mockOpen).not.toHaveBeenCalled();
});
it('closes after native open failure and redacts credentials', async () => {
  opener.start.mockRejectedValue(new Error(`failed ${qurl}`));
  await expect(detect(Buffer.from('x'), { guildId })).rejects.toThrow(/qv2t1\.\[REDACTED\]/);
  expect(opener.close).toHaveBeenCalledTimes(1);
  expect(send).not.toHaveBeenCalled();
});
it('closes after POST failure and preserves status', async () => {
  send.mockResolvedValue({ ok: false, status: 503, text: async () => 'unavailable' });
  await expect(detect(Buffer.from('x'), { guildId })).rejects.toMatchObject({ status: 503 });
  expect(opener.close).toHaveBeenCalledTimes(1);
});
it('keeps attribution identity from a successful response', async () => {
  send.mockResolvedValue({ ok: true, json: async () => ({ detected: true, qurl_id: 'q_known', confidence: 1, match_pct: 100 }) });
  await expect(detect(Buffer.from('x'), { guildId })).resolves.toMatchObject({ detected: true, qurl_id: 'q_known' });
});
it('caches only resource identity and mints anew for each guild', async () => {
  await detect(Buffer.from('x'), { guildId });
  const otherTarget = `${origin}/api/detect/discord/${otherGuild}`;
  mockClient.createQurlForResource.mockResolvedValue({ resource_id: resourceId, qurl_link: qurl, qurl_site: origin, target_path: `/api/detect/discord/${otherGuild}` });
  opener.fetch.mockImplementation(async build => send(build(new URL(otherTarget))));
  await detect(Buffer.from('x'), { guildId: otherGuild });
  expect(mockClient.listAllResources).toHaveBeenCalledTimes(1);
  expect(mockClient.createQurlForResource).toHaveBeenLastCalledWith(resourceId, { expires_in: '5m', session_duration: '5m', target_path: `/api/detect/discord/${otherGuild}` });
});
it.each([[[]], [[{ status: 'active', resource_id: resourceId }, { status: 'active', resource_id: resourceId }]]])('rejects absent or ambiguous resources', async resources => {
  mockClient.listAllResources.mockImplementation(async function* () { yield* resources; });
  await expect(detect(Buffer.from('x'), { guildId })).rejects.toThrow(/resource/);
  expect(mockClient.createQurlForResource).not.toHaveBeenCalled();
});

it.each([undefined, '/api/detect', `/api/detect/discord/${otherGuild}`, path + '/'])('rejects a mismatched path echo %s', async target_path => {
  mockClient.createQurlForResource.mockResolvedValue({ resource_id: resourceId, qurl_link: qurl, qurl_site: origin, target_path });
  await expect(detect(Buffer.from('x'), { guildId })).rejects.toThrow(/mismatched guild path/);
  expect(mockOpen).not.toHaveBeenCalled();
});
it('refreshes the resource after a failed mint and backs off repeated failures', async () => {
  mockClient.createQurlForResource.mockRejectedValue(new Error('mint unavailable'));
  await expect(detect(Buffer.from('x'), { guildId })).rejects.toThrow('mint unavailable');
  await expect(detect(Buffer.from('x'), { guildId })).rejects.toThrow('mint unavailable');
  expect(mockClient.listAllResources).toHaveBeenCalledTimes(2);
  await expect(detect(Buffer.from('x'), { guildId })).rejects.toMatchObject({ retryAfterMs: expect.any(Number) });
  expect(mockClient.createQurlForResource).toHaveBeenCalledTimes(2);
});

it('uses fetch supported by the installed native SDK', async () => {
  const { createPortalOpener } = jest.requireActual('@layervai/qurl/node');
  const real = createPortalOpener({ qurl });
  expect(typeof real.fetch).toBe('function');
  await real.close();
});

it.each(['QURL_API_KEY', 'DETECT_TUNNEL_SLUG'])('rejects missing %s before network access', async key => {
  require('../src/config')[key] = '';
  await expect(detect(Buffer.from('x'), { guildId })).rejects.toThrow(/configured/);
  expect(mockClient.listAllResources).not.toHaveBeenCalled();
  expect(mockOpen).not.toHaveBeenCalled();
});
it('allows one retry after a slug lookup transport error', async () => {
  mockClient.listAllResources.mockImplementationOnce(async function* () { yield await Promise.reject(new Error('lookup unavailable')); });
  await expect(detect(Buffer.from('x'), { guildId })).rejects.toThrow('lookup unavailable');
  await expect(detect(Buffer.from('x'), { guildId })).resolves.toEqual(result);
  expect(mockClient.listAllResources).toHaveBeenCalledTimes(2);
});
it('accepts the configured sandbox tunnel suffix through the complete request', async () => {
  jest.resetModules();
  require('../src/config').QURL_ENDPOINT = 'https://api.layerv.xyz';
  require('../src/config').DETECT_EXTRA_NON_PROD_QURL_ENDPOINT_HOSTS = ['api.layerv.xyz'];
  detect = require('../src/connector').detectWatermark;
  const site = 'https://detect-test.qurl.site.layerv.xyz';
  mockClient.createQurlForResource.mockResolvedValue({ resource_id: resourceId, qurl_link: qurl, qurl_site: site, target_path: path });
  opener.fetch.mockImplementation(async build => send(build(new URL(site + path))));
  await expect(detect(Buffer.from('x'), { guildId })).resolves.toEqual(result);
  expect(send).toHaveBeenCalledTimes(1);
});
