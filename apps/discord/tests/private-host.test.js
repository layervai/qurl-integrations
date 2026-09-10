
const {
  IPV4_LITERAL_RE,
  parseIPv4Octets,
  ipv4LocalScope,
  ipv6LocalScope,
  unwrapIPv4Mapped,
  isPrivateHost,
} = require('../src/utils/private-host');

describe('private-host module is dependency-free', () => {
  it('has no require() calls — the boot path must not pull in the SDK', () => {
    const fs = require('fs');
    const path = require('path');
    const src = fs.readFileSync(
      path.join(__dirname, '../src/utils/private-host.js'), 'utf8'
    );
    expect(src).not.toMatch(/\brequire\s*\(/);
  });
});

describe('unwrapIPv4Mapped — every IPv4-in-IPv6 embedding (#1043)', () => {
  it.each([
    ['::ffff:7f00:1', '127.0.0.1', 'IPv4-mapped, hex'],
    ['::ffff:127.0.0.1', '127.0.0.1', 'IPv4-mapped, dotted'],
    ['::7f00:1', '127.0.0.1', 'IPv4-compatible, hex'],
    ['::127.0.0.1', '127.0.0.1', 'IPv4-compatible, dotted'],
    ['::ffff:0:7f00:1', '127.0.0.1', 'IPv4-translated (SIIT)'],
    ['64:ff9b::7f00:1', '127.0.0.1', 'NAT64 well-known prefix'],
    ['64:ff9b::a9fe:a9fe', '169.254.169.254', 'NAT64 to IMDS'],
  ])('decodes %s -> %s (%s)', (host, expected) => {
    expect(unwrapIPv4Mapped(host)).toBe(expected);
  });

  it('decodes rather than blanket-rejecting, so NAT64 to a public IP survives', () => {
    expect(isPrivateHost('64:ff9b::808:808')).toBe(false);
    expect(isPrivateHost('64:ff9b::cb00:710a')).toBe(false);  // 203.0.113.10
    expect(isPrivateHost('::808:808')).toBe(false);
    expect(isPrivateHost('::ffff:0:808:808')).toBe(false);
  });

  it('leaves a lone hextet unmatched — it can only mean 0/8 or 255.255/16', () => {
    expect(unwrapIPv4Mapped('::a')).toBeNull();
  });
});

describe('unwrapIPv4Mapped — issue #1035 (SSRF bypass)', () => {
  it('unwraps the hex tail that callers actually pass', () => {
    expect(unwrapIPv4Mapped('::ffff:7f00:1')).toBe('127.0.0.1');
    expect(unwrapIPv4Mapped('::ffff:a00:1')).toBe('10.0.0.1');
    expect(unwrapIPv4Mapped('::ffff:c0a8:1')).toBe('192.168.0.1');
    expect(unwrapIPv4Mapped('::ffff:a9fe:a9fe')).toBe('169.254.169.254');
  });

  it('unwraps the dotted tail a human types', () => {
    expect(unwrapIPv4Mapped('::ffff:127.0.0.1')).toBe('127.0.0.1');
    expect(unwrapIPv4Mapped('::ffff:8.8.8.8')).toBe('8.8.8.8');
  });

  it('normalizes case itself rather than trusting the caller', () => {
    expect(unwrapIPv4Mapped('::FFFF:7F00:1')).toBe('127.0.0.1');
    expect(unwrapIPv4Mapped('::ffff:7f00:1')).toBe('127.0.0.1');
    expect(ipv6LocalScope('FE80::1')).toBe('link-local');
    expect(ipv6LocalScope('FC00::1')).toBe('unique-local');
    expect(isPrivateHost('::FFFF:7F00:1')).toBe(true);
  });

  it('returns null when there is no embedded IPv4 to decode', () => {
    expect(unwrapIPv4Mapped('::')).toBeNull();
    expect(unwrapIPv4Mapped('fe80::1')).toBeNull();
    expect(unwrapIPv4Mapped('::ffff:zz:1')).toBeNull();   // not hex
    expect(unwrapIPv4Mapped('::ffff:1:2:3')).toBeNull();  // not exactly two groups
  });

  it('reads an all-digit lone hextet as decimal, which is harmless', () => {
    expect(unwrapIPv4Mapped('::1')).toBe('1');
    expect(isPrivateHost('::1')).toBe(true);
    expect(isPrivateHost('1')).toBe(true); //  decimal 1 -> 0.0.0.1, in 0/8
  });
});

describe('isPrivateHost — issue #1035 regression pins', () => {
  it.each([
    ['::ffff:7f00:1', '127.0.0.1 loopback'],
    ['::FFFF:7F00:1', 'uppercase loopback'],
    ['::ffff:a00:1', '10.0.0.1'],
    ['::ffff:ac10:1', '172.16.0.1'],
    ['::ffff:c0a8:1', '192.168.0.1'],
    ['::ffff:a9fe:a9fe', '169.254.169.254 IMDS'],
    ['::ffff:6440:1', '100.64.0.1 CGNAT'],
    ['::ffff:0:0', '0.0.0.0'],
    ['::ffff:ffff:ffff', '255.255.255.255 broadcast'],
  ])('blocks %s (%s)', host => {
    expect(isPrivateHost(host)).toBe(true);
  });

  it('does not over-block PUBLIC addresses in the same hex-mapped form', () => {
    expect(isPrivateHost('::ffff:cb00:710a')).toBe(false);  // 203.0.113.10
    expect(isPrivateHost('::ffff:808:808')).toBe(false);    // 8.8.8.8
    expect(isPrivateHost('::ffff:8.8.8.8')).toBe(false);
  });
});

describe('parseIPv4Octets', () => {
  it('rejects labels Number() accepts but the URL spec does not', () => {
    for (const mode of [{ allowLeadingZeros: false }, { allowLeadingZeros: true }]) {
      expect(parseIPv4Octets('10.2.3.1e2', mode)).toBeNull();
      expect(parseIPv4Octets('192.168.0.1e1', mode)).toBeNull();
      expect(parseIPv4Octets('10.2.3.+1', mode)).toBeNull();
      expect(parseIPv4Octets('10.2.3.0x1', mode)).toBeNull();
    }
  });

  it('parses canonical dotted quads in both modes', () => {
    expect(parseIPv4Octets('10.0.0.1')).toEqual([10, 0, 0, 1]);
    expect(parseIPv4Octets('10.0.0.1', { allowLeadingZeros: true })).toEqual([10, 0, 0, 1]);
  });

  it('differs on leading zeros — strict is URL-canonicalized input only', () => {
    expect(parseIPv4Octets('0.0.0.01')).toBeNull();
    expect(parseIPv4Octets('0.0.0.01', { allowLeadingZeros: true })).toEqual([0, 0, 0, 1]);
  });

  it('rejects non-strings and wrong shapes', () => {
    expect(parseIPv4Octets(undefined)).toBeNull();
    expect(parseIPv4Octets('10.0.0')).toBeNull();
    expect(parseIPv4Octets('10.0.0.0.0')).toBeNull();
    expect(parseIPv4Octets('256.0.0.1')).toBeNull();
  });

  it('IPV4_LITERAL_RE agrees with the strict parse', () => {
    for (const h of ['10.0.0.1', '0.0.0.0', '255.255.255.255']) {
      expect(IPV4_LITERAL_RE.test(h)).toBe(true);
    }
    for (const h of ['010.0.0.1', '256.0.0.1', '10.2.3.1e2']) {
      expect(IPV4_LITERAL_RE.test(h)).toBe(false);
    }
  });
});

describe('ipv4LocalScope — opt-in ranges keep the two postures apart', () => {
  const boot = { includeCgnat: false, includeMulticast: false };
  const ssrf = { includeCgnat: true, includeMulticast: true };

  it('screens the always-on ranges under both postures', () => {
    for (const opts of [boot, ssrf]) {
      expect(ipv4LocalScope([0, 0, 0, 0], opts)).toBe('this-network');
      expect(ipv4LocalScope([10, 0, 0, 1], opts)).toBe('private');
      expect(ipv4LocalScope([127, 0, 0, 1], opts)).toBe('loopback');
      expect(ipv4LocalScope([169, 254, 169, 254], opts)).toBe('link-local');
      expect(ipv4LocalScope([172, 16, 0, 1], opts)).toBe('private');
      expect(ipv4LocalScope([192, 168, 0, 1], opts)).toBe('private');
    }
  });

  it('holds the range boundaries', () => {
    expect(ipv4LocalScope([172, 15, 255, 255], ssrf)).toBeNull();
    expect(ipv4LocalScope([172, 31, 255, 255], ssrf)).toBe('private');
    expect(ipv4LocalScope([172, 32, 0, 1], ssrf)).toBeNull();
    expect(ipv4LocalScope([169, 253, 255, 255], ssrf)).toBeNull();
    expect(ipv4LocalScope([9, 255, 255, 255], ssrf)).toBeNull();
    expect(ipv4LocalScope([11, 0, 0, 1], ssrf)).toBeNull();
  });

  it('screens CGNAT only under the SSRF posture', () => {
    expect(ipv4LocalScope([100, 64, 0, 1], ssrf)).toBe('cgnat');
    expect(ipv4LocalScope([100, 64, 0, 1], boot)).toBeNull();
    expect(ipv4LocalScope([100, 63, 255, 255], ssrf)).toBeNull();
    expect(ipv4LocalScope([100, 128, 0, 1], ssrf)).toBeNull();
  });

  it('screens multicast/reserved only under the SSRF posture', () => {
    expect(ipv4LocalScope([224, 0, 0, 1], ssrf)).toBe('multicast');
    expect(ipv4LocalScope([224, 0, 0, 1], boot)).toBeNull();
    expect(ipv4LocalScope([223, 255, 255, 255], ssrf)).toBeNull();
  });

  it('returns null for a null parse', () => {
    expect(ipv4LocalScope(null, ssrf)).toBeNull();
  });
});

describe('ipv6LocalScope', () => {
  it('names the local scopes by first hextet', () => {
    expect(ipv6LocalScope('::')).toBe('unspecified');
    expect(ipv6LocalScope('::1')).toBe('loopback');
    expect(ipv6LocalScope('fc00::1')).toBe('unique-local');
    expect(ipv6LocalScope('fd00::1')).toBe('unique-local');
    expect(ipv6LocalScope('fdff:ffff::1')).toBe('unique-local');
    expect(ipv6LocalScope('fe80::1')).toBe('link-local');
    expect(ipv6LocalScope('febf::1')).toBe('link-local');
    expect(ipv6LocalScope('fec0::1')).toBe('site-local');
    expect(ipv6LocalScope('feff::1')).toBe('site-local');
  });

  it('returns null for public literals', () => {
    expect(ipv6LocalScope('2606:4700:4700::1111')).toBeNull();
    expect(ipv6LocalScope('2001:db8::1')).toBeNull();
  });

  it('does not treat a short first hextet as in-range', () => {
    expect(ipv6LocalScope('fc::1')).toBeNull();
    expect(ipv6LocalScope('fdd::1')).toBeNull();
    expect(ipv6LocalScope('fe8::1')).toBeNull();
  });
});

describe('isPrivateHost — fail-closed SSRF posture', () => {
  it('rejects an absent host', () => {
    expect(isPrivateHost('')).toBe(true);
    expect(isPrivateHost(undefined)).toBe(true);
    expect(isPrivateHost(null)).toBe(true);
  });

  it('rejects out-of-range and octal-looking literals outright', () => {
    expect(isPrivateHost('4294967296')).toBe(true);   // decimal past 32 bits
    expect(isPrivateHost('0x1ffffffff')).toBe(true);  // hex past 32 bits
    expect(isPrivateHost('0177.0.0.1')).toBe(true);
    expect(isPrivateHost('010.0.0.1')).toBe(true);
  });

  it('widens past the fc00::/7 mask for short hextets', () => {
    expect(isPrivateHost('fc::1')).toBe(true);
    expect(isPrivateHost('fdd::1')).toBe(true);
  });

  it('resolves alternate IPv4 literal forms', () => {
    expect(isPrivateHost('2130706433')).toBe(true);   // 127.0.0.1
    expect(isPrivateHost('0x7f000001')).toBe(true);   // 127.0.0.1
    expect(isPrivateHost('0xa000001')).toBe(true);    // 10.0.0.1
  });

  it('strips brackets from IPv6 literals', () => {
    expect(isPrivateHost('[::1]')).toBe(true);
    expect(isPrivateHost('[fe80::1]')).toBe(true);
    expect(isPrivateHost('[2606:4700:4700::1111]')).toBe(false);
  });

  it('leaves public hosts alone', () => {
    expect(isPrivateHost('example.com')).toBe(false);
    expect(isPrivateHost('8.8.8.8')).toBe(false);
    expect(isPrivateHost('203.0.113.10')).toBe(false);
    expect(isPrivateHost('qurl.link')).toBe(false);
  });
});
