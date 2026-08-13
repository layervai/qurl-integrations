const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

// A key referenced by the code but absent from messages.json does not throw: getMessage
// falls back to its hard-coded English second argument, so the string silently stops being
// localizable and nothing in CI notices. _locales/ is deliberately outside the Chrome/Edge
// lockstep check (see CLAUDE.md), and this is the guard on that fallback path.
//
// Scope, deliberately: everything here is within ONE app and never looks at message text.
// Cross-app parity — identical key sets, the sanctioned per-browser wording deltas, and the
// __MSG_*__ references in manifest.json, which SOURCE_ROOTS below does not scan — belongs to
// scripts/check-i18n-parity.sh. It has to live outside this file: this is itself a lockstep
// file, and the lockstep normalization masks Chrome/Edge on both sides, so an assertion here
// pinning browser-specific copy would be erased before the comparison.

const ROOT = path.join(__dirname, '..');

// Roots that ship or build the extension. test/ is excluded: its fixtures reference
// deliberately-absent keys to exercise the fallback path.
const SOURCE_ROOTS = ['background.js', 'content', 'lib', 'popup', 'scripts'];

// Matches getMessage('key', …) and apiGetMessage('key', …) — literal keys only.
// The [gG] is load-bearing: lib/qurl-api.js calls the wrapper as apiGetMessage with a
// capital G, so a lowercase-only pattern silently skips that whole file. Calls that pass
// a variable (chrome.i18n.getMessage(key), QURLI18n.getMessage(key)) cannot be checked
// statically and are skipped by the string-literal requirement.
const JS_KEY = /\b(?:api)?[gG]etMessage\(\s*'([a-zA-Z0-9_]+)'/g;

// popup.html localizes nodes declaratively; these attributes name catalog keys too.
const HTML_KEY = /\bdata-i18n(?:-attr-key)?="([a-zA-Z0-9_]+)"/g;

function walk(entry) {
  const abs = path.join(ROOT, entry);
  if (!fs.existsSync(abs)) return [];
  if (fs.statSync(abs).isFile()) return [abs];
  return fs.readdirSync(abs).flatMap((child) => walk(path.join(entry, child)));
}

function collect(files, pattern, extensions) {
  const found = new Map();
  for (const file of files) {
    if (!extensions.some((ext) => file.endsWith(ext))) continue;
    const source = fs.readFileSync(file, 'utf8');
    for (const match of source.matchAll(pattern)) {
      if (!found.has(match[1])) found.set(match[1], path.relative(ROOT, file));
    }
  }
  return found;
}

const catalogPath = path.join(ROOT, '_locales/en/messages.json');
const catalog = JSON.parse(fs.readFileSync(catalogPath, 'utf8'));
const files = SOURCE_ROOTS.flatMap(walk);

test('every getMessage key used in source is declared in _locales/en/messages.json', function () {
  const used = collect(files, JS_KEY, ['.js']);
  assert.ok(used.size > 0, 'found no getMessage call sites — the scan is broken, not the catalog');

  const missing = [...used].filter(([key]) => !(key in catalog));
  assert.deepEqual(
    missing.map(([key, file]) => `${key} (${file})`),
    [],
    'keys referenced in source but missing from the message catalog'
  );
});

test('every data-i18n key used in markup is declared in _locales/en/messages.json', function () {
  const used = collect(files, HTML_KEY, ['.html']);
  assert.ok(used.size > 0, 'found no data-i18n attributes — the scan is broken, not the catalog');

  const missing = [...used].filter(([key]) => !(key in catalog));
  assert.deepEqual(
    missing.map(([key, file]) => `${key} (${file})`),
    [],
    'keys referenced in markup but missing from the message catalog'
  );
});

test('the scan reaches the apiGetMessage call sites in lib/qurl-api.js', function () {
  // lib/qurl-api.js calls the wrapper under its renamed identifier (apiGetMessage) to
  // avoid a shared-global collision with popup.js in the popup page. That file holds
  // most of the error keys, and the suite above keeps passing on other files' keys if
  // the pattern stops reaching it — so assert the coverage directly rather than trusting
  // the aggregate count.
  const used = collect([path.join(ROOT, 'lib/qurl-api.js')], JS_KEY, ['.js']);

  assert.ok(
    used.size > 0,
    'no keys found in lib/qurl-api.js — JS_KEY no longer matches apiGetMessage(...)'
  );

  const missing = [...used].filter(([key]) => !(key in catalog));
  assert.deepEqual(
    missing.map(([key]) => key),
    [],
    'keys referenced in lib/qurl-api.js but missing from the message catalog'
  );
});

test('every catalog entry has a non-empty message', function () {
  const empty = Object.entries(catalog)
    .filter(([, entry]) => typeof entry.message !== 'string' || entry.message.trim() === '')
    .map(([key]) => key);

  assert.deepEqual(empty, [], 'catalog entries with a missing or empty message');
});
