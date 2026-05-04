// Shared cookie parser — extracted because both `routes/oauth.js`
// (GitHub OAuth) and `routes/qurl-oauth.js` (qURL OAuth) had the same
// strict-uniqueness, malformed-pct-tolerant implementation. Single
// source of truth keeps the two CSRF flows from drifting on what
// counts as "no cookie."
//
// Format is `name=value; name=value; ...`; we only need one named
// cookie, but a browser extension or a sibling-subdomain can produce
// duplicates with the same name — any ambiguity = no binding,
// treat as "no cookie."
function readCookie(req, name) {
  const header = req.headers.cookie;
  if (!header) return null;
  const matches = [];
  for (const part of header.split(';')) {
    const eq = part.indexOf('=');
    if (eq === -1) continue;
    const k = part.slice(0, eq).trim();
    if (k !== name) continue;
    // Attacker-controlled cookies can contain malformed %-encoding
    // (e.g. `%ZZ`); decodeURIComponent would throw URIError. Treat a
    // malformed cookie as "no cookie".
    try {
      matches.push(decodeURIComponent(part.slice(eq + 1).trim()));
    } catch {
      return null;
    }
    if (matches.length > 1) return null;
  }
  return matches.length === 1 ? matches[0] : null;
}

module.exports = { readCookie };
