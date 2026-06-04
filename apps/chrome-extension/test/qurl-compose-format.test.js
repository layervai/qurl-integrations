const test = require('node:test');
const assert = require('node:assert/strict');

const formatter = require('../lib/qurl-compose-format.js');

test('buildLinkHtml escapes filenames and URLs', function () {
  const html = formatter.buildLinkHtml([
    {
      filename: 'report & <draft>.pdf',
      link: 'https://example.com/q/abc?x=1&y=2',
      expiry: null,
    },
  ]);

  assert.match(html, /report &amp; &lt;draft&gt;\.pdf/);
  assert.match(html, /https:\/\/example\.com\/q\/abc\?x=1&amp;y=2/);
});

test('buildLinkHtml localizes unnamed-file fallbacks', function () {
  const originalChrome = global.chrome;
  global.chrome = {
    i18n: {
      getMessage(key) {
        if (key === 'unnamed_file') {
          return 'Localized unnamed file';
        }
        return '';
      },
    },
  };

  try {
    const html = formatter.buildLinkHtml([{
      filename: '',
      link: 'https://example.com/q/abc',
      expiry: null,
    }]);

    assert.match(html, /Localized unnamed file/);
  } finally {
    global.chrome = originalChrome;
  }
});

test('buildLinkHtml drops unsafe link schemes and invalid expiries', function () {
  const html = formatter.buildLinkHtml([
    {
      filename: 'dangerous.html',
      link: 'javascript:alert(1)',
      expiry: 'not-a-date',
    },
  ]);

  assert.doesNotMatch(html, /href=/);
  assert.doesNotMatch(html, /javascript:/i);
  assert.doesNotMatch(html, /Expires:/);
  assert.match(html, /dangerous\.html/);
});

test('buildLinkPlainText keeps plain text output readable', function () {
  const text = formatter.buildLinkPlainText([
    {
      filename: 'report.pdf',
      link: 'https://example.com/q/abc',
      expiry: '2026-05-01T12:00:00Z',
    },
  ]);

  assert.match(text, /^report\.pdf: https:\/\/example\.com\/q\/abc \(Expires: /);
});

test('buildLinkPlainText supports multiple results and omits null expiries', function () {
  const formattedExpiry = formatter.formatExpiry('2026-05-02T08:30:00Z');
  const text = formatter.buildLinkPlainText([
    {
      filename: 'report.pdf',
      link: 'https://example.com/q/abc',
      expiry: null,
    },
    {
      filename: 'notes.txt',
      link: 'https://example.com/q/def',
      expiry: '2026-05-02T08:30:00Z',
    },
  ]);

  assert.equal(
    text,
    `report.pdf: https://example.com/q/abc\nnotes.txt: https://example.com/q/def (Expires: ${formattedExpiry})`
  );
});

test('normalizeAllowedLink only accepts https URLs', function () {
  assert.equal(formatter.normalizeAllowedLink('https://example.com/q/abc'), 'https://example.com/q/abc');
  assert.equal(formatter.normalizeAllowedLink('http://example.com/q/abc'), null);
  assert.equal(formatter.normalizeAllowedLink('javascript:alert(1)'), null);
  assert.equal(formatter.normalizeAllowedLink('data:text/html,hello'), null);
  assert.equal(formatter.normalizeAllowedLink('not-a-url'), null);
});

test('formatExpiry returns null for invalid timestamps', function () {
  assert.equal(formatter.formatExpiry('never'), null);
});

test('buildLinkHtml uses the localized expiry suffix when chrome.i18n is available', function () {
  const originalChrome = global.chrome;
  global.chrome = {
    i18n: {
      getMessage(key, substitutions) {
        if (key === 'expiry_suffix') {
          return ` [Ends ${substitutions[0]}]`;
        }
        return '';
      },
    },
  };

  try {
    const formattedExpiry = formatter.formatExpiry('2026-05-01T12:00:00Z');
    const html = formatter.buildLinkHtml([{
      filename: 'report.pdf',
      link: 'https://example.com/q/abc',
      expiry: '2026-05-01T12:00:00Z',
    }]);

    // Literal contains-check (not a RegExp): the formatted expiry includes a UTC offset like
    // "+0000", and "+" is a regex metacharacter that would otherwise break the match.
    assert.ok(html.includes(`[Ends ${formattedExpiry}]`), `expected localized suffix in: ${html}`);
  } finally {
    global.chrome = originalChrome;
  }
});
