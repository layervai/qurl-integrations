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
  targetGuardReport,
  resolveGuardInputs,
  VERDICT_LABEL,
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
    ['a domain merely ending in the prod name', 'https://notqurl.site'],
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
    ['scheme-less host:port', 'localhost:8080'],
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

  it('lets the env var clear an unnamed sandbox', () => {
    // A host the operator simply did not enumerate is a recognition failure,
    // not a known-dangerous target, so the cheaper override is enough.
    expect(isTargetAuthorized(target('unrecognized'), ENV_ONLY)).toBe(true);
  });

  it('does NOT let the env var clear an unparseable target', () => {
    // A URL that will not parse is never a legitimate sandbox, so there is
    // nothing for the weaker override to mean.
    expect(isTargetAuthorized(target('unparseable'), ENV_ONLY)).toBe(false);
    expect(isTargetAuthorized(target('unparseable'), FLAG_ONLY)).toBe(true);
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
    // config.js throws at module load if this is set under NODE_ENV=production.
    // Unset here so the drift check fails on drift, not on the DDB-mock setup
    // a future runner might have in its environment.
    delete process.env.DDB_TEST_ENDPOINT;
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

describe('loadtest target guard — the enforcement decision', () => {
  // main() is unreachable from a test (it runs the load test) and scripts/ is
  // outside collectCoverageFrom, so this is the only thing standing between a
  // refactor and a guard that silently stops guarding.
  const build = (urls, opts = {}) => targetGuardReport({
    targets: classifyTargets({
      qurlEndpoint: urls.qurl,
      connectorUrl: urls.connector,
      allowedHosts: opts.allowedHosts || NO_ALLOWLIST,
    }),
    allowlistErrors: opts.allowlistErrors || [],
    allowProdFlag: opts.allowProdFlag || false,
    allowProdEnv: opts.allowProdEnv || false,
  });
  const SAFE = { qurl: 'http://localhost:8080', connector: 'http://localhost:9808' };
  const PROD = { qurl: 'https://api.layerv.ai', connector: 'http://localhost:9808' };

  it('is not fatal and says nothing when both targets are recognized', () => {
    const r = build(SAFE);
    expect(r.fatal).toBe(false);
    expect(r.lines).toEqual([]);
    expect(r.warnings).toEqual([]);
  });

  it('blocks a production target when no override is given', () => {
    const r = build(PROD);
    expect(r.fatal).toBe(true);
    expect(r.blocked.map((t) => t.name)).toEqual(['QURL_ENDPOINT']);
    expect(r.lines.join('\n')).toContain('pass --allow-production on the command line');
  });

  it('blocks a production target under the env override alone', () => {
    // The regression that matters most: .env.loadtest must not be able to do
    // this. Asserted on the real decision, not just on isTargetAuthorized.
    const r = build(PROD, { allowProdEnv: true });
    expect(r.fatal).toBe(true);
    expect(r.warnings).toEqual([]);
  });

  it('permits a production target under the typed flag, and says which override fired', () => {
    const r = build(PROD, { allowProdFlag: true });
    expect(r.fatal).toBe(false);
    expect(r.warnings).toHaveLength(1);
    expect(r.warnings[0]).toContain('overridden via --allow-production');
  });

  it('names the env override by name when it is the one that fired', () => {
    const r = build({ qurl: 'https://sbx.example.invalid', connector: 'http://localhost:9808' }, { allowProdEnv: true });
    expect(r.fatal).toBe(false);
    expect(r.warnings[0]).toContain('overridden via LOADTEST_ALLOW_PRODUCTION=1');
  });

  it('keeps the port visible in the override warning', () => {
    // rawUrl, not host — a warning that hid :9808 would misreport the target.
    const r = build({ qurl: 'http://localhost:8080', connector: 'https://sbx.example.invalid:9808' }, { allowProdFlag: true });
    expect(r.warnings[0]).toContain('sbx.example.invalid:9808');
  });

  it('offers a paste-ready allowlist line naming only the unrecognized host', () => {
    const r = build({ qurl: 'https://api-eu.layerv.ai:8443/v1', connector: 'https://api.layerv.ai' });
    const line = r.lines.find((l) => l.includes('LOADTEST_TARGET_HOSTS='));
    expect(line).toBe('If that is the intended sandbox: LOADTEST_TARGET_HOSTS=api-eu.layerv.ai');
    // Never offers to allowlist the production host alongside it.
    expect(line).not.toContain('api.layerv.ai,');
  });

  it('tells the operator a scheme is missing rather than offering an empty grant', () => {
    const r = build({ qurl: 'localhost:8080', connector: 'http://localhost:9808' });
    expect(r.fatal).toBe(true);
    expect(r.lines.join('\n')).toContain('needs a scheme');
    expect(r.lines.join('\n')).not.toContain('LOADTEST_TARGET_HOSTS=\n');
    expect(r.lines.some((l) => l.endsWith('LOADTEST_TARGET_HOSTS='))).toBe(false);
  });

  it('is fatal on a malformed allowlist entry even when the targets are fine', () => {
    const r = build(SAFE, { allowlistErrors: ['bad entry'] });
    expect(r.fatal).toBe(true);
    expect(r.lines[0]).toBe('FATAL: bad entry');
  });

  it('is fatal on a malformed allowlist entry even under the typed flag', () => {
    const r = build(SAFE, { allowlistErrors: ['bad entry'], allowProdFlag: true });
    expect(r.fatal).toBe(true);
  });

  it('always prints both targets so the operator sees which half was fine', () => {
    const table = build(PROD).lines.filter((l) => l.startsWith('  '));
    expect(table).toHaveLength(2);
    expect(table[0]).toContain('QURL_ENDPOINT');
    expect(table[1]).toContain('CONNECTOR_URL');
  });

  it('labels every verdict, marking safe ones ok and the rest REFUSED', () => {
    // A verdict added to neither SAFE_VERDICTS nor VERDICT_LABEL would print
    // `[undefined]` in the operator's table.
    const verdicts = ['loopback', 'allowlisted', 'production', 'unrecognized', 'unparseable'];
    for (const v of verdicts) {
      expect(VERDICT_LABEL[v]).toBeDefined();
      const safe = !isRefusedTarget({ verdict: v });
      expect(VERDICT_LABEL[v].startsWith(safe ? 'ok' : 'REFUSED')).toBe(true);
    }
  });
});

describe('loadtest target guard — requiring the script is inert', () => {
  it('does not run the load test or touch process.env', () => {
    // The file gates BOTH main() and the .env.loadtest splice on
    // `require.main === module`. If a future edit dropped the conjunct on the
    // splice, a developer with a .env.loadtest would silently get it merged
    // into the jest process while CI, which has no such file, stayed green.
    const before = JSON.stringify(process.env);
    jest.isolateModules(() => { require('../scripts/loadtest-standalone'); });
    expect(JSON.stringify(process.env)).toBe(before);
  });
});

describe('loadtest target guard — the names read from the outside world', () => {
  // Pins the literal env-var and flag spellings. Every other test supplies
  // these values directly, so without this a rename here would be invisible.
  it('reads the allowlist from LOADTEST_TARGET_HOSTS', () => {
    const r = resolveGuardInputs({ LOADTEST_TARGET_HOSTS: 'sbx.example.internal' }, []);
    expect([...r.hosts]).toEqual(['sbx.example.internal']);
    expect(r.errors).toEqual([]);
  });

  it('ignores an allowlist under any other variable name', () => {
    const r = resolveGuardInputs({ LOADTEST_TARGET_OK: 'sbx.example.internal' }, []);
    expect(r.hosts.size).toBe(0);
  });

  it('reads the env override from LOADTEST_ALLOW_PRODUCTION=1 only', () => {
    expect(resolveGuardInputs({ LOADTEST_ALLOW_PRODUCTION: '1' }, []).allowProdEnv).toBe(true);
    // Must be the literal '1' — 'true'/'yes' stay off, matching the repo's
    // posture on MAP_COMMAND_ENABLED, so a typo cannot enable an override.
    expect(resolveGuardInputs({ LOADTEST_ALLOW_PRODUCTION: 'true' }, []).allowProdEnv).toBe(false);
    expect(resolveGuardInputs({}, []).allowProdEnv).toBe(false);
  });

  it('reads the flag from argv as --allow-production', () => {
    expect(resolveGuardInputs({}, ['--allow-production']).allowProdFlag).toBe(true);
    expect(resolveGuardInputs({}, ['--allow-prod']).allowProdFlag).toBe(false);
    expect(resolveGuardInputs({}, []).allowProdFlag).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Static check: the script calls mintLinks with its options object.
//
// Filed after PR #1168 fixed a real 100%-failure bug — the file leg called
// `mintLinks(resourceId, expiresAt, batchSize)` positionally against a
// `(resourceId, { expiresAt, n, ... })` signature. Destructuring an ISO string
// yields `undefined` for every key, so the `n` bound check threw before fetch
// and the leg issued ZERO mint requests while reporting them as failures.
//
// This is a STATIC check because runRound is unreachable from a test: it is
// not in module.exports, its only caller main() is behind
// `require.main === module`, and scripts/ sits outside collectCoverageFrom, so
// neither jest nor the coverage gate can see the call site. eslint extends
// only eslint:recommended — no arity or type checking — so it cannot see it
// either. Parsing the source is what is left. Same approach as
// tests/ddb-reserved-words-static.test.js.
//
// The expected key set is derived from connector.js's actual parameter list
// rather than hard-coded, so a rename there surfaces here instead of silently
// making this test assert a stale contract.

const fs = require('fs');
const path = require('path');
const parser = require('@babel/parser');
const traverseModule = require('@babel/traverse');
const traverse = traverseModule.default || traverseModule;

describe('loadtest script — mintLinks call shape', () => {
  const parseFile = (...segments) =>
    parser.parse(fs.readFileSync(path.join(__dirname, '..', ...segments), 'utf8'), {
      sourceType: 'unambiguous',
    });

  /** Option names mintLinks actually destructures, read from its signature. */
  const declaredOptionKeys = () => {
    let keys = null;
    traverse(parseFile('src', 'connector.js'), {
      FunctionDeclaration(p) {
        if (p.node.id?.name !== 'mintLinks') return;
        const second = p.node.params[1];
        // `{ ... } = {}` parses as AssignmentPattern wrapping the ObjectPattern.
        const pattern = second.type === 'AssignmentPattern' ? second.left : second;
        expect(pattern.type).toBe('ObjectPattern');
        keys = new Set(
          pattern.properties.filter(pr => pr.type === 'ObjectProperty').map(pr => pr.key.name),
        );
      },
    });
    expect(keys).not.toBeNull();
    return keys;
  };

  /** Every mintLinks(...) call node in the load-test script. */
  const mintLinksCalls = () => {
    const calls = [];
    traverse(parseFile('scripts', 'loadtest-standalone.js'), {
      CallExpression(p) {
        const callee = p.node.callee;
        const name =
          callee.type === 'Identifier' ? callee.name
            : callee.type === 'MemberExpression' && callee.property.type === 'Identifier'
              ? callee.property.name
              : null;
        if (name === 'mintLinks') calls.push(p.node);
      },
    });
    return calls;
  };

  it('passes an options object, never positional arguments', () => {
    const calls = mintLinksCalls();
    // Fails closed if the call site moves or a second one appears unreviewed.
    expect(calls).toHaveLength(1);
    const args = calls[0].arguments;
    // mintLinks takes exactly (resourceId, opts) — a third argument is the
    // positional form that silently dropped batchSize.
    expect(args).toHaveLength(2);
    expect(args[1].type).toBe('ObjectExpression');
  });

  it('names only options mintLinks destructures, and always passes n', () => {
    const declared = declaredOptionKeys();
    const opts = mintLinksCalls()[0].arguments[1];
    // A spread would make the passed keys unresolvable statically; keep the
    // call site literal so this check stays meaningful.
    expect(opts.properties.every(pr => pr.type === 'ObjectProperty')).toBe(true);
    const passed = opts.properties.map(pr => pr.key.name ?? pr.key.value);
    // Catches a misspelled key (`count:`/`batchSize:`) that would leave n
    // undefined and reproduce the original throw.
    expect(passed.filter(k => !declared.has(k))).toEqual([]);
    // n is the one whose absence caused the 100%-failure bug.
    expect(passed).toContain('n');
    expect(passed).toContain('expiresAt');
  });
});
