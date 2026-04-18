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
}));

const { createOneTimeLink } = require('../src/qurl');

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
