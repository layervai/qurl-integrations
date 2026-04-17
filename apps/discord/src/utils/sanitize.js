/**
 * Sanitize a filename to prevent path traversal and other filesystem issues.
 * Also strips leading dots (hidden-file names like .env) and returns a stable
 * fallback if the result is empty so callers never get an unusable filename.
 */
function sanitizeFilename(name) {
  const cleaned = String(name ?? '')
    .replace(/[/\\:*?"<>|]/g, '_')
    .replace(/\.\./g, '_')
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
  return String(s ?? '').replace(/[\\*_~`>|]/g, '\\$&');
}

module.exports = { sanitizeFilename, escapeDiscordMarkdown };
