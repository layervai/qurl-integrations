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

// Last-resort display-name fallback. Centralized so the
// resolveSenderAlias chain, sanitizeDisplayName, and the
// defensive default in revokeAllLinks all agree on one literal.
const DISPLAY_NAME_FALLBACK = 'Someone';

// `maxCodepoints` defaults to 64 (display-name budget). Larger
// surfaces (locationName, attachment.name → 256) pass their own
// cap; the strip + NFKC + codepoint-slice contract is identical.
// `fallback` is the string substituted when input is null/undefined
// AND when the strip + cap chain collapses to empty. Defaults to
// the display-name fallback for the historical caller; non-display
// surfaces (e.g. the per-DM guildName provenance row) pass `''`
// to opt out — surfacing "Someone" on an all-strip hostile guild
// name would degrade the trust signal the strip exists to defend.
function stripControlAndBidi(s, maxCodepoints = 64, fallback = DISPLAY_NAME_FALLBACK) {
  const cleaned = String(s ?? fallback).normalize('NFKC').replace(STRIP_RE, '');
  // Codepoint-aware slice. `String.prototype.slice` operates on UTF-16
  // code units, so a cap on a name like `'A'.repeat(63) + emoji`
  // would split a surrogate pair and Discord would render the lone high
  // surrogate as tofu. `Array.from(str)` iterates by codepoint.
  return Array.from(cleaned).slice(0, maxCodepoints).join('') || fallback;
}

/**
 * Sanitize a user-controlled label that lands inside Discord message
 * content (NOT a display-name slot): strip bidi / zero-width / control
 * chars (RLO spoofing defense), NFKC-normalize, codepoint-slice to
 * `maxCodepoints`, then escape markdown so a crafted label can't
 * inject `**bold**` / `[masked-link](https://evil)` / spoilers.
 *
 * Callers: /qurl map's `locationName`, /qurl file's
 * `attachment.name`-derived `resourceLabel`. Returns '' on empty /
 * all-strip-char input (NOT the 'Someone' display-name fallback) so
 * the caller can render its own empty-state.
 */
function sanitizeContentLabel(s, maxCodepoints = 256) {
  if (s == null || s === '') return '';
  const cleaned = String(s).normalize('NFKC').replace(STRIP_RE, '');
  if (!cleaned) return '';
  const sliced = Array.from(cleaned).slice(0, maxCodepoints).join('');
  return escapeDiscordMarkdown(sliced);
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
  return escapeDiscordMarkdown(stripControlAndBidi(s)) || DISPLAY_NAME_FALLBACK;
}

/**
 * Like sanitizeDisplayName but skips markdown escaping. Use for
 * plain-text contexts where backslash-escapes would render literally
 * (e.g. inside a `.txt` file attachment, or a Discord embed's
 * setAuthor `name` slot). NFKC + bidi/zero-width/control strip +
 * 64-codepoint cap still apply.
 *
 * `fallback` is what's substituted on null/undefined input or when
 * the strip + cap chain collapses to empty (all-strip hostile
 * input). Defaults to 'Someone' for the historical caller; surfaces
 * that would degrade if the fallback string leaked into the rendered
 * output (e.g. the per-DM author-row guildName, which would render
 * the nonsense `Vik · Someone` on all-strip hostile input) pass
 * `{ fallback: '' }` to opt out and handle the empty case themselves.
 */
function sanitizeDisplayNamePlain(s, { fallback = DISPLAY_NAME_FALLBACK } = {}) {
  return stripControlAndBidi(s, 64, fallback);
}

/**
 * Strip bidi / zero-width / control / line-separator codepoints from
 * a user-controlled message body without any length cap, NFKC pass,
 * or markdown escape. Used by sanitizeMessage to layer the same RLO
 * spoofing defense onto the personal-message surface that
 * sanitizeContentLabel applies to labels — without disrupting
 * sanitizeMessage's own slice / markdown-escape ordering.
 *
 * NFKC IS applied first because U+FEFF (BOM) and a few other strip
 * codepoints are only matched against canonical forms after NFKC.
 * No fallback string — empty input returns empty output (the
 * sanitizeMessage caller has its own empty-message handling).
 */
function stripBidiAndControls(s) {
  return String(s ?? '').normalize('NFKC').replace(STRIP_RE, '');
}

module.exports = {
  DISPLAY_NAME_FALLBACK,
  sanitizeFilename,
  escapeDiscordMarkdown,
  sanitizeDisplayName,
  sanitizeDisplayNamePlain,
  sanitizeContentLabel,
  stripBidiAndControls,
};
