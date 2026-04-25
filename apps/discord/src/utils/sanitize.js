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
 * 64-char slice is a defensive upper bound; Discord caps display names at
 * 32 in API v10. Slice happens AFTER the strip and BEFORE the escape so a
 * trailing escape sequence (\\) cannot be truncated mid-pair into a single
 * backslash. Use this at every site that renders a Discord username /
 * display name / nickname inside markdown formatting (DM embeds, channel
 * announcements, etc.) so the spoof defense does not drift between sites.
 */
function sanitizeDisplayName(s) {
  // `?? 'Someone'` (not `||`) — matches the `??` in resolveSenderAlias's
  // fallback chain, so the two halves of the same flow read consistently.
  // Difference is academic for string inputs (empty string falls through
  // the post-escape `|| 'Someone'` anyway), but keeps the intent
  // unambiguous: only `null`/`undefined` should trip the fallback here.
  const stripped = String(s ?? 'Someone').normalize('NFKC')
    // eslint-disable-next-line no-control-regex -- intentional: bidi/zero-width/control strip
    .replace(/[\u0000-\u001F\u007F\u00AD\u200B-\u200F\u2028\u2029\u202A-\u202E\u2066-\u2069\uFEFF]/g, '')
    .slice(0, 64);
  return escapeDiscordMarkdown(stripped) || 'Someone';
}

module.exports = { sanitizeFilename, escapeDiscordMarkdown, sanitizeDisplayName };
