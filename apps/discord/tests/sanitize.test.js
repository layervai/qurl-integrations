/**
 * Tests for src/utils/sanitize.js — filename sanitization and Discord
 * markdown escaping. The markdown escape is the primary defense against
 * masked-link phishing in embeds, so adversarial coverage matters.
 */

const { sanitizeFilename, escapeDiscordMarkdown, sanitizeContentLabel, stripBidiAndControls, sanitizeDisplayNamePlain } = require('../src/utils/sanitize');

describe('sanitizeFilename', () => {
  it('strips path traversal', () => {
    expect(sanitizeFilename('../../etc/passwd')).not.toContain('..');
    expect(sanitizeFilename('a/../b.txt')).not.toContain('/');
  });

  it('strips control characters', () => {
    // eslint-disable-next-line no-control-regex
    expect(sanitizeFilename('foo\x00bar.bin')).not.toMatch(/\x00/);
    // eslint-disable-next-line no-control-regex
    expect(sanitizeFilename('line1\nline2.txt')).not.toMatch(/\n/);
  });

  it('neutralizes leading-dot hidden-file names', () => {
    expect(sanitizeFilename('.env')).toBe('_env');
    expect(sanitizeFilename('...hidden')).toMatch(/^_/);
  });

  it('returns "unnamed_file" on empty / null / sanitized-to-nothing input', () => {
    expect(sanitizeFilename('')).toBe('unnamed_file');
    expect(sanitizeFilename(null)).toBe('unnamed_file');
    expect(sanitizeFilename(undefined)).toBe('unnamed_file');
    // eslint-disable-next-line no-control-regex
    expect(sanitizeFilename('\x00\x01\x02')).toBe('unnamed_file');
  });

  it('caps at 200 chars', () => {
    expect(sanitizeFilename('a'.repeat(500)).length).toBeLessThanOrEqual(200);
  });

  it('replaces shell/path metacharacters', () => {
    expect(sanitizeFilename('file<>:"|?*\\name')).not.toMatch(/[<>:"|?*\\]/);
  });
});

describe('escapeDiscordMarkdown', () => {
  it('escapes masked-link syntax (primary phishing vector)', () => {
    const evil = '[Free Nitro](https://evil.example.com)';
    const out = escapeDiscordMarkdown(evil);
    expect(out).not.toContain('[Free Nitro]');
    expect(out).toContain('\\[');
    expect(out).toContain('\\]');
    expect(out).toContain('\\(');
    expect(out).toContain('\\)');
  });

  it('escapes bold / italic / underline / code / strikethrough / spoiler', () => {
    expect(escapeDiscordMarkdown('**bold**')).toContain('\\*\\*');
    expect(escapeDiscordMarkdown('__underline__')).toContain('\\_\\_');
    expect(escapeDiscordMarkdown('`code`')).toContain('\\`');
    expect(escapeDiscordMarkdown('~~strike~~')).toContain('\\~\\~');
    expect(escapeDiscordMarkdown('||spoiler||')).toContain('\\|\\|');
  });

  it('escapes block-quote markers', () => {
    expect(escapeDiscordMarkdown('> quote')).toContain('\\>');
  });

  it('escapes backslash itself', () => {
    expect(escapeDiscordMarkdown('a\\b')).toContain('\\\\');
  });

  it('returns empty for null/undefined', () => {
    expect(escapeDiscordMarkdown(null)).toBe('');
    expect(escapeDiscordMarkdown(undefined)).toBe('');
  });

  it('leaves safe text alone', () => {
    const safe = 'Hello world 123 - plain text.';
    expect(escapeDiscordMarkdown(safe)).toBe(safe);
  });

  it('coerces non-strings', () => {
    expect(escapeDiscordMarkdown(42)).toBe('42');
    expect(escapeDiscordMarkdown(true)).toBe('true');
  });
});

describe('sanitizeContentLabel', () => {
  it('strips bidi/zero-width chars before markdown-escape', () => {
    const rlo = String.fromCharCode(0x202E);
    const zwsp = String.fromCharCode(0x200B);
    expect(sanitizeContentLabel(`${rlo}Backwards${zwsp}Cafe`)).toBe('BackwardsCafe');
  });

  it('escapes markdown after the strip pass', () => {
    expect(sanitizeContentLabel('**bold** [link](url)'))
      .toBe('\\*\\*bold\\*\\* \\[link\\]\\(url\\)');
  });

  it('caps at the supplied codepoint count, surrogate-pair safe', () => {
    // 254 ASCII + emoji surrogate pair = 256 codepoints (NFKC keeps
    // the emoji as a pair), so the cap is not hit.
    const exact = 'a'.repeat(254) + '\u{1F600}';
    expect(sanitizeContentLabel(exact, 256)).toBe(exact);
    // 255 ASCII + emoji (256 codepoints) — at the boundary. Naive
    // .slice(0, 256) on UTF-16 code units would land mid-surrogate.
    const boundary = 'a'.repeat(255) + '\u{1F600}';
    const out = sanitizeContentLabel(boundary, 256);
    const lone = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/;
    expect(out).not.toMatch(lone);
  });

  it('returns empty for null/undefined/empty (NOT the display-name Someone fallback)', () => {
    expect(sanitizeContentLabel(null)).toBe('');
    expect(sanitizeContentLabel(undefined)).toBe('');
    expect(sanitizeContentLabel('')).toBe('');
  });

  it('returns empty for input that becomes empty after strip', () => {
    // All zero-width — display-name helper falls back to 'Someone'
    // here, but content-label callers want '' (the
    // locationName/resourceLabel empty branch then renders nothing
    // or a default).
    expect(sanitizeContentLabel('​‌‍')).toBe('');
  });
});

describe('stripBidiAndControls', () => {
  it('strips RLO / ZWSP / control codepoints without escaping markdown', () => {
    const rlo = String.fromCharCode(0x202E);
    const zwsp = String.fromCharCode(0x200B);
    const out = stripBidiAndControls(`Hello${rlo}World${zwsp}!`);
    expect(out).toBe('HelloWorld!');
  });

  it('does NOT escape markdown chars (sanitizeContentLabel does, this helper does not)', () => {
    // The personal-message path needs to layer bidi-strip BEFORE its
    // own markdown escape; this helper must not pre-escape.
    expect(stripBidiAndControls('**bold**')).toBe('**bold**');
    expect(stripBidiAndControls('[link](url)')).toBe('[link](url)');
  });

  it('NFKC-normalizes before strip', () => {
    // U+FEFF (BOM) and a few other strip codepoints are only matched
    // against canonical forms after NFKC normalization.
    const bom = String.fromCharCode(0xFEFF);
    expect(stripBidiAndControls(`x${bom}y`)).toBe('xy');
  });

  it('returns empty for null/undefined input (no Someone fallback)', () => {
    expect(stripBidiAndControls(null)).toBe('');
    expect(stripBidiAndControls(undefined)).toBe('');
  });

  it('has no length cap (unlike stripControlAndBidi)', () => {
    // sanitizeMessage owns its own 500-char cap downstream; this
    // helper must not pre-truncate.
    const big = 'a'.repeat(2000);
    expect(stripBidiAndControls(big)).toBe(big);
  });
});

describe('sanitizeDisplayNamePlain — idempotence (load-bearing for rerenderConfirmCard cache-miss path)', () => {
  // The flow_state payload persists `recipientAliases` produced by
  // resolveRecipientAlias at pick time. On rerenderConfirmCard's
  // cache-miss path, the persisted alias is re-passed through
  // resolveRecipientAlias → sanitizeDisplayNamePlain. The chain is
  // idempotent today (NFKC + bidi/zero-width strip have fixed points
  // after one pass + the 64-codepoint cap leaves an already-capped
  // input unchanged). These tests pin that invariant: a future
  // sanitize-semantics change that breaks idempotence flips the
  // cache-miss render into a double-sanitize bug, and these red lights
  // catch it before that.

  it('idempotent on plain ASCII', () => {
    const once = sanitizeDisplayNamePlain('Alice');
    expect(sanitizeDisplayNamePlain(once)).toBe(once);
  });

  it('idempotent after RLO strip', () => {
    const rlo = String.fromCharCode(0x202E);
    const once = sanitizeDisplayNamePlain(`${rlo}Bob`);
    expect(sanitizeDisplayNamePlain(once)).toBe(once);
  });

  it('idempotent after NFKC normalization', () => {
    // U+FF21 (FULLWIDTH LATIN CAPITAL A) normalizes to 'A' under NFKC.
    const once = sanitizeDisplayNamePlain('Ａlice');
    expect(sanitizeDisplayNamePlain(once)).toBe(once);
  });

  it('idempotent on an already-capped 64-codepoint input', () => {
    const long = 'x'.repeat(64);
    const once = sanitizeDisplayNamePlain(long);
    expect(sanitizeDisplayNamePlain(once)).toBe(once);
    expect(once).toHaveLength(64);
  });

  it('idempotent on emoji (surrogate-pair safe)', () => {
    const once = sanitizeDisplayNamePlain('\u{1F600} Alice');
    expect(sanitizeDisplayNamePlain(once)).toBe(once);
  });
});
