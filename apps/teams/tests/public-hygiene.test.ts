import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

const appRoot = join(import.meta.dirname, '..');
const SKIP = new Set(['node_modules', 'dist', 'coverage', '.turbo']);

function sourceFiles(dir: string): string[] {
  return readdirSync(dir).flatMap(entry => {
    if (SKIP.has(entry)) return [];
    const path = join(dir, entry);
    if (statSync(path).isDirectory()) return sourceFiles(path);
    return /\.(ts|mjs|js|json|md|ya?ml)$/.test(entry) ? [path] : [];
  });
}

const files = sourceFiles(appRoot).filter(path => !path.endsWith('package-lock.json'));

describe('public-repo hygiene', () => {
  // qurl-integrations is PUBLIC. `layerv.ai` is product surface and is fine --
  // the manifest ships those marketing URLs by design (D10). The PRE-PROD
  // domain is not: naming it here publishes internal topology, and a test
  // fixture is the easiest place for one to slip in unnoticed.
  //
  // Scoped to apps/teams deliberately. apps/cli carries ~54 pre-existing
  // references on main, so a repo-wide guard would be a migration rather than
  // a check -- the same reasoning internal/ciworkflows'
  // public_source_sanitization_test.go gives for the names it declines to ban.
  // Those are tracked separately; this keeps the new surface clean.
  it('names no pre-production or sandbox host', () => {
    const offenders = files
      .map(path => ({ path, hits: readFileSync(path, 'utf8').match(/[A-Za-z0-9.-]*layerv\.xyz/g) ?? [] }))
      .filter(entry => entry.hits.length > 0)
      .map(entry => `${entry.path.slice(appRoot.length + 1)}: ${[...new Set(entry.hits)].join(', ')}`);

    expect(offenders, 'use the *.example convention (api.sandbox.example, hub.nhp.example) instead').toEqual([]);
  });

  it('embeds no AWS account id or real ARN', () => {
    const offenders = files
      .map(path => {
        const text = readFileSync(path, 'utf8');
        // 123 is the repo's conventional throwaway account in fixture ARNs.
        const hits = (text.match(/arn:aws[a-z-]*:[a-z0-9-]+:[a-z0-9-]*:\d{6,}:/g) ?? [])
          .filter(arn => !/:(\d{1,5}):$/.test(arn));
        return { path, hits };
      })
      .filter(entry => entry.hits.length > 0)
      .map(entry => `${entry.path.slice(appRoot.length + 1)}: ${[...new Set(entry.hits)].join(', ')}`);

    expect(offenders).toEqual([]);
  });

  it('scanned a meaningful number of files', () => {
    // Guards the walker itself: a bad SKIP entry or extension filter would
    // otherwise make every assertion above vacuously pass.
    expect(files.length).toBeGreaterThan(20);
  });
});
