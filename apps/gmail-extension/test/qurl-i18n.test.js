const test = require('node:test');
const assert = require('node:assert/strict');

const qurlI18n = require('../lib/qurl-i18n.js');

test('applyFallbackSubstitutions replaces repeated placeholders and preserves dollar signs in values', function () {
  const result = qurlI18n.applyFallbackSubstitutions(
    'File $1 was uploaded twice: $1',
    ['report$5.pdf']
  );

  assert.equal(result, 'File report$5.pdf was uploaded twice: report$5.pdf');
});

test('applyFallbackSubstitutions handles multi-digit placeholders without corrupting lower indices', function () {
  const substitutions = ['one', 'two', 'three', 'four', 'five', 'six', 'seven', 'eight', 'nine', 'ten$2'];
  const result = qurlI18n.applyFallbackSubstitutions('Values: $10 then $1 then $2', substitutions);

  assert.equal(result, 'Values: ten$2 then one then two');
});
