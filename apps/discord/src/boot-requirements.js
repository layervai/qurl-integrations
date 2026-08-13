// Pure helpers computing which env-vars the bot refuses to boot without.
// Extracted from index.js so they can be unit-tested without importing
// the full bot (which would side-effect client.login()). The lists are
// the highest-risk branch in the module: a regression here could either
// boot in prod with missing secrets OR die on a spurious false-positive.

const { MIN_STATE_SECRET_LENGTH } = require('./utils/oauth-state');

// Required at boot in EVERY environment.
//
// Explicitly NOT on this list:
//   - GUILD_ID: optional by design — unset means multi-tenant mode, and
//     a set value has already been snowflake-validated by config.js.
//     Re-checking truthiness here would never catch a real misconfig.
//   - BASE_URL: config.js supplies an unconditional "http://localhost:3000"
//     default, so `cfg.BASE_URL` is always truthy. The real enforcement is
//     baseUrlHttpsProblem (below), called from index.js's production block,
//     which runs regardless of this required-list membership.
// Listing either would be decorative — the downstream checks are the
// authority. Keeping this list to the keys whose absence is actually a
// boot blocker.
function bootRequired() {
  return ['DISCORD_TOKEN'];
}

// Additionally required when NODE_ENV=production. QURL_API_KEY is NOT
// here: it is only the global fallback for /qurl send + /qurl map, and
// every deployment shape relies on per-guild /qurl setup instead.
//
// KEY_ENCRYPTION_KEY appears here AND in missingKekRequiredKeys. The
// two overlap on prod-with-OAuth (both fail closed there); the
// load-bearing distinct case is staging/preview with qURL OAuth
// configured, which missingKekRequiredKeys catches and this
// production-only list would not.
function prodRequired() {
  return ['METRICS_TOKEN', 'KEY_ENCRYPTION_KEY'];
}

// Compute which required keys are missing from a given config-like
// object. Separate from bootRequired so tests can build a "config" with
// specific holes and assert the exact missing list.
function missingBootKeys(cfg) {
  return bootRequired().filter(key => !cfg[key]);
}

function missingProdKeys(env) {
  return prodRequired().filter(k => !env[k]);
}

// KEY_ENCRYPTION_KEY is required independently of NODE_ENV whenever the
// qURL OAuth setup flow is configured — staging/preview environments
// that run guided setup persist real qURL API keys, and crypto.encrypt's
// dev plaintext fallback must never reach
// `guild_configs.qurl_api_key`.
//
// The trigger is passed in rather than derived here: KEY_ENCRYPTION_KEY
// lives in raw env (index.js reads process.env for it, and the smoke
// test below does too), while `isQurlOAuthConfigured` is a derived flag
// config.js computes from the AUTH0_* block including its domain-shape
// validation. Taking both keeps this helper pure and avoids a second,
// drifting copy of that derivation.
function missingKekRequiredKeys(env, isQurlOAuthConfigured) {
  if (!isQurlOAuthConfigured) return [];
  return env.KEY_ENCRYPTION_KEY ? [] : ['KEY_ENCRYPTION_KEY'];
}

function isPrivateIPv4Literal(hostname) {
  const parts = hostname.split('.');
  if (parts.length !== 4) return false;
  const octets = parts.map(part => Number(part));
  // The String(octet) round-trip is NOT a leading-zero/octal defense — WHATWG
  // already canonicalizes those inside new URL() (`010.0.0.1` arrives as
  // `8.0.0.1`). It rejects labels that Number() accepts but the URL spec's
  // IPv4 parser does not, which therefore arrive as ordinary DOMAIN
  // hostnames: `Number('1e2')` is 100, so without it a public host like
  // 10.2.3.1e2 or 192.168.0.1e1 would read as a private literal and
  // crash-loop a legitimate deploy at boot.
  if (octets.some((octet, idx) => !Number.isInteger(octet) || octet < 0 || octet > 255 || String(octet) !== parts[idx])) {
    return false;
  }
  // CGNAT 100.64.0.0/10 is deliberately NOT screened: unlike the ranges below
  // it can front a legitimately reachable origin, so rejecting it would fail
  // a valid deploy. Same reasoning excludes the TEST-NET blocks — the screen
  // rejects hosts that CANNOT serve a public OAuth redirect, not every host
  // that merely looks unusual.
  const [a, b] = octets;
  return a === 0 //                              0.0.0.0/8 "this network"
    || a === 10
    || a === 127
    || (a === 169 && b === 254)
    || (a === 172 && b >= 16 && b <= 31)
    || (a === 192 && b === 168);
}

// The v6 half of the same cheap literal screen. Kept separate because the
// parser hands back IPv4-mapped addresses in hex (`::ffff:127.0.0.1`
// serializes as `::ffff:7f00:1`), so the dotted form never survives to a
// string compare and has to be mapped back to octets.
// Deliberately common-forms-only, matching the cheap-literal scope of the
// screen as a whole: the deprecated IPv4-COMPATIBLE form (`::127.0.0.1`,
// which serializes to `::7f00:1`) and site-local `fec0::/10` both fall
// through. Both address classes are dead in practice, `::1` covers realistic
// loopback, and reachability is not knowable at boot anyway.
function isLocalOnlyIPv6(host) {
  if (host === '::' || host === '::1') return true;
  const mapped = /^::ffff:([0-9a-f]{1,4}):([0-9a-f]{1,4})$/.exec(host);
  if (mapped) {
    const [hi, lo] = mapped.slice(1).map(group => parseInt(group, 16));
    return isPrivateIPv4Literal([hi >> 8, hi & 0xff, lo >> 8, lo & 0xff].join('.'));
  }
  const firstGroup = parseInt(host.split(':')[0], 16);
  if (!Number.isInteger(firstGroup)) return false;
  return (firstGroup & 0xfe00) === 0xfc00 //  fc00::/7  unique-local
    || (firstGroup & 0xffc0) === 0xfe80; //   fe80::/10 link-local
}

function isLocalOnlyHost(hostname) {
  // Strip the brackets the parser keeps around an IPv6 literal, and the
  // trailing dot of an absolute FQDN — `localhost.` resolves the same as
  // `localhost`, so it must not slip past the name compares below.
  const host = hostname.toLowerCase().replace(/^\[|\]$/g, '').replace(/\.$/, '');
  return host === 'localhost'
    || host.endsWith('.localhost')
    || isPrivateIPv4Literal(host)
    // The colon test is load-bearing, not a fast path: parseInt() below is a
    // lenient prefix parse, so parseInt('fc00.example.com', 16) is 0xfc00 and
    // the unique-local mask would misread real public hosts as link-local.
    || (host.includes(':') && isLocalOnlyIPv6(host));
}

// Textual userinfo strip, for values `new URL` can't parse into a
// username/password (a malformed origin, or an opaque `scheme:host` form).
// Those still carry the credential as raw text, so the parsed-only redaction
// below would put it in the boot log verbatim. WHATWG treats the LAST `@`
// before the path as the userinfo separator, so the greedy match is correct;
// `[^/?#]*` keeps it inside the authority, leaving a later `/path@thing`
// alone.
function stripUserinfo(value) {
  return value.replace(/^([a-zA-Z][a-zA-Z0-9+.-]*:(?:\/\/)?)[^/?#]*@/, '$1');
}

function baseUrlForError(rawBaseUrl) {
  try {
    const parsed = new URL(rawBaseUrl);
    if (parsed.username || parsed.password) {
      parsed.username = '';
      parsed.password = '';
      return parsed.href;
    }
  } catch {
    // Fall through to the textual strip — a value that fails to parse can
    // still be carrying `user:pass@`, and a hand-edited SSM param is exactly
    // the input most likely to be both malformed and credential-bearing.
  }
  return stripUserinfo(rawBaseUrl);
}

// BASE_URL https guardrail. The qURL guided setup flow builds an absolute
// OAuth redirect from config.BASE_URL — the /oauth/qurl/start link
// (commands.js) and the /oauth/qurl/callback redirect_uri
// (routes/qurl-oauth.js). That router mounts UNCONDITIONALLY in server.js,
// and /qurl setup takes the OAuth path whenever isQurlOAuthConfigured, so a
// localhost BASE_URL silently dead-ends setup at the redirect — in plain
// single-guild and multi-tenant deploys alike (#619). The Stage-2 Discord
// install callback (routes/discord-install.js) embeds BASE_URL too, but
// isDiscordInstallConfigured ⟹ isQurlOAuthConfigured (config.js), so the
// isQurlOAuthConfigured gate already covers it.
//
// The check parses BASE_URL (new URL) rather than prefix-matching: parsing
// normalizes the case-insensitive scheme (RFC 3986) and rejects a bare
// "https://" with no host that would still build a broken redirect. OAuth
// needs a public bare origin because the code composes
// `${BASE_URL}/oauth/qurl/callback`; embedded paths, query strings, userinfo,
// or obvious local-only IP literals either miss the registered mux route or
// can't serve the external Auth0 browser redirect. We intentionally do NOT
// resolve DNS at boot — reachability belongs in deploy smoke tests — but we
// can reject localhost/loopback/private IP literals cheaply.
//
// Intentionally NOT gated on: the per-guild webhook bridge
// (guild-webhook-link.js → `${BASE_URL}/webhooks/qurl`) also embeds
// BASE_URL, but it's fire-and-forget and non-fatal — a wrong bridge URL
// degrades qURL view-count delivery from push to the existing poll
// fallback, it doesn't dead-end a user flow. Blocking boot on it would
// force BASE_URL onto the plain qURL-sharing deploys #619 keeps free to
// ignore it.
//
// Outside the qURL setup flow BASE_URL is unused for redirects, but a stale
// explicit http:// value is still rejected (the original canary).
// `baseUrlExplicitlySet` (caller-computed from process.env, treating
// "" / whitespace-only as unset) separates "operator set a bad value" from
// "fell back to the localhost default" so an empty SSM param doesn't
// false-positive. Caller gates on NODE_ENV==='production'; string-or-null
// mirrors unsupportedRoleShipperCombo et al.
function baseUrlHttpsProblem(cfg, baseUrlExplicitlySet) {
  let parsed = null;
  try {
    parsed = new URL(cfg.BASE_URL);
  } catch {
    // Malformed BASE_URL (incl. a host-less "https://") is not usable.
  }
  const usesHttps = parsed?.protocol === 'https:';
  // Bare origin only. server.js mounts the qURL OAuth router at the root
  // while the redirect URI is built by concatenation
  // (`${BASE_URL}/oauth/qurl/callback`), so a path-prefixed BASE_URL yields
  // a redirect_uri no mounted route can ever serve. qurl-integrations-infra
  // rejects the same shape at plan time as of qurl-integrations-infra#1379
  // (its `base_url` variable now validates `^https://[^[:space:]/?#]+$`),
  // so plan-time and boot-time agree — a prefixed value can no longer reach
  // a deploy.
  const isBareOrigin = Boolean(
    parsed
      && parsed.host
      && !parsed.username
      && !parsed.password
      && parsed.pathname === '/'
      && !parsed.search
      && !parsed.hash
  );
  const isPublicOrigin = Boolean(parsed && !isLocalOnlyHost(parsed.hostname));
  const usableOAuthOrigin = usesHttps && isBareOrigin && isPublicOrigin;
  if (usableOAuthOrigin) return null;
  const displayBaseUrl = baseUrlForError(cfg.BASE_URL);
  if (cfg.isQurlOAuthConfigured) {
    return (
      'BASE_URL must be a public bare https:// origin in production ' +
      '— the qURL guided setup flow builds its OAuth redirect from it, and a ' +
      `non-public or non-origin value dead-ends setup at the redirect. Got: ${displayBaseUrl}. ` +
      "Set BASE_URL to the bot's public https:// origin in the deployment template."
    );
  }
  if (baseUrlExplicitlySet && !usesHttps) {
    return `BASE_URL must use https:// in production (got ${displayBaseUrl})`;
  }
  return null;
}

// QURL_BOT_EVENTS_QUEUE_URL is the load-bearing piece of the event-
// shipper path: producer publishes to it, consumer polls from it. When
// ENABLE_EVENT_SHIPPER=true and the queue URL isn't set, the producer
// silently drops every dispatch and the consumer sits dormant — both
// failure modes are silent enough that they wouldn't surface in
// monitoring until /qurl interactions start timing out from the user's
// end. Fail-closed at boot is preferable.
//
// API asymmetry: this takes the PARSED `cfg` while the siblings
// (missingBootKeys, missingProdKeys, missingKekRequiredKeys) take
// raw `env`. The reason is that ENABLE_EVENT_SHIPPER is parsed in
// config.js (`process.env.ENABLE_EVENT_SHIPPER === 'true'` → boolean)
// and consumers should not re-implement that parsing. Reading from
// cfg keeps the literal-'true' contract in one place.
function missingEventShipperKeys(cfg) {
  if (!cfg.ENABLE_EVENT_SHIPPER) return [];
  return cfg.QURL_BOT_EVENTS_QUEUE_URL ? [] : ['QURL_BOT_EVENTS_QUEUE_URL'];
}

// View-update push (feat #60). Mirrors missingEventShipperKeys: when
// ENABLE_VIEW_UPDATE_PUSH=true, QURL_BOT_VIEW_UPDATES_QUEUE_URL is
// required. A misconfigured deploy would otherwise drop every view
// event silently (publisher) or throw at start() (consumer); the
// uniform boot-time check makes the failure mode loud and consistent
// with the existing event-shipper gate.
//
// Intentionally no combined-mode rejector (no analog of
// unsupportedRoleShipperCombo). The registry's silent-drop-on-miss +
// status==='opened' idempotency guard make combined-mode safe: a
// duplicate dispatch within one process is a no-op at the handler
// layer. Pinned by tests/boot-requirements.test.js's absence
// assertion — a copy-paste-from-shipper refactor that adds a
// rejector would fail that test.
function missingViewUpdatePushKeys(cfg) {
  if (!cfg.ENABLE_VIEW_UPDATE_PUSH) return [];
  return cfg.QURL_BOT_VIEW_UPDATES_QUEUE_URL ? [] : ['QURL_BOT_VIEW_UPDATES_QUEUE_URL'];
}

// PROCESS_ROLE=combined paired with ENABLE_EVENT_SHIPPER=true is
// unsupported and rejected at boot. In combined mode both `isGateway`
// and `isHttp` evaluate true, which derives `isWorker=true`, which
// would arm both the gateway-side publish hook AND the worker-side
// consumer in one process. Every interaction would land twice: once
// via the in-process gateway WS frame, once via the SQS round-trip.
// Side effects (DM fan-out, flow-state writes) double; telemetry
// reports two dispatches per real interaction. The listener gate
// alone can't close this — discord.js's InteractionCreate action
// fires synchronously on the gateway WS frame regardless of whether
// the local listener is registered, so even gating the worker-side
// dispatcher leaves the gateway publish path firing alongside the
// consumer.
//
// The supported flag-on shape is the two-process split: a separate
// PROCESS_ROLE=gateway (singleton) publishing to SQS, and one or
// more PROCESS_ROLE=http replicas consuming. Combined mode stays
// supported for sandbox / local-dev / pre-split deployments —
// just with the flag off, running the legacy in-process path.
//
// Returns the operator-facing message on rejection or null on
// success. Kept as a string-or-null rather than throwing so the
// caller in index.js logs the message + exits via the same pattern
// as missingBootKeys (one log + process.exit) rather than handling
// a thrown error specially.
function unsupportedRoleShipperCombo(role, eventShipperEnabled) {
  if (role === 'combined' && eventShipperEnabled) {
    return (
      'PROCESS_ROLE=combined with ENABLE_EVENT_SHIPPER=true is not supported ' +
      '(would dispatch every interaction twice — once via the in-process gateway ' +
      'WS frame, once via the SQS round-trip). Run two processes: ' +
      'PROCESS_ROLE=gateway (singleton, publishes) + PROCESS_ROLE=http (consumes). ' +
      'For local dev / sandbox in one process, leave ENABLE_EVENT_SHIPPER unset.'
    );
  }
  return null;
}

// Parallel to unsupportedRoleShipperCombo for ENABLE_GATEWAY_RESUME.
// Three unsupported shapes:
//
//   1. resume=true with role=combined — combined mode runs both
//      tiers in one process, which the legacy discord.js Client
//      owns end-to-end. The resume shim would conflict with the
//      Client's WS ownership.
//   2. resume=true with shipper=false — the resume shim
//      (@discordjs/ws WebSocketManager) replaces discord.js's Client
//      entirely, so the in-process interaction dispatcher has no
//      `client.on('interactionCreate')` emitter to attach to. The
//      flag-on path is only coherent when the shipper has already
//      moved dispatch to SQS.
//   3. resume=true with storeType!=ddb — the resume guarantee only
//      holds when session state is persisted across processes.
//      A non-ddb backend (none supported today; this branch is a
//      defense-in-depth canary for a future backend addition)
//      would lack the cross-process visibility the resume path
//      needs, and a resume against the previous sequence would
//      fail every restart. Rejecting at boot is preferable to a
//      silent IDENTIFY-every-restart degradation that mimics
//      flag-off behavior.
//
// Returns the operator-facing message on rejection or null on
// success. Same string-or-null shape as unsupportedRoleShipperCombo.
function unsupportedRoleResumeCombo(role, resumeEnabled, eventShipperEnabled, storeType) {
  if (!resumeEnabled) return null;
  if (role === 'combined') {
    return (
      'PROCESS_ROLE=combined with ENABLE_GATEWAY_RESUME=true is not supported ' +
      '(the resume shim owns the WebSocket and conflicts with the legacy ' +
      'discord.js Client that combined mode runs). Run two processes: ' +
      'PROCESS_ROLE=gateway (singleton, owns the resume shim) + ' +
      'PROCESS_ROLE=http (consumes from SQS). For local dev / sandbox in ' +
      'one process, leave ENABLE_GATEWAY_RESUME unset.'
    );
  }
  if (!eventShipperEnabled) {
    // Role-neutral framing: the same env may be applied uniformly
    // across gateway + http task defs, and an http operator reading
    // this message shouldn't see gateway-tier-specific language
    // (the shim is never constructed on http; the rejection here
    // is a consistency canary, not a description of what http does).
    return (
      'ENABLE_GATEWAY_RESUME=true requires ENABLE_EVENT_SHIPPER=true. ' +
      'The two flags co-design: the gateway tier forwards every Discord ' +
      'frame to SQS while the worker tier consumes it, so a resume path ' +
      'without the shipper-shape split has nowhere to dispatch. Enable ' +
      'the shipper first, or leave ENABLE_GATEWAY_RESUME unset.'
    );
  }
  // Defense-in-depth canary: `store/index.js` already rejects every
  // non-ddb STORE_TYPE at module load with a listing-of-valid-backends
  // error, so in practice the bot can't reach this function with a
  // non-ddb value. This branch survives so that if a future PR adds a
  // second backend to `VALID_BACKENDS` without thinking through the
  // RESUME cross-process semantics, the bot still refuses to boot the
  // unsupported combo instead of silently IDENTIFYing every restart.
  if (storeType !== 'ddb') {
    return (
      `ENABLE_GATEWAY_RESUME=true requires STORE_TYPE=ddb (got '${storeType}'). ` +
      'Cross-process RESUME persists session state to the gateway-session DDB ' +
      'table; any non-ddb backend lacks the cross-process visibility the next ' +
      'process needs. Set STORE_TYPE=ddb (or leave unset to take the default) ' +
      'in the deployment template, or leave ENABLE_GATEWAY_RESUME unset.'
    );
  }
  return null;
}

// Parallel to unsupportedRoleResumeCombo for ENABLE_GATEWAY_HOT_STANDBY.
// Caller guarantees the upstream resume combo check has already run
// (resumeEnabled=true → shipper+ddb already validated upstream), so
// the 3-arg signature here is sufficient — no need to re-check
// shipper/storeType.
//
// Two unsupported shapes:
//
//   1. hotStandby=true with role!=gateway — the leader coordinator
//      drives the gateway-ws-shim's manager handle; only the gateway
//      tier constructs the shim, so http/combined have nothing to
//      hand off. Rejecting at boot avoids a deploy where the hot-
//      standby flag is set in a uniform env block but the role isn't
//      gateway — which would silently no-op the leader path and
//      mask the misconfig as "the lock never gets acquired."
//   2. hotStandby=true with resume=false — pushHandoff hands the
//      incoming task a snapshot of session_id + sequence so it can
//      RESUME without dropping events. Without the cross-process
//      RESUME path, "handoff" degenerates to "both replicas
//      IDENTIFY against the same token" — Discord rejects the
//      second IDENTIFY and the second replica flaps the session.
//
// Returns the operator-facing message on rejection or null on
// success. Same string-or-null shape as unsupportedRoleResumeCombo.
function unsupportedRoleHotStandbyCombo(role, hotStandbyEnabled, resumeEnabled) {
  if (!hotStandbyEnabled) return null;
  if (role !== 'gateway') {
    return (
      `ENABLE_GATEWAY_HOT_STANDBY=true requires PROCESS_ROLE=gateway (got '${role}'). ` +
      'The hot-standby control plane (leader election + push handoff) drives the ' +
      'gateway-ws-shim manager, which only the gateway tier constructs. http and ' +
      'combined roles have no manager to hand off. Set PROCESS_ROLE=gateway on the ' +
      'gateway task def, or leave ENABLE_GATEWAY_HOT_STANDBY unset on http/combined.'
    );
  }
  if (!resumeEnabled) {
    return (
      'ENABLE_GATEWAY_HOT_STANDBY=true requires ENABLE_GATEWAY_RESUME=true. ' +
      'Push handoff transfers session_id + sequence from the outgoing task to the ' +
      'incoming task; without cross-process RESUME the incoming task would IDENTIFY ' +
      'against the same bot token and Discord would flap the session. Enable RESUME ' +
      'first, or leave ENABLE_GATEWAY_HOT_STANDBY unset.'
    );
  }
  return null;
}

// Required env vars when ENABLE_GATEWAY_HOT_STANDBY=true on a gateway
// replica. Returns the array of missing keys (parallel shape to
// missingMapCommandKeys / missingEventShipperKeys). Each value is
// load-bearing: INSTANCE_ID keys the lock row + peer-heartbeat row;
// INSTANCE_IP is the address peers reach this replica on; the HMAC
// secret authenticates every control-channel envelope. A boot with
// any of these unset would either crash at first use or — worse —
// run with a zero-knowledge HMAC that fails every verify silently.
function missingHotStandbyKeys(cfg) {
  if (!cfg.ENABLE_GATEWAY_HOT_STANDBY) return [];
  const missing = [];
  if (!cfg.INSTANCE_ID) missing.push('INSTANCE_ID');
  if (!cfg.INSTANCE_IP) missing.push('INSTANCE_IP');
  // GATEWAY_HANDOFF_HMAC presence is surfaced via `hasGatewayHandoffHmac`
  // (boolean flag) rather than the raw value — the secret string is
  // never exposed as a config-object property to keep it unreachable
  // through heap-dump-accessible references. See config.js's
  // `takeGatewayHandoffHmac` for the security rationale.
  if (!cfg.hasGatewayHandoffHmac) missing.push('GATEWAY_HANDOFF_HMAC');
  return missing;
}

// Shape checks for INSTANCE_ID + INSTANCE_IP (run AFTER missing-keys
// passes). Matches the secret-loader's "fail at boot, not at first
// use" posture: an env override like `INSTANCE_ID=${ECS_TASK_ARN}`
// (unsubstituted shell expansion an operator pasted by mistake) or
// `INSTANCE_IP=10.0.0.999` (non-IPv4) would pass the presence gate
// and surface as a baffling DDB-lock-can't-acquire or peer-
// unreachable error at runtime. A cheap regex catches both.
//
// INSTANCE_ID: rejects the `${...}` template-literal pattern (the
// classic env-override substitution-failure footgun). Otherwise
// permissive — the downstream lock/heartbeat code does not require
// a specific format.
//
// INSTANCE_IP: must parse as an IPv4 dotted-quad. The hot-standby
// awsvpc deployment puts each task on a unique ENI with a v4
// address; v6 is not in scope for Pillar 3 today (the control
// channel binds to the v4 ENI, peers reach each other over v4 SG
// rules). If v6 ever lands, this check loosens.
//
// Leading-zero octets are rejected (`01.02.03.04` would parse as
// octal under some resolvers); each octet is `0` alone, `1-9`, or
// `1[0-9]-25[0-5]` with no leading zero. ECS task-def injection
// produces canonical no-leading-zero v4 strings; this just closes
// the operator-typo door.
//
// Returns an array of operator-facing message strings (one per
// problem) or [] when all values are well-shaped. Hot-standby off
// → skip entirely.
const IPV4_RE = /^(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]\d|\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]\d|\d)$/;
function invalidHotStandbyValues(cfg) {
  if (!cfg.ENABLE_GATEWAY_HOT_STANDBY) return [];
  const problems = [];
  if (cfg.INSTANCE_ID && cfg.INSTANCE_ID.includes('${')) {
    problems.push(
      `INSTANCE_ID looks like an unsubstituted template literal ('${cfg.INSTANCE_ID}'). ` +
      'INSTANCE_ID is derived from os.hostname() by default; an env override here was set to an unresolved placeholder.'
    );
  }
  if (cfg.INSTANCE_IP && !IPV4_RE.test(cfg.INSTANCE_IP)) {
    problems.push(
      `INSTANCE_IP must be a valid IPv4 address (got '${cfg.INSTANCE_IP}'). ` +
      'Hot-standby uses v4 for the control-channel binding + peer reach; v6 is not in scope today. ' +
      'If you set INSTANCE_IP as an env override, unset it to fall back to the derivation from os.networkInterfaces().'
    );
  }
  // Link-local (169.254.0.0/16) is well-formed IPv4 but not routable
  // peer-to-peer. config.deriveInstanceIp filters it out of the
  // os.networkInterfaces walk; the override path must reject it for
  // the same reason. Common operator paste-error: copying the ECS
  // task-metadata endpoint URL (169.254.170.2 / 169.254.172.2) out
  // of AWS docs.
  if (cfg.INSTANCE_IP && IPV4_RE.test(cfg.INSTANCE_IP) && cfg.INSTANCE_IP.startsWith('169.254.')) {
    problems.push(
      `INSTANCE_IP is link-local (got '${cfg.INSTANCE_IP}'). ` +
      '169.254.0.0/16 is RFC 3927 link-local and not routable peer-to-peer; push-handoff would POST to an unreachable address. ' +
      'Unset the env override so it falls back to the os.networkInterfaces() derivation, or set it to the task\'s awsvpc-assigned private IP.'
    );
  }
  return problems;
}

// State-signing secret checks for the shared OAuth state signer
// (utils/oauth-state.js). Two rules, both production-only (the caller
// in index.js sits inside the NODE_ENV=production block, keeping dev
// localhost workflows convenient):
//
//   Presence — when qURL OAuth is configured (AUTH0_* set; every
//   sign/verify call site gates on isQurlOAuthConfigured), SOME key in
//   the signer's resolution chain must exist. Without the rule, a
//   deploy with Auth0 configured and no state secret would boot and
//   500 on the first /qurl setup.
//
//   Shape — ANY set state secret must clear the signer's length floor,
//   in EVERY mode, so a short value fails at deploy instead of
//   deferred-500ing the first /qurl setup. The signer re-enforces the
//   same floor lazily at sign/verify time; this is the loud-at-deploy
//   copy. Deliberately fail-loud-on-any-set-value: set-but-wrong is a
//   misconfig in its own right even where nothing would resolve it, and
//   a later config change shouldn't unearth a latent bad value.
//
// Presence-vs-shape separation mirrors missingHotStandbyKeys /
// invalidHotStandbyValues: a key that is unset simply doesn't
// participate in resolution (the signer falls through), so only set
// values are shape-checked, and each problem gets its own message.
//
// Every invalidStateSecretValues message ends with the same remediation
// clause — one constant so the sites can't drift on the recommended
// generator.
const STATE_SECRET_REMEDIATION = 'Generate with: openssl rand -hex 32';

// Returns an array of operator-facing message strings, [] on success
// (same shape as invalidHotStandbyValues above).
function invalidStateSecretValues(cfg) {
  const problems = [];
  if (cfg.isQurlOAuthConfigured
      && !cfg.QURL_OAUTH_STATE_SECRET && !cfg.OAUTH_STATE_SECRET) {
    problems.push(
      'qURL OAuth is configured (AUTH0_* set) but no state-signing secret is available: ' +
      `set QURL_OAUTH_STATE_SECRET (preferred) or OAUTH_STATE_SECRET. ${STATE_SECRET_REMEDIATION}`
    );
  }
  for (const key of ['OAUTH_STATE_SECRET', 'QURL_OAUTH_STATE_SECRET']) {
    const value = cfg[key];
    if (value && value.length < MIN_STATE_SECRET_LENGTH) {
      problems.push(
        `${key} is shorter than ${MIN_STATE_SECRET_LENGTH} chars (got ${value.length}); ` +
        `the state signer will refuse to mint with it. ${STATE_SECRET_REMEDIATION}`
      );
    }
  }
  return problems;
}

// PLACEHOLDER is treated as missing because the SSM parameter
// ships with that literal sentinel value; remediation ("seed a
// real key") is identical to the empty-key case.
//
// TODO(infra-sentinel-sync): the literal "PLACEHOLDER" is also
// the seed value for `aws_ssm_parameter.bot` in
// qurl-integrations-infra/qurl-bot-discord/terraform/main.tf
// (search that repo for `value = "PLACEHOLDER"`). If infra ever
// renames the sentinel (e.g., "REPLACE_ME"), update here in
// lockstep — otherwise the boot check silently regresses to
// "non-empty value passes" and the original incident class
// returns. `git grep TODO(infra-sentinel-sync)` finds the marker.
const GOOGLE_MAPS_API_KEY_PLACEHOLDER_SENTINEL = 'PLACEHOLDER';
function missingMapCommandKeys(cfg) {
  if (!cfg.MAP_COMMAND_ENABLED) return [];
  const key = cfg.GOOGLE_MAPS_API_KEY;
  if (!key || key === GOOGLE_MAPS_API_KEY_PLACEHOLDER_SENTINEL) {
    return ['GOOGLE_MAPS_API_KEY'];
  }
  return [];
}

// Process-role parsing for the gateway/HTTP split. Lifted out of
// index.js so the invalid-value path is testable without spawning a
// child process — same shape as missingBootKeys above. See
// .env.example's PROCESS_ROLE block for the operator-facing
// description of each role.
const VALID_PROCESS_ROLES = Object.freeze(['combined', 'gateway', 'http']);

// Normalize and validate a PROCESS_ROLE value. Returns
// `{role, isGateway, isHttp}` on success; throws an Error tagged
// with `code = 'INVALID_PROCESS_ROLE'` on an unknown role so index.js
// can surface a single boot-fail log line + exit(1) without
// re-implementing the validation.
//
// Accepts the raw env-var string (or undefined). Empty / whitespace-only
// values fall back to 'combined' — matching the documented default and
// avoiding a false-positive boot failure when an SSM-seeded param is
// templated to "" or " ".
function resolveProcessRole(rawValue) {
  const trimmed = String(rawValue ?? '').trim();
  const role = trimmed || 'combined';
  if (!VALID_PROCESS_ROLES.includes(role)) {
    const err = new Error(
      `Invalid PROCESS_ROLE: '${role}'. Valid values: ${VALID_PROCESS_ROLES.join(', ')}. ` +
      `Set PROCESS_ROLE to one of these, or leave unset to default to 'combined'.`
    );
    err.code = 'INVALID_PROCESS_ROLE';
    throw err;
  }
  return {
    role,
    isGateway: role === 'gateway' || role === 'combined',
    isHttp: role === 'http' || role === 'combined',
  };
}

// Whether the local `interactionCreate` listener should be registered
// in this process. Lifted out of index.js so the gate logic is pure +
// unit-testable across every role × flag permutation (combined + flag-on
// is unreachable here — `unsupportedRoleShipperCombo` rejects it at
// boot — but the predicate must remain coherent for any caller that
// somehow reaches it post-bypass, so it's defined for all inputs).
//
// Three intended shapes:
//   - Gateway tier + flag-off  → register (legacy in-process dispatch)
//   - Gateway tier + flag-on   → DO NOT register (publishes to SQS;
//                                local listener would dispatch the
//                                same payload a second time after the
//                                worker tier's consumer re-emits)
//   - Worker tier (HTTP + flag-on) → register (SQS consumer
//                                reconstructs the interaction and
//                                re-emits locally; the listener
//                                routes via handleCommand /
//                                handleFlowInteraction)
//   - HTTP-only + flag-off     → DO NOT register (no gateway WS,
//                                no SQS consumer, so the listener
//                                would never fire and registering it
//                                would just leak handler references)
//
// The predicate `(isGateway && !flag) || (isHttp && flag)` collapses
// these four cases. Combined mode (isGateway=true, isHttp=true) with
// flag-off reduces to the first disjunct (register); with flag-on it
// would reduce to "register" via BOTH disjuncts (the false-positive
// the boot rejection guards against — see unsupportedRoleShipperCombo).
function shouldRegisterInteractionListener({ isGateway, isHttp, eventShipperEnabled }) {
  return (isGateway && !eventShipperEnabled) || (isHttp && eventShipperEnabled);
}

module.exports = {
  bootRequired,
  prodRequired,
  missingBootKeys,
  missingProdKeys,
  missingKekRequiredKeys,
  baseUrlHttpsProblem,
  missingEventShipperKeys,
  missingViewUpdatePushKeys,
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
};
