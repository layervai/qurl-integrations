/**
 * Tests for the target safety guard in scripts/loadtest-standalone.js.
 *
 * The script mints COUNT resources per round for the whole DURATION_S window —
 * tens of thousands on a normal run — so the guard deciding WHERE that volume
 * lands is the highest-consequence code in the file. It is also the part with
 * no natural runtime signal: a guard that wrongly permits a target does not
 * fail, it succeeds against the wrong environment.
 *
 * The guard replaced a two-entry exact-string denylist that failed OPEN. Each
 * spelling that slipped past that denylist has a test below, so a future
 * refactor back toward string equality fails here instead of in production.
 *
 * Requiring the script does NOT run the load test: its CLI entry point is
 * behind `require.main === module`, matching scripts/gateway-resume-spike.js.
 */

const {
  targetHost,
  isLoopbackHost,
  isProductionHost,
  parseTargetAllowlist,
  classifyTarget,
  classifyTargets,
  isRefusedTarget,
  normalizeHost,
  isTargetAuthorized,
} = require('../scripts/loadtest-standalone');

const NO_ALLOWLIST = new Set();
const verdictFor = (url, allowed = NO_ALLOWLIST) => classifyTarget('QURL_ENDPOINT', url, allowed).verdict;

describe('loadtest target guard — production spellings that the old denylist missed', () => {
  // The old guard was `config.QURL_ENDPOINT === 'https://api.layerv.ai'` and
  // `config.CONNECTOR_URL === 'https://get.qurl.link:9808'`. Everything in
  // this table is a real production host wearing a spelling that is not
  // byte-equal to those two strings.
  it.each([
    ['exact prod API', 'https://api.layerv.ai'],
    ['exact prod connector', 'https://get.qurl.link:9808'],
    ['trailing slash', 'https://api.layerv.ai/'],
    ['trailing slash on connector', 'https://get.qurl.link:9808/'],
    ['a path', 'https://api.layerv.ai/v1'],
    ['a different port', 'https://api.layerv.ai:8443'],
    ['no port on the connector host', 'https://get.qurl.link'],
    ['http instead of https', 'http://api.layerv.ai'],
    ['uppercase host', 'https://API.LayerV.AI'],
    ['a resolved qURL site', 'https://abc123.qurl.site'],
    ['the qurl.site apex', 'https://qurl.site'],
    ['the qurl.link apex', 'https://qurl.link'],
    ['a regional connector host', 'https://get-eu.qurl.link'],
    ['doubled trailing dots', 'https://api.layerv.ai../v1'],
    ['embedded credentials', 'https://user:pass@api.layerv.ai'],
    ['a fully-qualified trailing dot', 'https://api.layerv.ai./'],
  ])('refuses production via %s', (_label, url) => {
    expect(verdictFor(url)).toBe('production');
  });

  it('rejects a trailing-dot production host in LOADTEST_TARGET_HOSTS', () => {
    // Fail-open guard on the guard: `api.layerv.ai.` is the same host to DNS,
    // so if it were stored verbatim it would be an ordinary allowlist grant
    // that silently outranks nothing — and then permit production.
    const { hosts, errors } = parseTargetAllowlist('api.layerv.ai.');
    expect(errors).toHaveLength(1);
    expect(errors[0]).toContain('--allow-production');
    expect(hosts.size).toBe(0);
  });

  it('normalizes host case and every root dot', () => {
    expect(normalizeHost('API.LayerV.AI.')).toBe('api.layerv.ai');
    expect(normalizeHost('api.layerv.ai...')).toBe('api.layerv.ai');
    expect(normalizeHost('sandbox.example')).toBe('sandbox.example');
  });

  it('refuses production even when the host is named in LOADTEST_TARGET_HOSTS', () => {
    // Production must not be grantable from the environment — env vars get
    // copied between environments and pasted into .env.loadtest files.
    const allowed = new Set(['api.layerv.ai']);
    expect(verdictFor('https://api.layerv.ai', allowed)).toBe('production');
  });
});

describe('loadtest target guard — unrecognized hosts fail closed', () => {
  it.each([
    ['a regional host', 'https://api-eu.layerv.ai'],
    ['a future API hostname', 'https://api2.layerv.ai'],
    ['a customer custom domain', 'https://links.acme-corp.com'],
    ['a staging host not named in the allowlist', 'https://api.staging.layerv.ai'],
    ['a lookalike domain', 'https://api.layerv.ai.evil.example'],
    ['a non-loopback private address', 'http://10.0.0.5:8080'],
    ['an RFC1918 /16 address', 'http://192.168.1.10:9808'],
    ['a link-local address', 'http://169.254.169.254'],
  ])('refuses %s as unrecognized', (_label, url) => {
    expect(verdictFor(url)).toBe('unrecognized');
  });

  it.each([
    ['empty', ''],
    ['not a URL', 'not a url'],
    ['scheme-relative', '//api.layerv.ai'],
  ])('refuses an unparseable target (%s)', (_label, url) => {
    expect(verdictFor(url)).toBe('unparseable');
    expect(targetHost(url)).toBeNull();
  });

  it('does not treat private addresses as safe', () => {
    // A 10.x address is "private" per src/utils/private-host.js but can be a
    // production VPC endpoint, so isPrivateHost is deliberately not the
    // predicate here. Pin that: a change to isLoopbackHost that reaches for
    // isPrivateHost instead would flip these to true.
    expect(isLoopbackHost('10.0.0.5')).toBe(false);
    expect(isLoopbackHost('192.168.1.10')).toBe(false);
    expect(isLoopbackHost('172.16.0.1')).toBe(false);
    expect(isLoopbackHost('169.254.169.254')).toBe(false);
    expect(isLoopbackHost('[fd00::1]')).toBe(false);
  });
});

describe('loadtest target guard — hosts that may proceed', () => {
  it.each([
    ['localhost, the config dev default', 'http://localhost:8080'],
    ['the connector dev default', 'http://localhost:9808'],
    ['IPv4 loopback', 'http://127.0.0.1:8080'],
    ['elsewhere in 127.0.0.0/8', 'http://127.7.7.7:1'],
    ['IPv6 loopback', 'http://[::1]:8080'],
  ])('permits %s', (_label, url) => {
    expect(verdictFor(url)).toBe('loopback');
  });

  it('permits a host the operator named in LOADTEST_TARGET_HOSTS', () => {
    const { hosts, errors } = parseTargetAllowlist('sandbox.example.internal');
    expect(errors).toEqual([]);
    expect(verdictFor('https://sandbox.example.internal', hosts)).toBe('allowlisted');
  });

  it('matches allowlisted hosts regardless of port, path, or case', () => {
    const { hosts } = parseTargetAllowlist('sandbox.example.internal');
    expect(verdictFor('https://sandbox.example.internal:9808/api', hosts)).toBe('allowlisted');
    expect(verdictFor('https://SANDBOX.example.internal', hosts)).toBe('allowlisted');
  });

  it('does not extend an allowlisted host to its subdomains', () => {
    // Entries are exact hosts. Granting a parent would silently widen the
    // grant to every name under it.
    const { hosts } = parseTargetAllowlist('sandbox.example.internal');
    expect(verdictFor('https://api.sandbox.example.internal', hosts)).toBe('unrecognized');
  });

  it.each([
    ['a qurl.site tunnel host', 'sbx.qurl.site.layerv.ai'],
    ['a qurl.link tunnel host', 'qurl.link.layerv.xyz'],
  ])('does not misfile %s as production', (_label, host) => {
    // The non-prod tunnel spellings end in .layerv.ai / .layerv.xyz, so
    // widening PROD_DOMAINS to qurl.link must not swallow them.
    expect(isProductionHost(host)).toBe(false);
    const { hosts, errors } = parseTargetAllowlist(host);
    expect(errors).toEqual([]);
    expect(verdictFor(`https://${host}`, hosts)).toBe('allowlisted');
  });

  it('permits a non-prod tunnel host without misfiling it as production', () => {
    // `.qurl.site.layerv.ai` is connector.js's NON-prod tunnel suffix. It must
    // not collide with the `qurl.site` production domain rule — it is merely
    // unrecognized until the operator names it.
    expect(isProductionHost('sbx.qurl.site.layerv.ai')).toBe(false);
    expect(isProductionHost('sbx.qurl.site.layerv.xyz')).toBe(false);
    const { hosts } = parseTargetAllowlist('sbx.qurl.site.layerv.ai');
    expect(verdictFor('https://sbx.qurl.site.layerv.ai', hosts)).toBe('allowlisted');
  });
});

describe('loadtest target guard — LOADTEST_TARGET_HOSTS parsing', () => {
  it('splits, trims, lowercases, and drops empties', () => {
    const { hosts, errors } = parseTargetAllowlist('  A.example , ,b.example,\n c.example ,');
    expect(errors).toEqual([]);
    expect([...hosts].sort()).toEqual(['a.example', 'b.example', 'c.example']);
  });

  it.each([
    ['undefined', undefined],
    ['unset/empty', ''],
    ['only separators', ' , , '],
  ])('yields an empty allowlist for %s', (_label, raw) => {
    const { hosts, errors } = parseTargetAllowlist(raw);
    expect(errors).toEqual([]);
    expect(hosts.size).toBe(0);
  });

  // Fail-fast on entries that could never match a hostname, matching config.js's
  // posture on DETECT_EXTRA_NON_PROD_HOST_SUFFIXES: a quietly-inert grant means
  // the operator believes they allowed a host they did not.
  it.each([
    ['a scheme', 'https://sandbox.example'],
    ['a port', 'sandbox.example:9808'],
    ['a path', 'sandbox.example/api'],
    ['a bracketed IPv6 literal', '[::1]'],
  ])('rejects an entry carrying %s', (_label, entry) => {
    const { hosts, errors } = parseTargetAllowlist(entry);
    expect(errors).toHaveLength(1);
    expect(errors[0]).toContain('bare hostname');
    expect(hosts.size).toBe(0);
  });

  it.each([
    ['1', '1'],
    ['0', '0'],
    ['12345', '12345'],
  ])('rejects the boolean-looking value %s', (_label, entry) => {
    // The name takes hostnames, but reads enough like a switch that someone
    // will try `=1`. Stored verbatim it is a grant that can never match:
    // `new URL('http://1')` canonicalizes the host to the ADDRESS 0.0.0.1.
    const { hosts, errors } = parseTargetAllowlist(entry);
    expect(errors).toHaveLength(1);
    expect(errors[0]).toContain('not an on/off switch');
    expect(hosts.size).toBe(0);
  });

  it('rejects a wildcard entry rather than silently not matching it', () => {
    const { hosts, errors } = parseTargetAllowlist('*.example');
    expect(errors).toHaveLength(1);
    expect(errors[0]).toContain('wildcards');
    expect(hosts.size).toBe(0);
  });

  it.each([
    ['the prod API host', 'api.layerv.ai'],
    ['the prod connector host', 'get.qurl.link'],
    ['a prod qURL site', 'abc123.qurl.site'],
    ['the qurl.link apex', 'qurl.link'],
    ['a regional connector', 'get-eu.qurl.link'],
    ['a prod host with doubled trailing dots', 'api.layerv.ai..'],
  ])('rejects %s and points at the command-line flag', (_label, entry) => {
    const { hosts, errors } = parseTargetAllowlist(entry);
    expect(errors).toHaveLength(1);
    expect(errors[0]).toContain('--allow-production');
    expect(hosts.size).toBe(0);
  });

  it.each([
    ['a leading empty label', '.sandbox.example'],
    ['a doubled inner dot', 'sandbox..example'],
    ['a space', 'sandbox example'],
  ])('rejects an entry with %s as unparseable', (_label, entry) => {
    const { hosts, errors } = parseTargetAllowlist(entry);
    expect(errors).toHaveLength(1);
    expect(hosts.size).toBe(0);
    expect(errors[0]).toMatch(/not a parseable hostname|bare hostname/);
  });

  it('normalizes an entry the same way a target URL is normalized', () => {
    // A fullwidth 's' is IDNA-mapped to ASCII by the URL parser on the TARGET
    // side. Storing the entry verbatim would make it a grant no target could
    // ever match — inert, and the operator would see a refusal for a host they
    // believe they named.
    const { hosts, errors } = parseTargetAllowlist('\uFF53andbox.example.internal');
    expect(errors).toEqual([]);
    expect([...hosts]).toEqual(['sandbox.example.internal']);
    expect(verdictFor('https://sandbox.example.internal', hosts)).toBe('allowlisted');
  });

  it('keeps the valid entries alongside a rejected one', () => {
    const { hosts, errors } = parseTargetAllowlist('good.example,api.layerv.ai,also-good.example');
    expect(errors).toHaveLength(1);
    expect([...hosts].sort()).toEqual(['also-good.example', 'good.example']);
  });
});

describe('loadtest target guard — both targets are judged', () => {
  it('judges the connector independently of the API endpoint', () => {
    // The failure this pins: a sandbox API paired with a production connector.
    // Both targets take load, so both have to clear the guard.
    const targets = classifyTargets({
      qurlEndpoint: 'http://localhost:8080',
      connectorUrl: 'https://get.qurl.link:9808',
      allowedHosts: NO_ALLOWLIST,
    });
    expect(targets.map((t) => [t.name, t.verdict])).toEqual([
      ['QURL_ENDPOINT', 'loopback'],
      ['CONNECTOR_URL', 'production'],
    ]);
    expect(targets.filter(isRefusedTarget)).toHaveLength(1);
  });

  it('reports no refusals when both targets are recognized', () => {
    const targets = classifyTargets({
      qurlEndpoint: 'http://localhost:8080',
      connectorUrl: 'http://localhost:9808',
      allowedHosts: NO_ALLOWLIST,
    });
    expect(targets.filter(isRefusedTarget)).toEqual([]);
  });

  it('refuses both when neither is recognized', () => {
    const targets = classifyTargets({
      qurlEndpoint: 'https://api-eu.layerv.ai',
      connectorUrl: 'https://connector-eu.layerv.ai',
      allowedHosts: NO_ALLOWLIST,
    });
    expect(targets.filter(isRefusedTarget)).toHaveLength(2);
  });

  it('carries the raw target through for the operator-facing message', () => {
    // The refusal prints rawUrl, not the parsed host, so the operator sees
    // exactly the value their environment supplied.
    const [endpoint] = classifyTargets({
      qurlEndpoint: 'https://api.layerv.ai:8443/v1',
      connectorUrl: 'http://localhost:9808',
      allowedHosts: NO_ALLOWLIST,
    });
    expect(endpoint.rawUrl).toBe('https://api.layerv.ai:8443/v1');
    expect(endpoint.host).toBe('api.layerv.ai');
  });
});

describe('loadtest target guard — override strength is asymmetric', () => {
  const FLAG_ONLY = { allowProdFlag: true, allowProdEnv: false };
  const ENV_ONLY = { allowProdFlag: false, allowProdEnv: true };
  const NEITHER = { allowProdFlag: false, allowProdEnv: false };
  const target = (verdict) => ({ name: 'QURL_ENDPOINT', rawUrl: 'x', host: 'h', verdict });

  it('lets the env var clear a merely unrecognized target', () => {
    // An unnamed sandbox is a recognition failure, not a known-dangerous
    // target, so the cheaper override is enough.
    expect(isTargetAuthorized(target('unrecognized'), ENV_ONLY)).toBe(true);
    expect(isTargetAuthorized(target('unparseable'), ENV_ONLY)).toBe(true);
  });

  it('does NOT let the env var clear a production target', () => {
    // The load-bearing one. `.env.loadtest` is spliced into process.env when
    // the script runs, so an env-only override would let a gitignored file in
    // a working copy silently disable this guard for every future run — the
    // same vector parseTargetAllowlist refuses for LOADTEST_TARGET_HOSTS.
    expect(isTargetAuthorized(target('production'), ENV_ONLY)).toBe(false);
  });

  it('lets the typed flag clear anything', () => {
    expect(isTargetAuthorized(target('production'), FLAG_ONLY)).toBe(true);
    expect(isTargetAuthorized(target('unrecognized'), FLAG_ONLY)).toBe(true);
  });

  it('authorizes safe verdicts with no override at all', () => {
    expect(isTargetAuthorized(target('loopback'), NEITHER)).toBe(true);
    expect(isTargetAuthorized(target('allowlisted'), NEITHER)).toBe(true);
  });

  it('refuses every unsafe verdict and no safe one', () => {
    // Direct assertion on the predicate itself, rather than only through
    // classifyTargets — a verdict added to neither set would slip past.
    expect(['loopback', 'allowlisted'].map((v) => isRefusedTarget(target(v)))).toEqual([false, false]);
    expect(['production', 'unrecognized', 'unparseable'].map((v) => isRefusedTarget(target(v))))
      .toEqual([true, true, true]);
  });
});

describe('loadtest target guard — the mirrored production hostnames are real', () => {
  // PROD_HOSTS mirrors src/config.js's NODE_ENV=production fallbacks (the
  // TODO(upstream-contract) marker says so). A comment cannot notice drift;
  // this can. If config.js moves a production endpoint and this list is not
  // moved with it, the guard silently stops recognizing production and this
  // test fails instead.
  const loadProdConfig = () => {
    const saved = { ...process.env };
    jest.resetModules();
    process.env.NODE_ENV = 'production';
    delete process.env.QURL_ENDPOINT;
    delete process.env.CONNECTOR_URL;
    try {
      return require('../src/config');
    } finally {
      process.env = saved;
      jest.resetModules();
    }
  };

  it('classifies both of config.js production fallbacks as production', () => {
    const prodConfig = loadProdConfig();
    const targets = classifyTargets({
      qurlEndpoint: prodConfig.QURL_ENDPOINT,
      connectorUrl: prodConfig.CONNECTOR_URL,
      allowedHosts: NO_ALLOWLIST,
    });
    expect(targets.map((t) => t.verdict)).toEqual(['production', 'production']);
  });
});
