/**
 * Sanitize a filename to prevent path traversal and other filesystem issues.
 * Also strips leading dots (hidden-file names like .env) and returns a stable
 * fallback if the result is empty so callers never get an unusable filename.
 */
function sanitizeFilename(name) {
  const cleaned = String(name ?? '')
    .replace(/[/\\:*?"<>|]/g, '_')
    .replace(/\.\./g, '_')
    // eslint-disable-next-line no-control-regex -- intentional: strip control chars
    .replace(/[\x00-\x1f\x7f]/g, '')
    .replace(/^\.+/, '_')
    .substring(0, 200)
    .trim();
  return cleaned || 'unnamed_file';
}

/**
 * Escape the Discord markdown characters that render as rich text in message
 * content and embed fields. Use for any user-controlled string that lands in
 * an embed where we want it rendered literally (e.g. locationName from a
 * modal, which would otherwise let attackers inject `**PHISHING**` banners).
 */
function escapeDiscordMarkdown(s) {
  // Covers bold/italic/underline/code/strikethrough/block-quote/spoiler + masked-link
  // syntax [text](url). Without the bracket/paren escapes an attacker could inject
  // `[Free Prizes](https://phishing.com)` as a clickable link in any embed field.
  return String(s ?? '').replace(/[\\*_~`>|[\]()]/g, '\\$&');
}

// Bidi / zero-width / control / BOM / line-separator chars that Discord
// allows in display names but `escapeDiscordMarkdown` doesn't strip.
// Built via `new RegExp(...)` (instead of a literal /.../) so the
// \uXXXX escapes go through string parsing — keeps the source
// ASCII-only and avoids editor-/tool-chain confusion over raw control
// codepoints in a regex literal.
const STRIP_RE = new RegExp(
  // eslint-disable-next-line no-control-regex -- intentional: bidi/zero-width/control strip
  '[\\u0000-\\u001F\\u007F\\u00AD\\u061C\\u200B-\\u200F\\u2028\\u2029\\u202A-\\u202E\\u2066-\\u2069\\uFEFF]',
  'g',
);

function stripControlAndBidi(s) {
  const cleaned = String(s ?? 'Someone').normalize('NFKC').replace(STRIP_RE, '');
  // Codepoint-aware slice. `String.prototype.slice` operates on UTF-16
  // code units, so a 64-char cap on a name like `'A'.repeat(63) + emoji`
  // would split a surrogate pair and Discord would render the lone high
  // surrogate as tofu. `Array.from(str)` iterates by codepoint.
  return Array.from(cleaned).slice(0, 64).join('') || 'Someone';
}

/**
 * Sanitize a Discord display name (alias) for safe rendering inside `**...**`
 * markdown. Two layers:
 *   1. NFKC-normalize, then strip ASCII control / soft-hyphen / zero-width /
 *      bidi-control / line-paragraph-separator / BOM characters. Display
 *      names allow these and `escapeDiscordMarkdown` does not touch them.
 *      Without this strip, an attacker named with a leading U+202E (RLO)
 *      can flip text direction inside the rendered string and visually
 *      spoof a different sender identity. ZWSP-padded names mimicking
 *      another member are similarly defused.
 *   2. escapeDiscordMarkdown — handles markdown injection (bold, italics,
 *      backtick code, block-quote, masked-link `[text](url)`).
 * Returns 'Someone' if input is null/undefined/empty or becomes empty after
 * the strip (e.g. an alias composed entirely of zero-width chars).
 *
 * 64-char slice caps the post-strip INPUT length, not the rendered
 * length: `escapeDiscordMarkdown` runs after the slice and may double
 * the string (each escapable char becomes two). Slice happens AFTER
 * the strip and BEFORE the escape so a trailing escape sequence (\\)
 * cannot be truncated mid-pair into a single backslash.
 */
function sanitizeDisplayName(s) {
  return escapeDiscordMarkdown(stripControlAndBidi(s)) || 'Someone';
}

/**
 * Like sanitizeDisplayName but skips markdown escaping. Use for
 * plain-text contexts where backslash-escapes would render literally
 * (e.g. inside a `.txt` file attachment). NFKC + bidi/zero-width/
 * control strip + 64-codepoint cap still apply.
 */
function sanitizeDisplayNamePlain(s) {
  return stripControlAndBidi(s);
}

module.exports = { sanitizeFilename, escapeDiscordMarkdown, sanitizeDisplayName, sanitizeDisplayNamePlain };
