import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
// @ts-expect-error -- plain ESM build script, intentionally not TypeScript.
import { buildPackage, renderManifest } from '../manifest/build.mjs';

const template = readFileSync(join(import.meta.dirname, '..', 'manifest', 'manifest.template.json'), 'utf8');
const BOT_UUID = 'a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d';
const DOMAIN = 'teams.connector.layerv.xyz';

const render = (overrides: Record<string, string> = {}) =>
  renderManifest(template, { env: 'sandbox', appId: BOT_UUID, domain: DOMAIN, ...overrides });

describe('teams app manifest', () => {
  it('ships no real identifiers in the committed template', () => {
    // qurl-integrations is PUBLIC. The public marketing URLs on layerv.ai are
    // fine and are locked by D10; what must never appear here is the pre-prod
    // domain or any sandbox hostname. CI in the private infra repo substitutes
    // the host and the bot app id at package time.
    expect(template).not.toMatch(/layerv\.xyz/);
    expect(template).not.toMatch(/teams\.connector\./);
    expect(template).not.toMatch(/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i);
    expect(template).toContain('${BOT_APP_ID}');
    expect(template).toContain('${BOT_DOMAIN}');
  });

  it('pins the GA manifest schema version', () => {
    // 1.29 is described by Microsoft but is NOT on the "generally available"
    // list. Sideloading a non-GA manifest version is rejected.
    expect(JSON.parse(render()).manifestVersion).toBe('1.28');
  });

  it('substitutes every placeholder into a valid manifest', () => {
    const manifest = JSON.parse(render());
    expect(manifest.id).toBe(BOT_UUID);
    expect(manifest.bots[0].botId).toBe(BOT_UUID);
    expect(manifest.webApplicationInfo.id).toBe(BOT_UUID);
    expect(manifest.validDomains).toEqual([DOMAIN]);
    expect(render()).not.toMatch(/\$\{\w+\}/);
  });

  it('names the app per environment', () => {
    expect(JSON.parse(render()).name.short).toBe('qURL (sandbox)');
    expect(JSON.parse(render({ env: 'production' })).name.short).toBe('qURL');
    expect(() => render({ env: 'staging' })).toThrow('env must be one of');
  });

  it('rejects an app id or domain Teams would silently not match', () => {
    expect(() => render({ appId: 'not-a-uuid' })).toThrow('app-id must be');
    // A scheme in validDomains never matches: Teams compares host only.
    expect(() => render({ domain: 'https://teams.connector.layerv.xyz' })).toThrow('domain must be');
    expect(() => render({ domain: 'teams.connector.layerv.xyz:443' })).toThrow('domain must be');
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
