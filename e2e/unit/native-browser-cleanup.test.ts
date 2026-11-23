import { EventEmitter } from 'node:events';
import type { Page, Request } from 'playwright';
import { attachBrowserCleanup, checkNativeCleanupConfig, observeBrowserCleanup, parseNativeCleanupConfig, nativeCleanupBundle, nativeCleanupDisposition } from '../helpers/nativeBrowserCleanup';

class AuthenticatedDenial extends Error {}
const publicConfig = `const QURL_LINK_CONFIG = {
        nativeSessionCleanupEnabled: true,
        visitorSessionBootstrapEnabled: true,
        issuerTrustStore: {"issuer":"public"},
        relayAllowlist: ["relay.example"]
      };`;
function fixture() {
  const page = new EventEmitter();
  const retirement = jest.fn().mockResolvedValue({ state: 'closed' });
  const observe = jest.fn().mockResolvedValue({ retireSession: retirement });
  const config = {
    sdk: { ServerDenyError: AuthenticatedDenial, TrustStore: { fromSpkiDerB64: jest.fn() }, RelayAllowlist: class {}, observeQurlV2SessionCleanup: observe },
    options: { trustStore: {}, relayAllowlist: {}, authServiceId: 'qurl' },
    relayHosts: new Set(['relay.example']),
  };
  const outgoing = Buffer.alloc(300, 1);
  const reply = Buffer.alloc(300, 2);
  const response = { status: () => 200, headers: () => ({}), body: async () => reply };
  const request = {
    url: () => 'https://relay.example/relay/server', method: () => 'POST',
    postDataBuffer: () => outgoing, response: async () => response,
  };
  const finish = attachBrowserCleanup(page as unknown as Page, 'private-link', config);
  return { page, request: request as unknown as Request, response, outgoing, reply, finish, observe, retirement };
}
test('default does not load an SDK or observe a browser', async () => {
  const old = process.env.QURL_NATIVE_CLEANUP_ENABLED;
  delete process.env.QURL_NATIVE_CLEANUP_ENABLED;
  try {
    await checkNativeCleanupConfig();
    expect(await observeBrowserCleanup({} as Page, 'secret')).toBeUndefined();
  } finally {
    if (old === undefined) delete process.env.QURL_NATIVE_CLEANUP_ENABLED;
    else process.env.QURL_NATIVE_CLEANUP_ENABLED = old;
  }
});
test('trusted config is parsed, never evaluated', () => {
  expect(parseNativeCleanupConfig(publicConfig)).toEqual({ issuers: { issuer: 'public' }, relays: ['relay.example'] });
  for (const invalid of [publicConfig + publicConfig,
    publicConfig.replace('nativeSessionCleanupEnabled: true', 'nativeSessionCleanupEnabled: false'),
    publicConfig.replace('visitorSessionBootstrapEnabled: true', 'visitorSessionBootstrapEnabled: false'),
    publicConfig.replace('["relay.example"]', '[]'),
    publicConfig.replace('["relay.example"]', '["https://evil.example/"]'),
    publicConfig.replace('{"issuer":"public"}', '{}')]) {
    expect(() => parseNativeCleanupConfig(invalid)).toThrow();
  }
});
test('observes paired real browser request and retires its original handle; packets are wiped', async () => {
  const f = fixture();
  f.page.emit('request', f.request);
  f.page.emit('requestfinished', f.request);
  expect(await f.finish()).toEqual({ exchanges: 1, closed: 1, denied: 0, unresolved: 0 });
  expect(f.observe).toHaveBeenCalledWith('private-link', f.outgoing, f.reply, expect.any(Object));
  expect(f.retirement).toHaveBeenCalledTimes(1);
  expect(f.outgoing.every((n) => n === 0)).toBe(true);
  expect(f.reply.every((n) => n === 0)).toBe(true);
  expect(f.page.listenerCount('request')).toBe(0);
});
test('closing and a retryable failure do not mean closed', async () => {
  jest.useFakeTimers();
  const f = fixture();
  f.retirement.mockResolvedValueOnce({ state: 'closing' }).mockRejectedValueOnce(new Error('private payload')).mockResolvedValueOnce({ state: 'closed' });
  f.page.emit('request', f.request); f.page.emit('requestfinished', f.request);
  const result = f.finish();
  await jest.runAllTimersAsync();
  expect(await result).toEqual({ exchanges: 1, closed: 1, denied: 0, unresolved: 0 });
  expect(f.retirement).toHaveBeenCalledTimes(3);
  jest.useRealTimers();
});
test('attempts all handles even when one cannot close', async () => {
  jest.useFakeTimers();
  const f = fixture();
  const first = jest.fn().mockResolvedValue({ state: 'closing' });
  f.observe.mockResolvedValueOnce({ retireSession: first });
  f.page.emit('request', f.request); f.page.emit('requestfinished', f.request);
  const second = { ...f.request } as Request;
  f.page.emit('request', second); f.page.emit('requestfinished', second);
  const result = f.finish();
  await jest.runAllTimersAsync();
  expect(await result).toEqual({ exchanges: 2, closed: 1, denied: 0, unresolved: 1 });
  expect(first.mock.calls.length).toBeGreaterThan(3);
  expect(f.retirement).toHaveBeenCalledTimes(1);
  jest.useRealTimers();
});
test.each(['unverified', 'transport', 'failed'])('no false closed on %s exchange', async (kind) => {
  const f = fixture();
  if (kind === 'unverified') f.observe.mockRejectedValue(new Error('secret link'));
  if (kind === 'transport') f.response.status = () => 503;
  f.page.emit('request', f.request);
  f.page.emit(kind === 'failed' ? 'requestfailed' : 'requestfinished', f.request);
  expect(await f.finish()).toEqual({ exchanges: 1, closed: 0, denied: 0, unresolved: 1 });
  expect(f.retirement).not.toHaveBeenCalled();
});
test('unrelated requests cannot supply authority; absent admission remains unresolved', async () => {
  const f = fixture();
  const other = { ...f.request, url: () => 'https://evil.example/relay/server' };
  f.page.emit('request', other); f.page.emit('requestfinished', other);
  expect(await f.finish()).toEqual({ exchanges: 0, closed: 0, denied: 0, unresolved: 1 });
  expect(f.observe).not.toHaveBeenCalled();
});
test('a fifth exchange is unresolved and cannot cause more cleanup network calls', async () => {
  const f = fixture();
  for (let i = 0; i < 5; i++) {
    const r = { ...f.request } as Request;
    f.page.emit('request', r); f.page.emit('requestfinished', r);
  }
  expect(await f.finish()).toEqual({ exchanges: 5, closed: 4, denied: 0, unresolved: 1 });
  expect(f.observe).toHaveBeenCalledTimes(4);
});

test('served script must be the exact new SDK bundle with SRI, not an old or duplicate script', () => {
  const sha = 'a'.repeat(64);
  const script = `<script type="module" src="/nhp-agent.${sha}.min.js" integrity="sha384-${'A'.repeat(64)}"></script>`;
  expect(nativeCleanupBundle(script, sha).path).toBe(`/nhp-agent.${sha}.min.js`);
  for (const html of ['', script + script, script.replace(sha, 'b'.repeat(64)), script.replace('sha384-', 'sha256-')]) {
    expect(() => nativeCleanupBundle(html, sha)).toThrow('served bundle mismatch');
  }
});

test.each(['current', 'old-flag', 'old-bundle', 'wrong-bytes'])('pre-fixture configuration gate: %s', async (kind) => {
  const { mkdtempSync, writeFileSync, rmSync } = require('node:fs') as typeof import('node:fs');
  const { tmpdir } = require('node:os') as typeof import('node:os');
  const { join } = require('node:path') as typeof import('node:path');
  const { createHash } = require('node:crypto') as typeof import('node:crypto');
  const dir = mkdtempSync(join(tmpdir(), 'native-cleanup-config-'));
  const oldEnv = { ...process.env };
  const oldFetch = global.fetch;
  try {
    const module = join(dir, 'portal.cjs');
    writeFileSync(module, 'module.exports={ServerDenyError:class extends Error{},TrustStore:{fromSpkiDerB64:async()=>({})},RelayAllowlist:class{},observeQurlV2SessionCleanup:async()=>{throw Error("not called")}};');
    const bundle = Buffer.from('reviewed-sdk');
    const hash = createHash('sha256').update(bundle).digest('hex');
    const sri = createHash('sha384').update(bundle).digest('base64');
    let html = publicConfig + `<script type="module" src="/nhp-agent.${hash}.min.js" integrity="sha384-${sri}"></script>`;
    if (kind === 'old-flag') html = html.replace('nativeSessionCleanupEnabled: true', 'nativeSessionCleanupEnabled: false');
    if (kind === 'old-bundle') html = html.replace(hash, 'b'.repeat(64));
    process.env.QURL_NATIVE_CLEANUP_ENABLED = '1';
    process.env.QURL_NATIVE_CLEANUP_PORTAL_MODULE = module;
    process.env.QURL_NATIVE_CLEANUP_BUNDLE_SHA256 = hash;
    process.env.MINT_API_URL = 'https://api.layerv.xyz/v1/qurls';
    process.env.QURL_PUBLIC_CONFIG_URL = 'https://qurl.link.layerv.xyz/';
    global.fetch = jest.fn().mockResolvedValueOnce(new Response(html)).mockResolvedValueOnce(new Response(kind === 'wrong-bytes' ? 'old-sdk' : bundle));
    jest.resetModules();
    const gate = require('../helpers/nativeBrowserCleanup') as typeof import('../helpers/nativeBrowserCleanup');
    if (kind === 'current') await expect(gate.checkNativeCleanupConfig()).resolves.toBeUndefined();
    else await expect(gate.checkNativeCleanupConfig()).rejects.toThrow('native cleanup configuration failed');
    expect(global.fetch).toHaveBeenCalledTimes(kind === 'old-flag' || kind === 'old-bundle' ? 1 : 2);
    for (const call of (global.fetch as jest.Mock).mock.calls) expect(call[1].redirect).toBe('error');
  } finally {
    process.env = oldEnv;
    global.fetch = oldFetch;
    rmSync(dir, { recursive: true, force: true });
  }
});

test('an unfinished browser relay is bounded and never counted closed', async () => {
  jest.useFakeTimers();
  try {
    const f = fixture();
    f.page.emit('request', f.request);
    const result = f.finish();
    await jest.runAllTimersAsync();
    expect(await result).toEqual({ exchanges: 1, closed: 0, denied: 0, unresolved: 1 });
    f.page.emit('requestfinished', f.request);
    expect(f.observe).not.toHaveBeenCalled();
    expect(f.retirement).not.toHaveBeenCalled();
  } finally { jest.useRealTimers(); }
});

test('an authenticated strict denial is no admission only in the existing negative arm', async () => {
  const f = fixture();
  f.observe.mockRejectedValue(new AuthenticatedDenial());
  f.page.emit('request', f.request); f.page.emit('requestfinished', f.request);
  const result = await f.finish();
  expect(result).toEqual({ exchanges: 1, closed: 0, denied: 1, unresolved: 0 });
  expect(nativeCleanupDisposition(result, true, false)).toBe('no_admission');
  expect(nativeCleanupDisposition(result, false, false)).toBe('unresolved');
  expect(nativeCleanupDisposition(result, true, true)).toBe('unresolved');
  expect(f.retirement).not.toHaveBeenCalled();
});
test('transport, malformed, absent and successful admissions cannot substitute for expected denial', () => {
  for (const result of [
    { exchanges: 1, closed: 0, denied: 0, unresolved: 1 },
    { exchanges: 0, closed: 0, denied: 0, unresolved: 1 },
    { exchanges: 1, closed: 1, denied: 0, unresolved: 0 },
    { exchanges: 2, closed: 0, denied: 2, unresolved: 0 },
  ]) expect(nativeCleanupDisposition(result, true, false)).toBe('unresolved');
});
