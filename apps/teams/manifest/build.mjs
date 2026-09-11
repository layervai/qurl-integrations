#!/usr/bin/env node
// Builds the sideloadable Teams app package.
//
// The template ships with placeholders, never real values: qurl-integrations is
// a PUBLIC repo, and the bot app id and pre-prod hostname are internal. CI in
// the private infra repo supplies them from tfvars/SSM at package time, exactly
// like the Slack manifests.
//
//   node manifest/build.mjs --env sandbox --app-id <uuid> --domain <host>
//
// Produces manifest/dist/qurl-teams-<env>.zip.

import { createHash } from 'node:crypto';
import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { deflateRawSync } from 'node:zlib';

const here = dirname(fileURLToPath(import.meta.url));

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
// Bare DNS host. No scheme, no port, no path: Teams matches validDomains
// against the host only, and a scheme here silently never matches.
const HOST = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$/;

const ENVIRONMENTS = {
  sandbox: { shortName: 'qURL (sandbox)' },
  production: { shortName: 'qURL' },
};

export function renderManifest(template, { env, appId, domain }) {
  const environment = ENVIRONMENTS[env];
  if (!environment) throw new Error(`env must be one of ${Object.keys(ENVIRONMENTS).join(', ')}`);
  if (!UUID.test(appId)) throw new Error('app-id must be the Azure Bot application (client) id as a UUID');
  if (!HOST.test(domain)) throw new Error('domain must be a bare DNS host name, with no scheme, port, or path');

  const values = { BOT_APP_ID: appId, BOT_DOMAIN: domain, APP_NAME_SHORT: environment.shortName };
  const rendered = template.replaceAll(/\$\{(\w+)\}/g, (_, key) => {
    if (!(key in values)) throw new Error(`template references unknown placeholder ${key}`);
    return values[key];
  });

  // Parse to prove the substitution produced valid JSON before anyone tries to
  // upload it, and to fail loudly if a placeholder survived.
  const manifest = JSON.parse(rendered);
  const leftovers = rendered.match(/\$\{\w+\}/g);
  if (leftovers) throw new Error(`unsubstituted placeholders: ${leftovers.join(', ')}`);
  if (manifest.name.short.length > 30) throw new Error('name.short exceeds the Teams 30-character limit');
  if (manifest.description.short.length > 80) throw new Error('description.short exceeds the Teams 80-character limit');
  if (manifest.description.full.length > 4000) throw new Error('description.full exceeds the Teams 4000-character limit');
  return rendered;
}

// Minimal deterministic zip writer. A Teams package is three files; pulling in
// an archiver dependency to place them is not worth it, and determinism (fixed
// timestamps) means the same inputs always produce the same bytes.
function zip(entries) {
  const chunks = [];
  const central = [];
  let offset = 0;
  for (const { name, data } of entries) {
    const nameBytes = Buffer.from(name, 'utf8');
    const compressed = deflateRawSync(data, { level: 9 });
    const crc = crc32(data);
    const local = Buffer.alloc(30);
    local.writeUInt32LE(0x04034b50, 0);
    local.writeUInt16LE(20, 4);
    local.writeUInt16LE(0, 6);
    local.writeUInt16LE(8, 8);
    local.writeUInt16LE(0, 10); // fixed time
    local.writeUInt16LE(0x0021, 12); // fixed date (1980-01-01)
    local.writeUInt32LE(crc, 14);
    local.writeUInt32LE(compressed.length, 18);
    local.writeUInt32LE(data.length, 22);
    local.writeUInt16LE(nameBytes.length, 26);
    local.writeUInt16LE(0, 28);
    chunks.push(local, nameBytes, compressed);

    const entry = Buffer.alloc(46);
    entry.writeUInt32LE(0x02014b50, 0);
    entry.writeUInt16LE(20, 4);
    entry.writeUInt16LE(20, 6);
    entry.writeUInt16LE(0, 8);
    entry.writeUInt16LE(8, 10);
    entry.writeUInt16LE(0, 12);
    entry.writeUInt16LE(0x0021, 14);
    entry.writeUInt32LE(crc, 16);
    entry.writeUInt32LE(compressed.length, 20);
    entry.writeUInt32LE(data.length, 24);
    entry.writeUInt16LE(nameBytes.length, 28);
    entry.writeUInt32LE(0, 42);
    entry.writeUInt32LE(offset, 42);
    central.push(entry, nameBytes);
    offset += local.length + nameBytes.length + compressed.length;
  }
  const centralBuf = Buffer.concat(central);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0);
  end.writeUInt16LE(entries.length, 8);
  end.writeUInt16LE(entries.length, 10);
  end.writeUInt32LE(centralBuf.length, 12);
  end.writeUInt32LE(offset, 16);
  return Buffer.concat([...chunks, centralBuf, end]);
}

let crcTable;
function crc32(buf) {
  if (!crcTable) {
    crcTable = new Int32Array(256);
    for (let i = 0; i < 256; i += 1) {
      let c = i;
      for (let k = 0; k < 8; k += 1) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
      crcTable[i] = c;
    }
  }
  let crc = -1;
  for (const byte of buf) crc = (crc >>> 8) ^ crcTable[(crc ^ byte) & 0xff];
  return (crc ^ -1) >>> 0;
}

export function buildPackage({ env, appId, domain }) {
  const template = readFileSync(join(here, 'manifest.template.json'), 'utf8');
  const manifest = renderManifest(template, { env, appId, domain });
  return zip([
    { name: 'manifest.json', data: Buffer.from(manifest, 'utf8') },
    { name: 'color.png', data: readFileSync(join(here, 'color.png')) },
    { name: 'outline.png', data: readFileSync(join(here, 'outline.png')) },
  ]);
}

function arg(name) {
  const i = process.argv.indexOf(`--${name}`);
  return i === -1 ? undefined : process.argv[i + 1];
}

if (process.argv[1] && import.meta.url.endsWith(process.argv[1].split('/').pop())) {
  const env = arg('env') ?? 'sandbox';
  const appId = arg('app-id');
  const domain = arg('domain');
  if (!appId || !domain) {
    process.stderr.write('usage: node manifest/build.mjs --env <sandbox|production> --app-id <uuid> --domain <host>\n');
    process.exit(2);
  }
  const out = join(here, 'dist');
  rmSync(out, { recursive: true, force: true });
  mkdirSync(out, { recursive: true });
  const pkg = buildPackage({ env, appId, domain });
  const file = join(out, `qurl-teams-${env}.zip`);
  writeFileSync(file, pkg);
  process.stdout.write(`${file}\nsha256 ${createHash('sha256').update(pkg).digest('hex')}\n`);
}
