/**
 * Tests for src/utils/sanitize.js — filename sanitization and Discord
 * markdown escaping. The markdown escape is the primary defense against
 * masked-link phishing in embeds, so adversarial coverage matters.
 */

const { sanitizeFilename, escapeDiscordMarkdown } = require('../src/utils/sanitize');

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
