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
 *      backtick code, block-quote, masked-link `[text](url)`). Display
 *      names allow these chars too.
 * Returns 'Someone' if input is null/undefined/empty or becomes empty after
 * the strip (e.g. an alias composed entirely of zero-width chars).
 *
 * 64-char slice caps the post-strip INPUT length, not the rendered
 * length: `escapeDiscordMarkdown` runs after the slice and may double
 * the string (each escapable char becomes two) so the rendered alias
 * can be up to ~128 chars. That's still well under Discord's embed
 * field limits, so the in-input bound is what matters for the spoof
 * defense. Slice happens AFTER the strip and BEFORE the escape so a
 * trailing escape sequence (\\) cannot be truncated mid-pair into a
 * single backslash. Use this at every site that renders a Discord
 * username / display name / nickname inside markdown formatting (DM
 * embed, channel announcement, etc.) so the spoof defense does not
 * drift between sites.
 */
function sanitizeDisplayName(s) {
  // `?? 'Someone'` (not `||`) — matches the `??` in resolveSenderAlias's
  // fallback chain, so the two halves of the same flow read consistently.
  // Difference is academic for string inputs (empty string falls through
  // the post-escape `|| 'Someone'` anyway), but keeps the intent
  // unambiguous: only `null`/`undefined` should trip the fallback here.
  const cleaned = String(s ?? 'Someone').normalize('NFKC')
    // eslint-disable-next-line no-control-regex -- intentional: bidi/zero-width/control strip
    .replace(/[\u0000-\u001F\u007F\u00AD\u061C\u200B-\u200F\u2028\u2029\u202A-\u202E\u2066-\u2069\uFEFF]/g, '');
  // Codepoint-aware slice. `String.prototype.slice` operates on UTF-16
  // code units, so a 64-char cap on a name like `'A'.repeat(63) + '🎉'`
  // would split the emoji's surrogate pair and Discord would render the
  // lone high surrogate as tofu. `Array.from(str)` iterates by codepoint,
  // so an emoji at the boundary either fully survives (if it fits) or is
  // fully dropped — never half-included.
  const stripped = Array.from(cleaned).slice(0, 64).join('');
  return escapeDiscordMarkdown(stripped) || 'Someone';
}

module.exports = { sanitizeFilename, escapeDiscordMarkdown, sanitizeDisplayName };
