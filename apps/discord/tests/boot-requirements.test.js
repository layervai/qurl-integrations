
const {
  bootRequired,
  prodRequired,
  missingBootKeys,
  missingProdKeys,
  missingKekRequiredKeys,
  baseUrlHttpsProblem,
  missingEventShipperKeys,
  unsupportedRoleShipperCombo,
  unsupportedRoleResumeCombo,
  unsupportedRoleHotStandbyCombo,
  missingHotStandbyKeys,
  invalidHotStandbyValues,
  invalidStateSecretValues,
  shouldRegisterInteractionListener,
  missingMapCommandKeys,
  GOOGLE_MAPS_API_KEY_PLACEHOLDER_SENTINEL,
  VALID_PROCESS_ROLES,
  resolveProcessRole,
} = require('../src/boot-requirements');
const { MIN_STATE_SECRET_LENGTH } = require('../src/utils/oauth-state');

describe('bootRequired', () => {
  it('demands only DISCORD_TOKEN (GUILD_ID and BASE_URL are enforced upstream)', () => {
    expect(bootRequired()).toEqual(['DISCORD_TOKEN']);
  });
});

describe('prodRequired', () => {
  it('omits QURL_API_KEY — every deployment configures per-guild via /qurl setup', () => {
    expect(prodRequired().sort()).toEqual([
      'KEY_ENCRYPTION_KEY', 'METRICS_TOKEN',
    ]);
  });
});

describe('missingBootKeys', () => {
  it('returns empty when every boot key is present', () => {
    expect(missingBootKeys({ DISCORD_TOKEN: 't', GUILD_ID: '123', BASE_URL: 'https://h' })).toEqual([]);
  });

  it('surfaces the exact missing key (not just a count)', () => {
    expect(missingBootKeys({})).toEqual(['DISCORD_TOKEN']);
  });

  it('does not flag GUILD_ID or BASE_URL as missing — both are optional here', () => {
    expect(missingBootKeys({ DISCORD_TOKEN: 't' })).toEqual([]);
  });

  it('treats empty strings as missing (not just undefined)', () => {
    expect(missingBootKeys({ DISCORD_TOKEN: '' })).toEqual(['DISCORD_TOKEN']);
  });
});

describe('missingProdKeys', () => {
  it('returns empty when every prod key is set in env', () => {
    expect(missingProdKeys({ METRICS_TOKEN: 'x', KEY_ENCRYPTION_KEY: 'x' })).toEqual([]);
  });

  it('does not demand QURL_API_KEY', () => {
    expect(missingProdKeys({ METRICS_TOKEN: 'x', KEY_ENCRYPTION_KEY: 'x', QURL_API_KEY: undefined })).toEqual([]);
  });

  it('surfaces missing encryption key loudly — no silent fallback possible', () => {
    expect(missingProdKeys({ METRICS_TOKEN: 'x' })).toEqual(['KEY_ENCRYPTION_KEY']);
  });
});

describe('missingKekRequiredKeys', () => {
  it('returns empty when qURL OAuth is not configured (nothing persists a key, no KEK demand)', () => {
    expect(missingKekRequiredKeys({}, false)).toEqual([]);
    expect(missingKekRequiredKeys({ KEY_ENCRYPTION_KEY: '' }, false)).toEqual([]);
  });

  it('flags KEY_ENCRYPTION_KEY when qURL OAuth is configured without KEK', () => {
    expect(missingKekRequiredKeys({}, true)).toEqual(['KEY_ENCRYPTION_KEY']);
    expect(missingKekRequiredKeys({ KEY_ENCRYPTION_KEY: '' }, true)).toEqual(['KEY_ENCRYPTION_KEY']);
  });

  it('returns empty when both hold, regardless of NODE_ENV', () => {
    expect(missingKekRequiredKeys({ KEY_ENCRYPTION_KEY: 'k' }, true)).toEqual([]);
  });
});

describe('baseUrlHttpsProblem', () => {
  const LOCALHOST = 'http://localhost:3000'; // config.js BASE_URL default
  function cfg(overrides = {}) {
    return {
      isQurlOAuthConfigured: false,
      BASE_URL: LOCALHOST,
      ...overrides,
    };
  }

  it('accepts a bare https:// BASE_URL origin (the good prod case)', () => {
    const HTTPS = 'https://bot.example.com';
    expect(baseUrlHttpsProblem(cfg({ BASE_URL: HTTPS }), true)).toBeNull();
    expect(baseUrlHttpsProblem(cfg({ isQurlOAuthConfigured: true, BASE_URL: HTTPS }), true)).toBeNull();
  });

  it('accepts public origins that the local-only screen must not false-positive', () => {
    for (const good of [
      'https://bot.eu.example.com',
      'https://bot.example.com:8443',
      'https://172.32.0.1',
      'https://[2001:db8::1]',
    ]) {
      expect(baseUrlHttpsProblem(cfg({ isQurlOAuthConfigured: true, BASE_URL: good }), true)).toBeNull();
    }
  });

  it('accepts an uppercase HTTPS:// scheme (URL scheme is case-insensitive)', () => {
    expect(
      baseUrlHttpsProblem(cfg({ isQurlOAuthConfigured: true, BASE_URL: 'HTTPS://bot.example.com' }), true),
    ).toBeNull();
  });

  it('rejects a host-less "https://" BASE_URL (would build a broken redirect)', () => {
    const msg = baseUrlHttpsProblem(cfg({ isQurlOAuthConfigured: true, BASE_URL: 'https://' }), true);
    expect(msg).not.toBeNull();
    expect(msg).toContain('public bare https:// origin');
    expect(msg).toContain('Got: https://.');
  });

  it('rejects qURL OAuth configured + BASE_URL with path/query/fragment/userinfo', () => {
    for (const bad of [
      'https://bot.example.com/prefix',
      'https://bot.example.com?debug=true',
      'https://bot.example.com#callback',
    ]) {
      const msg = baseUrlHttpsProblem(cfg({ isQurlOAuthConfigured: true, BASE_URL: bad }), true);
      expect(msg).not.toBeNull();
      expect(msg).toContain('public bare https:// origin');
      expect(msg).toContain(bad);
    }
  });

  it('redacts BASE_URL userinfo from boot errors — malformed values too', () => {
    for (const bad of [
      'https://svc:hunter2@bot.example.com', //      parses -> URL-level redaction
      'https://svc:hunter2@bot.example.com:port', // invalid port -> parse throws
      'https://svc:hunter2@', //                    host-less -> parse throws
      'https://:hunter2@bot.example.com', //         password-only branch
    ]) {
      const msg = baseUrlHttpsProblem(cfg({ isQurlOAuthConfigured: true, BASE_URL: bad }), true);
      expect(msg).not.toBeNull();
      expect(msg).toContain('public bare https:// origin');
      expect(msg).not.toMatch(/hunter2/);
      expect(msg).not.toMatch(/svc/);
    }
  });

  it('redacts surgically — the host survives so the error stays diagnosable', () => {
    const msg = baseUrlHttpsProblem(
      cfg({ isQurlOAuthConfigured: true, BASE_URL: 'https://svc:hunter2@bot.example.com:port' }),
      true,
    );
    expect(msg).toContain('Got: https://bot.example.com:port');
    expect(msg).not.toMatch(/hunter2/);
  });

  it('rejects qURL OAuth configured + local-only BASE_URL host literals', () => {
    for (const bad of [
      'https://localhost',
      'https://bot.localhost',
      'https://127.0.0.1',
      'https://10.0.3.4',
      'https://172.16.0.2',
      'https://192.168.1.20',
      'https://169.254.169.254', // link-local / cloud instance metadata
      'https://0.0.0.0', //         unspecified address
      'https://localhost.', //      absolute-FQDN form of localhost
      'https://[::1]',
      'https://[::ffff:127.0.0.1]', // IPv4-mapped loopback (serializes as ::ffff:7f00:1)
      'https://[fd00::1]', //          unique-local, fc00::/7
      'https://[fe80::1]', //          link-local, fe80::/10
    ]) {
      const msg = baseUrlHttpsProblem(cfg({ isQurlOAuthConfigured: true, BASE_URL: bad }), true);
      expect(msg).not.toBeNull();
      expect(msg).toContain('public bare https:// origin');
      expect(msg).toContain(bad);
    }
  });

  it('lets a non-consuming deploy keep a non-origin https BASE_URL', () => {
    const nonConsuming = { isQurlOAuthConfigured: false, BASE_URL: 'https://bot.example.com/prefix' };
    expect(baseUrlHttpsProblem(nonConsuming, true)).toBeNull();
    expect(baseUrlHttpsProblem({ ...nonConsuming, isQurlOAuthConfigured: true }, true)).not.toBeNull();
  });

  it('rejects alternate IPv4 encodings of a local-only host', () => {
    for (const bad of [
      'https://0x7f000001', // hex      -> 127.0.0.1
      'https://2130706433', // decimal  -> 127.0.0.1
      'https://0177.0.0.1', // octal    -> 127.0.0.1
      'https://127.1', //     short     -> 127.0.0.1
    ]) {
      expect(new URL(bad).hostname).toBe('127.0.0.1');
      const msg = baseUrlHttpsProblem(cfg({ isQurlOAuthConfigured: true, BASE_URL: bad }), true);
      expect(msg).not.toBeNull();
      expect(msg).toContain('public bare https:// origin');
    }
  });

  it('accepts public origins the SSRF guard would reject', () => {
    for (const ok of [
      'https://100.64.0.1', //       CGNAT 100.64/10 can front a reachable origin
      'https://100.127.255.255', //  top of the CGNAT range
      'https://[fec0::1]', //        deprecated site-local, not a boot concern
      'https://10.2.3.1e2', //       DOMAIN: Number('1e2') is 100, but the URL
      'https://192.168.0.1e1', //    spec's IPv4 parser rejects the label
      'https://bot.example.com',
      'https://fd-detect.qurl.link', //  ULA-looking name, but no colon
    ]) {
      expect(baseUrlHttpsProblem(cfg({ isQurlOAuthConfigured: true, BASE_URL: ok }), true))
        .toBeNull();
    }
  });

  it('rejects qURL OAuth configured + BASE_URL unset (localhost fallback)', () => {
    const msg = baseUrlHttpsProblem(
      cfg({ isQurlOAuthConfigured: true, BASE_URL: LOCALHOST }),
      false, // not explicitly set — fell back to the localhost default
    );
    expect(msg).not.toBeNull();
    expect(msg).toMatch(/BASE_URL/);
    expect(msg).toMatch(/https:\/\//);
    expect(msg).toMatch(/qURL/);
    expect(msg).toContain(LOCALHOST);
  });

  it('does not false-positive a non-consuming deploy with BASE_URL unset', () => {
    expect(baseUrlHttpsProblem(cfg({ BASE_URL: LOCALHOST }), false)).toBeNull();
  });

  it('rejects a stale explicit http:// BASE_URL even when no surface consumes it', () => {
    const msg = baseUrlHttpsProblem(cfg({ BASE_URL: 'http://stale.example.com' }), true);
    expect(msg).not.toBeNull();
    expect(msg).toContain('http://stale.example.com');
  });

  it('uses the OAuth-aware message for consuming surfaces and the terse canary otherwise', () => {
    const consuming = baseUrlHttpsProblem(cfg({ isQurlOAuthConfigured: true, BASE_URL: LOCALHOST }), false);
    const canary = baseUrlHttpsProblem(cfg({ BASE_URL: 'http://x.example.com' }), true);
    expect(consuming).toMatch(/OAuth redirect/);
    expect(canary).not.toMatch(/OAuth redirect/);
    expect(canary).toMatch(/BASE_URL must use https:\/\/ in production/);
  });
});

describe('missingEventShipperKeys', () => {
  it('returns empty when the flag is unset (event-shipper path inactive)', () => {
    expect(missingEventShipperKeys({})).toEqual([]);
    expect(missingEventShipperKeys({ ENABLE_EVENT_SHIPPER: false })).toEqual([]);
    expect(
      missingEventShipperKeys({ ENABLE_EVENT_SHIPPER: false, QURL_BOT_EVENTS_QUEUE_URL: undefined })
    ).toEqual([]);
  });

  it('flags QURL_BOT_EVENTS_QUEUE_URL when flag is on without a URL', () => {
    expect(
      missingEventShipperKeys({ ENABLE_EVENT_SHIPPER: true })
    ).toEqual(['QURL_BOT_EVENTS_QUEUE_URL']);
    expect(
      missingEventShipperKeys({ ENABLE_EVENT_SHIPPER: true, QURL_BOT_EVENTS_QUEUE_URL: '' })
    ).toEqual(['QURL_BOT_EVENTS_QUEUE_URL']);
  });

  it('returns empty when both are set', () => {
    expect(
      missingEventShipperKeys({
        ENABLE_EVENT_SHIPPER: true,
        QURL_BOT_EVENTS_QUEUE_URL: 'https://sqs.us-east-2.amazonaws.com/123/qurl-bot-events',
      }),
    ).toEqual([]);
  });
});

describe('unsupportedRoleShipperCombo', () => {
  it('rejects combined + flag-on with operator-facing remediation', () => {
    const msg = unsupportedRoleShipperCombo('combined', true);
    expect(msg).not.toBeNull();
    expect(msg).toMatch(/PROCESS_ROLE=combined/);
    expect(msg).toMatch(/ENABLE_EVENT_SHIPPER=true/);
    expect(msg).toMatch(/PROCESS_ROLE=gateway/);
    expect(msg).toMatch(/PROCESS_ROLE=http/);
  });

  it.each([
    ['gateway', true],
    ['http', true],
    ['combined', false],
    ['gateway', false],
    ['http', false],
  ])('returns null for supported combination role=%s shipper=%s', (role, shipperEnabled) => {
    expect(unsupportedRoleShipperCombo(role, shipperEnabled)).toBeNull();
  });
});

describe('unsupportedRoleResumeCombo', () => {
  it('returns null when resume=false regardless of other inputs', () => {
    for (const role of ['combined', 'gateway', 'http']) {
      for (const shipper of [true, false]) {
        expect(unsupportedRoleResumeCombo(role, false, shipper)).toBeNull();
      }
    }
  });

  it('rejects resume=true with combined role and surfaces shim/Client conflict', () => {
    const msg = unsupportedRoleResumeCombo('combined', true, true);
    expect(msg).not.toBeNull();
    expect(msg).toMatch(/PROCESS_ROLE=combined/);
    expect(msg).toMatch(/ENABLE_GATEWAY_RESUME=true/);
    expect(msg).toMatch(/PROCESS_ROLE=gateway/);
    expect(msg).toMatch(/PROCESS_ROLE=http/);
    expect(unsupportedRoleResumeCombo('combined', true, false)).toBe(msg);
  });

  it('rejects resume=true with shipper=false on supported roles', () => {
    for (const role of ['gateway', 'http']) {
      const msg = unsupportedRoleResumeCombo(role, true, false);
      expect(msg).not.toBeNull();
      expect(msg).toMatch(/ENABLE_GATEWAY_RESUME=true requires ENABLE_EVENT_SHIPPER=true/);
      expect(msg).not.toMatch(/replaces discord\.js Client/);
      expect(msg).not.toMatch(/@discordjs\/ws/);
    }
  });

  it('returns null for the production-shape path', () => {
    expect(unsupportedRoleResumeCombo('gateway', true, true)).toBeNull();
    expect(unsupportedRoleResumeCombo('http', true, true)).toBeNull();
  });
});

describe('shouldRegisterInteractionListener', () => {
  test.each([
    ['combined',   false, true,  'legacy in-process; local listener handles dispatch'],
    ['combined',   true,  true,  'unreachable in prod (boot reject); predicate output coherent'],
    ['gateway',    false, true,  'legacy in-process gateway tier (single-process deploy)'],
    ['gateway',    true,  false, 'gateway tier publishes to SQS; local listener disconnected'],
    ['http',       false, false, 'no gateway WS + no SQS consumer; listener would never fire'],
    ['http',       true,  true,  'worker tier; SQS consumer re-emits, listener routes'],
  ])('role=%s flag=%s → %s (%s)', (role, eventShipperEnabled, expected) => {
    const { isGateway, isHttp } = resolveProcessRole(role);
    expect(shouldRegisterInteractionListener({ isGateway, isHttp, eventShipperEnabled })).toBe(expected);
  });

  it('is a pure function (no side effects, same input → same output)', () => {
    const args = { isGateway: true, isHttp: false, eventShipperEnabled: true };
    const first = shouldRegisterInteractionListener(args);
    const second = shouldRegisterInteractionListener(args);
    expect(first).toBe(second);
  });
});

describe('missingMapCommandKeys', () => {
  it('returns empty when the flag is off — Maps key state is irrelevant', () => {
    expect(missingMapCommandKeys({})).toEqual([]);
    expect(missingMapCommandKeys({ MAP_COMMAND_ENABLED: false })).toEqual([]);
    expect(
      missingMapCommandKeys({ MAP_COMMAND_ENABLED: false, GOOGLE_MAPS_API_KEY: '' }),
    ).toEqual([]);
    expect(
      missingMapCommandKeys({
        MAP_COMMAND_ENABLED: false,
        GOOGLE_MAPS_API_KEY: GOOGLE_MAPS_API_KEY_PLACEHOLDER_SENTINEL,
      }),
    ).toEqual([]);
  });

  it('flags GOOGLE_MAPS_API_KEY when toggle is on but the key is missing', () => {
    expect(
      missingMapCommandKeys({ MAP_COMMAND_ENABLED: true }),
    ).toEqual(['GOOGLE_MAPS_API_KEY']);
    expect(
      missingMapCommandKeys({ MAP_COMMAND_ENABLED: true, GOOGLE_MAPS_API_KEY: '' }),
    ).toEqual(['GOOGLE_MAPS_API_KEY']);
  });

  it('flags GOOGLE_MAPS_API_KEY when toggle is on but the key is still the PLACEHOLDER sentinel', () => {
    expect(
      missingMapCommandKeys({
        MAP_COMMAND_ENABLED: true,
        GOOGLE_MAPS_API_KEY: GOOGLE_MAPS_API_KEY_PLACEHOLDER_SENTINEL,
      }),
    ).toEqual(['GOOGLE_MAPS_API_KEY']);
  });

  it('returns empty when toggle is on AND the key is a real value', () => {
    expect(
      missingMapCommandKeys({
        MAP_COMMAND_ENABLED: true,
        GOOGLE_MAPS_API_KEY: 'AIzaSyA-real-looking-key-1234567890',
      }),
    ).toEqual([]);
  });
});

describe('resolveProcessRole', () => {
  it('VALID_PROCESS_ROLES is the canonical set in stable order (combined first as the default)', () => {
    expect(VALID_PROCESS_ROLES).toEqual(['combined', 'gateway', 'http']);
    expect(Object.isFrozen(VALID_PROCESS_ROLES)).toBe(true);
  });

  it.each([
    ['combined', { role: 'combined', isGateway: true, isHttp: true }],
    ['gateway', { role: 'gateway', isGateway: true, isHttp: false }],
    ['http', { role: 'http', isGateway: false, isHttp: true }],
  ])('resolves %s to expected role flags', (input, expected) => {
    expect(resolveProcessRole(input)).toEqual(expected);
  });

  it.each([undefined, null, '', '   ', '\t'])(
    'falls back to combined for unset / whitespace-only value (%p)',
    (input) => {
      expect(resolveProcessRole(input)).toEqual({
        role: 'combined', isGateway: true, isHttp: true,
      });
    }
  );

  it('trims surrounding whitespace before validating', () => {
    expect(resolveProcessRole('  http  ')).toEqual({
      role: 'http', isGateway: false, isHttp: true,
    });
  });

  it('throws on unknown role with INVALID_PROCESS_ROLE code (so index.js can exit(1))', () => {
    let caught;
    try {
      resolveProcessRole('gatewayy');
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(Error);
    expect(caught.code).toBe('INVALID_PROCESS_ROLE');
    expect(caught.message).toMatch(/'gatewayy'/);
    expect(caught.message).toMatch(/combined, gateway, http/);
  });

  it('rejects case-variant roles (no silent normalization)', () => {
    expect(() => resolveProcessRole('GATEWAY')).toThrow(/GATEWAY/);
    expect(() => resolveProcessRole('Combined')).toThrow(/Combined/);
  });
});

describe('unsupportedRoleHotStandbyCombo', () => {
  it('returns null when hot-standby=false regardless of other inputs', () => {
    for (const role of ['combined', 'gateway', 'http']) {
      for (const resume of [true, false]) {
        expect(unsupportedRoleHotStandbyCombo(role, false, resume)).toBeNull();
      }
    }
  });

  it('rejects hot-standby=true on combined role with operator-facing remediation', () => {
    const msg = unsupportedRoleHotStandbyCombo('combined', true, true);
    expect(msg).not.toBeNull();
    expect(msg).toMatch(/ENABLE_GATEWAY_HOT_STANDBY=true/);
    expect(msg).toMatch(/PROCESS_ROLE=gateway/);
    expect(msg).toMatch(/'combined'/);
  });

  it('rejects hot-standby=true on http role', () => {
    const msg = unsupportedRoleHotStandbyCombo('http', true, true);
    expect(msg).not.toBeNull();
    expect(msg).toMatch(/'http'/);
    expect(msg).toMatch(/no manager to hand off/);
  });

  it('rejects hot-standby=true with resume=false (would session-flap)', () => {
    const msg = unsupportedRoleHotStandbyCombo('gateway', true, false);
    expect(msg).not.toBeNull();
    expect(msg).toMatch(/requires ENABLE_GATEWAY_RESUME=true/);
    expect(msg).toMatch(/flap the session/);
  });

  it('returns null on the supported combo (gateway + resume + hot-standby)', () => {
    expect(unsupportedRoleHotStandbyCombo('gateway', true, true)).toBeNull();
  });

  it('sequences role check before resume check (operator sees the dominant fix first)', () => {
    const msg = unsupportedRoleHotStandbyCombo('combined', true, false);
    expect(msg).toMatch(/PROCESS_ROLE=gateway/);
    expect(msg).not.toMatch(/ENABLE_GATEWAY_RESUME=true/);
  });
});

describe('missingHotStandbyKeys', () => {
  function cfg(overrides = {}) {
    return {
      ENABLE_GATEWAY_HOT_STANDBY: true,
      INSTANCE_ID: 'task-abc-123',
      INSTANCE_IP: '10.0.1.42',
      hasGatewayHandoffHmac: true,
      ...overrides,
    };
  }

  it('returns empty when the flag is off (no requirements)', () => {
    expect(missingHotStandbyKeys({
      ENABLE_GATEWAY_HOT_STANDBY: false,
    })).toEqual([]);
  });

  it('returns empty when every required key is present', () => {
    expect(missingHotStandbyKeys(cfg())).toEqual([]);
  });

  it('surfaces missing INSTANCE_ID', () => {
    expect(missingHotStandbyKeys(cfg({ INSTANCE_ID: undefined }))).toEqual(['INSTANCE_ID']);
  });

  it('surfaces missing INSTANCE_IP', () => {
    expect(missingHotStandbyKeys(cfg({ INSTANCE_IP: null }))).toEqual(['INSTANCE_IP']);
  });

  it('surfaces missing GATEWAY_HANDOFF_HMAC (via hasGatewayHandoffHmac flag)', () => {
    expect(missingHotStandbyKeys(cfg({ hasGatewayHandoffHmac: false })))
      .toEqual(['GATEWAY_HANDOFF_HMAC']);
  });

  it('returns every missing key (not just the first) for one-shot remediation', () => {
    const missing = missingHotStandbyKeys(cfg({
      INSTANCE_ID: undefined,
      INSTANCE_IP: undefined,
      hasGatewayHandoffHmac: false,
    }));
    expect(missing).toEqual(['INSTANCE_ID', 'INSTANCE_IP', 'GATEWAY_HANDOFF_HMAC']);
  });
});

describe('invalidHotStandbyValues', () => {
  function cfg(overrides = {}) {
    return {
      ENABLE_GATEWAY_HOT_STANDBY: true,
      INSTANCE_ID: 'task-abc-123',
      INSTANCE_IP: '10.0.1.42',
      ...overrides,
    };
  }

  it('returns empty when the flag is off (no shape requirements)', () => {
    expect(invalidHotStandbyValues({
      ENABLE_GATEWAY_HOT_STANDBY: false,
      INSTANCE_ID: '${LITERALLY_UNRESOLVED}',
      INSTANCE_IP: 'not-an-ip',
    })).toEqual([]);
  });

  it('returns empty when both values are well-shaped', () => {
    expect(invalidHotStandbyValues(cfg())).toEqual([]);
  });

  it('flags unsubstituted template literal in INSTANCE_ID — env-override paste footgun', () => {
    const problems = invalidHotStandbyValues(cfg({ INSTANCE_ID: '${ECS_TASK_ARN}' }));
    expect(problems).toHaveLength(1);
    expect(problems[0]).toMatch(/INSTANCE_ID looks like an unsubstituted template literal/);
    expect(problems[0]).toContain('${ECS_TASK_ARN}');
  });

  it('flags non-IPv4 INSTANCE_IP (string)', () => {
    const problems = invalidHotStandbyValues(cfg({ INSTANCE_IP: 'not-an-ip' }));
    expect(problems).toHaveLength(1);
    expect(problems[0]).toMatch(/INSTANCE_IP must be a valid IPv4 address/);
    expect(problems[0]).toContain("'not-an-ip'");
  });

  it('flags out-of-range octets in INSTANCE_IP (10.0.0.999)', () => {
    const problems = invalidHotStandbyValues(cfg({ INSTANCE_IP: '10.0.0.999' }));
    expect(problems).toHaveLength(1);
    expect(problems[0]).toContain('10.0.0.999');
  });

  it('flags IPv6 INSTANCE_IP (out of scope for Pillar 3)', () => {
    const problems = invalidHotStandbyValues(cfg({ INSTANCE_IP: '::1' }));
    expect(problems).toHaveLength(1);
    expect(problems[0]).toMatch(/must be a valid IPv4/);
  });

  it('flags link-local INSTANCE_IP (paste-error from ECS metadata endpoint URL)', () => {
    const problems = invalidHotStandbyValues(cfg({ INSTANCE_IP: '169.254.172.2' }));
    expect(problems).toHaveLength(1);
    expect(problems[0]).toMatch(/link-local/);
    expect(problems[0]).toContain("'169.254.172.2'");
  });

  it('flags leading-zero IPv4 octets (octal-parse hazard under some resolvers)', () => {
    for (const ip of ['01.0.0.1', '10.01.0.1', '10.0.01.1', '10.0.0.01']) {
      const problems = invalidHotStandbyValues(cfg({ INSTANCE_IP: ip }));
      expect(problems).toHaveLength(1);
      expect(problems[0]).toMatch(/must be a valid IPv4/);
    }
  });

  it('accepts every octet boundary (0, 9, 10, 99, 100, 199, 200, 249, 255)', () => {
    const ips = ['0.0.0.0', '255.255.255.255', '10.99.100.249', '1.9.199.200'];
    for (const ip of ips) {
      expect(invalidHotStandbyValues(cfg({ INSTANCE_IP: ip }))).toEqual([]);
    }
  });

  it('reports every problem on a single call (one-shot operator remediation)', () => {
    const problems = invalidHotStandbyValues(cfg({
      INSTANCE_ID: '${ECS_TASK_ARN}',
      INSTANCE_IP: '999.999.999.999',
    }));
    expect(problems).toHaveLength(2);
  });

  it('does not trip on present-but-empty INSTANCE_IP (the missingHotStandbyKeys check catches that)', () => {
    expect(invalidHotStandbyValues(cfg({ INSTANCE_IP: '' }))).toEqual([]);
    expect(invalidHotStandbyValues(cfg({ INSTANCE_ID: '' }))).toEqual([]);
  });
});

describe('invalidStateSecretValues', () => {

  it('returns [] when nothing is configured and no state secret is set', () => {
    expect(invalidStateSecretValues({})).toEqual([]);
  });

  it('flags a missing state secret only when the qURL flow is configured', () => {
    const problems = invalidStateSecretValues({ isQurlOAuthConfigured: true });
    expect(problems).toHaveLength(1);
    expect(problems[0]).toMatch(/no state-signing secret is available/);
    expect(problems[0]).toMatch(/openssl rand -hex 32/);

    expect(invalidStateSecretValues({ isQurlOAuthConfigured: false })).toEqual([]);
  });

  it('flags a configured qURL OAuth flow with NO available signer key (would deferred-500 the first /qurl setup)', () => {
    const problems = invalidStateSecretValues({ isQurlOAuthConfigured: true });
    expect(problems).toHaveLength(1);
    expect(problems[0]).toMatch(/qURL OAuth is configured .* but no state-signing secret/);
    expect(problems[0]).toMatch(/QURL_OAUTH_STATE_SECRET \(preferred\) or OAUTH_STATE_SECRET/);
  });

  it('accepts a configured qURL OAuth flow when either chain key exists', () => {
    expect(invalidStateSecretValues({
      isQurlOAuthConfigured: true,
      QURL_OAUTH_STATE_SECRET: '1'.repeat(64),
    })).toEqual([]);
    expect(invalidStateSecretValues({
      isQurlOAuthConfigured: true,
      OAUTH_STATE_SECRET: '0'.repeat(64),
    })).toEqual([]);
  });

  it('flags a set-but-short state secret even when the qURL flow is not configured', () => {
    const problems = invalidStateSecretValues({
      isQurlOAuthConfigured: false,
      OAUTH_STATE_SECRET: 'shrt',
    });
    expect(problems).toHaveLength(1);
    expect(problems[0]).toMatch(/OAUTH_STATE_SECRET is shorter than \d+ chars \(got 4\)/);
  });

  it('does not demand a signer key when qURL OAuth is unconfigured (signer call sites all gate on isQurlOAuthConfigured)', () => {
    expect(invalidStateSecretValues({ isQurlOAuthConfigured: false })).toEqual([]);
  });

  it('flags a set-but-short OAUTH_STATE_SECRET in every mode (a short secret must fail at boot, not at first use)', () => {
    const oneShort = 'x'.repeat(MIN_STATE_SECRET_LENGTH - 1);
    for (const isQurlOAuthConfigured of [true, false]) {
      const problems = invalidStateSecretValues({ isQurlOAuthConfigured, OAUTH_STATE_SECRET: oneShort });
      expect(problems).toHaveLength(1);
      expect(problems[0]).toMatch(new RegExp(`OAUTH_STATE_SECRET is shorter than ${MIN_STATE_SECRET_LENGTH} chars \\(got ${oneShort.length}\\)`));
    }
  });

  it('flags a set-but-short QURL_OAUTH_STATE_SECRET in every mode (the qURL flow must not deferred-500)', () => {
    for (const isQurlOAuthConfigured of [true, false]) {
      const problems = invalidStateSecretValues({
        isQurlOAuthConfigured,
        OAUTH_STATE_SECRET: '0'.repeat(64),
        QURL_OAUTH_STATE_SECRET: 'shrt',
      });
      expect(problems).toHaveLength(1);
      expect(problems[0]).toMatch(/QURL_OAUTH_STATE_SECRET is shorter than/);
    }
  });

  it('reports every problem on a single call (one-shot operator remediation)', () => {
    const problems = invalidStateSecretValues({
      isQurlOAuthConfigured: false,
      OAUTH_STATE_SECRET: 'shrt',
      QURL_OAUTH_STATE_SECRET: 'also-shrt',
    });
    expect(problems).toHaveLength(2);
  });

  it('accepts secrets at and above the floor', () => {
    expect(invalidStateSecretValues({
      isQurlOAuthConfigured: true,
      OAUTH_STATE_SECRET: 'x'.repeat(MIN_STATE_SECRET_LENGTH),
    })).toEqual([]);
    expect(invalidStateSecretValues({
      isQurlOAuthConfigured: true,
      OAUTH_STATE_SECRET: '0'.repeat(64),
      QURL_OAUTH_STATE_SECRET: '1'.repeat(64),
    })).toEqual([]);
  });

});
