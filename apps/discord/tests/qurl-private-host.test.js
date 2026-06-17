/**
 * Tests for the isPrivateHost logic inside createOneTimeLink — SSRF guard.
 * We can't import isPrivateHost directly (not exported), so drive it via
 * createOneTimeLink and assert the thrown error.
 */

jest.mock('../src/config', () => ({
  QURL_API_KEY: 'test',
  QURL_ENDPOINT: 'https://api.test.local',
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
  await expect(createOneTimeLink(url, '1h', 'test', 'key'))
    .rejects.toThrow(/private|not allowed/i);
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
