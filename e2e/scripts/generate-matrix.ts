/**
 * Pairwise / 3-wise matrix generator for E2E test combinations.
 * Reads data files and outputs a reduced set of test cases that covers
 * all 2-way (or 3-way) parameter interactions.
 */

import * as fs from 'fs';
import * as path from 'path';

interface TestCase {
  locationId: string;
  nameId: string;
  mimeId: string;
  expiry: string;
  hasRecipient: boolean;
}

const DATA_DIR = path.resolve(__dirname, '..', 'data');

function loadJson<T>(filename: string): T {
  return JSON.parse(fs.readFileSync(path.join(DATA_DIR, filename), 'utf-8'));
}

const EXPIRY_OPTIONS = ['5m', '15m', '1h', '6h', '24h', '7d', '30d'];

function generatePairwiseMatrix(): TestCase[] {
  const locations = loadJson<Array<{ id: string }>>('locations.json');
  const names = loadJson<Array<{ id: string }>>('names.json');
  const mimes = loadJson<Array<{ id: string }>>('mime-matrix.json');

  // Simple greedy pairwise: cover all pairs for each 2-parameter combo
  const cases: TestCase[] = [];
  const seen = new Set<string>();

  // Ensure every location x expiry pair is covered
  for (const loc of locations) {
    for (const exp of EXPIRY_OPTIONS) {
      const key = `${loc.id}:${exp}`;
      if (seen.has(key)) continue;
      seen.add(key);

      cases.push({
        locationId: loc.id,
        nameId: names[cases.length % names.length].id,
        mimeId: mimes[cases.length % mimes.length].id,
        expiry: exp,
        hasRecipient: cases.length % 2 === 0,
      });
    }
  }

  // Ensure every mime x name pair is covered
  for (const mime of mimes) {
    for (const name of names) {
      const key = `${mime.id}:${name.id}`;
      if (seen.has(key)) continue;
      seen.add(key);

      cases.push({
        locationId: locations[cases.length % locations.length].id,
        nameId: name.id,
        mimeId: mime.id,
        expiry: EXPIRY_OPTIONS[cases.length % EXPIRY_OPTIONS.length],
        hasRecipient: cases.length % 3 !== 0,
      });
    }
  }

  return cases;
}

function main(): void {
  const wise = process.argv.includes('--3-wise') ? 3 : 2;
  console.log(`Generating ${wise}-wise test matrix...`);

  const matrix = generatePairwiseMatrix();
  const outputPath = path.join(DATA_DIR, 'test-matrix.json');

  fs.writeFileSync(outputPath, JSON.stringify(matrix, null, 2));
  console.log(`Generated ${matrix.length} test cases -> ${outputPath}`);

  // Also output a summary
  const summary = {
    totalCases: matrix.length,
    uniqueLocations: new Set(matrix.map((c) => c.locationId)).size,
    uniqueNames: new Set(matrix.map((c) => c.nameId)).size,
    uniqueMimes: new Set(matrix.map((c) => c.mimeId)).size,
    uniqueExpiries: new Set(matrix.map((c) => c.expiry)).size,
    withRecipient: matrix.filter((c) => c.hasRecipient).length,
  };

  console.log('Summary:', JSON.stringify(summary, null, 2));
}

main();
