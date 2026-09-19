import { spawnSync } from 'node:child_process';
import { copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { describe, expect, it } from 'vitest';
// @ts-expect-error -- plain ESM build script, intentionally not TypeScript.
import { buildPackage, renderManifest } from '../manifest/build.mjs';

const template = readFileSync(join(import.meta.dirname, '..', 'manifest', 'manifest.template.json'), 'utf8');
const buildScript = join(import.meta.dirname, '..', 'manifest', 'build.mjs');
const BOT_UUID = 'a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d';
const DOMAIN = 'teams.connector.example';

const render = (overrides: Record<string, string> = {}) =>
  renderManifest(template, { env: 'sandbox', appId: BOT_UUID, domain: DOMAIN, ...overrides });

describe('teams app manifest', () => {
  it('does not run the CLI when imported by another build.mjs', () => {
    const dir = mkdtempSync(join(tmpdir(), 'qurl teams manifest '));
    const entrypoint = join(dir, 'build.mjs');
    writeFileSync(entrypoint, `import ${JSON.stringify(pathToFileURL(buildScript).href)};\nprocess.stdout.write('imported');\n`);
    try {
      const result = spawnSync(process.execPath, [entrypoint], { encoding: 'utf8' });
      expect(result.status, result.stderr).toBe(0);
      expect(result.stdout).toBe('imported');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('runs the CLI when invoked through a differently named symlink', () => {
    const dir = mkdtempSync(join(tmpdir(), 'qurl teams manifest '));
    const entrypoint = join(dir, 'qurl-package');
    symlinkSync(buildScript, entrypoint);
    try {
      const result = spawnSync(process.execPath, [entrypoint], { encoding: 'utf8' });
      expect(result.status, result.stderr).toBe(2);
      expect(result.stderr).toContain('usage: node manifest/build.mjs');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('retains the previous package when CLI input validation fails', () => {
    const dir = mkdtempSync(join(tmpdir(), 'qurl teams manifest '));
    const previous = buildPackage({ env: 'sandbox', appId: BOT_UUID, domain: DOMAIN });
    const output = join(dir, 'dist', 'qurl-teams-sandbox.zip');
    try {
      copyFileSync(buildScript, join(dir, 'build.mjs'));
      writeFileSync(join(dir, 'manifest.template.json'), template);
      mkdirSync(join(dir, 'dist'));
      writeFileSync(output, previous);
      const result = spawnSync(process.execPath, [join(dir, 'build.mjs'), '--env', 'constructor', '--app-id', BOT_UUID, '--domain', DOMAIN], { encoding: 'utf8' });
      expect(result.status).not.toBe(0);
      expect(result.stderr).toContain('env must be one of');
      expect(readFileSync(output).equals(previous)).toBe(true);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('ships no real identifiers in the committed template', () => {
    // qurl-integrations is PUBLIC. The public marketing URLs on layerv.ai are
    // fine and are locked by D10; what must never appear here is the pre-prod
    // domain or any sandbox hostname. The operator supplies the host and bot
    // app id at package time.
    expect(template).not.toMatch(/layerv\.xyz/);
    expect(template).not.toMatch(/teams\.connector\./);
    expect(template).not.toMatch(/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i);
    expect(template).toContain('${BOT_APP_ID}');
    expect(template).toContain('${BOT_DOMAIN}');
  });

  it('pins the GA manifest schema version', () => {
    // Pin a supported version; this bot does not need newer schema features.
    expect(JSON.parse(render()).manifestVersion).toBe('1.28');
  });

  it('substitutes every placeholder into a valid manifest', () => {
    const manifest = JSON.parse(render());
    expect(manifest.id).toBe(BOT_UUID);
    expect(manifest.bots[0].botId).toBe(BOT_UUID);
    // Authentication uses our external Auth0 flow, not Teams SSO.
    expect(manifest).not.toHaveProperty('webApplicationInfo');
    expect(manifest.validDomains).toEqual([DOMAIN]);
    expect(render()).not.toMatch(/\$\{\w+\}/);
  });

  it('names the app per environment', () => {
    expect(JSON.parse(render()).name.short).toBe('qURL (sandbox)');
    expect(JSON.parse(render({ env: 'production' })).name.short).toBe('qURL');
    for (const env of ['staging', 'constructor', 'toString', 'valueOf', '__proto__']) {
      expect(() => render({ env })).toThrow('env must be one of');
    }
  });

  it('rejects unknown placeholders including inherited object properties', () => {
    for (const key of ['UNKNOWN', 'constructor', 'toString', '__proto__']) {
      const invalid = template.replace('${APP_NAME_SHORT}', `\${${key}}`);
      expect(() => renderManifest(invalid, { env: 'sandbox', appId: BOT_UUID, domain: DOMAIN })).toThrow(`template references unknown placeholder ${key}`);
    }
  });

  it('rejects an app id or domain Teams would silently not match', () => {
    expect(() => render({ appId: 'not-a-uuid' })).toThrow('app-id must be');
    // A scheme in validDomains never matches: Teams compares host only.
    expect(() => render({ domain: 'https://teams.connector.example' })).toThrow('domain must be');
    expect(() => render({ domain: 'teams.connector.example:443' })).toThrow('domain must be');
    expect(() => render({ domain: 'localhost' })).toThrow('domain must be');
  });

  it('declares both install scopes and the connector commands', () => {
    const bot = JSON.parse(render()).bots[0];
    expect(bot.scopes).toEqual(['personal', 'team']);
    const titles = bot.commandLists.flatMap((list: { commands: { title: string }[] }) => list.commands.map(c => c.title));
    // `setup` must be discoverable in personal scope: the setup link is only
    // ever delivered to a personal chat, so that is where users start.
    expect(titles).toContain('setup');
    expect(titles).toContain('protect-connector');
    expect(titles).toContain('get');
    const scopes = bot.commandLists.flatMap((list: { scopes: string[] }) => list.scopes);
    expect(scopes).toEqual(['personal', 'team']);
    const personal = bot.commandLists.find((list: { scopes: string[] }) => list.scopes.includes('personal'));
    for (const channelCommand of ['list', 'aliases', 'get']) {
      expect(personal.commands.map((command: { title: string }) => command.title)).not.toContain(channelCommand);
    }
  });

  it('stays inside the Teams field length limits', () => {
    const manifest = JSON.parse(render({ env: 'production' }));
    expect(manifest.name.short.length).toBeLessThanOrEqual(30);
    expect(manifest.description.short.length).toBeLessThanOrEqual(80);
    expect(manifest.description.full.length).toBeLessThanOrEqual(4000);
  });

  it('builds a deterministic three-file package', () => {
    const first = buildPackage({ env: 'sandbox', appId: BOT_UUID, domain: DOMAIN });
    const second = buildPackage({ env: 'sandbox', appId: BOT_UUID, domain: DOMAIN });
    // Byte-identical rebuilds let the infra repo pin a package by digest.
    expect(first.equals(second)).toBe(true);
    const text = first.toString('latin1');
    for (const entry of ['manifest.json', 'color.png', 'outline.png']) {
      expect(text).toContain(entry);
    }
  });
});
