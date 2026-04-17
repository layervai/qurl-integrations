/**
 * Sanitize a filename to prevent path traversal and other filesystem issues.
 */
function sanitizeFilename(name) {
  return name
    .replace(/[/\\:*?"<>|]/g, '_')
    .replace(/\.\./g, '_')
    .replace(/[\x00-\x1f\x7f]/g, '')
    .substring(0, 200);
}

module.exports = { sanitizeFilename };
