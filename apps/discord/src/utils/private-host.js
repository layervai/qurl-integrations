// Syntactic classification of private / loopback / link-local hosts.
//
// This module is the single source of truth for the IP range table that the
// bot screens against. It is deliberately DEPENDENCY-FREE (no requires, like
// its peers sanitize.js / cookies.js / query-params.js) so the boot path can
// consume it without dragging in the @layervai/qurl SDK, constants.js and
// `dns` that qurl.js pulls in.
//
// Two callers with two different QUESTIONS share the table:
//
//   - qurl.js `isPrivateHost` asks "is this private?" for SSRF defense. It is
//     fail-CLOSED: an empty, unparseable, or merely suspicious host is
//     rejected, and it screens every non-public range including CGNAT and
//     multicast.
//   - a boot-time origin check asks "is this a usable PUBLIC origin?" It is
//     fail-OPEN on an absent value (a missing setting is a different error,
//     reported elsewhere) and deliberately does NOT screen CGNAT — a CGNAT
//     address can front a legitimately reachable origin, so rejecting it
//     would fail a valid deploy.
//
// Those postures legitimately differ, so this module exposes the
// classification and lets each caller compose its own question at the call
// site. Do not collapse the two postures into one predicate.
//
// SECURITY: every function here is SYNTACTIC only. It cannot see where a DNS
// name actually resolves. The SSRF path pairs it with a resolve-then-check
// step (qurl.js `assertNotPrivateAfterResolve`); do not treat a `false` here
// as proof a host is safe to fetch.

/**
 * Strict dotted-quad shape: each octet is `0`, `1-9`, or `10`-`255` with no
 * leading zero. Shared with the hot-standby INSTANCE_IP validation, which
 * screens the same operator-typo class.
 */
const IPV4_LITERAL_RE = /^(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]\d|\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]\d|\d)$/;

// Lenient shape, for the SSRF posture only — see parseIPv4Octets.
const IPV4_LENIENT_RE = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/;

/**
 * Parse a dotted-quad IPv4 literal into octets, or null when `host` is not
 * one.
 *
 * `allowLeadingZeros` selects the posture:
 *
 *   - false (default) — the WHATWG-shaped strict parse. Use when the input
 *     already came through `new URL().hostname`, which canonicalizes
 *     alternate literal forms (`010.0.0.1` arrives as `8.0.0.1`), so a
 *     leading zero at this point is an operator typo rather than an octal
 *     literal.
 *   - true — accepts leading zeros. The SSRF path needs this because it
 *     screens raw, un-canonicalized user input, where `0.0.0.01` must still
 *     read as 0.0.0.1 rather than falling through as a domain name.
 *
 * Both modes reject labels that `Number()` would accept but the URL spec's
 * IPv4 parser does not, and which therefore arrive as ordinary DOMAIN
 * hostnames. This is load-bearing, NOT an octal defense: `Number('1e2')` is
 * 100, so without the digits-only shape a public host like `10.2.3.1e2` or
 * `192.168.0.1e1` would read as a private literal — blocking a legitimate
 * target on the SSRF path and crash-looping a valid deploy on the boot path.
 */
function parseIPv4Octets(host, { allowLeadingZeros = false } = {}) {
  if (typeof host !== 'string') return null;
  if (allowLeadingZeros) {
    const m = IPV4_LENIENT_RE.exec(host);
    return m ? m.slice(1).map(Number) : null;
  }
  if (!IPV4_LITERAL_RE.test(host)) return null;
  return host.split('.').map(Number);
}

/**
 * The IPv4 range table. `octets` comes from parseIPv4Octets.
 *
 * The always-on ranges are the ones that cannot serve a public origin and
 * cannot be a legitimate fetch target. CGNAT and multicast/reserved are
 * opt-in because only the SSRF posture screens them — see the module header.
 *
 * Returns the range name (useful for operator-facing messages) or null when
 * the address is public as far as this table is concerned.
 */
function ipv4LocalScope(octets, { includeCgnat = false, includeMulticast = false } = {}) {
  if (!octets) return null;
  const [a, b] = octets;
  if (a === 0) return 'this-network';                          // 0.0.0.0/8
  if (a === 10) return 'private';                              // 10.0.0.0/8
  if (a === 127) return 'loopback';                            // 127.0.0.0/8
  if (a === 169 && b === 254) return 'link-local';             // 169.254.0.0/16 (IMDS)
  if (a === 172 && b >= 16 && b <= 31) return 'private';       // 172.16.0.0/12
  if (a === 192 && b === 168) return 'private';                // 192.168.0.0/16
  if (includeCgnat && a === 100 && b >= 64 && b <= 127) return 'cgnat';  // 100.64.0.0/10
  if (includeMulticast && a >= 224) return 'multicast';        // 224.0.0.0/4 + reserved
  return null;
}

/**
 * Unwrap an IPv4-mapped IPv6 literal to its dotted-quad form, or null.
 *
 * `host` must be bracket-stripped. Case is normalized defensively: the hex
 * tail is matched with [0-9a-f], and a composer who forgot to lowercase would
 * get a wrong `null` here — which is the fail-OPEN direction for the SSRF
 * caller, so this module does not rely on the contract being honoured.
 *
 * BOTH tail forms matter, and missing the hex one is a real SSRF bypass
 * (issue #1035): `new URL()` re-serializes `::ffff:127.0.0.1` to the hex form
 * `::ffff:7f00:1`, so the dotted tail is the form a human types and the hex
 * tail is the form callers actually pass. Screening only the dotted tail lets
 * `::ffff:a9fe:a9fe` (169.254.169.254, the IMDS address) read as public.
 *
 * Deliberately common-forms-only: the deprecated IPv4-COMPATIBLE form
 * (`::127.0.0.1`, which serializes to `::7f00:1`) falls through, as it is
 * dead in practice and `::1` covers realistic loopback.
 */
function unwrapIPv4Mapped(rawHost) {
  const host = String(rawHost).toLowerCase();
  const dotted = /^::ffff:([0-9.]+)$/.exec(host);
  if (dotted) return dotted[1];
  const hex = /^::ffff:([0-9a-f]{1,4}):([0-9a-f]{1,4})$/.exec(host);
  if (!hex) return null;
  const [hi, lo] = hex.slice(1).map(group => parseInt(group, 16));
  return [hi >> 8, hi & 0xff, lo >> 8, lo & 0xff].join('.');
}

/**
 * Classify an IPv6 literal's local scope by its first hextet, or null when it
 * is not local. `host` must be bracket-stripped; case is normalized
 * defensively, for the same fail-open reason as unwrapIPv4Mapped.
 *
 * Callers filter on the returned name: site-local is deprecated and only the
 * fail-closed SSRF posture bothers to screen it.
 *
 * The caller MUST gate this on the host containing a ':'. parseInt is a
 * lenient prefix parse, so parseInt('fc00.example.com', 16) is 0xfc00 and the
 * unique-local mask would misread real public DNS names as link-local.
 */
function ipv6LocalScope(rawHost) {
  const host = String(rawHost).toLowerCase();
  if (host === '::') return 'unspecified';
  if (host === '::1') return 'loopback';
  const firstGroup = parseInt(host.split(':')[0], 16);
  if (!Number.isInteger(firstGroup)) return null;
  if ((firstGroup & 0xfe00) === 0xfc00) return 'unique-local';  // fc00::/7
  if ((firstGroup & 0xffc0) === 0xfe80) return 'link-local';    // fe80::/10
  if ((firstGroup & 0xffc0) === 0xfec0) return 'site-local';    // fec0::/10, deprecated
  return null;
}

/**
 * Reject hostnames that resolve (by syntax) to loopback, link-local, or
 * RFC1918 private ranges. Defense-in-depth against a caller passing
 * `http://169.254.169.254/latest/meta-data/...` or similar; even if the
 * downstream qURL API is the one that actually fetches, we block at our own
 * input validation layer.
 *
 * This is the fail-CLOSED SSRF posture: an empty host, an out-of-range
 * numeric host, and an octal-looking literal are all rejected rather than
 * passed through, and every non-public range is screened.
 */
function isPrivateHost(host) {
  if (!host) return true;
  const h = String(host).toLowerCase();
  if (h === 'localhost' || h === '0.0.0.0' || h === '::' || h === '::1') return true;
  if (h.startsWith('[') && h.endsWith(']')) {
    // Bracketed IPv6 literal — strip and re-check.
    return isPrivateHost(h.slice(1, -1));
  }
  // IPv6 locals reach here bracket-stripped, so they always contain a ':'.
  // Gate on the ':' so a PUBLIC DNS name that merely starts with these letters
  // (e.g. `fd-cdn.example.com`) is NOT misclassified — DNS names never
  // contain a colon.
  if (h.includes(':')) {
    if (ipv6LocalScope(h)) return true;
    // Conservative widening, SSRF posture only: a short first hextet such as
    // `fc::1` or `fdd::1` is NOT inside fc00::/7, so the mask above returns
    // null. Reject it anyway — over-rejecting a malformed literal is the safe
    // direction here, and this preserves the long-standing behavior of this
    // guard. A public-origin check must NOT copy this widening.
    if (h.startsWith('fc') || h.startsWith('fd')) return true;
  }
  // IPv4-mapped IPv6 literal, dotted or hex tail (see unwrapIPv4Mapped).
  const mapped = unwrapIPv4Mapped(h);
  if (mapped) return isPrivateHost(mapped);
  // Decimal IPv4 literal (e.g. `2130706433` = 127.0.0.1) — browsers accept it
  // and Node's URL does too. Convert to dotted-quad.
  if (/^\d+$/.test(h)) {
    const n = Number(h);
    if (n >= 0 && n <= 0xFFFFFFFF) return isPrivateHost(numberToDotted(n));
    return true; // out-of-range numeric host: reject outright
  }
  // Hex IPv4 literal (e.g. `0x7f000001` = 127.0.0.1)
  if (/^0x[0-9a-f]+$/.test(h)) {
    const n = Number(h);
    if (Number.isFinite(n) && n >= 0 && n <= 0xFFFFFFFF) return isPrivateHost(numberToDotted(n));
    return true;
  }
  // Octal-prefixed IPv4 (e.g. `0177.0.0.1`) — treat any leading-zero
  // component as suspicious and reject conservatively.
  if (/^0\d/.test(h) && /^[0-9.]+$/.test(h)) return true;
  // Standard IPv4 dotted-quad. Leading zeros are allowed through the parse
  // because this path screens raw user input, not URL-canonicalized input.
  return Boolean(ipv4LocalScope(
    parseIPv4Octets(h, { allowLeadingZeros: true }),
    { includeCgnat: true, includeMulticast: true }
  ));
}

function numberToDotted(n) {
  return [(n >>> 24) & 0xFF, (n >>> 16) & 0xFF, (n >>> 8) & 0xFF, n & 0xFF].join('.');
}

module.exports = {
  IPV4_LITERAL_RE,
  parseIPv4Octets,
  ipv4LocalScope,
  ipv6LocalScope,
  unwrapIPv4Mapped,
  isPrivateHost,
};
