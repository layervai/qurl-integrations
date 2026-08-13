/**
 * Tests for the isPrivateHost SSRF guard. isPrivateHost is exported (also
 * consumed by connector.js's detect-tunnel check), so the prefix/literal logic
 * is pinned directly; the createOneTimeLink cases below additionally cover the
 * end-to-end path — guard verdict, thrown error, and no outbound request.
 */

jest.mock('../src/config', () => ({
  QURL_API_KEY: 'test',
  QURL_ENDPOINT: 'https://api.test.local',
}));

// Mock the SDK so a REGRESSION fails as a clean assertion rather than a ~17s
// real-network timeout (and real CI egress): every URL in this file must be
// rejected BEFORE a client is ever built, so `create` must never be called.
const mockClient = { create: jest.fn() };
jest.mock('@layervai/qurl', () => ({
  ...jest.requireActual('@layervai/qurl'),
  QURLClient: jest.fn().mockImplementation(() => mockClient),
}));

jest.mock('../src/logger', () => ({
  info: jest.fn(),
  warn: jest.fn(),
  error: jest.fn(),
  debug: jest.fn(),
  audit: jest.fn(),
}));

const { createOneTimeLink, isPrivateHost } = require('../src/qurl');

async function expectBlocked(url) {
  mockClient.create.mockClear();
  await expect(createOneTimeLink(url, '1h', 'test', 'key'))
    .rejects.toThrow(/private|not allowed/i);
  // The security property itself, not just the message: nothing was sent.
  expect(mockClient.create).not.toHaveBeenCalled();
}

describe('createOneTimeLink SSRF / private-host blocklist', () => {
  it('rejects non-http(s) schemes', async () => {
    await expect(createOneTimeLink('javascript:alert(1)', '1h', 't', 'k'))
      .rejects.toThrow(/http\/https/);
    await expect(createOneTimeLink('file:///etc/passwd', '1h', 't', 'k'))
      .rejects.toThrow(/http\/https/);
  });

  it('rejects loopback + localhost + wildcard', async () => {
    await expectBlocked('http://localhost/x');
    await expectBlocked('http://127.0.0.1/x');
    await expectBlocked('http://0.0.0.0/x');
  });

  it('rejects AWS IMDS', async () => {
    await expectBlocked('http://169.254.169.254/latest/meta-data/');
  });

  it('rejects RFC1918 + CGNAT + multicast', async () => {
    await expectBlocked('http://10.0.0.5/x');
    await expectBlocked('http://172.16.0.1/x');
    await expectBlocked('http://192.168.1.1/x');
    await expectBlocked('http://100.64.0.1/x'); // CGNAT
    await expectBlocked('http://224.0.0.1/x');  // multicast
  });

  it('rejects decimal IP literal (2130706433 = 127.0.0.1)', async () => {
    await expectBlocked('http://2130706433/x');
  });

  it('rejects hex IP literal (0x7f000001 = 127.0.0.1)', async () => {
    await expectBlocked('http://0x7f000001/x');
  });

  it('rejects octal-prefixed IP literal', async () => {
    await expectBlocked('http://0177.0.0.1/x');
  });

  it('rejects IPv6 loopback + link-local + unique-local', async () => {
    await expectBlocked('http://[::1]/x');
    await expectBlocked('http://[fe80::1]/x');
    await expectBlocked('http://[fd00::1]/x');
  });

  // End-to-end proof of the #1035 bypass: a bracketed IPv4-mapped literal used
  // to clear BOTH guards — isPrivateHost saw the re-serialized hex form its
  // regex didn't match, and assertNotPrivateAfterResolve early-returns on any
  // '['-prefixed host — so the target reached the qURL API.
  it('rejects IPv4-mapped IPv6 literals (bracketed, as callers pass them)', async () => {
    await expectBlocked('http://[::ffff:127.0.0.1]:8080/x');
    await expectBlocked('http://[::ffff:10.0.0.1]/x');
    await expectBlocked('http://[::ffff:169.254.169.254]/latest/meta-data/');
  });

});

// isPrivateHost is now exported (consumed by connector.js's detect-tunnel SSRF
// guard as well as createOneTimeLink), so pin its prefix logic directly. The
// fc/fd ULA check must NOT misclassify a public DNS name that merely starts
// with those letters — a false positive there would silently break /qurl detect
// (the tunnel target comes from qURL infra, not user input).
describe('isPrivateHost — IPv6 ULA prefix vs. public DNS', () => {
  it('classifies real IPv6 ULA / link-local / site-local literals as private', () => {
    expect(isPrivateHost('fd00::1')).toBe(true);   // unique-local fc00::/7
    expect(isPrivateHost('fc00::1')).toBe(true);   // unique-local fc00::/7
    expect(isPrivateHost('fe80::1')).toBe(true);   // link-local, bottom of fe80::/10
    expect(isPrivateHost('febf::1')).toBe(true);   // link-local, top of fe80::/10
    expect(isPrivateHost('fec0::1')).toBe(true);   // deprecated site-local fec0::/10
    expect(isPrivateHost('feff::1')).toBe(true);   // top of fec0::/10
  });

  it('does NOT misclassify public DNS names starting with fc/fd/fe as private', () => {
    // No colon ⇒ a hostname, not an IPv6 local literal. Pre-fix the bare
    // `startsWith('fc'|'fd')` (and the narrow `fe80:`) would have mishandled
    // these — the colon gate is what keeps a public DNS name out.
    expect(isPrivateHost('fd-detect.qurl.link')).toBe(false);
    expect(isPrivateHost('fcdn.example.com')).toBe(false);
    expect(isPrivateHost('feb-cdn.example.com')).toBe(false);  // 'feb' prefix, but no colon
    expect(isPrivateHost('detect-tunnel.qurl.link')).toBe(false);
  });
});

// IPv4-in-IPv6 embeddings — regression cover for the #1035 SSRF bypass. WHATWG
// URL parsing re-serializes a dotted IPv4-mapped literal to COMPRESSED HEX, so
// the hex spelling is what the URL-target callers receive (they pass
// `new URL(...).hostname`), while the resolve leg receives the dotted form from
// dns.lookup. The original guard matched `::ffff:[0-9.]+` only — a form that
// never arrives on the URL leg — so every private IPv4 smuggled through a check
// whose entire purpose was rejecting them.
describe('isPrivateHost — IPv4-in-IPv6 embeddings', () => {
  // This is the bug in one assertion, and the reason the hex branch must exist.
  it('documents that the parser rewrites the dotted literal to hex', () => {
    expect(new URL('https://[::ffff:127.0.0.1]').hostname).toBe('[::ffff:7f00:1]');
    expect(new URL('https://[::ffff:10.0.0.1]').hostname).toBe('[::ffff:a00:1]');
    expect(new URL('https://[::ffff:169.254.169.254]').hostname).toBe('[::ffff:a9fe:a9fe]');
  });

  it('classifies the compressed-hex form callers actually receive as private', () => {
    expect(isPrivateHost('::ffff:7f00:1')).toBe(true);     // 127.0.0.1 loopback
    expect(isPrivateHost('::ffff:a00:1')).toBe(true);      // 10.0.0.1 RFC1918
    expect(isPrivateHost('::ffff:a9fe:a9fe')).toBe(true);  // 169.254.169.254 IMDS
    expect(isPrivateHost('::ffff:c0a8:101')).toBe(true);   // 192.168.1.1 RFC1918
    expect(isPrivateHost('::ffff:ac10:1')).toBe(true);     // 172.16.0.1 RFC1918
    expect(isPrivateHost('::ffff:6440:1')).toBe(true);     // 100.64.0.1 CGNAT
  });

  // IPv6 serialization suppresses each hextet's LEADING ZEROS (RFC 5952 4.1),
  // so the hex branch must accept 1-4 digits per group rather than a fixed 4.
  it('handles hextets with suppressed leading zeros', () => {
    expect(new URL('https://[::ffff:0.0.0.1]').hostname).toBe('[::ffff:0:1]');
    expect(new URL('https://[::ffff:0.0.0.0]').hostname).toBe('[::ffff:0:0]');
    expect(isPrivateHost('::ffff:0:1')).toBe(true);        // 0.0.0.1, in 0.0.0.0/8
    expect(isPrivateHost('::ffff:0:0')).toBe(true);        // 0.0.0.0
  });

  // NOT vestigial, and NOT merely "the form a human types": inet_ntop — hence
  // dns.lookup — renders a mapped address dotted, so this is the spelling
  // assertNotPrivateAfterResolve feeds back into isPrivateHost. It is reachable
  // from attacker-controlled DNS (an AAAA of ::ffff:169.254.169.254), so the
  // dotted branch is load-bearing on the resolve leg exactly as the hex branch
  // is on the URL leg. This test exists to stop it being deleted as dead code.
  it('classifies the dotted spelling (the dns.lookup form) as private', () => {
    expect(isPrivateHost('::ffff:127.0.0.1')).toBe(true);
    expect(isPrivateHost('::ffff:169.254.169.254')).toBe(true);
  });

  // Pins the toLowerCase() at the top of isPrivateHost, which the [0-9a-f]
  // class depends on. Delete that call and every OTHER case in this describe
  // still passes — only this one catches it.
  it('is case-insensitive on the hex groups', () => {
    expect(isPrivateHost('::FFFF:7F00:1')).toBe(true);      // 127.0.0.1
    expect(isPrivateHost('::FFFF:A9FE:A9FE')).toBe(true);   // 169.254.169.254
  });

  // The ranges are decided by the reconstructed octets, so a mis-masked byte in
  // a future rewrite would show up here first. Only ac10/6440 are exercised
  // above, and neither pins an edge.
  it('respects the RFC1918 / CGNAT boundaries and full-width groups', () => {
    expect(isPrivateHost('::ffff:ac0f:ffff')).toBe(false);  // 172.15.255.255, below /12
    expect(isPrivateHost('::ffff:ac1f:ffff')).toBe(true);   // 172.31.255.255, top of /12
    expect(isPrivateHost('::ffff:ac20:1')).toBe(false);     // 172.32.0.1, above /12
    expect(isPrivateHost('::ffff:643f:ffff')).toBe(false);  // 100.63.255.255, below /10
    expect(isPrivateHost('::ffff:647f:ffff')).toBe(true);   // 100.127.255.255, top of /10
    expect(isPrivateHost('::ffff:6480:1')).toBe(false);     // 100.128.0.1, above /10
    expect(isPrivateHost('::ffff:ffff:ffff')).toBe(true);   // 255.255.255.255, 4-digit groups
  });

  // The mirror-image failure: over-blocking would break legitimate targets.
  it('does NOT over-block a mapped PUBLIC IPv4', () => {
    expect(new URL('https://[::ffff:8.8.8.8]').hostname).toBe('[::ffff:808:808]');
    expect(isPrivateHost('::ffff:808:808')).toBe(false);   // 8.8.8.8
    expect(isPrivateHost('::ffff:101:101')).toBe(false);   // 1.1.1.1
    expect(isPrivateHost('::ffff:1.1.1.1')).toBe(false);   // dotted spelling
  });

  // The sibling embeddings. These do not route to the embedded IPv4 on a stock
  // host, but that is a property of the RUNTIME, not of the address: an
  // IPv6-only subnet with DNS64/NAT64 (a supported AWS VPC config) makes
  // 64:ff9b:: route for real. Decode them rather than depend on host routing.
  it('decodes the sibling IPv4-in-IPv6 embeddings', () => {
    expect(isPrivateHost('::7f00:1')).toBe(true);            // ::127.0.0.1, v4-compatible
    expect(isPrivateHost('::a9fe:a9fe')).toBe(true);         // v4-compatible IMDS
    expect(isPrivateHost('::127.0.0.1')).toBe(true);         // v4-compatible, dotted
    expect(isPrivateHost('::ffff:0:7f00:1')).toBe(true);     // v4-translated (SIIT)
    expect(isPrivateHost('64:ff9b::7f00:1')).toBe(true);     // NAT64 loopback
    expect(isPrivateHost('64:ff9b::a9fe:a9fe')).toBe(true);  // NAT64 IMDS
  });

  // Decoding (not blanket prefix-rejection) is what keeps these allowed — a
  // NAT64 host reaching a PUBLIC IPv4 is legitimate traffic.
  it('does NOT over-block a PUBLIC IPv4 in a sibling embedding', () => {
    expect(isPrivateHost('64:ff9b::808:808')).toBe(false);   // NAT64 -> 8.8.8.8
    expect(isPrivateHost('::808:808')).toBe(false);          // v4-compatible -> 8.8.8.8
  });

  // The generalized prefix alternation must not swallow ordinary IPv6.
  it('leaves ordinary IPv6 alone — public stays public, ULA/link-local stays private', () => {
    expect(isPrivateHost('2001:db8::1')).toBe(false);
    expect(isPrivateHost('2606:4700:4700::1111')).toBe(false);  // public resolver
    expect(isPrivateHost('fd00::1')).toBe(true);                // still ULA
    expect(isPrivateHost('fe80::1')).toBe(true);                // still link-local
  });
});
