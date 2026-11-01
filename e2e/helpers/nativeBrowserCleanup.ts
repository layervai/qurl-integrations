import type { Page, Request } from 'playwright';
import { isAbsolute } from 'node:path';
import { createHash } from 'node:crypto';

interface CleanupHandle {
  retireSession(): Promise<{ readonly state: 'closing' | 'closed' }>;
}
interface PortalSDK {
  ServerDenyError: new (code: string) => Error;
  TrustStore: { fromSpkiDerB64(keys: Record<string, string>): Promise<unknown> };
  RelayAllowlist: new (hosts: string[]) => unknown;
  observeQurlV2SessionCleanup(link: string, request: Uint8Array, reply: Uint8Array,
    options: { trustStore: unknown; relayAllowlist: unknown; authServiceId: string }): Promise<CleanupHandle>;
}
interface CleanupConfig {
  sdk: PortalSDK;
  options: { trustStore: unknown; relayAllowlist: unknown; authServiceId: string };
  relayHosts: Set<string>;
}
export interface NativeCleanupResult {
  exchanges: number;
  closed: number;
  denied: number;
  unresolved: number;
}
let configured: Promise<CleanupConfig> | undefined;

/** The workflow supplies a resolved PUBLIC package export from its pinned build. */
export async function checkNativeCleanupConfig(): Promise<void> {
  if (process.env.QURL_NATIVE_CLEANUP_ENABLED !== '1') return;
  await cleanupConfig();
}
async function cleanupConfig(): Promise<CleanupConfig> {
  if (!configured) configured = loadCleanupConfig();
  return configured;
}
async function loadCleanupConfig(): Promise<CleanupConfig> {
  try {
    if (process.env.MINT_API_URL !== 'https://api.layerv.xyz/v1/qurls' ||
        process.env.QURL_PUBLIC_CONFIG_URL !== 'https://qurl.link.layerv.xyz/') throw new Error();
    const modulePath = process.env.QURL_NATIVE_CLEANUP_PORTAL_MODULE;
    if (!modulePath || !isAbsolute(modulePath)) throw new Error();
    // Jest's loader cannot bridge the SDK's CJS export to its ESM crypto dependencies.
    const sdk = process.getBuiltinModule('module').createRequire(modulePath)(modulePath) as PortalSDK;
    if (typeof sdk.observeQurlV2SessionCleanup !== 'function' || typeof sdk.ServerDenyError !== 'function') throw new Error();
    const html = (await readPublicBody(process.env.QURL_PUBLIC_CONFIG_URL)).toString('utf8');
    const config = parseNativeCleanupConfig(html);
    const expected = process.env.QURL_NATIVE_CLEANUP_BUNDLE_SHA256;
    if (!expected || !/^[0-9a-f]{64}$/.test(expected)) throw new Error();
    const script = nativeCleanupBundle(html, expected);
    const bundle = await readPublicBody(new URL(script.path, process.env.QURL_PUBLIC_CONFIG_URL).toString());
    if (createHash('sha256').update(bundle).digest('hex') !== expected ||
        'sha384-' + createHash('sha384').update(bundle).digest('base64') !== script.integrity) throw new Error();
    return {
      sdk,
      options: {
        trustStore: await sdk.TrustStore.fromSpkiDerB64(config.issuers),
        relayAllowlist: new sdk.RelayAllowlist(config.relays), authServiceId: 'qurl',
      },
      relayHosts: new Set(config.relays),
    };
  } catch { throw new Error('native cleanup configuration failed'); }
}

async function readPublicBody(url: string): Promise<Buffer> {
  const response = await fetch(url, { redirect: 'error', signal: AbortSignal.timeout(8_000) });
  if (!response.ok || !response.body) throw new Error();
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    while (true) {
      const part = await reader.read();
      if (part.done) break;
      size += part.value.length;
      if (size > 1_048_576) throw new Error();
      chunks.push(part.value);
    }
  } finally { await reader.cancel(); }
  return Buffer.concat(chunks);
}

/** Require the normal renderer's exact new bundle and its SRI before admission. */
export function nativeCleanupBundle(html: string, expected: string): { path: string; integrity: string } {
  const scripts = [...html.matchAll(/<script type="module" src="(\/nhp-agent\.([0-9a-f]{64})\.min\.js)" integrity="(sha384-[A-Za-z0-9+/]{64})"><\/script>/g)];
  if (!/^[0-9a-f]{64}$/.test(expected) || scripts.length !== 1 || scripts[0][2] !== expected) {
    throw new Error('native cleanup served bundle mismatch');
  }
  return { path: scripts[0][1], integrity: scripts[0][3] };
}

/** Parse the existing public, rendered config without evaluating JavaScript. */
export function parseNativeCleanupConfig(html: string): { issuers: Record<string, string>; relays: string[] } {
  const blocks = [...html.matchAll(/const QURL_LINK_CONFIG = \{([\s\S]*?)\n {6}\};/g)];
  if (blocks.length !== 1) throw new Error('native cleanup public config invalid');
  const block = blocks[0][1];
  const one = (pattern: RegExp): string => {
    const matches = [...block.matchAll(pattern)];
    if (matches.length !== 1) throw new Error('native cleanup public config invalid');
    return matches[0][1];
  };
  if (one(/^ {8}nativeSessionCleanupEnabled: (true|false),$/gm) !== 'true' ||
      one(/^ {8}visitorSessionBootstrapEnabled: (true|false),$/gm) !== 'true') {
    throw new Error('native cleanup is not enabled');
  }
  const issuers: unknown = JSON.parse(one(/^ {8}issuerTrustStore: (\{[^\r\n]*\}),$/gm));
  const relays: unknown = JSON.parse(one(/^ {8}relayAllowlist: (\[[^\r\n]*\])$/gm));
  if (!issuers || typeof issuers !== 'object' || Array.isArray(issuers) ||
      !Object.keys(issuers).length || Object.entries(issuers).some(([k, v]) => !k || typeof v !== 'string') ||
      !Array.isArray(relays) || !relays.length || relays.some((v) => typeof v !== 'string' || !/^[a-z0-9.-]+(?::[0-9]+)?$/.test(v))) {
    throw new Error('native cleanup public config invalid');
  }
  return { issuers: issuers as Record<string, string>, relays: relays as string[] };
}

export async function observeBrowserCleanup(page: Page, link: string): Promise<(() => Promise<NativeCleanupResult>) | undefined> {
  if (process.env.QURL_NATIVE_CLEANUP_ENABLED !== '1') return undefined;
  return attachBrowserCleanup(page, link, await cleanupConfig());
}

/** Retain only original observed exchanges in Node memory across page navigation.
 * No route interception, page hook, second admission, or persisted capability. */
export function attachBrowserCleanup(page: Page, link: string, config: CleanupConfig): () => Promise<NativeCleanupResult> {
  const pending = new Set<Request>();
  const tasks: Promise<void>[] = [];
  const handles: CleanupHandle[] = [];
  const result: NativeCleanupResult = { exchanges: 0, closed: 0, denied: 0, unresolved: 0 };
  const matches = (request: Request): boolean => {
    try {
      const url = new URL(request.url());
      return request.method() === 'POST' && url.protocol === 'https:' && !url.username && !url.password &&
        !url.search && !url.hash && config.relayHosts.has(url.host) && /^\/relay\/[A-Za-z0-9_-]+$/.test(url.pathname);
    } catch { return false; }
  };
  const started = (request: Request) => {
    if (!matches(request)) return;
    if (++result.exchanges > 4) { result.unresolved++; return; }
    pending.add(request);
  };
  const finished = (request: Request) => {
    if (!pending.delete(request)) return;
    tasks.push((async () => {
      let outgoing: Buffer | null = null;
      let reply: Buffer | undefined;
      try {
        outgoing = request.postDataBuffer();
        const response = await request.response();
        if (!outgoing || outgoing.length <= 240 || outgoing.length > 4096 || !response || response.status() !== 200) throw new Error();
        const length = response.headers()['content-length'];
        if (length && (!/^[0-9]+$/.test(length) || Number(length) > 4096)) throw new Error();
        reply = await response.body(); // requestfinished means the bounded relay fetch has ended.
        if (reply.length <= 240 || reply.length > 4096) throw new Error();
        handles.push(await config.sdk.observeQurlV2SessionCleanup(link, outgoing, reply, config.options));
      } catch (error) {
        if (error instanceof config.sdk.ServerDenyError) result.denied++;
        else result.unresolved++;
      }
      finally { outgoing?.fill(0); reply?.fill(0); }
    })());
  };
  const failed = (request: Request) => {
    if (pending.delete(request)) result.unresolved++;
  };
  page.on('request', started);
  page.on('requestfinished', finished);
  page.on('requestfailed', failed);
  return async () => {
    // The existing browser relay transport has a 10-second timeout. Allow it to
    // finish before removing listeners, including when a content assertion fails.
    const deadline = Date.now() + 12_000;
    while (pending.size && Date.now() < deadline) await new Promise((resolve) => setTimeout(resolve, 25));
    page.off('request', started);
    page.off('requestfinished', finished);
    page.off('requestfailed', failed);
    result.unresolved += pending.size;
    await Promise.all(tasks);
    // Match the normal owned Go cleanup: one five-minute group budget,
    // all pending generations per round, ten-second SDK calls, five-second pace.
    const cleanupDeadline = Date.now() + 5 * 60_000;
    let remaining = handles;
    while (remaining.length && Date.now() + 10_000 <= cleanupDeadline) {
      const states = await Promise.all(remaining.map(async (handle) => {
        try { return (await handle.retireSession()).state === 'closed'; }
        catch { return false; } // A retryable failure never drops its original key.
      }));
      result.closed += states.filter(Boolean).length;
      remaining = remaining.filter((_, index) => !states[index]);
      if (remaining.length) {
        const wait = Math.min(5_000, cleanupDeadline - Date.now());
        if (wait > 0) await new Promise((resolve) => setTimeout(resolve, wait));
      }
    }
    result.unresolved += remaining.length;
    if (!result.exchanges) result.unresolved++;
    return { ...result };
  };
}

/** A denied attempt is not a closed session. Require its authenticated exact pair. */
export function nativeCleanupDisposition(result: NativeCleanupResult, expectedNoAdmission: boolean, contentPassed: boolean): 'closed' | 'no_admission' | 'unresolved' {
  if (expectedNoAdmission) {
    return !contentPassed && result.exchanges === 1 && result.denied === 1 && result.closed === 0 && result.unresolved === 0
      ? 'no_admission' : 'unresolved';
  }
  return result.closed > 0 && result.closed === result.exchanges && result.denied === 0 && result.unresolved === 0
    ? 'closed' : 'unresolved';
}
