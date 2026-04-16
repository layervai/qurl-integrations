/**
 * Look up test data by ID from the JSON data files.
 */

import * as path from 'path';
import * as fs from 'fs';

const DATA_DIR = path.resolve(__dirname, '..', 'data');

export interface Location {
  id: string;
  url: string;
  label: string;
  type: 'http' | 'https' | 's3' | 'gcs' | 'local';
  expectedStatus?: number;
}

export interface NameEntry {
  id: string;
  filename: string;
  message: string;
}

export interface MimeEntry {
  id: string;
  extension: string;
  mimeType: string;
  sizeBytes: number;
  label: string;
}

let locationsCache: Location[] | null = null;
let namesCache: NameEntry[] | null = null;
let mimeCache: MimeEntry[] | null = null;

function loadJson<T>(filename: string): T {
  const filePath = path.join(DATA_DIR, filename);
  const raw = fs.readFileSync(filePath, 'utf-8');
  return JSON.parse(raw) as T;
}

export function getLocations(): Location[] {
  if (!locationsCache) {
    locationsCache = loadJson<Location[]>('locations.json');
  }
  return locationsCache;
}

export function getLocation(id: string): Location {
  const loc = getLocations().find((l) => l.id === id);
  if (!loc) throw new Error(`Location not found: ${id}`);
  return loc;
}

export function getNames(): NameEntry[] {
  if (!namesCache) {
    namesCache = loadJson<NameEntry[]>('names.json');
  }
  return namesCache;
}

export function getName(id: string): NameEntry {
  const name = getNames().find((n) => n.id === id);
  if (!name) throw new Error(`Name entry not found: ${id}`);
  return name;
}

export function getMimeMatrix(): MimeEntry[] {
  if (!mimeCache) {
    mimeCache = loadJson<MimeEntry[]>('mime-matrix.json');
  }
  return mimeCache;
}

export function getMime(id: string): MimeEntry {
  const mime = getMimeMatrix().find((m) => m.id === id);
  if (!mime) throw new Error(`MIME entry not found: ${id}`);
  return mime;
}

/**
 * Get the path to a generated test file.
 */
export function testFilePath(filename: string): string {
  return path.join(DATA_DIR, 'files', filename);
}
